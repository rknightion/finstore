package truelayer

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/consent"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
)

// RunScheduled runs one scheduled sync: /me, /accounts, and for each account (Monzo pots
// excluded) its standing orders and direct debits, and for Lloyds its transactions for the last
// SyncDays days and then its balance. It holds the writer lock for the run and catches the
// projection up at the end. It does nothing and returns nil when there is no active consent. A
// refused refresh or data call ends the consent and returns ErrConsentLost; the daily limit or a
// 429 ends the run partial with rate_limited and leaves the consent alone.
func (w *Worker) RunScheduled(ctx context.Context) error {
	cur, err := w.consents.Current(ctx, w.st.DB())
	if errors.Is(err, consent.ErrNoConsent) {
		return nil
	}
	if err != nil {
		return err
	}
	c, expired, err := w.consents.ExpireIfDue(ctx, w.st.DB(), cur.ID)
	if err != nil {
		return err
	}
	w.recordConsent(ctx, c)
	if expired || c.State != store.ConsentActive {
		return nil
	}
	if lost, err := w.loseIfTokenMissing(ctx, c); err != nil || lost {
		return err
	}
	return w.st.HoldWriterLock(ctx, func(ctx context.Context, conn *pgx.Conn) error {
		runID, err := store.StartRun(ctx, w.st.DB(), w.source, KindSync, w.now())
		if err != nil {
			return err
		}
		return w.runLocked(ctx, conn, runID, c.ID, KindSync, "")
	})
}

// runLocked runs run runID, a connect or a scheduled run already recorded, under the writer lock
// held on conn, within RunTimeout, and records its end. token, when set, is used for every call as
// it is.
func (w *Worker) runLocked(ctx context.Context, conn *pgx.Conn, runID, consentID int64, kind, token string) error {
	q := w.st.DB()
	rctx, cancel := context.WithTimeout(ctx, w.opts.RunTimeout)
	defer cancel()
	tally := &runTally{}
	runErr := w.steps(withTally(withAccessToken(rctx, token), tally), runID, kind, tally)
	outcome := outcomeOK
	switch {
	case runErr != nil && isRateLimited(runErr):
		// Rate limited: the run ends here, the consent does not.
		runErr = nil
		outcome = outcomePartial
		tally.fail(categoryRateLimited)
	case runErr != nil:
		outcome = outcomeFailed
		tally.mu.Lock()
		tally.category = category(runErr)
		tally.mu.Unlock()
	case tally.partial:
		outcome = outcomePartial
	}
	// A Lloyds connect that ended before every account's history arrived, for whatever reason, has
	// lost the full history until a reconnect: history_failed is what alerts on that.
	if kind == KindConnect && w.opts.Connection == ConnLloyds && !tally.historyComplete() {
		tally.failHistory()
		outcome = outcomePartial
	}
	if isAccessDenied(runErr) {
		w.markLost(ctx, consentID)
		runErr = errors.Join(ErrConsentLost, runErr)
	}
	finishErr := w.finishRun(ctx, runID, kind, outcome, tally)
	var okErr error
	if outcome == outcomeOK {
		at := w.now()
		okErr = store.SetConsentLastSyncOK(context.WithoutCancel(ctx), q, consentID, at)
		if okErr == nil {
			w.metrics.SyncSucceeded(ctx, w.source, at)
		}
	}
	_, projErr := w.proj.CatchUp(context.WithoutCancel(ctx), conn, "core")
	w.log.InfoContext(ctx, "truelayer run finished", semconv.LogKind, kind, semconv.LogRunID, runID,
		semconv.LogOutcome, outcome, semconv.LogErrorCategory, tally.category, semconv.LogRows, tally.rows)
	return errors.Join(runErr, finishErr, okErr, projErr)
}

// steps makes a run's calls. Only a failure to list the accounts, the rate limit, a refused data
// call or refresh, a store failure or the deadline is returned; any other failed read marks the
// run partial and the run goes on.
func (w *Worker) steps(ctx context.Context, runID int64, kind string, tally *runTally) error {
	q := w.st.DB()
	listID, err := store.InsertFetch(ctx, q, store.Fetch{Source: w.source, Kind: kind, RunID: runID, StartedAt: w.now()})
	if err != nil {
		return err
	}
	lctx := withFetch(ctx, listID)
	// A connect read /me when it approved the consent, under its meta fetch.
	if kind == KindSync {
		if err := w.step(lctx, tally, func(ctx context.Context) error { _, err := w.api.me(ctx); return err }); err != nil {
			return err
		}
	}
	accts, err := w.api.accounts(lctx)
	if err != nil {
		return err
	}
	tally.setListed()
	for _, a := range accts {
		if a.bankRef == "" {
			// Nothing can be keyed or attached without a sort code and account number.
			continue
		}
		if w.opts.Connection == ConnMonzo && a.typ == typeSavings {
			// A Monzo pot: its transfers are in Monzo's own feed and it has no mandates.
			continue
		}
		if w.opts.Connection == ConnLloyds {
			tally.addPendingHistory()
		}
		ref := a.bankRef
		fid, err := store.InsertFetch(ctx, q, store.Fetch{Source: w.source, Kind: kind, AccountKey: &ref, RunID: runID, StartedAt: w.now()})
		if err != nil {
			return err
		}
		actx := withFetch(ctx, fid)
		for _, tmpl := range []string{pathStandingOrders, pathDirectDebits} {
			if err := w.step(actx, tally, func(ctx context.Context) error { return w.api.read(ctx, tmpl, a) }); err != nil {
				return err
			}
		}
		if w.opts.Connection != ConnLloyds {
			continue
		}
		// Transactions before the balance: the ledger checks the running balance when it applies
		// the balance, and must see the period's rows and disappearances first.
		if err := w.history(ctx, runID, kind, a, tally); err != nil {
			return err
		}
		if err := w.step(actx, tally, func(ctx context.Context) error { return w.api.read(ctx, pathBalance, a) }); err != nil {
			return err
		}
	}
	return nil
}

// step runs one read the run can do without.
func (w *Worker) step(ctx context.Context, tally *runTally, fn func(context.Context) error) error {
	err := fn(ctx)
	if err == nil {
		return nil
	}
	if stops(err) {
		return err
	}
	tally.fail(category(err))
	return nil
}

// localMidnight is the start of a calendar day in loc.
func localMidnight(day time.Time, loc *time.Location) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
}

// history fetches one Lloyds account's transactions in one call, under its own fetch: from
// HistoryFrom on connect, SyncDays back on a scheduled run, to today. The fetch's period runs from
// local midnight of from to local midnight after to; a listing that arrives whole gets a
// raw.fetch_complete.
func (w *Worker) history(ctx context.Context, runID int64, kind string, a listedAccount, tally *runTally) error {
	q := w.st.DB()
	now := w.now().In(w.opts.Location)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	from := today.AddDate(0, 0, -w.opts.SyncDays)
	if kind == KindConnect {
		h := w.opts.HistoryFrom
		from = time.Date(h.Year(), h.Month(), h.Day(), 0, 0, 0, 0, time.UTC)
	}
	start := localMidnight(from, w.opts.Location)
	end := localMidnight(today.AddDate(0, 0, 1), w.opts.Location)
	ref := a.bankRef
	fid, err := store.InsertFetch(ctx, q, store.Fetch{Source: w.source, Kind: kind, AccountKey: &ref,
		PeriodStart: &start, PeriodEnd: &end, RunID: runID, StartedAt: w.now()})
	if err != nil {
		return err
	}
	rows, err := w.api.transactions(withFetch(ctx, fid), a, from, today)
	if err != nil {
		// On a connect the missing history makes the run history_failed (runLocked).
		if stops(err) {
			return err
		}
		tally.fail(category(err))
		return nil
	}
	tally.addRows(rows)
	if _, err = store.InsertFetchComplete(ctx, q, store.FetchComplete{FetchID: fid, CompletedAt: w.now(), Pages: 1}); err != nil {
		return err
	}
	tally.historyDone()
	return nil
}

// finishRun records the end of a run and counts it with its error category.
func (w *Worker) finishRun(ctx context.Context, runID int64, kind, outcome string, tally *runTally) error {
	var cat *string
	if tally.category != "" {
		c := tally.category
		cat = &c
	}
	err := store.FinishRun(context.WithoutCancel(ctx), w.st.DB(), runID, store.RunResult{
		FinishedAt: w.now(), Outcome: outcome, Requests: int(tally.requests.Load()), RowsSeen: tally.rows, ErrorCategory: cat,
	})
	w.metrics.SyncRunWithCategory(ctx, w.source, kind, outcome, tally.category)
	return err
}

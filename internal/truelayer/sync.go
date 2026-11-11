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
// SyncDays days and then its balance; for Lloyds with the cards scope in /me, the same for each
// credit card; then the optional reads (pending transactions, the Monzo card list, /info once a
// local day). It holds the writer lock for the run and catches the projection up at the end. It
// does nothing and returns nil when there is no active consent. A refused refresh or data call
// ends the consent and returns ErrConsentLost, except on an optional read, which never ends it;
// the daily limit or a 429 on a ledger read ends the run partial with rate_limited and leaves the
// consent alone.
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
		return w.runLocked(ctx, conn, runID, c.ID, KindSync, "", nil)
	})
}

// runLocked runs run runID, a connect or a scheduled run already recorded, under the writer lock
// held on conn, within RunTimeout, and records its end. token, when set, is used for every call as
// it is. scopes are the scopes the consent was granted as the connect's own approval read them
// from /me; a scheduled run reads them from its own /me instead.
func (w *Worker) runLocked(ctx context.Context, conn *pgx.Conn, runID, consentID int64, kind, token string, scopes []string) error {
	q := w.st.DB()
	rctx, cancel := context.WithTimeout(ctx, w.opts.RunTimeout)
	defer cancel()
	tally := &runTally{}
	runErr := w.steps(withTally(withAccessToken(rctx, token), tally), runID, kind, tally, scopes)
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
	stats, projErr := w.proj.CatchUp(context.WithoutCancel(ctx), conn, "core")
	// The ledger counts the card accounts a newest card list left out; its gauge is the last count.
	for source, n := range stats.CardsUnlisted {
		w.metrics.CardsUnlisted(ctx, source, n)
	}
	w.log.InfoContext(ctx, "truelayer run finished", semconv.LogKind, kind, semconv.LogRunID, runID,
		semconv.LogOutcome, outcome, semconv.LogErrorCategory, tally.category, semconv.LogRows, tally.rows)
	return errors.Join(runErr, finishErr, okErr, projErr)
}

// grantedScopes is the set of scopes a consent was granted, as the current consent's own /me says.
// A run never reads it back from raw, so an earlier consent's answer can never widen it.
type grantedScopes map[string]bool

func newGrantedScopes(scopes []string) grantedScopes {
	g := grantedScopes{}
	for _, s := range scopes {
		g[s] = true
	}
	return g
}

// steps makes a run's calls. Only a failure to list the accounts, the rate limit, a refused data
// call or refresh, a store failure or the deadline is returned; any other failed read marks the
// run partial and the run goes on. A connect run passes the scopes its approval read from /me; a
// scheduled run calls /me first and uses that answer. With no readable scopes, none is granted.
func (w *Worker) steps(ctx context.Context, runID int64, kind string, tally *runTally, scopes []string) error {
	q := w.st.DB()
	listID, err := store.InsertFetch(ctx, q, store.Fetch{Source: w.source, Kind: kind, RunID: runID, StartedAt: w.now()})
	if err != nil {
		return err
	}
	lctx := withFetch(ctx, listID)
	granted := newGrantedScopes(scopes)
	// A connect read /me when it approved the consent, under its meta fetch.
	if kind == KindSync {
		granted = grantedScopes{}
		if err := w.step(lctx, tally, func(ctx context.Context) error {
			me, err := w.api.me(ctx)
			if err == nil {
				granted = newGrantedScopes(me.Scopes)
			}
			return err
		}); err != nil {
			return err
		}
	}
	accts, err := w.api.accounts(lctx)
	if err != nil {
		return err
	}
	tally.setListed()
	var optional []optionalRead
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
		if err := w.history(ctx, runID, kind, a, pathTransactions, w.opts.HistoryFrom, tally); err != nil {
			return err
		}
		if err := w.step(actx, tally, func(ctx context.Context) error { return w.api.read(ctx, pathBalance, a) }); err != nil {
			return err
		}
		optional = append(optional, optionalRead{ctx: actx, endpoint: pathPending,
			fn: func(ctx context.Context) error { return w.api.read(ctx, pathPending, a) }})
	}
	// A Lloyds consent that lists cards among its scopes: the card list, then each card as an
	// account of its own. All of it is ledger data, so it comes before any optional read.
	if w.opts.Connection == ConnLloyds && granted["cards"] {
		more, err := w.cards(ctx, lctx, runID, kind, tally)
		if err != nil {
			return err
		}
		optional = append(optional, more...)
	}
	return w.optionalReads(ctx, lctx, optional, granted)
}

// optionalRead is one optional read waiting for the end of the ledger reads, with the context
// (the fetch) its answer is to be stored under.
type optionalRead struct {
	ctx      context.Context
	endpoint string
	fn       func(context.Context) error
}

// optionalReads makes the run's optional reads, all after every ledger read: the pending
// transactions of every account and then of every card, for the Monzo connection the card list
// (stored raw, so that it shows whether Monzo Flex is exposed this way; Monzo's own API is the
// source of its ledger), and /info when it was not already read that local day. None of them can
// end a consent or change the run's outcome (stepOptional).
func (w *Worker) optionalReads(ctx, lctx context.Context, reads []optionalRead, granted grantedScopes) error {
	run := &optionalRun{}
	for _, r := range reads {
		if err := w.stepOptional(r.ctx, run, r.endpoint, r.fn); err != nil {
			return err
		}
	}
	if w.opts.Connection == ConnMonzo && granted["cards"] {
		if err := w.stepOptional(lctx, run, pathCards, w.api.listCards); err != nil {
			return err
		}
	}
	// /info changes rarely: one attempt a local day is enough.
	if granted["info"] && !run.limited {
		n, err := store.CountCallsToday(ctx, w.st.DB(), w.source, pathInfo, "", w.now().In(w.opts.Location))
		if err != nil {
			return err
		}
		if n == 0 {
			return w.stepOptional(lctx, run, pathInfo, w.api.info)
		}
	}
	return nil
}

// errCardsRefused is returned by a card read the bank refused (401, 403 or access_denied).
var errCardsRefused = errors.New("truelayer: card read refused")

// cardRead runs one card read. A refusal by the bank does not end the consent (a card scope the
// bank does not honour must not cost the working ledger feed): it returns errCardsRefused. Any
// other failure follows the rules of a ledger read (step). A refused token refresh is no bank
// answer to the card call and still ends the consent.
func (w *Worker) cardRead(ctx context.Context, tally *runTally, fn func(context.Context) error) error {
	err := fn(ctx)
	switch {
	case err == nil:
		return nil
	case isRefused(err) && !isTokenCall(err):
		return errCardsRefused
	case stops(err):
		return err
	}
	tally.fail(category(err))
	return nil
}

// cardsRefused records that the bank refused a card read: the run is partial with cards_refused, the
// consent stays, and the run makes no further card call. cardHistoryOwed is how many pending
// history counts of the connect check to release: the cards' history is simply absent, which does
// not make the run history_failed.
func (w *Worker) cardsRefused(ctx context.Context, tally *runTally, endpoint string, cardHistoryOwed int) {
	for range cardHistoryOwed {
		tally.historyDone()
	}
	tally.fail(categoryCardsRefused)
	w.log.WarnContext(ctx, "truelayer card read refused", semconv.LogEndpoint, endpoint, semconv.LogErrorCategory, categoryCardsRefused)
}

// cards reads a Lloyds consent's cards as ledger data: the card list, then for each usable card its
// transactions and its balance, each card under a fetch keyed by its card key. It returns the
// card's pending reads for the optional phase. On a connect the card list counts towards the
// history check until it answers, and each card until its history arrives, so a /cards or a card's
// transactions call that fails ends the run history_failed: that history is lost until the next
// reconnect. A refusal of any card read is the exception: it keeps the consent, ends the card calls
// of the run, and marks the run partial with cards_refused (cardRead). A card that usableCards skips
// is logged, and two cards sharing their last four also mark the run partial.
func (w *Worker) cards(ctx, lctx context.Context, runID int64, kind string, tally *runTally) ([]optionalRead, error) {
	tally.addPendingHistory()
	var list cardList
	listed := false
	err := w.cardRead(lctx, tally, func(ctx context.Context) error {
		var err error
		list, err = w.api.cards(ctx)
		listed = err == nil
		return err
	})
	if errors.Is(err, errCardsRefused) {
		w.cardsRefused(ctx, tally, pathCards, 1)
		return nil, nil
	}
	if err != nil || !listed {
		return nil, err
	}
	tally.historyDone()
	if list.NoDigits > 0 {
		w.log.WarnContext(ctx, "truelayer cards skipped: no usable last four digits", semconv.LogEndpoint, pathCards, semconv.LogCount, list.NoDigits)
	}
	if list.Shared > 0 {
		w.log.WarnContext(ctx, "truelayer cards skipped: last four digits shared", semconv.LogEndpoint, pathCards, semconv.LogCount, list.Shared)
		tally.fail(categoryCardKeyShared)
	}
	var optional []optionalRead
	for _, c := range list.Cards {
		card := listedAccount{id: c.ID, typ: typeCard, bankRef: c.Key}
		tally.addPendingHistory()
		key := c.Key
		fid, err := store.InsertFetch(ctx, w.st.DB(), store.Fetch{Source: w.source, Kind: kind, AccountKey: &key, RunID: runID, StartedAt: w.now()})
		if err != nil {
			return nil, err
		}
		cctx := withFetch(ctx, fid)
		if err := w.history(ctx, runID, kind, card, pathCardTransactions, w.opts.CardHistoryFrom, tally); err != nil {
			if errors.Is(err, errCardsRefused) {
				w.cardsRefused(ctx, tally, pathCardTransactions, 1)
				return nil, nil
			}
			return nil, err
		}
		if err := w.cardRead(cctx, tally, func(ctx context.Context) error { return w.api.read(ctx, pathCardBalance, card) }); err != nil {
			if errors.Is(err, errCardsRefused) {
				w.cardsRefused(ctx, tally, pathCardBalance, 0)
				return nil, nil
			}
			return nil, err
		}
		optional = append(optional, optionalRead{ctx: cctx, endpoint: pathCardPending,
			fn: func(ctx context.Context) error { return w.api.read(ctx, pathCardPending, card) }})
	}
	return optional, nil
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

// history fetches one Lloyds account's or card's transactions (template says which) in one call,
// under its own fetch: from connectFrom on connect, SyncDays back on a scheduled run, to today. The
// fetch's period runs from local midnight of from to local midnight after to; a listing that
// arrives whole gets a raw.fetch_complete.
func (w *Worker) history(ctx context.Context, runID int64, kind string, a listedAccount, template string, connectFrom time.Time, tally *runTally) error {
	q := w.st.DB()
	now := w.now().In(w.opts.Location)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	from := today.AddDate(0, 0, -w.opts.SyncDays)
	if kind == KindConnect {
		from = time.Date(connectFrom.Year(), connectFrom.Month(), connectFrom.Day(), 0, 0, 0, 0, time.UTC)
	}
	start := localMidnight(from, w.opts.Location)
	end := localMidnight(today.AddDate(0, 0, 1), w.opts.Location)
	ref := a.bankRef
	fid, err := store.InsertFetch(ctx, q, store.Fetch{Source: w.source, Kind: kind, AccountKey: &ref,
		PeriodStart: &start, PeriodEnd: &end, RunID: runID, StartedAt: w.now()})
	if err != nil {
		return err
	}
	rows, err := w.api.transactions(withFetch(ctx, fid), template, a, from, today)
	if err != nil {
		// A card's refused history never ends the consent (see cards).
		if template == pathCardTransactions && isRefused(err) && !isTokenCall(err) {
			return errCardsRefused
		}
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

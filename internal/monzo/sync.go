package monzo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/bankhttp"
	"github.com/rknightion/finstore/internal/consent"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
)

// Scheduled run kinds and the period each fetches.
const (
	KindHourly  = "hourly"
	KindNightly = "nightly"

	HourlyPeriod  = 7 * 24 * time.Hour
	NightlyPeriod = 89 * 24 * time.Hour
)

// errBackfillStarted stops a scheduled run that finds a full-history fetch waiting to start.
var errBackfillStarted = errors.New("monzo: a full-history fetch started")

// RunScheduled runs one scheduled sync of kind KindHourly or KindNightly: for each open account
// it reads the balance, fetches the period ending just after that reading, reads the balance
// again and reads the pots (see syncAccount for the fetches written). It then
// catches the projection up. It does nothing and returns nil when there is no active consent or a
// full-history fetch is running. Lost permissions end the consent and return ErrConsentLost.
func (w *Worker) RunScheduled(ctx context.Context, kind string) error {
	var period time.Duration
	switch kind {
	case KindHourly:
		period = HourlyPeriod
	case KindNightly:
		period = NightlyPeriod
	default:
		return fmt.Errorf("monzo: unknown scheduled run kind %q", kind)
	}
	if w.backfilling.Load() > 0 {
		w.log.InfoContext(ctx, "monzo scheduled sync skipped: full-history fetch running", semconv.LogKind, kind)
		return nil
	}
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
	return w.st.HoldWriterLock(ctx, store.HolderMonzoSync, func(ctx context.Context, conn *pgx.Conn) error {
		// A full-history fetch may have started while this run waited for the lock.
		if w.backfilling.Load() > 0 {
			return nil
		}
		return w.scheduledLocked(ctx, conn, c, kind, period)
	})
}

func (w *Worker) scheduledLocked(ctx context.Context, conn *pgx.Conn, c store.Consent, kind string, period time.Duration) error {
	q := w.st.DB()
	runID, err := store.StartRun(ctx, q, Source, kind, w.now())
	if err != nil {
		return err
	}
	tally := &runTally{}
	runErr := w.scheduledSteps(withTally(ctx, tally), runID, kind, period, tally)
	outcome := outcomeOK
	switch {
	case errors.Is(runErr, errBackfillStarted):
		runErr = nil
		outcome = outcomePartial
		tally.fail("backfill_started")
	case runErr != nil:
		outcome = outcomeFailed
		tally.fail(category(runErr))
	case tally.partial:
		outcome = outcomePartial
	}
	lost := hasCategory(runErr, bankhttp.CategoryForbiddenInsufficientPermissions)
	if lost {
		w.markLostPermissions(ctx, c.ID)
		runErr = errors.Join(ErrConsentLost, runErr)
	}
	finishErr := w.finishRun(ctx, runID, kind, outcome, tally)
	var okErr error
	if outcome == outcomeOK {
		at := w.now()
		okErr = store.SetConsentLastSyncOK(ctx, q, c.ID, at)
		if okErr == nil {
			w.metrics.SyncSucceeded(ctx, Source, at)
		}
	}
	_, projErr := w.proj.CatchUp(ctx, conn, "core")
	w.log.InfoContext(ctx, "monzo scheduled sync finished", semconv.LogKind, kind, semconv.LogRunID, runID,
		semconv.LogOutcome, outcome, semconv.LogPages, tally.pages, semconv.LogRows, tally.rows)
	return errors.Join(runErr, finishErr, okErr, projErr)
}

func (w *Worker) scheduledSteps(ctx context.Context, runID int64, kind string, period time.Duration, tally *runTally) error {
	q := w.st.DB()
	listID, err := store.InsertFetch(ctx, q, store.Fetch{Source: Source, Kind: kind, RunID: runID, StartedAt: w.now()})
	if err != nil {
		return err
	}
	accts, err := w.api.accounts(withFetch(ctx, listID))
	if err != nil {
		return err
	}
	for _, a := range accts {
		if a.Closed {
			continue
		}
		if w.backfilling.Load() > 0 {
			return errBackfillStarted
		}
		if err := w.syncAccount(ctx, runID, kind, a.ID, period, tally); err != nil {
			return err
		}
	}
	return nil
}

// syncAccount reads one account's opening balance, then fetches its period, ending at the moment
// after that reading so that every transaction the opening balance includes is inside it.
//
// The opening balance is recorded under a fetch with the account and no period; the period's
// pages, completion, closing balance and pots under a second fetch with the period. The ledger
// pairs the two balance readings by run and account, not by fetch.
//
// Invariant: the period fetch covers exactly what it lists. PeriodStart is the since of its
// first page and PeriodEnd the before of every page; it pages once from the start, never
// resuming, and bankhttp's retries repeat the same request. It gets a raw.fetch_complete only
// when every page arrived and no step went by id.
func (w *Worker) syncAccount(ctx context.Context, runID int64, kind, id string, period time.Duration, tally *runTally) error {
	q := w.st.DB()
	openID, err := store.InsertFetch(ctx, q, store.Fetch{Source: Source, Kind: kind, AccountKey: &id, RunID: runID, StartedAt: w.now()})
	if err != nil {
		return err
	}
	if err := w.readStep(withFetch(ctx, openID), tally, func(ctx context.Context) error { return w.api.balance(ctx, id) }); err != nil {
		return err
	}
	end := w.now().UTC().Truncate(time.Microsecond)
	start := end.Add(-period)
	fetchID, err := store.InsertFetch(ctx, q, store.Fetch{
		Source: Source, Kind: kind, AccountKey: &id, PeriodStart: &start, PeriodEnd: &end, RunID: runID, StartedAt: w.now(),
	})
	if err != nil {
		return err
	}
	res, err := w.pageTransactions(ctx, fetchID, id, formatTime(start), start, end, time.Time{}, nil)
	tally.add(res.pages, res.rows)
	if err != nil {
		if !isBankFailure(err) || hasCategory(err, bankhttp.CategoryForbiddenInsufficientPermissions) {
			return err
		}
		tally.fail(category(err))
	}
	switch {
	case res.complete && res.idStep:
		// Rows of a timestamp group larger than a page were skipped: the fetch proves nothing
		// about absence.
		tally.fail(categoryTimestampGroup)
	case res.complete:
		// The completion goes straight after the last page, before the closing balance: the
		// ledger applies disappearances at the completion row and checks the balance at the
		// second reading, in seq order, so the check must see the period's disappearances.
		if _, err := store.InsertFetchComplete(ctx, q, store.FetchComplete{
			FetchID: fetchID, CompletedAt: res.completedAt, Pages: res.pages,
		}); err != nil {
			return err
		}
	}
	fctx := withFetch(ctx, fetchID)
	for _, step := range []func(context.Context) error{
		func(ctx context.Context) error { return w.api.balance(ctx, id) },
		func(ctx context.Context) error { return w.api.pots(ctx, id) },
	} {
		if err := w.readStep(fctx, tally, step); err != nil {
			return err
		}
	}
	return nil
}

// markLostPermissions ends an active consent after a 403 forbidden.insufficient_permissions.
func (w *Worker) markLostPermissions(ctx context.Context, id int64) {
	ctx = context.WithoutCancel(ctx)
	c, err := w.consents.Lost(ctx, w.st.DB(), id, consent.ReasonInsufficientPermissions)
	if err != nil {
		w.log.ErrorContext(ctx, "monzo consent could not be marked lost", semconv.LogErrorCategory, category(err))
		return
	}
	w.recordConsent(ctx, c)
	w.log.WarnContext(ctx, "monzo consent lost", semconv.LogState, c.State,
		semconv.LogErrorCategory, string(consent.ReasonInsufficientPermissions))
}

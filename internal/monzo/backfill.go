package monzo

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/bankhttp"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
)

const (
	// RangeLength is the longest period one full-history range covers. Monzo refuses more than 365
	// days.
	RangeLength = 120 * 24 * time.Hour
	// rangeLead is how far before an account's created time its first range starts.
	rangeLead = 24 * time.Hour

	kindBackfill = "backfill"
	kindMeta     = "meta"

	outcomeOK      = "ok"
	outcomePartial = "partial"
	outcomeFailed  = "failed"

	categoryWindowClosed = "window_closed"
	// categoryTimestampGroup marks a listing that stepped past a full page of rows sharing one
	// timestamp: the bank skipped the rest of that group, so the listing is incomplete.
	categoryTimestampGroup = "timestamp_group_over_page"
)

// rangeJob is one range to fetch in a full-history run.
type rangeJob struct {
	win store.BackfillWindow
	// before is the end the attempt asks up to: the slot's end, or the run's now for the last
	// slot, whose end lies in the future.
	before time.Time
	// refetch is set for a range that was already complete: it is fetched again from its start.
	refetch bool
}

// runTally collects what a run did, from several goroutines.
type runTally struct {
	requests atomic.Int64
	mu       sync.Mutex
	rows     int
	pages    int
	partial  bool
	category string
}

func (t *runTally) add(pages, rows int) {
	t.mu.Lock()
	t.pages += pages
	t.rows += rows
	t.mu.Unlock()
}

// fail marks the run partial and keeps the first category.
func (t *runTally) fail(category string) {
	t.mu.Lock()
	t.partial = true
	if t.category == "" {
		t.category = category
	}
	t.mu.Unlock()
}

// planRanges records every account's ranges on a fixed grid, so every run finds the same rows
// again: slot k of an account is [start + k*RangeLength, start + (k+1)*RangeLength), where start
// is one day before the account was created, for every slot that starts before now. The last
// slot ends in the future and is fetched up to now. It returns the order to fetch them in:
// unfinished ranges (including any left by an earlier run) oldest first, then ranges already
// complete newest first, to be fetched again.
func (w *Worker) planRanges(ctx context.Context, accts []account, now time.Time) ([]rangeJob, error) {
	q := w.st.DB()
	var all []store.BackfillWindow
	for _, a := range accts {
		start := a.Created.Add(-rangeLead).UTC().Truncate(time.Microsecond)
		for s := start; s.Before(now); s = s.Add(RangeLength) {
			if _, err := store.UpsertBackfillWindow(ctx, q, a.ID, s, s.Add(RangeLength)); err != nil {
				return nil, err
			}
		}
		ws, err := store.ListBackfillWindows(ctx, q, a.ID)
		if err != nil {
			return nil, err
		}
		for _, win := range ws {
			if win.PeriodStart.Before(now) {
				all = append(all, win)
			}
		}
	}
	var unfinished, done []rangeJob
	for _, win := range all {
		if win.CompletedAt == nil {
			unfinished = append(unfinished, rangeJob{win: win, before: earliest(win.PeriodEnd, now)})
		} else {
			done = append(done, rangeJob{win: win, refetch: true, before: earliest(win.PeriodEnd, now)})
		}
	}
	slices.SortStableFunc(unfinished, func(a, b rangeJob) int {
		return cmp.Or(a.win.PeriodStart.Compare(b.win.PeriodStart), cmp.Compare(a.win.AccountKey, b.win.AccountKey),
			cmp.Compare(a.win.ID, b.win.ID))
	})
	slices.SortStableFunc(done, func(a, b rangeJob) int {
		return cmp.Or(b.win.PeriodStart.Compare(a.win.PeriodStart), cmp.Compare(a.win.AccountKey, b.win.AccountKey),
			cmp.Compare(a.win.ID, b.win.ID))
	})
	return append(unfinished, done...), nil
}

// fetchRange makes one attempt at a range under its own raw.fetch. It does nothing when the
// deadline has already passed.
func (w *Worker) fetchRange(ctx context.Context, runID int64, job rangeJob, deadline time.Time, tally *runTally) error {
	if !w.now().Before(deadline) {
		if !job.refetch {
			tally.fail(categoryWindowClosed)
		}
		return nil
	}
	q := w.st.DB()
	win := job.win
	acct, before := win.AccountKey, job.before
	listFrom, cursor, cursorTime := win.PeriodStart, formatTime(win.PeriodStart), win.PeriodStart
	fromID := false
	if !job.refetch && win.Cursor != nil && *win.Cursor != "" {
		cursor = *win.Cursor
		if t, ok := parseTime(cursor); ok {
			listFrom, cursorTime = t, t
		} else {
			// An id cursor was left by an id step, which skipped rows of its timestamp group.
			fromID, cursorTime = true, time.Time{}
		}
	}
	// Invariant: a fetch that gets a raw.fetch_complete covers exactly what it listed.
	// PeriodStart is the first instant asked for (the range start, or the time cursor it resumes
	// from, inclusive) and PeriodEnd the before it used. The ledger treats a completed fetch as
	// the full listing of its period and marks anything else in it disappeared. A fetch that
	// starts from an id cursor or steps by id is never completed.
	fetchID, err := store.InsertFetch(ctx, q, store.Fetch{
		Source: Source, Kind: kindBackfill, AccountKey: &acct, PeriodStart: &listFrom, PeriodEnd: &before,
		RunID: runID, StartedAt: w.now(),
	})
	if err != nil {
		return err
	}
	res, err := w.pageTransactions(ctx, fetchID, acct, cursor, cursorTime, before, deadline,
		func(ctx context.Context, next string) error { return store.SetBackfillCursor(ctx, q, win.ID, next) })
	tally.add(res.pages, res.rows)
	switch {
	case err != nil:
		if !isBankFailure(err) || hasCategory(err, bankhttp.CategoryForbiddenInsufficientPermissions) {
			return err
		}
		// forbidden_verification_required (the window has closed) and any other failure end the
		// range as unfinished.
		if !job.refetch {
			tally.fail(category(err))
		}
		w.log.WarnContext(ctx, "monzo range ended unfinished", semconv.LogFetchID, fetchID,
			semconv.LogErrorCategory, category(err), semconv.LogPages, res.pages)
		return nil
	case res.complete && (res.idStep || fromID):
		// Every page arrived, but rows of a timestamp group larger than a page were skipped: the
		// range stays unfinished, which is the truth.
		tally.fail(categoryTimestampGroup)
		return nil
	case res.complete:
		if _, err := store.InsertFetchComplete(ctx, q, store.FetchComplete{
			FetchID: fetchID, CompletedAt: res.completedAt, Pages: res.pages,
		}); err != nil {
			return err
		}
		return store.CompleteBackfillWindow(ctx, q, win.ID, res.completedAt)
	default:
		if !job.refetch {
			tally.fail(categoryWindowClosed)
		}
		return nil
	}
}

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// runBackfill runs the full-history fetch for consent id: it holds the writer lock for the whole
// run, commits every page as it arrives, and catches the projection up once at the end. The
// deadline is the consent's token issue time plus the window, so a restart keeps it.
func (w *Worker) runBackfill(ctx context.Context, consentID int64) error {
	w.backfilling.Add(1)
	defer w.backfilling.Add(-1)
	cons, err := store.GetConsent(ctx, w.st.DB(), consentID)
	if err != nil {
		return err
	}
	if cons.State != store.ConsentActive || !cons.IsCurrent {
		return nil
	}
	deadline := cons.TokenIssuedAt.Add(w.opts.Window)
	return w.st.HoldWriterLock(ctx, func(ctx context.Context, conn *pgx.Conn) error {
		return w.backfillLocked(ctx, conn, cons, deadline)
	})
}

func (w *Worker) backfillLocked(ctx context.Context, conn *pgx.Conn, cons store.Consent, deadline time.Time) error {
	q := w.st.DB()
	runID, err := store.StartRun(ctx, q, Source, kindBackfill, w.now())
	if err != nil {
		return err
	}
	tally := &runTally{}
	runErr := w.backfillSteps(withTally(ctx, tally), runID, deadline, tally)
	outcome := outcomeOK
	switch {
	case runErr != nil:
		outcome = outcomeFailed
		tally.fail(category(runErr))
	case tally.partial:
		outcome = outcomePartial
	}
	if hasCategory(runErr, bankhttp.CategoryForbiddenInsufficientPermissions) {
		w.markLostPermissions(ctx, cons.ID)
	}
	finishErr := w.finishRun(ctx, runID, kindBackfill, outcome, tally)
	_, projErr := w.proj.CatchUp(ctx, conn, "core")
	w.log.InfoContext(ctx, "monzo full-history fetch finished", semconv.LogRunID, runID, semconv.LogOutcome, outcome,
		semconv.LogPages, tally.pages, semconv.LogRows, tally.rows)
	return errors.Join(runErr, finishErr, projErr)
}

// backfillSteps lists the accounts, reads balances, fetches the ranges and reads balances, pots
// and webhooks again. Only a failure to list the accounts, a store failure or lost permissions
// is returned; anything else marks the run partial.
func (w *Worker) backfillSteps(ctx context.Context, runID int64, deadline time.Time, tally *runTally) error {
	q := w.st.DB()
	listID, err := store.InsertFetch(ctx, q, store.Fetch{Source: Source, Kind: kindBackfill, RunID: runID, StartedAt: w.now()})
	if err != nil {
		return err
	}
	accts, err := w.api.accounts(withFetch(ctx, listID))
	if err != nil {
		return err
	}

	// One fetch per account for its balance, pot and webhook reads, so the two balance readings
	// share a run and an account for the reconciliation check.
	meta := make(map[string]int64, len(accts))
	for _, a := range accts {
		id := a.ID
		fid, err := store.InsertFetch(ctx, q, store.Fetch{Source: Source, Kind: kindBackfill, AccountKey: &id, RunID: runID, StartedAt: w.now()})
		if err != nil {
			return err
		}
		meta[a.ID] = fid
		if a.Closed {
			continue
		}
		if err := w.readStep(withFetch(ctx, fid), tally, func(ctx context.Context) error { return w.api.balance(ctx, id) }); err != nil {
			return err
		}
	}

	// The ranges' end is taken after the opening balances, so a transaction the opening balance
	// already includes is inside the fetched period.
	now := w.now().UTC().Truncate(time.Microsecond)
	jobs, err := w.planRanges(ctx, accts, now)
	if err != nil {
		return err
	}
	if err := w.runRanges(ctx, runID, jobs, deadline, tally); err != nil {
		return err
	}

	for _, a := range accts {
		id := a.ID
		fctx := withFetch(ctx, meta[a.ID])
		steps := []func(context.Context) error{
			func(ctx context.Context) error { return w.api.balance(ctx, id) },
			func(ctx context.Context) error { return w.api.pots(ctx, id) },
		}
		if !a.Closed {
			steps = append(steps, func(ctx context.Context) error { return w.api.webhooks(ctx, id) })
		}
		for _, step := range steps {
			if err := w.readStep(fctx, tally, step); err != nil {
				return err
			}
		}
	}
	return nil
}

// readStep runs one read that the run can do without. A bank failure other than lost permissions
// marks the run partial; anything else (lost permissions, a lost consent, a store failure) is
// returned.
func (w *Worker) readStep(ctx context.Context, tally *runTally, step func(context.Context) error) error {
	err := step(ctx)
	if err == nil {
		return nil
	}
	if !isBankFailure(err) || hasCategory(err, bankhttp.CategoryForbiddenInsufficientPermissions) {
		return err
	}
	tally.fail(category(err))
	return nil
}

// runRanges fetches jobs in order, Concurrency at a time, until they are done or the deadline
// passes.
func (w *Worker) runRanges(ctx context.Context, runID int64, jobs []rangeJob, deadline time.Time, tally *runTally) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := make(chan rangeJob)
	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	for range max(w.opts.Concurrency, 1) {
		wg.Go(func() {
			for job := range ch {
				if err := w.fetchRange(ctx, runID, job, deadline, tally); err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					cancel()
				}
			}
		})
	}
feed:
	for _, j := range jobs {
		select {
		case ch <- j:
		case <-ctx.Done():
			break feed
		}
	}
	close(ch)
	wg.Wait()
	return firstErr
}

// finishRun records the end of a run and counts it.
func (w *Worker) finishRun(ctx context.Context, runID int64, kind, outcome string, tally *runTally) error {
	var cat *string
	if tally.category != "" {
		c := tally.category
		cat = &c
	}
	err := store.FinishRun(context.WithoutCancel(ctx), w.st.DB(), runID, store.RunResult{
		FinishedAt: w.now(), Outcome: outcome, Requests: int(tally.requests.Load()), RowsSeen: tally.rows, ErrorCategory: cat,
	})
	w.metrics.SyncRun(ctx, Source, kind, outcome)
	return err
}

package monzo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/codes"

	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
)

// A run that hits its deadline is a timeout, not a cancellation: the state gauges skip a
// cancelled run (a deploy), so a deadline recorded as one would hide a real failure.
func TestCategoryOfAContextError(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"deadline":         {context.DeadlineExceeded, "timeout"},
		"wrapped deadline": {fmt.Errorf("store: write: %w", context.DeadlineExceeded), "timeout"},
		"cancelled":        {context.Canceled, "cancelled"},
		"wrapped canceled": {fmt.Errorf("store: write: %w", context.Canceled), "cancelled"},
		"other":            {errors.New("boom"), "internal"},
	} {
		if got := category(tc.err); got != tc.want {
			t.Errorf("%s: category = %q, want %q", name, got, tc.want)
		}
	}
}

// Serve stopping while a bank call is in flight is not a failure of the bank: the run is recorded
// cancelled, whatever category the interrupted call reported (a cut-off attempt reads network),
// so the run before it stands on the state gauges.
func TestShutdownDuringARunRecordsItCancelled(t *testing.T) {
	t.Parallel()
	rig := newSpanRig(t)
	h := rig.harness(t, nil)
	openAccount(h, "acc_test_1", 90)
	h.connect(nil)
	h.fake.Advance(time.Hour)
	rig.mem.Reset()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	h.mu.Lock()
	h.onRequest = func(r *http.Request) {
		if r.URL.Path == pathTransactions {
			stop()
			time.Sleep(50 * time.Millisecond)
		}
	}
	h.mu.Unlock()
	if err := h.w.RunScheduled(ctx, KindHourly); err == nil {
		t.Fatal("a run stopped mid-fetch reported no error")
	}
	run := h.lastRun(KindHourly)
	if run.Outcome == nil || *run.Outcome == outcomeOK || run.ErrorCategory == nil || *run.ErrorCategory != "cancelled" {
		t.Fatalf("run = %+v, want a run that did not end ok, cancelled", run)
	}
	runs := rig.runs(KindHourly)
	if len(runs) != 1 || spanAttrs(runs[0])[semconv.SpanErrorType] != "cancelled" {
		t.Errorf("sync.run spans %v, want one with error.type cancelled", runs)
	}
}

// The category recorded for a run that did not end ok follows the context it ran in: when that
// was cancelled (serve stopping) the run is cancelled, even if the first thing to go wrong was a
// cut-off bank call that reported network (the tally keeps the first category). An ok run is never
// rewritten, and a run on a context that is merely past its deadline keeps its own category.
func TestFinishRunOfACancelledContextRecordsCancelled(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	finish := func(ctx context.Context, outcome, cat string) string {
		t.Helper()
		id, err := store.StartRun(context.Background(), h.st.DB(), Source, KindHourly, h.fake.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := h.w.finishRun(ctx, id, KindHourly, outcome, &runTally{category: cat, partial: outcome != outcomeOK}); err != nil {
			t.Fatal(err)
		}
		r := mustRun(t, h.st, id)
		if r.ErrorCategory == nil {
			return ""
		}
		return *r.ErrorCategory
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	expired, done := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer done()
	if got := finish(cancelled, outcomePartial, "network"); got != "cancelled" {
		t.Errorf("partial network run on a cancelled context = %q, want cancelled", got)
	}
	if got := finish(cancelled, outcomeFailed, "network"); got != "cancelled" {
		t.Errorf("failed network run on a cancelled context = %q, want cancelled", got)
	}
	if got := finish(cancelled, outcomeOK, ""); got != "" {
		t.Errorf("ok run on a cancelled context = %q, want no category", got)
	}
	if got := finish(expired, outcomePartial, "timeout"); got != "timeout" {
		t.Errorf("run on a context past its deadline = %q, want timeout", got)
	}
}

// A full-history fetch that never gets the writer lock (stopped while it waits) still ends its
// span, with an outcome and a failed status, so the trace is not left looking unfinished.
func TestBackfillThatNeverGetsTheLockEndsItsSpanFailed(t *testing.T) {
	t.Parallel()
	rig := newSpanRig(t)
	h := rig.harness(t, nil)
	openAccount(h, "acc_test_1", 90)
	h.connect(nil)
	rig.mem.Reset()
	id := h.consent().ID

	held, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_ = h.st.HoldWriterLock(context.Background(), store.HolderTest, func(context.Context, *pgx.Conn) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer func() { close(release); <-done }()

	ctx, stop := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer stop()
	if err := h.w.runBackfill(ctx, id); err == nil {
		t.Fatal("a backfill that never got the lock reported no error")
	}
	runs := rig.runs(kindBackfill)
	if len(runs) != 1 {
		t.Fatalf("%d backfill sync.run spans, want 1", len(runs))
	}
	a := spanAttrs(runs[0])
	if a[semconv.SpanRunOutcome] != outcomeFailed || a[semconv.SpanErrorType] == "" {
		t.Errorf("sync.run attributes %v, want outcome failed and an error.type", a)
	}
	if runs[0].Status.Code != codes.Error || runs[0].Status.Description == "" {
		t.Errorf("sync.run status %v, want an error with a category", runs[0].Status)
	}
}

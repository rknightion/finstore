package truelayer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/store"
)

// A run that hits its deadline is a timeout, not a cancellation: the state gauges skip a
// cancelled run (a deploy), so a deadline recorded as one would hide a real failure.
func TestCategoryOfAContextError(t *testing.T) {
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
	h := newHarness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	hook := func(_ http.ResponseWriter, r *http.Request) bool {
		if templateOf(r.URL.Path) == pathTransactions {
			stop()
			time.Sleep(50 * time.Millisecond)
		}
		return false
	}
	h.onRequest.Store(&hook)
	if err := w.RunScheduled(ctx); err == nil {
		t.Fatal("a run stopped mid-fetch reported no error")
	}
	run := h.lastRun(Source(ConnLloyds))
	got := ""
	if run.ErrorCategory != nil {
		got = *run.ErrorCategory
	}
	if run.Kind != KindSync || run.Outcome == nil || *run.Outcome == outcomeOK || got != "cancelled" {
		t.Fatalf("run = kind %s outcome %v category %q, want a sync run that did not end ok, cancelled", run.Kind, run.Outcome, got)
	}
}

// The category recorded for a run that did not end ok follows the context it ran in: when that
// was cancelled (serve stopping) the run is cancelled, even if the first thing to go wrong was a
// cut-off bank call that reported network. An ok run is never rewritten, and a run on a context
// that is merely past its deadline keeps its own category.
func TestFinishRunOfACancelledContextRecordsCancelled(t *testing.T) {
	h := newHarness(t)
	w := h.worker(ConnLloyds, nil)
	finish := func(ctx context.Context, outcome, cat string) string {
		t.Helper()
		id, err := store.StartRun(context.Background(), h.st.DB(), Source(ConnLloyds), KindSync, h.fake.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := w.finishRun(ctx, id, KindSync, outcome, &runTally{category: cat, partial: outcome != outcomeOK}); err != nil {
			t.Fatal(err)
		}
		r, err := store.GetRun(context.Background(), h.st.DB(), id)
		if err != nil {
			t.Fatal(err)
		}
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
	// History lost to a stop is a real loss, not a cancellation: it keeps its category.
	if got := finish(cancelled, outcomePartial, categoryHistoryFailed); got != categoryHistoryFailed {
		t.Errorf("history_failed run on a cancelled context = %q, want history_failed kept", got)
	}
	if got := finish(cancelled, outcomeOK, ""); got != "" {
		t.Errorf("ok run on a cancelled context = %q, want no category", got)
	}
	if got := finish(expired, outcomePartial, "timeout"); got != "timeout" {
		t.Errorf("run on a context past its deadline = %q, want timeout", got)
	}
}

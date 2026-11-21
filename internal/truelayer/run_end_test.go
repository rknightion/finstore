package truelayer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
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

// The run-finished log line carries the category the row, the counter and the span record. Here the
// first thing to go wrong is a dropped connection (network) and serve then stops mid-run, so the
// row says cancelled; the line must not say network.
func TestRunFinishedLogLineCarriesTheCategoryAfterTheCancelledRewrite(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	h.fake.Advance(24 * time.Hour)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	hook := func(rw http.ResponseWriter, r *http.Request) bool {
		switch templateOf(r.URL.Path) {
		case pathStandingOrders:
			if conn, _, err := rw.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
			return true
		case pathTransactions:
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
	if run.ErrorCategory == nil || *run.ErrorCategory != "cancelled" {
		t.Fatalf("run category %v, want cancelled", run.ErrorCategory)
	}
	lines := h.logs.attrs("truelayer run finished")
	if len(lines) == 0 || !strings.Contains(lines[len(lines)-1], "error_category=cancelled ") {
		t.Fatalf("run-finished log lines = %q, want the last to carry error_category=cancelled", lines)
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

// Only a category that stopping the run can itself produce (network, cancelled, internal, or none)
// is rewritten to cancelled when serve stops mid-run. A real finding earlier in the run is kept, so
// a deploy landing mid-run cannot turn a server error, a rate limit or a refused card read into a
// cancelled run that the state gauges skip.
func TestFinishRunOfACancelledContextKeepsARealFinding(t *testing.T) {
	h := newHarness(t)
	w := h.worker(ConnLloyds, nil)
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	for cat, want := range map[string]string{
		"network":             "cancelled",
		"cancelled":           "cancelled",
		"internal":            "cancelled",
		"":                    "cancelled",
		"server_error":        "server_error",
		categoryRateLimited:   categoryRateLimited,
		categoryCardsRefused:  categoryCardsRefused,
		categoryCardKeyShared: categoryCardKeyShared,
		categorySuperseded:    categorySuperseded,
		"timeout":             "timeout",
		categoryConsentLost:   categoryConsentLost,
	} {
		id, err := store.StartRun(context.Background(), h.st.DB(), Source(ConnLloyds), KindSync, h.fake.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := w.finishRun(cancelled, id, KindSync, outcomePartial, &runTally{category: cat, partial: true}); err != nil {
			t.Fatal(err)
		}
		r, err := store.GetRun(context.Background(), h.st.DB(), id)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if r.ErrorCategory != nil {
			got = *r.ErrorCategory
		}
		if got != want {
			t.Errorf("partial %q run on a cancelled context = %q, want %q", cat, got, want)
		}
	}
}

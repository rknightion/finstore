package truelayer

import (
	"context"
	"testing"
)

// Minor 7: a scheduled run that takes the writer lock between the approval and the connect fetch
// runs first; the connect still runs after it, on the exchanged token, within the daily budget.
func TestScheduledRunBetweenApprovalAndConnect(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	var schedErr error
	w.afterApprove = func(ctx context.Context) { schedErr = w.RunScheduled(ctx) }
	h.connect(w)
	if schedErr != nil {
		t.Fatalf("scheduled run: %v", schedErr)
	}
	syncs := h.runs(Source(ConnLloyds), KindSync)
	if len(syncs) != 1 {
		t.Fatalf("%d sync runs, want 1", len(syncs))
	}
	requireRun(t, syncs[0], KindSync, outcomeOK, "")
	connect := h.onlyRun(Source(ConnLloyds), KindConnect)
	requireRun(t, connect, KindConnect, outcomeOK, "")
	if connect.FinishedAt.Before(*syncs[0].FinishedAt) {
		t.Fatal("the connect finished before the scheduled run that took the lock first")
	}
	var histories int
	for _, r := range h.fake.Requests() {
		if templateOf(r.Path) == pathTransactions && r.Query.Get("from") == "2024-08-01" {
			histories++
		}
	}
	if histories != 2 {
		t.Fatalf("%d full-history calls, want one per account", histories)
	}
}

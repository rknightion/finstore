package truelayer

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/consent"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/truelayertest"
)

// #17 AC1: a Lloyds consent that Resume approves with no connect run recorded for it (the callback
// could not record one after the exchange) gets a connect run ended partial with history_failed, so
// the lost full history is alerted on. A consent that already has its run gets no second one.
func TestResumeRecordsAConnectRunForAWaitingLloydsConsentThatHasNone(t *testing.T) {
	h := newHarness(t, truelayertest.WithConsentLifetime(80*24*time.Hour))
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	ctx := context.Background()
	// The state HandleCallback leaves when the run write fails after the exchange: the consent
	// awaits approval, the token is stored, no connect run exists.
	link, _ := w.AuthoriseURL(ctx)
	code, state, _ := h.fake.Authorise(link)
	att, err := w.consents.ConsumeAttempt(ctx, h.st.DB(), state)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.tokens.exchange(ctx, code, att.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(h.runs(Source(ConnLloyds), KindConnect)); n != 0 {
		t.Fatalf("%d connect runs before the restart, want none", n)
	}

	restarted := h.worker(ConnLloyds, nil)
	if err := restarted.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	restarted.Wait()
	runs := h.runs(Source(ConnLloyds), KindConnect)
	if len(runs) != 1 {
		t.Fatalf("%d connect runs after Resume, want 1", len(runs))
	}
	requireRun(t, runs[0], KindConnect, outcomePartial, categoryHistoryFailed)
	if n := h.syncRuns(outcomePartial, categoryHistoryFailed); n != 1 {
		t.Fatalf("finstore.sync.runs{partial, history_failed} = %d, want 1", n)
	}
	if c := h.consent(Source(ConnLloyds)); c.State != store.ConsentActive {
		t.Fatalf("consent %s, want active after the approval", c.State)
	}
	// A second restart finds the consent active and the run recorded: nothing more is recorded.
	if err := h.worker(ConnLloyds, nil).Resume(ctx); err != nil {
		t.Fatalf("second Resume: %v", err)
	}
	if n := len(h.runs(Source(ConnLloyds), KindConnect)); n != 1 {
		t.Fatalf("%d connect runs after a second Resume, want 1", n)
	}
}

// A waiting Monzo consent with no run records none: Monzo's connect has no history to lose.
func TestResumeRecordsNoConnectRunForAWaitingMonzoConsent(t *testing.T) {
	h := newHarness(t)
	h.monzoFixture()
	w := h.worker(ConnMonzo, nil)
	ctx := context.Background()
	link, _ := w.AuthoriseURL(ctx)
	code, state, _ := h.fake.Authorise(link)
	att, err := w.consents.ConsumeAttempt(ctx, h.st.DB(), state)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.tokens.exchange(ctx, code, att.ID); err != nil {
		t.Fatal(err)
	}
	restarted := h.worker(ConnMonzo, nil)
	if err := restarted.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	restarted.Wait()
	if n := len(h.runs(Source(ConnMonzo), KindConnect)); n != 0 {
		t.Fatalf("%d Monzo connect runs after Resume, want none", n)
	}
}

// #17 AC2: a callback that arrives while the first Lloyds connect is fetching replaces its consent
// and cancels it mid-run. The cancelled run ends superseded (not history_failed, which would raise a
// false History Missing alert) and logs no error; the newer connect fetches the history.
func TestLloydsConnectCancelledByANewerCallbackEndsSuperseded(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	ctx := context.Background()

	var fired atomic.Bool
	var secondErr atomic.Value
	hook := func(_ http.ResponseWriter, r *http.Request) bool {
		if templateOf(r.URL.Path) != pathTransactions || !fired.CompareAndSwap(false, true) {
			return false
		}
		// The first connect is fetching: the user connects again and the callback lands.
		link, err := w.AuthoriseURL(ctx)
		if err != nil {
			secondErr.Store(err)
			return false
		}
		code, state, err := h.fake.Authorise(link)
		if err == nil {
			err = w.HandleCallback(ctx, state, code)
		}
		if err != nil {
			secondErr.Store(err)
		}
		time.Sleep(50 * time.Millisecond)
		return false
	}
	h.onRequest.Store(&hook)
	h.connect(w)
	if err, _ := secondErr.Load().(error); err != nil {
		t.Fatalf("the second callback: %v", err)
	}
	runs := h.runs(Source(ConnLloyds), KindConnect)
	if len(runs) != 2 {
		t.Fatalf("%d connect runs, want 2", len(runs))
	}
	requireRun(t, runs[0], KindConnect, outcomePartial, categorySuperseded)
	requireRun(t, runs[1], KindConnect, outcomeOK, "")
	if n := h.syncRuns(outcomePartial, categoryHistoryFailed); n != 0 {
		t.Fatalf("finstore.sync.runs{partial, history_failed} = %d, want none: a replaced connect lost nothing", n)
	}
	if errs := h.logs.errors(); len(errs) != 0 {
		t.Fatalf("a connect replaced mid-fetch logged errors: %v", errs)
	}
}

// #17 AC3: a Lloyds connect that loses its consent keeps the failed outcome and still reports its
// history missing, as history_failed. The reason the consent was lost stays on the consent.
func TestLloydsConnectThatLosesItsConsentStaysFailedWithHistoryFailed(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	w.afterApprove = func(context.Context) { h.fake.ExpireConsent(ConnLloyds) }
	h.connect(w)
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindConnect, outcomeFailed, categoryHistoryFailed)
	if n := h.syncRuns(outcomeFailed, categoryHistoryFailed); n != 1 {
		t.Fatalf("finstore.sync.runs{failed, history_failed} = %d, want 1", n)
	}
	c := h.consent(Source(ConnLloyds))
	if c.State != store.ConsentLost || c.EndReason == nil || *c.EndReason != string(consent.ReasonAccessDenied) {
		t.Fatalf("consent %s %v, want lost access_denied", c.State, c.EndReason)
	}
}

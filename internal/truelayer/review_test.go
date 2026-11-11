package truelayer

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/truelayertest"
)

// callback runs the auth link and the callback without waiting for the background work.
func (h *harness) callback(w *Worker) {
	h.t.Helper()
	ctx := context.Background()
	link, err := w.AuthoriseURL(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	code, state, err := h.fake.Authorise(link)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := w.HandleCallback(ctx, state, code); err != nil {
		h.t.Fatalf("HandleCallback: %v", err)
	}
}

// onlyRun returns the source's one run of kind.
func (h *harness) onlyRun(source, kind string) store.Run {
	h.t.Helper()
	rs := h.runs(source, kind)
	if len(rs) != 1 {
		h.t.Fatalf("%d %s runs of %s, want 1", len(rs), kind, source)
	}
	return rs[0]
}

// M1: a stop after the callback returned but before its background work took the writer lock
// leaves a connect run that a restart ends partial with history_failed, so the lost history
// alerts; the restart still approves the consent.
func TestStopBeforeTheConnectTookTheLockEndsItsRunHistoryFailed(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	release := h.holdWriterLock()
	w := h.worker(ConnLloyds, nil)
	h.callback(w)
	w.Close()
	release()

	restarted := h.worker(ConnLloyds, nil)
	if err := restarted.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	restarted.Wait()
	if c := h.consent(Source(ConnLloyds)); c.State != store.ConsentActive {
		t.Fatalf("consent %s, want active", c.State)
	}
	requireRun(t, h.onlyRun(Source(ConnLloyds), KindConnect), KindConnect, outcomePartial, categoryHistoryFailed)
}

// M2: on a Lloyds connect, anything that ends the run before every account's history arrived
// makes it partial with history_failed: a 429 on a standing orders call, or a failed account list.
func TestLloydsConnectEndedBeforeTheHistoryIsHistoryFailed(t *testing.T) {
	for name, fail := range map[string]func(f *truelayertest.Fake){
		"429 on standing orders": func(f *truelayertest.Fake) { f.Fail429(ConnLloyds, pathStandingOrders, 1) },
		"500 on accounts":        func(f *truelayertest.Fake) { f.Fail(ConnLloyds, pathAccounts, http.StatusInternalServerError, 1) },
		"500 on a balance after the history": func(f *truelayertest.Fake) {
			f.Fail(ConnLloyds, pathBalance, http.StatusInternalServerError, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.lloydsFixture()
			fail(h.fake)
			w := h.worker(ConnLloyds, nil)
			h.connect(w)
			r := h.onlyRun(Source(ConnLloyds), KindConnect)
			if name == "500 on a balance after the history" {
				// A failed balance after every history arrived is only partial.
				requireRun(t, r, KindConnect, outcomePartial, "server_error")
				return
			}
			requireRun(t, r, KindConnect, outcomePartial, categoryHistoryFailed)
			if c := h.consent(Source(ConnLloyds)); c.State != store.ConsentActive {
				t.Fatalf("consent %s", c.State)
			}
		})
	}
}

// Minor 1: an exchanged token that expired while the connect waited for the lock is not used: the
// run ends history_failed and the consent stays active.
func TestExpiredExchangedTokenEndsTheConnectWithoutLosingTheConsent(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	release := h.holdWriterLock()
	w := h.worker(ConnLloyds, nil)
	h.callback(w)
	h.fake.Advance(61 * time.Minute)
	release()
	w.Wait()
	if c := h.consent(Source(ConnLloyds)); c.State != store.ConsentActive {
		t.Fatalf("consent %s %v, want active", c.State, c.EndReason)
	}
	requireRun(t, h.onlyRun(Source(ConnLloyds), KindConnect), KindConnect, outcomePartial, categoryHistoryFailed)
	for _, r := range h.fake.Requests() {
		if templateOf(r.Path) == pathAccounts {
			t.Fatalf("the connect called %s on an expired token", r.Path)
		}
	}
}

// Minor 3: a Lloyds connect cancelled by Close part-way ends partial with history_failed.
func TestLloydsConnectCancelledByCloseIsHistoryFailed(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	hook := func(_ http.ResponseWriter, r *http.Request) bool {
		if templateOf(r.URL.Path) == pathStandingOrders {
			go w.Close()
			time.Sleep(200 * time.Millisecond)
		}
		return false
	}
	h.onRequest.Store(&hook)
	h.callback(w)
	w.Wait()
	requireRun(t, h.onlyRun(Source(ConnLloyds), KindConnect), KindConnect, outcomePartial, categoryHistoryFailed)
}

// Minor 4: a second callback cancels the first's background work; the second consent ends
// active, its connect runs, the first connect's run is ended as superseded, and nothing is logged
// as an error.
func TestSecondCallbackSupersedesTheFirstQuietly(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	release := h.holdWriterLock()
	w := h.worker(ConnLloyds, nil)
	h.callback(w)
	first := h.consent(Source(ConnLloyds))
	h.callback(w)
	release()
	w.Wait()
	second := h.consent(Source(ConnLloyds))
	if second.ID == first.ID || second.State != store.ConsentActive {
		t.Fatalf("current consent %d %s, want the second, active", second.ID, second.State)
	}
	if c, _ := store.GetConsent(context.Background(), h.st.DB(), first.ID); c.State != store.ConsentReplaced {
		t.Fatalf("first consent %s, want replaced", c.State)
	}
	rs := h.runs(Source(ConnLloyds), KindConnect)
	if len(rs) != 2 {
		t.Fatalf("%d connect runs, want 2", len(rs))
	}
	requireRun(t, rs[0], KindConnect, outcomePartial, categorySuperseded)
	requireRun(t, rs[1], KindConnect, outcomeOK, "")
	if e := h.logs.errors(); len(e) != 0 {
		t.Fatalf("logged at ERROR: %v", e)
	}
}

// Minor 2: a call that failed in transport may still have reached the bank, so it counts towards
// the daily limit: it is recorded with status 0 and no body.
func TestTransportFailureCountsTowardsTheDailyLimit(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	dropped := false
	hook := func(w http.ResponseWriter, r *http.Request) bool {
		if templateOf(r.URL.Path) == pathBalance && !dropped {
			dropped = true
			// A truncated body: the request reached the bank and no whole response came back. (A
			// connection closed before any response would be retried by net/http itself.)
			if hj, ok := w.(http.Hijacker); ok {
				if conn, buf, err := hj.Hijack(); err == nil {
					_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"res")
					_ = buf.Flush()
					_ = conn.Close()
				}
			}
			return true
		}
		return false
	}
	h.onRequest.Store(&hook)
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	n, err := store.CountCallsToday(context.Background(), h.st.DB(), Source(ConnLloyds), pathBalance, refJoint, h.fake.Now().In(h.loc))
	if err != nil || n != 1 {
		t.Fatalf("balance calls counted today = %d, %v; want the dropped one", n, err)
	}
	var status int
	if err := h.st.DB().QueryRow(context.Background(), `SELECT status FROM raw.api_response
		WHERE endpoint = $1 AND params->>'bank_ref' = $2`, pathBalance, refJoint).Scan(&status); err != nil || status != 0 {
		t.Fatalf("dropped call stored with status %d, %v; want 0", status, err)
	}
}

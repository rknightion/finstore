package truelayer

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/consent"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
)

// scaWindow is how long after a connect the fake's mandate reads still work.
const scaWindow = 10 * time.Minute

func (h *harness) liveMandates() (sos, dds int) {
	h.t.Helper()
	if err := h.st.DB().QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM core.standing_order WHERE disappeared_at IS NULL),
		(SELECT count(*) FROM core.direct_debit WHERE disappeared_at IS NULL)`).Scan(&sos, &dds); err != nil {
		h.t.Fatal(err)
	}
	return sos, dds
}

// A Lloyds scheduled sync outside the SCA window: both mandate reads answer 403 sca_exceeded. The
// run stays ok, the stored mandates stay (even one the bank has since cancelled, which a refreshed
// list would have ended), the consent is untouched, the rest of the run is made, and each refused
// read is counted as sca_required.
func TestScheduledScaExceededOnMandatesKeepsTheMandatesAndTheRunOK(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	h.fake.SetSCAWindow(ConnLloyds, scaWindow)
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindConnect, outcomeOK, "")
	if sos, dds := h.liveMandates(); sos != 1 || dds != 1 {
		t.Fatalf("after the connect %d standing orders and %d direct debits, want 1 and 1", sos, dds)
	}
	before := h.consent(Source(ConnLloyds))

	// The bank cancels the standing order; a successful read would end it here.
	h.fake.RemoveStandingOrders(ConnLloyds, "acc-joint", "MADE UP SAVER")
	h.fake.Advance(6 * time.Hour)
	from := len(h.fake.Requests())
	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatalf("RunScheduled: %v", err)
	}
	run := h.lastRun(Source(ConnLloyds))
	requireRun(t, run, KindSync, outcomeOK, "")

	if sos, dds := h.liveMandates(); sos != 1 || dds != 1 {
		t.Errorf("after the refused reads %d standing orders and %d direct debits are live, want the stored 1 and 1", sos, dds)
	}
	after := h.consent(Source(ConnLloyds))
	if after.State != store.ConsentActive || after.ID != before.ID || after.EndReason != nil {
		t.Errorf("consent %+v after the sync, want the same active consent %+v", after, before)
	}
	if after.LastSyncOKAt == nil || (before.LastSyncOKAt != nil && !after.LastSyncOKAt.After(*before.LastSyncOKAt)) {
		t.Errorf("last_sync_ok %v after the sync, was %v: an ok run must set it", after.LastSyncOKAt, before.LastSyncOKAt)
	}

	// Each of the two accounts' two mandate reads was made once and counted as sca_required.
	for _, ep := range []string{pathStandingOrders, pathDirectDebits} {
		if n := h.optional(ep, semconv.ResultSCARequired); n != 2 {
			t.Errorf("optional_reads{%s, sca_required} = %d, want 2", ep, n)
		}
		if n := h.optional(ep, "failed") + h.optional(ep, "refused"); n != 0 {
			t.Errorf("optional_reads{%s} counted %d as failed or refused", ep, n)
		}
	}
	if n := h.syncRuns(outcomeOK, ""); n != 2 {
		t.Errorf("sync.runs{ok} = %d, want the connect and the sync", n)
	}
	if n := h.syncRuns(outcomePartial, ""); n != 0 {
		t.Errorf("sync.runs{partial} = %d, want 0", n)
	}
	var paths []string
	for _, r := range h.requests(from) {
		paths = append(paths, templateOf(r.Path))
	}
	for _, want := range []string{pathTransactions, pathBalance, pathPending} {
		if !slices.Contains(paths, want) {
			t.Errorf("the sync never asked for %s: %v", want, paths)
		}
	}
	const msg = "truelayer mandate read needs a fresh authentication"
	if got := h.logs.messages(slog.LevelInfo); !slices.Contains(got, msg) {
		t.Errorf("info lines %v lack %q", got, msg)
	}
	if got := h.logs.messages(slog.LevelWarn); slices.Contains(got, msg) {
		t.Errorf("the expected refusal was logged as a warning: %v", got)
	}
	for _, a := range h.logs.attrs(msg) {
		if !strings.Contains(a, semconv.LogEndpoint+"=/data/v1/accounts/{id}/") || strings.Contains(a, "sca_exceeded") {
			t.Errorf("log attributes %q: want the endpoint template and no bank text", a)
		}
	}
}

// The same refusal during a connect is not exempt: the run is partial with unexpected_status, as it
// was before, and nothing is counted as sca_required.
func TestConnectScaExceededOnMandatesStillMakesTheRunPartial(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	h.fake.SetSCAWindow(ConnLloyds, 0)
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindConnect, outcomePartial, "unexpected_status")
	if c := h.consent(Source(ConnLloyds)); c.State != store.ConsentActive {
		t.Errorf("consent %s, want active", c.State)
	}
	for _, ep := range []string{pathStandingOrders, pathDirectDebits} {
		if n := h.optional(ep, semconv.ResultSCARequired); n != 0 {
			t.Errorf("optional_reads{%s, sca_required} = %d on a connect, want 0", ep, n)
		}
	}
}

// Any other 403 on a scheduled sync, and sca_exceeded on any other endpoint, keeps the old
// behaviour: the run is partial and nothing is counted as sca_required. None of them ends the
// consent.
func TestScheduledOtherRefusalsStillMakeTheRunPartial(t *testing.T) {
	for name, tc := range map[string]struct {
		endpoint, code, category string
	}{
		"another bank code on standing orders": {pathStandingOrders, "forbidden.something_else", "unexpected_status"},
		"no bank code on direct debits":        {pathDirectDebits, "", "unexpected_status"},
		"insufficient permissions":             {pathDirectDebits, "forbidden.insufficient_permissions", "forbidden_insufficient_permissions"},
		"sca_exceeded on the balance":          {pathBalance, "sca_exceeded", "unexpected_status"},
		"sca_exceeded on transactions":         {pathTransactions, "sca_exceeded", "unexpected_status"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.lloydsFixture()
			w := h.worker(ConnLloyds, nil)
			h.connect(w)
			h.fake.Advance(6 * time.Hour)
			h.fake.FailWith(ConnLloyds, tc.endpoint, 403, tc.code, 1)
			if err := w.RunScheduled(context.Background()); err != nil {
				t.Fatalf("RunScheduled: %v", err)
			}
			requireRun(t, h.lastRun(Source(ConnLloyds)), KindSync, outcomePartial, tc.category)
			if c := h.consent(Source(ConnLloyds)); c.State != store.ConsentActive {
				t.Errorf("consent %s, want active", c.State)
			}
			if n := h.optional(tc.endpoint, semconv.ResultSCARequired); n != 0 {
				t.Errorf("optional_reads{%s, sca_required} = %d, want 0", tc.endpoint, n)
			}
		})
	}
}

// sca_exceeded is never consent loss, even where the refusal is not exempt: a connect-time 403
// leaves the consent active (above) and a lost-consent 403 still ends it.
func TestAccessDeniedOnAMandateStillEndsTheConsent(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	h.fake.SetSCAWindow(ConnLloyds, scaWindow)
	h.fake.Advance(6 * time.Hour)
	h.fake.ExpireConsent(ConnLloyds)
	if err := w.RunScheduled(context.Background()); err == nil {
		t.Fatal("RunScheduled succeeded on an expired consent")
	}
	c := h.consent(Source(ConnLloyds))
	if c.State != store.ConsentLost || c.EndReason == nil || *c.EndReason != string(consent.ReasonAccessDenied) {
		t.Fatalf("consent %s %v, want lost access_denied", c.State, c.EndReason)
	}
}

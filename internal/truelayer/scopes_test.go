package truelayer

import (
	"context"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/consent"
	"github.com/rknightion/finstore/internal/truelayertest"
)

// scopesWithout is every scope a connect asks for except those named.
func scopesWithout(drop ...string) []string {
	var out []string
	for _, s := range []string{"info", "accounts", "balance", "transactions", "direct_debits", "standing_orders", "offline_access", "cards"} {
		keep := true
		for _, d := range drop {
			keep = keep && s != d
		}
		if keep {
			out = append(out, s)
		}
	}
	return out
}

// A consent whose /me lacks info is never asked for /info, on a connect or on a scheduled run.
func TestNoInfoCallWhenMeLacksTheInfoScope(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	h.fake.SetScopes(ConnLloyds, scopesWithout("info")...)
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindConnect, outcomeOK, "")
	h.fake.Advance(24 * time.Hour)
	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindSync, outcomeOK, "")
	if n := h.fake.Count(pathInfo); n != 0 {
		t.Fatalf("/info called %d times under a consent without the info scope", n)
	}
	// The scopes come from the current /me, not from an earlier one: granting info makes the next
	// scheduled run read it.
	h.fake.SetScopes(ConnLloyds, scopesWithout()...)
	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := h.fake.Count(pathInfo); n != 1 {
		t.Fatalf("/info called %d times after the scope was granted, want 1", n)
	}
}

// A /me that cannot be read grants nothing: a scheduled run whose /me fails makes no /info call
// even though the earlier consent's stored /me listed info.
func TestNoInfoCallWhenMeFailsOnAScheduledRun(t *testing.T) {
	h := newHarness(t)
	w := h.connectedNextDay()
	h.fake.FailMe(ConnLloyds, 1)
	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The failed /me is a ledger-side failure of the run (it is not optional), nothing else is.
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindSync, outcomePartial, "server_error")
	if n := h.fake.Count(pathInfo); n != 1 {
		t.Fatalf("/info called %d times, want only the connect's", n)
	}
}

// On a reconnect whose /me fails (the retry too), the run that follows the callback treats info and
// cards as not granted, even though the earlier consent's stored /me listed them, and ends partial
// with scopes_unknown so that the skipped card reads are visible (#27 AC3).
func TestNoInfoCallOnAReconnectWhoseMeFails(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	if n := h.fake.Count(pathInfo); n != 1 {
		t.Fatalf("the first connect made %d /info calls, want 1", n)
	}
	h.fake.Advance(24 * time.Hour)
	h.fake.FailMe(ConnLloyds, meAttempts)
	meBefore, cardsBefore := h.fake.Count(pathMe), h.fake.Count(pathCards)
	h.connect(w)
	c := h.consent(Source(ConnLloyds))
	if c.ExpectedExpiryAt == nil || !c.ExpectedExpiryAt.Equal(h.fake.Now().Add(consent.Lifetime)) {
		t.Fatalf("expiry %v, want 90 days from now after a failed /me", c.ExpectedExpiryAt)
	}
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindConnect, outcomePartial, categoryScopesUnknown)
	if n := h.syncRuns(outcomePartial, categoryScopesUnknown); n != 1 {
		t.Fatalf("finstore.sync.runs{partial, scopes_unknown} = %d, want 1", n)
	}
	if n := h.fake.Count(pathMe) - meBefore; n != meAttempts {
		t.Fatalf("the reconnect read /me %d times, want %d (one attempt and the retry)", n, meAttempts)
	}
	if n := h.fake.Count(pathCards) - cardsBefore; n != 0 {
		t.Fatalf("the reconnect called /cards %d times with its scopes unknown, want none", n)
	}
	if n := h.fake.Count(pathInfo); n != 1 {
		t.Fatalf("/info called %d times, want no call on the reconnect", n)
	}
}

// A /me that fails once is read again within the connect: the run has the scopes of the answer, the
// consent the expiry /me reports, and the run ends ok (#27 AC3).
func TestAFailedMeIsRetriedWithinTheConnect(t *testing.T) {
	h := newHarness(t, truelayertest.WithConsentLifetime(80*24*time.Hour))
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	h.fake.FailMe(ConnLloyds, 1)
	h.connect(w)
	if n := h.fake.Count(pathMe); n != 2 {
		t.Fatalf("/me read %d times, want the failed attempt and one retry", n)
	}
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindConnect, outcomeOK, "")
	c := h.consent(Source(ConnLloyds))
	if c.ExpectedExpiryAt == nil || !c.ExpectedExpiryAt.Equal(h.fake.ConsentEnd(ConnLloyds)) {
		t.Fatalf("expiry %v, want the end /me reported on the retry", c.ExpectedExpiryAt)
	}
	if n := h.fake.Count(pathInfo); n != 1 {
		t.Fatalf("/info called %d times, want 1: the retried /me lists the info scope", n)
	}
	if n := h.syncRuns(outcomePartial, categoryScopesUnknown); n != 0 {
		t.Fatalf("finstore.sync.runs{partial, scopes_unknown} = %d, want none", n)
	}
}

// A Monzo connect reads no ledger data from the cards scope, so a /me that cannot be read costs
// nothing a run could show: the run stays ok.
func TestMonzoConnectWhoseMeFailsStaysOK(t *testing.T) {
	h := newHarness(t)
	h.monzoFixture()
	w := h.worker(ConnMonzo, nil)
	h.fake.FailMe(ConnMonzo, meAttempts)
	h.connect(w)
	requireRun(t, h.lastRun(Source(ConnMonzo)), KindConnect, outcomeOK, "")
}

// The connect run takes its scopes from the /me of the approval it follows.
func TestConnectRunUsesTheScopesOfItsOwnApproval(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	h.fake.Advance(24 * time.Hour)
	h.fake.SetScopes(ConnLloyds, scopesWithout("info")...)
	h.connect(w)
	if n := h.fake.Count(pathInfo); n != 1 {
		t.Fatalf("/info called %d times, want the first connect's only", n)
	}
}

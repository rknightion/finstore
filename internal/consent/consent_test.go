package consent_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/consent"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

const source = "monzo"

// normaliserRev stands in for normaliserRev: consent never touches core, and importing the
// ledger would tie these tests to a package they do not use.
const normaliserRev = 1

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func setup(t *testing.T) (store.DBTX, *consent.Manager, *clock) {
	t.Helper()
	ctx := context.Background()
	super := testpg.Start(t)
	if err := store.Bootstrap(ctx, super, "owner-secret", "app-secret"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := store.Migrate(ctx, testpg.UserURL(t, super, store.RoleOwner), normaliserRev); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	st, err := store.Open(ctx, testpg.UserURL(t, super, store.RoleApp))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(st.Close)
	c := &clock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	return st.DB(), consent.New(source, c.now), c
}

// newConsent runs a whole connect: start, consume and exchange.
func newConsent(t *testing.T, q store.DBTX, m *consent.Manager, c *clock) store.Consent {
	t.Helper()
	ctx := context.Background()
	state, err := m.StartAttempt(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	a, err := m.ConsumeAttempt(ctx, q, state)
	if err != nil {
		t.Fatalf("ConsumeAttempt: %v", err)
	}
	got, err := m.Exchanged(ctx, q, a.ID, c.now())
	if err != nil {
		t.Fatalf("Exchanged: %v", err)
	}
	return got
}

func TestAttemptStateIsAcceptedOnlyOnce(t *testing.T) {
	q, m, _ := setup(t)
	ctx := context.Background()
	state, err := m.StartAttempt(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(state) < 43 {
		t.Fatalf("state %d characters long, want at least 32 random bytes encoded", len(state))
	}
	if _, err := m.ConsumeAttempt(ctx, q, state); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if _, err := m.ConsumeAttempt(ctx, q, state); !errors.Is(err, consent.ErrAttemptRefused) {
		t.Fatalf("second consume: %v, want ErrAttemptRefused", err)
	}
}

func TestUnknownOrEmptyStateIsRefused(t *testing.T) {
	q, m, _ := setup(t)
	ctx := context.Background()
	if _, err := m.StartAttempt(ctx, q); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"", "not-a-state-we-issued"} {
		if _, err := m.ConsumeAttempt(ctx, q, s); !errors.Is(err, consent.ErrAttemptRefused) {
			t.Fatalf("state %q: %v, want ErrAttemptRefused", s, err)
		}
	}
}

func TestExpiredAttemptIsRefused(t *testing.T) {
	q, m, c := setup(t)
	ctx := context.Background()
	state, err := m.StartAttempt(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(time.Hour)
	if _, err := m.ConsumeAttempt(ctx, q, state); !errors.Is(err, consent.ErrAttemptRefused) {
		t.Fatalf("consume after an hour: %v, want ErrAttemptRefused", err)
	}

	fresh, err := m.StartAttempt(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(59 * time.Minute)
	if _, err := m.ConsumeAttempt(ctx, q, fresh); err != nil {
		t.Fatalf("consume at 59 minutes: %v", err)
	}
}

func TestExchangeCreatesCurrentConsentAwaitingApprovalAndReplacesThePrevious(t *testing.T) {
	q, m, c := setup(t)
	ctx := context.Background()
	issued := c.now()
	first := newConsent(t, q, m, c)
	if first.State != store.ConsentAwaitingApproval || !first.IsCurrent || !first.TokenIssuedAt.Equal(issued) {
		t.Fatalf("first consent = %+v", first)
	}
	if _, err := m.Approved(ctx, q, first.ID); err != nil {
		t.Fatal(err)
	}

	c.advance(time.Hour)
	second := newConsent(t, q, m, c)
	old, err := store.GetConsent(ctx, q, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.State != store.ConsentReplaced || old.IsCurrent || old.EndedAt == nil {
		t.Fatalf("previous consent = %+v, want replaced and not current", old)
	}
	cur, err := m.Current(ctx, q)
	if err != nil || cur.ID != second.ID || cur.State != store.ConsentAwaitingApproval {
		t.Fatalf("Current = %+v, %v; want the second consent awaiting approval", cur, err)
	}
}

func TestExchangeAfterLostConsentKeepsItLost(t *testing.T) {
	q, m, c := setup(t)
	ctx := context.Background()
	first := newConsent(t, q, m, c)
	if _, err := m.Lost(ctx, q, first.ID, consent.ReasonApprovalTimeout); err != nil {
		t.Fatal(err)
	}
	newConsent(t, q, m, c)
	old, err := store.GetConsent(ctx, q, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.State != store.ConsentLost || old.IsCurrent || old.EndReason == nil || *old.EndReason != string(consent.ReasonApprovalTimeout) {
		t.Fatalf("lost consent after reconnect = %+v", old)
	}
}

func TestApprovalMakesConsentActiveFor90Days(t *testing.T) {
	q, m, c := setup(t)
	ctx := context.Background()
	cons := newConsent(t, q, m, c)
	c.advance(30 * time.Second)
	approvedAt := c.now()
	got, err := m.Approved(ctx, q, cons.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := approvedAt.AddDate(0, 0, 90)
	if got.State != store.ConsentActive || got.ApprovedAt == nil || !got.ApprovedAt.Equal(approvedAt) ||
		got.ExpectedExpiryAt == nil || !got.ExpectedExpiryAt.Equal(want) {
		t.Fatalf("approved consent = %+v, want active with expiry %v", got, want)
	}

	c.advance(80*24*time.Hour + time.Hour)
	st, err := m.Current(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if st.DaysToExpiry == nil || *st.DaysToExpiry != 9 || !st.Expiring {
		t.Fatalf("status after 80 days = days %v expiring %v, want 9 and true", st.DaysToExpiry, st.Expiring)
	}
}

func TestEachLossReasonFromItsLegalState(t *testing.T) {
	cases := []struct {
		reason  consent.Reason
		approve bool
	}{
		{consent.ReasonApprovalTimeout, false},
		{consent.ReasonRefreshRejected, false},
		{consent.ReasonRefreshRejected, true},
		{consent.ReasonInsufficientPermissions, true},
	}
	for _, tc := range cases {
		t.Run(string(tc.reason), func(t *testing.T) {
			q, m, c := setup(t)
			ctx := context.Background()
			cons := newConsent(t, q, m, c)
			if tc.approve {
				if _, err := m.Approved(ctx, q, cons.ID); err != nil {
					t.Fatal(err)
				}
			}
			got, err := m.Lost(ctx, q, cons.ID, tc.reason)
			if err != nil {
				t.Fatalf("Lost: %v", err)
			}
			if got.State != store.ConsentLost || got.EndReason == nil || *got.EndReason != string(tc.reason) || got.EndedAt == nil {
				t.Fatalf("lost consent = %+v", got)
			}
		})
	}
}

func TestConsentExpiresOnlyOnceItsExpectedExpiryHasPassed(t *testing.T) {
	q, m, c := setup(t)
	ctx := context.Background()
	cons := newConsent(t, q, m, c)
	if _, err := m.Approved(ctx, q, cons.ID); err != nil {
		t.Fatal(err)
	}
	c.advance(consent.Lifetime - time.Second)
	if got, changed, err := m.ExpireIfDue(ctx, q, cons.ID); err != nil || changed || got.State != store.ConsentActive {
		t.Fatalf("one second early: %+v changed=%v %v", got, changed, err)
	}
	if _, err := m.Expired(ctx, q, cons.ID); !errors.Is(err, consent.ErrIllegalTransition) {
		t.Fatalf("Expired before due: %v, want ErrIllegalTransition", err)
	}
	c.advance(time.Second)
	got, changed, err := m.ExpireIfDue(ctx, q, cons.ID)
	if err != nil || !changed || got.State != store.ConsentExpired {
		t.Fatalf("at expiry: %+v changed=%v %v", got, changed, err)
	}
	st, err := m.Current(ctx, q)
	if err != nil || st.State != store.ConsentExpired || st.DaysToExpiry != nil {
		t.Fatalf("Current after expiry = %+v, %v", st, err)
	}
}

func TestIllegalTransitionsAreRefused(t *testing.T) {
	q, m, c := setup(t)
	ctx := context.Background()
	awaiting := newConsent(t, q, m, c)

	// Missing permissions cannot end a consent that was never approved: those 403s are expected.
	if _, err := m.Lost(ctx, q, awaiting.ID, consent.ReasonInsufficientPermissions); !errors.Is(err, consent.ErrIllegalTransition) {
		t.Fatalf("insufficient_permissions while awaiting approval: %v", err)
	}
	// A consent awaiting approval cannot expire.
	if _, err := m.Expired(ctx, q, awaiting.ID); !errors.Is(err, consent.ErrIllegalTransition) {
		t.Fatalf("expire while awaiting approval: %v", err)
	}
	if _, err := m.Approved(ctx, q, awaiting.ID); err != nil {
		t.Fatal(err)
	}
	// Approval happens once.
	if _, err := m.Approved(ctx, q, awaiting.ID); !errors.Is(err, consent.ErrIllegalTransition) {
		t.Fatalf("second approval: %v", err)
	}
	// An approval timeout cannot end an active consent.
	if _, err := m.Lost(ctx, q, awaiting.ID, consent.ReasonApprovalTimeout); !errors.Is(err, consent.ErrIllegalTransition) {
		t.Fatalf("approval_timeout while active: %v", err)
	}
	if _, err := m.Lost(ctx, q, awaiting.ID, consent.ReasonRefreshRejected); err != nil {
		t.Fatal(err)
	}
	// Nothing comes back from lost.
	if _, err := m.Approved(ctx, q, awaiting.ID); !errors.Is(err, consent.ErrIllegalTransition) {
		t.Fatalf("approve a lost consent: %v", err)
	}
	if _, err := m.Lost(ctx, q, awaiting.ID, consent.ReasonRefreshRejected); !errors.Is(err, consent.ErrIllegalTransition) {
		t.Fatalf("lose a lost consent: %v", err)
	}
	if _, err := m.Lost(ctx, q, awaiting.ID, consent.Reason("made_up")); err == nil {
		t.Fatal("unknown reason accepted")
	}

	// A replaced consent cannot be approved.
	second := newConsent(t, q, m, c)
	newConsent(t, q, m, c)
	if _, err := m.Approved(ctx, q, second.ID); !errors.Is(err, consent.ErrIllegalTransition) {
		t.Fatalf("approve a replaced consent: %v", err)
	}
}

func TestCurrentWithNoConsent(t *testing.T) {
	q, m, _ := setup(t)
	if _, err := m.Current(context.Background(), q); !errors.Is(err, consent.ErrNoConsent) {
		t.Fatalf("Current = %v, want ErrNoConsent", err)
	}
}

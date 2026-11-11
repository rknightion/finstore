package consent_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/consent"
	"github.com/rknightion/finstore/internal/store"
)

// A bank that reports the consent's expiry is approved with that expiry, not the 90 days Approved
// assumes; only a consent awaiting approval can be approved that way.
func TestApprovedUntilUsesTheGivenExpiry(t *testing.T) {
	q, m, c := setup(t)
	ctx := context.Background()
	cons := newConsent(t, q, m, c)
	c.advance(time.Minute)
	until := c.now().Add(89*24*time.Hour + 7*time.Hour)
	got, err := m.ApprovedUntil(ctx, q, cons.ID, until)
	if err != nil {
		t.Fatalf("ApprovedUntil: %v", err)
	}
	if got.State != store.ConsentActive || got.ApprovedAt == nil || !got.ApprovedAt.Equal(c.now()) ||
		got.ExpectedExpiryAt == nil || !got.ExpectedExpiryAt.Equal(until) {
		t.Fatalf("consent = %+v", got)
	}
	if _, err := m.ApprovedUntil(ctx, q, cons.ID, until); !errors.Is(err, consent.ErrIllegalTransition) {
		t.Fatalf("second approval: %v, want ErrIllegalTransition", err)
	}
}

// A data call refused for access ends an active consent; a consent still awaiting approval is not
// ended that way.
func TestAccessDeniedEndsOnlyAnActiveConsent(t *testing.T) {
	q, m, c := setup(t)
	ctx := context.Background()
	waiting := newConsent(t, q, m, c)
	if _, err := m.Lost(ctx, q, waiting.ID, consent.ReasonAccessDenied); !errors.Is(err, consent.ErrIllegalTransition) {
		t.Fatalf("awaiting approval: %v, want ErrIllegalTransition", err)
	}
	if _, err := m.Approved(ctx, q, waiting.ID); err != nil {
		t.Fatal(err)
	}
	lost, err := m.Lost(ctx, q, waiting.ID, consent.ReasonAccessDenied)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if lost.State != store.ConsentLost || lost.EndReason == nil || *lost.EndReason != "access_denied" {
		t.Fatalf("consent = %+v", lost)
	}
}

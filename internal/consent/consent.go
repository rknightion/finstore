// Package consent holds the states of a bank consent and the transitions between them, over the
// store's sync.connect_attempt and sync.consent accessors.
//
// A connect attempt is started when the owner asks to connect and is consumed once, by the OAuth
// callback that presents its state value. Exchanging the code creates a consent awaiting approval
// and replaces the previous one; approval makes it active for 90 days; it then ends as lost (with
// a reason) or expired. Every method takes the DBTX to run on, so a caller can group a transition
// with other writes in one transaction.
package consent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/store"
)

const (
	// AttemptTTL is how long a connect attempt's state value is accepted.
	AttemptTTL = time.Hour
	// Lifetime is how long an approved consent is expected to last.
	Lifetime = 90 * 24 * time.Hour
	// ExpiringWithin is how close to its expected expiry an active consent counts as expiring.
	ExpiringWithin = 14 * 24 * time.Hour

	stateBytes = 32
)

// Reason is why a consent was lost.
type Reason string

// The reasons a consent is lost.
const (
	ReasonInsufficientPermissions Reason = "insufficient_permissions"
	ReasonRefreshRejected         Reason = "refresh_rejected"
	ReasonApprovalTimeout         Reason = "approval_timeout"
)

// End reasons stored for the two other ending states.
const (
	endReasonReplaced = "reconnected"
	endReasonExpired  = "expected_expiry_passed"
)

// States lists every consent state, for the state metric.
func States() []string {
	return []string{store.ConsentAwaitingApproval, store.ConsentActive, store.ConsentReplaced,
		store.ConsentLost, store.ConsentExpired}
}

// ErrAttemptRefused is returned when a presented state value matches no attempt that is unused and
// unexpired. Nothing has changed when it is returned.
var ErrAttemptRefused = errors.New("consent: connect attempt unknown, expired or already used")

// ErrIllegalTransition is returned when a consent is not in a state the transition starts from.
var ErrIllegalTransition = errors.New("consent: illegal transition")

// ErrNoConsent is returned when the source has no current consent.
var ErrNoConsent = errors.New("consent: no current consent")

// Manager applies consent transitions for one source with an injectable clock.
type Manager struct {
	source string
	now    func() time.Time
}

// New returns a Manager for source. A nil now uses time.Now.
func New(source string, now func() time.Time) *Manager {
	if now == nil {
		now = time.Now
	}
	return &Manager{source: source, now: now}
}

// HashState is the SHA-256 stored for a state value.
func HashState(state string) []byte {
	sum := sha256.Sum256([]byte(state))
	return sum[:]
}

// StartAttempt creates a connect attempt and returns its state value: 32 random bytes, base64url
// without padding. Only the value's SHA-256 is stored, valid for AttemptTTL. Earlier unused
// attempts that have passed their expiry are given the outcome expired.
func (m *Manager) StartAttempt(ctx context.Context, q store.DBTX) (string, error) {
	buf := make([]byte, stateBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("consent: random state: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(buf)
	now := m.now()
	if _, err := store.ExpireConnectAttempts(ctx, q, now); err != nil {
		return "", err
	}
	if _, err := store.CreateConnectAttempt(ctx, q, m.source, HashState(state), now, now.Add(AttemptTTL)); err != nil {
		return "", err
	}
	return state, nil
}

// ConsumeAttempt marks the attempt that state belongs to used and returns it. An unknown, expired
// or already used state, or one issued for another source, returns ErrAttemptRefused.
func (m *Manager) ConsumeAttempt(ctx context.Context, q store.DBTX, state string) (store.ConnectAttempt, error) {
	if state == "" {
		return store.ConnectAttempt{}, ErrAttemptRefused
	}
	a, err := store.ConsumeConnectAttempt(ctx, q, HashState(state), m.now())
	if errors.Is(err, store.ErrAttemptUnusable) {
		return store.ConnectAttempt{}, ErrAttemptRefused
	}
	if err != nil {
		return store.ConnectAttempt{}, err
	}
	if a.Source != m.source {
		return store.ConnectAttempt{}, ErrAttemptRefused
	}
	return a, nil
}

// AttemptRefused records that a consumed attempt's code exchange failed.
func (m *Manager) AttemptRefused(ctx context.Context, q store.DBTX, attemptID int64) error {
	return store.SetConnectAttemptOutcome(ctx, q, attemptID, store.AttemptRefused)
}

// Exchanged records a successful code exchange for attempt attemptID, in one transaction: the
// previous current consent, when it is awaiting approval or active, becomes replaced; a new
// consent awaiting approval with tokenIssuedAt is created and made current; the attempt's outcome
// becomes exchanged. A previous consent already lost or expired keeps its state and stops being
// current. It returns the new consent.
func (m *Manager) Exchanged(ctx context.Context, q store.DBTX, attemptID int64, tokenIssuedAt time.Time) (store.Consent, error) {
	var out store.Consent
	err := pgx.BeginFunc(ctx, q, func(tx pgx.Tx) error {
		prev, err := store.CurrentConsent(ctx, tx, m.source)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		case prev.State == store.ConsentAwaitingApproval || prev.State == store.ConsentActive:
			reason := endReasonReplaced
			if _, err := store.TransitionConsent(ctx, tx, prev.ID, store.ConsentTransition{
				To: store.ConsentReplaced, At: m.now(), Reason: &reason,
				From: []string{store.ConsentAwaitingApproval, store.ConsentActive},
			}); err != nil {
				return transitionErr(err)
			}
		}
		id, err := store.CreateConsent(ctx, tx, m.source, tokenIssuedAt)
		if err != nil {
			return err
		}
		if err := store.SetCurrentConsent(ctx, tx, id); err != nil {
			return err
		}
		if err := store.SetConnectAttemptOutcome(ctx, tx, attemptID, store.AttemptExchanged); err != nil {
			return err
		}
		out, err = store.GetConsent(ctx, tx, id)
		return err
	})
	if err != nil {
		return store.Consent{}, err
	}
	return out, nil
}

// Approved makes consent id active from now, expected to expire Lifetime later. Only a consent
// awaiting approval can be approved.
func (m *Manager) Approved(ctx context.Context, q store.DBTX, id int64) (store.Consent, error) {
	now := m.now()
	expiry := now.Add(Lifetime)
	c, err := store.TransitionConsent(ctx, q, id, store.ConsentTransition{
		To: store.ConsentActive, At: now, ExpectedExpiryAt: &expiry,
		From: []string{store.ConsentAwaitingApproval},
	})
	return c, transitionErr(err)
}

// Lost ends consent id as lost for reason. An approval timeout ends only a consent awaiting
// approval; missing permissions end only an active one; a rejected refresh ends either.
func (m *Manager) Lost(ctx context.Context, q store.DBTX, id int64, reason Reason) (store.Consent, error) {
	var from []string
	switch reason {
	case ReasonApprovalTimeout:
		from = []string{store.ConsentAwaitingApproval}
	case ReasonInsufficientPermissions:
		from = []string{store.ConsentActive}
	case ReasonRefreshRejected:
		from = []string{store.ConsentAwaitingApproval, store.ConsentActive}
	default:
		return store.Consent{}, fmt.Errorf("consent: unknown loss reason %q", reason)
	}
	r := string(reason)
	c, err := store.TransitionConsent(ctx, q, id, store.ConsentTransition{
		To: store.ConsentLost, At: m.now(), Reason: &r, From: from,
	})
	return c, transitionErr(err)
}

// ExpireIfDue ends consent id as expired when it is active and its expected expiry is at or
// before now. It returns the consent as it now is and whether it changed. Any other consent is
// returned unchanged.
func (m *Manager) ExpireIfDue(ctx context.Context, q store.DBTX, id int64) (store.Consent, bool, error) {
	c, err := store.GetConsent(ctx, q, id)
	if err != nil {
		return store.Consent{}, false, err
	}
	now := m.now()
	if c.State != store.ConsentActive || c.ExpectedExpiryAt == nil || now.Before(*c.ExpectedExpiryAt) {
		return c, false, nil
	}
	c, err = m.Expired(ctx, q, id)
	if err != nil {
		return store.Consent{}, false, err
	}
	return c, true, nil
}

// Expired ends consent id as expired. Only an active consent whose expected expiry has passed can
// expire.
func (m *Manager) Expired(ctx context.Context, q store.DBTX, id int64) (store.Consent, error) {
	c, err := store.GetConsent(ctx, q, id)
	if err != nil {
		return store.Consent{}, err
	}
	now := m.now()
	if c.State != store.ConsentActive || c.ExpectedExpiryAt == nil || now.Before(*c.ExpectedExpiryAt) {
		return store.Consent{}, ErrIllegalTransition
	}
	r := endReasonExpired
	c, err = store.TransitionConsent(ctx, q, id, store.ConsentTransition{
		To: store.ConsentExpired, At: now, Reason: &r, From: []string{store.ConsentActive},
	})
	return c, transitionErr(err)
}

// Status is the current consent as the status page and the metrics see it.
type Status struct {
	store.Consent
	// DaysToExpiry is the whole days left before the expected expiry, rounded down and never below
	// zero; nil when the consent has no expected expiry or has ended.
	DaysToExpiry *int
	// Expiring is true for an active consent whose expected expiry is within ExpiringWithin.
	Expiring bool
}

// Current returns the source's current consent, or ErrNoConsent.
func (m *Manager) Current(ctx context.Context, q store.DBTX) (Status, error) {
	c, err := store.CurrentConsent(ctx, q, m.source)
	if errors.Is(err, store.ErrNotFound) {
		return Status{}, ErrNoConsent
	}
	if err != nil {
		return Status{}, err
	}
	s := Status{Consent: c}
	if c.ExpectedExpiryAt != nil && c.State == store.ConsentActive {
		left := c.ExpectedExpiryAt.Sub(m.now())
		days := max(int(left/(24*time.Hour)), 0)
		s.DaysToExpiry = &days
		s.Expiring = left < ExpiringWithin
	}
	return s, nil
}

func transitionErr(err error) error {
	if errors.Is(err, store.ErrStateConflict) {
		return fmt.Errorf("%w: %w", ErrIllegalTransition, err)
	}
	return err
}

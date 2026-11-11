package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Run is a sync.run row: one sync or import run.
type Run struct {
	ID            int64
	Source        string
	Kind          string // backfill | hourly | nightly | import | capture
	StartedAt     time.Time
	FinishedAt    *time.Time
	Outcome       *string // ok | partial | failed
	Requests      int
	RowsSeen      int
	ErrorCategory *string
}

// RunResult is what FinishRun records.
type RunResult struct {
	FinishedAt    time.Time
	Outcome       string // ok | partial | failed
	Requests      int
	RowsSeen      int
	ErrorCategory *string // a category, never text from a response
}

// StartRun inserts a run and returns its id.
func StartRun(ctx context.Context, q DBTX, source, kind string, startedAt time.Time) (int64, error) {
	var id int64
	if err := q.QueryRow(ctx, `INSERT INTO sync.run (source, kind, started_at) VALUES ($1, $2, $3) RETURNING id`,
		source, kind, startedAt).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: start run: %w", err)
	}
	return id, nil
}

// FinishRun records the end of run id.
func FinishRun(ctx context.Context, q DBTX, id int64, r RunResult) error {
	tag, err := q.Exec(ctx, `UPDATE sync.run SET finished_at = $2, outcome = $3, requests = $4, rows_seen = $5,
		error_category = $6 WHERE id = $1`, id, r.FinishedAt, r.Outcome, r.Requests, r.RowsSeen, r.ErrorCategory)
	if err != nil {
		return fmt.Errorf("store: finish run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetRun returns run id, or ErrNotFound.
func GetRun(ctx context.Context, q DBTX, id int64) (Run, error) {
	var r Run
	err := q.QueryRow(ctx, `SELECT id, source, kind, started_at, finished_at, outcome, requests, rows_seen, error_category
		FROM sync.run WHERE id = $1`, id).
		Scan(&r.ID, &r.Source, &r.Kind, &r.StartedAt, &r.FinishedAt, &r.Outcome, &r.Requests, &r.RowsSeen, &r.ErrorCategory)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("store: get run: %w", err)
	}
	return r, nil
}

// Consent states.
const (
	ConsentAwaitingApproval = "awaiting_approval"
	ConsentActive           = "active"
	ConsentReplaced         = "replaced"
	ConsentLost             = "lost"
	ConsentExpired          = "expired"
)

// Consent is a sync.consent row.
type Consent struct {
	ID               int64
	Source           string
	State            string
	TokenIssuedAt    time.Time
	ApprovedAt       *time.Time
	ExpectedExpiryAt *time.Time
	EndedAt          *time.Time
	EndReason        *string
	IsCurrent        bool
	LastSyncOKAt     *time.Time
}

// ConsentTransition is a change of a consent's state.
type ConsentTransition struct {
	To   string    // the new state
	At   time.Time // when it happened
	From []string  // when not empty, the change happens only from one of these states
	// ExpectedExpiryAt is stored with a change to ConsentActive, which also sets approved_at to At.
	ExpectedExpiryAt *time.Time
	// Reason is stored as end_reason with a change to ConsentReplaced, ConsentLost or
	// ConsentExpired, which also sets ended_at to At.
	Reason *string
}

// ErrStateConflict is returned by TransitionConsent when the consent is not in one of the From
// states.
var ErrStateConflict = errors.New("store: consent is not in an expected state")

const consentColumns = `id, source, state, token_issued_at, approved_at, expected_expiry_at, ended_at, end_reason,
	is_current, last_sync_ok_at`

func scanConsent(r pgx.Row) (Consent, error) {
	var c Consent
	err := r.Scan(&c.ID, &c.Source, &c.State, &c.TokenIssuedAt, &c.ApprovedAt, &c.ExpectedExpiryAt, &c.EndedAt,
		&c.EndReason, &c.IsCurrent, &c.LastSyncOKAt)
	return c, err
}

// CreateConsent inserts a consent in state awaiting_approval, not current, and returns its id.
func CreateConsent(ctx context.Context, q DBTX, source string, tokenIssuedAt time.Time) (int64, error) {
	var id int64
	if err := q.QueryRow(ctx, `INSERT INTO sync.consent (source, state, token_issued_at) VALUES ($1, $2, $3) RETURNING id`,
		source, ConsentAwaitingApproval, tokenIssuedAt).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: create consent: %w", err)
	}
	return id, nil
}

// SetCurrentConsent makes consent id the current one of its source, and no other.
func SetCurrentConsent(ctx context.Context, q DBTX, id int64) error {
	return pgx.BeginFunc(ctx, q, func(tx pgx.Tx) error {
		var source string
		if err := tx.QueryRow(ctx, `SELECT source FROM sync.consent WHERE id = $1 FOR UPDATE`, id).Scan(&source); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("store: set current consent: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE sync.consent SET is_current = false WHERE source = $1 AND is_current AND id <> $2`,
			source, id); err != nil {
			return fmt.Errorf("store: set current consent: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE sync.consent SET is_current = true WHERE id = $1`, id); err != nil {
			return fmt.Errorf("store: set current consent: %w", err)
		}
		return nil
	})
}

// TransitionConsent changes consent id's state as tr describes and returns the updated row. It
// returns ErrNotFound for an unknown id and ErrStateConflict when tr.From is set and the consent is
// in none of its states.
func TransitionConsent(ctx context.Context, q DBTX, id int64, tr ConsentTransition) (Consent, error) {
	from := tr.From
	if from == nil {
		from = []string{}
	}
	row := q.QueryRow(ctx, `UPDATE sync.consent SET state = $2,
		  approved_at = CASE WHEN $2 = 'active' THEN $3 ELSE approved_at END,
		  expected_expiry_at = CASE WHEN $2 = 'active' THEN $4 ELSE expected_expiry_at END,
		  ended_at = CASE WHEN $2 IN ('replaced','lost','expired') THEN $3 ELSE ended_at END,
		  end_reason = CASE WHEN $2 IN ('replaced','lost','expired') THEN $5 ELSE end_reason END
		WHERE id = $1 AND (cardinality($6::text[]) = 0 OR state = ANY($6))
		RETURNING `+consentColumns, id, tr.To, tr.At, tr.ExpectedExpiryAt, tr.Reason, from)
	c, err := scanConsent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := GetConsent(ctx, q, id); errors.Is(gerr, ErrNotFound) {
			return Consent{}, ErrNotFound
		}
		return Consent{}, ErrStateConflict
	}
	if err != nil {
		return Consent{}, fmt.Errorf("store: transition consent: %w", err)
	}
	return c, nil
}

// GetConsent returns consent id, or ErrNotFound.
func GetConsent(ctx context.Context, q DBTX, id int64) (Consent, error) {
	c, err := scanConsent(q.QueryRow(ctx, `SELECT `+consentColumns+` FROM sync.consent WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Consent{}, ErrNotFound
	}
	if err != nil {
		return Consent{}, fmt.Errorf("store: get consent: %w", err)
	}
	return c, nil
}

// CurrentConsent returns the current consent of source, or ErrNotFound.
func CurrentConsent(ctx context.Context, q DBTX, source string) (Consent, error) {
	c, err := scanConsent(q.QueryRow(ctx, `SELECT `+consentColumns+` FROM sync.consent WHERE source = $1 AND is_current`, source))
	if errors.Is(err, pgx.ErrNoRows) {
		return Consent{}, ErrNotFound
	}
	if err != nil {
		return Consent{}, fmt.Errorf("store: current consent: %w", err)
	}
	return c, nil
}

// SetConsentLastSyncOK records a successful sync under consent id.
func SetConsentLastSyncOK(ctx context.Context, q DBTX, id int64, at time.Time) error {
	tag, err := q.Exec(ctx, `UPDATE sync.consent SET last_sync_ok_at = $2 WHERE id = $1`, id, at)
	if err != nil {
		return fmt.Errorf("store: set last sync ok: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Connect attempt outcomes.
const (
	AttemptExchanged = "exchanged"
	AttemptExpired   = "expired"
	AttemptRefused   = "refused"
)

// ConnectAttempt is a sync.connect_attempt row.
type ConnectAttempt struct {
	ID          int64
	Source      string
	StateSHA256 []byte
	StartedAt   time.Time
	ExpiresAt   time.Time
	UsedAt      *time.Time
	Outcome     *string
}

// ErrAttemptUnusable is returned by ConsumeConnectAttempt for a state hash that is unknown,
// expired, already used or already given an outcome.
var ErrAttemptUnusable = errors.New("store: connect attempt unknown, expired or used")

// CreateConnectAttempt inserts an attempt for the SHA-256 of its state value and returns its id.
func CreateConnectAttempt(ctx context.Context, q DBTX, source string, stateSHA256 []byte, startedAt, expiresAt time.Time) (int64, error) {
	var id int64
	if err := q.QueryRow(ctx, `INSERT INTO sync.connect_attempt (source, state_sha256, started_at, expires_at)
		VALUES ($1, $2, $3, $4) RETURNING id`, source, stateSHA256, startedAt, expiresAt).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: create connect attempt: %w", err)
	}
	return id, nil
}

// ConsumeConnectAttempt marks the attempt with this state hash used at now and returns it, in one
// statement, so of two concurrent callers exactly one succeeds. It returns ErrAttemptUnusable
// when the attempt is unknown, expired at now, already used or already has an outcome.
func ConsumeConnectAttempt(ctx context.Context, q DBTX, stateSHA256 []byte, now time.Time) (ConnectAttempt, error) {
	var a ConnectAttempt
	err := q.QueryRow(ctx, `UPDATE sync.connect_attempt SET used_at = $2
		WHERE state_sha256 = $1 AND used_at IS NULL AND outcome IS NULL AND expires_at > $2
		RETURNING id, source, state_sha256, started_at, expires_at, used_at, outcome`, stateSHA256, now).
		Scan(&a.ID, &a.Source, &a.StateSHA256, &a.StartedAt, &a.ExpiresAt, &a.UsedAt, &a.Outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConnectAttempt{}, ErrAttemptUnusable
	}
	if err != nil {
		return ConnectAttempt{}, fmt.Errorf("store: consume connect attempt: %w", err)
	}
	return a, nil
}

// SetConnectAttemptOutcome records the outcome of attempt id (AttemptExchanged or AttemptRefused).
func SetConnectAttemptOutcome(ctx context.Context, q DBTX, id int64, outcome string) error {
	tag, err := q.Exec(ctx, `UPDATE sync.connect_attempt SET outcome = $2 WHERE id = $1`, id, outcome)
	if err != nil {
		return fmt.Errorf("store: set connect attempt outcome: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ExpireConnectAttempts gives outcome expired to every unused attempt whose expiry is at or before
// now, and returns how many it changed.
func ExpireConnectAttempts(ctx context.Context, q DBTX, now time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `UPDATE sync.connect_attempt SET outcome = 'expired'
		WHERE used_at IS NULL AND outcome IS NULL AND expires_at <= $1`, now)
	if err != nil {
		return 0, fmt.Errorf("store: expire connect attempts: %w", err)
	}
	return tag.RowsAffected(), nil
}

// BackfillWindow is a sync.backfill_window row: one (account, period) range of a full-history
// fetch.
type BackfillWindow struct {
	ID          int64
	AccountKey  string
	PeriodStart time.Time
	PeriodEnd   time.Time
	Cursor      *string
	CompletedAt *time.Time
}

const backfillColumns = `id, account_key, period_start, period_end, cursor, completed_at`

func scanBackfill(r pgx.CollectableRow) (BackfillWindow, error) {
	var w BackfillWindow
	err := r.Scan(&w.ID, &w.AccountKey, &w.PeriodStart, &w.PeriodEnd, &w.Cursor, &w.CompletedAt)
	return w, err
}

// UpsertBackfillWindow records the range when it is new and returns the stored row either way,
// with any cursor and completion already recorded.
func UpsertBackfillWindow(ctx context.Context, q DBTX, accountKey string, periodStart, periodEnd time.Time) (BackfillWindow, error) {
	rows, err := q.Query(ctx, `WITH ins AS (
		  INSERT INTO sync.backfill_window (account_key, period_start, period_end) VALUES ($1, $2, $3)
		  ON CONFLICT (account_key, period_start, period_end) DO NOTHING
		  RETURNING `+backfillColumns+`)
		SELECT `+backfillColumns+` FROM ins
		UNION ALL
		SELECT `+backfillColumns+` FROM sync.backfill_window
		WHERE account_key = $1 AND period_start = $2 AND period_end = $3`, accountKey, periodStart, periodEnd)
	if err != nil {
		return BackfillWindow{}, fmt.Errorf("store: upsert backfill window: %w", err)
	}
	ws, err := pgx.CollectRows(rows, scanBackfill)
	if err != nil {
		return BackfillWindow{}, fmt.Errorf("store: upsert backfill window: %w", err)
	}
	if len(ws) == 0 {
		return BackfillWindow{}, fmt.Errorf("store: upsert backfill window: no row")
	}
	return ws[0], nil
}

// SetBackfillCursor stores the cursor of window id.
func SetBackfillCursor(ctx context.Context, q DBTX, id int64, cursor string) error {
	tag, err := q.Exec(ctx, `UPDATE sync.backfill_window SET cursor = $2 WHERE id = $1`, id, cursor)
	if err != nil {
		return fmt.Errorf("store: set backfill cursor: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CompleteBackfillWindow marks window id complete at at (the arrival of its last page).
func CompleteBackfillWindow(ctx context.Context, q DBTX, id int64, at time.Time) error {
	tag, err := q.Exec(ctx, `UPDATE sync.backfill_window SET completed_at = $2 WHERE id = $1`, id, at)
	if err != nil {
		return fmt.Errorf("store: complete backfill window: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListIncompleteBackfillWindows returns every window with no completion, newest period first.
func ListIncompleteBackfillWindows(ctx context.Context, q DBTX) ([]BackfillWindow, error) {
	rows, err := q.Query(ctx, `SELECT `+backfillColumns+` FROM sync.backfill_window
		WHERE completed_at IS NULL ORDER BY period_start DESC, account_key, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list incomplete backfill windows: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanBackfill)
	if err != nil {
		return nil, fmt.Errorf("store: list incomplete backfill windows: %w", err)
	}
	return out, nil
}

// ListBackfillWindows returns every window of an account, oldest period first.
func ListBackfillWindows(ctx context.Context, q DBTX, accountKey string) ([]BackfillWindow, error) {
	rows, err := q.Query(ctx, `SELECT `+backfillColumns+` FROM sync.backfill_window
		WHERE account_key = $1 ORDER BY period_start, id`, accountKey)
	if err != nil {
		return nil, fmt.Errorf("store: list backfill windows: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanBackfill)
	if err != nil {
		return nil, fmt.Errorf("store: list backfill windows: %w", err)
	}
	return out, nil
}

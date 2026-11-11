package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The schemas Core may be bound to.
const (
	SchemaCore     = "core"
	SchemaCoreNext = "core_next"
)

// ErrBadSchema is returned for a schema name outside SchemaCore and SchemaCoreNext.
var ErrBadSchema = errors.New("store: schema must be core or core_next")

func checkSchema(schema string) error {
	if schema != SchemaCore && schema != SchemaCoreNext {
		return fmt.Errorf("%w: %q", ErrBadSchema, schema)
	}
	return nil
}

// Core reads and writes the tables of one core schema. Build it with NewCore.
type Core struct {
	q      DBTX
	schema string
}

// NewCore binds q to schema, which must be SchemaCore or SchemaCoreNext.
func NewCore(q DBTX, schema string) (Core, error) {
	if err := checkSchema(schema); err != nil {
		return Core{}, err
	}
	return Core{q: q, schema: schema}, nil
}

// Schema returns the schema c is bound to.
func (c Core) Schema() string { return c.schema }

// sql replaces every "core." in stmt with the bound schema. Statements are constants of this
// package and the schema is allow-listed by NewCore.
func (c Core) sql(stmt string) string {
	if c.schema == SchemaCore {
		return stmt
	}
	return strings.ReplaceAll(stmt, "core.", c.schema+".")
}

// Account is a core.account row.
type Account struct {
	ID             uuid.UUID
	Source         string
	SourceKey      string
	Slug           string
	Type           string // current | joint | pot | savings | mortgage | flex | loan | rewards | business | prepaid
	Feed           string // api | file
	Currency       string
	Name           string
	ParentID       *uuid.UUID
	ExpectsImports bool
	OpenedOn       *time.Time // a date
	Closed         bool
}

// Transaction is a core.transaction row. It always mirrors the latest version.
type Transaction struct {
	ID               uuid.UUID
	AccountID        uuid.UUID
	Source           string
	SourceKey        string
	AmountMinor      int64
	Currency         string
	LocalAmountMinor *int64
	LocalCurrency    *string
	CreatedAt        *time.Time
	SettledAt        *time.Time
	TxnDate          time.Time // a date
	Status           string    // pending | settled | declined
	Description      string
	Counterparty     *string
	MerchantID       *string
	MerchantName     *string
	Category         *string
	Scheme           *string
	Notes            *string
	VersionCount     int
	CurrentHash      []byte
	FirstSeenAt      time.Time
	LastSeenAt       time.Time
	DisappearedAt    *time.Time
}

// TransactionVersion is a core.transaction_version row.
type TransactionVersion struct {
	TransactionID uuid.UUID
	VersionNo     int
	Content       json.RawMessage
	ContentHash   []byte
	SeenAt        time.Time
	RawSeq        int64
}

// TransactionState is what the version logic and the running sum need of a stored transaction.
type TransactionState struct {
	ID            uuid.UUID
	AccountID     uuid.UUID
	AmountMinor   int64
	Status        string
	VersionCount  int
	CurrentHash   []byte
	DisappearedAt *time.Time
}

// EffectiveAmount is the amount the running sum counts: the amount, or 0 when the transaction is
// declined or disappeared.
func (s TransactionState) EffectiveAmount() int64 {
	if s.Status == "declined" || s.DisappearedAt != nil {
		return 0
	}
	return s.AmountMinor
}

// ApplyOutcome says what ApplyTransaction did.
type ApplyOutcome string

// ApplyTransaction outcomes.
const (
	TransactionCreated   ApplyOutcome = "created"   // a new transaction and its version 1
	TransactionChanged   ApplyOutcome = "changed"   // a new version; the row now mirrors it
	TransactionUnchanged ApplyOutcome = "unchanged" // same hash; only last_seen_at moved
)

// ApplyResult is what ApplyTransaction did and the state before it.
type ApplyResult struct {
	Outcome  ApplyOutcome
	Previous *TransactionState // nil when Outcome is TransactionCreated
	Current  TransactionState
}

// BalanceSnapshot is a core.balance_snapshot row.
type BalanceSnapshot struct {
	AccountID         uuid.UUID
	RawSeq            int64
	Ordinal           int
	At                *time.Time
	OnDate            *time.Time // a date
	BalanceMinor      int64
	TotalBalanceMinor *int64
	Currency          string
}

// Reconciliation is a core.reconciliation row: an account's current reconciliation state.
type Reconciliation struct {
	AccountID         uuid.UUID
	Method            string // monzo_absolute | file_continuity | balances_only | none
	RunningSumMinor   int64
	LastCheckAt       *time.Time
	LastCheckOK       *bool
	LastDiffMinor     *int64
	LastPassAt        *time.Time
	ReconciledThrough *time.Time // a date
	BreakCount        int
	KnownException    *string
}

// FileGap is a core.file_gap row: two imported files that do not join up.
type FileGap struct {
	AccountID  uuid.UUID
	AfterDate  time.Time // last date of the earlier file
	BeforeDate time.Time // first date of the later file
	DiffMinor  int64
	RawSeq     int64
}

// ProjectionState is the single core.projection_state row.
type ProjectionState struct {
	LastSeq       int64
	NormaliserRev int
}

const accountColumns = `id, source, source_key, slug, type, feed, currency, name, parent_id, expects_imports, opened_on, closed`

func scanAccount(r pgx.CollectableRow) (Account, error) {
	var a Account
	err := r.Scan(&a.ID, &a.Source, &a.SourceKey, &a.Slug, &a.Type, &a.Feed, &a.Currency, &a.Name,
		&a.ParentID, &a.ExpectsImports, &a.OpenedOn, &a.Closed)
	return a, err
}

// UpsertAccount inserts a, or replaces every column of the row with a's id.
func (c Core) UpsertAccount(ctx context.Context, a Account) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.account (`+accountColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (id) DO UPDATE SET source = EXCLUDED.source, source_key = EXCLUDED.source_key,
		  slug = EXCLUDED.slug, type = EXCLUDED.type, feed = EXCLUDED.feed, currency = EXCLUDED.currency,
		  name = EXCLUDED.name, parent_id = EXCLUDED.parent_id, expects_imports = EXCLUDED.expects_imports,
		  opened_on = EXCLUDED.opened_on, closed = EXCLUDED.closed`),
		a.ID, a.Source, a.SourceKey, a.Slug, a.Type, a.Feed, a.Currency, a.Name, a.ParentID,
		a.ExpectsImports, a.OpenedOn, a.Closed)
	if err != nil {
		return fmt.Errorf("store: upsert account: %w", err)
	}
	return nil
}

func (c Core) oneAccount(ctx context.Context, where string, args ...any) (Account, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT `+accountColumns+` FROM core.account WHERE `+where), args...)
	if err != nil {
		return Account{}, fmt.Errorf("store: get account: %w", err)
	}
	a, err := pgx.CollectExactlyOneRow(rows, scanAccount)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("store: get account: %w", err)
	}
	return a, nil
}

// GetAccount returns the account with id, or ErrNotFound.
func (c Core) GetAccount(ctx context.Context, id uuid.UUID) (Account, error) {
	return c.oneAccount(ctx, "id = $1", id)
}

// GetAccountBySlug returns the account with slug, or ErrNotFound.
func (c Core) GetAccountBySlug(ctx context.Context, slug string) (Account, error) {
	return c.oneAccount(ctx, "slug = $1", slug)
}

// GetAccountBySource returns the account with this source and source key, or ErrNotFound.
func (c Core) GetAccountBySource(ctx context.Context, source, sourceKey string) (Account, error) {
	return c.oneAccount(ctx, "source = $1 AND source_key = $2", source, sourceKey)
}

// ListAccounts returns every account ordered by slug.
func (c Core) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT `+accountColumns+` FROM core.account ORDER BY slug`))
	if err != nil {
		return nil, fmt.Errorf("store: list accounts: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanAccount)
	if err != nil {
		return nil, fmt.Errorf("store: list accounts: %w", err)
	}
	return out, nil
}

const transactionColumns = `id, account_id, source, source_key, amount_minor, currency, local_amount_minor,
	local_currency, created_at, settled_at, txn_date, status, description, counterparty, merchant_id,
	merchant_name, category, scheme, notes, version_count, current_hash, first_seen_at, last_seen_at,
	disappeared_at`

func scanTransaction(r pgx.CollectableRow) (Transaction, error) {
	var t Transaction
	err := r.Scan(&t.ID, &t.AccountID, &t.Source, &t.SourceKey, &t.AmountMinor, &t.Currency,
		&t.LocalAmountMinor, &t.LocalCurrency, &t.CreatedAt, &t.SettledAt, &t.TxnDate, &t.Status,
		&t.Description, &t.Counterparty, &t.MerchantID, &t.MerchantName, &t.Category, &t.Scheme,
		&t.Notes, &t.VersionCount, &t.CurrentHash, &t.FirstSeenAt, &t.LastSeenAt, &t.DisappearedAt)
	return t, err
}

// GetTransaction returns the transaction with id, or ErrNotFound.
func (c Core) GetTransaction(ctx context.Context, id uuid.UUID) (Transaction, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT `+transactionColumns+` FROM core.transaction WHERE id = $1`), id)
	if err != nil {
		return Transaction{}, fmt.Errorf("store: get transaction: %w", err)
	}
	t, err := pgx.CollectExactlyOneRow(rows, scanTransaction)
	if errors.Is(err, pgx.ErrNoRows) {
		return Transaction{}, ErrNotFound
	}
	if err != nil {
		return Transaction{}, fmt.Errorf("store: get transaction: %w", err)
	}
	return t, nil
}

// GetTransactionState returns the version and running-sum state of the transaction with id, or
// ErrNotFound. It locks the row until the end of the transaction q belongs to.
func (c Core) GetTransactionState(ctx context.Context, id uuid.UUID) (TransactionState, error) {
	var s TransactionState
	err := c.q.QueryRow(ctx, c.sql(`SELECT id, account_id, amount_minor, status, version_count, current_hash, disappeared_at
		FROM core.transaction WHERE id = $1 FOR UPDATE`), id).
		Scan(&s.ID, &s.AccountID, &s.AmountMinor, &s.Status, &s.VersionCount, &s.CurrentHash, &s.DisappearedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TransactionState{}, ErrNotFound
	}
	if err != nil {
		return TransactionState{}, fmt.Errorf("store: get transaction state: %w", err)
	}
	return s, nil
}

// UpsertTransaction writes every column of t, inserting or replacing the row with t's id. The
// caller keeps VersionCount and CurrentHash in step with the versions it inserts; ApplyTransaction
// does both.
func (c Core) UpsertTransaction(ctx context.Context, t Transaction) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.transaction (`+transactionColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24)
		ON CONFLICT (id) DO UPDATE SET account_id = EXCLUDED.account_id, source = EXCLUDED.source,
		  source_key = EXCLUDED.source_key, amount_minor = EXCLUDED.amount_minor, currency = EXCLUDED.currency,
		  local_amount_minor = EXCLUDED.local_amount_minor, local_currency = EXCLUDED.local_currency,
		  created_at = EXCLUDED.created_at, settled_at = EXCLUDED.settled_at, txn_date = EXCLUDED.txn_date,
		  status = EXCLUDED.status, description = EXCLUDED.description, counterparty = EXCLUDED.counterparty,
		  merchant_id = EXCLUDED.merchant_id, merchant_name = EXCLUDED.merchant_name, category = EXCLUDED.category,
		  scheme = EXCLUDED.scheme, notes = EXCLUDED.notes, version_count = EXCLUDED.version_count,
		  current_hash = EXCLUDED.current_hash, first_seen_at = EXCLUDED.first_seen_at,
		  last_seen_at = EXCLUDED.last_seen_at, disappeared_at = EXCLUDED.disappeared_at`),
		t.ID, t.AccountID, t.Source, t.SourceKey, t.AmountMinor, t.Currency, t.LocalAmountMinor,
		t.LocalCurrency, t.CreatedAt, t.SettledAt, t.TxnDate, t.Status, t.Description, t.Counterparty,
		t.MerchantID, t.MerchantName, t.Category, t.Scheme, t.Notes, t.VersionCount, t.CurrentHash,
		t.FirstSeenAt, t.LastSeenAt, t.DisappearedAt)
	if err != nil {
		return fmt.Errorf("store: upsert transaction: %w", err)
	}
	return nil
}

// InsertTransactionVersion inserts v.
func (c Core) InsertTransactionVersion(ctx context.Context, v TransactionVersion) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.transaction_version
		(transaction_id, version_no, content, content_hash, seen_at, raw_seq) VALUES ($1, $2, $3, $4, $5, $6)`),
		v.TransactionID, v.VersionNo, string(v.Content), v.ContentHash, v.SeenAt, v.RawSeq)
	if err != nil {
		return fmt.Errorf("store: insert transaction version: %w", err)
	}
	return nil
}

// ListTransactionVersions returns the versions of a transaction in version order.
func (c Core) ListTransactionVersions(ctx context.Context, id uuid.UUID) ([]TransactionVersion, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT transaction_id, version_no, content, content_hash, seen_at, raw_seq
		FROM core.transaction_version WHERE transaction_id = $1 ORDER BY version_no`), id)
	if err != nil {
		return nil, fmt.Errorf("store: list transaction versions: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (TransactionVersion, error) {
		var v TransactionVersion
		var content []byte
		err := r.Scan(&v.TransactionID, &v.VersionNo, &content, &v.ContentHash, &v.SeenAt, &v.RawSeq)
		v.Content = content
		return v, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list transaction versions: %w", err)
	}
	return out, nil
}

// ApplyTransaction is the version logic in one call. t carries the normalised columns (its
// VersionCount, CurrentHash, FirstSeenAt, LastSeenAt and DisappearedAt are ignored); content and
// hash are the version content and its hash; seenAt and rawSeq say when and from which raw row.
//
//   - No stored row: inserts t as version 1 with first and last seen at seenAt.
//   - Stored hash equals hash: moves last_seen_at forward to seenAt and changes nothing else.
//   - Otherwise: inserts the next version and rewrites the row to mirror it, keeping first_seen_at
//     and disappeared_at.
//
// The returned Previous state lets the caller adjust the running sum by
// Current.EffectiveAmount() - Previous.EffectiveAmount().
func (c Core) ApplyTransaction(ctx context.Context, t Transaction, content json.RawMessage, hash []byte, seenAt time.Time, rawSeq int64) (ApplyResult, error) {
	prev, err := c.GetTransactionState(ctx, t.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		t.VersionCount = 1
		t.CurrentHash = hash
		t.FirstSeenAt = seenAt
		t.LastSeenAt = seenAt
		t.DisappearedAt = nil
		if err := c.UpsertTransaction(ctx, t); err != nil {
			return ApplyResult{}, err
		}
		if err := c.InsertTransactionVersion(ctx, TransactionVersion{TransactionID: t.ID, VersionNo: 1,
			Content: content, ContentHash: hash, SeenAt: seenAt, RawSeq: rawSeq}); err != nil {
			return ApplyResult{}, err
		}
		return ApplyResult{Outcome: TransactionCreated, Current: stateOf(t)}, nil
	case err != nil:
		return ApplyResult{}, err
	}
	if bytes.Equal(prev.CurrentHash, hash) {
		if _, err := c.q.Exec(ctx, c.sql(`UPDATE core.transaction SET last_seen_at = greatest(last_seen_at, $2) WHERE id = $1`),
			t.ID, seenAt); err != nil {
			return ApplyResult{}, fmt.Errorf("store: touch transaction: %w", err)
		}
		return ApplyResult{Outcome: TransactionUnchanged, Previous: &prev, Current: prev}, nil
	}
	var firstSeen, lastSeen time.Time
	if err := c.q.QueryRow(ctx, c.sql(`SELECT first_seen_at, last_seen_at FROM core.transaction WHERE id = $1`), t.ID).
		Scan(&firstSeen, &lastSeen); err != nil {
		return ApplyResult{}, fmt.Errorf("store: read transaction times: %w", err)
	}
	t.VersionCount = prev.VersionCount + 1
	t.CurrentHash = hash
	t.FirstSeenAt = firstSeen
	t.LastSeenAt = lastSeen
	if seenAt.After(lastSeen) {
		t.LastSeenAt = seenAt
	}
	t.DisappearedAt = prev.DisappearedAt
	if err := c.InsertTransactionVersion(ctx, TransactionVersion{TransactionID: t.ID, VersionNo: t.VersionCount,
		Content: content, ContentHash: hash, SeenAt: seenAt, RawSeq: rawSeq}); err != nil {
		return ApplyResult{}, err
	}
	if err := c.UpsertTransaction(ctx, t); err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Outcome: TransactionChanged, Previous: &prev, Current: stateOf(t)}, nil
}

func stateOf(t Transaction) TransactionState {
	return TransactionState{ID: t.ID, AccountID: t.AccountID, AmountMinor: t.AmountMinor, Status: t.Status,
		VersionCount: t.VersionCount, CurrentHash: t.CurrentHash, DisappearedAt: t.DisappearedAt}
}

// DisappearanceResult lists what SetDisappeared changed.
type DisappearanceResult struct {
	Disappeared []TransactionState // disappeared_at newly set; state after the change
	Returned    []TransactionState // disappeared_at cleared; state after the change
}

// SetDisappeared applies a completed fetch of accountID's transactions over [periodStart,
// periodEnd): every transaction of the account whose created_at lies in the period, is not in
// present, and has no disappeared_at gets disappeared_at = at; every transaction of the account
// that is in present and has disappeared_at set has it cleared. Rows already in the right state
// are untouched. The result lets the caller adjust the running sum and record versions.
func (c Core) SetDisappeared(ctx context.Context, accountID uuid.UUID, periodStart, periodEnd time.Time, present []uuid.UUID, at time.Time) (DisappearanceResult, error) {
	if present == nil {
		present = []uuid.UUID{}
	}
	rows, err := c.q.Query(ctx, c.sql(`
		WITH gone AS (
		  UPDATE core.transaction SET disappeared_at = $5
		  WHERE account_id = $1 AND created_at >= $2 AND created_at < $3
		    AND disappeared_at IS NULL AND NOT (id = ANY($4))
		  RETURNING id, account_id, amount_minor, status, version_count, current_hash, disappeared_at, true AS gone
		), back AS (
		  UPDATE core.transaction SET disappeared_at = NULL
		  WHERE account_id = $1 AND id = ANY($4) AND disappeared_at IS NOT NULL
		  RETURNING id, account_id, amount_minor, status, version_count, current_hash, disappeared_at, false AS gone
		)
		SELECT * FROM gone UNION ALL SELECT * FROM back ORDER BY id`),
		accountID, periodStart, periodEnd, present, at)
	if err != nil {
		return DisappearanceResult{}, fmt.Errorf("store: set disappeared: %w", err)
	}
	var res DisappearanceResult
	if err := collectDisappearance(rows, &res); err != nil {
		return DisappearanceResult{}, err
	}
	return res, nil
}

func collectDisappearance(rows pgx.Rows, res *DisappearanceResult) error {
	defer rows.Close()
	for rows.Next() {
		var s TransactionState
		var gone bool
		if err := rows.Scan(&s.ID, &s.AccountID, &s.AmountMinor, &s.Status, &s.VersionCount, &s.CurrentHash, &s.DisappearedAt, &gone); err != nil {
			return fmt.Errorf("store: set disappeared: %w", err)
		}
		if gone {
			res.Disappeared = append(res.Disappeared, s)
		} else {
			res.Returned = append(res.Returned, s)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: set disappeared: %w", err)
	}
	return nil
}

// InsertBalanceSnapshot inserts b.
func (c Core) InsertBalanceSnapshot(ctx context.Context, b BalanceSnapshot) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.balance_snapshot
		(account_id, raw_seq, ordinal, at, on_date, balance_minor, total_balance_minor, currency)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`),
		b.AccountID, b.RawSeq, b.Ordinal, b.At, b.OnDate, b.BalanceMinor, b.TotalBalanceMinor, b.Currency)
	if err != nil {
		return fmt.Errorf("store: insert balance snapshot: %w", err)
	}
	return nil
}

// LatestBalanceSnapshot returns the account's chronologically latest snapshot (by at, else
// on_date, then raw_seq and ordinal), or ErrNotFound.
func (c Core) LatestBalanceSnapshot(ctx context.Context, accountID uuid.UUID) (BalanceSnapshot, error) {
	var b BalanceSnapshot
	err := c.q.QueryRow(ctx, c.sql(`SELECT account_id, raw_seq, ordinal, at, on_date, balance_minor, total_balance_minor, currency
		FROM core.balance_snapshot WHERE account_id = $1
		ORDER BY coalesce(at, on_date::timestamp AT TIME ZONE 'UTC') DESC NULLS LAST, raw_seq DESC, ordinal DESC LIMIT 1`), accountID).
		Scan(&b.AccountID, &b.RawSeq, &b.Ordinal, &b.At, &b.OnDate, &b.BalanceMinor, &b.TotalBalanceMinor, &b.Currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return BalanceSnapshot{}, ErrNotFound
	}
	if err != nil {
		return BalanceSnapshot{}, fmt.Errorf("store: latest balance snapshot: %w", err)
	}
	return b, nil
}

const reconciliationColumns = `account_id, method, running_sum_minor, last_check_at, last_check_ok, last_diff_minor,
	last_pass_at, reconciled_through, break_count, known_exception`

// GetReconciliation returns the account's reconciliation row, or ErrNotFound.
func (c Core) GetReconciliation(ctx context.Context, accountID uuid.UUID) (Reconciliation, error) {
	var r Reconciliation
	err := c.q.QueryRow(ctx, c.sql(`SELECT `+reconciliationColumns+` FROM core.reconciliation WHERE account_id = $1`), accountID).
		Scan(&r.AccountID, &r.Method, &r.RunningSumMinor, &r.LastCheckAt, &r.LastCheckOK, &r.LastDiffMinor,
			&r.LastPassAt, &r.ReconciledThrough, &r.BreakCount, &r.KnownException)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reconciliation{}, ErrNotFound
	}
	if err != nil {
		return Reconciliation{}, fmt.Errorf("store: get reconciliation: %w", err)
	}
	return r, nil
}

// PutReconciliation inserts r or replaces every column of the account's row.
func (c Core) PutReconciliation(ctx context.Context, r Reconciliation) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.reconciliation (`+reconciliationColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (account_id) DO UPDATE SET method = EXCLUDED.method,
		  running_sum_minor = EXCLUDED.running_sum_minor, last_check_at = EXCLUDED.last_check_at,
		  last_check_ok = EXCLUDED.last_check_ok, last_diff_minor = EXCLUDED.last_diff_minor,
		  last_pass_at = EXCLUDED.last_pass_at, reconciled_through = EXCLUDED.reconciled_through,
		  break_count = EXCLUDED.break_count, known_exception = EXCLUDED.known_exception`),
		r.AccountID, r.Method, r.RunningSumMinor, r.LastCheckAt, r.LastCheckOK, r.LastDiffMinor,
		r.LastPassAt, r.ReconciledThrough, r.BreakCount, r.KnownException)
	if err != nil {
		return fmt.Errorf("store: put reconciliation: %w", err)
	}
	return nil
}

// EnsureReconciliation creates the account's reconciliation row with method and a zero running
// sum when it has none, and leaves an existing row alone.
func (c Core) EnsureReconciliation(ctx context.Context, accountID uuid.UUID, method string) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.reconciliation (account_id, method) VALUES ($1, $2)
		ON CONFLICT (account_id) DO NOTHING`), accountID, method)
	if err != nil {
		return fmt.Errorf("store: ensure reconciliation: %w", err)
	}
	return nil
}

// AddToRunningSum adds delta to the account's running sum and returns the new sum. The row must
// exist (EnsureReconciliation); otherwise it returns ErrNotFound.
func (c Core) AddToRunningSum(ctx context.Context, accountID uuid.UUID, delta int64) (int64, error) {
	var sum int64
	err := c.q.QueryRow(ctx, c.sql(`UPDATE core.reconciliation SET running_sum_minor = running_sum_minor + $2
		WHERE account_id = $1 RETURNING running_sum_minor`), accountID, delta).Scan(&sum)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: add to running sum: %w", err)
	}
	return sum, nil
}

// InsertFileGap inserts g.
func (c Core) InsertFileGap(ctx context.Context, g FileGap) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.file_gap (account_id, after_date, before_date, diff_minor, raw_seq)
		VALUES ($1, $2, $3, $4, $5)`), g.AccountID, g.AfterDate, g.BeforeDate, g.DiffMinor, g.RawSeq)
	if err != nil {
		return fmt.Errorf("store: insert file gap: %w", err)
	}
	return nil
}

// ListFileGaps returns the account's file gaps in raw_seq order.
func (c Core) ListFileGaps(ctx context.Context, accountID uuid.UUID) ([]FileGap, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT account_id, after_date, before_date, diff_minor, raw_seq
		FROM core.file_gap WHERE account_id = $1 ORDER BY raw_seq`), accountID)
	if err != nil {
		return nil, fmt.Errorf("store: list file gaps: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (FileGap, error) {
		var g FileGap
		err := r.Scan(&g.AccountID, &g.AfterDate, &g.BeforeDate, &g.DiffMinor, &g.RawSeq)
		return g, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list file gaps: %w", err)
	}
	return out, nil
}

// GetProjectionState returns the projection state row, or ErrNotFound before it is seeded.
func (c Core) GetProjectionState(ctx context.Context) (ProjectionState, error) {
	var p ProjectionState
	err := c.q.QueryRow(ctx, c.sql(`SELECT last_seq, normaliser_rev FROM core.projection_state`)).Scan(&p.LastSeq, &p.NormaliserRev)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectionState{}, ErrNotFound
	}
	if err != nil {
		return ProjectionState{}, fmt.Errorf("store: get projection state: %w", err)
	}
	return p, nil
}

// SetProjectionState writes the projection state row.
func (c Core) SetProjectionState(ctx context.Context, p ProjectionState) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.projection_state (only_row, last_seq, normaliser_rev) VALUES (true, $1, $2)
		ON CONFLICT (only_row) DO UPDATE SET last_seq = EXCLUDED.last_seq, normaliser_rev = EXCLUDED.normaliser_rev`),
		p.LastSeq, p.NormaliserRev)
	if err != nil {
		return fmt.Errorf("store: set projection state: %w", err)
	}
	return nil
}

// SetLastSeq records that every raw row up to seq has been normalised. The row must exist.
func (c Core) SetLastSeq(ctx context.Context, seq int64) error {
	tag, err := c.q.Exec(ctx, c.sql(`UPDATE core.projection_state SET last_seq = $1`), seq)
	if err != nil {
		return fmt.Errorf("store: set last seq: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

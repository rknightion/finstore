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
	Type           string // current | joint | pot | savings | mortgage | flex | loan | rewards | business | prepaid | credit_card
	Feed           string // api | file
	Currency       string
	Name           string
	ParentID       *uuid.UUID
	ExpectsImports bool
	OpenedOn       *time.Time // a date
	Closed         bool
	// BankRef is the account's bank reference (sort code and account number, digits only, joined
	// by a space); nil until a source or a statement sets it.
	BankRef *string
	// EndedOn is the last day the account has transactions on (a date), set by an account_end
	// operator entry; nil while the account has no end.
	EndedOn *time.Time
	// Detail is the source's whole account object as JSON, or nil when the source gave none.
	Detail json.RawMessage
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
	TransactionUnchanged ApplyOutcome = "unchanged" // same hash as the row; only the seen times moved
	// TransactionOlderVersion: a new version seen before the row's last_seen_at (an old capture
	// loaded late). The version is recorded; the row, and so its effective amount, is unchanged.
	TransactionOlderVersion ApplyOutcome = "older_version"
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
	Method            string // monzo_absolute | running_balance | file_continuity | balances_only | none
	RunningSumMinor   int64
	LastCheckAt       *time.Time
	LastCheckOK       *bool
	LastDiffMinor     *int64
	LastPassAt        *time.Time
	ReconciledThrough *time.Time // a date
	BreakCount        int
	KnownException    *string
	// AcceptedDiffMinor is the difference the operator accepted with KnownException.
	AcceptedDiffMinor *int64
	// FailedOn is, for running_balance, the earliest day that does not add up (a date).
	FailedOn *time.Time
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
	// Zone is the time zone name the projection was built with; nil until the first CatchUp.
	Zone *string
}

const accountColumns = `id, source, source_key, slug, type, feed, currency, name, parent_id, expects_imports, opened_on, closed,
	bank_ref, ended_on, detail`

func scanAccount(r pgx.CollectableRow) (Account, error) {
	var a Account
	var detail []byte
	err := r.Scan(&a.ID, &a.Source, &a.SourceKey, &a.Slug, &a.Type, &a.Feed, &a.Currency, &a.Name,
		&a.ParentID, &a.ExpectsImports, &a.OpenedOn, &a.Closed, &a.BankRef, &a.EndedOn, &detail)
	a.Detail = detail
	return a, err
}

// UpsertAccount inserts a, or replaces every column of the row with a's id, except that an a with no
// Detail keeps the detail the row has: a path that reads an account and writes it back (an account
// end, a file account) must not lose it, and a source that gives none (Monzo) leaves it null.
func (c Core) UpsertAccount(ctx context.Context, a Account) error {
	var detail any
	if len(a.Detail) > 0 {
		detail = string(a.Detail)
	}
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.account (`+accountColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (id) DO UPDATE SET source = EXCLUDED.source, source_key = EXCLUDED.source_key,
		  slug = EXCLUDED.slug, type = EXCLUDED.type, feed = EXCLUDED.feed, currency = EXCLUDED.currency,
		  name = EXCLUDED.name, parent_id = EXCLUDED.parent_id, expects_imports = EXCLUDED.expects_imports,
		  opened_on = EXCLUDED.opened_on, closed = EXCLUDED.closed, bank_ref = EXCLUDED.bank_ref,
		  ended_on = EXCLUDED.ended_on, detail = COALESCE(EXCLUDED.detail, account.detail)`),
		a.ID, a.Source, a.SourceKey, a.Slug, a.Type, a.Feed, a.Currency, a.Name, a.ParentID,
		a.ExpectsImports, a.OpenedOn, a.Closed, a.BankRef, a.EndedOn, detail)
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
// The row mirrors the version seen latest (by seen time; at equal times, the later raw row), so a
// response stored late but fetched early, such as an old capture loaded after live data, never
// becomes the current state. Version numbers follow raw seq order.
//
//   - No stored row: inserts t as version 1 with first and last seen at seenAt.
//   - Stored hash equals hash: first_seen_at moves back to seenAt if earlier and last_seen_at
//     forward to it if later; nothing else changes.
//   - Otherwise, seenAt not before last_seen_at: inserts the next version and rewrites the row to
//     mirror it, keeping disappeared_at; first_seen_at is the earlier of the two, last_seen_at
//     becomes seenAt.
//   - Otherwise (seenAt before last_seen_at): inserts the next version, counts it in
//     version_count and moves first_seen_at back if earlier; the row keeps mirroring its version.
//
// The returned Previous state lets the caller adjust the running sum by
// Current.EffectiveAmount() - Previous.EffectiveAmount(), which is 0 for TransactionOlderVersion.
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
		if _, err := c.q.Exec(ctx, c.sql(`UPDATE core.transaction
			SET first_seen_at = least(first_seen_at, $2), last_seen_at = greatest(last_seen_at, $2) WHERE id = $1`),
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
	if seenAt.Before(firstSeen) {
		firstSeen = seenAt
	}
	versionNo := prev.VersionCount + 1
	if err := c.InsertTransactionVersion(ctx, TransactionVersion{TransactionID: t.ID, VersionNo: versionNo,
		Content: content, ContentHash: hash, SeenAt: seenAt, RawSeq: rawSeq}); err != nil {
		return ApplyResult{}, err
	}
	if seenAt.Before(lastSeen) {
		if _, err := c.q.Exec(ctx, c.sql(`UPDATE core.transaction SET version_count = $2, first_seen_at = $3 WHERE id = $1`),
			t.ID, versionNo, firstSeen); err != nil {
			return ApplyResult{}, fmt.Errorf("store: count older version: %w", err)
		}
		cur := prev
		cur.VersionCount = versionNo
		return ApplyResult{Outcome: TransactionOlderVersion, Previous: &prev, Current: cur}, nil
	}
	t.VersionCount = versionNo
	t.CurrentHash = hash
	t.FirstSeenAt = firstSeen
	t.LastSeenAt = seenAt
	t.DisappearedAt = prev.DisappearedAt
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

// FetchPeriod is a core.fetch_period row: the period, as instants, that one completed fetch of
// an account's transactions proved complete.
type FetchPeriod struct {
	FetchID     int64
	AccountID   uuid.UUID
	PeriodStart time.Time
	PeriodEnd   time.Time
}

// PutFetchPeriod inserts p, or replaces the row of p's fetch.
func (c Core) PutFetchPeriod(ctx context.Context, p FetchPeriod) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.fetch_period (fetch_id, account_id, period_start, period_end)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (fetch_id) DO UPDATE SET account_id = EXCLUDED.account_id,
		  period_start = EXCLUDED.period_start, period_end = EXCLUDED.period_end`),
		p.FetchID, p.AccountID, p.PeriodStart, p.PeriodEnd)
	if err != nil {
		return fmt.Errorf("store: put fetch period: %w", err)
	}
	return nil
}

// GetFetchPeriod returns the period recorded for fetchID, or ErrNotFound.
func (c Core) GetFetchPeriod(ctx context.Context, fetchID int64) (FetchPeriod, error) {
	var p FetchPeriod
	err := c.q.QueryRow(ctx, c.sql(`SELECT fetch_id, account_id, period_start, period_end FROM core.fetch_period
		WHERE fetch_id = $1`), fetchID).Scan(&p.FetchID, &p.AccountID, &p.PeriodStart, &p.PeriodEnd)
	if errors.Is(err, pgx.ErrNoRows) {
		return FetchPeriod{}, ErrNotFound
	}
	if err != nil {
		return FetchPeriod{}, fmt.Errorf("store: get fetch period: %w", err)
	}
	return p, nil
}

// SetDisappeared applies the completed fetch fetchID, whose account and period [period_start,
// period_end) are its core.fetch_period row (PutFetchPeriod), as completed at at: every
// transaction of the account whose created_at lies in the period, is not in present, has no
// disappeared_at, and was last seen no later than at gets disappeared_at = at; every transaction
// of the account that is in present and disappeared no later than at has it cleared. A fetch
// from an old capture loaded late therefore never overrides a later sighting or a later
// disappearance. Rows already in the right state are untouched, and a fetch with no period row
// changes nothing. The comparison is between instants, so a period that starts part-way through
// a day leaves that day's earlier rows alone. The result lets the caller adjust the running sum.
func (c Core) SetDisappeared(ctx context.Context, fetchID int64, present []uuid.UUID, at time.Time) (DisappearanceResult, error) {
	if present == nil {
		present = []uuid.UUID{}
	}
	rows, err := c.q.Query(ctx, c.sql(`
		WITH p AS (
		  SELECT account_id, period_start, period_end FROM core.fetch_period WHERE fetch_id = $1
		), gone AS (
		  UPDATE core.transaction t SET disappeared_at = $3
		  FROM p
		  WHERE t.account_id = p.account_id AND t.created_at >= p.period_start AND t.created_at < p.period_end
		    AND t.disappeared_at IS NULL AND t.last_seen_at <= $3 AND NOT (t.id = ANY($2))
		  RETURNING t.id, t.account_id, t.amount_minor, t.status, t.version_count, t.current_hash, t.disappeared_at, true AS gone
		), back AS (
		  UPDATE core.transaction t SET disappeared_at = NULL
		  FROM p
		  WHERE t.account_id = p.account_id AND t.id = ANY($2) AND t.disappeared_at <= $3
		  RETURNING t.id, t.account_id, t.amount_minor, t.status, t.version_count, t.current_hash, t.disappeared_at, false AS gone
		)
		SELECT * FROM gone UNION ALL SELECT * FROM back ORDER BY id`),
		fetchID, present, at)
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

// PendingTransaction is a core.pending_transaction row: one pending transaction of an account, as
// of its account's newest pending snapshot. It is never part of core.transaction.
type PendingTransaction struct {
	TransactionID string
	TxnDate       time.Time // a date
	AmountMinor   int64
	Currency      string
	Description   string
	MerchantName  *string
	Content       json.RawMessage
}

// PendingSnapshot is the whole pending list one response gave for one account.
type PendingSnapshot struct {
	AsOf   time.Time
	RawSeq int64
	Rows   []PendingTransaction
}

// PendingSnapshotRow is a core.pending_snapshot row.
type PendingSnapshotRow struct {
	AccountID uuid.UUID
	AsOf      time.Time
	RawSeq    int64
	RowCount  int
}

// ReplacePendingSnapshot makes snap the account's pending list: it writes the account's
// pending_snapshot row (an empty snapshot included, which records that nothing was pending at
// snap.AsOf), deletes the account's pending_transaction rows and inserts snap.Rows. Run it inside
// the transaction that applies the raw row, so the three steps are one.
func (c Core) ReplacePendingSnapshot(ctx context.Context, accountID uuid.UUID, snap PendingSnapshot) error {
	if _, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.pending_snapshot (account_id, as_of, raw_seq, row_count)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (account_id) DO UPDATE SET as_of = EXCLUDED.as_of, raw_seq = EXCLUDED.raw_seq,
		  row_count = EXCLUDED.row_count`), accountID, snap.AsOf, snap.RawSeq, len(snap.Rows)); err != nil {
		return fmt.Errorf("store: write pending snapshot: %w", err)
	}
	if _, err := c.q.Exec(ctx, c.sql(`DELETE FROM core.pending_transaction WHERE account_id = $1`), accountID); err != nil {
		return fmt.Errorf("store: clear pending transactions: %w", err)
	}
	for _, r := range snap.Rows {
		if _, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.pending_transaction
			(account_id, transaction_id, txn_date, amount_minor, currency, description, merchant_name, content)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`),
			accountID, r.TransactionID, r.TxnDate, r.AmountMinor, r.Currency, r.Description, r.MerchantName, string(r.Content)); err != nil {
			return fmt.Errorf("store: insert pending transaction: %w", err)
		}
	}
	return nil
}

// GetPendingSnapshot returns the account's pending snapshot record, or ErrNotFound when no pending
// read has succeeded for it.
func (c Core) GetPendingSnapshot(ctx context.Context, accountID uuid.UUID) (PendingSnapshotRow, error) {
	var s PendingSnapshotRow
	err := c.q.QueryRow(ctx, c.sql(`SELECT account_id, as_of, raw_seq, row_count FROM core.pending_snapshot WHERE account_id = $1`),
		accountID).Scan(&s.AccountID, &s.AsOf, &s.RawSeq, &s.RowCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return PendingSnapshotRow{}, ErrNotFound
	}
	if err != nil {
		return PendingSnapshotRow{}, fmt.Errorf("store: get pending snapshot: %w", err)
	}
	return s, nil
}

// ListPending returns the account's pending transactions ordered by transaction id.
func (c Core) ListPending(ctx context.Context, accountID uuid.UUID) ([]PendingTransaction, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT transaction_id, txn_date, amount_minor, currency, description, merchant_name, content
		FROM core.pending_transaction WHERE account_id = $1 ORDER BY transaction_id`), accountID)
	if err != nil {
		return nil, fmt.Errorf("store: list pending transactions: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (PendingTransaction, error) {
		var p PendingTransaction
		var content []byte
		err := r.Scan(&p.TransactionID, &p.TxnDate, &p.AmountMinor, &p.Currency, &p.Description, &p.MerchantName, &content)
		p.Content = content
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list pending transactions: %w", err)
	}
	return out, nil
}

// UnendedCardKeys returns the source keys of source's credit_card accounts that have no end,
// ordered by key.
func (c Core) UnendedCardKeys(ctx context.Context, source string) ([]string, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT source_key FROM core.account
		WHERE source = $1 AND type = 'credit_card' AND ended_on IS NULL ORDER BY source_key`), source)
	if err != nil {
		return nil, fmt.Errorf("store: list card accounts: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: list card accounts: %w", err)
	}
	return out, nil
}

const reconciliationColumns = `account_id, method, running_sum_minor, last_check_at, last_check_ok, last_diff_minor,
	last_pass_at, reconciled_through, break_count, known_exception, accepted_diff_minor, failed_on`

// GetReconciliation returns the account's reconciliation row, or ErrNotFound.
func (c Core) GetReconciliation(ctx context.Context, accountID uuid.UUID) (Reconciliation, error) {
	var r Reconciliation
	err := c.q.QueryRow(ctx, c.sql(`SELECT `+reconciliationColumns+` FROM core.reconciliation WHERE account_id = $1`), accountID).
		Scan(&r.AccountID, &r.Method, &r.RunningSumMinor, &r.LastCheckAt, &r.LastCheckOK, &r.LastDiffMinor,
			&r.LastPassAt, &r.ReconciledThrough, &r.BreakCount, &r.KnownException, &r.AcceptedDiffMinor, &r.FailedOn)
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
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (account_id) DO UPDATE SET method = EXCLUDED.method,
		  running_sum_minor = EXCLUDED.running_sum_minor, last_check_at = EXCLUDED.last_check_at,
		  last_check_ok = EXCLUDED.last_check_ok, last_diff_minor = EXCLUDED.last_diff_minor,
		  last_pass_at = EXCLUDED.last_pass_at, reconciled_through = EXCLUDED.reconciled_through,
		  break_count = EXCLUDED.break_count, known_exception = EXCLUDED.known_exception,
		  accepted_diff_minor = EXCLUDED.accepted_diff_minor, failed_on = EXCLUDED.failed_on`),
		r.AccountID, r.Method, r.RunningSumMinor, r.LastCheckAt, r.LastCheckOK, r.LastDiffMinor,
		r.LastPassAt, r.ReconciledThrough, r.BreakCount, r.KnownException, r.AcceptedDiffMinor, r.FailedOn)
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

// ReplaceFileGaps deletes every file gap of the account and inserts gaps in its place, in one
// transaction (a savepoint when c is bound to a transaction).
func (c Core) ReplaceFileGaps(ctx context.Context, accountID uuid.UUID, gaps []FileGap) error {
	return pgx.BeginFunc(ctx, c.q, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, c.sql(`DELETE FROM core.file_gap WHERE account_id = $1`), accountID); err != nil {
			return fmt.Errorf("store: delete file gaps: %w", err)
		}
		inner := Core{q: tx, schema: c.schema}
		for _, g := range gaps {
			if g.AccountID != accountID {
				return errors.New("store: replace file gaps: gap for another account")
			}
			if err := inner.InsertFileGap(ctx, g); err != nil {
				return err
			}
		}
		return nil
	})
}

// Statement is a core.statement row: one accepted imported statement.
type Statement struct {
	AccountID    uuid.UUID
	RawSeq       int64     // the raw.import_file seq
	FirstDate    time.Time // a date
	LastDate     time.Time // a date
	OpeningMinor int64
	ClosingMinor int64
	// ExactPeriod says the dates are the statement's printed period, not only its row dates.
	ExactPeriod bool
}

// InsertStatement inserts s.
func (c Core) InsertStatement(ctx context.Context, s Statement) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.statement
		(account_id, raw_seq, first_date, last_date, opening_minor, closing_minor, exact_period)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`),
		s.AccountID, s.RawSeq, s.FirstDate, s.LastDate, s.OpeningMinor, s.ClosingMinor, s.ExactPeriod)
	if err != nil {
		return fmt.Errorf("store: insert statement: %w", err)
	}
	return nil
}

// ListStatements returns the account's statements ordered by first_date, then last_date
// descending, then raw_seq: the order the continuity walk reads them in.
func (c Core) ListStatements(ctx context.Context, accountID uuid.UUID) ([]Statement, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT account_id, raw_seq, first_date, last_date, opening_minor, closing_minor,
		exact_period FROM core.statement WHERE account_id = $1 ORDER BY first_date, last_date DESC, raw_seq`), accountID)
	if err != nil {
		return nil, fmt.Errorf("store: list statements: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Statement, error) {
		var s Statement
		err := r.Scan(&s.AccountID, &s.RawSeq, &s.FirstDate, &s.LastDate, &s.OpeningMinor, &s.ClosingMinor, &s.ExactPeriod)
		return s, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list statements: %w", err)
	}
	return out, nil
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
	err := c.q.QueryRow(ctx, c.sql(`SELECT last_seq, normaliser_rev, zone FROM core.projection_state`)).Scan(&p.LastSeq, &p.NormaliserRev, &p.Zone)
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
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.projection_state (only_row, last_seq, normaliser_rev, zone) VALUES (true, $1, $2, $3)
		ON CONFLICT (only_row) DO UPDATE SET last_seq = EXCLUDED.last_seq, normaliser_rev = EXCLUDED.normaliser_rev,
		  zone = EXCLUDED.zone`),
		p.LastSeq, p.NormaliserRev, p.Zone)
	if err != nil {
		return fmt.Errorf("store: set projection state: %w", err)
	}
	return nil
}

// SetZone records the time zone the projection is built with. The row must exist.
func (c Core) SetZone(ctx context.Context, zone string) error {
	tag, err := c.q.Exec(ctx, c.sql(`UPDATE core.projection_state SET zone = $1`), zone)
	if err != nil {
		return fmt.Errorf("store: set zone: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
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

// Mandate kinds: the tables core.standing_order and core.direct_debit.
const (
	MandateStandingOrder = "standing_order"
	MandateDirectDebit   = "direct_debit"
)

// ErrBadMandateKind is returned for a kind outside MandateStandingOrder and MandateDirectDebit.
var ErrBadMandateKind = errors.New("store: mandate kind must be standing_order or direct_debit")

// mandateTables returns the row table and the version table of kind, and the version table's
// key column. The names are constants, so they are safe to put in a statement.
func mandateTables(kind string) (table, versionTable, idColumn string, err error) {
	switch kind {
	case MandateStandingOrder:
		return "core.standing_order", "core.standing_order_version", "standing_order_id", nil
	case MandateDirectDebit:
		return "core.direct_debit", "core.direct_debit_version", "direct_debit_id", nil
	}
	return "", "", "", fmt.Errorf("%w: %q", ErrBadMandateKind, kind)
}

// UnresolvedMandates returns, for every source that holds a standing order or a direct debit, how
// many of its mandates of either kind have no account: a source whose mandates have all resolved
// is present with 0.
func (c Core) UnresolvedMandates(ctx context.Context) (map[string]int, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT source, count(*) FILTER (WHERE account_id IS NULL)::int FROM (
		  SELECT source, account_id FROM core.standing_order
		  UNION ALL SELECT source, account_id FROM core.direct_debit) m
		GROUP BY source`))
	if err != nil {
		return nil, fmt.Errorf("store: unresolved mandates: %w", err)
	}
	out := map[string]int{}
	var source string
	var n int
	_, err = pgx.ForEachRow(rows, []any{&source, &n}, func() error {
		out[source] = n
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: unresolved mandates: %w", err)
	}
	return out, nil
}

// MandateState is the version state of a stored mandate.
type MandateState struct {
	VersionCount  int
	CurrentHash   []byte
	FirstSeenAt   time.Time
	LastSeenAt    time.Time
	DisappearedAt *time.Time
}

// StandingOrder is a core.standing_order row. It mirrors its latest version. Dates are dates.
type StandingOrder struct {
	ID               uuid.UUID
	AccountID        *uuid.UUID // nil while no account has BankRef
	BankRef          string
	Source           string
	MandateKey       string
	Payee            *string
	Reference        *string
	Frequency        *string
	Status           *string
	FirstDate        *time.Time
	FirstAmountMinor *int64
	NextDate         *time.Time
	NextAmountMinor  *int64
	FinalDate        *time.Time
	FinalAmountMinor *int64
	Currency         *string
	MandateState
}

// DirectDebit is a core.direct_debit row. It mirrors its latest version. Dates are dates.
type DirectDebit struct {
	ID                  uuid.UUID
	AccountID           *uuid.UUID // nil while no account has BankRef
	BankRef             string
	Source              string
	MandateKey          string
	Name                *string
	Status              *string
	PreviousDate        *time.Time
	PreviousAmountMinor *int64
	Currency            *string
	MandateState
}

// MandateVersion is a row of core.standing_order_version or core.direct_debit_version.
type MandateVersion struct {
	MandateID   uuid.UUID
	VersionNo   int
	Content     json.RawMessage
	ContentHash []byte
	SeenAt      time.Time
	RawSeq      int64
}

const standingOrderColumns = `id, account_id, bank_ref, source, mandate_key, payee, reference, frequency, status,
	first_date, first_amount_minor, next_date, next_amount_minor, final_date, final_amount_minor, currency,
	version_count, current_hash, first_seen_at, last_seen_at, disappeared_at`

const directDebitColumns = `id, account_id, bank_ref, source, mandate_key, name, status, previous_date,
	previous_amount_minor, currency, version_count, current_hash, first_seen_at, last_seen_at, disappeared_at`

// GetStandingOrder returns the standing order with id, or ErrNotFound.
func (c Core) GetStandingOrder(ctx context.Context, id uuid.UUID) (StandingOrder, error) {
	var s StandingOrder
	err := c.q.QueryRow(ctx, c.sql(`SELECT `+standingOrderColumns+` FROM core.standing_order WHERE id = $1`), id).
		Scan(&s.ID, &s.AccountID, &s.BankRef, &s.Source, &s.MandateKey, &s.Payee, &s.Reference, &s.Frequency, &s.Status,
			&s.FirstDate, &s.FirstAmountMinor, &s.NextDate, &s.NextAmountMinor, &s.FinalDate, &s.FinalAmountMinor,
			&s.Currency, &s.VersionCount, &s.CurrentHash, &s.FirstSeenAt, &s.LastSeenAt, &s.DisappearedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return StandingOrder{}, ErrNotFound
	}
	if err != nil {
		return StandingOrder{}, fmt.Errorf("store: get standing order: %w", err)
	}
	return s, nil
}

// GetDirectDebit returns the direct debit with id, or ErrNotFound.
func (c Core) GetDirectDebit(ctx context.Context, id uuid.UUID) (DirectDebit, error) {
	var d DirectDebit
	err := c.q.QueryRow(ctx, c.sql(`SELECT `+directDebitColumns+` FROM core.direct_debit WHERE id = $1`), id).
		Scan(&d.ID, &d.AccountID, &d.BankRef, &d.Source, &d.MandateKey, &d.Name, &d.Status, &d.PreviousDate,
			&d.PreviousAmountMinor, &d.Currency, &d.VersionCount, &d.CurrentHash, &d.FirstSeenAt, &d.LastSeenAt,
			&d.DisappearedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DirectDebit{}, ErrNotFound
	}
	if err != nil {
		return DirectDebit{}, fmt.Errorf("store: get direct debit: %w", err)
	}
	return d, nil
}

// ListMandateVersions returns the versions of the mandate of kind with id, in version order.
func (c Core) ListMandateVersions(ctx context.Context, kind string, id uuid.UUID) ([]MandateVersion, error) {
	_, versions, idColumn, err := mandateTables(kind)
	if err != nil {
		return nil, err
	}
	rows, err := c.q.Query(ctx, c.sql(`SELECT `+idColumn+`, version_no, content, content_hash, seen_at, raw_seq
		FROM `+versions+` WHERE `+idColumn+` = $1 ORDER BY version_no`), id)
	if err != nil {
		return nil, fmt.Errorf("store: list mandate versions: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (MandateVersion, error) {
		var v MandateVersion
		var content []byte
		err := r.Scan(&v.MandateID, &v.VersionNo, &content, &v.ContentHash, &v.SeenAt, &v.RawSeq)
		v.Content = content
		return v, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list mandate versions: %w", err)
	}
	return out, nil
}

// ApplyStandingOrder is the version logic of ApplyTransaction for a standing order: s carries the
// business columns and the resolved account (its MandateState is ignored); content and hash are
// the version's. The row's account follows every record applied, so a mandate stored with no
// account takes one as soon as a record of it resolves. disappeared_at is kept: only a mandate
// list (SetMandatesDisappeared) sets or clears it.
func (c Core) ApplyStandingOrder(ctx context.Context, s StandingOrder, content json.RawMessage, hash []byte, seenAt time.Time, rawSeq int64) (ApplyOutcome, error) {
	return c.applyMandate(ctx, MandateStandingOrder, s.ID, s.AccountID, func(st MandateState) error {
		_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.standing_order (`+standingOrderColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
			ON CONFLICT (id) DO UPDATE SET account_id = EXCLUDED.account_id, payee = EXCLUDED.payee,
			  reference = EXCLUDED.reference, frequency = EXCLUDED.frequency, status = EXCLUDED.status,
			  first_date = EXCLUDED.first_date, first_amount_minor = EXCLUDED.first_amount_minor,
			  next_date = EXCLUDED.next_date, next_amount_minor = EXCLUDED.next_amount_minor,
			  final_date = EXCLUDED.final_date, final_amount_minor = EXCLUDED.final_amount_minor,
			  currency = EXCLUDED.currency, version_count = EXCLUDED.version_count,
			  current_hash = EXCLUDED.current_hash, first_seen_at = EXCLUDED.first_seen_at,
			  last_seen_at = EXCLUDED.last_seen_at, disappeared_at = EXCLUDED.disappeared_at`),
			s.ID, s.AccountID, s.BankRef, s.Source, s.MandateKey, s.Payee, s.Reference, s.Frequency, s.Status,
			s.FirstDate, s.FirstAmountMinor, s.NextDate, s.NextAmountMinor, s.FinalDate, s.FinalAmountMinor,
			s.Currency, st.VersionCount, st.CurrentHash, st.FirstSeenAt, st.LastSeenAt, st.DisappearedAt)
		if err != nil {
			return fmt.Errorf("store: upsert standing order: %w", err)
		}
		return nil
	}, content, hash, seenAt, rawSeq)
}

// ApplyDirectDebit is ApplyStandingOrder for a direct debit.
func (c Core) ApplyDirectDebit(ctx context.Context, d DirectDebit, content json.RawMessage, hash []byte, seenAt time.Time, rawSeq int64) (ApplyOutcome, error) {
	return c.applyMandate(ctx, MandateDirectDebit, d.ID, d.AccountID, func(st MandateState) error {
		_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.direct_debit (`+directDebitColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			ON CONFLICT (id) DO UPDATE SET account_id = EXCLUDED.account_id, name = EXCLUDED.name,
			  status = EXCLUDED.status, previous_date = EXCLUDED.previous_date,
			  previous_amount_minor = EXCLUDED.previous_amount_minor, currency = EXCLUDED.currency,
			  version_count = EXCLUDED.version_count, current_hash = EXCLUDED.current_hash,
			  first_seen_at = EXCLUDED.first_seen_at, last_seen_at = EXCLUDED.last_seen_at,
			  disappeared_at = EXCLUDED.disappeared_at`),
			d.ID, d.AccountID, d.BankRef, d.Source, d.MandateKey, d.Name, d.Status, d.PreviousDate,
			d.PreviousAmountMinor, d.Currency, st.VersionCount, st.CurrentHash, st.FirstSeenAt, st.LastSeenAt,
			st.DisappearedAt)
		if err != nil {
			return fmt.Errorf("store: upsert direct debit: %w", err)
		}
		return nil
	}, content, hash, seenAt, rawSeq)
}

// applyMandate runs the version logic of ApplyTransaction over a mandate table: write is called
// with the row's new version state to insert or rewrite the row with its business columns.
func (c Core) applyMandate(ctx context.Context, kind string, id uuid.UUID, accountID *uuid.UUID, write func(MandateState) error,
	content json.RawMessage, hash []byte, seenAt time.Time, rawSeq int64,
) (ApplyOutcome, error) {
	table, versions, idColumn, err := mandateTables(kind)
	if err != nil {
		return "", err
	}
	insertVersion := func(no int) error {
		if _, err := c.q.Exec(ctx, c.sql(`INSERT INTO `+versions+` (`+idColumn+`, version_no, content, content_hash, seen_at, raw_seq)
			VALUES ($1, $2, $3, $4, $5, $6)`), id, no, string(content), hash, seenAt, rawSeq); err != nil {
			return fmt.Errorf("store: insert %s version: %w", kind, err)
		}
		return nil
	}
	var prev MandateState
	err = c.q.QueryRow(ctx, c.sql(`SELECT version_count, current_hash, first_seen_at, last_seen_at, disappeared_at
		FROM `+table+` WHERE id = $1 FOR UPDATE`), id).
		Scan(&prev.VersionCount, &prev.CurrentHash, &prev.FirstSeenAt, &prev.LastSeenAt, &prev.DisappearedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err := write(MandateState{VersionCount: 1, CurrentHash: hash, FirstSeenAt: seenAt, LastSeenAt: seenAt}); err != nil {
			return "", err
		}
		if err := insertVersion(1); err != nil {
			return "", err
		}
		return TransactionCreated, nil
	case err != nil:
		return "", fmt.Errorf("store: get %s state: %w", kind, err)
	}
	if bytes.Equal(prev.CurrentHash, hash) {
		if _, err := c.q.Exec(ctx, c.sql(`UPDATE `+table+` SET account_id = $2,
			first_seen_at = least(first_seen_at, $3), last_seen_at = greatest(last_seen_at, $3) WHERE id = $1`),
			id, accountID, seenAt); err != nil {
			return "", fmt.Errorf("store: touch %s: %w", kind, err)
		}
		return TransactionUnchanged, nil
	}
	first := prev.FirstSeenAt
	if seenAt.Before(first) {
		first = seenAt
	}
	no := prev.VersionCount + 1
	if err := insertVersion(no); err != nil {
		return "", err
	}
	if seenAt.Before(prev.LastSeenAt) {
		if _, err := c.q.Exec(ctx, c.sql(`UPDATE `+table+` SET version_count = $2, first_seen_at = $3, account_id = $4
			WHERE id = $1`), id, no, first, accountID); err != nil {
			return "", fmt.Errorf("store: count older %s version: %w", kind, err)
		}
		return TransactionOlderVersion, nil
	}
	if err := write(MandateState{VersionCount: no, CurrentHash: hash, FirstSeenAt: first, LastSeenAt: seenAt,
		DisappearedAt: prev.DisappearedAt}); err != nil {
		return "", err
	}
	return TransactionChanged, nil
}

// SetMandatesDisappeared applies a complete list of source's mandates of kind for bankRef, seen at
// at: every such mandate not in present, not already disappeared and last seen no later than at
// gets disappeared_at = at; every one in present that disappeared no later than at has it
// cleared. It returns how many disappeared and how many returned.
func (c Core) SetMandatesDisappeared(ctx context.Context, kind, source, bankRef string, present []uuid.UUID, at time.Time) (disappeared, returned int, err error) {
	table, _, _, err := mandateTables(kind)
	if err != nil {
		return 0, 0, err
	}
	if present == nil {
		present = []uuid.UUID{}
	}
	err = c.q.QueryRow(ctx, c.sql(`
		WITH gone AS (
		  UPDATE `+table+` SET disappeared_at = $4
		  WHERE source = $1 AND bank_ref = $2 AND NOT (id = ANY($3)) AND disappeared_at IS NULL AND last_seen_at <= $4
		  RETURNING id
		), back AS (
		  UPDATE `+table+` SET disappeared_at = NULL
		  WHERE source = $1 AND bank_ref = $2 AND id = ANY($3) AND disappeared_at <= $4
		  RETURNING id
		)
		SELECT (SELECT count(*) FROM gone), (SELECT count(*) FROM back)`),
		source, bankRef, present, at).Scan(&disappeared, &returned)
	if err != nil {
		return 0, 0, fmt.Errorf("store: set %s disappeared: %w", kind, err)
	}
	return disappeared, returned, nil
}

// AccountsByBankRef returns the accounts with bankRef and no end date, ordered by slug.
func (c Core) AccountsByBankRef(ctx context.Context, bankRef string) ([]Account, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT `+accountColumns+` FROM core.account
		WHERE bank_ref = $1 AND ended_on IS NULL ORDER BY slug`), bankRef)
	if err != nil {
		return nil, fmt.Errorf("store: accounts by bank reference: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanAccount)
	if err != nil {
		return nil, fmt.Errorf("store: accounts by bank reference: %w", err)
	}
	return out, nil
}

// SharedBankRefs returns every account that shares its bank reference with an account of another
// source while neither has an end date, mapped to the slug of the first such other account by
// slug.
func (c Core) SharedBankRefs(ctx context.Context) (map[uuid.UUID]string, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT DISTINCT ON (a.id) a.id, b.slug
		FROM core.account a
		JOIN core.account b ON b.bank_ref = a.bank_ref AND b.source <> a.source AND b.ended_on IS NULL
		WHERE a.bank_ref IS NOT NULL AND a.ended_on IS NULL
		ORDER BY a.id, b.slug`))
	if err != nil {
		return nil, fmt.Errorf("store: shared bank references: %w", err)
	}
	out := map[uuid.UUID]string{}
	var id uuid.UUID
	var slug string
	if _, err := pgx.ForEachRow(rows, []any{&id, &slug}, func() error {
		out[id] = slug
		return nil
	}); err != nil {
		return nil, fmt.Errorf("store: shared bank references: %w", err)
	}
	return out, nil
}

// ListReconciliations returns every reconciliation row, ordered by account id.
func (c Core) ListReconciliations(ctx context.Context) ([]Reconciliation, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT `+reconciliationColumns+` FROM core.reconciliation ORDER BY account_id`))
	if err != nil {
		return nil, fmt.Errorf("store: list reconciliations: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Reconciliation, error) {
		var x Reconciliation
		err := r.Scan(&x.AccountID, &x.Method, &x.RunningSumMinor, &x.LastCheckAt, &x.LastCheckOK, &x.LastDiffMinor,
			&x.LastPassAt, &x.ReconciledThrough, &x.BreakCount, &x.KnownException, &x.AcceptedDiffMinor, &x.FailedOn)
		return x, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list reconciliations: %w", err)
	}
	return out, nil
}

// DayRow is one transaction as the running_balance check reads it: its day, its amount and the
// content of its latest version.
type DayRow struct {
	TxnDate     time.Time // a date
	AmountMinor int64
	Content     json.RawMessage
}

// LiveTransactionRows returns the account's transactions that have not disappeared and are not
// declined, with their latest version's content, ordered by day, then source key.
func (c Core) LiveTransactionRows(ctx context.Context, accountID uuid.UUID) ([]DayRow, error) {
	// The current version is the newest one with the row's hash: an older version loaded late has
	// a higher version number but is not what the row mirrors.
	rows, err := c.q.Query(ctx, c.sql(`SELECT t.txn_date, t.amount_minor, v.content
		FROM core.transaction t
		CROSS JOIN LATERAL (SELECT content FROM core.transaction_version
		  WHERE transaction_id = t.id AND content_hash = t.current_hash
		  ORDER BY version_no DESC LIMIT 1) v
		WHERE t.account_id = $1 AND t.disappeared_at IS NULL AND t.status <> 'declined'
		ORDER BY t.txn_date, t.source_key`), accountID)
	if err != nil {
		return nil, fmt.Errorf("store: live transaction rows: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (DayRow, error) {
		var d DayRow
		var content []byte
		err := r.Scan(&d.TxnDate, &d.AmountMinor, &content)
		d.Content = content
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: live transaction rows: %w", err)
	}
	return out, nil
}

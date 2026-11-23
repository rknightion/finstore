package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Units and prices cross this boundary as decimal text: they are bound as text, cast to numeric by
// the statement, and read back with ::text, so no value ever passes through a float.

// Instrument is a core.instrument row: one holdable item at a provider.
type Instrument struct {
	ID          uuid.UUID
	Provider    string
	ProviderKey string
	Ticker      *string
}

// EnsureInstrument inserts i unless the instrument exists. Nothing in the row depends on import
// order, so an existing row is left as it is.
func (c Core) EnsureInstrument(ctx context.Context, i Instrument) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.instrument (id, provider, provider_key, ticker)
		VALUES ($1, $2, $3, $4) ON CONFLICT (id) DO NOTHING`), i.ID, i.Provider, i.ProviderKey, i.Ticker)
	if err != nil {
		return fmt.Errorf("store: ensure instrument: %w", err)
	}
	return nil
}

// InvestmentImport is a core.investment_import row: one accepted investment file.
type InvestmentImport struct {
	RawSeq     int64
	PlatformID uuid.UUID
	Format     string
	Kind       string
	RowCount   int
	CapturedAt *time.Time
	ImportedAt time.Time
}

// InsertInvestmentImport inserts i.
func (c Core) InsertInvestmentImport(ctx context.Context, i InvestmentImport) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.investment_import
		(raw_seq, platform_id, format, kind, row_count, captured_at, imported_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`),
		i.RawSeq, i.PlatformID, i.Format, i.Kind, i.RowCount, i.CapturedAt, i.ImportedAt)
	if err != nil {
		return fmt.Errorf("store: insert investment import: %w", err)
	}
	return nil
}

const investmentImportColumns = `raw_seq, platform_id, format, kind, row_count, captured_at, imported_at`

func scanInvestmentImport(r pgx.CollectableRow) (InvestmentImport, error) {
	var i InvestmentImport
	err := r.Scan(&i.RawSeq, &i.PlatformID, &i.Format, &i.Kind, &i.RowCount, &i.CapturedAt, &i.ImportedAt)
	return i, err
}

// GetInvestmentImport returns the accepted file with this raw seq, or ErrNotFound.
func (c Core) GetInvestmentImport(ctx context.Context, rawSeq int64) (InvestmentImport, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT `+investmentImportColumns+` FROM core.investment_import WHERE raw_seq = $1`), rawSeq)
	if err != nil {
		return InvestmentImport{}, fmt.Errorf("store: get investment import: %w", err)
	}
	i, err := pgx.CollectExactlyOneRow(rows, scanInvestmentImport)
	if errors.Is(err, pgx.ErrNoRows) {
		return InvestmentImport{}, ErrNotFound
	}
	if err != nil {
		return InvestmentImport{}, fmt.Errorf("store: get investment import: %w", err)
	}
	return i, nil
}

// ListInvestmentImports returns the platform's accepted files of the given kinds, in raw seq order.
func (c Core) ListInvestmentImports(ctx context.Context, platformID uuid.UUID, kinds []string) ([]InvestmentImport, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT `+investmentImportColumns+` FROM core.investment_import
		WHERE platform_id = $1 AND kind = ANY($2) ORDER BY raw_seq`), platformID, kinds)
	if err != nil {
		return nil, fmt.Errorf("store: list investment imports: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanInvestmentImport)
	if err != nil {
		return nil, fmt.Errorf("store: list investment imports: %w", err)
	}
	return out, nil
}

// LatestLedgerImport returns the newest imported_at of the platform's ledger files, or nil when it
// has none. A transactions export states no date of its own, so this is the latest it can have
// been taken.
func (c Core) LatestLedgerImport(ctx context.Context, platformID uuid.UUID) (*time.Time, error) {
	var at *time.Time
	err := c.q.QueryRow(ctx, c.sql(`SELECT max(imported_at) FROM core.investment_import
		WHERE platform_id = $1 AND kind = 'ledger'`), platformID).Scan(&at)
	if err != nil {
		return nil, fmt.Errorf("store: latest ledger import: %w", err)
	}
	return at, nil
}

// GetImportFile returns the raw import file with this seq, or ErrNotFound.
func GetImportFile(ctx context.Context, q DBTX, seq int64) (ImportFile, error) {
	var f ImportFile
	err := q.QueryRow(ctx, `SELECT seq, account_slug, format, file_name, sha256, size_bytes, imported_at, body
		FROM raw.import_file WHERE seq = $1`, seq).
		Scan(&f.Seq, &f.AccountSlug, &f.Format, &f.FileName, &f.SHA256, &f.SizeBytes, &f.ImportedAt, &f.Body)
	if errors.Is(err, pgx.ErrNoRows) {
		return ImportFile{}, ErrNotFound
	}
	if err != nil {
		return ImportFile{}, fmt.Errorf("store: get import file: %w", err)
	}
	return f, nil
}

// ListChildAccounts returns the accounts whose parent is parentID, ordered by slug.
func (c Core) ListChildAccounts(ctx context.Context, parentID uuid.UUID) ([]Account, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT `+accountColumns+` FROM core.account WHERE parent_id = $1 ORDER BY slug`), parentID)
	if err != nil {
		return nil, fmt.Errorf("store: list child accounts: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanAccount)
	if err != nil {
		return nil, fmt.Errorf("store: list child accounts: %w", err)
	}
	return out, nil
}

// InvestmentEvent is a core.investment_event row. Its version columns are set by
// ApplyInvestmentEvent and ignored on the way in.
type InvestmentEvent struct {
	ID             uuid.UUID
	AccountID      uuid.UUID
	Source         string
	SourceKey      string
	EventType      string
	ProviderType   string
	InstrumentID   *uuid.UUID
	InstrumentName *string
	OrderDate      time.Time  // a date
	SettleDate     *time.Time // a date
	Units          *string    // decimal text, unsigned; nil on a cash row
	Price          *string    // decimal text
	PriceUnit      *string
	GrossMinor     int64
	CashMinor      int64
	FlowMinor      int64
	Currency       string
	VersionCount   int
	CurrentHash    []byte
	FirstSeenAt    time.Time
	LastSeenAt     time.Time
	DisappearedAt  *time.Time
}

// EffectiveDate is settle_date, or order_date when the event has none.
func (e InvestmentEvent) EffectiveDate() time.Time {
	if e.SettleDate != nil {
		return *e.SettleDate
	}
	return e.OrderDate
}

const investmentEventColumns = `id, account_id, source, source_key, event_type, provider_type, instrument_id,
	instrument_name, order_date, settle_date, units::text, price::text, price_unit, gross_minor, cash_minor,
	flow_minor, currency, version_count, current_hash, first_seen_at, last_seen_at, disappeared_at`

func scanInvestmentEvent(r pgx.CollectableRow) (InvestmentEvent, error) {
	var e InvestmentEvent
	err := r.Scan(&e.ID, &e.AccountID, &e.Source, &e.SourceKey, &e.EventType, &e.ProviderType, &e.InstrumentID,
		&e.InstrumentName, &e.OrderDate, &e.SettleDate, &e.Units, &e.Price, &e.PriceUnit, &e.GrossMinor, &e.CashMinor,
		&e.FlowMinor, &e.Currency, &e.VersionCount, &e.CurrentHash, &e.FirstSeenAt, &e.LastSeenAt, &e.DisappearedAt)
	return e, err
}

// GetInvestmentEvent returns the event with id, or ErrNotFound.
func (c Core) GetInvestmentEvent(ctx context.Context, id uuid.UUID) (InvestmentEvent, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT `+investmentEventColumns+` FROM core.investment_event WHERE id = $1`), id)
	if err != nil {
		return InvestmentEvent{}, fmt.Errorf("store: get investment event: %w", err)
	}
	e, err := pgx.CollectExactlyOneRow(rows, scanInvestmentEvent)
	if errors.Is(err, pgx.ErrNoRows) {
		return InvestmentEvent{}, ErrNotFound
	}
	if err != nil {
		return InvestmentEvent{}, fmt.Errorf("store: get investment event: %w", err)
	}
	return e, nil
}

// ListInvestmentEvents returns every event of the account, disappeared ones included, ordered by id.
func (c Core) ListInvestmentEvents(ctx context.Context, accountID uuid.UUID) ([]InvestmentEvent, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT `+investmentEventColumns+` FROM core.investment_event
		WHERE account_id = $1 ORDER BY id`), accountID)
	if err != nil {
		return nil, fmt.Errorf("store: list investment events: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanInvestmentEvent)
	if err != nil {
		return nil, fmt.Errorf("store: list investment events: %w", err)
	}
	return out, nil
}

func (c Core) writeInvestmentEvent(ctx context.Context, e InvestmentEvent) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.investment_event (id, account_id, source, source_key, event_type,
		  provider_type, instrument_id, instrument_name, order_date, settle_date, units, price, price_unit,
		  gross_minor, cash_minor, flow_minor, currency, version_count, current_hash, first_seen_at, last_seen_at,
		  disappeared_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::text::numeric, $12::text::numeric, $13, $14, $15, $16,
		  $17, $18, $19, $20, $21, $22)
		ON CONFLICT (id) DO UPDATE SET event_type = EXCLUDED.event_type, provider_type = EXCLUDED.provider_type,
		  instrument_id = EXCLUDED.instrument_id, instrument_name = EXCLUDED.instrument_name,
		  order_date = EXCLUDED.order_date, settle_date = EXCLUDED.settle_date, units = EXCLUDED.units,
		  price = EXCLUDED.price, price_unit = EXCLUDED.price_unit, gross_minor = EXCLUDED.gross_minor,
		  cash_minor = EXCLUDED.cash_minor, flow_minor = EXCLUDED.flow_minor, currency = EXCLUDED.currency,
		  version_count = EXCLUDED.version_count, current_hash = EXCLUDED.current_hash,
		  first_seen_at = EXCLUDED.first_seen_at, last_seen_at = EXCLUDED.last_seen_at,
		  disappeared_at = EXCLUDED.disappeared_at`),
		e.ID, e.AccountID, e.Source, e.SourceKey, e.EventType, e.ProviderType, e.InstrumentID, e.InstrumentName,
		e.OrderDate, e.SettleDate, e.Units, e.Price, e.PriceUnit, e.GrossMinor, e.CashMinor, e.FlowMinor, e.Currency,
		e.VersionCount, e.CurrentHash, e.FirstSeenAt, e.LastSeenAt, e.DisappearedAt)
	if err != nil {
		return fmt.Errorf("store: write investment event: %w", err)
	}
	return nil
}

func (c Core) insertInvestmentEventVersion(ctx context.Context, id uuid.UUID, no int, content json.RawMessage, hash []byte, seenAt time.Time, rawSeq int64) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.investment_event_version
		(event_id, version_no, content, content_hash, seen_at, raw_seq) VALUES ($1, $2, $3, $4, $5, $6)`),
		id, no, string(content), hash, seenAt, rawSeq)
	if err != nil {
		return fmt.Errorf("store: insert investment event version: %w", err)
	}
	return nil
}

// ApplyInvestmentEvent is ApplyTransaction's version logic for an investment event: a sighting adds
// a version unless its state is recorded beside it in seen time; the row mirrors the newest
// sighting by seen time (at equal times, the later raw row) and keeps disappeared_at; an older
// sighting, such as an old file imported late, never changes the row. e's version columns are
// ignored.
func (c Core) ApplyInvestmentEvent(ctx context.Context, e InvestmentEvent, content json.RawMessage, hash []byte, seenAt time.Time, rawSeq int64) (ApplyOutcome, error) {
	prev, err := c.GetInvestmentEvent(ctx, e.ID)
	if errors.Is(err, ErrNotFound) {
		e.VersionCount, e.CurrentHash, e.FirstSeenAt, e.LastSeenAt, e.DisappearedAt = 1, hash, seenAt, seenAt, nil
		if err := c.writeInvestmentEvent(ctx, e); err != nil {
			return "", err
		}
		if err := c.insertInvestmentEventVersion(ctx, e.ID, 1, content, hash, seenAt, rawSeq); err != nil {
			return "", err
		}
		return TransactionCreated, nil
	}
	if err != nil {
		return "", err
	}
	recorded, err := c.recordedBeside(ctx, "core.investment_event_version", "event_id", e.ID, hash, seenAt, rawSeq)
	if err != nil {
		return "", err
	}
	versionNo := prev.VersionCount
	if !recorded {
		versionNo++
		if err := c.insertInvestmentEventVersion(ctx, e.ID, versionNo, content, hash, seenAt, rawSeq); err != nil {
			return "", err
		}
	}
	if !seenAt.Before(prev.LastSeenAt) && !bytes.Equal(prev.CurrentHash, hash) {
		e.VersionCount, e.CurrentHash, e.FirstSeenAt, e.LastSeenAt, e.DisappearedAt =
			versionNo, hash, prev.FirstSeenAt, seenAt, prev.DisappearedAt
		if seenAt.Before(prev.FirstSeenAt) {
			e.FirstSeenAt = seenAt
		}
		if err := c.writeInvestmentEvent(ctx, e); err != nil {
			return "", err
		}
		return TransactionChanged, nil
	}
	if _, err := c.q.Exec(ctx, c.sql(`UPDATE core.investment_event SET version_count = $2,
		first_seen_at = least(first_seen_at, $3), last_seen_at = greatest(last_seen_at, $3) WHERE id = $1`),
		e.ID, versionNo, seenAt); err != nil {
		return "", fmt.Errorf("store: touch investment event: %w", err)
	}
	if recorded {
		return TransactionUnchanged, nil
	}
	return TransactionOlderVersion, nil
}

// SetInvestmentEventsDisappeared makes gone the account's disappeared contributions and transfers
// out: a row in gone keeps the time it already disappeared at or takes at, and any other row of
// those two types has disappeared_at cleared. No other event type ever disappears.
func (c Core) SetInvestmentEventsDisappeared(ctx context.Context, accountID uuid.UUID, gone []uuid.UUID, at time.Time) error {
	if gone == nil {
		gone = []uuid.UUID{}
	}
	_, err := c.q.Exec(ctx, c.sql(`UPDATE core.investment_event
		SET disappeared_at = CASE WHEN id = ANY($2) THEN coalesce(disappeared_at, $3::timestamptz) END
		WHERE account_id = $1 AND event_type IN ('contribution', 'transfer_out')
		  AND (disappeared_at IS NOT NULL OR id = ANY($2))`), accountID, gone, at)
	if err != nil {
		return fmt.Errorf("store: set investment events disappeared: %w", err)
	}
	return nil
}

// HoldingSnapshot is a core.holding_snapshot row: one holding at one capture.
type HoldingSnapshot struct {
	AccountID      uuid.UUID
	RawSeq         int64
	InstrumentID   uuid.UUID
	InstrumentName string
	AsOf           time.Time // a date
	CapturedAt     time.Time
	Units          string // decimal text
	Price          string // decimal text
	PriceUnit      string
	ValueMinor     int64
	BookCostMinor  *int64
	Currency       string
}

// InsertHoldingSnapshot inserts h.
func (c Core) InsertHoldingSnapshot(ctx context.Context, h HoldingSnapshot) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.holding_snapshot (account_id, raw_seq, instrument_id, instrument_name,
		  as_of, captured_at, units, price, price_unit, value_minor, book_cost_minor, currency)
		VALUES ($1, $2, $3, $4, $5, $6, $7::text::numeric, $8::text::numeric, $9, $10, $11, $12)`),
		h.AccountID, h.RawSeq, h.InstrumentID, h.InstrumentName, h.AsOf, h.CapturedAt, h.Units, h.Price, h.PriceUnit,
		h.ValueMinor, h.BookCostMinor, h.Currency)
	if err != nil {
		return fmt.Errorf("store: insert holding snapshot: %w", err)
	}
	return nil
}

// ListHoldingSnapshots returns the account's holdings at one capture, ordered by instrument.
func (c Core) ListHoldingSnapshots(ctx context.Context, accountID uuid.UUID, rawSeq int64) ([]HoldingSnapshot, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT account_id, raw_seq, instrument_id, instrument_name, as_of, captured_at,
		units::text, price::text, price_unit, value_minor, book_cost_minor, currency
		FROM core.holding_snapshot WHERE account_id = $1 AND raw_seq = $2 ORDER BY instrument_id`), accountID, rawSeq)
	if err != nil {
		return nil, fmt.Errorf("store: list holding snapshots: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (HoldingSnapshot, error) {
		var h HoldingSnapshot
		err := r.Scan(&h.AccountID, &h.RawSeq, &h.InstrumentID, &h.InstrumentName, &h.AsOf, &h.CapturedAt, &h.Units,
			&h.Price, &h.PriceUnit, &h.ValueMinor, &h.BookCostMinor, &h.Currency)
		return h, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list holding snapshots: %w", err)
	}
	return out, nil
}

// Valuation is a core.valuation row: one account's value at one capture.
type Valuation struct {
	AccountID         uuid.UUID
	RawSeq            int64
	AsOf              time.Time // a date
	CapturedAt        time.Time
	TotalMinor        int64
	CashMinor         int64
	ReportedGainMinor *int64
	Currency          string
}

// InsertValuation inserts v.
func (c Core) InsertValuation(ctx context.Context, v Valuation) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.valuation (account_id, raw_seq, as_of, captured_at, total_minor,
		cash_minor, reported_gain_minor, currency) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`),
		v.AccountID, v.RawSeq, v.AsOf, v.CapturedAt, v.TotalMinor, v.CashMinor, v.ReportedGainMinor, v.Currency)
	if err != nil {
		return fmt.Errorf("store: insert valuation: %w", err)
	}
	return nil
}

// ListValuations returns the account's valuations, newest date first and, within a date, the
// latest capture first (printed capture time, then raw seq).
func (c Core) ListValuations(ctx context.Context, accountID uuid.UUID) ([]Valuation, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT account_id, raw_seq, as_of, captured_at, total_minor, cash_minor,
		reported_gain_minor, currency FROM core.valuation WHERE account_id = $1
		ORDER BY as_of DESC, captured_at DESC, raw_seq DESC`), accountID)
	if err != nil {
		return nil, fmt.Errorf("store: list valuations: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Valuation, error) {
		var v Valuation
		err := r.Scan(&v.AccountID, &v.RawSeq, &v.AsOf, &v.CapturedAt, &v.TotalMinor, &v.CashMinor,
			&v.ReportedGainMinor, &v.Currency)
		return v, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list valuations: %w", err)
	}
	return out, nil
}

// PensionStatement is a core.pension_statement row: one statement file of one account.
type PensionStatement struct {
	AccountID    uuid.UUID
	RawSeq       int64
	PeriodStart  time.Time // a date
	PeriodEnd    time.Time // a date
	IssuedOn     time.Time // a date
	OpeningMinor int64
	PaidInMinor  int64
	PaidOutMinor int64
	GrowthMinor  int64
	ChargesMinor *int64
	ClosingMinor int64
	Charges      json.RawMessage
	Projections  json.RawMessage
	IsWinner     bool
}

// InsertPensionStatement inserts s.
func (c Core) InsertPensionStatement(ctx context.Context, s PensionStatement) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.pension_statement (account_id, raw_seq, period_start, period_end,
		  issued_on, opening_minor, paid_in_minor, paid_out_minor, growth_minor, charges_minor, closing_minor, charges,
		  projections, is_winner)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`),
		s.AccountID, s.RawSeq, s.PeriodStart, s.PeriodEnd, s.IssuedOn, s.OpeningMinor, s.PaidInMinor, s.PaidOutMinor,
		s.GrowthMinor, s.ChargesMinor, s.ClosingMinor, string(s.Charges), string(s.Projections), s.IsWinner)
	if err != nil {
		return fmt.Errorf("store: insert pension statement: %w", err)
	}
	return nil
}

// ListPensionStatements returns the account's statements in winner order: newest issue date first,
// then highest raw seq.
func (c Core) ListPensionStatements(ctx context.Context, accountID uuid.UUID) ([]PensionStatement, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT account_id, raw_seq, period_start, period_end, issued_on, opening_minor,
		paid_in_minor, paid_out_minor, growth_minor, charges_minor, closing_minor, charges, projections, is_winner
		FROM core.pension_statement WHERE account_id = $1 ORDER BY issued_on DESC, raw_seq DESC`), accountID)
	if err != nil {
		return nil, fmt.Errorf("store: list pension statements: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (PensionStatement, error) {
		var s PensionStatement
		var charges, projections []byte
		err := r.Scan(&s.AccountID, &s.RawSeq, &s.PeriodStart, &s.PeriodEnd, &s.IssuedOn, &s.OpeningMinor, &s.PaidInMinor,
			&s.PaidOutMinor, &s.GrowthMinor, &s.ChargesMinor, &s.ClosingMinor, &charges, &projections, &s.IsWinner)
		s.Charges, s.Projections = charges, projections
		return s, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list pension statements: %w", err)
	}
	return out, nil
}

// SetStatementWinners makes winners the account's winning statements, by raw seq.
func (c Core) SetStatementWinners(ctx context.Context, accountID uuid.UUID, winners []int64) error {
	if winners == nil {
		winners = []int64{}
	}
	_, err := c.q.Exec(ctx, c.sql(`UPDATE core.pension_statement SET is_winner = (raw_seq = ANY($2))
		WHERE account_id = $1 AND is_winner IS DISTINCT FROM (raw_seq = ANY($2))`), accountID, winners)
	if err != nil {
		return fmt.Errorf("store: set statement winners: %w", err)
	}
	return nil
}

// TradeNote is a core.trade_note row: one trade confirmation.
type TradeNote struct {
	AccountID  uuid.UUID
	Reference  string
	RawSeq     int64
	ISIN       string
	ExecutedAt time.Time
	Venue      *string
	Units      string // decimal text
	ExactPrice string // decimal text
	GrossMinor int64
	FeeMinor   int64
	TaxMinor   int64
	NetMinor   int64
	SettleDate time.Time // a date
}

// PutTradeNote stores n as the note of its account and reference, unless a note from a higher raw
// seq is already there.
func (c Core) PutTradeNote(ctx context.Context, n TradeNote) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.trade_note AS t (account_id, reference, raw_seq, isin, executed_at, venue,
		  units, exact_price, gross_minor, fee_minor, tax_minor, net_minor, settle_date)
		VALUES ($1, $2, $3, $4, $5, $6, $7::text::numeric, $8::text::numeric, $9, $10, $11, $12, $13)
		ON CONFLICT (account_id, reference) DO UPDATE SET raw_seq = EXCLUDED.raw_seq, isin = EXCLUDED.isin,
		  executed_at = EXCLUDED.executed_at, venue = EXCLUDED.venue, units = EXCLUDED.units,
		  exact_price = EXCLUDED.exact_price, gross_minor = EXCLUDED.gross_minor, fee_minor = EXCLUDED.fee_minor,
		  tax_minor = EXCLUDED.tax_minor, net_minor = EXCLUDED.net_minor, settle_date = EXCLUDED.settle_date
		WHERE EXCLUDED.raw_seq >= t.raw_seq`),
		n.AccountID, n.Reference, n.RawSeq, n.ISIN, n.ExecutedAt, n.Venue, n.Units, n.ExactPrice, n.GrossMinor,
		n.FeeMinor, n.TaxMinor, n.NetMinor, n.SettleDate)
	if err != nil {
		return fmt.Errorf("store: put trade note: %w", err)
	}
	return nil
}

// HoldingCheck is a core.holding_check row: one instrument in an account's last checked snapshot.
type HoldingCheck struct {
	AccountID     uuid.UUID
	InstrumentID  uuid.UUID
	CheckedAsOf   time.Time // a date
	ExpectedUnits string    // decimal text
	ReportedUnits string    // decimal text
	Difference    string    // decimal text, reported minus expected
}

// ReplaceHoldingChecks deletes the account's holding checks and inserts checks in their place, in
// one transaction (a savepoint when c is bound to a transaction).
func (c Core) ReplaceHoldingChecks(ctx context.Context, accountID uuid.UUID, checks []HoldingCheck) error {
	return pgx.BeginFunc(ctx, c.q, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, c.sql(`DELETE FROM core.holding_check WHERE account_id = $1`), accountID); err != nil {
			return fmt.Errorf("store: delete holding checks: %w", err)
		}
		for _, h := range checks {
			if h.AccountID != accountID {
				return errors.New("store: replace holding checks: check for another account")
			}
			if _, err := tx.Exec(ctx, c.sql(`INSERT INTO core.holding_check
				(account_id, instrument_id, checked_as_of, expected_units, reported_units, difference)
				VALUES ($1, $2, $3, $4::text::numeric, $5::text::numeric, $6::text::numeric)`),
				h.AccountID, h.InstrumentID, h.CheckedAsOf, h.ExpectedUnits, h.ReportedUnits, h.Difference); err != nil {
				return fmt.Errorf("store: insert holding check: %w", err)
			}
		}
		return nil
	})
}

// ListHoldingChecks returns the account's holding checks, ordered by instrument.
func (c Core) ListHoldingChecks(ctx context.Context, accountID uuid.UUID) ([]HoldingCheck, error) {
	rows, err := c.q.Query(ctx, c.sql(`SELECT account_id, instrument_id, checked_as_of, expected_units::text,
		reported_units::text, difference::text FROM core.holding_check WHERE account_id = $1 ORDER BY instrument_id`), accountID)
	if err != nil {
		return nil, fmt.Errorf("store: list holding checks: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (HoldingCheck, error) {
		var h HoldingCheck
		err := r.Scan(&h.AccountID, &h.InstrumentID, &h.CheckedAsOf, &h.ExpectedUnits, &h.ReportedUnits, &h.Difference)
		return h, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list holding checks: %w", err)
	}
	return out, nil
}

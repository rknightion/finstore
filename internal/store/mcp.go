package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The MCP reader: how an agent's SQL and the curated tool queries reach the database. Every call
// runs alone, in a read-only transaction, on a connection that is closed afterwards, as the login
// role RoleMCP. Postgres privileges keep it away from secret and from writes; the Go code below
// bounds time, rows, bytes and what a call can leave behind in its session.

// Limits of the reader, shared with the tools that call it.
const (
	// ReaderWait is how long a call waits for the one connection before it gives up with ErrBusy.
	ReaderWait = 30 * time.Second
	// ReaderDeadline is the Go deadline of a whole call, the wait included. The statement itself is
	// bounded by RoleMCP's 15 s statement_timeout.
	ReaderDeadline = 50 * time.Second

	// CuratedDefaultLimit and CuratedMaxLimit are the row limit of a curated tool when the caller
	// gives none, and the most it may ask for. CuratedMaxBytes caps the encoded size of its rows.
	CuratedDefaultLimit = 100
	CuratedMaxLimit     = 1000
	CuratedMaxBytes     = 512 << 10
	// QueryDefaultRows, QueryMaxRows and QueryMaxBytes are the limits of query_sql.
	QueryDefaultRows = 200
	QueryMaxRows     = 2000
	QueryMaxBytes    = 1 << 20
)

// Outcomes of a reader call, as Outcome classifies them. They are the values logs and metrics carry.
const (
	OutcomeOK      = "ok"
	OutcomeInvalid = "invalid"
	OutcomeBusy    = "busy"
	OutcomeTimeout = "timeout"
	OutcomeRefused = "refused"
	OutcomeError   = "error"
)

var (
	// ErrBusy is returned when the wait for the reader's one connection ran out.
	ErrBusy = errors.New("store: another query is running; try again")
	// ErrInvalid wraps every error caused by the caller's input: a bad limit, an empty statement,
	// an unknown enum value, a date that does not parse, a slug that is not a slug.
	ErrInvalid = errors.New("store: invalid input")
)

// Outcome classifies the error of a reader call (nil for success, truncated or not) into one of the
// Outcome constants: ErrInvalid is invalid, ErrBusy busy, SQLSTATE 57014 (cancelled, including the
// role's statement_timeout) and the Go deadline timeout, SQLSTATE 42501 (privilege), 25006
// (read-only transaction) and 42601 (syntax, which includes a second statement) refused, and
// anything else error.
func Outcome(err error) string {
	if err == nil {
		return OutcomeOK
	}
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, ErrInvalid):
		return OutcomeInvalid
	case errors.Is(err, ErrBusy):
		return OutcomeBusy
	case errors.Is(err, context.DeadlineExceeded):
		return OutcomeTimeout
	case errors.As(err, &pgErr):
		switch pgErr.Code {
		case "57014":
			return OutcomeTimeout
		case "42501", "25006", "42601":
			return OutcomeRefused
		}
	}
	return OutcomeError
}

// readerRuntimeParams are sent in every connection's startup packet.
var readerRuntimeParams = map[string]string{
	"statement_timeout":                   "15s",
	"lock_timeout":                        "2s",
	"idle_in_transaction_session_timeout": "30s",
	"default_transaction_read_only":       "on",
}

// cancelDeadlineDelay is how long a cancelled call waits for the server to answer its cancel request
// before the socket's deadline cuts the connection.
const cancelDeadlineDelay = 5 * time.Second

// Reader runs the MCP tools' SQL as RoleMCP. Open one with OpenReader.
type Reader struct {
	pool *pgxpool.Pool
	// wait and deadline are ReaderWait and ReaderDeadline; the tests shorten them.
	wait, deadline time.Duration
}

// OpenReader connects a one-connection pool to url, which must name RoleMCP (see CheckRole), and
// checks it with a ping. The pool never keeps a connection: each is closed when its call is done,
// so nothing a call does to its session (an advisory lock, a setting, a prepared statement)
// outlives the call.
//
// It takes WithTelemetry and ForAgentSQL; with ForAgentSQL no database span carries the statement
// or is named after it. The reader does not call otelpgx.RecordStats: a pool of one connection
// that closes after each call has no useful statistics, and they would collide with the app pool's.
func OpenReader(ctx context.Context, url string, opts ...OpenOption) (*Reader, error) {
	oc := newOpenConfig(opts)
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("store: open reader: %w", err)
	}
	// Set after parsing, so nothing in the URL (pool_max_conns) overrides them.
	cfg.MaxConns = 1
	cfg.MinConns = 0
	cfg.AfterRelease = func(*pgx.Conn) bool { return false }
	// Startup parameters outrank the role's and the database's settings, which a login as the role can
	// rewrite for itself (ALTER ROLE ... IN DATABASE ... SET statement_timeout = 0). The reader's
	// limits are its own. temp_file_limit stays on the role: only a superuser can set it.
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	for k, v := range readerRuntimeParams {
		cfg.ConnConfig.RuntimeParams[k] = v
	}
	// Cancelling a call (the Go deadline, the caller, a truncated result) sends the server a cancel
	// request, so the statement stops; the default handler only closes the socket, and a statement that
	// does not write to it keeps running.
	cfg.ConnConfig.BuildContextWatcherHandler = func(c *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: c, DeadlineDelay: cancelDeadlineDelay}
	}
	if oc.enabled() {
		cfg.ConnConfig.Tracer = oc.queryTracer()
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: open reader: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping reader: %w", err)
	}
	return &Reader{pool: pool, wait: ReaderWait, deadline: ReaderDeadline}, nil
}

// Close closes the pool.
func (r *Reader) Close() { r.pool.Close() }

// CheckRole returns an error unless the reader is connected as RoleMCP. serve calls it at startup
// so that a connection string naming a stronger role cannot silently give agents write access.
func (r *Reader) CheckRole(ctx context.Context) error {
	rows, _, err := read(ctx, r, "SELECT current_user::text, session_user::text", nil, Limits{MaxRows: 1, MaxBytes: 1024}, nil, nil,
		func(rows pgx.Rows, _ *pgtype.Map) ([2]string, error) {
			var u [2]string
			return u, rows.Scan(&u[0], &u[1])
		})
	if err != nil {
		return fmt.Errorf("store: check reader role: %w", err)
	}
	if len(rows) != 1 {
		return errors.New("store: check reader role: no answer")
	}
	// session_user too: a superuser connection that starts with role=finstore_mcp has the right
	// current_user and can still reset it.
	if rows[0][0] != RoleMCP || rows[0][1] != RoleMCP {
		return fmt.Errorf("store: the reader is connected as %q (session user %q), want %q",
			rows[0][0], rows[0][1], RoleMCP)
	}
	return nil
}

// Limits bound what a call returns: at most MaxRows rows and MaxBytes bytes of encoded rows. A row
// that would take the encoded size past MaxBytes is not returned. Both must be positive.
type Limits struct{ MaxRows, MaxBytes int }

// Result is what Query returns. Rows hold JSON-ready values (see the encoding table on Query).
type Result struct {
	Columns   []string
	Rows      [][]any
	Truncated bool
	Elapsed   time.Duration
}

// binaryOIDs are the result types Query asks Postgres to send in binary, because their text form
// depends on a session setting (DateStyle, bytea_output) or is wasteful (bytea). Every other type
// comes as Postgres text output.
var binaryOIDs = pgx.QueryResultFormatsByOID{
	pgtype.ByteaOID: pgx.BinaryFormatCode, pgtype.DateOID: pgx.BinaryFormatCode,
	pgtype.TimestampOID: pgx.BinaryFormatCode, pgtype.TimestamptzOID: pgx.BinaryFormatCode,
}

// maxExactInt is the largest magnitude a JSON number carries exactly in a float64 reader.
const maxExactInt = 1 << 53

// Query runs one SQL statement as RoleMCP in a read-only transaction and returns its rows,
// encoded for JSON:
//
//	int2, int4, int8     number when within ±2^53, else a decimal string
//	numeric              string
//	float4, float8, bool number, boolean (NaN and the infinities are strings)
//	text, varchar, name  string
//	uuid                 canonical string
//	date                 YYYY-MM-DD
//	timestamptz, timestamp  RFC 3339 in UTC (a timestamp without a zone is read as UTC)
//	jsonb, json          inline JSON
//	bytea                {"bytea_length": n}
//	anything else        Postgres text output as a string (arrays and ranges included)
//	NULL                 nil
//
// A second statement is refused before anything runs. The statement is cancelled once MaxRows rows
// or MaxBytes bytes have been read and another row exists, and Truncated is set; that is not an
// error. Errors are classified by Outcome.
func (r *Reader) Query(ctx context.Context, sql string, limits Limits) (Result, error) {
	start := time.Now()
	var cols []string
	rows, truncated, err := read(ctx, r, sql, nil, limits, binaryOIDs,
		func(fields []pgconn.FieldDescription) {
			for _, f := range fields {
				cols = append(cols, f.Name)
			}
		},
		func(rows pgx.Rows, tm *pgtype.Map) ([]any, error) {
			fields, raws := rows.FieldDescriptions(), rows.RawValues()
			out := make([]any, len(raws))
			for i, raw := range raws {
				v, err := encodeValue(tm, fields[i].DataTypeOID, raw)
				if err != nil {
					return nil, fmt.Errorf("column %q: %w", fields[i].Name, err)
				}
				out[i] = v
			}
			return out, nil
		})
	if err != nil {
		return Result{}, err
	}
	if cols == nil {
		cols = []string{}
	}
	if rows == nil {
		rows = [][]any{}
	}
	return Result{Columns: cols, Rows: rows, Truncated: truncated, Elapsed: time.Since(start)}, nil
}

// read is the one method every reader call goes through. It
//
//  1. acquires the connection, waiting at most r.wait; a timed-out wait is ErrBusy;
//  2. begins a read-only transaction;
//  3. runs one statement with QueryExecModeDescribeExec, which refuses a second statement before
//     anything runs;
//  4. decodes rows until limits.MaxRows have been read or the next row would take the encoded
//     size past limits.MaxBytes; when another row exists it sets truncated and cancels the
//     statement instead of reading the rest;
//  5. rolls back with a context that is not cancelled, ignoring a rollback error on a connection
//     the cancel closed;
//  6. releases the connection, which the pool closes (AfterRelease), ending the session with its
//     locks, settings and prepared statements.
//
// The whole call, wait included, runs under r.deadline.
//
// decode turns the current row into a T; its encoded JSON size is what counts against MaxBytes.
// formats, when not nil, is passed to pgx as the result formats by type OID; onFields, when not
// nil, sees the column descriptions before the first row.
func read[T any](ctx context.Context, r *Reader, sql string, args []any, limits Limits,
	formats pgx.QueryResultFormatsByOID, onFields func([]pgconn.FieldDescription),
	decode func(pgx.Rows, *pgtype.Map) (T, error)) (out []T, truncated bool, err error) {
	if limits.MaxRows <= 0 || limits.MaxBytes <= 0 {
		return nil, false, fmt.Errorf("%w: row and byte limits must be positive", ErrInvalid)
	}
	if strings.TrimSpace(sql) == "" {
		return nil, false, fmt.Errorf("%w: the statement is empty", ErrInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, r.deadline)
	defer cancel()

	acquireCtx, cancelAcquire := context.WithTimeout(ctx, r.wait)
	pc, err := r.pool.Acquire(acquireCtx)
	cancelAcquire()
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return nil, false, ErrBusy
		}
		return nil, false, fmt.Errorf("store: reader: acquire connection: %w", err)
	}
	// Released last: the rollback below runs first. The pool closes the connection.
	defer pc.Release()

	tx, err := pc.Conn().BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, false, fmt.Errorf("store: reader: begin: %w", err)
	}
	defer func() {
		rctx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		// A connection the cancel below closed cannot roll back; the session is gone, which is
		// the same.
		_ = tx.Rollback(rctx)
	}()

	qctx, stopQuery := context.WithCancel(ctx)
	defer stopQuery()
	opts := make([]any, 0, 2+len(args))
	opts = append(opts, pgx.QueryExecModeDescribeExec)
	if formats != nil {
		opts = append(opts, formats)
	}
	rows, err := tx.Query(qctx, sql, append(opts, args...)...)
	if err != nil {
		return nil, false, err
	}
	if onFields != nil {
		onFields(rows.FieldDescriptions())
	}
	tm := pc.Conn().TypeMap()
	total := 0
	for rows.Next() {
		if len(out) >= limits.MaxRows {
			truncated = true
			break
		}
		v, derr := decode(rows, tm)
		if derr != nil {
			stopQuery()
			rows.Close()
			return nil, false, fmt.Errorf("store: reader: decode row: %w", derr)
		}
		size, merr := json.Marshal(v)
		if merr != nil {
			stopQuery()
			rows.Close()
			return nil, false, fmt.Errorf("store: reader: encode row: %w", merr)
		}
		if total+len(size)+1 > limits.MaxBytes {
			truncated = true
			break
		}
		total += len(size) + 1
		out = append(out, v)
	}
	if truncated {
		// Another row exists: stop the statement rather than read the rest. Whatever the cancel
		// does to the connection, the call succeeded.
		stopQuery()
		rows.Close()
		return out, true, nil
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return out, false, nil
}

// infinityText is "infinity" or "-infinity" for a date or timestamp that is one.
func infinityText(m pgtype.InfinityModifier) (string, bool) {
	switch m {
	case pgtype.Infinity:
		return "infinity", true
	case pgtype.NegativeInfinity:
		return "-infinity", true
	}
	return "", false
}

// encodeValue turns one result value into its JSON-ready form. oid is the column's type; raw is
// the value in the format binaryOIDs asked for, nil for NULL.
func encodeValue(tm *pgtype.Map, oid uint32, raw []byte) (any, error) {
	if raw == nil {
		return nil, nil
	}
	text := string(raw)
	switch oid {
	case pgtype.ByteaOID:
		return map[string]int{"bytea_length": len(raw)}, nil
	case pgtype.DateOID:
		var d pgtype.Date
		if err := tm.Scan(oid, pgx.BinaryFormatCode, raw, &d); err != nil {
			return nil, err
		}
		if inf, ok := infinityText(d.InfinityModifier); ok {
			return inf, nil
		}
		return d.Time.Format(time.DateOnly), nil
	case pgtype.TimestamptzOID:
		var t pgtype.Timestamptz
		if err := tm.Scan(oid, pgx.BinaryFormatCode, raw, &t); err != nil {
			return nil, err
		}
		if inf, ok := infinityText(t.InfinityModifier); ok {
			return inf, nil
		}
		return t.Time.UTC().Format(time.RFC3339Nano), nil
	case pgtype.TimestampOID:
		var t pgtype.Timestamp
		if err := tm.Scan(oid, pgx.BinaryFormatCode, raw, &t); err != nil {
			return nil, err
		}
		if inf, ok := infinityText(t.InfinityModifier); ok {
			return inf, nil
		}
		return t.Time.UTC().Format(time.RFC3339Nano), nil
	case pgtype.Int2OID, pgtype.Int4OID, pgtype.Int8OID:
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, err
		}
		if n > maxExactInt || n < -maxExactInt {
			return text, nil
		}
		return n, nil
	case pgtype.Float4OID, pgtype.Float8OID:
		f, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return text, nil
		}
		return f, nil
	case pgtype.BoolOID:
		return text == "t", nil
	case pgtype.JSONBOID, pgtype.JSONOID:
		if json.Valid(raw) {
			return json.RawMessage(append([]byte(nil), raw...)), nil
		}
		return text, nil
	}
	// numeric, text, varchar, name, uuid and every other type: the text output.
	return text, nil
}

// --- the curated queries ---------------------------------------------------------------------

// Overview is what the overview tool returns: every account's coverage and every source's sync
// status.
type Overview struct {
	Accounts []OverviewAccount `json:"accounts"`
	Sources  []SyncStatusRow   `json:"sources"`
}

// AccountGap is one entry of an account's gaps, as agent.coverage reports it. Dates are
// YYYY-MM-DD; for a "no file covers these days" gap From is the last date of the earlier file and
// To the first date of the later one.
type AccountGap struct {
	From      *string `json:"from"`
	To        *string `json:"to"`
	Reason    string  `json:"reason"`
	DiffMinor *int64  `json:"diff_minor,omitempty"`
}

// OverviewAccount is a row of agent.coverage joined to agent.accounts on the slug. Dates are
// YYYY-MM-DD strings. Type, Name, Currency and Closed come from agent.accounts.
type OverviewAccount struct {
	AccountSlug         string       `json:"account_slug"`
	Feed                string       `json:"feed"`
	EarliestDate        *string      `json:"earliest_date"`
	LatestDate          *string      `json:"latest_date"`
	TransactionCount    int64        `json:"transaction_count"`
	ReconciliationState string       `json:"reconciliation_state"`
	ReconciledThrough   *string      `json:"reconciled_through"`
	LastDiffMinor       *int64       `json:"last_diff_minor"`
	Gaps                []AccountGap `json:"gaps"`
	LatestStatementDate *string      `json:"latest_statement_date"`
	FailedOn            *string      `json:"failed_on"`
	Source              string       `json:"source"`
	Type                string       `json:"type"`
	Name                string       `json:"name"`
	Currency            string       `json:"currency"`
	Closed              bool         `json:"closed"`
	EndedOn             *string      `json:"ended_on"`
}

// Overview reads agent.coverage (with the account's type, name, currency, closed and ended_on) and
// agent.sync_status, in two reader calls.
func (r *Reader) Overview(ctx context.Context) (Overview, error) {
	limits := Limits{MaxRows: CuratedMaxLimit, MaxBytes: CuratedMaxBytes}
	accounts, truncated, err := read(ctx, r, `
		SELECT c.account_slug, c.feed,
		       to_char(c.earliest_date, 'YYYY-MM-DD'), to_char(c.latest_date, 'YYYY-MM-DD'),
		       c.transaction_count, c.reconciliation_state,
		       to_char(c.reconciled_through, 'YYYY-MM-DD'), c.last_diff_minor, c.gaps,
		       to_char(c.latest_statement_date, 'YYYY-MM-DD'), to_char(c.failed_on, 'YYYY-MM-DD'), c.source,
		       coalesce(a.type, ''), coalesce(a.name, ''), coalesce(a.currency, ''), coalesce(a.closed, false),
		       to_char(a.ended_on, 'YYYY-MM-DD')
		FROM agent.coverage c
		LEFT JOIN agent.accounts a ON a.slug = c.account_slug
		ORDER BY c.account_slug`, nil, limits, nil, nil,
		func(rows pgx.Rows, _ *pgtype.Map) (OverviewAccount, error) {
			var a OverviewAccount
			var gaps []byte
			if err := rows.Scan(&a.AccountSlug, &a.Feed, &a.EarliestDate, &a.LatestDate, &a.TransactionCount,
				&a.ReconciliationState, &a.ReconciledThrough, &a.LastDiffMinor, &gaps, &a.LatestStatementDate,
				&a.FailedOn, &a.Source, &a.Type, &a.Name, &a.Currency, &a.Closed, &a.EndedOn); err != nil {
				return a, err
			}
			a.Gaps = []AccountGap{}
			if gaps != nil {
				if err := json.Unmarshal(gaps, &a.Gaps); err != nil {
					return a, fmt.Errorf("decode gaps: %w", err)
				}
			}
			return a, nil
		})
	if err != nil {
		return Overview{}, fmt.Errorf("store: overview: %w", err)
	}
	if truncated {
		return Overview{}, errors.New("store: overview: more accounts than one read returns")
	}
	sources, _, err := read(ctx, r, `
		SELECT source, consent_state, approved_at, expected_expiry_at, days_to_expiry,
		       last_sync_ok_at, last_run_outcome, last_import_at
		FROM agent.sync_status ORDER BY source`, nil, limits, nil, nil,
		func(rows pgx.Rows, _ *pgtype.Map) (SyncStatusRow, error) {
			var s SyncStatusRow
			var imports []byte
			if err := rows.Scan(&s.Source, &s.ConsentState, &s.ApprovedAt, &s.ExpectedExpiryAt, &s.DaysToExpiry,
				&s.LastSyncOKAt, &s.LastRunOutcome, &imports); err != nil {
				return s, err
			}
			if imports != nil {
				if err := json.Unmarshal(imports, &s.LastImportAt); err != nil {
					return s, fmt.Errorf("decode last_import_at: %w", err)
				}
			}
			// UTC, as Query encodes instants: pgx decodes into the process's zone.
			s.ApprovedAt, s.ExpectedExpiryAt, s.LastSyncOKAt = utc(s.ApprovedAt), utc(s.ExpectedExpiryAt), utc(s.LastSyncOKAt)
			for k, v := range s.LastImportAt {
				s.LastImportAt[k] = utc(v)
			}
			return s, nil
		})
	if err != nil {
		return Overview{}, fmt.Errorf("store: overview: %w", err)
	}
	if accounts == nil {
		accounts = []OverviewAccount{}
	}
	if sources == nil {
		sources = []SyncStatusRow{}
	}
	return Overview{Accounts: accounts, Sources: sources}, nil
}

// Directions of money for SearchParams and SummaryParams.
const (
	DirectionIn  = "in"
	DirectionOut = "out"
)

// Values of SummaryParams.GroupBy.
const (
	GroupMonth        = "month"
	GroupCategory     = "category"
	GroupMerchant     = "merchant"
	GroupCounterparty = "counterparty"
	GroupAccount      = "account"
)

// NoneKey is the key of a SpendingSummary group whose grouping column is NULL.
const NoneKey = "(none)"

// SearchParams are the filters of SearchTransactions. Every field is optional. From and To are
// inclusive YYYY-MM-DD dates on txn_date. Text matches case-insensitively, as a substring, against
// description, counterparty, merchant_name and notes; its % and _ match themselves. Direction is
// "in" (amount_minor above zero), "out" (below zero) or empty for both. MinAmount and MaxAmount are
// signed minor units as stored, inclusive. Limit is 0 for CuratedDefaultLimit, else 1 to
// CuratedMaxLimit.
type SearchParams struct {
	Accounts      []string `json:"accounts,omitempty"`
	From          string   `json:"from,omitempty"`
	To            string   `json:"to,omitempty"`
	Text          string   `json:"text,omitempty"`
	Category      string   `json:"category,omitempty"`
	Direction     string   `json:"direction,omitempty"`
	MinAmount     *int64   `json:"min_amount,omitempty"`
	MaxAmount     *int64   `json:"max_amount,omitempty"`
	IncludeDetail bool     `json:"include_detail,omitempty"`
	Limit         int      `json:"limit,omitempty"`
}

// TransactionRow is a transaction as SearchTransactions returns it, from agent.transactions.
// TxnDate is YYYY-MM-DD. Detail is the latest version's whole source record, present only when
// SearchParams.IncludeDetail was set.
type TransactionRow struct {
	ID           string     `json:"id"`
	AccountSlug  string     `json:"account_slug"`
	TxnDate      string     `json:"txn_date"`
	CreatedAt    *time.Time `json:"created_at"`
	AmountMinor  int64      `json:"amount_minor"`
	Currency     string     `json:"currency"`
	Status       string     `json:"status"`
	Description  string     `json:"description"`
	Counterparty *string    `json:"counterparty"`
	MerchantName *string    `json:"merchant_name"`
	Category     *string    `json:"category"`
	Notes        *string    `json:"notes"`
	PotSlug      *string    `json:"pot_slug"`
	Detail       any        `json:"detail,omitempty"`
}

// likeEscaper escapes the characters ILIKE treats as pattern syntax.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// validSlug accepts what an account slug can hold: Monzo account types pass through unchanged into
// a slug, so it can have an underscore, and an operator chooses the slug of a statement account.
// Queries are parameterised, so this is input hygiene: letters, digits, hyphen, underscore, dot.
func validSlug(s string) bool {
	if s == "" || len(s) > 100 {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

// parseDay parses an optional YYYY-MM-DD filter; empty is nil.
func parseDay(name, v string) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.DateOnly, v)
	if err != nil {
		return nil, fmt.Errorf("%w: %s must be a YYYY-MM-DD date", ErrInvalid, name)
	}
	return &t, nil
}

// dayRange parses from and to and refuses from after to.
func dayRange(from, to string) (f, t *time.Time, err error) {
	if f, err = parseDay("from", from); err != nil {
		return nil, nil, err
	}
	if t, err = parseDay("to", to); err != nil {
		return nil, nil, err
	}
	if f != nil && t != nil && f.After(*t) {
		return nil, nil, fmt.Errorf("%w: from is after to", ErrInvalid)
	}
	return f, t, nil
}

// slugFilter validates account slugs; an empty list is no filter (nil).
func slugFilter(accounts []string) ([]string, error) {
	if len(accounts) == 0 {
		return nil, nil
	}
	for _, a := range accounts {
		if !validSlug(a) {
			return nil, fmt.Errorf("%w: an account must be a slug of letters, digits, hyphens, underscores and dots", ErrInvalid)
		}
	}
	return accounts, nil
}

func curatedLimit(n int) (Limits, error) {
	switch {
	case n == 0:
		n = CuratedDefaultLimit
	case n < 0 || n > CuratedMaxLimit:
		return Limits{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalid, CuratedMaxLimit)
	}
	return Limits{MaxRows: n, MaxBytes: CuratedMaxBytes}, nil
}

// utc returns t in UTC, nil for nil.
func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// decodeAny decodes JSON keeping numbers exact.
func decodeAny(b []byte) (any, error) {
	if b == nil {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// SearchTransactions reads agent.transactions with the filters of p, newest txn_date first, then
// newest created_at. The bool is true when more rows matched than were returned (the limit or the
// byte cap was reached).
func (r *Reader) SearchTransactions(ctx context.Context, p SearchParams) ([]TransactionRow, bool, error) {
	from, to, err := dayRange(p.From, p.To)
	if err != nil {
		return nil, false, err
	}
	accounts, err := slugFilter(p.Accounts)
	if err != nil {
		return nil, false, err
	}
	switch p.Direction {
	case "", DirectionIn, DirectionOut:
	default:
		return nil, false, fmt.Errorf("%w: direction must be %q, %q or empty", ErrInvalid, DirectionIn, DirectionOut)
	}
	limits, err := curatedLimit(p.Limit)
	if err != nil {
		return nil, false, err
	}
	var pattern, category, direction *string
	if p.Text != "" {
		s := "%" + likeEscaper.Replace(p.Text) + "%"
		pattern = &s
	}
	if p.Category != "" {
		category = &p.Category
	}
	if p.Direction != "" {
		direction = &p.Direction
	}
	rows, truncated, err := read(ctx, r, `
		SELECT id::text, account_slug, to_char(txn_date, 'YYYY-MM-DD'), created_at, amount_minor, currency,
		       status, description, counterparty, merchant_name, category, notes, pot_slug,
		       CASE WHEN $9::boolean THEN detail END
		FROM agent.transactions
		WHERE ($1::text[] IS NULL OR account_slug = ANY($1))
		  AND ($2::date IS NULL OR txn_date >= $2)
		  AND ($3::date IS NULL OR txn_date <= $3)
		  AND ($4::text IS NULL OR description ILIKE $4 ESCAPE '\' OR counterparty ILIKE $4 ESCAPE '\'
		       OR merchant_name ILIKE $4 ESCAPE '\' OR notes ILIKE $4 ESCAPE '\')
		  AND ($5::text IS NULL OR category = $5)
		  AND ($6::text IS NULL OR ($6 = 'in' AND amount_minor > 0) OR ($6 = 'out' AND amount_minor < 0))
		  AND ($7::bigint IS NULL OR amount_minor >= $7)
		  AND ($8::bigint IS NULL OR amount_minor <= $8)
		ORDER BY txn_date DESC, created_at DESC NULLS LAST, id`,
		[]any{accounts, from, to, pattern, category, direction, p.MinAmount, p.MaxAmount, p.IncludeDetail},
		limits, nil, nil,
		func(rows pgx.Rows, _ *pgtype.Map) (TransactionRow, error) {
			var t TransactionRow
			var detail []byte
			if err := rows.Scan(&t.ID, &t.AccountSlug, &t.TxnDate, &t.CreatedAt, &t.AmountMinor, &t.Currency,
				&t.Status, &t.Description, &t.Counterparty, &t.MerchantName, &t.Category, &t.Notes, &t.PotSlug,
				&detail); err != nil {
				return t, err
			}
			t.CreatedAt = utc(t.CreatedAt)
			var err error
			t.Detail, err = decodeAny(detail)
			return t, err
		})
	if err != nil {
		return nil, false, fmt.Errorf("store: search transactions: %w", err)
	}
	if rows == nil {
		rows = []TransactionRow{}
	}
	return rows, truncated, nil
}

// SummaryParams are the inputs of SpendingSummary. GroupBy is required: one of the Group
// constants. From and To are optional inclusive YYYY-MM-DD dates on txn_date. Direction is "out"
// (the default), which sums rows below zero and reports the total as a positive number, or "in",
// which sums rows above zero. ExcludePotTransfers leaves out rows whose pot_slug is set; nil means
// true. Limit is as for SearchParams.
type SummaryParams struct {
	GroupBy             string   `json:"group_by"`
	From                string   `json:"from,omitempty"`
	To                  string   `json:"to,omitempty"`
	Accounts            []string `json:"accounts,omitempty"`
	Direction           string   `json:"direction,omitempty"`
	ExcludePotTransfers *bool    `json:"exclude_pot_transfers,omitempty"`
	Limit               int      `json:"limit,omitempty"`
}

// SummaryGroup is one row of SpendingSummary: a key and a currency, never summed across
// currencies. Key is YYYY-MM for GroupMonth, otherwise the grouping column's value, NoneKey for
// NULL.
type SummaryGroup struct {
	Key        string `json:"key"`
	Currency   string `json:"currency"`
	TotalMinor int64  `json:"total_minor"`
	Count      int64  `json:"count"`
}

// SpendingSummary totals agent.transactions by GroupBy and currency. Declined transactions are
// excluded. Groups of GroupMonth come in key order; the others largest total first. The bool is true
// when more groups existed than were returned.
func (r *Reader) SpendingSummary(ctx context.Context, p SummaryParams) ([]SummaryGroup, bool, error) {
	switch p.GroupBy {
	case GroupMonth, GroupCategory, GroupMerchant, GroupCounterparty, GroupAccount:
	default:
		return nil, false, fmt.Errorf("%w: group_by must be one of %s, %s, %s, %s, %s", ErrInvalid,
			GroupMonth, GroupCategory, GroupMerchant, GroupCounterparty, GroupAccount)
	}
	from, to, err := dayRange(p.From, p.To)
	if err != nil {
		return nil, false, err
	}
	accounts, err := slugFilter(p.Accounts)
	if err != nil {
		return nil, false, err
	}
	direction := p.Direction
	switch direction {
	case "":
		direction = DirectionOut
	case DirectionIn, DirectionOut:
	default:
		return nil, false, fmt.Errorf("%w: direction must be %q or %q", ErrInvalid, DirectionIn, DirectionOut)
	}
	exclude := p.ExcludePotTransfers == nil || *p.ExcludePotTransfers
	limits, err := curatedLimit(p.Limit)
	if err != nil {
		return nil, false, err
	}
	rows, truncated, err := read(ctx, r, `
		SELECT key, currency, sum(abs(amount_minor))::bigint AS total_minor, count(*)::bigint AS n
		FROM (
		  SELECT t.currency, t.amount_minor,
		         CASE $1::text
		           WHEN 'month' THEN to_char(t.txn_date, 'YYYY-MM')
		           WHEN 'category' THEN coalesce(t.category, '(none)')
		           WHEN 'merchant' THEN coalesce(t.merchant_name, '(none)')
		           WHEN 'counterparty' THEN coalesce(t.counterparty, '(none)')
		           WHEN 'account' THEN t.account_slug
		         END AS key
		  FROM agent.transactions t
		  WHERE t.status <> 'declined'
		    AND (($2::text = 'out' AND t.amount_minor < 0) OR ($2::text = 'in' AND t.amount_minor > 0))
		    AND ($3::date IS NULL OR t.txn_date >= $3)
		    AND ($4::date IS NULL OR t.txn_date <= $4)
		    AND ($5::text[] IS NULL OR t.account_slug = ANY($5))
		    AND (NOT $6::boolean OR t.pot_slug IS NULL)
		) s
		GROUP BY key, currency
		ORDER BY CASE WHEN $1::text = 'month' THEN key END, total_minor DESC, key, currency`,
		[]any{p.GroupBy, direction, from, to, accounts, exclude}, limits, nil, nil,
		func(rows pgx.Rows, _ *pgtype.Map) (SummaryGroup, error) {
			var g SummaryGroup
			return g, rows.Scan(&g.Key, &g.Currency, &g.TotalMinor, &g.Count)
		})
	if err != nil {
		return nil, false, fmt.Errorf("store: spending summary: %w", err)
	}
	if rows == nil {
		rows = []SummaryGroup{}
	}
	return rows, truncated, nil
}

// Kinds of Relation.
const (
	KindTable            = "table"
	KindView             = "view"
	KindMaterializedView = "materialized view"
)

// RelationColumn is a column of a Relation that the reader may read. Type is Postgres's name for the
// type, as format_type gives it ("timestamp with time zone", "text[]", "numeric(10,2)").
type RelationColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
}

// Relation is a table or view the reader may look at. Comment is the relation's comment, empty when
// it has none.
type Relation struct {
	Schema  string           `json:"schema"`
	Name    string           `json:"name"`
	Kind    string           `json:"kind"`
	Comment string           `json:"comment"`
	Columns []RelationColumn `json:"columns"`
}

// readableSchemas are the schemas the reader can read and DescribeSchema lists.
var readableSchemas = []string{"agent", "core", "raw", "sync"}

// DescribeSchema lists the tables and views of schema (agent, core, raw or sync; empty for all
// four) with their kind, comment and columns, ordered by schema and name.
func (r *Reader) DescribeSchema(ctx context.Context, schema string) ([]Relation, error) {
	var filter *string
	if schema != "" {
		found := false
		for _, s := range readableSchemas {
			found = found || s == schema
		}
		if !found {
			return nil, fmt.Errorf("%w: schema must be one of %s or empty", ErrInvalid, strings.Join(readableSchemas, ", "))
		}
		filter = &schema
	}
	rows, truncated, err := read(ctx, r, `
		SELECT n.nspname::text, c.relname::text,
		       CASE c.relkind WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized view' ELSE 'table' END,
		       coalesce(obj_description(c.oid, 'pg_class'), ''),
		       coalesce((SELECT jsonb_agg(jsonb_build_object(
		                   'name', a.attname::text,
		                   'type', format_type(a.atttypid, a.atttypmod),
		                   'nullable', NOT a.attnotnull) ORDER BY a.attnum)
		                 FROM pg_attribute a
		                 WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		                   AND has_column_privilege(c.oid, a.attnum, 'SELECT')), '[]'::jsonb)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = ANY($2::text[]) AND ($1::text IS NULL OR n.nspname = $1)
		  AND c.relkind IN ('r', 'p', 'v', 'm')
		ORDER BY n.nspname, c.relname`,
		[]any{filter, readableSchemas}, Limits{MaxRows: CuratedMaxLimit, MaxBytes: CuratedMaxBytes}, nil, nil,
		func(rows pgx.Rows, _ *pgtype.Map) (Relation, error) {
			var rel Relation
			var cols []byte
			if err := rows.Scan(&rel.Schema, &rel.Name, &rel.Kind, &rel.Comment, &cols); err != nil {
				return rel, err
			}
			rel.Columns = []RelationColumn{}
			if err := json.Unmarshal(cols, &rel.Columns); err != nil {
				return rel, fmt.Errorf("decode columns: %w", err)
			}
			return rel, nil
		})
	if err != nil {
		return nil, fmt.Errorf("store: describe schema: %w", err)
	}
	if truncated {
		return nil, errors.New("store: describe schema: more relations than one read returns")
	}
	if rows == nil {
		rows = []Relation{}
	}
	return rows, nil
}

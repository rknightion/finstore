package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
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
	GroupPayee        = "payee"
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
// which sums rows above zero. FlowClasses are the classes of agent.cash_flows to count; empty means
// spending for "out" and income and refund for "in". ExcludePotTransfers matters only when
// FlowClasses is empty: false adds the pot transfers (class internal, reason pot); nil means true.
// IncludePending counts pending rows as well as settled ones. Limit is as for SearchParams.
type SummaryParams struct {
	GroupBy             string   `json:"group_by"`
	From                string   `json:"from,omitempty"`
	To                  string   `json:"to,omitempty"`
	Accounts            []string `json:"accounts,omitempty"`
	Direction           string   `json:"direction,omitempty"`
	ExcludePotTransfers *bool    `json:"exclude_pot_transfers,omitempty"`
	FlowClasses         []string `json:"flow_classes,omitempty"`
	IncludePending      bool     `json:"include_pending,omitempty"`
	Limit               int      `json:"limit,omitempty"`
}

// SummaryGroup is one row of SpendingSummary: a key and a currency, never summed across
// currencies. Key is YYYY-MM for GroupMonth, otherwise the grouping column's value, NoneKey for
// NULL. For GroupPayee it is the payee's display name, and GroupCategory reads the effective
// category (the operator's setting first).
type SummaryGroup struct {
	Key        string `json:"key"`
	Currency   string `json:"currency"`
	TotalMinor int64  `json:"total_minor"`
	Count      int64  `json:"count"`
}

// SpendingSummary totals agent.cash_flows by GroupBy and currency. Declined transactions are not
// in the view, and pending ones count only with IncludePending. The classes counted are FlowClasses,
// or by default spending for money out and income and refund for money in. Groups of GroupMonth
// come in key order; the others largest total first. The bool is true when more groups existed than
// were returned.
func (r *Reader) SpendingSummary(ctx context.Context, p SummaryParams) ([]SummaryGroup, bool, error) {
	switch p.GroupBy {
	case GroupMonth, GroupCategory, GroupMerchant, GroupCounterparty, GroupAccount, GroupPayee:
	default:
		return nil, false, fmt.Errorf("%w: group_by must be one of %s, %s, %s, %s, %s, %s", ErrInvalid,
			GroupMonth, GroupCategory, GroupMerchant, GroupCounterparty, GroupAccount, GroupPayee)
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
	for _, c := range p.FlowClasses {
		if !slices.Contains(FlowClasses, c) {
			return nil, false, fmt.Errorf("%w: flow_classes must be among %s", ErrInvalid, strings.Join(FlowClasses, ", "))
		}
	}
	classes, potTransfers := p.FlowClasses, false
	if len(classes) == 0 {
		classes = []string{"spending"}
		if direction == DirectionIn {
			classes = []string{"income", "refund"}
		}
		potTransfers = p.ExcludePotTransfers != nil && !*p.ExcludePotTransfers
	}
	limits, err := curatedLimit(p.Limit)
	if err != nil {
		return nil, false, err
	}
	rows, truncated, err := read(ctx, r, `
		SELECT key, currency, sum(abs(amount_minor))::bigint AS total_minor, count(*)::bigint AS n
		FROM (
		  SELECT c.currency, c.amount_minor,
		         CASE $1::text
		           WHEN 'month' THEN to_char(c.txn_date, 'YYYY-MM')
		           WHEN 'category' THEN coalesce(c.category, '(none)')
		           WHEN 'merchant' THEN coalesce(c.merchant_name, '(none)')
		           WHEN 'counterparty' THEN coalesce(c.counterparty, '(none)')
		           WHEN 'account' THEN c.account_slug
		           WHEN 'payee' THEN c.payee_name
		         END AS key
		  FROM agent.cash_flows c
		  WHERE (c.status = 'settled' OR $7::boolean)
		    AND (($2::text = 'out' AND c.amount_minor < 0) OR ($2::text = 'in' AND c.amount_minor > 0))
		    AND ($3::date IS NULL OR c.txn_date >= $3)
		    AND ($4::date IS NULL OR c.txn_date <= $4)
		    AND ($5::text[] IS NULL OR c.account_slug = ANY($5))
		    AND (c.flow_class = ANY($6::text[]) OR ($8::boolean AND c.flow_class = 'internal' AND c.flow_reason = 'pot'))
		) s
		GROUP BY key, currency
		ORDER BY CASE WHEN $1::text = 'month' THEN key END, total_minor DESC, key, currency`,
		[]any{p.GroupBy, direction, from, to, accounts, classes, p.IncludePending, potTransfers}, limits, nil, nil,
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

// PortfolioParams are the inputs of Portfolio. Accounts holds slugs of investment child accounts or
// of platforms, and a platform stands for its children; empty is every investment account. Limit
// caps each array of the result: 0 for CuratedDefaultLimit, else 1 to CuratedMaxLimit.
type PortfolioParams struct {
	Accounts []string `json:"accounts,omitempty"`
	Limit    int      `json:"limit,omitempty"`
}

// PortfolioHolding is one row of agent.holdings. Units and prices are exact decimal text. The
// reported fields are null for a position only the last check knows, and the three check fields
// are null when no check covers the instrument.
type PortfolioHolding struct {
	Instrument     string  `json:"instrument"`
	Ticker         *string `json:"ticker"`
	ISIN           *string `json:"isin"`
	Units          *string `json:"units"`
	Price          *string `json:"price"`
	PriceUnit      *string `json:"price_unit"`
	ValueMinor     *int64  `json:"value_minor"`
	Currency       string  `json:"currency"`
	BookCostMinor  *int64  `json:"book_cost_minor"`
	ExpectedUnits  *string `json:"expected_units"`
	UnitDifference *string `json:"unit_difference"`
	CheckedAsOf    *string `json:"checked_as_of"`
}

// PortfolioAccount is one investment child account at its newest valuation, with its holdings. A
// value of 0 with no holdings is a real account.
type PortfolioAccount struct {
	AccountSlug         string             `json:"account_slug"`
	Provider            string             `json:"provider"`
	AsOf                string             `json:"as_of"`
	TotalMinor          int64              `json:"total_minor"`
	CashMinor           int64              `json:"cash_minor"`
	Currency            string             `json:"currency"`
	ReconciliationState string             `json:"reconciliation_state"`
	Holdings            []PortfolioHolding `json:"holdings"`
}

// PensionSummaryParams are the inputs of PensionSummary. Accounts is as for PortfolioParams.
// FromTaxYear and ToTaxYear are optional tax years such as 2025-26, both included. Limit caps each
// array of the result, as for PortfolioParams.
type PensionSummaryParams struct {
	Accounts    []string `json:"accounts,omitempty"`
	FromTaxYear string   `json:"from_tax_year,omitempty"`
	ToTaxYear   string   `json:"to_tax_year,omitempty"`
	Limit       int      `json:"limit,omitempty"`
}

// TaxYearFlows is one account's cash flows and ledger fees in one tax year and currency.
// TransfersOutMinor is null for a statement-only account's year that winning statements do not
// fully cover, because a transfer out is known only from a statement.
type TaxYearFlows struct {
	AccountSlug           string `json:"account_slug"`
	TaxYear               string `json:"tax_year"`
	Currency              string `json:"currency"`
	PaidInMinor           int64  `json:"paid_in_minor"`
	TransfersInMinor      int64  `json:"transfers_in_minor"`
	TransfersOutMinor     *int64 `json:"transfers_out_minor"`
	WithdrawalsMinor      int64  `json:"withdrawals_minor"`
	AdjustmentsMinor      int64  `json:"adjustments_minor"`
	SiblingTransfersMinor int64  `json:"sibling_transfers_minor"`
	FeesMinor             int64  `json:"fees_minor"`
	InterestMinor         int64  `json:"interest_minor"`
}

// InvestmentReturn is one row of agent.investment_returns.
type InvestmentReturn struct {
	AccountSlug      string  `json:"account_slug"`
	IntervalKind     string  `json:"interval_kind"`
	OpeningDate      *string `json:"opening_date"`
	ClosingDate      *string `json:"closing_date"`
	OpeningMinor     *int64  `json:"opening_minor"`
	ClosingMinor     *int64  `json:"closing_minor"`
	ValueChangeMinor *int64  `json:"value_change_minor"`
	NetFlowMinor     *int64  `json:"net_flow_minor"`
	ReturnMinor      *int64  `json:"return_minor"`
	NullReason       *string `json:"null_reason"`
	TaxYear          *string `json:"tax_year"`
	Currency         string  `json:"currency"`
}

// StatementCharge is the charge one winning statement prints for its period. ChargesMinor null
// means unknown, never zero. It is never split across tax years.
type StatementCharge struct {
	AccountSlug  string `json:"account_slug"`
	PeriodStart  string `json:"period_start"`
	PeriodEnd    string `json:"period_end"`
	ChargesMinor *int64 `json:"charges_minor"`
	Currency     string `json:"currency"`
}

// MissingData says what could not be stated for an account, and for which tax year when the gap is
// one year's.
type MissingData struct {
	AccountSlug string  `json:"account_slug"`
	TaxYear     *string `json:"tax_year"`
	Reason      string  `json:"reason"`
}

// PensionSummary is what the pension_summary tool returns.
type PensionSummary struct {
	TaxYears         []TaxYearFlows     `json:"tax_years"`
	Returns          []InvestmentReturn `json:"returns"`
	StatementCharges []StatementCharge  `json:"statement_charges"`
	Missing          []MissingData      `json:"missing"`
}

// parseTaxYear reads a tax year such as 2025-26 and returns its first calendar year; empty is nil.
func parseTaxYear(name, v string) (*int, error) {
	if v == "" {
		return nil, nil
	}
	bad := fmt.Errorf("%w: %s must be a tax year such as 2025-26", ErrInvalid, name)
	if len(v) != 7 || v[4] != '-' {
		return nil, bad
	}
	for i, c := range v {
		if i != 4 && (c < '0' || c > '9') {
			return nil, bad
		}
	}
	start, err := strconv.Atoi(v[:4])
	if err != nil {
		return nil, bad
	}
	end, err := strconv.Atoi(v[5:])
	if err != nil || start < 1900 || (start+1)%100 != end {
		return nil, bad
	}
	return &start, nil
}

// CheckTaxYears validates an optional from and to tax year and refuses from after to. Its error
// wraps ErrInvalid.
func CheckTaxYears(from, to string) error {
	_, _, err := taxYearRange(from, to)
	return err
}

func taxYearRange(from, to string) (f, t *int, err error) {
	if f, err = parseTaxYear("from_tax_year", from); err != nil {
		return nil, nil, err
	}
	if t, err = parseTaxYear("to_tax_year", to); err != nil {
		return nil, nil, err
	}
	if f != nil && t != nil && *f > *t {
		return nil, nil, fmt.Errorf("%w: from_tax_year is after to_tax_year", ErrInvalid)
	}
	return f, t, nil
}

// investmentRow is one row of a curated investment statement: which array it belongs to, whether
// that array had more rows than the limit, and the element as JSON. Its encoded size is what counts
// against the shared byte budget.
type investmentRow struct {
	Kind    string          `json:"k"`
	More    bool            `json:"m"`
	Payload json.RawMessage `json:"p"`
}

func decodeInvestmentRow(rows pgx.Rows, _ *pgtype.Map) (investmentRow, error) {
	var row investmentRow
	var payload []byte
	if err := rows.Scan(&row.Kind, &row.More, &payload); err != nil {
		return row, err
	}
	row.Payload = append(json.RawMessage(nil), payload...)
	return row, nil
}

// investmentTargetsSQL selects the investment child accounts the slugs in $1 name, a platform's slug
// naming all of its children. A null $1 selects every child.
const investmentTargetsSQL = `
	SELECT a.slug, substr(a.source, length('investment:') + 1) AS provider, r.method
	FROM core.account a
	JOIN core.account platform ON platform.id = a.parent_id
	JOIN core.reconciliation r ON r.account_id = a.id
	WHERE a.source LIKE 'investment:%'
	  AND ($1::text[] IS NULL OR a.slug = ANY($1) OR platform.slug = ANY($1))`

// portfolioSQL is one statement: a row per account, each followed by its holdings, largest value
// first. $2 caps the accounts and each account's holdings; a row's more says its array was cut.
const portfolioSQL = `
	WITH target AS (` + investmentTargetsSQL + `
	),
	acc AS (
	  SELECT t.slug, t.provider, v.as_of, v.total_minor, v.cash_minor, v.currency,
	         coalesce(c.reconciliation_state, 'not_checked') AS reconciliation_state,
	         row_number() OVER (ORDER BY t.slug) AS rn, count(*) OVER () AS n
	  FROM target t
	  JOIN LATERAL (SELECT w.as_of, w.total_minor, w.cash_minor, w.currency
	                FROM agent.valuations w WHERE w.account_slug = t.slug
	                ORDER BY w.as_of DESC LIMIT 1) v ON true
	  LEFT JOIN agent.coverage c ON c.account_slug = t.slug
	),
	held AS (
	  SELECT h.*, a.rn AS account_rn,
	         row_number() OVER (PARTITION BY h.account_slug
	                            ORDER BY coalesce(h.value_minor, -1) DESC, h.instrument_name, h.ticker) AS rn,
	         count(*) OVER (PARTITION BY h.account_slug) AS n
	  FROM agent.holdings h
	  JOIN acc a ON a.slug = h.account_slug AND a.rn <= $2::int
	)
	SELECT u.kind, u.more, u.payload FROM (
	  SELECT 'account'::text AS kind, a.rn AS account_ord, 0::bigint AS item_ord, a.n > $2::int AS more,
	         jsonb_build_object('account_slug', a.slug, 'provider', a.provider, 'as_of', a.as_of,
	           'total_minor', a.total_minor, 'cash_minor', a.cash_minor, 'currency', a.currency,
	           'reconciliation_state', a.reconciliation_state) AS payload
	  FROM acc a WHERE a.rn <= $2::int
	  UNION ALL
	  SELECT 'holding', h.account_rn, h.rn, h.n > $2::int,
	         jsonb_build_object('account_slug', h.account_slug, 'instrument', h.instrument_name,
	           'ticker', h.ticker, 'isin', h.isin, 'units', h.units::text, 'price', h.price::text,
	           'price_unit', h.price_unit, 'value_minor', h.value_minor, 'currency', h.currency,
	           'book_cost_minor', h.book_cost_minor, 'expected_units', h.expected_units::text,
	           'unit_difference', h.unit_difference::text, 'checked_as_of', h.checked_as_of)
	  FROM held h WHERE h.rn <= $2::int
	) u
	ORDER BY u.account_ord, u.item_ord`

// Portfolio reads every selected investment account's newest valuation and holdings in one
// statement. An account with no valuation has no data and is not listed. Limit caps the accounts
// and each account's holdings, the rows share CuratedMaxBytes, and the bool is true when anything
// was cut.
func (r *Reader) Portfolio(ctx context.Context, p PortfolioParams) ([]PortfolioAccount, bool, error) {
	accounts, err := slugFilter(p.Accounts)
	if err != nil {
		return nil, false, err
	}
	limits, err := curatedLimit(p.Limit)
	if err != nil {
		return nil, false, err
	}
	rows, truncated, err := read(ctx, r, portfolioSQL, []any{accounts, limits.MaxRows},
		Limits{MaxRows: limits.MaxRows * (limits.MaxRows + 1), MaxBytes: limits.MaxBytes}, nil, nil, decodeInvestmentRow)
	if err != nil {
		return nil, false, fmt.Errorf("store: portfolio: %w", err)
	}
	out := []PortfolioAccount{}
	index := map[string]int{}
	for _, row := range rows {
		truncated = truncated || row.More
		switch row.Kind {
		case "account":
			var a PortfolioAccount
			if err := json.Unmarshal(row.Payload, &a); err != nil {
				return nil, false, fmt.Errorf("store: portfolio: decode account: %w", err)
			}
			a.Holdings = []PortfolioHolding{}
			index[a.AccountSlug] = len(out)
			out = append(out, a)
		case "holding":
			var h struct {
				AccountSlug string `json:"account_slug"`
				PortfolioHolding
			}
			if err := json.Unmarshal(row.Payload, &h); err != nil {
				return nil, false, fmt.Errorf("store: portfolio: decode holding: %w", err)
			}
			if i, ok := index[h.AccountSlug]; ok {
				out[i].Holdings = append(out[i].Holdings, h.PortfolioHolding)
			}
		}
	}
	return out, truncated, nil
}

// taxYearOf is the SQL for the first calendar year of the tax year holding the date expression d.
// d is always a column name written in this file.
func taxYearOf(d string) string {
	return `(extract(year FROM ` + d + `)::int - CASE WHEN ` + d + ` < make_date(extract(year FROM ` + d + `)::int, 4, 6) THEN 1 ELSE 0 END)`
}

// taxYearLabel is the SQL for the label (2025-26) of the tax year starting in the int expression y.
func taxYearLabel(y string) string {
	return `(` + y + `::text || '-' || lpad(((` + y + ` + 1) % 100)::text, 2, '0'))`
}

// pensionSummarySQL is one statement over agent.investment_events, agent.pension_statements and
// agent.investment_returns. $2 and $3 bound the tax years by their first calendar year, $4 and $5
// are the first and last day of that range, and $6 caps each array. Rows are ordered by position
// within their array, so a byte budget that runs out cuts all four arrays evenly.
var pensionSummarySQL = `
	WITH target AS (` + investmentTargetsSQL + `
	),
	ev AS (
	  SELECT e.account_slug, e.event_type, e.flow_minor, e.cash_minor, e.currency,
	         ` + taxYearOf("e.effective_date") + ` AS ty
	  FROM agent.investment_events e
	  JOIN target t ON t.slug = e.account_slug
	),
	stmt AS (
	  SELECT s.account_slug, s.period_start, s.period_end, s.charges_minor, s.currency,
	         ` + taxYearOf("s.period_start") + ` AS first_ty,
	         ` + taxYearOf("s.period_end") + ` AS last_ty
	  FROM agent.pension_statements s
	  JOIN target t ON t.slug = s.account_slug
	),
	covered AS (
	  SELECT s.account_slug, range_agg(daterange(s.period_start, s.period_end, '[]')) AS days
	  FROM stmt s
	  GROUP BY s.account_slug
	),
	years AS (
	  SELECT e.account_slug, e.ty, e.currency FROM ev e
	  UNION
	  SELECT s.account_slug, g.ty, s.currency
	  FROM stmt s
	  CROSS JOIN LATERAL generate_series(s.first_ty, s.last_ty) AS g(ty)
	),
	by_year AS (
	  SELECT y.account_slug, y.ty, y.currency, t.method,
	         coalesce(sum(e.flow_minor) FILTER (WHERE e.event_type = 'contribution'), 0)::bigint AS paid_in,
	         coalesce(sum(e.flow_minor) FILTER (WHERE e.event_type = 'transfer_in'), 0)::bigint AS transfers_in,
	         coalesce(sum(e.flow_minor) FILTER (WHERE e.event_type = 'transfer_out'), 0)::bigint AS transfers_out,
	         coalesce(sum(e.flow_minor) FILTER (WHERE e.event_type = 'withdrawal'), 0)::bigint AS withdrawals,
	         coalesce(sum(e.flow_minor) FILTER (WHERE e.event_type = 'adjustment'), 0)::bigint AS adjustments,
	         coalesce(sum(e.flow_minor) FILTER (WHERE e.event_type = 'sibling_transfer'), 0)::bigint AS sibling_transfers,
	         coalesce(sum(e.cash_minor) FILTER (WHERE e.event_type = 'fee'), 0)::bigint AS fees,
	         coalesce(sum(e.cash_minor) FILTER (WHERE e.event_type = 'interest'), 0)::bigint AS interest
	  FROM years y
	  JOIN target t ON t.slug = y.account_slug
	  LEFT JOIN ev e ON e.account_slug = y.account_slug AND e.ty = y.ty AND e.currency = y.currency
	  WHERE ($2::int IS NULL OR y.ty >= $2) AND ($3::int IS NULL OR y.ty <= $3)
	  GROUP BY y.account_slug, y.ty, y.currency, t.method
	),
	tax_rows AS (
	  SELECT b.*,
	         coalesce(c.days @> daterange(make_date(b.ty, 4, 6), make_date(b.ty + 1, 4, 5), '[]'), false) AS fully_covered,
	         row_number() OVER (ORDER BY b.account_slug, b.ty, b.currency) AS rn,
	         count(*) OVER () AS n
	  FROM by_year b
	  LEFT JOIN covered c ON c.account_slug = b.account_slug
	),
	ret AS (
	  SELECT r.*,
	         row_number() OVER (ORDER BY r.account_slug, r.closing_date NULLS LAST, r.opening_date NULLS FIRST, r.interval_kind) AS rn,
	         count(*) OVER () AS n
	  FROM agent.investment_returns r
	  JOIN target t ON t.slug = r.account_slug
	  WHERE ($4::date IS NULL OR r.closing_date IS NULL OR r.closing_date >= $4)
	    AND ($5::date IS NULL OR r.opening_date IS NULL OR r.opening_date < $5)
	),
	charges AS (
	  SELECT s.account_slug, s.period_start, s.period_end, s.charges_minor, s.currency,
	         row_number() OVER (ORDER BY s.account_slug, s.period_start) AS rn,
	         count(*) OVER () AS n
	  FROM stmt s
	  WHERE ($4::date IS NULL OR s.period_end >= $4) AND ($5::date IS NULL OR s.period_start <= $5)
	),
	miss AS (
	  SELECT d.account_slug, d.tax_year, d.reason,
	         row_number() OVER (ORDER BY d.account_slug, d.tax_year NULLS FIRST, d.reason) AS rn,
	         count(*) OVER () AS n
	  FROM (
	    SELECT r.account_slug, r.tax_year, r.null_reason AS reason FROM ret r WHERE r.null_reason IS NOT NULL
	    UNION
	    SELECT b.account_slug, ` + taxYearLabel("b.ty") + `, 'flows_unverified'
	    FROM tax_rows b
	    WHERE b.method = 'statement_flows' AND NOT b.fully_covered
	  ) d
	)
	SELECT u.kind, u.more, u.payload FROM (
	  SELECT 'tax_year'::text AS kind, b.rn, b.n > $6::int AS more,
	         jsonb_build_object('account_slug', b.account_slug, 'tax_year', ` + taxYearLabel("b.ty") + `,
	           'currency', b.currency, 'paid_in_minor', b.paid_in, 'transfers_in_minor', b.transfers_in,
	           'transfers_out_minor', CASE WHEN b.method = 'statement_flows' AND NOT b.fully_covered THEN NULL ELSE b.transfers_out END,
	           'withdrawals_minor', b.withdrawals, 'adjustments_minor', b.adjustments,
	           'sibling_transfers_minor', b.sibling_transfers, 'fees_minor', b.fees, 'interest_minor', b.interest) AS payload
	  FROM tax_rows b WHERE b.rn <= $6::int
	  UNION ALL
	  SELECT 'return', r.rn, r.n > $6::int,
	         jsonb_build_object('account_slug', r.account_slug, 'interval_kind', r.interval_kind,
	           'opening_date', r.opening_date, 'closing_date', r.closing_date, 'opening_minor', r.opening_minor,
	           'closing_minor', r.closing_minor, 'value_change_minor', r.value_change_minor,
	           'net_flow_minor', r.net_flow_minor, 'return_minor', r.return_minor, 'null_reason', r.null_reason,
	           'tax_year', r.tax_year, 'currency', r.currency)
	  FROM ret r WHERE r.rn <= $6::int
	  UNION ALL
	  SELECT 'statement_charge', c.rn, c.n > $6::int,
	         jsonb_build_object('account_slug', c.account_slug, 'period_start', c.period_start,
	           'period_end', c.period_end, 'charges_minor', c.charges_minor, 'currency', c.currency)
	  FROM charges c WHERE c.rn <= $6::int
	  UNION ALL
	  SELECT 'missing', m.rn, m.n > $6::int,
	         jsonb_build_object('account_slug', m.account_slug, 'tax_year', m.tax_year, 'reason', m.reason)
	  FROM miss m WHERE m.rn <= $6::int
	) u
	ORDER BY u.rn, u.kind`

// PensionSummary reads cash flows by tax year, returns, statement charges and what is missing for
// the selected investment accounts, in one statement. Limit caps each of the four arrays, the rows
// share CuratedMaxBytes, and the bool is true when anything was cut.
func (r *Reader) PensionSummary(ctx context.Context, p PensionSummaryParams) (PensionSummary, bool, error) {
	out := PensionSummary{TaxYears: []TaxYearFlows{}, Returns: []InvestmentReturn{},
		StatementCharges: []StatementCharge{}, Missing: []MissingData{}}
	accounts, err := slugFilter(p.Accounts)
	if err != nil {
		return out, false, err
	}
	fromYear, toYear, err := taxYearRange(p.FromTaxYear, p.ToTaxYear)
	if err != nil {
		return out, false, err
	}
	limits, err := curatedLimit(p.Limit)
	if err != nil {
		return out, false, err
	}
	var firstDay, lastDay *time.Time
	if fromYear != nil {
		d := time.Date(*fromYear, time.April, 6, 0, 0, 0, 0, time.UTC)
		firstDay = &d
	}
	if toYear != nil {
		d := time.Date(*toYear+1, time.April, 5, 0, 0, 0, 0, time.UTC)
		lastDay = &d
	}
	rows, truncated, err := read(ctx, r, pensionSummarySQL, []any{accounts, fromYear, toYear, firstDay, lastDay, limits.MaxRows},
		Limits{MaxRows: 4 * limits.MaxRows, MaxBytes: limits.MaxBytes}, nil, nil, decodeInvestmentRow)
	if err != nil {
		return out, false, fmt.Errorf("store: pension summary: %w", err)
	}
	for _, row := range rows {
		truncated = truncated || row.More
		var derr error
		switch row.Kind {
		case "tax_year":
			var v TaxYearFlows
			derr = json.Unmarshal(row.Payload, &v)
			out.TaxYears = append(out.TaxYears, v)
		case "return":
			var v InvestmentReturn
			derr = json.Unmarshal(row.Payload, &v)
			out.Returns = append(out.Returns, v)
		case "statement_charge":
			var v StatementCharge
			derr = json.Unmarshal(row.Payload, &v)
			out.StatementCharges = append(out.StatementCharges, v)
		case "missing":
			var v MissingData
			derr = json.Unmarshal(row.Payload, &v)
			out.Missing = append(out.Missing, v)
		}
		if derr != nil {
			return PensionSummary{}, false, fmt.Errorf("store: pension summary: decode row: %w", derr)
		}
	}
	return out, truncated, nil
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

// --- the insight tools -----------------------------------------------------------------------

// Flow classes of agent.cash_flows, and the account scopes of agent.account_scopes. The views own
// the rules that give a row its class or an account its scope; the reader only checks the names a
// caller passes.
var (
	FlowClasses   = []string{"internal", "scope_transfer", "own_external", "investment", "debt_repayment", "income", "refund", "spending"}
	AccountScopes = []string{"personal", "joint", "business"}
	// SeriesStatuses are the statuses of agent.recurring_payments.
	SeriesStatuses = []string{"active", "due", "overdue", "lapsed", "ended"}
)

// Sorts of RecurringPayments and Payees.
const (
	SortAnnualCost = "annual_cost"
	SortNextDate   = "next_date"
	SortChange12m  = "change_12m"
	SortOut        = "out"
	SortIn         = "in"
	SortLastDate   = "last_date"
)

// PriceChange is one entry of a series' price_changes.
type PriceChange struct {
	Date      string `json:"date"`
	FromMinor int64  `json:"from_minor"`
	ToMinor   int64  `json:"to_minor"`
}

// RecurringSeries is a row of agent.recurring_payments, with its column names. Dates are YYYY-MM-DD.
type RecurringSeries struct {
	SeriesKey          string        `json:"series_key"`
	PayeeID            string        `json:"payee_id"`
	PayeeName          string        `json:"payee_name"`
	Evidence           string        `json:"evidence"`
	Cadence            string        `json:"cadence"`
	AmountKind         string        `json:"amount_kind"`
	Occurrences        int64         `json:"occurrences"`
	FirstDate          string        `json:"first_date"`
	LastDate           string        `json:"last_date"`
	LastAmountMinor    int64         `json:"last_amount_minor"`
	TypicalAmountMinor int64         `json:"typical_amount_minor"`
	Currency           string        `json:"currency"`
	AnnualCostMinor    *int64        `json:"annual_cost_minor"`
	NextExpectedDate   *string       `json:"next_expected_date"`
	Status             string        `json:"status"`
	PriceChanges       []PriceChange `json:"price_changes"`
	Change12mMinor     *int64        `json:"change_12m_minor"`
	FlowClass          string        `json:"flow_class"`
	Category           *string       `json:"category"`
	CategoryName       *string       `json:"category_name"`
	AccountSlugs       []string      `json:"account_slugs"`
	OperatorState      *string       `json:"operator_state"`
	Confidence         string        `json:"confidence"`
}

// AnnualCostTotal is the annual cost of every series that matched, in one currency.
type AnnualCostTotal struct {
	Currency   string `json:"currency"`
	TotalMinor int64  `json:"total_minor"`
}

// RecurringParams are the inputs of RecurringPayments. Statuses empty means active, due and
// overdue. Sort is empty for SortAnnualCost. Limit is as for SearchParams.
type RecurringParams struct {
	Statuses       []string
	IncludeIgnored bool
	ChangedOnly    bool
	Query          string
	Sort           string
	Limit          int
}

// Recurring is what RecurringPayments returns: the series rows, cut at the limit, and the annual
// cost of every matching series per currency, counted before the cut.
type Recurring struct {
	Series          []RecurringSeries `json:"series"`
	AnnualCostTotal []AnnualCostTotal `json:"annual_cost_total"`
}

// PayeeYear is one calendar year of a payee's totals (agent.payee_years).
type PayeeYear struct {
	Year     int64 `json:"year"`
	OutMinor int64 `json:"out_minor"`
	InMinor  int64 `json:"in_minor"`
	TxnCount int64 `json:"txn_count"`
}

// PayeeRow is one payee and currency of agent.payees, with its totals over the range asked for.
type PayeeRow struct {
	PayeeID      string      `json:"payee_id"`
	PayeeName    string      `json:"payee_name"`
	Currency     string      `json:"currency"`
	FlowClass    *string     `json:"flow_class"`
	Category     *string     `json:"category"`
	CategoryName *string     `json:"category_name"`
	Employer     *bool       `json:"employer"`
	FirstDate    string      `json:"first_date"`
	LastDate     string      `json:"last_date"`
	TxnCount     int64       `json:"txn_count"`
	OutMinor     int64       `json:"out_minor"`
	InMinor      int64       `json:"in_minor"`
	Keys         []string    `json:"keys"`
	NameVariants []string    `json:"name_variants"`
	Years        []PayeeYear `json:"years"`
}

// PayeesParams are the inputs of Payees. From and To are YYYY-MM-DD dates, empty for none. Sort is
// empty for SortOut.
type PayeesParams struct {
	Query     string
	FlowClass string
	From, To  string
	Sort      string
	Limit     int
}

// MonthFlow is a month, scope and currency of agent.monthly_flows, settled rows (or settled and
// pending together), without the status column.
type MonthFlow struct {
	Month                 string   `json:"month"`
	Scope                 string   `json:"scope"`
	Currency              string   `json:"currency"`
	IncomeMinor           int64    `json:"income_minor"`
	SalaryMinor           int64    `json:"salary_minor"`
	RefundMinor           int64    `json:"refund_minor"`
	SpendingMinor         int64    `json:"spending_minor"`
	DebtRepaymentMinor    int64    `json:"debt_repayment_minor"`
	InvestmentOutMinor    int64    `json:"investment_out_minor"`
	InvestmentInMinor     int64    `json:"investment_in_minor"`
	OwnExternalOutMinor   int64    `json:"own_external_out_minor"`
	OwnExternalInMinor    int64    `json:"own_external_in_minor"`
	ScopeTransferOutMinor int64    `json:"scope_transfer_out_minor"`
	ScopeTransferInMinor  int64    `json:"scope_transfer_in_minor"`
	InternalOutMinor      int64    `json:"internal_out_minor"`
	InternalInMinor       int64    `json:"internal_in_minor"`
	NetMinor              int64    `json:"net_minor"`
	SavingsRate           *float64 `json:"savings_rate"`
}

// CategoryMonth is a row of agent.category_months.
type CategoryMonth struct {
	Month         string  `json:"month"`
	Scope         string  `json:"scope"`
	Currency      string  `json:"currency"`
	Category      *string `json:"category"`
	CategoryName  *string `json:"category_name"`
	SpendingMinor int64   `json:"spending_minor"`
	TxnCount      int64   `json:"txn_count"`
}

// UpcomingPayment is a row of agent.upcoming_payments.
type UpcomingPayment struct {
	Date        string  `json:"date"`
	PayeeID     *string `json:"payee_id"`
	PayeeName   string  `json:"payee_name"`
	AmountMinor int64   `json:"amount_minor"`
	Currency    string  `json:"currency"`
	Source      string  `json:"source"`
	SeriesKey   *string `json:"series_key"`
	AccountSlug *string `json:"account_slug"`
}

// CashFlowParams are the inputs of CashFlow. FromMonth and ToMonth are YYYY-MM, empty for the
// defaults (the last 12 complete months in Europe/London). Scopes empty means all. CategoryLimit is
// 0 for DefaultCategoryLimit. Limit caps each array.
type CashFlowParams struct {
	FromMonth, ToMonth string
	Scopes             []string
	CategoryLimit      int
	IncludePending     bool
	Limit              int
}

// DefaultCategoryLimit and MaxCategoryLimit bound CashFlowParams.CategoryLimit.
const (
	DefaultCategoryLimit = 10
	MaxCategoryLimit     = 100
)

// CashFlow is what the cash_flow tool returns.
type CashFlow struct {
	Months     []MonthFlow       `json:"months"`
	Categories []CategoryMonth   `json:"categories"`
	Upcoming   []UpcomingPayment `json:"upcoming"`
}

// ProfileSpan is the first and last transaction date, null on an empty ledger.
type ProfileSpan struct {
	FirstDate *string `json:"first_date"`
	LastDate  *string `json:"last_date"`
}

// ProfileAccount is an account with its resolved scope and where the scope came from.
type ProfileAccount struct {
	Slug        string `json:"slug"`
	Source      string `json:"source"`
	Scope       string `json:"scope"`
	ScopeSource string `json:"scope_source"`
}

// ProfileMedian is the monthly medians of one scope and currency.
type ProfileMedian struct {
	Scope         string   `json:"scope"`
	Currency      string   `json:"currency"`
	IncomeMinor   int64    `json:"income_minor"`
	SalaryMinor   int64    `json:"salary_minor"`
	SpendingMinor int64    `json:"spending_minor"`
	SavingsRate   *float64 `json:"savings_rate"`
}

// ProfileRecurring counts the live recurring series of one evidence and currency.
type ProfileRecurring struct {
	Evidence        string `json:"evidence"`
	Currency        string `json:"currency"`
	Count           int64  `json:"count"`
	AnnualCostMinor int64  `json:"annual_cost_minor"`
}

// ProfilePayee is one of the largest payees by spending.
type ProfilePayee struct {
	PayeeID       string `json:"payee_id"`
	PayeeName     string `json:"payee_name"`
	Currency      string `json:"currency"`
	SpendingMinor int64  `json:"spending_minor"`
}

// ProfileSeries is one of the largest recurring series by annual cost.
type ProfileSeries struct {
	SeriesKey       string `json:"series_key"`
	PayeeName       string `json:"payee_name"`
	Currency        string `json:"currency"`
	AnnualCostMinor int64  `json:"annual_cost_minor"`
}

// ProfileAnomalyCount is the anomalies of one kind in the last 90 days.
type ProfileAnomalyCount struct {
	Kind  string `json:"kind"`
	Count int64  `json:"count"`
}

// ProfileUnclassified is the spending that only the direction classified, in one currency.
type ProfileUnclassified struct {
	Currency    string `json:"currency"`
	Count       int64  `json:"count"`
	AmountMinor int64  `json:"amount_minor"`
}

// ProfileToCorrect is what the operator could still correct.
type ProfileToCorrect struct {
	UnnamedCategories       []string              `json:"unnamed_categories"`
	UnclassifiedSpending    []ProfileUnclassified `json:"unclassified_spending"`
	UnclassifiedOBTransfers int64                 `json:"unclassified_ob_transfers"`
	UnpairedCardCredits     int64                 `json:"unpaired_card_credits"`
	RuleProblems            int64                 `json:"rule_problems"`
}

// FinanceProfile is what the finance_profile tool returns.
type FinanceProfile struct {
	Span           ProfileSpan           `json:"span"`
	Accounts       []ProfileAccount      `json:"accounts"`
	MonthlyMedians []ProfileMedian       `json:"monthly_medians"`
	SalaryDay      *int64                `json:"salary_day"`
	Recurring      []ProfileRecurring    `json:"recurring"`
	TopPayees      []ProfilePayee        `json:"top_payees"`
	TopSeries      []ProfileSeries       `json:"top_series"`
	Anomalies90d   []ProfileAnomalyCount `json:"anomalies_90d"`
	ToCorrect      ProfileToCorrect      `json:"to_correct"`
}

// FinanceProfileParams are the inputs of FinanceProfile. Limit caps each array.
type FinanceProfileParams struct{ Limit int }

// decodePayload reads a row of a statement that returns one JSON payload per row.
func decodePayload(rows pgx.Rows, _ *pgtype.Map) (json.RawMessage, error) {
	var payload []byte
	if err := rows.Scan(&payload); err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), payload...), nil
}

// readTagged runs a statement whose rows are (kind, more, payload), as decodeInvestmentRow reads
// them, and returns them with whether the read was cut or any array reported more rows.
func readTagged(ctx context.Context, r *Reader, sql string, args []any, maxRows int) ([]investmentRow, bool, error) {
	rows, truncated, err := read(ctx, r, sql, args, Limits{MaxRows: maxRows, MaxBytes: CuratedMaxBytes}, nil, nil, decodeInvestmentRow)
	if err != nil {
		return nil, false, err
	}
	for _, row := range rows {
		truncated = truncated || row.More
	}
	return rows, truncated, nil
}

// likePattern is the ILIKE pattern for a substring, nil for none.
func likePattern(s string) *string {
	if s == "" {
		return nil
	}
	p := "%" + likeEscaper.Replace(s) + "%"
	return &p
}

// recurringSQL is one statement: first the annual cost of every matching series per currency, then
// the series, in sort order. $1 statuses, $2 include ignored, $3 changed only, $4 pattern, $5 sort,
// $6 the limit; a series row's more says its array was cut. Everything is read from the view.
const recurringSQL = `
	WITH f AS (
	  SELECT r.* FROM agent.recurring_payments r
	  WHERE r.status = ANY($1::text[])
	    AND ($2::boolean OR r.operator_state IS DISTINCT FROM 'ignored')
	    AND (NOT $3::boolean OR coalesce(r.change_12m_minor, 0) <> 0)
	    AND ($4::text IS NULL OR r.payee_name ILIKE $4 ESCAPE '\')
	),
	ranked AS (
	  SELECT f.*, row_number() OVER (
	           ORDER BY CASE WHEN $5::text = 'annual_cost' THEN f.annual_cost_minor END DESC NULLS LAST,
	                    CASE WHEN $5::text = 'next_date' THEN f.next_expected_date END ASC NULLS LAST,
	                    CASE WHEN $5::text = 'change_12m' THEN abs(f.change_12m_minor) END DESC NULLS LAST,
	                    f.series_key) AS rn,
	         count(*) OVER () AS n
	  FROM f
	),
	totals AS (
	  SELECT currency, sum(annual_cost_minor)::bigint AS total_minor, row_number() OVER (ORDER BY currency) AS rn
	  FROM f GROUP BY currency HAVING count(annual_cost_minor) > 0
	)
	SELECT u.kind, u.more, u.payload FROM (
	  SELECT 'total'::text AS kind, 0 AS part, t.rn, false AS more,
	         jsonb_build_object('currency', t.currency, 'total_minor', t.total_minor) AS payload
	  FROM totals t
	  UNION ALL
	  SELECT 'series', 1, r.rn, r.n > $6::int,
	         jsonb_build_object('series_key', r.series_key, 'payee_id', r.payee_id, 'payee_name', r.payee_name,
	           'evidence', r.evidence, 'cadence', r.cadence, 'amount_kind', r.amount_kind,
	           'occurrences', r.occurrences, 'first_date', r.first_date, 'last_date', r.last_date,
	           'last_amount_minor', r.last_amount_minor, 'typical_amount_minor', r.typical_amount_minor,
	           'currency', r.currency, 'annual_cost_minor', r.annual_cost_minor,
	           'next_expected_date', r.next_expected_date, 'status', r.status, 'price_changes', r.price_changes,
	           'change_12m_minor', r.change_12m_minor, 'flow_class', r.flow_class, 'category', r.category,
	           'category_name', r.category_name, 'account_slugs', r.account_slugs,
	           'operator_state', r.operator_state, 'confidence', r.confidence)
	  FROM ranked r WHERE r.rn <= $6::int
	) u
	ORDER BY u.part, u.rn`

// totalsMaxRows is how many rows beyond the limit a statement with per-currency totals may read.
const totalsMaxRows = 100

// RecurringPayments reads agent.recurring_payments in one statement: the series that match the
// filters, sorted and cut at Limit, and the annual cost per currency of every matching series
// before the cut (series with no annual cost, the irregular ones, add nothing). Status defaults to
// active, due and overdue; ignored series are left out unless IncludeIgnored. The bool is true
// when the series were cut.
func (r *Reader) RecurringPayments(ctx context.Context, p RecurringParams) (Recurring, bool, error) {
	out := Recurring{Series: []RecurringSeries{}, AnnualCostTotal: []AnnualCostTotal{}}
	statuses := p.Statuses
	if len(statuses) == 0 {
		statuses = []string{"active", "due", "overdue"}
	}
	for _, s := range statuses {
		if !slices.Contains(SeriesStatuses, s) {
			return out, false, fmt.Errorf("%w: status must be one of %s", ErrInvalid, strings.Join(SeriesStatuses, ", "))
		}
	}
	sortBy := p.Sort
	switch sortBy {
	case "":
		sortBy = SortAnnualCost
	case SortAnnualCost, SortNextDate, SortChange12m:
	default:
		return out, false, fmt.Errorf("%w: sort must be %s, %s or %s", ErrInvalid, SortAnnualCost, SortNextDate, SortChange12m)
	}
	limits, err := curatedLimit(p.Limit)
	if err != nil {
		return out, false, err
	}
	rows, truncated, err := readTagged(ctx, r, recurringSQL,
		[]any{statuses, p.IncludeIgnored, p.ChangedOnly, likePattern(p.Query), sortBy, limits.MaxRows},
		limits.MaxRows+totalsMaxRows)
	if err != nil {
		return out, false, fmt.Errorf("store: recurring payments: %w", err)
	}
	for _, row := range rows {
		var derr error
		switch row.Kind {
		case "total":
			var v AnnualCostTotal
			derr = json.Unmarshal(row.Payload, &v)
			out.AnnualCostTotal = append(out.AnnualCostTotal, v)
		case "series":
			var v RecurringSeries
			derr = json.Unmarshal(row.Payload, &v)
			if v.PriceChanges == nil {
				v.PriceChanges = []PriceChange{}
			}
			if v.AccountSlugs == nil {
				v.AccountSlugs = []string{}
			}
			out.Series = append(out.Series, v)
		}
		if derr != nil {
			return Recurring{}, false, fmt.Errorf("store: recurring payments: decode row: %w", derr)
		}
	}
	return out, truncated, nil
}

// payeesSQL is one statement over agent.payees. With a range ($3 true) the totals are summed from
// agent.cash_flows over the range (settled rows, every class but internal and scope_transfer) and a
// payee with nothing in the range is left out; without one they are the view's lifetime totals.
// $1 pattern, $2 flow class, $4 from, $5 to, $6 sort.
const payeesSQL = `
	WITH rng AS (
	  SELECT c.payee_id, c.currency,
	         coalesce(sum(-c.amount_minor) FILTER (WHERE c.amount_minor < 0), 0)::bigint AS out_minor,
	         coalesce(sum(c.amount_minor) FILTER (WHERE c.amount_minor > 0), 0)::bigint AS in_minor,
	         count(*)::bigint AS txn_count
	  FROM agent.cash_flows c
	  WHERE $3::boolean AND c.status = 'settled' AND c.flow_class NOT IN ('internal', 'scope_transfer')
	    AND ($4::date IS NULL OR c.txn_date >= $4) AND ($5::date IS NULL OR c.txn_date <= $5)
	  GROUP BY c.payee_id, c.currency
	),
	p AS (
	  SELECT p.payee_id, p.payee_name, p.currency, p.flow_class, p.category, p.category_name, p.employer,
	         p.first_date, p.last_date, p.keys, p.name_variants,
	         CASE WHEN $3::boolean THEN r.txn_count ELSE p.txn_count END AS txn_count,
	         CASE WHEN $3::boolean THEN r.out_minor ELSE p.out_minor END AS out_minor,
	         CASE WHEN $3::boolean THEN r.in_minor ELSE p.in_minor END AS in_minor
	  FROM agent.payees p
	  LEFT JOIN rng r ON r.payee_id = p.payee_id AND r.currency = p.currency
	  WHERE (NOT $3::boolean OR r.payee_id IS NOT NULL)
	    AND ($1::text IS NULL OR p.payee_name ILIKE $1 ESCAPE '\'
	         OR EXISTS (SELECT 1 FROM unnest(p.name_variants) v WHERE v ILIKE $1 ESCAPE '\'))
	    AND ($2::text IS NULL OR p.flow_class = $2)
	)
	SELECT jsonb_build_object('payee_id', p.payee_id, 'payee_name', p.payee_name, 'currency', p.currency,
	         'flow_class', p.flow_class, 'category', p.category, 'category_name', p.category_name,
	         'employer', p.employer, 'first_date', p.first_date, 'last_date', p.last_date,
	         'txn_count', p.txn_count, 'out_minor', p.out_minor, 'in_minor', p.in_minor,
	         'keys', p.keys, 'name_variants', coalesce(p.name_variants, ARRAY[]::text[]),
	         'years', coalesce((SELECT jsonb_agg(jsonb_build_object('year', y.year, 'out_minor', y.out_minor,
	                                                               'in_minor', y.in_minor, 'txn_count', y.txn_count)
	                                             ORDER BY y.year)
	                            FROM agent.payee_years y WHERE y.payee_id = p.payee_id AND y.currency = p.currency),
	                           '[]'::jsonb))
	FROM p
	ORDER BY CASE WHEN $6::text = 'out' THEN p.out_minor WHEN $6::text = 'in' THEN p.in_minor END DESC NULLS LAST,
	         CASE WHEN $6::text = 'last_date' THEN p.last_date END DESC NULLS LAST,
	         p.payee_id, p.currency`

// Payees reads agent.payees and agent.payee_years in one statement. Without From and To the totals
// are the view's lifetime ones, every flow class counted; with either, they are summed from
// agent.cash_flows over the range, settled rows of every class but internal and scope_transfer,
// and a payee with nothing in the range is left out. Sort is SortOut (the default), SortIn or
// SortLastDate, ties by payee id. The bool is true when more payees matched than were returned or
// the byte cap was reached.
func (r *Reader) Payees(ctx context.Context, p PayeesParams) ([]PayeeRow, bool, error) {
	if p.FlowClass != "" && !slices.Contains(FlowClasses, p.FlowClass) {
		return nil, false, fmt.Errorf("%w: flow_class must be one of %s", ErrInvalid, strings.Join(FlowClasses, ", "))
	}
	sortBy := p.Sort
	switch sortBy {
	case "":
		sortBy = SortOut
	case SortOut, SortIn, SortLastDate:
	default:
		return nil, false, fmt.Errorf("%w: sort must be %s, %s or %s", ErrInvalid, SortOut, SortIn, SortLastDate)
	}
	from, to, err := dayRange(p.From, p.To)
	if err != nil {
		return nil, false, err
	}
	limits, err := curatedLimit(p.Limit)
	if err != nil {
		return nil, false, err
	}
	var class *string
	if p.FlowClass != "" {
		class = &p.FlowClass
	}
	rows, truncated, err := read(ctx, r, payeesSQL,
		[]any{likePattern(p.Query), class, from != nil || to != nil, from, to, sortBy}, limits, nil, nil, decodePayload)
	if err != nil {
		return nil, false, fmt.Errorf("store: payees: %w", err)
	}
	out := make([]PayeeRow, 0, len(rows))
	for _, raw := range rows {
		var v PayeeRow
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, false, fmt.Errorf("store: payees: decode row: %w", err)
		}
		if v.Keys == nil {
			v.Keys = []string{}
		}
		if v.NameVariants == nil {
			v.NameVariants = []string{}
		}
		if v.Years == nil {
			v.Years = []PayeeYear{}
		}
		out = append(out, v)
	}
	return out, truncated, nil
}

// cashFlowSQL is one statement with three arrays, read from one snapshot: months (agent.monthly_flows
// summed over the statuses wanted), categories (agent.category_months, the top $5 in each month,
// scope and currency, settled rows only) and upcoming (agent.upcoming_payments). $1 and $2 are the
// first and last month, null for the defaults (the 12 complete months before today in
// Europe/London); $3 the scopes, $4 whether pending rows count, $6 the limit of each array.
const cashFlowSQL = `
	WITH rng AS (
	  SELECT x.to_m, coalesce($1::text, to_char((x.to_m || '-01')::date - interval '11 months', 'YYYY-MM')) AS from_m
	  FROM (SELECT coalesce($2::text, to_char(date_trunc('month', ((now() AT TIME ZONE 'Europe/London')::date)::timestamp)
	                                         - interval '1 month', 'YYYY-MM')) AS to_m) x
	),
	mf AS (
	  SELECT m.month, m.scope, m.currency,
	         sum(m.income_minor)::bigint AS income_minor, sum(m.salary_minor)::bigint AS salary_minor,
	         sum(m.refund_minor)::bigint AS refund_minor, sum(m.spending_minor)::bigint AS spending_minor,
	         sum(m.debt_repayment_minor)::bigint AS debt_repayment_minor,
	         sum(m.investment_out_minor)::bigint AS investment_out_minor,
	         sum(m.investment_in_minor)::bigint AS investment_in_minor,
	         sum(m.own_external_out_minor)::bigint AS own_external_out_minor,
	         sum(m.own_external_in_minor)::bigint AS own_external_in_minor,
	         sum(m.scope_transfer_out_minor)::bigint AS scope_transfer_out_minor,
	         sum(m.scope_transfer_in_minor)::bigint AS scope_transfer_in_minor,
	         sum(m.internal_out_minor)::bigint AS internal_out_minor,
	         sum(m.internal_in_minor)::bigint AS internal_in_minor,
	         sum(m.net_minor)::bigint AS net_minor,
	         -- One row: the view's own rate. Settled and pending rows together: the view's formula
	         -- over the sums.
	         CASE WHEN count(*) = 1 THEN max(m.savings_rate)
	              WHEN sum(m.income_minor) > 0
	              THEN round((sum(m.income_minor) - (sum(m.spending_minor) - sum(m.refund_minor))
	                          - sum(m.debt_repayment_minor))::numeric / sum(m.income_minor), 4) END AS savings_rate
	  FROM agent.monthly_flows m, rng
	  WHERE m.month BETWEEN rng.from_m AND rng.to_m
	    AND (m.status = 'settled' OR $4::boolean)
	    AND ($3::text[] IS NULL OR m.scope = ANY($3))
	  GROUP BY m.month, m.scope, m.currency
	),
	mfr AS (
	  SELECT mf.*, row_number() OVER (ORDER BY mf.month, mf.scope, mf.currency) AS rn, count(*) OVER () AS n FROM mf
	),
	cm AS (
	  SELECT m.*, row_number() OVER (PARTITION BY m.month, m.scope, m.currency
	                                 ORDER BY m.spending_minor DESC, m.category NULLS LAST) AS cat_rank
	  FROM agent.category_months m, rng
	  WHERE m.month BETWEEN rng.from_m AND rng.to_m AND ($3::text[] IS NULL OR m.scope = ANY($3))
	),
	cmr AS (
	  SELECT c.*, row_number() OVER (ORDER BY c.month, c.scope, c.currency, c.cat_rank) AS rn, count(*) OVER () AS n
	  FROM cm c WHERE c.cat_rank <= $5::int
	),
	upr AS (
	  SELECT u.*, row_number() OVER (ORDER BY u.date, coalesce(u.series_key, ''), u.payee_name, u.amount_minor,
	                                          coalesce(u.account_slug, '')) AS rn, count(*) OVER () AS n
	  FROM agent.upcoming_payments u
	  WHERE $3::text[] IS NULL OR u.account_slug IS NULL
	     OR EXISTS (SELECT 1 FROM agent.account_scopes s WHERE s.slug = u.account_slug AND s.scope = ANY($3))
	)
	SELECT u.kind, u.more, u.payload FROM (
	  SELECT 'month'::text AS kind, 0 AS part, r.rn, r.n > $6::int AS more,
	         jsonb_build_object('month', r.month, 'scope', r.scope, 'currency', r.currency,
	           'income_minor', r.income_minor, 'salary_minor', r.salary_minor, 'refund_minor', r.refund_minor,
	           'spending_minor', r.spending_minor, 'debt_repayment_minor', r.debt_repayment_minor,
	           'investment_out_minor', r.investment_out_minor, 'investment_in_minor', r.investment_in_minor,
	           'own_external_out_minor', r.own_external_out_minor, 'own_external_in_minor', r.own_external_in_minor,
	           'scope_transfer_out_minor', r.scope_transfer_out_minor, 'scope_transfer_in_minor', r.scope_transfer_in_minor,
	           'internal_out_minor', r.internal_out_minor, 'internal_in_minor', r.internal_in_minor,
	           'net_minor', r.net_minor, 'savings_rate', r.savings_rate) AS payload
	  FROM mfr r WHERE r.rn <= $6::int
	  UNION ALL
	  SELECT 'category', 1, r.rn, r.n > $6::int,
	         jsonb_build_object('month', r.month, 'scope', r.scope, 'currency', r.currency, 'category', r.category,
	           'category_name', r.category_name, 'spending_minor', r.spending_minor, 'txn_count', r.txn_count)
	  FROM cmr r WHERE r.rn <= $6::int
	  UNION ALL
	  SELECT 'upcoming', 2, r.rn, r.n > $6::int,
	         jsonb_build_object('date', r.date, 'payee_id', r.payee_id, 'payee_name', r.payee_name,
	           'amount_minor', r.amount_minor, 'currency', r.currency, 'source', r.source,
	           'series_key', r.series_key, 'account_slug', r.account_slug)
	  FROM upr r WHERE r.rn <= $6::int
	) u
	ORDER BY u.part, u.rn`

// parseMonth checks an optional YYYY-MM month.
func parseMonth(name, v string) (*string, error) {
	if v == "" {
		return nil, nil
	}
	if _, err := time.Parse("2006-01", v); err != nil || len(v) != len("2006-01") {
		return nil, fmt.Errorf("%w: %s must be a YYYY-MM month", ErrInvalid, name)
	}
	return &v, nil
}

// scopeFilter validates account scopes; an empty list is no filter (nil).
func scopeFilter(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	for _, s := range scopes {
		if !slices.Contains(AccountScopes, s) {
			return nil, fmt.Errorf("%w: a scope must be one of %s", ErrInvalid, strings.Join(AccountScopes, ", "))
		}
	}
	return scopes, nil
}

// CashFlow reads agent.monthly_flows, agent.category_months and agent.upcoming_payments in one
// statement. The months are the range's rows summed over the settled status, or over both statuses
// with IncludePending; the categories are the settled top CategoryLimit by spending in each month,
// scope and currency; the upcoming payments are the view's, those of an account in another scope
// than Scopes left out (a row with no account stays). Limit caps each array, and the bool is true
// when any was cut.
func (r *Reader) CashFlow(ctx context.Context, p CashFlowParams) (CashFlow, bool, error) {
	out := CashFlow{Months: []MonthFlow{}, Categories: []CategoryMonth{}, Upcoming: []UpcomingPayment{}}
	from, err := parseMonth("from_month", p.FromMonth)
	if err != nil {
		return out, false, err
	}
	to, err := parseMonth("to_month", p.ToMonth)
	if err != nil {
		return out, false, err
	}
	if from != nil && to != nil && *from > *to {
		return out, false, fmt.Errorf("%w: from_month is after to_month", ErrInvalid)
	}
	scopes, err := scopeFilter(p.Scopes)
	if err != nil {
		return out, false, err
	}
	catLimit := p.CategoryLimit
	switch {
	case catLimit == 0:
		catLimit = DefaultCategoryLimit
	case catLimit < 0 || catLimit > MaxCategoryLimit:
		return out, false, fmt.Errorf("%w: category_limit must be between 1 and %d", ErrInvalid, MaxCategoryLimit)
	}
	limits, err := curatedLimit(p.Limit)
	if err != nil {
		return out, false, err
	}
	rows, truncated, err := readTagged(ctx, r, cashFlowSQL,
		[]any{from, to, scopes, p.IncludePending, catLimit, limits.MaxRows}, 3*limits.MaxRows)
	if err != nil {
		return out, false, fmt.Errorf("store: cash flow: %w", err)
	}
	for _, row := range rows {
		var derr error
		switch row.Kind {
		case "month":
			var v MonthFlow
			derr = json.Unmarshal(row.Payload, &v)
			out.Months = append(out.Months, v)
		case "category":
			var v CategoryMonth
			derr = json.Unmarshal(row.Payload, &v)
			out.Categories = append(out.Categories, v)
		case "upcoming":
			var v UpcomingPayment
			derr = json.Unmarshal(row.Payload, &v)
			out.Upcoming = append(out.Upcoming, v)
		}
		if derr != nil {
			return CashFlow{}, false, fmt.Errorf("store: cash flow: decode row: %w", derr)
		}
	}
	return out, truncated, nil
}

// profileSQL is one statement with every part of the finance profile, read from one snapshot. The
// transactions come from one pass over agent.cash_flows. $1 caps each array; the two top lists are
// capped at ten as well.
const profileSQL = `
	WITH w AS (
	  SELECT (now() AT TIME ZONE 'Europe/London')::date AS today,
	         to_char(date_trunc('month', ((now() AT TIME ZONE 'Europe/London')::date)::timestamp) - interval '12 months', 'YYYY-MM') AS from_m,
	         to_char(date_trunc('month', ((now() AT TIME ZONE 'Europe/London')::date)::timestamp) - interval '1 month', 'YYYY-MM') AS to_m
	),
	cf AS MATERIALIZED (
	  SELECT c.account_slug, c.txn_date, c.status, c.amount_minor, c.currency, c.txn_category, c.payee_id, c.payee_name,
	         c.flow_class, c.flow_reason, c.employer, c.category, c.category_name
	  FROM agent.cash_flows c
	),
	span AS (SELECT min(txn_date) AS first_date, max(txn_date) AS last_date FROM cf),
	acc AS (
	  SELECT a.slug, a.source, s.scope, s.scope_source,
	         row_number() OVER (ORDER BY a.slug) AS rn, count(*) OVER () AS n
	  FROM agent.accounts a JOIN agent.account_scopes s ON s.slug = a.slug
	),
	med AS (
	  SELECT m.scope, m.currency,
	         round(percentile_cont(0.5) WITHIN GROUP (ORDER BY m.income_minor))::bigint AS income_minor,
	         round(percentile_cont(0.5) WITHIN GROUP (ORDER BY m.salary_minor))::bigint AS salary_minor,
	         round(percentile_cont(0.5) WITHIN GROUP (ORDER BY m.spending_minor))::bigint AS spending_minor,
	         round((percentile_cont(0.5) WITHIN GROUP (ORDER BY m.savings_rate))::numeric, 4) AS savings_rate
	  FROM agent.monthly_flows m, w
	  WHERE m.status = 'settled' AND m.month BETWEEN w.from_m AND w.to_m
	  GROUP BY m.scope, m.currency
	),
	medr AS (SELECT med.*, row_number() OVER (ORDER BY scope, currency) AS rn, count(*) OVER () AS n FROM med),
	sal AS (
	  SELECT mode() WITHIN GROUP (ORDER BY extract(day FROM txn_date)::int) AS day
	  FROM cf WHERE status = 'settled' AND flow_class = 'income' AND employer IS TRUE
	),
	live AS MATERIALIZED (
	  SELECT r.* FROM agent.recurring_payments r
	  WHERE r.status IN ('active', 'due', 'overdue') AND r.operator_state IS DISTINCT FROM 'ignored'
	),
	rec AS (
	  SELECT l.evidence, l.currency, count(*)::bigint AS k, coalesce(sum(l.annual_cost_minor), 0)::bigint AS annual,
	         row_number() OVER (ORDER BY l.evidence, l.currency) AS rn, count(*) OVER () AS n
	  FROM live l GROUP BY l.evidence, l.currency
	),
	tp AS (
	  SELECT c.payee_id, max(c.payee_name) AS payee_name, c.currency, sum(-c.amount_minor)::bigint AS spending,
	         row_number() OVER (ORDER BY sum(-c.amount_minor) DESC, c.payee_id, c.currency) AS rn,
	         count(*) OVER () AS n
	  FROM cf c, w
	  WHERE c.status = 'settled' AND c.flow_class = 'spending' AND c.txn_date >= w.today - 365
	  GROUP BY c.payee_id, c.currency
	),
	ts AS (
	  SELECT l.series_key, l.payee_name, l.currency, l.annual_cost_minor,
	         row_number() OVER (ORDER BY l.annual_cost_minor DESC, l.series_key) AS rn, count(*) OVER () AS n
	  FROM live l WHERE l.annual_cost_minor IS NOT NULL
	),
	an AS (
	  SELECT a.kind, count(*)::bigint AS k, row_number() OVER (ORDER BY a.kind) AS rn, count(*) OVER () AS n
	  FROM agent.anomalies a, w WHERE a.txn_date >= w.today - 90
	  GROUP BY a.kind
	),
	uc AS (
	  SELECT d.category, row_number() OVER (ORDER BY d.category) AS rn, count(*) OVER () AS n
	  FROM (SELECT DISTINCT category FROM cf
	        WHERE status = 'settled' AND category LIKE 'category\_%' ESCAPE '\' AND category_name IS NULL) d
	),
	classed AS MATERIALIZED (
	  SELECT DISTINCT payee_id FROM agent.payee_keys WHERE setting_flow_class IS NOT NULL
	),
	us AS (
	  SELECT c.currency, count(*)::bigint AS k, sum(-c.amount_minor)::bigint AS amount,
	         row_number() OVER (ORDER BY c.currency) AS rn, count(*) OVER () AS n
	  FROM cf c LEFT JOIN classed k ON k.payee_id = c.payee_id
	  WHERE c.status = 'settled' AND c.flow_class = 'spending' AND c.flow_reason = 'direction' AND k.payee_id IS NULL
	  GROUP BY c.currency
	),
	counts AS (
	  SELECT (SELECT count(*) FROM cf c WHERE c.status = 'settled' AND c.txn_category = 'TRANSFER'
	            AND c.flow_reason NOT IN ('pot', 'flex', 'own_account', 'paired', 'rule'))::bigint AS ob_transfers,
	         (SELECT count(*) FROM cf c JOIN agent.accounts a ON a.slug = c.account_slug
	          WHERE a.type = 'credit_card' AND c.status = 'settled' AND c.amount_minor > 0
	            AND c.flow_reason = 'card_credit')::bigint AS card_credits,
	         (SELECT count(*) FROM agent.payee_rule_status WHERE problem IS NOT NULL)::bigint AS rule_problems
	)
	SELECT u.kind, u.more, u.payload FROM (
	  SELECT 'span'::text AS kind, 1 AS part, 1::bigint AS rn, false AS more,
	         jsonb_build_object('first_date', s.first_date, 'last_date', s.last_date) AS payload FROM span s
	  UNION ALL
	  SELECT 'account', 2, a.rn, a.n > $1::int,
	         jsonb_build_object('slug', a.slug, 'source', a.source, 'scope', a.scope, 'scope_source', a.scope_source)
	  FROM acc a WHERE a.rn <= $1::int
	  UNION ALL
	  SELECT 'median', 3, m.rn, m.n > $1::int,
	         jsonb_build_object('scope', m.scope, 'currency', m.currency, 'income_minor', m.income_minor,
	           'salary_minor', m.salary_minor, 'spending_minor', m.spending_minor, 'savings_rate', m.savings_rate)
	  FROM medr m WHERE m.rn <= $1::int
	  UNION ALL
	  SELECT 'salary_day', 4, 1, false, jsonb_build_object('day', s.day) FROM sal s
	  UNION ALL
	  SELECT 'recurring', 5, r.rn, r.n > $1::int,
	         jsonb_build_object('evidence', r.evidence, 'currency', r.currency, 'count', r.k, 'annual_cost_minor', r.annual)
	  FROM rec r WHERE r.rn <= $1::int
	  UNION ALL
	  SELECT 'top_payee', 6, t.rn, $1::int < 10 AND t.n > $1::int,
	         jsonb_build_object('payee_id', t.payee_id, 'payee_name', t.payee_name, 'currency', t.currency,
	           'spending_minor', t.spending)
	  FROM tp t WHERE t.rn <= least(10, $1::int)
	  UNION ALL
	  SELECT 'top_series', 7, t.rn, $1::int < 10 AND t.n > $1::int,
	         jsonb_build_object('series_key', t.series_key, 'payee_name', t.payee_name, 'currency', t.currency,
	           'annual_cost_minor', t.annual_cost_minor)
	  FROM ts t WHERE t.rn <= least(10, $1::int)
	  UNION ALL
	  SELECT 'anomaly', 8, a.rn, a.n > $1::int, jsonb_build_object('kind', a.kind, 'count', a.k)
	  FROM an a WHERE a.rn <= $1::int
	  UNION ALL
	  SELECT 'unnamed_category', 9, u.rn, u.n > $1::int, jsonb_build_object('category', u.category)
	  FROM uc u WHERE u.rn <= $1::int
	  UNION ALL
	  SELECT 'unclassified', 10, u.rn, u.n > $1::int,
	         jsonb_build_object('currency', u.currency, 'count', u.k, 'amount_minor', u.amount)
	  FROM us u WHERE u.rn <= $1::int
	  UNION ALL
	  SELECT 'counts', 11, 1, false,
	         jsonb_build_object('unclassified_ob_transfers', c.ob_transfers, 'unpaired_card_credits', c.card_credits,
	                            'rule_problems', c.rule_problems)
	  FROM counts c
	) u
	ORDER BY u.part, u.rn`

// profileArrays is the most arrays of the profile that a limit caps, plus its single rows.
const profileArrays = 8

// FinanceProfile reads the profile of the data in one statement; see the finance_profile tool for
// what it holds. Limit caps each array (the two top lists hold ten at most), and the bool is true
// when any was cut. A part with no data is null (the span, the salary day) or an empty list.
func (r *Reader) FinanceProfile(ctx context.Context, p FinanceProfileParams) (FinanceProfile, bool, error) {
	out := FinanceProfile{
		Accounts: []ProfileAccount{}, MonthlyMedians: []ProfileMedian{}, Recurring: []ProfileRecurring{},
		TopPayees: []ProfilePayee{}, TopSeries: []ProfileSeries{}, Anomalies90d: []ProfileAnomalyCount{},
		ToCorrect: ProfileToCorrect{UnnamedCategories: []string{}, UnclassifiedSpending: []ProfileUnclassified{}},
	}
	limits, err := curatedLimit(p.Limit)
	if err != nil {
		return out, false, err
	}
	rows, truncated, err := readTagged(ctx, r, profileSQL, []any{limits.MaxRows}, profileArrays*limits.MaxRows+3)
	if err != nil {
		return out, false, fmt.Errorf("store: finance profile: %w", err)
	}
	for _, row := range rows {
		var derr error
		switch row.Kind {
		case "span":
			derr = json.Unmarshal(row.Payload, &out.Span)
		case "account":
			var v ProfileAccount
			derr = json.Unmarshal(row.Payload, &v)
			out.Accounts = append(out.Accounts, v)
		case "median":
			var v ProfileMedian
			derr = json.Unmarshal(row.Payload, &v)
			out.MonthlyMedians = append(out.MonthlyMedians, v)
		case "salary_day":
			var v struct {
				Day *int64 `json:"day"`
			}
			derr = json.Unmarshal(row.Payload, &v)
			out.SalaryDay = v.Day
		case "recurring":
			var v ProfileRecurring
			derr = json.Unmarshal(row.Payload, &v)
			out.Recurring = append(out.Recurring, v)
		case "top_payee":
			var v ProfilePayee
			derr = json.Unmarshal(row.Payload, &v)
			out.TopPayees = append(out.TopPayees, v)
		case "top_series":
			var v ProfileSeries
			derr = json.Unmarshal(row.Payload, &v)
			out.TopSeries = append(out.TopSeries, v)
		case "anomaly":
			var v ProfileAnomalyCount
			derr = json.Unmarshal(row.Payload, &v)
			out.Anomalies90d = append(out.Anomalies90d, v)
		case "unnamed_category":
			var v struct {
				Category string `json:"category"`
			}
			derr = json.Unmarshal(row.Payload, &v)
			out.ToCorrect.UnnamedCategories = append(out.ToCorrect.UnnamedCategories, v.Category)
		case "unclassified":
			var v ProfileUnclassified
			derr = json.Unmarshal(row.Payload, &v)
			out.ToCorrect.UnclassifiedSpending = append(out.ToCorrect.UnclassifiedSpending, v)
		case "counts":
			var v struct {
				OBTransfers int64 `json:"unclassified_ob_transfers"`
				CardCredits int64 `json:"unpaired_card_credits"`
				Problems    int64 `json:"rule_problems"`
			}
			derr = json.Unmarshal(row.Payload, &v)
			out.ToCorrect.UnclassifiedOBTransfers, out.ToCorrect.UnpairedCardCredits, out.ToCorrect.RuleProblems =
				v.OBTransfers, v.CardCredits, v.Problems
		}
		if derr != nil {
			return FinanceProfile{}, false, fmt.Errorf("store: finance profile: decode row: %w", derr)
		}
	}
	return out, truncated, nil
}

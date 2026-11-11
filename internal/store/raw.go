package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/jackc/pgx/v5"
)

// RawKind names the raw table a RawRow came from.
type RawKind string

// The five raw tables.
const (
	RawFetch         RawKind = "fetch"
	RawAPIResponse   RawKind = "api_response"
	RawFetchComplete RawKind = "fetch_complete"
	RawImportFile    RawKind = "import_file"
	RawOperatorEntry RawKind = "operator_entry"
)

// Fetch is a raw.fetch row: one fetch from a source, written when it starts.
type Fetch struct {
	ID          int64 // from raw.seq; ignored on insert
	Source      string
	Kind        string     // backfill | hourly | nightly | capture | meta
	AccountKey  *string    // the source's account id; nil for an account-list fetch
	PeriodStart *time.Time // the period asked for; nil when the fetch has none
	PeriodEnd   *time.Time
	RunID       int64
	StartedAt   time.Time
}

// APIResponse is a raw.api_response row: one response from a bank data endpoint, byte for byte.
type APIResponse struct {
	Seq         int64 // ignored on insert
	FetchID     int64
	Source      string
	Endpoint    string
	Params      json.RawMessage // nil is stored as {}
	Status      int
	ContentType string
	FetchedAt   time.Time
	Body        []byte
	BodySHA256  []byte // computed by InsertAPIResponse; ignored on insert
}

// FetchComplete is a raw.fetch_complete row: the fetch received every page.
type FetchComplete struct {
	Seq         int64 // ignored on insert
	FetchID     int64
	CompletedAt time.Time
	Pages       int
}

// ImportFile is a raw.import_file row: one imported statement file.
type ImportFile struct {
	Seq         int64 // ignored on insert
	AccountSlug string
	Format      string // halifax-pdf | manual-csv
	FileName    string
	SHA256      []byte // computed by InsertImportFile; ignored on insert
	SizeBytes   int    // computed by InsertImportFile; ignored on insert
	ImportedAt  time.Time
	Body        []byte
}

// OperatorEntry is a raw.operator_entry row: one fact entered by command.
type OperatorEntry struct {
	Seq       int64 // ignored on insert
	Kind      string
	Payload   json.RawMessage
	EnteredAt time.Time
}

// RawRow is one row of any raw table, as RawRowsAfter yields it. Kind says which one field is set.
type RawRow struct {
	Seq           int64 // the row's key from raw.seq (raw.fetch.id for a fetch)
	Kind          RawKind
	Fetch         *Fetch
	APIResponse   *APIResponse
	FetchComplete *FetchComplete
	ImportFile    *ImportFile
	OperatorEntry *OperatorEntry
}

// InsertFetch inserts f and returns its id.
func InsertFetch(ctx context.Context, q DBTX, f Fetch) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `INSERT INTO raw.fetch (source, kind, account_key, period_start, period_end, run_id, started_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		f.Source, f.Kind, f.AccountKey, f.PeriodStart, f.PeriodEnd, f.RunID, f.StartedAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: insert fetch: %w", err)
	}
	return id, nil
}

// InsertAPIResponse stores r with the SHA-256 of its body and returns its seq.
func InsertAPIResponse(ctx context.Context, q DBTX, r APIResponse) (int64, error) {
	sum := sha256.Sum256(r.Body)
	params := r.Params
	if params == nil {
		params = json.RawMessage(`{}`)
	}
	body := r.Body
	if body == nil {
		body = []byte{}
	}
	var seq int64
	err := q.QueryRow(ctx, `INSERT INTO raw.api_response
		(fetch_id, source, endpoint, params, status, content_type, fetched_at, body, body_sha256)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING seq`,
		r.FetchID, r.Source, r.Endpoint, string(params), r.Status, r.ContentType, r.FetchedAt, body, sum[:]).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("store: insert api response: %w", err)
	}
	return seq, nil
}

// InsertFetchComplete records that a fetch received every page and returns the row's seq.
func InsertFetchComplete(ctx context.Context, q DBTX, c FetchComplete) (int64, error) {
	var seq int64
	err := q.QueryRow(ctx, `INSERT INTO raw.fetch_complete (fetch_id, completed_at, pages)
		VALUES ($1, $2, $3) RETURNING seq`, c.FetchID, c.CompletedAt, c.Pages).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("store: insert fetch complete: %w", err)
	}
	return seq, nil
}

// InsertImportFile stores f with its SHA-256 and size. When the same bytes were already imported
// for the same account in the same format it stores nothing and returns the existing row's seq
// with existed true. The same bytes under another format are a new row.
func InsertImportFile(ctx context.Context, q DBTX, f ImportFile) (seq int64, existed bool, err error) {
	sum := sha256.Sum256(f.Body)
	body := f.Body
	if body == nil {
		body = []byte{}
	}
	err = q.QueryRow(ctx, `INSERT INTO raw.import_file
		(account_slug, format, file_name, sha256, size_bytes, imported_at, body)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (account_slug, format, sha256) DO NOTHING RETURNING seq`,
		f.AccountSlug, f.Format, f.FileName, sum[:], len(body), f.ImportedAt, body).Scan(&seq)
	if err == nil {
		return seq, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("store: insert import file: %w", err)
	}
	if err := q.QueryRow(ctx, `SELECT seq FROM raw.import_file WHERE account_slug = $1 AND format = $2 AND sha256 = $3`,
		f.AccountSlug, f.Format, sum[:]).Scan(&seq); err != nil {
		return 0, false, fmt.Errorf("store: find existing import file: %w", err)
	}
	return seq, true, nil
}

// InsertOperatorEntry stores e and returns its seq.
func InsertOperatorEntry(ctx context.Context, q DBTX, e OperatorEntry) (int64, error) {
	var seq int64
	err := q.QueryRow(ctx, `INSERT INTO raw.operator_entry (kind, payload, entered_at)
		VALUES ($1, $2, $3) RETURNING seq`, e.Kind, string(e.Payload), e.EnteredAt).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("store: insert operator entry: %w", err)
	}
	return seq, nil
}

// APIResponseExists reports whether a response with this endpoint, request parameters, fetch time
// and body SHA-256 is already stored, which makes loading a capture idempotent. params is
// compared as jsonb, so key order and spacing do not matter; nil means {}, as InsertAPIResponse
// stores it. The parameters keep apart two accounts' responses with the same bytes at the same
// instant.
func APIResponseExists(ctx context.Context, q DBTX, endpoint string, params json.RawMessage, fetchedAt time.Time, bodySHA256 []byte) (bool, error) {
	if params == nil {
		params = json.RawMessage(`{}`)
	}
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM raw.api_response
		WHERE endpoint = $1 AND fetched_at = $2 AND body_sha256 = $3 AND params = $4::jsonb)`,
		endpoint, fetchedAt, bodySHA256, string(params)).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("store: look up api response: %w", err)
	}
	return ok, nil
}

// GetFetch returns the fetch with id, or ErrNotFound.
func GetFetch(ctx context.Context, q DBTX, id int64) (Fetch, error) {
	rows, err := q.Query(ctx, `SELECT `+fetchColumns+` FROM raw.fetch WHERE id = $1`, id)
	if err != nil {
		return Fetch{}, fmt.Errorf("store: get fetch: %w", err)
	}
	f, err := pgx.CollectExactlyOneRow(rows, scanFetch)
	if errors.Is(err, pgx.ErrNoRows) {
		return Fetch{}, ErrNotFound
	}
	if err != nil {
		return Fetch{}, fmt.Errorf("store: get fetch: %w", err)
	}
	return f, nil
}

// FetchCompleted reports whether the fetch with id has a raw.fetch_complete row.
func FetchCompleted(ctx context.Context, q DBTX, fetchID int64) (bool, error) {
	var ok bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM raw.fetch_complete WHERE fetch_id = $1)`, fetchID).Scan(&ok); err != nil {
		return false, fmt.Errorf("store: look up fetch completion: %w", err)
	}
	return ok, nil
}

// APIResponsesForFetch returns every response of a fetch in seq order, bodies included.
func APIResponsesForFetch(ctx context.Context, q DBTX, fetchID int64) ([]APIResponse, error) {
	rows, err := q.Query(ctx, `SELECT `+apiResponseColumns+` FROM raw.api_response WHERE fetch_id = $1 ORDER BY seq`, fetchID)
	if err != nil {
		return nil, fmt.Errorf("store: responses for fetch: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanAPIResponse)
	if err != nil {
		return nil, fmt.Errorf("store: responses for fetch: %w", err)
	}
	return out, nil
}

// PreviousResponseInRun returns the latest response before beforeSeq for endpoint whose fetch
// belongs to runID and names accountKey: the previous /balance reading of the same run for the
// Monzo check. found is false when there is none.
func PreviousResponseInRun(ctx context.Context, q DBTX, runID int64, accountKey, endpoint string, beforeSeq int64) (r APIResponse, found bool, err error) {
	rows, err := q.Query(ctx, `SELECT `+prefixed("r.", apiResponseColumnList)+`
		FROM raw.api_response r JOIN raw.fetch f ON f.id = r.fetch_id
		WHERE f.run_id = $1 AND f.account_key = $2 AND r.endpoint = $3 AND r.seq < $4
		ORDER BY r.seq DESC LIMIT 1`, runID, accountKey, endpoint, beforeSeq)
	if err != nil {
		return APIResponse{}, false, fmt.Errorf("store: previous response in run: %w", err)
	}
	r, err = pgx.CollectExactlyOneRow(rows, scanAPIResponse)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIResponse{}, false, nil
	}
	if err != nil {
		return APIResponse{}, false, fmt.Errorf("store: previous response in run: %w", err)
	}
	return r, true, nil
}

// MaxRawSeq returns the largest key in any raw table, or 0 when raw is empty.
func MaxRawSeq(ctx context.Context, q DBTX) (int64, error) {
	var seq int64
	err := q.QueryRow(ctx, `SELECT greatest(
		(SELECT coalesce(max(id), 0) FROM raw.fetch),
		(SELECT coalesce(max(seq), 0) FROM raw.api_response),
		(SELECT coalesce(max(seq), 0) FROM raw.fetch_complete),
		(SELECT coalesce(max(seq), 0) FROM raw.import_file),
		(SELECT coalesce(max(seq), 0) FROM raw.operator_entry))`).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("store: max raw seq: %w", err)
	}
	return seq, nil
}

// rawBatchSize bounds how many raw rows (and bodies) RawRowsAfter holds in memory at once.
const rawBatchSize = 100

// RawRowsAfter yields every row of the five raw tables with a key greater than after, in ascending
// key order. It reads in batches and holds no cursor open between yields, so the caller may write
// on the same q (the writer-lock transaction) while iterating. Iteration stops at the first error,
// which is yielded with a zero RawRow.
func RawRowsAfter(ctx context.Context, q DBTX, after int64) iter.Seq2[RawRow, error] {
	return func(yield func(RawRow, error) bool) {
		last := after
		for {
			batch, err := rawBatch(ctx, q, last)
			if err != nil {
				yield(RawRow{}, err)
				return
			}
			for _, r := range batch {
				if !yield(r, nil) {
					return
				}
				last = r.Seq
			}
			if len(batch) < rawBatchSize {
				return
			}
		}
	}
}

func rawBatch(ctx context.Context, q DBTX, after int64) ([]RawRow, error) {
	rows, err := q.Query(ctx, `SELECT seq, kind FROM (
		  (SELECT id AS seq, 'fetch' AS kind FROM raw.fetch WHERE id > $1 ORDER BY id LIMIT $2)
		  UNION ALL (SELECT seq, 'api_response' FROM raw.api_response WHERE seq > $1 ORDER BY seq LIMIT $2)
		  UNION ALL (SELECT seq, 'fetch_complete' FROM raw.fetch_complete WHERE seq > $1 ORDER BY seq LIMIT $2)
		  UNION ALL (SELECT seq, 'import_file' FROM raw.import_file WHERE seq > $1 ORDER BY seq LIMIT $2)
		  UNION ALL (SELECT seq, 'operator_entry' FROM raw.operator_entry WHERE seq > $1 ORDER BY seq LIMIT $2)
		) k ORDER BY seq LIMIT $2`, after, rawBatchSize)
	if err != nil {
		return nil, fmt.Errorf("store: list raw rows: %w", err)
	}
	type key struct {
		seq  int64
		kind RawKind
	}
	keys, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (key, error) {
		var k key
		err := r.Scan(&k.seq, &k.kind)
		return k, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list raw rows: %w", err)
	}
	ids := make(map[RawKind][]int64)
	for _, k := range keys {
		ids[k.kind] = append(ids[k.kind], k.seq)
	}
	byKey := make(map[int64]RawRow, len(keys))
	if len(ids[RawFetch]) > 0 {
		rs, err := q.Query(ctx, `SELECT `+fetchColumns+` FROM raw.fetch WHERE id = ANY($1)`, ids[RawFetch])
		if err != nil {
			return nil, fmt.Errorf("store: read raw fetches: %w", err)
		}
		fs, err := pgx.CollectRows(rs, scanFetch)
		if err != nil {
			return nil, fmt.Errorf("store: read raw fetches: %w", err)
		}
		for i := range fs {
			byKey[fs[i].ID] = RawRow{Seq: fs[i].ID, Kind: RawFetch, Fetch: &fs[i]}
		}
	}
	if len(ids[RawAPIResponse]) > 0 {
		rs, err := q.Query(ctx, `SELECT `+apiResponseColumns+` FROM raw.api_response WHERE seq = ANY($1)`, ids[RawAPIResponse])
		if err != nil {
			return nil, fmt.Errorf("store: read raw responses: %w", err)
		}
		as, err := pgx.CollectRows(rs, scanAPIResponse)
		if err != nil {
			return nil, fmt.Errorf("store: read raw responses: %w", err)
		}
		for i := range as {
			byKey[as[i].Seq] = RawRow{Seq: as[i].Seq, Kind: RawAPIResponse, APIResponse: &as[i]}
		}
	}
	if len(ids[RawFetchComplete]) > 0 {
		rs, err := q.Query(ctx, `SELECT seq, fetch_id, completed_at, pages FROM raw.fetch_complete WHERE seq = ANY($1)`, ids[RawFetchComplete])
		if err != nil {
			return nil, fmt.Errorf("store: read raw fetch completions: %w", err)
		}
		cs, err := pgx.CollectRows(rs, func(r pgx.CollectableRow) (FetchComplete, error) {
			var c FetchComplete
			err := r.Scan(&c.Seq, &c.FetchID, &c.CompletedAt, &c.Pages)
			return c, err
		})
		if err != nil {
			return nil, fmt.Errorf("store: read raw fetch completions: %w", err)
		}
		for i := range cs {
			byKey[cs[i].Seq] = RawRow{Seq: cs[i].Seq, Kind: RawFetchComplete, FetchComplete: &cs[i]}
		}
	}
	if len(ids[RawImportFile]) > 0 {
		rs, err := q.Query(ctx, `SELECT seq, account_slug, format, file_name, sha256, size_bytes, imported_at, body
			FROM raw.import_file WHERE seq = ANY($1)`, ids[RawImportFile])
		if err != nil {
			return nil, fmt.Errorf("store: read raw import files: %w", err)
		}
		fs, err := pgx.CollectRows(rs, func(r pgx.CollectableRow) (ImportFile, error) {
			var f ImportFile
			err := r.Scan(&f.Seq, &f.AccountSlug, &f.Format, &f.FileName, &f.SHA256, &f.SizeBytes, &f.ImportedAt, &f.Body)
			return f, err
		})
		if err != nil {
			return nil, fmt.Errorf("store: read raw import files: %w", err)
		}
		for i := range fs {
			byKey[fs[i].Seq] = RawRow{Seq: fs[i].Seq, Kind: RawImportFile, ImportFile: &fs[i]}
		}
	}
	if len(ids[RawOperatorEntry]) > 0 {
		rs, err := q.Query(ctx, `SELECT seq, kind, payload, entered_at FROM raw.operator_entry WHERE seq = ANY($1)`, ids[RawOperatorEntry])
		if err != nil {
			return nil, fmt.Errorf("store: read raw operator entries: %w", err)
		}
		es, err := pgx.CollectRows(rs, func(r pgx.CollectableRow) (OperatorEntry, error) {
			var e OperatorEntry
			var payload []byte
			err := r.Scan(&e.Seq, &e.Kind, &payload, &e.EnteredAt)
			e.Payload = payload
			return e, err
		})
		if err != nil {
			return nil, fmt.Errorf("store: read raw operator entries: %w", err)
		}
		for i := range es {
			byKey[es[i].Seq] = RawRow{Seq: es[i].Seq, Kind: RawOperatorEntry, OperatorEntry: &es[i]}
		}
	}
	out := make([]RawRow, 0, len(keys))
	for _, k := range keys {
		r, ok := byKey[k.seq]
		if !ok {
			return nil, fmt.Errorf("store: raw row %d (%s) vanished while reading", k.seq, k.kind)
		}
		out = append(out, r)
	}
	return out, nil
}

const fetchColumns = `id, source, kind, account_key, period_start, period_end, run_id, started_at`

func scanFetch(r pgx.CollectableRow) (Fetch, error) {
	var f Fetch
	err := r.Scan(&f.ID, &f.Source, &f.Kind, &f.AccountKey, &f.PeriodStart, &f.PeriodEnd, &f.RunID, &f.StartedAt)
	return f, err
}

var apiResponseColumnList = []string{"seq", "fetch_id", "source", "endpoint", "params", "status",
	"content_type", "fetched_at", "body", "body_sha256"}

var apiResponseColumns = prefixed("", apiResponseColumnList)

func prefixed(p string, cols []string) string {
	s := ""
	for i, c := range cols {
		if i > 0 {
			s += ", "
		}
		s += p + c
	}
	return s
}

func scanAPIResponse(r pgx.CollectableRow) (APIResponse, error) {
	var a APIResponse
	var params []byte
	err := r.Scan(&a.Seq, &a.FetchID, &a.Source, &a.Endpoint, &params, &a.Status, &a.ContentType,
		&a.FetchedAt, &a.Body, &a.BodySHA256)
	a.Params = params
	return a, err
}

// Package capture loads capture directories into raw.
//
// A capture directory is what a one-off download script wrote: index.jsonl (one JSON object per
// HTTP response), bodies/ (the exact response bytes) and run.json (which fetches finished). Load
// validates every directory before it writes anything, then writes the responses with their
// original fetch times in an order that lets the balance check see the complete transaction set:
// account lists, then transaction pages grouped into fetches, then everything else.
//
// Nothing from an index or a body reaches an error message: errors name the directory, the line
// and the kind of problem only.
package capture

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/store"
)

const (
	sourceMonzo  = "monzo"
	fetchCapture = "capture"
	fetchMeta    = "meta"
	runKind      = "capture"

	endpointAccounts     = "/accounts"
	endpointTransactions = "/transactions"
)

// Projector normalises the raw rows written so far. ledger's CatchUp satisfies it.
type Projector interface {
	CatchUp(ctx context.Context, q store.DBTX, schema string) (ledger.Stats, error)
}

// Result says what one Load did.
type Result struct {
	Directories      int
	ResponsesRead    int // index lines read, across all directories
	ResponsesLoaded  int // responses written to raw
	ResponsesSkipped int // responses already stored
	FetchesCompleted int // raw.fetch_complete rows written
	Projection       ledger.Stats
}

// indexEntry is one line of index.jsonl. Unknown fields are ignored.
type indexEntry struct {
	Seq          int             `json:"seq"`
	Source       string          `json:"source"`
	Endpoint     string          `json:"endpoint"`
	Params       json.RawMessage `json:"params"`
	Status       int             `json:"status"`
	ContentType  string          `json:"content_type"`
	FetchedAt    string          `json:"fetched_at"`
	SHA256       string          `json:"sha256"`
	BodyFile     string          `json:"body_file"`
	WindowSince  string          `json:"window_since"`
	WindowBefore string          `json:"window_before"`
}

type runFile struct {
	Ranges []struct {
		AccountID string `json:"account_id"`
		Since     string `json:"since"`
		Before    string `json:"before"`
		Complete  bool   `json:"complete"`
	} `json:"ranges"`
}

// response is a validated index entry with its body.
type response struct {
	dir       int // position of its directory in the dirs argument
	endpoint  string
	params    json.RawMessage
	status    int
	ctype     string
	fetchedAt time.Time
	body      []byte
	sum       []byte
	seq       int // the index's own seq, a tie-breaker
	// page marks a page of a transactions fetch: a /transactions response that carries its
	// window. A /transactions response without one is a probe and is stored like any other.
	page bool
	// account is the account the request named (account_id, or current_account_id for pots), or
	// empty. A response that names an account is stored under a fetch of that account, which is
	// how the projection finds an account's earlier balance reading in the same run.
	account       string
	since, before time.Time // pages only
}

// fetchKey identifies one transactions fetch: the pages of one (account, window) in one directory.
type fetchKey struct {
	dir           int
	account       string
	since, before int64 // UnixNano
}

type fetchGroup struct {
	key      fetchKey
	pages    []*response
	complete bool
}

// Load writes the capture directories dirs to raw and then calls proj.CatchUp once.
//
// Every directory is read and checked before anything is written; a problem fails the whole load
// with an error that names the directory, the index line and the kind of problem. The writes
// happen in one transaction under the writer lock, so a failure leaves raw as it was. A response
// already stored (same endpoint, fetch time and body hash) is skipped, so loading the same
// directories twice loads once. now supplies the run's start and finish times.
func Load(ctx context.Context, st *store.Store, proj Projector, dirs []string, now func() time.Time) (Result, error) {
	if len(dirs) == 0 {
		return Result{}, errors.New("capture: no directories given")
	}
	var all []*response
	complete := make(map[fetchKey]bool)
	for i, dir := range dirs {
		rs, done, err := readDir(dir, i)
		if err != nil {
			return Result{}, err
		}
		all = append(all, rs...)
		for k := range done {
			complete[k] = true
		}
	}
	res := Result{Directories: len(dirs), ResponsesRead: len(all)}

	err := st.HoldWriterLock(ctx, func(ctx context.Context, conn *pgx.Conn) error {
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			return write(ctx, tx, all, complete, now, &res)
		})
		if err != nil {
			return err
		}
		stats, err := proj.CatchUp(ctx, conn, store.SchemaCore)
		if err != nil {
			return fmt.Errorf("capture: project loaded responses: %w", err)
		}
		res.Projection = stats
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

// write stores every response in the order the balance check needs.
func write(ctx context.Context, q store.DBTX, all []*response, complete map[fetchKey]bool, now func() time.Time, res *Result) error {
	var accounts, others []*response
	groups := make(map[fetchKey]*fetchGroup)
	var order []*fetchGroup
	for _, r := range all {
		switch {
		case r.endpoint == endpointAccounts:
			accounts = append(accounts, r)
		case r.page:
			k := fetchKey{r.dir, r.account, r.since.UnixNano(), r.before.UnixNano()}
			g := groups[k]
			if g == nil {
				g = &fetchGroup{key: k, complete: complete[k]}
				groups[k] = g
				order = append(order, g)
			}
			g.pages = append(g.pages, r)
		default:
			others = append(others, r)
		}
	}
	byTime := func(a, b *response) int {
		if c := a.fetchedAt.Compare(b.fetchedAt); c != 0 {
			return c
		}
		if a.dir != b.dir {
			return a.dir - b.dir
		}
		return a.seq - b.seq
	}
	slices.SortStableFunc(accounts, byTime)
	slices.SortStableFunc(others, byTime)
	for _, g := range order {
		slices.SortStableFunc(g.pages, byTime)
	}
	slices.SortStableFunc(order, func(a, b *fetchGroup) int { return byTime(a.pages[0], b.pages[0]) })

	// The meta fetch of a directory starts at its earliest non-transaction response.
	metaStart := make(map[int]time.Time)
	for _, r := range slices.Concat(accounts, others) {
		if t, ok := metaStart[r.dir]; !ok || r.fetchedAt.Before(t) {
			metaStart[r.dir] = r.fetchedAt
		}
	}

	runID, err := store.StartRun(ctx, q, sourceMonzo, runKind, now())
	if err != nil {
		return err
	}
	// One meta fetch per directory for responses that name no account, and one per directory and
	// account for those that do.
	type metaKey struct {
		dir     int
		account string
	}
	metaFetch := make(map[metaKey]int64)
	storeMeta := func(r *response) error {
		skip, err := exists(ctx, q, r)
		if err != nil {
			return err
		}
		if skip {
			res.ResponsesSkipped++
			return nil
		}
		k := metaKey{r.dir, r.account}
		id, ok := metaFetch[k]
		if !ok {
			f := store.Fetch{Source: sourceMonzo, Kind: fetchMeta, RunID: runID, StartedAt: metaStart[r.dir]}
			if r.account != "" {
				f.AccountKey = &r.account
			}
			id, err = store.InsertFetch(ctx, q, f)
			if err != nil {
				return err
			}
			metaFetch[k] = id
		}
		return insert(ctx, q, id, r, res)
	}

	for _, r := range accounts {
		if err := storeMeta(r); err != nil {
			return err
		}
	}
	for _, g := range order {
		if err := writeFetch(ctx, q, runID, g, res); err != nil {
			return err
		}
	}
	for _, r := range others {
		if err := storeMeta(r); err != nil {
			return err
		}
	}
	return store.FinishRun(ctx, q, runID, store.RunResult{
		FinishedAt: now(), Outcome: "ok", Requests: res.ResponsesRead, RowsSeen: res.ResponsesLoaded,
	})
}

// writeFetch stores one transactions fetch and, when the capture finished it, its completion row
// straight after the last page. A fetch whose pages were all stored already writes nothing.
//
// A fetch with only some pages stored already (a capture loaded while it was still running, then
// loaded again once finished) is stored again whole, under a new fetch row: a completion row
// speaks for every page of its fetch, so the fetch it completes must hold them all. The pages
// stored twice are harmless to the projection, which sees the same transactions again.
func writeFetch(ctx context.Context, q store.DBTX, runID int64, g *fetchGroup, res *Result) error {
	first, last := g.pages[0], g.pages[len(g.pages)-1]
	held := 0
	for _, r := range g.pages {
		skip, err := exists(ctx, q, r)
		if err != nil {
			return err
		}
		if skip {
			held++
		}
	}
	if held == len(g.pages) {
		res.ResponsesSkipped += held
		return nil
	}
	account := first.account
	fetchID, err := store.InsertFetch(ctx, q, store.Fetch{
		Source: sourceMonzo, Kind: fetchCapture, AccountKey: &account,
		PeriodStart: &first.since, PeriodEnd: &first.before, RunID: runID, StartedAt: first.fetchedAt,
	})
	if err != nil {
		return err
	}
	for _, r := range g.pages {
		if err := insert(ctx, q, fetchID, r, res); err != nil {
			return err
		}
	}
	if g.complete {
		if _, err := store.InsertFetchComplete(ctx, q, store.FetchComplete{
			FetchID: fetchID, CompletedAt: last.fetchedAt, Pages: len(g.pages),
		}); err != nil {
			return err
		}
		res.FetchesCompleted++
	}
	return nil
}

func exists(ctx context.Context, q store.DBTX, r *response) (bool, error) {
	return store.APIResponseExists(ctx, q, r.endpoint, r.params, r.fetchedAt, r.sum)
}

func insert(ctx context.Context, q store.DBTX, fetchID int64, r *response, res *Result) error {
	_, err := store.InsertAPIResponse(ctx, q, store.APIResponse{
		FetchID: fetchID, Source: sourceMonzo, Endpoint: r.endpoint, Params: r.params, Status: r.status,
		ContentType: r.ctype, FetchedAt: r.fetchedAt, Body: r.body,
	})
	if err != nil {
		return err
	}
	res.ResponsesLoaded++
	return nil
}

// readDir reads and validates one capture directory. The returned set holds the fetches run.json
// marks complete.
func readDir(dir string, pos int) ([]*response, map[fetchKey]bool, error) {
	fail := func(line int, problem string) error {
		if line > 0 {
			return fmt.Errorf("capture: %s: index.jsonl line %d: %s", dir, line, problem)
		}
		return fmt.Errorf("capture: %s: %s", dir, problem)
	}

	complete, err := readRun(dir, pos)
	if err != nil {
		return nil, nil, err
	}

	f, err := os.Open(filepath.Join(dir, "index.jsonl"))
	if err != nil {
		return nil, nil, fail(0, "index.jsonl cannot be opened")
	}
	defer func() { _ = f.Close() }()

	var out []*response
	rd := bufio.NewReader(f)
	for line := 1; ; line++ {
		raw, rerr := rd.ReadBytes('\n')
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return nil, nil, fail(line, "unreadable")
		}
		if raw = bytes.TrimSpace(raw); len(raw) > 0 {
			r, err := parseEntry(dir, pos, line, raw)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, r)
		}
		if rerr != nil {
			break
		}
	}
	return out, complete, nil
}

func parseEntry(dir string, pos, line int, raw []byte) (*response, error) {
	fail := func(problem string) error {
		return fmt.Errorf("capture: %s: index.jsonl line %d: %s", dir, line, problem)
	}
	var e indexEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fail("not a valid index entry")
	}
	if e.Source != sourceMonzo {
		return nil, fail("unexpected source")
	}
	if e.Endpoint == "" {
		return nil, fail("missing endpoint")
	}
	at, err := time.Parse(time.RFC3339Nano, e.FetchedAt)
	if err != nil {
		return nil, fail("fetched_at is not a valid time")
	}
	want, err := hex.DecodeString(e.SHA256)
	if err != nil || len(want) != sha256.Size {
		return nil, fail("sha256 is not a valid hash")
	}
	params := e.Params
	var obj map[string]any
	if len(params) == 0 || string(params) == "null" {
		params = json.RawMessage(`{}`)
	} else if err := json.Unmarshal(params, &obj); err != nil {
		return nil, fail("params is not an object")
	}
	if !filepath.IsLocal(e.BodyFile) || strings.ContainsAny(e.BodyFile, `/\`) {
		return nil, fail("body_file is not a plain file name")
	}
	body, err := os.ReadFile(filepath.Join(dir, "bodies", e.BodyFile))
	if err != nil {
		return nil, fail("body file is missing or unreadable")
	}
	if got := sha256.Sum256(body); !bytes.Equal(got[:], want) {
		return nil, fail("body hash does not match the index")
	}
	r := &response{dir: pos, endpoint: e.Endpoint, params: params, status: e.Status,
		ctype: e.ContentType, fetchedAt: at, body: body, sum: want, seq: e.Seq}
	if r.account, _ = obj["account_id"].(string); r.account == "" {
		r.account, _ = obj["current_account_id"].(string)
	}
	if e.Endpoint == endpointTransactions && (e.WindowSince != "" || e.WindowBefore != "") {
		r.page = true
		if r.account == "" {
			return nil, fail("transaction page has no account_id")
		}
		if r.since, err = time.Parse(time.RFC3339Nano, e.WindowSince); err != nil {
			return nil, fail("window_since is not a valid time")
		}
		if r.before, err = time.Parse(time.RFC3339Nano, e.WindowBefore); err != nil {
			return nil, fail("window_before is not a valid time")
		}
	}
	return r, nil
}

// readRun reads run.json and returns the fetches it marks complete.
func readRun(dir string, pos int) (map[fetchKey]bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		return nil, fmt.Errorf("capture: %s: run.json is missing or unreadable", dir)
	}
	var rf runFile
	if err := json.Unmarshal(data, &rf); err != nil {
		return nil, fmt.Errorf("capture: %s: run.json is not valid", dir)
	}
	done := make(map[fetchKey]bool)
	for i, rg := range rf.Ranges {
		since, err1 := time.Parse(time.RFC3339Nano, rg.Since)
		before, err2 := time.Parse(time.RFC3339Nano, rg.Before)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("capture: %s: run.json range %d has an invalid time", dir, i+1)
		}
		if rg.Complete {
			done[fetchKey{pos, rg.AccountID, since.UnixNano(), before.UnixNano()}] = true
		}
	}
	return done, nil
}

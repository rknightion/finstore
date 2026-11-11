package capture_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/capture"
	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// All values below are made up by hand.

type entry struct {
	endpoint string
	params   map[string]any
	status   int
	at       time.Time
	body     string
	since    string // window_since, transaction pages only
	before   string
}

type rng struct {
	Account  string `json:"account_id"`
	Since    string `json:"since"`
	Before   string `json:"before"`
	Pages    int    `json:"pages"`
	Complete bool   `json:"complete"`
}

var t0 = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

func at(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

func txPage(account, since, before string, when time.Time, body string) entry {
	return entry{endpoint: "/transactions", status: 200, at: when, body: body, since: since, before: before,
		params: map[string]any{"account_id": account, "expand[]": "merchant", "limit": 100, "since": since, "before": before}}
}

func meta(endpoint string, params map[string]any, status int, when time.Time, body string) entry {
	return entry{endpoint: endpoint, params: params, status: status, at: when, body: body}
}

// writeDir writes a capture directory. Entries are written in the order given, which is not the
// order of their fetch times.
func writeDir(t *testing.T, entries []entry, ranges []rng) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "bodies"), 0o755); err != nil {
		t.Fatal(err)
	}
	var idx bytes.Buffer
	for i, e := range entries {
		sum := sha256.Sum256([]byte(e.body))
		name := fmt.Sprintf("%04d.json", i)
		if err := os.WriteFile(filepath.Join(dir, "bodies", name), []byte(e.body), 0o600); err != nil {
			t.Fatal(err)
		}
		line := map[string]any{
			"seq": i, "source": "monzo", "endpoint": e.endpoint, "params": e.params, "kind": "test",
			"status": e.status, "content_type": "application/json",
			"fetched_at": e.at.In(time.FixedZone("", 3600)).Format(time.RFC3339Nano), "took_ms": 12,
			"sha256": hex.EncodeToString(sum[:]), "bytes": len(e.body), "body_file": name, "retried": false,
		}
		if e.since != "" {
			line["window_since"], line["window_before"] = e.since, e.before
		}
		b, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		idx.Write(b)
		idx.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "index.jsonl"), idx.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	run, err := json.Marshal(map[string]any{"ranges": ranges, "other": "ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.json"), run, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

const (
	y24s, y24b = "2024-01-01T00:00:00Z", "2025-01-01T00:00:00Z"
	y25s, y25b = "2025-01-01T00:00:00Z", "2026-01-01T00:00:00Z"
)

// mainDir has two accounts. The balance and pots responses are fetched before the transaction
// pages, and are written first in the index, so only the load order can put them last.
func mainDir(t *testing.T) (string, []entry) {
	t.Helper()
	es := []entry{
		meta("/balance", map[string]any{"account_id": "acc_one"}, 200, at(0), `{"balance":100}`),
		meta("/pots", map[string]any{"current_account_id": "acc_one"}, 200, at(1), `{"pots":[]}`),
		meta("/accounts", nil, 200, at(2), `{"accounts":[{"id":"acc_one"},{"id":"acc_two"}]}`),
		txPage("acc_one", y25s, y25b, at(10), `{"transactions":[{"id":"tx_a"}]}`),
		txPage("acc_one", y24s, y24b, at(11), `{"transactions":[{"id":"tx_b"}]}`),
		txPage("acc_two", y24s, y24b, at(12), `{"transactions":[{"id":"tx_c"}]}`),
		txPage("acc_one", y24s, y24b, at(13), `{"transactions":[{"id":"tx_d"}]}`),
		meta("/ping/whoami", nil, 403, at(14), `{"code":"forbidden"}`),
		meta("/webhooks", map[string]any{"account_id": "acc_one"}, 200, at(15), `{"webhooks":[]}`),
		meta("/balance", map[string]any{"account_id": "acc_two"}, 200, at(16), `{"balance":7}`),
	}
	ranges := []rng{
		{Account: "acc_one", Since: y24s, Before: y24b, Pages: 2, Complete: true},
		{Account: "acc_one", Since: y25s, Before: y25b, Pages: 1, Complete: false},
		{Account: "acc_two", Since: y24s, Before: y24b, Pages: 1, Complete: true},
	}
	return writeDir(t, es, ranges), es
}

// refetchDir refetches acc_one 2025 with a different range split, interleaved in time with mainDir.
func refetchDir(t *testing.T) (string, []entry) {
	t.Helper()
	es := []entry{
		txPage("acc_one", "2025-06-01T00:00:00Z", y25b, at(20), `{"transactions":[{"id":"tx_e"}]}`),
		meta("/accounts", nil, 200, at(19), `{"accounts":[{"id":"acc_one"}]}`),
		txPage("acc_one", "2025-06-01T00:00:00Z", y25b, at(21), `{"transactions":[{"id":"tx_f"}]}`),
		meta("/balance", map[string]any{"account_id": "acc_one"}, 200, at(22), `{"balance":101}`),
	}
	ranges := []rng{{Account: "acc_one", Since: "2025-06-01T00:00:00Z", Before: y25b, Pages: 2, Complete: true}}
	return writeDir(t, es, ranges), es
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	super := testpg.Start(t)
	if err := store.Bootstrap(ctx, super, "owner-secret", "app-secret"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := store.Migrate(ctx, testpg.UserURL(t, super, store.RoleOwner), ledger.NormaliserRev); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	st, err := store.Open(ctx, testpg.UserURL(t, super, store.RoleApp))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// stubProjector records its calls and the raw row counts it saw.
type stubProjector struct {
	calls   int
	schema  string
	atCall  counts
	counter func(q store.DBTX) counts
}

func (p *stubProjector) CatchUp(_ context.Context, q store.DBTX, schema string) (ledger.Stats, error) {
	p.calls++
	p.schema = schema
	p.atCall = p.counter(q)
	return ledger.Stats{RowsProcessed: 42}, nil
}

type counts struct{ fetches, responses, completions, runs int }

func count(t *testing.T, q store.DBTX) counts {
	t.Helper()
	var c counts
	err := q.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM raw.fetch), (SELECT count(*) FROM raw.api_response),
		(SELECT count(*) FROM raw.fetch_complete), (SELECT count(*) FROM sync.run)`).
		Scan(&c.fetches, &c.responses, &c.completions, &c.runs)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return c
}

func newProjector(t *testing.T) *stubProjector {
	return &stubProjector{counter: func(q store.DBTX) counts { return count(t, q) }}
}

func fixedNow() time.Time { return time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC) }

func loadAll(t *testing.T, st *store.Store, p capture.Projector, dirs ...string) (capture.Result, error) {
	t.Helper()
	return capture.Load(context.Background(), st, p, dirs, fixedNow, nil)
}

func rawRows(t *testing.T, st *store.Store) []store.RawRow {
	t.Helper()
	var rows []store.RawRow
	for r, err := range store.RawRowsAfter(context.Background(), st.DB(), 0) {
		if err != nil {
			t.Fatalf("RawRowsAfter: %v", err)
		}
		rows = append(rows, r)
	}
	return rows
}

func TestLoadStoresEveryResponseExactly(t *testing.T) {
	st := newStore(t)
	dirA, esA := mainDir(t)
	dirB, esB := refetchDir(t)
	proj := newProjector(t)

	res, err := loadAll(t, st, proj, dirA, dirB)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	total := len(esA) + len(esB)
	if res.Directories != 2 || res.ResponsesRead != total || res.ResponsesLoaded != total || res.ResponsesSkipped != 0 {
		t.Fatalf("result = %+v", res)
	}
	if res.FetchesCompleted != 3 {
		t.Fatalf("FetchesCompleted = %d, want 3", res.FetchesCompleted)
	}
	if res.Projection.RowsProcessed != 42 {
		t.Fatalf("Projection not returned: %+v", res.Projection)
	}

	// 4 transaction fetches (three in dirA, one in dirB), and the other responses under one meta
	// fetch per directory for those that name no account (2) plus one per directory and account
	// for those that do (acc_one and acc_two in dirA, acc_one in dirB).
	c := count(t, st.DB())
	if c.fetches != 9 || c.responses != total || c.completions != 3 {
		t.Fatalf("counts = %+v", c)
	}

	// Every response is stored byte for byte with its original fetch time.
	byKey := map[string]store.APIResponse{}
	fetches := map[int64]store.Fetch{}
	for _, row := range rawRows(t, st) {
		switch row.Kind {
		case store.RawAPIResponse:
			byKey[fmt.Sprintf("%s|%d", row.APIResponse.Endpoint, row.APIResponse.FetchedAt.UnixNano())] = *row.APIResponse
		case store.RawFetch:
			fetches[row.Fetch.ID] = *row.Fetch
		}
	}
	for _, e := range append(append([]entry{}, esA...), esB...) {
		got, ok := byKey[fmt.Sprintf("%s|%d", e.endpoint, e.at.UnixNano())]
		if !ok {
			t.Fatalf("response at %v for %s not stored", e.at, e.endpoint)
		}
		if string(got.Body) != e.body || got.Status != e.status || got.ContentType != "application/json" || got.Source != "monzo" {
			t.Fatalf("response %s at %v stored differently: status %d", e.endpoint, e.at, got.Status)
		}
		var wantParams, gotParams map[string]any
		wb, _ := json.Marshal(e.params)
		if e.params == nil {
			wb = []byte(`{}`)
		}
		_ = json.Unmarshal(wb, &wantParams)
		if err := json.Unmarshal(got.Params, &gotParams); err != nil || fmt.Sprint(gotParams) != fmt.Sprint(wantParams) {
			t.Fatalf("params of %s differ: %s", e.endpoint, got.Params)
		}
		f := fetches[got.FetchID]
		if e.endpoint == "/transactions" {
			if f.Kind != "capture" || f.AccountKey == nil || f.PeriodStart == nil || !f.PeriodStart.Equal(mustTime(t, e.since)) ||
				!f.PeriodEnd.Equal(mustTime(t, e.before)) || f.RunID == 0 {
				t.Fatalf("transaction fetch = %+v", f)
			}
		} else {
			named, _ := e.params["account_id"].(string)
			if named == "" {
				named, _ = e.params["current_account_id"].(string)
			}
			var got string
			if f.AccountKey != nil {
				got = *f.AccountKey
			}
			if f.Kind != "meta" || got != named || f.PeriodStart != nil || f.PeriodEnd != nil || f.RunID == 0 {
				t.Fatalf("%s response is under fetch %+v, want a meta fetch naming account %q with no period", e.endpoint, f, named)
			}
		}
	}

	// One fetch per (account, range, directory): the 2024 range of acc_one has two pages.
	pages := map[int64]int{}
	for _, r := range byKey {
		pages[r.FetchID]++
	}
	var twoPage int
	for id, f := range fetches {
		if f.Kind == "capture" && pages[id] == 2 {
			twoPage++
		}
	}
	if twoPage != 2 { // acc_one 2024 in dirA, acc_one 2025-06 in dirB
		t.Fatalf("fetches with two pages = %d, want 2", twoPage)
	}

	run, err := store.GetRun(context.Background(), st.DB(), fetchRun(fetches))
	if err != nil || run.Source != "monzo" || run.Kind != "capture" || run.Outcome == nil || *run.Outcome != "ok" {
		t.Fatalf("run = %+v, %v", run, err)
	}
}

func fetchRun(fs map[int64]store.Fetch) int64 {
	for _, f := range fs {
		return f.RunID
	}
	return 0
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestLoadOrdersAccountsThenTransactionsThenTheRest(t *testing.T) {
	st := newStore(t)
	dirA, _ := mainDir(t)
	dirB, _ := refetchDir(t)
	if _, err := loadAll(t, st, newProjector(t), dirA, dirB); err != nil {
		t.Fatalf("Load: %v", err)
	}
	rows := rawRows(t, st)

	var lastAccounts, firstTx, lastTx, firstOther int64
	firstTx, firstOther = 1<<62, 1<<62
	type fetchSpan struct{ first, last, complete int64 }
	spans := map[int64]*fetchSpan{}
	for _, row := range rows {
		switch row.Kind {
		case store.RawAPIResponse:
			r := row.APIResponse
			switch r.Endpoint {
			case "/accounts":
				lastAccounts = max(lastAccounts, row.Seq)
			case "/transactions":
				firstTx, lastTx = min(firstTx, row.Seq), max(lastTx, row.Seq)
				s := spans[r.FetchID]
				if s == nil {
					s = &fetchSpan{first: row.Seq}
					spans[r.FetchID] = s
				}
				s.last = row.Seq
			default:
				firstOther = min(firstOther, row.Seq)
			}
		case store.RawFetchComplete:
			if s := spans[row.FetchComplete.FetchID]; s != nil {
				s.complete = row.Seq
			} else {
				spans[row.FetchComplete.FetchID] = &fetchSpan{complete: row.Seq}
			}
		}
	}
	if lastAccounts >= firstTx {
		t.Fatalf("an account list (seq %d) follows a transaction page (seq %d)", lastAccounts, firstTx)
	}
	if lastTx >= firstOther {
		t.Fatalf("a transaction page (seq %d) follows another response (seq %d)", lastTx, firstOther)
	}

	// Each fetch's completion sits after its last page and before the next fetch's first page.
	var ordered []*fetchSpan
	for _, s := range spans {
		ordered = append(ordered, s)
	}
	for i := range ordered {
		for j := i + 1; j < len(ordered); j++ {
			if ordered[j].first < ordered[i].first {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}
	}
	if len(ordered) != 4 {
		t.Fatalf("fetches with pages = %d, want 4", len(ordered))
	}
	completions := 0
	for i, s := range ordered {
		if s.complete == 0 {
			continue
		}
		completions++
		if s.complete < s.last {
			t.Fatalf("fetch %d: completion %d precedes its last page %d", i, s.complete, s.last)
		}
		if i+1 < len(ordered) && s.complete > ordered[i+1].first {
			t.Fatalf("fetch %d: completion %d follows the next fetch's first page %d", i, s.complete, ordered[i+1].first)
		}
	}
	if completions != 3 {
		t.Fatalf("completions = %d, want 3", completions)
	}
}

func TestLoadCompletesOnlyFetchesRunJSONMarksComplete(t *testing.T) {
	st := newStore(t)
	dirA, _ := mainDir(t)
	if _, err := loadAll(t, st, newProjector(t), dirA); err != nil {
		t.Fatalf("Load: %v", err)
	}
	complete := map[int64]store.FetchComplete{}
	fetches := map[int64]store.Fetch{}
	for _, row := range rawRows(t, st) {
		switch row.Kind {
		case store.RawFetchComplete:
			complete[row.FetchComplete.FetchID] = *row.FetchComplete
		case store.RawFetch:
			fetches[row.Fetch.ID] = *row.Fetch
		}
	}
	if len(complete) != 2 {
		t.Fatalf("completions = %d, want 2", len(complete))
	}
	for id, c := range complete {
		f := fetches[id]
		if f.PeriodStart == nil || f.PeriodEnd == nil || !f.PeriodStart.Equal(mustTime(t, y24s)) {
			t.Fatalf("completed fetch is not a 2024 range: %+v", f)
		}
		wantPages := map[string]int{"acc_one": 2, "acc_two": 1}[*f.AccountKey]
		if c.Pages != wantPages {
			t.Fatalf("pages = %d, want %d", c.Pages, wantPages)
		}
	}
	// acc_one 2024: completed at the time of its last page (minute 13).
	for id, c := range complete {
		if *fetches[id].AccountKey == "acc_one" && !c.CompletedAt.Equal(at(13)) {
			t.Fatalf("completed at %v, want %v", c.CompletedAt, at(13))
		}
		if *fetches[id].AccountKey == "acc_one" && !fetches[id].StartedAt.Equal(at(11)) {
			t.Fatalf("fetch started at %v, want %v", fetches[id].StartedAt, at(11))
		}
	}
}

func TestLoadTwiceLoadsOnce(t *testing.T) {
	st := newStore(t)
	dirA, esA := mainDir(t)
	dirB, esB := refetchDir(t)
	if _, err := loadAll(t, st, newProjector(t), dirA, dirB); err != nil {
		t.Fatalf("first Load: %v", err)
	}
	before := count(t, st.DB())
	res, err := loadAll(t, st, newProjector(t), dirA, dirB)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	after := count(t, st.DB())
	total := len(esA) + len(esB)
	if res.ResponsesLoaded != 0 || res.ResponsesSkipped != total || res.ResponsesRead != total || res.FetchesCompleted != 0 {
		t.Fatalf("second result = %+v", res)
	}
	if after.fetches != before.fetches || after.responses != before.responses || after.completions != before.completions {
		t.Fatalf("raw counts changed: %+v -> %+v", before, after)
	}
}

func TestLoadRejectsBadDirectoriesBeforeWriting(t *testing.T) {
	const marker = "ACCOUNT-MARKER"
	good := func() []entry {
		return []entry{
			meta("/accounts", nil, 200, at(0), `{"accounts":[]}`),
			txPage(marker, y24s, y24b, at(1), `{"transactions":[]}`),
		}
	}
	tests := []struct {
		name    string
		corrupt func(t *testing.T, dir string)
		want    string
	}{
		{"hash mismatch", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "bodies", "0001.json"), []byte(`{"tampered":true}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "line 2"},
		{"missing body", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "bodies", "0001.json")); err != nil {
				t.Fatal(err)
			}
		}, "line 2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t)
			// The first directory is fine; the second is damaged: nothing of either may be written.
			okDir := writeDir(t, good(), nil)
			bad := writeDir(t, good(), nil)
			tc.corrupt(t, bad)
			proj := newProjector(t)
			_, err := loadAll(t, st, proj, okDir, bad)
			if err == nil {
				t.Fatal("Load succeeded")
			}
			msg := err.Error()
			if !strings.Contains(msg, bad) || !strings.Contains(msg, tc.want) {
				t.Fatalf("error does not name directory and line: %q", msg)
			}
			if strings.Contains(msg, marker) || strings.Contains(msg, "tampered") || strings.Contains(msg, "transactions") {
				t.Fatalf("error leaks content: %q", msg)
			}
			if c := count(t, st.DB()); c != (counts{}) {
				t.Fatalf("rows written despite the failure: %+v", c)
			}
			if proj.calls != 0 {
				t.Fatalf("projector called %d times", proj.calls)
			}
		})
	}
}

func TestLoadStoresNon200Responses(t *testing.T) {
	st := newStore(t)
	dir := writeDir(t, []entry{
		meta("/ping/whoami", nil, 403, at(0), `{"code":"forbidden.insufficient_permissions"}`),
		meta("/balance", map[string]any{"account_id": "acc_one"}, 429, at(1), ``),
	}, nil)
	res, err := loadAll(t, st, newProjector(t), dir)
	if err != nil || res.ResponsesLoaded != 2 {
		t.Fatalf("Load = %+v, %v", res, err)
	}
	got := map[int]string{}
	for _, row := range rawRows(t, st) {
		if row.Kind == store.RawAPIResponse {
			got[row.APIResponse.Status] = string(row.APIResponse.Body)
		}
	}
	if got[403] != `{"code":"forbidden.insufficient_permissions"}` || got[429] != "" || len(got) != 2 {
		t.Fatalf("stored = %v", got)
	}
}

func TestLoadCallsProjectorOnceAfterAllRows(t *testing.T) {
	st := newStore(t)
	dirA, esA := mainDir(t)
	dirB, esB := refetchDir(t)
	proj := newProjector(t)
	if _, err := loadAll(t, st, proj, dirA, dirB); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if proj.calls != 1 || proj.schema != store.SchemaCore {
		t.Fatalf("projector calls = %d, schema %q", proj.calls, proj.schema)
	}
	final := count(t, st.DB())
	if proj.atCall != final || final.responses != len(esA)+len(esB) || final.completions != 3 {
		t.Fatalf("at call %+v, final %+v", proj.atCall, final)
	}
}

// A completion row tells the projection "these pages are the whole period", so the fetch it
// completes must hold every page. When a capture is loaded while still running and loaded again
// once finished, the finished fetch is stored whole under a new fetch row. Completing a fetch
// that held only the new page would mark the first page's transactions as disappeared; writing
// no completion at all would leave the period reported as a gap for good.
func TestLoadStoresAFinishedFetchWholeWhenSomeOfItsPagesWereLoadedBefore(t *testing.T) {
	st := newStore(t)
	since, before := "2026-01-01T00:00:00Z", "2026-03-01T00:00:00Z"
	page1 := txPage("acc_made_up", since, before, at(1), `{"transactions":[{"id":"tx_a"}]}`)
	page2 := txPage("acc_made_up", since, before, at(2), `{"transactions":[{"id":"tx_b"}]}`)

	cutShort := writeDir(t, []entry{page1}, []rng{{Account: "acc_made_up", Since: since, Before: before, Pages: 1}})
	if _, err := loadAll(t, st, newProjector(t), cutShort); err != nil {
		t.Fatal(err)
	}
	if c := count(t, st.DB()); c.completions != 0 || c.responses != 1 {
		t.Fatalf("after the cut-short capture: %+v, want 1 response and no completion", c)
	}

	finished := writeDir(t, []entry{page1, page2},
		[]rng{{Account: "acc_made_up", Since: since, Before: before, Pages: 2, Complete: true}})
	res, err := loadAll(t, st, newProjector(t), finished)
	if err != nil {
		t.Fatal(err)
	}
	if res.FetchesCompleted != 1 {
		t.Fatalf("second load: %+v, want the finished fetch completed", res)
	}
	// The completed fetch holds both pages.
	ctx := context.Background()
	var completed int64
	for _, row := range rawRows(t, st) {
		if row.Kind == store.RawFetchComplete {
			completed = row.FetchComplete.FetchID
		}
	}
	pages, err := store.APIResponsesForFetch(ctx, st.DB(), completed)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 {
		t.Fatalf("the completed fetch holds %d of its 2 pages", len(pages))
	}
	// A third load of the finished capture changes nothing.
	before3 := count(t, st.DB())
	if _, err := loadAll(t, st, newProjector(t), finished); err != nil {
		t.Fatal(err)
	}
	if after := count(t, st.DB()); after.responses != before3.responses || after.completions != before3.completions || after.fetches != before3.fetches {
		t.Fatalf("a repeated load changed raw: %+v then %+v", before3, after)
	}
}

// A request made to test where the bank's time limit falls is a /transactions response with no
// window. It is not a page of any fetch: treating it as one rejected the whole directory, and
// giving it a period would let a completion row mark real transactions as disappeared.
func TestLoadStoresATransactionsProbeAsAnOrdinaryResponse(t *testing.T) {
	st := newStore(t)
	probe := entry{endpoint: "/transactions", status: 403, at: at(30), body: `{"code":"forbidden.verification_required"}`,
		params: map[string]any{"account_id": "acc_one", "since": "2019-01-01T00:00:00Z", "limit": 1}}
	dir := writeDir(t, []entry{
		txPage("acc_one", y24s, y24b, at(10), `{"transactions":[{"id":"tx_a"}]}`),
		probe,
	}, []rng{{Account: "acc_one", Since: y24s, Before: y24b, Pages: 1, Complete: true}})
	res, err := loadAll(t, st, newProjector(t), dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.ResponsesLoaded != 2 || res.FetchesCompleted != 1 {
		t.Fatalf("result = %+v, want both responses loaded and one fetch complete", res)
	}
	fetches := map[int64]store.Fetch{}
	var probeFetch int64
	for _, row := range rawRows(t, st) {
		switch row.Kind {
		case store.RawFetch:
			fetches[row.Fetch.ID] = *row.Fetch
		case store.RawAPIResponse:
			if row.APIResponse.Status == 403 {
				probeFetch = row.APIResponse.FetchID
			}
		}
	}
	f, ok := fetches[probeFetch]
	if !ok || f.Kind != "meta" || f.PeriodStart != nil || f.PeriodEnd != nil {
		t.Fatalf("the probe is under fetch %+v, want a meta fetch with no period", f)
	}
	if done, err := store.FetchCompleted(context.Background(), st.DB(), probeFetch); err != nil || done {
		t.Fatalf("the probe's fetch is complete (%v, %v); it must never be", done, err)
	}
}

// The projection confirms a balance against the same account's earlier reading in the same run,
// and finds that reading through the fetch's account. Balances stored under a fetch that names
// no account are never checked, so every account would read "not checked" after a load.
func TestLoadMakesAnAccountsEarlierBalanceReadingFindable(t *testing.T) {
	st := newStore(t)
	dir := writeDir(t, []entry{
		meta("/balance", map[string]any{"account_id": "acc_one"}, 200, at(0), `{"balance":100}`),
		meta("/balance", map[string]any{"account_id": "acc_two"}, 200, at(1), `{"balance":7}`),
		txPage("acc_one", y24s, y24b, at(10), `{"transactions":[{"id":"tx_a"}]}`),
		meta("/balance", map[string]any{"account_id": "acc_one"}, 200, at(20), `{"balance":100}`),
	}, []rng{{Account: "acc_one", Since: y24s, Before: y24b, Pages: 1, Complete: true}})
	if _, err := loadAll(t, st, newProjector(t), dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	ctx := context.Background()
	var second store.APIResponse
	fetches := map[int64]store.Fetch{}
	for _, row := range rawRows(t, st) {
		switch row.Kind {
		case store.RawFetch:
			fetches[row.Fetch.ID] = *row.Fetch
		case store.RawAPIResponse:
			if row.APIResponse.Endpoint == "/balance" && row.APIResponse.FetchedAt.Equal(at(20)) {
				second = *row.APIResponse
			}
		}
	}
	if second.Seq == 0 {
		t.Fatal("the second balance reading was not stored")
	}
	prev, found, err := store.PreviousResponseInRun(ctx, st.DB(), fetches[second.FetchID].RunID, "acc_one", "/balance", second.Seq)
	if err != nil || !found {
		t.Fatalf("the earlier reading of acc_one is not findable: found %v, %v", found, err)
	}
	if !prev.FetchedAt.Equal(at(0)) || string(prev.Body) != `{"balance":100}` {
		t.Fatalf("the earlier reading found is the wrong one: fetched %v", prev.FetchedAt)
	}
}

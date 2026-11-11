package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/store"
)

func TestRawKeysShareOneSequenceAndIterateInOrder(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	app := openStore(t, c.app)
	q := app.DB()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	type inserted struct {
		key  int64
		kind store.RawKind
	}
	var want []inserted
	add := func(key int64, kind store.RawKind, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("insert %s: %v", kind, err)
		}
		want = append(want, inserted{key, kind})
	}
	// Enough rounds that the iterator crosses several of its batches.
	for i := range 60 {
		fetchID, err := store.InsertFetch(ctx, q, store.Fetch{Source: "monzo", Kind: "hourly", AccountKey: ptr("acc"),
			PeriodStart: ptr(now), PeriodEnd: ptr(now.Add(time.Hour)), RunID: 7, StartedAt: now})
		add(fetchID, store.RawFetch, err)
		seq, err := store.InsertOperatorEntry(ctx, q, store.OperatorEntry{Kind: "account_add",
			Payload: []byte(fmt.Sprintf(`{"n":%d}`, i)), EnteredAt: now})
		add(seq, store.RawOperatorEntry, err)
		seq, err = store.InsertAPIResponse(ctx, q, store.APIResponse{FetchID: fetchID, Source: "monzo", Endpoint: "/balance",
			Status: 200, ContentType: "application/json", FetchedAt: now, Body: []byte(fmt.Sprintf(`{"b":%d}`, i))})
		add(seq, store.RawAPIResponse, err)
		seq, _, err = store.InsertImportFile(ctx, q, store.ImportFile{AccountSlug: "joint", Format: "manual-csv",
			FileName: "f.csv", ImportedAt: now, Body: []byte(fmt.Sprintf("file %d", i))})
		add(seq, store.RawImportFile, err)
		seq, err = store.InsertFetchComplete(ctx, q, store.FetchComplete{FetchID: fetchID, CompletedAt: now, Pages: 1})
		add(seq, store.RawFetchComplete, err)
	}
	for i := 1; i < len(want); i++ {
		if want[i].key <= want[i-1].key {
			t.Fatalf("key %d (%s) is not above the previous key %d (%s)", want[i].key, want[i].kind, want[i-1].key, want[i-1].kind)
		}
	}

	var got []inserted
	for r, err := range store.RawRowsAfter(ctx, q, 0) {
		if err != nil {
			t.Fatalf("RawRowsAfter: %v", err)
		}
		got = append(got, inserted{r.Seq, r.Kind})
		set := 0
		for _, p := range []bool{r.Fetch != nil, r.APIResponse != nil, r.FetchComplete != nil, r.ImportFile != nil, r.OperatorEntry != nil} {
			if p {
				set++
			}
		}
		if set != 1 {
			t.Fatalf("row %d: %d payloads set", r.Seq, set)
		}
		if r.Kind == store.RawAPIResponse {
			sum := sha256.Sum256(r.APIResponse.Body)
			if !bytes.Equal(r.APIResponse.BodySHA256, sum[:]) {
				t.Fatalf("row %d: stored SHA-256 is not the body's", r.Seq)
			}
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("iterator order differs from insertion order:\n got %v\nwant %v", got, want)
	}

	// From a midpoint, only the later rows.
	mid := want[len(want)/2].key
	n := 0
	for r, err := range store.RawRowsAfter(ctx, q, mid) {
		if err != nil {
			t.Fatal(err)
		}
		if r.Seq <= mid {
			t.Fatalf("row %d is not after %d", r.Seq, mid)
		}
		n++
	}
	if n != len(want)-len(want)/2-1 {
		t.Fatalf("after the midpoint: got %d rows, want %d", n, len(want)-len(want)/2-1)
	}
	if maxSeq, err := store.MaxRawSeq(ctx, q); err != nil || maxSeq != want[len(want)-1].key {
		t.Fatalf("MaxRawSeq = %d, %v; want %d", maxSeq, err, want[len(want)-1].key)
	}
}

func TestRawIteratorAllowsWritesInTheSameTransaction(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	app := openStore(t, c.app)
	for range 250 {
		if _, err := store.InsertOperatorEntry(ctx, app.DB(), store.OperatorEntry{Kind: "k", Payload: []byte(`{}`), EnteredAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	err := app.WithWriterLock(ctx, func(ctx context.Context, tx pgx.Tx) error {
		core := mustCore(t, tx, store.SchemaCore)
		n := 0
		for r, err := range store.RawRowsAfter(ctx, tx, 0) {
			if err != nil {
				return err
			}
			if err := core.SetLastSeq(ctx, r.Seq); err != nil {
				return err
			}
			n++
		}
		if n != 250 {
			return fmt.Errorf("iterated %d rows, want 250", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("normalise-shaped loop: %v", err)
	}
}

func TestImportFileAndCaptureIdempotence(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	q := openStore(t, c.app).DB()
	now := time.Now().UTC().Truncate(time.Microsecond)

	f := store.ImportFile{AccountSlug: "joint", Format: "halifax-pdf", FileName: "a.pdf", ImportedAt: now, Body: []byte("%PDF statement")}
	seq1, existed, err := store.InsertImportFile(ctx, q, f)
	if err != nil || existed {
		t.Fatalf("first import: seq %d existed %v err %v", seq1, existed, err)
	}
	f.FileName = "renamed.pdf"
	seq2, existed, err := store.InsertImportFile(ctx, q, f)
	if err != nil || !existed || seq2 != seq1 {
		t.Fatalf("same bytes again: seq %d existed %v err %v; want seq %d existed", seq2, existed, err, seq1)
	}
	// The same bytes under another format are a new row (a file refused under the wrong format
	// can be imported under the right one); under that format again, the same row.
	f.Format = "manual-csv"
	seq3, existed, err := store.InsertImportFile(ctx, q, f)
	if err != nil || existed || seq3 == seq1 {
		t.Fatalf("same bytes, other format: seq %d existed %v err %v", seq3, existed, err)
	}
	if seq4, existed, err := store.InsertImportFile(ctx, q, f); err != nil || !existed || seq4 != seq3 {
		t.Fatalf("same bytes, same other format again: seq %d existed %v err %v; want seq %d", seq4, existed, err, seq3)
	}
	f.AccountSlug = "mortgage"
	if _, existed, err := store.InsertImportFile(ctx, q, f); err != nil || existed {
		t.Fatalf("same bytes for another account: existed %v err %v", existed, err)
	}

	fetchID, err := store.InsertFetch(ctx, q, store.Fetch{Source: "monzo", Kind: "capture", RunID: 1, StartedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`not json at all`)
	sum := sha256.Sum256(body)
	if ok, err := store.APIResponseExists(ctx, q, "/accounts", nil, now, sum[:]); err != nil || ok {
		t.Fatalf("before insert: exists %v err %v", ok, err)
	}
	if _, err := store.InsertAPIResponse(ctx, q, store.APIResponse{FetchID: fetchID, Source: "monzo", Endpoint: "/accounts",
		Status: 502, ContentType: "text/html", FetchedAt: now, Body: body}); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.APIResponseExists(ctx, q, "/accounts", nil, now, sum[:]); err != nil || !ok {
		t.Fatalf("after insert: exists %v err %v", ok, err)
	}
	if ok, _ := store.APIResponseExists(ctx, q, "/accounts", nil, now.Add(time.Second), sum[:]); ok {
		t.Fatal("a different fetch time matched")
	}
	// Two accounts' identical responses at the same instant differ only by their parameters.
	paramsA, paramsB := json.RawMessage(`{"account_id": "acc_a"}`), json.RawMessage(`{"account_id":"acc_b"}`)
	if _, err := store.InsertAPIResponse(ctx, q, store.APIResponse{FetchID: fetchID, Source: "monzo", Endpoint: "/balance",
		Params: paramsA, Status: 200, ContentType: "application/json", FetchedAt: now, Body: body}); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.APIResponseExists(ctx, q, "/balance", json.RawMessage(`{"account_id":"acc_a"}`), now, sum[:]); err != nil || !ok {
		t.Fatalf("same params, other spacing: exists %v err %v", ok, err)
	}
	if ok, err := store.APIResponseExists(ctx, q, "/balance", paramsB, now, sum[:]); err != nil || ok {
		t.Fatalf("another account's params matched: exists %v err %v", ok, err)
	}
	rs, err := store.APIResponsesForFetch(ctx, q, fetchID)
	if err != nil || len(rs) != 2 || !bytes.Equal(rs[0].Body, body) || string(rs[0].Params) != "{}" {
		t.Fatalf("APIResponsesForFetch = %+v, %v", rs, err)
	}
}

func TestCountCallsTodayCountsOneEndpointAndBankRefInTheLocalDay(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	q := openStore(t, c.app).DB()
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	// A summer day in London is 23:00 UTC the day before to 23:00 UTC that day.
	dayStart := time.Date(2030, time.July, 10, 0, 0, 0, 0, london)
	const src, txns = "truelayer:ob-lloyds", "/data/v1/accounts/{id}/transactions"
	fetchID, err := store.InsertFetch(ctx, q, store.Fetch{Source: src, Kind: "sync", RunID: 1, StartedAt: dayStart})
	if err != nil {
		t.Fatal(err)
	}
	call := func(source, endpoint string, params string, status int, at time.Time) {
		t.Helper()
		if _, err := store.InsertAPIResponse(ctx, q, store.APIResponse{FetchID: fetchID, Source: source, Endpoint: endpoint,
			Params: json.RawMessage(params), Status: status, ContentType: "application/json", FetchedAt: at,
			Body: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	ref := `{"account_id":"made-up-1","bank_ref":"112233 12345678"}`
	call(src, txns, ref, 200, dayStart)                                // first instant of the day
	call(src, txns, ref, 429, dayStart.Add(6*time.Hour))               // a refused call counts too
	call(src, txns, ref, 200, dayStart.Add(24*time.Hour-time.Second))  // last second of the day
	call(src, txns, ref, 200, dayStart.Add(-time.Second))              // the day before
	call(src, txns, ref, 200, dayStart.Add(24*time.Hour))              // the day after
	call(src, txns, `{"bank_ref":"112233 87654321"}`, 200, dayStart)   // another account
	call(src, "/data/v1/accounts/{id}/balance", ref, 200, dayStart)    // another endpoint
	call("truelayer:ob-monzo", txns, ref, 200, dayStart)               // another source
	call(src, "/data/v1/accounts", `{}`, 200, dayStart.Add(time.Hour)) // no bank reference
	call(src, "/data/v1/accounts", `{}`, 200, dayStart.Add(2*time.Hour))

	day := time.Date(2030, time.July, 10, 15, 0, 0, 0, london)
	if n, err := store.CountCallsToday(ctx, q, src, txns, "112233 12345678", day); err != nil || n != 3 {
		t.Fatalf("transactions calls today = %d, %v; want 3", n, err)
	}
	if n, err := store.CountCallsToday(ctx, q, src, "/data/v1/accounts", "", day); err != nil || n != 2 {
		t.Fatalf("account list calls today = %d, %v; want 2 (a call with no bank reference counts against none)", n, err)
	}
	if n, err := store.CountCallsToday(ctx, q, src, txns, "112233 12345678", day.AddDate(0, 0, 1)); err != nil || n != 1 {
		t.Fatalf("transactions calls the next day = %d, %v; want 1", n, err)
	}
}

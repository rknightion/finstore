package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	if ok, err := store.APIResponseExists(ctx, q, "/accounts", now, sum[:]); err != nil || ok {
		t.Fatalf("before insert: exists %v err %v", ok, err)
	}
	if _, err := store.InsertAPIResponse(ctx, q, store.APIResponse{FetchID: fetchID, Source: "monzo", Endpoint: "/accounts",
		Status: 502, ContentType: "text/html", FetchedAt: now, Body: body}); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.APIResponseExists(ctx, q, "/accounts", now, sum[:]); err != nil || !ok {
		t.Fatalf("after insert: exists %v err %v", ok, err)
	}
	if ok, _ := store.APIResponseExists(ctx, q, "/accounts", now.Add(time.Second), sum[:]); ok {
		t.Fatal("a different fetch time matched")
	}
	rs, err := store.APIResponsesForFetch(ctx, q, fetchID)
	if err != nil || len(rs) != 1 || !bytes.Equal(rs[0].Body, body) || string(rs[0].Params) != "{}" {
		t.Fatalf("APIResponsesForFetch = %+v, %v", rs, err)
	}
}

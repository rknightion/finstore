package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/store"
)

var testNS = uuid.MustParse("6f1c1f5e-2f43-4a39-9a3e-3b3e1d1b0c55")

func testAccount(slug, typ, feed string) store.Account {
	return store.Account{ID: uuid.NewSHA1(testNS, []byte(slug)), Source: "test", SourceKey: slug, Slug: slug,
		Type: typ, Feed: feed, Currency: "GBP", Name: slug}
}

func testTxn(acc store.Account, key string, amount int64, day time.Time) store.Transaction {
	return store.Transaction{ID: uuid.NewSHA1(testNS, []byte(acc.Slug+"/"+key)), AccountID: acc.ID, Source: "test",
		SourceKey: key, AmountMinor: amount, Currency: "GBP", TxnDate: day, Status: "settled", Description: "made up"}
}

func TestTransactionVersions(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	app := openStore(t, c.app)
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	err := app.WithWriterLock(ctx, store.HolderTest, func(ctx context.Context, tx pgx.Tx) error {
		core := mustCore(t, tx, store.SchemaCore)
		acc := testAccount("monzo-current", "current", "api")
		if err := core.UpsertAccount(ctx, acc); err != nil {
			return err
		}
		txn := testTxn(acc, "tx_1", -1250, t0)

		r1, err := core.ApplyTransaction(ctx, txn, []byte(`{"amount":-1250}`), []byte("hash-1"), t0, 10)
		if err != nil {
			return err
		}
		r2, err := core.ApplyTransaction(ctx, txn, []byte(`{"amount":-1250}`), []byte("hash-1"), t0.Add(time.Hour), 11)
		if err != nil {
			return err
		}
		if r1.Outcome != store.TransactionCreated || r2.Outcome != store.TransactionUnchanged {
			t.Errorf("outcomes %s, %s; want created, unchanged", r1.Outcome, r2.Outcome)
		}
		vs, err := core.ListTransactionVersions(ctx, txn.ID)
		if err != nil {
			return err
		}
		if len(vs) != 1 {
			t.Fatalf("same content twice: %d versions, want 1", len(vs))
		}
		got, err := core.GetTransaction(ctx, txn.ID)
		if err != nil {
			return err
		}
		if !got.LastSeenAt.Equal(t0.Add(time.Hour)) || !got.FirstSeenAt.Equal(t0) {
			t.Errorf("seen times %v..%v; want %v..%v", got.FirstSeenAt, got.LastSeenAt, t0, t0.Add(time.Hour))
		}

		txn.AmountMinor = -1300
		r3, err := core.ApplyTransaction(ctx, txn, []byte(`{"amount":-1300}`), []byte("hash-2"), t0.Add(2*time.Hour), 12)
		if err != nil {
			return err
		}
		if r3.Outcome != store.TransactionChanged || r3.Previous == nil || r3.Previous.AmountMinor != -1250 {
			t.Errorf("changed content: %+v", r3)
		}
		vs, err = core.ListTransactionVersions(ctx, txn.ID)
		if err != nil {
			return err
		}
		if len(vs) != 2 || vs[1].VersionNo != 2 || vs[1].RawSeq != 12 || string(vs[1].ContentHash) != "hash-2" {
			t.Fatalf("changed content: versions %+v, want a second version from raw row 12", vs)
		}
		got, err = core.GetTransaction(ctx, txn.ID)
		if err != nil {
			return err
		}
		if got.AmountMinor != -1300 || got.VersionCount != 2 || string(got.CurrentHash) != "hash-2" || !got.FirstSeenAt.Equal(t0) {
			t.Errorf("row does not mirror the latest version: %+v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDisappearance(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	core := mustCore(t, openStore(t, c.app).DB(), store.SchemaCore)
	acc := testAccount("monzo-current", "current", "api")
	if err := core.UpsertAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 7)
	in1 := testTxn(acc, "in1", -100, start)
	in1.CreatedAt = ptr(start.Add(time.Hour))
	in2 := testTxn(acc, "in2", -200, start)
	in2.CreatedAt = ptr(start.Add(2 * time.Hour))
	outside := testTxn(acc, "out", -300, end)
	outside.CreatedAt = ptr(end) // the period's end is exclusive
	for _, tx := range []store.Transaction{in1, in2, outside} {
		if _, err := core.ApplyTransaction(ctx, tx, []byte(`{}`), []byte(tx.SourceKey), start, 1); err != nil {
			t.Fatal(err)
		}
	}
	// SetDisappeared reads the account and the period from the fetch's core.fetch_period row.
	if err := core.PutFetchPeriod(ctx, store.FetchPeriod{FetchID: 41, AccountID: acc.ID, PeriodStart: start, PeriodEnd: end}); err != nil {
		t.Fatal(err)
	}
	if p, err := core.GetFetchPeriod(ctx, 41); err != nil || !p.PeriodStart.Equal(start) || !p.PeriodEnd.Equal(end) || p.AccountID != acc.ID {
		t.Fatalf("fetch period %+v, %v", p, err)
	}
	if _, err := core.GetFetchPeriod(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing fetch period: %v", err)
	}
	at := end.Add(time.Hour)
	// A fetch with no period row changes nothing.
	if res, err := core.SetDisappeared(ctx, 42, nil, at); err != nil || len(res.Disappeared)+len(res.Returned) != 0 {
		t.Fatalf("fetch with no period: %+v, %v", res, err)
	}
	res, err := core.SetDisappeared(ctx, 41, []uuid.UUID{in1.ID}, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Disappeared) != 1 || res.Disappeared[0].ID != in2.ID || len(res.Returned) != 0 {
		t.Fatalf("first fetch: %+v; want only in2 disappeared", res)
	}
	if res.Disappeared[0].EffectiveAmount() != 0 {
		t.Errorf("a disappeared transaction still counts %d", res.Disappeared[0].EffectiveAmount())
	}
	// Again with the same result: nothing changes.
	if res, err := core.SetDisappeared(ctx, 41, []uuid.UUID{in1.ID}, at); err != nil || len(res.Disappeared)+len(res.Returned) != 0 {
		t.Fatalf("repeat: %+v, %v", res, err)
	}
	res, err = core.SetDisappeared(ctx, 41, []uuid.UUID{in1.ID, in2.ID}, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Returned) != 1 || res.Returned[0].ID != in2.ID || res.Returned[0].DisappearedAt != nil {
		t.Fatalf("returned again: %+v; want in2 cleared", res)
	}
	got, err := core.GetTransaction(ctx, outside.ID)
	if err != nil || got.DisappearedAt != nil {
		t.Fatalf("a transaction outside the period was touched: %+v %v", got, err)
	}
}

// LiveTransactionRows gives each row's current content: the version the row mirrors, not a
// late-loaded older version that has the highest version number.
func TestLiveTransactionRowsReadTheCurrentVersionNotALateOlderOne(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	core := mustCore(t, openStore(t, c.app).DB(), store.SchemaCore)
	acc := testAccount("lloyds-joint", "joint", "api")
	if err := core.UpsertAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2030, 1, 5, 12, 0, 0, 0, time.UTC)
	tx := testTxn(acc, "r1", -100, t0)
	for i, v := range []struct {
		content string
		seen    time.Time
	}{
		{`{"v":"first"}`, t0},
		{`{"v":"current"}`, t0.Add(time.Hour)},
		{`{"v":"older, loaded late"}`, t0.Add(-time.Hour)},
	} {
		if _, err := core.ApplyTransaction(ctx, tx, []byte(v.content), []byte(v.content), v.seen, int64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := core.LiveTransactionRows(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || string(rows[0].Content) != `{"v": "current"}` {
		t.Fatalf("rows %+v, want the current version's content", rows)
	}
}

func TestReconciliationAndProjectionState(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	core := mustCore(t, openStore(t, c.app).DB(), store.SchemaCore)
	acc := testAccount("monzo-current", "current", "api")
	if err := core.UpsertAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	if _, err := core.AddToRunningSum(ctx, acc.ID, 5); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("running sum without a row: %v", err)
	}
	if err := core.EnsureReconciliation(ctx, acc.ID, "monzo_absolute"); err != nil {
		t.Fatal(err)
	}
	if _, err := core.AddToRunningSum(ctx, acc.ID, -700); err != nil {
		t.Fatal(err)
	}
	sum, err := core.AddToRunningSum(ctx, acc.ID, 200)
	if err != nil || sum != -500 {
		t.Fatalf("running sum %d, %v; want -500", sum, err)
	}
	r, err := core.GetReconciliation(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.LastCheckOK = ptr(true)
	r.LastCheckAt = ptr(time.Now().UTC())
	if err := core.PutReconciliation(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := core.EnsureReconciliation(ctx, acc.ID, "none"); err != nil {
		t.Fatal(err)
	}
	if r2, err := core.GetReconciliation(ctx, acc.ID); err != nil || r2.Method != "monzo_absolute" || r2.RunningSumMinor != -500 || r2.LastCheckOK == nil || !*r2.LastCheckOK {
		t.Fatalf("after put and ensure: %+v %v", r2, err)
	}

	ps, err := core.GetProjectionState(ctx)
	if err != nil || ps != (store.ProjectionState{LastSeq: 0, NormaliserRev: testRev}) {
		t.Fatalf("seeded projection state %+v, %v", ps, err)
	}
	if err := core.SetLastSeq(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if ps, _ := core.GetProjectionState(ctx); ps.LastSeq != 42 {
		t.Fatalf("last seq %d, want 42", ps.LastSeq)
	}

	if _, err := store.NewCore(nil, "public"); !errors.Is(err, store.ErrBadSchema) {
		t.Fatalf("NewCore accepted schema public: %v", err)
	}
	if _, err := core.GetAccountBySlug(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown slug: %v", err)
	}
	if got, err := core.GetAccountBySource(ctx, "test", "monzo-current"); err != nil || got.ID != acc.ID {
		t.Fatalf("by source: %+v %v", got, err)
	}
}

// A sighting in a state already recorded next to it in seen time adds no version, so
// version_count counts distinct states: a late-loaded sighting of the state that was current at
// its time, or of the state that came next, adds none. A return to an earlier state after a change
// is a new transition and adds one.
func TestASightingOfARecordedStateAddsNoVersion(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	core := mustCore(t, openStore(t, c.app).DB(), store.SchemaCore)
	acc := testAccount("monzo-current", "current", "api")
	if err := core.UpsertAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2030, 1, 5, 12, 0, 0, 0, time.UTC)
	tx := testTxn(acc, "tx_1", -100, t0)
	for i, v := range []struct {
		state string
		seen  time.Time
		want  store.ApplyOutcome
		count int
	}{
		{"pending", t0, store.TransactionCreated, 1},
		{"settled", t0.Add(2 * time.Hour), store.TransactionChanged, 2},
		{"pending", t0.Add(-time.Hour), store.TransactionUnchanged, 2},    // the state current at its time, loaded late
		{"settled", t0.Add(time.Hour), store.TransactionUnchanged, 2},     // the state that came next, loaded late
		{"settled", t0.Add(3 * time.Hour), store.TransactionUnchanged, 2}, // the current state again
		{"pending", t0.Add(4 * time.Hour), store.TransactionChanged, 3},   // back to pending: a new transition
	} {
		r, err := core.ApplyTransaction(ctx, tx, []byte(`{"state":"`+v.state+`"}`), []byte(v.state), v.seen, int64(i+1))
		if err != nil {
			t.Fatal(err)
		}
		got, err := core.GetTransaction(ctx, tx.ID)
		if err != nil {
			t.Fatal(err)
		}
		vs, err := core.ListTransactionVersions(ctx, tx.ID)
		if err != nil {
			t.Fatal(err)
		}
		if r.Outcome != v.want || got.VersionCount != v.count || len(vs) != v.count {
			t.Fatalf("sighting %d (%s at %v): outcome %s, version_count %d, %d versions; want %s and %d",
				i+1, v.state, v.seen, r.Outcome, got.VersionCount, len(vs), v.want, v.count)
		}
	}
	got, err := core.GetTransaction(ctx, tx.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.FirstSeenAt.Equal(t0.Add(-time.Hour)) || !got.LastSeenAt.Equal(t0.Add(4*time.Hour)) || string(got.CurrentHash) != "pending" {
		t.Fatalf("row %+v", got)
	}
}

// agent.transactions shows the version the row mirrors, which need not have the highest version
// number: a distinct state loaded late, seen before the current one, is numbered after it.
func TestAgentTransactionsShowsTheCurrentVersionNotTheHighestNumbered(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	db := openStore(t, c.app).DB()
	core := mustCore(t, db, store.SchemaCore)
	acc := testAccount("monzo-current", "current", "api")
	if err := core.UpsertAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2030, 1, 5, 12, 0, 0, 0, time.UTC)
	tx := testTxn(acc, "tx_1", -100, t0)
	for i, v := range []struct {
		content string
		seen    time.Time
	}{
		{`{"v":"first"}`, t0},
		{`{"v":"current"}`, t0.Add(2 * time.Hour)},
		{`{"v":"between, loaded late"}`, t0.Add(time.Hour)},
	} {
		if _, err := core.ApplyTransaction(ctx, tx, []byte(v.content), []byte(v.content), v.seen, int64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	var detail string
	if err := db.QueryRow(ctx, `SELECT version_count, detail->>'v' FROM agent.transactions WHERE id = $1`, tx.ID).Scan(&n, &detail); err != nil {
		t.Fatal(err)
	}
	if n != 3 || detail != "current" {
		t.Fatalf("agent.transactions: version_count %d, detail %q; want 3 and the current version", n, detail)
	}
}

// A standing order follows the same version rule as a transaction, and agent.standing_orders
// shows the current version, not the highest numbered.
func TestAMandateSightingOfARecordedStateAddsNoVersion(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	db := openStore(t, c.app).DB()
	core := mustCore(t, db, store.SchemaCore)
	t0 := time.Date(2030, 1, 5, 12, 0, 0, 0, time.UTC)
	so := store.StandingOrder{ID: uuid.NewSHA1(testNS, []byte("so/1")), BankRef: "11-22-33 12345678", Source: "test", MandateKey: "so_1"}
	for i, v := range []struct {
		state string
		seen  time.Time
		want  store.ApplyOutcome
	}{
		{"first", t0, store.TransactionCreated},
		{"current", t0.Add(2 * time.Hour), store.TransactionChanged},
		{"first", t0.Add(-time.Hour), store.TransactionUnchanged},     // the state current at its time, loaded late
		{"between", t0.Add(time.Hour), store.TransactionOlderVersion}, // a distinct state, loaded late
	} {
		got, err := core.ApplyStandingOrder(ctx, so, []byte(`{"v":"`+v.state+`"}`), []byte(v.state), v.seen, int64(i+1))
		if err != nil {
			t.Fatal(err)
		}
		if got != v.want {
			t.Fatalf("sighting %d (%s): outcome %s, want %s", i+1, v.state, got, v.want)
		}
	}
	var n int
	var detail string
	if err := db.QueryRow(ctx, `SELECT version_count, detail->>'v' FROM agent.standing_orders WHERE id = $1`, so.ID).Scan(&n, &detail); err != nil {
		t.Fatal(err)
	}
	if n != 3 || detail != "current" {
		t.Fatalf("agent.standing_orders: version_count %d, detail %q; want 3 and the current version", n, detail)
	}
}

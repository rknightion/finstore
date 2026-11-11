package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/store"
)

// fillCore writes the same small projection into any core schema.
func fillCore(t *testing.T, core store.Core) (store.Account, store.Transaction) {
	t.Helper()
	ctx := context.Background()
	t0 := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	acc := testAccount("monzo-current", "current", "api")
	if err := core.UpsertAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	txn := testTxn(acc, "tx_1", -999, t0)
	if _, err := core.ApplyTransaction(ctx, txn, []byte(`{"id":"tx_1"}`), []byte("h1"), t0, 5); err != nil {
		t.Fatal(err)
	}
	if err := core.EnsureReconciliation(ctx, acc.ID, "monzo_absolute"); err != nil {
		t.Fatal(err)
	}
	if _, err := core.AddToRunningSum(ctx, acc.ID, -999); err != nil {
		t.Fatal(err)
	}
	if err := core.SetLastSeq(ctx, 5); err != nil {
		t.Fatal(err)
	}
	return acc, txn
}

func TestVerifyReportsDifferencesAndSwapsNothing(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	appDB := openStore(t, c.app).DB()
	owner := openStore(t, c.owner)
	fillCore(t, mustCore(t, appDB, store.SchemaCore))

	if err := store.CreateCoreNext(ctx, owner.DB(), testRev+1); err != nil {
		t.Fatalf("CreateCoreNext: %v", err)
	}
	next := mustCore(t, owner.DB(), store.SchemaCoreNext)
	acc, _ := fillCore(t, next)

	diffs, err := store.DiffCore(ctx, owner.DB())
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 0 {
		t.Fatalf("identical schemas reported differences: %+v", diffs)
	}

	changed := acc
	changed.Name = "renamed"
	if err := next.UpsertAccount(ctx, changed); err != nil {
		t.Fatal(err)
	}
	if err := next.InsertBalanceSnapshot(ctx, store.BalanceSnapshot{AccountID: acc.ID, RawSeq: 6, Ordinal: 0,
		At: ptr(time.Now().UTC()), BalanceMinor: -999, Currency: "GBP"}); err != nil {
		t.Fatal(err)
	}
	diffs, err = store.DiffCore(ctx, owner.DB())
	if err != nil {
		t.Fatal(err)
	}
	want := []store.CoreDiff{
		{Table: "account", Key: acc.ID.String(), Column: "name", Kind: store.DiffChanged},
		{Table: "balance_snapshot", Key: acc.ID.String() + "|6|0", Column: "", Kind: store.DiffOnlyInCoreNext},
	}
	if len(diffs) != len(want) {
		t.Fatalf("diffs %+v, want %+v", diffs, want)
	}
	for i := range want {
		if diffs[i] != want[i] {
			t.Errorf("diff %d = %+v, want %+v", i, diffs[i], want[i])
		}
	}

	if err := store.DropCoreNext(ctx, owner.DB()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DiffCore(ctx, owner.DB()); !errors.Is(err, store.ErrNoCoreNext) {
		t.Fatalf("core_next survived DropCoreNext: %v", err)
	}
	live, err := mustCore(t, appDB, store.SchemaCore).GetAccount(ctx, acc.ID)
	if err != nil || live.Name != acc.Name {
		t.Fatalf("verify changed core: %+v %v", live, err)
	}
	if ps, err := mustCore(t, appDB, store.SchemaCore).GetProjectionState(ctx); err != nil || ps.NormaliserRev != testRev {
		t.Fatalf("verify changed the revision: %+v %v", ps, err)
	}
}

func TestSwapCoreKeepsViewsAndGrants(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	app := openStore(t, c.app)
	owner := openStore(t, c.owner)
	fillCore(t, mustCore(t, app.DB(), store.SchemaCore))

	const newRev = testRev + 1
	err := owner.WithWriterLock(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := store.CreateCoreNext(ctx, tx, newRev); err != nil {
			return err
		}
		acc, _ := fillCore(t, mustCore(t, tx, store.SchemaCoreNext))
		acc.Name = "rebuilt"
		if err := mustCore(t, tx, store.SchemaCoreNext).UpsertAccount(ctx, acc); err != nil {
			return err
		}
		return store.SwapCore(ctx, tx, newRev)
	})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	core := mustCore(t, app.DB(), store.SchemaCore)
	accs, err := core.ListAccounts(ctx)
	if err != nil || len(accs) != 1 || accs[0].Name != "rebuilt" {
		t.Fatalf("core after swap: %+v %v", accs, err)
	}
	if ps, err := core.GetProjectionState(ctx); err != nil || ps != (store.ProjectionState{LastSeq: 5, NormaliserRev: newRev}) {
		t.Fatalf("projection state after swap: %+v %v", ps, err)
	}
	if _, err := store.DiffCore(ctx, owner.DB()); !errors.Is(err, store.ErrNoCoreNext) {
		t.Fatalf("core_next after swap: %v", err)
	}
	var schemas int
	if err := owner.DB().QueryRow(ctx, "SELECT count(*) FROM pg_namespace WHERE nspname IN ('core_old', 'core_next')").Scan(&schemas); err != nil || schemas != 0 {
		t.Fatalf("leftover schemas: %d %v", schemas, err)
	}

	assertAgentBoundary(t, c.super)

	// The app's grants on the new core are intact, and it reads the recreated views.
	extra := testAccount("monzo-joint", "joint", "api")
	if err := core.UpsertAccount(ctx, extra); err != nil {
		t.Fatalf("finstore_app write to swapped core: %v", err)
	}
	rows, err := store.Coverage(ctx, app.DB())
	if err != nil || len(rows) != 2 {
		t.Fatalf("coverage after swap: %+v %v", rows, err)
	}
	_, err = app.DB().Exec(ctx, "CREATE TABLE core.intruder (x int)")
	requireDenied(t, err, "finstore_app CREATE TABLE in swapped core")

	if err := store.Migrate(ctx, c.owner, newRev); err != nil {
		t.Fatalf("Migrate after swap: %v", err)
	}
	if err := store.Migrate(ctx, c.owner, testRev); !errors.Is(err, store.ErrRenormaliseRequired) {
		t.Fatalf("Migrate with the old revision after swap: %v", err)
	}
}

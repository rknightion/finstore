package ledger_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/store"
)

// scenario writes raw rows covering every kind of change, catching up after each step as the
// live service does.
func scenario(e *env) {
	jan := &period{time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2030, 2, 1, 0, 0, 0, 0, time.UTC)}
	e.accounts(fAcc{ID: "acc_1", Type: "current", Opened: "2029-01-01"}, fAcc{ID: "acc_2", Type: "current", Opened: "2028-01-01"},
		fAcc{ID: "pot_1", Type: "pot", Parent: "acc_1"})
	e.catchUp()
	tx1, tx2 := txn("tx_1", "acc_1", 1000, jan5), txn("tx_2", "acc_1", -50, jan5.Add(time.Hour))
	f := e.fetch(1, "acc_1", jan)
	e.respond(f, "/transactions", body(fBody{Transactions: []json.RawMessage{tx1, tx2}}))
	e.complete(f)
	e.balance(1, "acc_1", 950)
	e.balance(1, "acc_1", 950)
	e.catchUp()
	// A change, then a disappearance, then a return.
	f = e.fetch(2, "acc_1", jan)
	e.respond(f, "/transactions", body(fBody{Transactions: []json.RawMessage{txn("tx_1", "acc_1", 1100, jan5)}}))
	e.complete(f)
	e.balance(2, "acc_1", 1100)
	e.balance(2, "acc_1", 1100)
	e.catchUp()
	f = e.fetch(3, "acc_1", jan)
	e.respond(f, "/transactions", body(fBody{Transactions: []json.RawMessage{txn("tx_1", "acc_1", 1100, jan5), tx2}}))
	e.complete(f)
	e.respond(e.fetch(3, "acc_1", nil), "/transactions", "not json")
	e.balance(4, "pot_1", 20)
	e.catchUp()
	// An old capture loaded late: an earlier version of tx_1, and a completed January fetch that
	// predates the live sightings of tx_2. Neither may change the current state.
	old := e.fetch(8, "acc_1", jan)
	e.respondAt(old, "/transactions", body(fBody{Transactions: []json.RawMessage{txn("tx_1", "acc_1", 900, jan5)}}), capturedAt)
	e.completeAt(old, capturedAt.Add(time.Minute))
	e.catchUp()
	// A page stored after its fetch's completion row: the live path applied the completion before
	// the page existed, so replay must not let the page count as present at the completion.
	txA := txn("tx_a", "acc_2", 40, jan5)
	f = e.fetch(6, "acc_2", jan)
	e.respond(f, "/transactions", body(fBody{Transactions: []json.RawMessage{txA}}))
	e.complete(f)
	e.catchUp()
	f = e.fetch(7, "acc_2", jan)
	e.complete(f)
	e.catchUp()
	e.respond(f, "/transactions", body(fBody{Transactions: []json.RawMessage{txA}}))
	e.catchUp()
	e.operator(ledger.EntryAcceptDifference, ledger.AcceptDifference{Slug: "fake-current", DiffMinor: ptr(int64(3)), Reason: "made-up"})
	e.balance(5, "acc_2", 3)
	e.balance(5, "acc_2", 3)
	e.operator(ledger.EntryAccountAdd, ledger.AccountAdd{Slug: "bank-current", Type: "current", Currency: "GBP", Name: "Made-up", ExpectsImports: true})
	e.catchUp()
	// Statements out of date order: November, then September (a gap), then October (the heal).
	e.importFile("bank-current", "manual-csv", novCSV)
	e.importFile("bank-current", "manual-csv", sepCSV)
	e.catchUp()
	e.importFile("bank-current", "manual-csv", octCSV)
	e.catchUp()
	// A Halifax account: its reference is set by the first statement, and a statement for another
	// bank account is rejected, live and on replay.
	e.operator(ledger.EntryAccountAdd, ledger.AccountAdd{Slug: "halifax-current", Type: "current", Currency: "GBP"})
	e.importFile("halifax-current", "halifax-pdf", halifaxPDF(time.February, 50000, halifaxRows(time.February), "11-22-33", "12345678"))
	e.importFile("halifax-current", "halifax-pdf", halifaxPDF(time.January, 40000, halifaxRows(time.January), "11-22-33", "12345678"))
	e.importFile("halifax-current", "halifax-pdf", halifaxPDF(time.March, 50000, halifaxRows(time.March), "11-22-33", "87654321"))
	e.catchUp()
}

func TestReplayFromEmptyGivesTheIncrementalCore(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	scenario(e)

	err := e.owner.HoldWriterLock(ctx, func(ctx context.Context, conn *pgx.Conn) error {
		if err := store.CreateCoreNext(ctx, conn, ledger.NormaliserRev); err != nil {
			return err
		}
		if _, err := e.l.CatchUp(ctx, conn, store.SchemaCoreNext); err != nil {
			return err
		}
		diffs, err := store.DiffCore(ctx, conn)
		if err != nil {
			return err
		}
		if len(diffs) != 0 {
			t.Errorf("replay differs from the incremental core: %+v", diffs)
		}
		return store.DropCoreNext(ctx, conn)
	})
	if err != nil {
		t.Fatal(err)
	}
	// The scenario did exercise what it claims to.
	accID := wantAccountID(fakeSource, "acc_1")
	if got := e.txn(wantTxnID(accID, fakeSource, "tx_1")); got.VersionCount != 3 || got.AmountMinor != 1100 {
		t.Fatalf("tx_1 has %d versions", got.VersionCount)
	}
	if r := e.recon(accID); r.RunningSumMinor != 1050 || r.LastCheckOK == nil || !*r.LastCheckOK {
		t.Fatalf("acc_1 reconciliation %+v", r)
	}
	if r := e.recon(wantAccountID(ledger.SourceManual, "bank-current")); r.BreakCount != 0 || !sameDay(r.ReconciledThrough, date("2030-11-10")) {
		t.Fatalf("bank-current reconciliation %+v", r)
	}
	if a := e.account("halifax-current"); a.BankRef == nil || *a.BankRef != "112233 12345678" {
		t.Fatalf("halifax-current bank_ref %v", a.BankRef)
	}
}

func TestRenormaliseVerifyReportsADifferenceAndChangesNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	scenario(e)
	accID := wantAccountID(fakeSource, "acc_1")
	if _, err := e.core().AddToRunningSum(ctx, accID, 1); err != nil {
		t.Fatal(err)
	}

	diffs := e.renormalise(true)
	want := store.CoreDiff{Table: "reconciliation", Key: accID.String(), Column: "running_sum_minor", Kind: store.DiffChanged}
	if len(diffs) != 1 || diffs[0] != want {
		t.Fatalf("diffs %+v, want only %+v", diffs, want)
	}
	if r := e.recon(accID); r.RunningSumMinor != 1051 {
		t.Fatalf("verify changed core: running sum %d", r.RunningSumMinor)
	}
	var exists bool
	if err := e.app.DB().QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'core_next')").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("verify left core_next behind")
	}
}

func TestRenormaliseSwapRebuildsCoreAndTheViewsStillAnswer(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	scenario(e)
	accID := wantAccountID(fakeSource, "acc_1")
	if _, err := e.core().AddToRunningSum(ctx, accID, 1); err != nil {
		t.Fatal(err)
	}

	e.renormalise(false)
	if r := e.recon(accID); r.RunningSumMinor != 1050 {
		t.Fatalf("swap kept the seeded difference: running sum %d", r.RunningSumMinor)
	}
	conn, err := pgx.Connect(ctx, e.super)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "SET ROLE "+store.RoleAgent); err != nil {
		t.Fatal(err)
	}
	var txns, accounts int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM agent.transactions").Scan(&txns); err != nil {
		t.Fatalf("agent.transactions after swap: %v", err)
	}
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM agent.coverage").Scan(&accounts); err != nil {
		t.Fatalf("agent.coverage after swap: %v", err)
	}
	// tx_1, tx_2, four bank-current rows and four rows from two Halifax statements; tx_a is gone.
	if txns != 10 || accounts != 5 {
		t.Fatalf("views after swap: %d transactions, %d accounts", txns, accounts)
	}
	// The app role can still write the new core.
	e.balance(9, "acc_1", 1050)
	e.balance(9, "acc_1", 1050)
	e.catchUp()
}

// A core built by the previous revision has another shape (here: no bank_ref, ended_on,
// failed_on or fetch_period). Renormalise must not catch that core up with this revision's code:
// with a row not yet projected, both --verify and the swap still succeed, and the swap projects
// the row.
func TestRenormaliseOverAnOlderRevisionCoreSkipsTheLiveCatchUp(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.accounts(fAcc{ID: "acc_1", Type: "current", BankRef: refA})
	e.catchUp()
	if _, err := e.owner.DB().Exec(ctx, `DROP VIEW agent.accounts, agent.transactions, agent.balances, agent.coverage,
		agent.standing_orders, agent.direct_debits CASCADE`); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`ALTER TABLE core.account RENAME COLUMN bank_ref TO file_ref`,
		`ALTER TABLE core.account DROP COLUMN ended_on`,
		`ALTER TABLE core.reconciliation DROP COLUMN failed_on`,
		`DROP TABLE core.fetch_period`,
	} {
		if _, err := e.owner.DB().Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.owner.DB().Exec(ctx, `UPDATE core.projection_state SET normaliser_rev = $1`, ledger.NormaliserRev-1); err != nil {
		t.Fatal(err)
	}
	// Stored after the old binary's last catch-up: never projected into the old core.
	e.accounts(fAcc{ID: "acc_2", Type: "current", BankRef: refB})

	if diffs := e.renormalise(true); len(diffs) == 0 {
		t.Fatal("verify over a reshaped core reported no difference")
	}
	e.renormalise(false)
	if a := e.account("fake-current-2"); a.BankRef == nil || *a.BankRef != refB {
		t.Fatalf("the unprojected account after the swap: %+v", a)
	}
	ps, err := e.core().GetProjectionState(ctx)
	if err != nil || ps.NormaliserRev != ledger.NormaliserRev || ps.LastSeq != e.maxRawSeq() {
		t.Fatalf("projection state %+v, %v", ps, err)
	}
}

package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/importer"
	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// A revision 5 database reaches revision 6 through the usual steps: migrate asks for a renormalise,
// verify reports each new table and the rows the old service had not read and no difference in a
// bank-derived row, and the rebuild leaves a core that verifies clean. Going back, a build with no
// investment formats (what the older image is to these files) rejects them, keeps the bytes and
// creates no child, and the newer build then restores everything from raw.
func TestUpgradeFromRevisionFiveAndBack(t *testing.T) {
	e := newInvestmentEnv(t)
	ctx := context.Background()
	e.operator(ledger.EntryAccountAdd, ledger.AccountAdd{Slug: "bank-joint", Type: "joint", Currency: "GBP", Name: "Made-up"})
	e.importFile("bank-joint", string(importer.FormatManualCSV), csvFile(0, csvRow{"2030-01-02", "PAY", 100, "made-up"}))
	if st := e.catchUp(); st.FilesRejected != 0 {
		t.Fatalf("bank file: %v", st.Rejections)
	}

	// What revision 5 left: no investment table, and the older revision recorded.
	newTables := []string{"holding_check", "holding_snapshot", "instrument", "investment_event", "investment_event_version",
		"investment_import", "pension_statement", "trade_note", "valuation"}
	for _, table := range newTables {
		if _, err := e.owner.DB().Exec(ctx, `DROP TABLE core.`+table+` CASCADE`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.owner.DB().Exec(ctx, `UPDATE core.projection_state SET normaliser_rev = 5`); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, testpg.UserURL(t, e.super, store.RoleOwner), ledger.NormaliserRev); !errors.Is(err, store.ErrRenormaliseRequired) {
		t.Fatalf("migrate over a revision 5 core: %v", err)
	}

	// Investment files that arrive with the new image, not yet projected.
	const acct = "ACC00001234"
	entry, err := json.Marshal(ledger.AccountAdd{Slug: "fid", Type: "platform", Currency: "GBP", Name: "Made-up", ExpectsImports: true,
		Provider: ledger.ProviderFidelity})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertOperatorEntry(ctx, e.app.DB(), store.OperatorEntry{Kind: ledger.EntryAccountAdd, Payload: entry, EnteredAt: e.tick()}); err != nil {
		t.Fatal(err)
	}
	file, err := json.Marshal(ledgerFile(acct, cashRow(acct, "R1", ledger.EventInterest, "2030-01-05", 100)))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.InsertImportFile(ctx, e.app.DB(), store.ImportFile{AccountSlug: "fid", Format: fakeFidelityFormat,
		FileName: "made-up", ImportedAt: e.tick(), Body: file}); err != nil {
		t.Fatal(err)
	}

	tables := map[string]bool{}
	for _, d := range e.renormalise(true) {
		switch {
		case d.Kind == store.DiffTable:
			tables[d.Table] = true
		case d.Kind == store.DiffOnlyInCoreNext && (d.Table == "account" || d.Table == "reconciliation"):
		case d.Table == "projection_state":
		default:
			t.Errorf("a difference in a bank-derived row: %+v", d)
		}
	}
	for _, table := range newTables {
		if !tables[table] {
			t.Errorf("verify did not report the new table %s (reported %v)", table, tables)
		}
	}

	e.renormalise(false)
	if ps, err := e.core().GetProjectionState(ctx); err != nil || ps.NormaliserRev != ledger.NormaliserRev {
		t.Fatalf("after the rebuild: %+v, %v", ps, err)
	}
	if n := e.count(`SELECT count(*) FROM core.investment_event`); n != 1 {
		t.Fatalf("%d events after the rebuild", n)
	}
	if diffs := e.renormalise(true); len(diffs) != 0 {
		t.Fatalf("verify after the rebuild: %+v", diffs)
	}

	// Back: without the formats the file is refused and no child exists; its bytes stay in raw.
	newer := e.l
	e.l = ledger.New(nil, map[string]ledger.FileNormaliser{string(importer.FormatManualCSV): importer.Normaliser{}},
		time.UTC, slog.New(slog.NewJSONHandler(e.logs, nil)), nil)
	e.renormalise(false)
	if n := e.count(`SELECT count(*) FROM core.account WHERE parent_id IS NOT NULL`); n != 0 {
		t.Fatalf("%d child accounts without the formats", n)
	}
	if n := e.count(`SELECT count(*) FROM core.account WHERE type = 'platform'`); n != 1 {
		t.Fatalf("%d platforms without the formats", n)
	}
	if n := e.count(`SELECT count(*) FROM raw.import_file WHERE format = $1`, fakeFidelityFormat); n != 1 {
		t.Fatalf("%d raw files", n)
	}
	if !strings.Contains(e.logs.String(), "unknown_format") {
		t.Fatal("the refused file was not logged as unknown_format")
	}
	e.l = newer
	e.renormalise(false)
	if n := e.count(`SELECT count(*) FROM core.investment_event`); n != 1 {
		t.Fatalf("%d events after coming back", n)
	}
}

// Revision 5 of the normaliser adds no version for a sighting of a state already recorded beside
// it (a late-loaded sighting of an older state), and records accepted statement gaps. Revision 6
// adds the investment tables. Revision 7 adds the payee rule tables and core.account.scope.
// Revision 8 lets a payee setting hold the borrowing flow class. Revision 9 adds core.txn_class.
func TestNormaliserRevisionIsNine(t *testing.T) {
	if ledger.NormaliserRev != 9 {
		t.Fatalf("NormaliserRev = %d, want 9", ledger.NormaliserRev)
	}
}

// A core built by revision 4 holds an extra version for a late-loaded sighting of an older state:
// version 3 repeats version 1, and version_count counts it. Renormalise rebuilds it with one
// version per distinct state, agent.transactions still shows the current state, and a verify
// afterwards reports no difference.
func TestRenormaliseOverARevisionFourCoreDropsRepeatedVersions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.accounts(fAcc{ID: "acc_1", Type: "current", Opened: "2029-01-01"})
	first := e.clock
	e.respond(e.fetch(2, "acc_1", nil), "/transactions", body(fBody{Transactions: []json.RawMessage{txn("tx_1", "acc_1", -100, jan5)}}))
	e.respond(e.fetch(3, "acc_1", nil), "/transactions", body(fBody{Transactions: []json.RawMessage{txn("tx_1", "acc_1", -150, jan5)}}))
	// The first state again, from an old capture fetched before either, loaded late.
	lateAt := first.AddDate(0, 0, -1)
	late := e.respondAt(e.fetch(4, "acc_1", nil), "/transactions", body(fBody{Transactions: []json.RawMessage{txn("tx_1", "acc_1", -100, jan5)}}), lateAt)
	e.catchUp()
	txID := wantTxnID(wantAccountID(fakeSource, "acc_1"), fakeSource, "tx_1")

	// What revision 4 made of those rows: a third version repeating the first, counted. (This
	// code made none; revision 4's code made it already.)
	owner := e.owner.DB()
	if _, err := owner.Exec(ctx, `INSERT INTO core.transaction_version (transaction_id, version_no, content, content_hash, seen_at, raw_seq)
		SELECT transaction_id, 3, content, content_hash, $2, $3 FROM core.transaction_version
		WHERE transaction_id = $1 AND version_no = 1
		ON CONFLICT DO NOTHING`, txID, lateAt, late); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `UPDATE core.transaction SET version_count = 3 WHERE id = $1`, txID); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `UPDATE core.projection_state SET normaliser_rev = 4`); err != nil {
		t.Fatal(err)
	}

	if diffs := e.renormalise(true); len(diffs) == 0 {
		t.Fatal("verify over a revision-4 core with a repeated version reported no difference")
	}
	e.renormalise(false)
	if got := e.txn(txID); got.VersionCount != 2 || got.AmountMinor != -150 {
		t.Fatalf("after the rebuild: %+v, want 2 versions and the later state", got)
	}
	versions, err := e.core().ListTransactionVersions(ctx, txID)
	if err != nil || len(versions) != 2 {
		t.Fatalf("after the rebuild: %d versions, %v", len(versions), err)
	}
	var n int
	var amount float64
	if err := e.app.DB().QueryRow(ctx, `SELECT version_count, (detail->>'amount')::float FROM agent.transactions WHERE id = $1`, txID).
		Scan(&n, &amount); err != nil || n != 2 || amount != -150 {
		t.Fatalf("agent.transactions: version_count %d, detail amount %v, %v", n, amount, err)
	}
	if diffs := e.renormalise(true); len(diffs) != 0 {
		t.Fatalf("verify after the rebuild: %+v", diffs)
	}
	ps, err := e.core().GetProjectionState(ctx)
	if err != nil || ps.NormaliserRev != ledger.NormaliserRev {
		t.Fatalf("projection state %+v, %v", ps, err)
	}
}

// Codex's sequence through the ledger: A at t1, A at t3, B at t2 loaded late, B at t4. The row
// ends at B and the running sum follows it.
func TestRunningSumFollowsANewerSightingThatAddsNoVersion(t *testing.T) {
	e := newEnv(t)
	e.accounts(fAcc{ID: "acc_1", Type: "current", Opened: "2029-01-01"})
	t1 := e.clock.Add(time.Hour)
	for i, v := range []struct {
		amount int64
		seen   time.Time
	}{{-100, t1}, {-100, t1.Add(2 * time.Hour)}, {-150, t1.Add(time.Hour)}, {-150, t1.Add(3 * time.Hour)}} {
		e.respondAt(e.fetch(int64(i+2), "acc_1", nil), "/transactions", body(fBody{Transactions: []json.RawMessage{txn("tx_1", "acc_1", v.amount, jan5)}}), v.seen)
		e.catchUp()
	}
	accID := wantAccountID(fakeSource, "acc_1")
	txID := wantTxnID(accID, fakeSource, "tx_1")
	if got := e.txn(txID); got.AmountMinor != -150 || got.VersionCount != 2 {
		t.Fatalf("transaction %+v, want amount -150 and 2 versions", got)
	}
	if r := e.recon(accID); r.RunningSumMinor != -150 {
		t.Fatalf("running sum %d, want -150", r.RunningSumMinor)
	}
}

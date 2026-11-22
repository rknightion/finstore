package ledger_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rknightion/finstore/internal/ledger"
)

// Revision 4 of the normaliser adds no version for a sighting of a state already recorded beside
// it (a late-loaded sighting of an older state), and records accepted statement gaps.
func TestNormaliserRevisionIsFour(t *testing.T) {
	if ledger.NormaliserRev != 4 {
		t.Fatalf("NormaliserRev = %d, want 4", ledger.NormaliserRev)
	}
}

// A core built by revision 3 holds an extra version for a late-loaded sighting of an older state:
// version 3 repeats version 1, and version_count counts it. Renormalise rebuilds it with one
// version per distinct state, agent.transactions still shows the current state, and a verify
// afterwards reports no difference.
func TestRenormaliseOverARevisionThreeCoreDropsRepeatedVersions(t *testing.T) {
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

	// What revision 3 made of those rows: a third version repeating the first, counted. (This
	// code made none; revision 3's code made it already.)
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
	if _, err := owner.Exec(ctx, `UPDATE core.projection_state SET normaliser_rev = 3`); err != nil {
		t.Fatal(err)
	}

	if diffs := e.renormalise(true); len(diffs) == 0 {
		t.Fatal("verify over a revision-3 core with a repeated version reported no difference")
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

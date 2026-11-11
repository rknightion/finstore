package ledger_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rknightion/finstore/internal/ledger"
)

func detailJSON(t *testing.T, raw json.RawMessage, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("detail %q is not JSON: %v", raw, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Fatalf("detail = %s, want %s", gb, wb)
	}
}

// Revision 3 of the normaliser adds core.account.detail and the two pending tables.
func TestNormaliserRevisionIsThree(t *testing.T) {
	if ledger.NormaliserRev != 3 {
		t.Fatalf("NormaliserRev = %d, want 3", ledger.NormaliserRev)
	}
}

// A core built by revision 2 has no detail column and no pending tables. Renormalise rebuilds it
// with them, filled from raw, and a verify afterwards reports no difference.
func TestRenormaliseOverARevisionTwoCoreRebuildsItAndVerifyThenFindsNoDifference(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.accounts(fAcc{ID: "acc_1", Type: "current", BankRef: refA, Detail: json.RawMessage(`{"display_name":"Made Up"}`)})
	e.catchUp()
	if _, err := e.owner.DB().Exec(ctx, `DROP VIEW agent.accounts, agent.transactions, agent.balances, agent.coverage,
		agent.standing_orders, agent.direct_debits, agent.pot_transactions, agent.pot_reconciliation,
		agent.transaction_history, agent.sync_status, agent.pending_transactions CASCADE`); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP TABLE core.pending_transaction`,
		`DROP TABLE core.pending_snapshot`,
		`ALTER TABLE core.account DROP COLUMN detail`,
		`UPDATE core.projection_state SET normaliser_rev = 2`,
	} {
		if _, err := e.owner.DB().Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	if diffs := e.renormalise(true); len(diffs) == 0 {
		t.Fatal("verify over a revision-2 core reported no difference")
	}
	e.renormalise(false)
	detailJSON(t, e.account("fake-current").Detail, `{"display_name":"Made Up"}`)
	var n int
	if err := e.app.DB().QueryRow(ctx, `SELECT (SELECT count(*) FROM core.pending_snapshot) + (SELECT count(*) FROM core.pending_transaction)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("pending tables after the rebuild: %d rows, %v", n, err)
	}
	if diffs := e.renormalise(true); len(diffs) != 0 {
		t.Fatalf("verify after the rebuild: %+v", diffs)
	}
	ps, err := e.core().GetProjectionState(ctx)
	if err != nil || ps.NormaliserRev != ledger.NormaliserRev {
		t.Fatalf("projection state %+v, %v", ps, err)
	}
}

// Ending an account reads it and writes it back; its detail survives.
func TestAccountEndKeepsTheAccountDetail(t *testing.T) {
	e := newEnv(t)
	e.accounts(fAcc{ID: "acc_1", Type: "current", BankRef: refA, Detail: json.RawMessage(`{"display_name":"Made Up"}`)})
	e.catchUp()
	last := "2030-02-01"
	e.end("fake-current", &last)
	a := e.account("fake-current")
	if a.EndedOn == nil {
		t.Fatal("the account was not ended")
	}
	detailJSON(t, a.Detail, `{"display_name":"Made Up"}`)
	// A later listing that carries no detail leaves it as it was.
	e.accounts(fAcc{ID: "acc_1", Type: "current", BankRef: refA})
	e.catchUp()
	detailJSON(t, e.account("fake-current").Detail, `{"display_name":"Made Up"}`)
}

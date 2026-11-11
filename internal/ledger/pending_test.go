package ledger_test

import (
	"context"
	"strings"
	"testing"
	"time"
)

type pendingRowOut struct {
	slug, source, id string
	amount           int64
	merchant         *string
	desc             string
}

// agentPending returns agent.pending_transactions ordered by slug and id.
func (e *env) agentPending() []pendingRowOut {
	e.t.Helper()
	rows, err := e.app.DB().Query(context.Background(), `SELECT account_slug, source, transaction_id, amount_minor, merchant_name, description
		FROM agent.pending_transactions ORDER BY account_slug, transaction_id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []pendingRowOut
	for rows.Next() {
		var r pendingRowOut
		if err := rows.Scan(&r.slug, &r.source, &r.id, &r.amount, &r.merchant, &r.desc); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// pendingAsOf returns agent.accounts.pending_as_of for slug (nil when no pending read succeeded).
func (e *env) pendingAsOf(slug string) *time.Time {
	e.t.Helper()
	var at *time.Time
	if err := e.app.DB().QueryRow(context.Background(), `SELECT pending_as_of FROM agent.accounts WHERE slug = $1`, slug).Scan(&at); err != nil {
		e.t.Fatal(err)
	}
	return at
}

// pendingSnapAt stores a pending response for account holding rows and returns the time it was
// fetched.
func (e *env) pendingSnapAt(account string, rows ...fPending) time.Time {
	e.t.Helper()
	at := e.tick()
	e.store(e.fetchAs(lloydsSource, 1, account, nil), lloydsSource, "/pending", 200,
		body(fBody{Pending: []fPendingSnap{{Account: account, Rows: rows}}}), at)
	return at
}

// A snapshot replaces the account's pending rows as a whole; an empty one clears them and still
// records when nothing was pending; the agent view shows the newest rows with their account and
// source, and pending_as_of on agent.accounts says how fresh they are.
func TestPendingSnapshotReplacesThePreviousOneAndAnEmptyOneClearsIt(t *testing.T) {
	e := newEnv(t)
	e.respondAs(e.fetchAs(lloydsSource, 1, "", nil), lloydsSource, "/accounts", 200,
		body(fBody{Accounts: []fAcc{{ID: refA, Type: "joint", BankRef: refA}}}))
	e.catchUp()
	if e.pendingAsOf("lloyds-joint") != nil {
		t.Fatal("pending_as_of is set before any pending read")
	}

	first := e.pendingSnapAt(refA, fPending{ID: "p-1", Date: "2030-02-28", Amount: -750, Desc: "ONE", Seller: "Made Up"},
		fPending{ID: "p-2", Date: "2030-02-28", Amount: 200, Desc: "TWO"})
	stats := e.catchUp()
	if stats.PendingSnapshots != 1 {
		t.Errorf("PendingSnapshots = %d, want 1", stats.PendingSnapshots)
	}
	got := e.agentPending()
	if len(got) != 2 || got[0].slug != "lloyds-joint" || got[0].source != lloydsSource || got[0].id != "p-1" || got[0].amount != -750 ||
		got[0].merchant == nil || *got[0].merchant != "Made Up" || got[1].merchant != nil || got[1].amount != 200 {
		t.Fatalf("first snapshot: %+v", got)
	}
	if at := e.pendingAsOf("lloyds-joint"); at == nil || !at.Equal(first) {
		t.Fatalf("pending_as_of = %v, want %v", at, first)
	}

	second := e.pendingSnapAt(refA, fPending{ID: "p-2", Date: "2030-02-28", Amount: 200, Desc: "TWO"},
		fPending{ID: "p-3", Date: "2030-03-01", Amount: -1, Desc: "THREE"})
	e.catchUp()
	got = e.agentPending()
	if len(got) != 2 || got[0].id != "p-2" || got[1].id != "p-3" {
		t.Fatalf("second snapshot: %+v, want p-2 and p-3 (p-1 settled)", got)
	}
	if at := e.pendingAsOf("lloyds-joint"); at == nil || !at.Equal(second) {
		t.Fatalf("pending_as_of = %v, want %v", at, second)
	}

	third := e.pendingSnapAt(refA)
	e.catchUp()
	if got = e.agentPending(); len(got) != 0 {
		t.Fatalf("an empty snapshot left %+v", got)
	}
	if at := e.pendingAsOf("lloyds-joint"); at == nil || !at.Equal(third) {
		t.Fatalf("pending_as_of after an empty snapshot = %v, want %v", at, third)
	}

	// Replay rebuilds both tables: they have primary keys and hold the same rows.
	e.pendingSnapAt(refA, fPending{ID: "p-9", Date: "2030-03-01", Amount: 5, Desc: "NINE"})
	e.catchUp()
	if diffs := e.renormalise(true); len(diffs) != 0 {
		t.Fatalf("renormalise --verify: %+v", diffs)
	}
	e.renormalise(false)
	if got = e.agentPending(); len(got) != 1 || got[0].id != "p-9" {
		t.Fatalf("after a rebuild: %+v", got)
	}
}

// The agent view leaves out an ended account's pending rows, like the other views, and never shows
// a row of an account that has no pending snapshot.
func TestPendingViewLeavesOutEndedAccounts(t *testing.T) {
	e := newEnv(t)
	e.respondAs(e.fetchAs(lloydsSource, 1, "", nil), lloydsSource, "/accounts", 200,
		body(fBody{Accounts: []fAcc{{ID: refA, Type: "joint", BankRef: refA}, {ID: refB, Type: "current", BankRef: refB}}}))
	e.catchUp()
	e.pendingSnapAt(refA, fPending{ID: "a-1", Date: "2030-02-28", Amount: -1})
	e.pendingSnapAt(refB, fPending{ID: "b-1", Date: "2030-02-28", Amount: -2})
	e.catchUp()
	if got := e.agentPending(); len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	last := "2030-02-01"
	e.end("lloyds-joint", &last)
	got := e.agentPending()
	if len(got) != 1 || got[0].slug != "lloyds-current" {
		t.Fatalf("after ending lloyds-joint: %+v", got)
	}
}

// A snapshot whose row has no id is a bad record: the raw row is skipped as a whole and counted, the
// account keeps the pending rows it had, and the projection goes on. A snapshot for an account the
// ledger does not hold is skipped the same way. The rows a normaliser left out are counted and
// logged without any of their text.
func TestBadPendingSnapshotsAreSkippedWithoutStoppingTheProjection(t *testing.T) {
	e := newEnv(t)
	e.respondAs(e.fetchAs(lloydsSource, 1, "", nil), lloydsSource, "/accounts", 200,
		body(fBody{Accounts: []fAcc{{ID: refA, Type: "joint", BankRef: refA}}}))
	e.pendingSnapAt(refA, fPending{ID: "p-1", Date: "2030-02-28", Amount: -1, Desc: "KEPT"})
	e.catchUp()

	e.pendingSnapAt(refA, fPending{ID: "", Date: "2030-02-28", Amount: -2, Desc: "NO ID"})
	e.pendingSnapAt("unknown account", fPending{ID: "x", Date: "2030-02-28", Amount: -3})
	e.respondAs(e.fetchAs(lloydsSource, 1, refA, nil), lloydsSource, "/pending", 200,
		body(fBody{Pending: []fPendingSnap{{Account: refA, Skipped: 3, Rows: []fPending{{ID: "p-2", Date: "2030-02-28", Amount: -4, Desc: "SECRET WORDS"}}}}}))
	stats := e.catchUp()
	if stats.Unparseable != 2 || stats.PendingSnapshots != 1 || stats.PendingSkipped != 3 {
		t.Fatalf("stats %+v, want 2 unparseable, 1 snapshot applied, 3 rows skipped", stats)
	}
	got := e.agentPending()
	if len(got) != 1 || got[0].id != "p-2" {
		t.Fatalf("pending rows %+v, want the last good snapshot's p-2", got)
	}
	logged := e.logs.String()
	if !strings.Contains(logged, "pending transactions left out by the normaliser") || strings.Contains(logged, "SECRET WORDS") ||
		strings.Contains(logged, "NO ID") {
		t.Errorf("log lines: %s", logged)
	}
}

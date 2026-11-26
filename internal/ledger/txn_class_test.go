package ledger_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/store"
)

func TestValidateTxnClass(t *testing.T) {
	id := uuid.New().String()
	for name, tc := range map[string]struct {
		p    ledger.TxnClass
		fail bool
	}{
		"class":               {ledger.TxnClass{TxnID: id, FlowClass: ptr("own_external")}, false},
		"borrowing":           {ledger.TxnClass{TxnID: id, FlowClass: ptr("borrowing")}, false},
		"income employer":     {ledger.TxnClass{TxnID: id, FlowClass: ptr("income"), Employer: true}, false},
		"clear":               {ledger.TxnClass{TxnID: id}, false},
		"employer not income": {ledger.TxnClass{TxnID: id, FlowClass: ptr("spending"), Employer: true}, true},
		"employer on a clear": {ledger.TxnClass{TxnID: id, Employer: true}, true},
		"unknown class":       {ledger.TxnClass{TxnID: id, FlowClass: ptr("bogus")}, true},
		"not a uuid":          {ledger.TxnClass{TxnID: "tx_made_up", FlowClass: ptr("income")}, true},
		"missing id":          {ledger.TxnClass{FlowClass: ptr("income")}, true},
	} {
		if err := ledger.ValidateTxnClass(tc.p); (err != nil) != tc.fail {
			t.Errorf("%s: error %v, want failure %v", name, err, tc.fail)
		}
	}
	// A refusal never echoes the id it was given.
	secret := "Made Up Id 12"
	if err := ledger.ValidateTxnClass(ledger.TxnClass{TxnID: secret, FlowClass: ptr("income")}); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("refusal %v must exist and not echo its input", err)
	}
}

// A txn_class entry is normalised into core.txn_class with the entry's seq and time; a later entry
// for the transaction replaces it, and a clear removes it. Another transaction's row is untouched.
func TestTxnClassEntriesSetReplaceAndClear(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, b := uuid.New(), uuid.New()
	set := func(id uuid.UUID, class string, employer bool) int64 {
		e.operator(ledger.EntryTxnClass, ledger.TxnClass{TxnID: id.String(), FlowClass: &class, Employer: employer})
		seq := e.lastEntry()
		if st := e.catchUp(); st.Unparseable != 0 {
			t.Fatalf("%d unparseable", st.Unparseable)
		}
		return seq
	}
	seqA := set(a, "income", true)
	set(b, "own_external", false)
	row, err := e.core().GetTxnClass(ctx, a)
	if err != nil || row.FlowClass != "income" || !row.Employer || row.EntrySeq != seqA || row.EnteredAt.IsZero() {
		t.Fatalf("row %+v, %v", row, err)
	}
	seqA = set(a, "spending", false)
	if row, err = e.core().GetTxnClass(ctx, a); err != nil || row.FlowClass != "spending" || row.Employer || row.EntrySeq != seqA {
		t.Fatalf("a later entry did not replace the earlier: %+v, %v", row, err)
	}
	e.operator(ledger.EntryTxnClass, ledger.TxnClass{TxnID: a.String()})
	if st := e.catchUp(); st.Unparseable != 0 {
		t.Fatalf("%d unparseable after a clear", st.Unparseable)
	}
	if _, err := e.core().GetTxnClass(ctx, a); err == nil {
		t.Fatal("the clear left the row")
	}
	if row, err = e.core().GetTxnClass(ctx, b); err != nil || row.FlowClass != "own_external" {
		t.Fatalf("another transaction's row changed: %+v, %v", row, err)
	}
	// Clearing what is not set changes nothing and is not an error.
	e.operator(ledger.EntryTxnClass, ledger.TxnClass{TxnID: uuid.NewString()})
	if st := e.catchUp(); st.Unparseable != 0 {
		t.Fatalf("%d unparseable after clearing nothing", st.Unparseable)
	}
	// Invalid entries are skipped as unparseable and write no row.
	e.operator(ledger.EntryTxnClass, ledger.TxnClass{TxnID: "not-a-uuid", FlowClass: ptr("income")})
	e.operator(ledger.EntryTxnClass, ledger.TxnClass{TxnID: uuid.NewString(), FlowClass: ptr("spending"), Employer: true})
	if st := e.catchUp(); st.Unparseable != 2 {
		t.Fatalf("unparseable = %d, want 2", st.Unparseable)
	}
	var n int
	if err := e.app.DB().QueryRow(ctx, `SELECT count(*) FROM core.txn_class`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("core.txn_class has %d rows, want 1 (%v)", n, err)
	}
}

// A rebuild from raw gives the same core.txn_class as the live run: the entry survives renormalise.
func TestTxnClassSurvivesReplay(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	scenario(e)
	kept, cleared := uuid.New(), uuid.New()
	e.operator(ledger.EntryTxnClass, ledger.TxnClass{TxnID: kept.String(), FlowClass: ptr("income"), Employer: true})
	e.operator(ledger.EntryTxnClass, ledger.TxnClass{TxnID: cleared.String(), FlowClass: ptr("refund")})
	e.operator(ledger.EntryTxnClass, ledger.TxnClass{TxnID: cleared.String()})
	e.catchUp()
	err := e.owner.HoldWriterLock(ctx, store.HolderTest, func(ctx context.Context, conn *pgx.Conn) error {
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
		var class string
		var employer bool
		var n int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM core_next.txn_class`).Scan(&n); err != nil || n != 1 {
			t.Errorf("core_next.txn_class has %d rows, want 1 (%v)", n, err)
		}
		if err := conn.QueryRow(ctx, `SELECT flow_class, employer FROM core_next.txn_class WHERE txn_id = $1`, kept).Scan(&class, &employer); err != nil ||
			class != "income" || !employer {
			t.Errorf("core_next.txn_class row %q %v, %v", class, employer, err)
		}
		return store.DropCoreNext(ctx, conn)
	})
	if err != nil {
		t.Fatal(err)
	}
}

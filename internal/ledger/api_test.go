package ledger_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
)

var jan5 = time.Date(2030, time.January, 5, 10, 0, 0, 0, time.UTC)

func TestChangedTransactionAddsAVersionAndUnchangedAddsNone(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.accounts(fAcc{ID: "acc_1", Type: "current", Opened: "2029-01-01"})
	f := e.fetch(2, "acc_1", nil)
	e.respond(f, "/transactions", body(fBody{Transactions: []json.RawMessage{txn("tx_1", "acc_1", -100, jan5)}}))
	st := e.catchUp()
	if st.TransactionsCreated != 1 || st.AccountsCreated != 1 || st.RowsProcessed != 4 {
		t.Fatalf("first catch-up stats %+v", st)
	}

	accID := wantAccountID(fakeSource, "acc_1")
	txID := wantTxnID(accID, fakeSource, "tx_1")
	if got := e.account("fake-current"); got.ID != accID || got.Feed != "api" {
		t.Fatalf("account %+v, want id %s feed api", got, accID)
	}
	if got := e.txn(txID); got.VersionCount != 1 || got.AmountMinor != -100 {
		t.Fatalf("transaction %+v", got)
	}

	// The same content again: no version, only last_seen_at moves.
	e.respond(e.fetch(3, "acc_1", nil), "/transactions", body(fBody{Transactions: []json.RawMessage{txn("tx_1", "acc_1", -100, jan5)}}))
	if st := e.catchUp(); st.TransactionsCreated != 0 || st.TransactionsChanged != 0 {
		t.Fatalf("unchanged catch-up stats %+v", st)
	}
	if got := e.txn(txID); got.VersionCount != 1 || !got.LastSeenAt.Equal(e.clock) {
		t.Fatalf("after unchanged response: %+v (want last seen %s)", got, e.clock)
	}

	// Changed content: a second version, the row mirrors it, the running sum follows it.
	e.respond(e.fetch(4, "acc_1", nil), "/transactions", body(fBody{Transactions: []json.RawMessage{txn("tx_1", "acc_1", -150, jan5)}}))
	if st := e.catchUp(); st.TransactionsChanged != 1 {
		t.Fatalf("changed catch-up stats %+v", st)
	}
	got := e.txn(txID)
	versions, err := e.core().ListTransactionVersions(ctx, txID)
	if err != nil {
		t.Fatal(err)
	}
	if got.VersionCount != 2 || got.AmountMinor != -150 || len(versions) != 2 || versions[1].RawSeq != e.lastSeq() {
		t.Fatalf("after change: %+v, %d versions", got, len(versions))
	}
	if r := e.recon(accID); r.RunningSumMinor != -150 || r.Method != "monzo_absolute" {
		t.Fatalf("reconciliation %+v", r)
	}
	if e.lastSeq() != e.maxRawSeq() {
		t.Fatalf("last_seq %d, raw max %d", e.lastSeq(), e.maxRawSeq())
	}
}

func TestSlugsFollowOpeningOrderAndNeverChange(t *testing.T) {
	e := newEnv(t)
	// Listed out of opening order, with source keys that sort against it.
	e.accounts(fAcc{ID: "acc_a", Type: "current", Opened: "2025-06-01"}, fAcc{ID: "acc_z", Type: "current", Opened: "2021-01-01"},
		fAcc{ID: "acc_j", Type: "joint", Opened: "2022-01-01"})
	e.catchUp()
	want := map[string]string{"fake-current": "acc_z", "fake-current-2": "acc_a", "fake-joint": "acc_j"}
	for slug, key := range want {
		if got := e.account(slug); got.ID != wantAccountID(fakeSource, key) {
			t.Fatalf("%s is %s, want %s", slug, got.SourceKey, key)
		}
	}
	// An account opened before both, seen later, takes the next suffix; nobody is renamed, even
	// when the list is seen again.
	e.accounts(fAcc{ID: "acc_a", Type: "current", Opened: "2025-06-01"}, fAcc{ID: "acc_m", Type: "current", Opened: "2019-01-01"},
		fAcc{ID: "acc_z", Type: "current", Opened: "2021-01-01", Closed: true})
	if st := e.catchUp(); st.AccountsCreated != 1 {
		t.Fatalf("stats %+v", st)
	}
	want["fake-current-3"] = "acc_m"
	for slug, key := range want {
		if got := e.account(slug); got.ID != wantAccountID(fakeSource, key) {
			t.Fatalf("%s is %s, want %s", slug, got.SourceKey, key)
		}
	}
	if !e.account("fake-current").Closed {
		t.Fatal("a re-listed account did not take its new closed state")
	}
}

func TestMissingFromCompletedFetchDisappearsAndReturns(t *testing.T) {
	e := newEnv(t)
	jan := &period{time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2030, 2, 1, 0, 0, 0, 0, time.UTC)}
	e.accounts(fAcc{ID: "acc_1", Type: "current"})
	tx1 := txn("tx_1", "acc_1", -100, jan5)
	tx2 := txn("tx_2", "acc_1", -50, jan5.Add(24*time.Hour))
	outside := txn("tx_3", "acc_1", -7, time.Date(2030, 3, 1, 0, 0, 0, 0, time.UTC))
	f := e.fetch(1, "acc_1", jan)
	e.respond(f, "/transactions", body(fBody{Transactions: []json.RawMessage{tx1, tx2}}))
	e.complete(f)
	e.respond(e.fetch(1, "acc_1", nil), "/transactions", body(fBody{Transactions: []json.RawMessage{outside}}))
	e.catchUp()
	accID := wantAccountID(fakeSource, "acc_1")
	tx2ID := wantTxnID(accID, fakeSource, "tx_2")
	if r := e.recon(accID); r.RunningSumMinor != -157 {
		t.Fatalf("running sum %d, want -157", r.RunningSumMinor)
	}

	// Missing from a fetch that never completed: proves nothing.
	cut := e.fetch(2, "acc_1", jan)
	e.respond(cut, "/transactions", body(fBody{Transactions: []json.RawMessage{tx1}}))
	if st := e.catchUp(); st.Disappeared != 0 {
		t.Fatalf("incomplete fetch disappeared rows: %+v", st)
	}
	if got := e.txn(tx2ID); got.DisappearedAt != nil {
		t.Fatalf("incomplete fetch set disappeared_at: %+v", got)
	}

	// Missing from a completed fetch of its period: disappears and leaves the sum. The row
	// outside the period is untouched.
	done := e.fetch(3, "acc_1", jan)
	e.respond(done, "/transactions", body(fBody{Transactions: []json.RawMessage{tx1}}))
	at := e.complete(done)
	if st := e.catchUp(); st.Disappeared != 1 {
		t.Fatalf("stats %+v", st)
	}
	if got := e.txn(tx2ID); got.DisappearedAt == nil || !got.DisappearedAt.Equal(at) {
		t.Fatalf("disappeared_at %v, want %s", got.DisappearedAt, at)
	}
	if got := e.txn(wantTxnID(accID, fakeSource, "tx_3")); got.DisappearedAt != nil {
		t.Fatal("a transaction outside the fetch's period disappeared")
	}
	if r := e.recon(accID); r.RunningSumMinor != -107 {
		t.Fatalf("running sum %d after disappearance, want -107", r.RunningSumMinor)
	}

	// Returned by a later completed fetch: cleared and back in the sum.
	back := e.fetch(4, "acc_1", jan)
	e.respond(back, "/transactions", body(fBody{Transactions: []json.RawMessage{tx1, tx2}}))
	e.complete(back)
	if st := e.catchUp(); st.Returned != 1 {
		t.Fatalf("stats %+v", st)
	}
	if got := e.txn(tx2ID); got.DisappearedAt != nil {
		t.Fatalf("returned transaction still disappeared: %+v", got)
	}
	if r := e.recon(accID); r.RunningSumMinor != -157 {
		t.Fatalf("running sum %d after return, want -157", r.RunningSumMinor)
	}
}

func TestUnparseableRowsAreCountedSkippedWholeAndDoNotStopTheRun(t *testing.T) {
	e := newEnv(t)
	e.accounts(fAcc{ID: "acc_1", Type: "current"})
	f := e.fetch(1, "acc_1", nil)
	e.respond(f, "/transactions", "<html>not json</html>")
	// One known and one unknown account: the whole row is skipped, so the known one is not stored.
	e.respond(f, "/transactions", body(fBody{Transactions: []json.RawMessage{
		txn("tx_ok", "acc_1", -1, jan5), txn("tx_bad", "acc_unknown", -2, jan5)}}))
	e.respondAs(f, "unregistered-source", "/transactions", 200, "{}")
	e.respond(f, "/transactions", body(fBody{Transactions: []json.RawMessage{txn("tx_after", "acc_1", -3, jan5)}}))
	st := e.catchUp()
	if st.Unparseable != 3 || st.TransactionsCreated != 1 {
		t.Fatalf("stats %+v", st)
	}
	accID := wantAccountID(fakeSource, "acc_1")
	if n := e.countTxns(accID); n != 1 {
		t.Fatalf("%d transactions stored, want only tx_after", n)
	}
	if r := e.recon(accID); r.RunningSumMinor != -3 {
		t.Fatalf("running sum %d, want -3", r.RunningSumMinor)
	}
	if e.lastSeq() != e.maxRawSeq() {
		t.Fatalf("last_seq %d did not reach %d", e.lastSeq(), e.maxRawSeq())
	}

	// Every skip was logged, with allow-listed fields only.
	lines := 0
	sc := bufio.NewScanner(e.logs)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		for k := range m {
			if !semconv.IsLogField(k) {
				t.Errorf("log field %q is not allow-listed", k)
			}
		}
		lines++
	}
	if lines != 3 {
		t.Fatalf("%d log lines, want 3", lines)
	}
}

func TestBalanceCheckNeedsTwoEqualReadingsInOneRun(t *testing.T) {
	e := newEnv(t)
	e.accounts(fAcc{ID: "acc_1", Type: "current"})
	e.respond(e.fetch(1, "acc_1", nil), "/transactions", body(fBody{Transactions: []json.RawMessage{
		txn("tx_1", "acc_1", 1000, jan5), txn("tx_2", "acc_1", -250, jan5)}}))
	e.balance(5, "acc_1", 750) // the only reading of run 5
	e.balance(6, "acc_1", 750) // the first reading of run 6
	e.catchUp()
	accID := wantAccountID(fakeSource, "acc_1")
	if r := e.recon(accID); r.LastCheckAt != nil || r.LastCheckOK != nil {
		t.Fatalf("checked without two equal readings in one run: %+v", r)
	}

	at := e.balance(6, "acc_1", 750)
	e.catchUp()
	r := e.recon(accID)
	if r.LastCheckOK == nil || !*r.LastCheckOK || *r.LastDiffMinor != 0 || !r.LastCheckAt.Equal(at) || !r.LastPassAt.Equal(at) {
		t.Fatalf("equal balance did not pass: %+v", r)
	}
}

func TestBalanceCheckFailsWithBalanceMinusSum(t *testing.T) {
	e := newEnv(t)
	e.accounts(fAcc{ID: "acc_1", Type: "current"})
	e.respond(e.fetch(1, "acc_1", nil), "/transactions", body(fBody{Transactions: []json.RawMessage{txn("tx_1", "acc_1", 1000, jan5)}}))
	e.balance(7, "acc_1", 1253)
	at := e.balance(7, "acc_1", 1253)
	e.catchUp()
	r := e.recon(wantAccountID(fakeSource, "acc_1"))
	if r.LastCheckOK == nil || *r.LastCheckOK || r.LastDiffMinor == nil || *r.LastDiffMinor != 253 ||
		!r.LastCheckAt.Equal(at) || r.LastPassAt != nil {
		t.Fatalf("mismatch not recorded as a failure of 253: %+v", r)
	}
}

func TestDifferingReadingsLeaveReconciliationUnchanged(t *testing.T) {
	e := newEnv(t)
	e.accounts(fAcc{ID: "acc_1", Type: "current"})
	e.respond(e.fetch(1, "acc_1", nil), "/transactions", body(fBody{Transactions: []json.RawMessage{txn("tx_1", "acc_1", 500, jan5)}}))
	e.balance(2, "acc_1", 500)
	e.balance(2, "acc_1", 500)
	e.catchUp()
	accID := wantAccountID(fakeSource, "acc_1")
	before := e.recon(accID)
	if before.LastCheckOK == nil || !*before.LastCheckOK {
		t.Fatalf("setup: %+v", before)
	}
	e.balance(3, "acc_1", 500)
	e.balance(3, "acc_1", 620)
	e.catchUp()
	if after := e.recon(accID); !reflect.DeepEqual(after, before) {
		t.Fatalf("differing readings changed the state:\n%+v\n%+v", before, after)
	}
}

func TestClosedAccountIsCheckedOnOneReading(t *testing.T) {
	e := newEnv(t)
	e.accounts(fAcc{ID: "acc_old", Type: "prepaid", Closed: true})
	e.respond(e.fetch(1, "acc_old", nil), "/transactions", body(fBody{Transactions: []json.RawMessage{txn("tx_1", "acc_old", 300, jan5)}}))
	at := e.balance(1, "acc_old", 0)
	e.catchUp()
	r := e.recon(wantAccountID(fakeSource, "acc_old"))
	if r.LastCheckAt == nil || !r.LastCheckAt.Equal(at) || *r.LastDiffMinor != -300 || *r.LastCheckOK {
		t.Fatalf("closed account not checked on a single reading: %+v", r)
	}
}

func TestPotIsBalancesOnlyAndNeverChecked(t *testing.T) {
	e := newEnv(t)
	e.accounts(fAcc{ID: "acc_1", Type: "current"}, fAcc{ID: "pot_1", Type: "pot", Parent: "acc_1"})
	e.balance(1, "pot_1", 4000)
	e.balance(1, "pot_1", 4000)
	e.catchUp()
	potID := wantAccountID(fakeSource, "pot_1")
	pot := e.account("fake-pot")
	if pot.ID != potID || pot.ParentID == nil || *pot.ParentID != wantAccountID(fakeSource, "acc_1") {
		t.Fatalf("pot %+v", pot)
	}
	r := e.recon(potID)
	if r.Method != "balances_only" || r.LastCheckAt != nil || r.LastCheckOK != nil {
		t.Fatalf("pot reconciliation %+v", r)
	}
	snap, err := e.core().LatestBalanceSnapshot(context.Background(), potID)
	if err != nil || snap.BalanceMinor != 4000 {
		t.Fatalf("pot snapshot %+v, %v", snap, err)
	}
}

func TestAcceptedDifferencePassesOnlyThatDifference(t *testing.T) {
	e := newEnv(t)
	e.accounts(fAcc{ID: "acc_1", Type: "current", Closed: true})
	e.respond(e.fetch(1, "acc_1", nil), "/transactions", body(fBody{Transactions: []json.RawMessage{txn("tx_1", "acc_1", 1000, jan5)}}))
	at := e.balance(1, "acc_1", 1253)
	e.catchUp()
	accID := wantAccountID(fakeSource, "acc_1")
	if r := e.recon(accID); *r.LastCheckOK {
		t.Fatalf("setup: %+v", r)
	}

	e.operator(ledger.EntryAcceptDifference, ledger.AcceptDifference{Slug: "fake-current", DiffMinor: ptr(int64(253)), Reason: "made-up reason"})
	e.catchUp()
	r := e.recon(accID)
	if !*r.LastCheckOK || r.KnownException == nil || *r.KnownException != "made-up reason" ||
		r.AcceptedDiffMinor == nil || *r.AcceptedDiffMinor != 253 || r.LastPassAt == nil || !r.LastPassAt.Equal(at) {
		t.Fatalf("accepted difference did not make the last check pass: %+v", r)
	}

	e.balance(2, "acc_1", 1254)
	e.catchUp()
	r = e.recon(accID)
	if *r.LastCheckOK || *r.LastDiffMinor != 254 || r.KnownException == nil {
		t.Fatalf("a different difference passed: %+v", r)
	}

	at = e.balance(3, "acc_1", 1253)
	e.catchUp()
	if r = e.recon(accID); !*r.LastCheckOK || !r.LastPassAt.Equal(at) || r.KnownException == nil {
		t.Fatalf("the accepted difference did not pass again: %+v", r)
	}
}

func TestAccountAddCreatesFileAccountAndRebuildRecreatesIt(t *testing.T) {
	e := newEnv(t)
	e.operator(ledger.EntryAccountAdd, ledger.AccountAdd{Slug: "bank-current", Type: "current", Currency: "GBP",
		Name: "Made-up bank", ExpectsImports: true})
	e.operator(ledger.EntryAccountAdd, ledger.AccountAdd{Slug: "bank-current", Type: "savings", Currency: "EUR", Name: "Duplicate"})
	if st := e.catchUp(); st.AccountsCreated != 1 || st.Unparseable != 0 {
		t.Fatalf("stats %+v", st)
	}
	want := store.Account{ID: wantAccountID(ledger.SourceManual, "bank-current"), Source: "manual", SourceKey: "bank-current",
		Slug: "bank-current", Type: "current", Feed: "file", Currency: "GBP", Name: "Made-up bank", ExpectsImports: true}
	if got := e.account("bank-current"); !reflect.DeepEqual(got, want) {
		t.Fatalf("account\n got %+v\nwant %+v", got, want)
	}
	if r := e.recon(want.ID); r.Method != "file_continuity" {
		t.Fatalf("method %s", r.Method)
	}

	if diffs := e.renormalise(false); diffs != nil {
		t.Fatalf("swap returned diffs %+v", diffs)
	}
	if got := e.account("bank-current"); !reflect.DeepEqual(got, want) {
		t.Fatalf("after rebuild\n got %+v\nwant %+v", got, want)
	}
}

func (e *env) fetchPeriod(fetchID int64) (store.FetchPeriod, bool) {
	e.t.Helper()
	p, err := e.core().GetFetchPeriod(context.Background(), fetchID)
	if errors.Is(err, store.ErrNotFound) {
		return store.FetchPeriod{}, false
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return p, true
}

// An hourly fetch's period starts part-way through a day. The period is recorded as instants, so
// a row created earlier that day, which the fetch did not list, is not marked disappeared.
func TestCompletedPeriodIsRecordedAsInstantsAndAMidDayStartLeavesEarlierRowsAlone(t *testing.T) {
	e := newEnv(t)
	e.accounts(fAcc{ID: "acc_1", Type: "current"})
	morning := time.Date(2030, 1, 5, 8, 0, 0, 0, time.UTC)
	e.respond(e.fetch(1, "acc_1", nil), "/transactions", body(fBody{Transactions: []json.RawMessage{txn("tx_morning", "acc_1", -100, morning)}}))
	hour := &period{time.Date(2030, 1, 5, 14, 0, 0, 0, time.UTC), time.Date(2030, 1, 5, 15, 0, 0, 0, time.UTC)}
	f := e.fetch(2, "acc_1", hour)
	e.respond(f, "/transactions", body(fBody{Transactions: []json.RawMessage{txn("tx_afternoon", "acc_1", -5, hour.start.Add(time.Minute))}}))
	e.complete(f)
	e.catchUp()
	p, ok := e.fetchPeriod(f)
	if !ok || !p.PeriodStart.Equal(hour.start) || !p.PeriodEnd.Equal(hour.end) || p.AccountID != wantAccountID(fakeSource, "acc_1") {
		t.Fatalf("fetch period %+v (recorded %v), want the raw period unchanged", p, ok)
	}
	if got := e.txn(wantTxnID(wantAccountID(fakeSource, "acc_1"), fakeSource, "tx_morning")); got.DisappearedAt != nil {
		t.Fatalf("a row created earlier in the day was marked disappeared: %+v", got)
	}
	// A fetch with no period records none.
	g := e.fetch(3, "acc_1", nil)
	e.complete(g)
	e.catchUp()
	if _, ok := e.fetchPeriod(g); ok {
		t.Fatal("a fetch with no period recorded one")
	}
}

// For a source whose history may start later than asked (TrueLayer), the completed period starts
// at the earliest row returned, so a shorter history on a later consent marks no older row
// disappeared; a response with no rows covers the period asked for.
func TestDateOnlySourcePeriodStartsAtItsEarliestRow(t *testing.T) {
	e := lloydsEnv(t)
	acc := wantAccountID(lloydsSource, refA)
	asked := &period{time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2030, 4, 1, 0, 0, 0, 0, time.UTC)}
	first := e.fetchAs(lloydsSource, 2, refA, asked)
	e.respondAs(first, lloydsSource, "/transactions", 200, body(fBody{Transactions: []json.RawMessage{
		lloydsRow("old", "2030-01-15", -100, "0"), lloydsRow("new", "2030-02-10", -100, "0")}}))
	e.complete(first)
	e.catchUp()
	if p, ok := e.fetchPeriod(first); !ok || !p.PeriodStart.Equal(date("2030-01-15")) || !p.PeriodEnd.Equal(asked.end) {
		t.Fatalf("first period %+v", p)
	}

	// A later consent returns history from February only.
	shorter := e.fetchAs(lloydsSource, 3, refA, asked)
	e.respondAs(shorter, lloydsSource, "/transactions", 200, body(fBody{Transactions: []json.RawMessage{
		lloydsRow("new", "2030-02-10", -100, "0")}}))
	e.complete(shorter)
	if st := e.catchUp(); st.Disappeared != 0 {
		t.Fatalf("a shorter history marked rows disappeared: %+v", st)
	}
	if p, ok := e.fetchPeriod(shorter); !ok || !p.PeriodStart.Equal(date("2030-02-10")) {
		t.Fatalf("shorter period %+v", p)
	}
	if got := e.txn(wantTxnID(acc, lloydsSource, "old")); got.DisappearedAt != nil {
		t.Fatalf("the older row disappeared: %+v", got)
	}

	// An empty account's response covers what was asked for.
	empty := e.fetchAs(lloydsSource, 4, refA, asked)
	e.respondAs(empty, lloydsSource, "/transactions", 200, body(fBody{}))
	e.complete(empty)
	e.catchUp()
	if p, ok := e.fetchPeriod(empty); !ok || !p.PeriodStart.Equal(asked.start) {
		t.Fatalf("empty period %+v", p)
	}
}

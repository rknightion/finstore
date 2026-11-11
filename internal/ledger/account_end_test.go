package ledger_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/store"
)

// The bank reference the made-up Halifax statements below carry.
const refHalifax = "112233 12345678"

// cutOver is a statement account holding January to March and an API account of another source
// with the same bank reference holding February onwards: the overlap an account_end resolves.
func cutOver(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.addFileAccount("halifax-joint")
	for _, m := range []struct {
		month   time.Month
		opening int64
	}{{time.January, 40000}, {time.February, 50000}, {time.March, 60000}} {
		e.importFile("halifax-joint", "halifax-pdf", halifaxPDF(m.month, m.opening, halifaxRows(m.month), "11-22-33", "12345678"))
	}
	e.respondAs(e.fetchAs(lloydsSource, 1, "", nil), lloydsSource, "/accounts", 200,
		body(fBody{Accounts: []fAcc{{ID: refHalifax, Type: "joint", BankRef: refHalifax}}}))
	e.lloydsRowsFor(refHalifax,
		lloydsRowOn(refHalifax, "l1", "2030-02-03", 12000, "620.00"),
		lloydsRowOn(refHalifax, "l2", "2030-02-20", -2000, "600.00"))
	e.catchUp()
	return e
}

func lloydsRowOn(account, id, day string, amount int64, after string) json.RawMessage {
	b, _ := json.Marshal(fTxn{ID: id, Account: account, Amount: amount, Created: day + "T00:00:00Z",
		RunningBalance: &fRunning{Amount: json.Number(after), Currency: "GBP"}})
	return b
}

func (e *env) lloydsRowsFor(account string, rows ...json.RawMessage) {
	e.t.Helper()
	e.respondAs(e.fetchAs(lloydsSource, 1, account, nil), lloydsSource, "/transactions", 200, body(fBody{Transactions: rows}))
}

// agentDates returns the transaction dates agent.transactions shows for slug.
func (e *env) agentDates(slug string) []time.Time {
	e.t.Helper()
	rows, err := e.app.DB().Query(context.Background(),
		"SELECT txn_date FROM agent.transactions WHERE account_slug = $1 ORDER BY txn_date", slug)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) end(slug string, lastDate *string) {
	e.t.Helper()
	e.operator(ledger.EntryAccountEnd, ledger.AccountEnd{Slug: slug, LastDate: lastDate})
	e.catchUp()
}

func (e *env) coverageRow(slug string) store.CoverageRow {
	e.t.Helper()
	rows, err := store.Coverage(context.Background(), e.app.DB())
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range rows {
		if r.AccountSlug == slug {
			return r
		}
	}
	e.t.Fatalf("no coverage row for %s", slug)
	return store.CoverageRow{}
}

func hasGap(r store.CoverageRow, reason string) bool {
	for _, g := range r.Gaps {
		if g.Reason == reason {
			return true
		}
	}
	return false
}

// Two accounts of different sources with one bank reference and neither ended are both a
// mismatch; ending one clears both, and clearing the end brings the report back.
func TestSharedBankRefWithoutAnEndIsAMismatchUntilOneAccountEnds(t *testing.T) {
	e := cutOver(t)
	halifax, lloyds := e.account("halifax-joint"), e.account("lloyds-joint")
	for _, a := range []store.Account{halifax, lloyds} {
		if r := e.recon(a.ID); r.LastCheckOK == nil || *r.LastCheckOK || r.KnownException == nil || *r.KnownException != "shared_bank_ref" {
			t.Fatalf("%s reconciliation %+v, want the shared bank reference reported", a.Slug, r)
		}
		if s := e.coverageState(a.Slug); s != "mismatch" {
			t.Fatalf("%s coverage state %q", a.Slug, s)
		}
	}
	if !hasGap(e.coverageRow("halifax-joint"), store.GapSharedBankRef+"lloyds-joint") ||
		!hasGap(e.coverageRow("lloyds-joint"), store.GapSharedBankRef+"halifax-joint") {
		t.Fatal("coverage does not name the account the reference is shared with")
	}

	e.end("halifax-joint", ptr("2030-01-31"))
	for _, a := range []store.Account{halifax, lloyds} {
		if r := e.recon(a.ID); r.KnownException != nil {
			t.Fatalf("%s still reported after the end: %+v", a.Slug, r)
		}
	}
	if s := e.coverageState("halifax-joint"); s != "reconciled" {
		t.Fatalf("halifax-joint coverage state %q after the end", s)
	}
	if hasGap(e.coverageRow("lloyds-joint"), store.GapSharedBankRef+"halifax-joint") {
		t.Fatal("lloyds-joint still reports the shared reference")
	}

	// A later balance check on the API account does not bring back a report that no longer holds;
	// clearing the end does.
	e.end("halifax-joint", nil)
	if r := e.recon(lloyds.ID); r.KnownException == nil || *r.KnownException != "shared_bank_ref" || *r.LastCheckOK {
		t.Fatalf("lloyds-joint after the end was cleared: %+v", r)
	}
	// While it holds, a passing balance check does not hide it.
	e.respondAs(e.fetchAs(lloydsSource, 9, refHalifax, nil), lloydsSource, "/balance", 200,
		body(fBody{Balances: []fBal{{Account: refHalifax, Balance: 60000}}}))
	e.catchUp()
	if r := e.recon(lloyds.ID); r.LastCheckOK == nil || *r.LastCheckOK {
		t.Fatalf("a balance check hid the shared reference: %+v", r)
	}
}

// After account_end, the statement account's rows after the date are kept in core but left out
// of the agent views, so the days the API account holds appear once.
func TestEndedAccountsLaterRowsAreHiddenAndALaterEntryReplacesTheDate(t *testing.T) {
	e := cutOver(t)
	halifax := e.account("halifax-joint")
	if n := len(e.agentDates("halifax-joint")); n != 6 {
		t.Fatalf("before the end agent.transactions shows %d halifax rows, want 6", n)
	}
	e.end("halifax-joint", ptr("2030-01-31"))
	if a := e.account("halifax-joint"); a.EndedOn == nil || !sameDay(a.EndedOn, date("2030-01-31")) {
		t.Fatalf("ended_on %v", a.EndedOn)
	}
	if d := e.agentDates("halifax-joint"); len(d) != 2 || d[1].After(date("2030-01-31")) {
		t.Fatalf("after the end agent.transactions shows halifax rows on %v", d)
	}
	if n := e.countTxns(halifax.ID); n != 6 {
		t.Fatalf("core lost rows: %d, want 6", n)
	}
	var balances int
	if err := e.app.DB().QueryRow(context.Background(), `SELECT count(*) FROM agent.balances
		WHERE account_slug = 'halifax-joint' AND on_date > '2030-01-31'`).Scan(&balances); err != nil || balances != 0 {
		t.Fatalf("agent.balances shows %d halifax readings after the end, %v", balances, err)
	}
	if d := e.agentDates("lloyds-joint"); len(d) != 2 {
		t.Fatalf("lloyds-joint rows %v", d)
	}

	// A later entry for the same account replaces the date.
	e.end("halifax-joint", ptr("2030-02-28"))
	if d := e.agentDates("halifax-joint"); len(d) != 4 {
		t.Fatalf("after moving the end agent.transactions shows halifax rows on %v", d)
	}
	e.end("halifax-joint", nil)
	if a := e.account("halifax-joint"); a.EndedOn != nil {
		t.Fatalf("cleared end still set: %v", a.EndedOn)
	}
	if n := len(e.agentDates("halifax-joint")); n != 6 {
		t.Fatalf("after clearing the end agent.transactions shows %d halifax rows", n)
	}
	if diffs := e.renormalise(true); len(diffs) != 0 {
		t.Fatalf("rebuild differs: %+v", diffs)
	}
}

// An ended statement account takes statements after its end without a continuity gap. (That an
// ended account is never import due is shown in internal/store, with dates in the past: these
// statements are dated in the future.)
func TestEndedStatementAccountRecordsNoGapAfterItsEnd(t *testing.T) {
	e := newEnv(t)
	e.operator(ledger.EntryAccountAdd, ledger.AccountAdd{Slug: "halifax-joint", Type: "joint", Currency: "GBP", ExpectsImports: true})
	e.end("halifax-joint", ptr("2030-01-31"))
	e.importFile("halifax-joint", "halifax-pdf", halifaxPDF(time.January, 40000, halifaxRows(time.January), "11-22-33", "12345678"))
	// March after a missing February, with a balance that does not follow: a gap for an open
	// account, nothing for one that ended in January.
	e.importFile("halifax-joint", "halifax-pdf", halifaxPDF(time.March, 99900, halifaxRows(time.March), "11-22-33", "12345678"))
	e.catchUp()
	if g := e.gaps("halifax-joint"); len(g) != 0 {
		t.Fatalf("gaps %+v", g)
	}
	r := e.recon(wantAccountID(ledger.SourceManual, "halifax-joint"))
	if r.BreakCount != 0 || r.LastCheckOK == nil || !*r.LastCheckOK || !sameDay(r.ReconciledThrough, date("2030-01-31")) {
		t.Fatalf("reconciliation %+v", r)
	}
	if row := e.coverageRow("halifax-joint"); row.ReconciliationState != "reconciled" || len(row.Gaps) != 0 {
		t.Fatalf("coverage %+v", row)
	}

	// Clearing the end brings the gap back.
	e.end("halifax-joint", nil)
	if g := e.gaps("halifax-joint"); len(g) != 1 {
		t.Fatalf("gaps after clearing the end %+v", g)
	}
	if s := e.coverageState("halifax-joint"); s != "mismatch" {
		t.Fatalf("coverage state %q after clearing the end", s)
	}
}

func TestAccountEndForAnUnknownAccountOrABadDateIsSkipped(t *testing.T) {
	e := newEnv(t)
	e.operator(ledger.EntryAccountEnd, ledger.AccountEnd{Slug: "nobody", LastDate: ptr("2030-01-31")})
	e.addFileAccount("bank-joint")
	e.operator(ledger.EntryAccountEnd, ledger.AccountEnd{Slug: "bank-joint", LastDate: ptr("31/01/2030")})
	if st := e.catchUp(); st.Unparseable != 2 {
		t.Fatalf("stats %+v", st)
	}
	if a := e.account("bank-joint"); a.EndedOn != nil {
		t.Fatalf("a bad entry ended the account: %v", a.EndedOn)
	}
}

// An accepted difference's reason is replaced by the shared bank reference report while that
// holds, and comes back when it clears, live and on a rebuild.
func TestAcceptedReasonReturnsWhenASharedBankRefClears(t *testing.T) {
	e := newEnv(t)
	e.accounts(fAcc{ID: "acc_1", Type: "current", BankRef: refA})
	e.balance(2, "acc_1", 5)
	e.balance(2, "acc_1", 5)
	e.operator(ledger.EntryAcceptDifference, ledger.AcceptDifference{Slug: "fake-current", DiffMinor: ptr(int64(5)), Reason: "made-up reason"})
	e.catchUp()
	accID := wantAccountID(fakeSource, "acc_1")
	if r := e.recon(accID); r.KnownException == nil || *r.KnownException != "made-up reason" || !*r.LastCheckOK {
		t.Fatalf("accepted: %+v", r)
	}
	e.respondAs(e.fetchAs(otherSource, 1, "", nil), otherSource, "/accounts", 200,
		body(fBody{Accounts: []fAcc{{ID: "other_1", Type: "current", BankRef: refA}}}))
	e.catchUp()
	if r := e.recon(accID); r.KnownException == nil || *r.KnownException != "shared_bank_ref" || *r.LastCheckOK {
		t.Fatalf("shared: %+v", r)
	}
	e.end("fake-other-current", ptr("2030-01-31"))
	check := func(when string) {
		t.Helper()
		r := e.recon(accID)
		if r.KnownException == nil || *r.KnownException != "made-up reason" || r.LastCheckOK == nil || !*r.LastCheckOK {
			t.Fatalf("%s: %+v, want the accepted reason back and a pass", when, r)
		}
	}
	check("live")
	e.renormalise(false)
	check("after renormalise")
}

// A statement account ended before every statement it holds has no check left: it is
// not_checked, and clearing a shared bank reference report does not turn it into a pass.
func TestAccountEndedBeforeAllItsStatementsIsNotChecked(t *testing.T) {
	e := cutOver(t)
	e.end("halifax-joint", ptr("2029-12-31"))
	r := e.recon(e.account("halifax-joint").ID)
	if r.LastCheckOK != nil || r.LastCheckAt != nil || r.KnownException != nil || r.BreakCount != 0 {
		t.Fatalf("reconciliation %+v, want no check", r)
	}
	if s := e.coverageState("halifax-joint"); s != "not_checked" {
		t.Fatalf("coverage state %q", s)
	}
}

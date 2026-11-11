package ledger_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/store"
)

// lloydsRow is a made-up date-only row with the balance after it, in decimal pounds.
func lloydsRow(id, day string, amount int64, after string) json.RawMessage {
	b, _ := json.Marshal(fTxn{ID: id, Account: refA, Amount: amount, Created: day + "T00:00:00Z",
		RunningBalance: &fRunning{Amount: json.Number(after), Currency: "GBP"}})
	return b
}

// lloydsEnv is an env holding one running_balance account under the Lloyds source.
func lloydsEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.respondAs(e.fetchAs(lloydsSource, 1, "", nil), lloydsSource, "/accounts", 200,
		body(fBody{Accounts: []fAcc{{ID: refA, Type: "joint", BankRef: refA}}}))
	e.catchUp()
	return e
}

// rows stores one transactions response for the Lloyds account in run.
func (e *env) lloydsRows(run int64, rows ...json.RawMessage) {
	e.t.Helper()
	e.respondAs(e.fetchAs(lloydsSource, run, refA, nil), lloydsSource, "/transactions", 200, body(fBody{Transactions: rows}))
}

// lloydsBalance stores one balance reading for the Lloyds account in run.
func (e *env) lloydsBalance(run int64, minor int64) {
	e.t.Helper()
	e.respondAs(e.fetchAs(lloydsSource, run, refA, nil), lloydsSource, "/balance", 200,
		body(fBody{Balances: []fBal{{Account: refA, Balance: minor}}}))
}

func (e *env) lloydsRecon() store.Reconciliation {
	e.t.Helper()
	return e.recon(wantAccountID(lloydsSource, refA))
}

func TestLloydsAccountIsNamedByTheSlugPrefixAndChecksRunningBalances(t *testing.T) {
	e := lloydsEnv(t)
	a := e.account("lloyds-joint")
	if a.ID != wantAccountID(lloydsSource, refA) || a.BankRef == nil || *a.BankRef != refA {
		t.Fatalf("account %+v", a)
	}
	if r := e.lloydsRecon(); r.Method != "running_balance" {
		t.Fatalf("method %q", r.Method)
	}
}

// The design's worked example: the previous day closed at 1,000.00; today -200.00 (800.00 after)
// and +50.00 (850.00 after) sum to -150.00 and close at 850.00. A same-day refund the next day
// sums to zero and closes at the opening balance.
func TestDaysThatAddUpToTheirRunningBalancesPass(t *testing.T) {
	e := lloydsEnv(t)
	e.lloydsRows(2,
		lloydsRow("r1", "2030-01-01", 100000, "1000.00"),
		lloydsRow("r2", "2030-01-02", -20000, "800.00"),
		lloydsRow("r3", "2030-01-02", 5000, "850.00"),
		lloydsRow("r4", "2030-01-03", 3000, "880.00"),
		lloydsRow("r5", "2030-01-03", -3000, "850"),
	)
	e.lloydsBalance(2, 85000)
	e.catchUp()
	r := e.lloydsRecon()
	if r.LastCheckOK == nil || !*r.LastCheckOK || r.BreakCount != 0 || r.FailedOn != nil ||
		!sameDay(r.ReconciledThrough, date("2030-01-03")) || r.LastDiffMinor == nil || *r.LastDiffMinor != 0 || r.LastPassAt == nil {
		t.Fatalf("reconciliation %+v", r)
	}
	if s := e.coverageState("lloyds-joint"); s != "reconciled" {
		t.Fatalf("coverage state %q", s)
	}
}

// The first day of history has no previous close: any opening that some row's balance before
// gives and that the day's sum takes to one of its running balances passes.
func TestFirstDayPassesWithAnyFittingOpeningBalance(t *testing.T) {
	e := lloydsEnv(t)
	// Listed in an order that is not the bank's: opening 500.00, -100.00 (400.00), +20.00 (420.00).
	e.lloydsRows(2,
		lloydsRow("r2", "2030-01-01", 2000, "420.00"),
		lloydsRow("r1", "2030-01-01", -10000, "400.00"),
	)
	e.lloydsBalance(2, 42000)
	e.catchUp()
	if r := e.lloydsRecon(); r.LastCheckOK == nil || !*r.LastCheckOK || r.BreakCount != 0 {
		t.Fatalf("reconciliation %+v", r)
	}
}

func TestFailingDayIsAMismatchNamingItsDate(t *testing.T) {
	e := lloydsEnv(t)
	e.lloydsRows(2,
		lloydsRow("r1", "2030-01-01", 100000, "1000.00"),
		lloydsRow("r2", "2030-01-02", -1000, "990.00"),
		// 990.00 - 10.00 is 980.00, not 970.00.
		lloydsRow("r3", "2030-01-03", -1000, "970.00"),
		lloydsRow("r4", "2030-01-04", -1000, "960.00"),
	)
	e.lloydsBalance(2, 96000)
	e.catchUp()
	r := e.lloydsRecon()
	if r.LastCheckOK == nil || *r.LastCheckOK || r.BreakCount != 1 || !sameDay(r.FailedOn, date("2030-01-03")) ||
		!sameDay(r.ReconciledThrough, date("2030-01-02")) {
		t.Fatalf("reconciliation %+v", r)
	}
	if s := e.coverageState("lloyds-joint"); s != "mismatch" {
		t.Fatalf("coverage state %q", s)
	}
	var failedOn *time.Time
	if err := e.app.DB().QueryRow(t.Context(), "SELECT failed_on FROM agent.coverage WHERE account_slug = 'lloyds-joint'").Scan(&failedOn); err != nil {
		t.Fatal(err)
	}
	if !sameDay(failedOn, date("2030-01-03")) {
		t.Fatalf("agent.coverage failed_on %v", failedOn)
	}

	// The bank corrects the row: the check is recomputed from scratch and passes.
	e.lloydsRows(3,
		lloydsRow("r3", "2030-01-03", -1000, "980.00"),
		lloydsRow("r4", "2030-01-04", -1000, "970.00"),
	)
	e.lloydsBalance(3, 97000)
	e.catchUp()
	if r := e.lloydsRecon(); r.LastCheckOK == nil || !*r.LastCheckOK || r.BreakCount != 0 || r.FailedOn != nil {
		t.Fatalf("after the correction %+v", r)
	}
}

// The balance reading is taken at another moment than the transactions call, so a difference is
// a mismatch only when the previous check saw the same one.
func TestBalanceDifferenceFailsOnlyWhenTheNextReadingRepeatsIt(t *testing.T) {
	e := lloydsEnv(t)
	e.lloydsRows(2, lloydsRow("r1", "2030-01-01", 100000, "1000.00"))
	e.lloydsBalance(2, 100500)
	e.catchUp()
	r := e.lloydsRecon()
	if r.LastCheckOK == nil || !*r.LastCheckOK || r.LastDiffMinor == nil || *r.LastDiffMinor != 500 {
		t.Fatalf("first difference: %+v", r)
	}

	e.lloydsBalance(3, 100500)
	e.catchUp()
	r = e.lloydsRecon()
	if r.LastCheckOK == nil || *r.LastCheckOK || *r.LastDiffMinor != 500 || r.BreakCount != 0 {
		t.Fatalf("repeated difference: %+v", r)
	}

	// Accepting it passes this difference only.
	e.operator(ledger.EntryAcceptDifference, ledger.AcceptDifference{Slug: "lloyds-joint", DiffMinor: ptr(int64(500)), Reason: "made-up"})
	e.catchUp()
	if r := e.lloydsRecon(); r.LastCheckOK == nil || !*r.LastCheckOK {
		t.Fatalf("accepted: %+v", r)
	}
	e.lloydsBalance(4, 100700)
	e.catchUp()
	e.lloydsBalance(5, 100700)
	e.catchUp()
	if r := e.lloydsRecon(); r.LastCheckOK == nil || *r.LastCheckOK || *r.LastDiffMinor != 700 {
		t.Fatalf("another repeated difference: %+v", r)
	}
}

func TestParseMinorIsExact(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "1": 100, "-1.5": -150, "1919.59": 191959, "0.01": 1, "-0.10": -10,
		"92233720368547758.07": 9223372036854775807} {
		got, err := ledger.ParseMinor(in)
		if err != nil || got != want {
			t.Errorf("ParseMinor(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "1.234", "1e2", "abc", "1.", ".5", "--1", "92233720368547758.08", "+1"} {
		if got, err := ledger.ParseMinor(in); err == nil {
			t.Errorf("ParseMinor(%q) = %d, want an error", in, got)
		}
	}
}

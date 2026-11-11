package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/importer/importertest"
	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/store"
)

// Three consecutive made-up months as manual-csv. A manual-csv statement covers its first to its
// last row date: September 3 to 28 (closing 97.00), October 5 (96.00), November 10 (95.50).
var (
	sepCSV = csvFile(10000, csvRow{"2030-09-03", "DEB", -500, "MADE UP"}, csvRow{"2030-09-28", "BGC", 200, "MADE UP"})
	octCSV = csvFile(9700, csvRow{"2030-10-05", "DEB", -100, "MADE UP"})
	novCSV = csvFile(9600, csvRow{"2030-11-10", "DEB", -50, "MADE UP"})
)

// halifaxRows is two made-up rows in month of 2030 that add 100.00 to the balance.
func halifaxRows(month time.Month) []importertest.Row {
	day := func(d int) string { return time.Date(2030, month, d, 0, 0, 0, 0, time.UTC).Format("02 Jan 06") }
	return []importertest.Row{
		{Date: day(3), Desc: "MADE UP PAYROLL", Type: "FPI", In: 12000},
		{Date: day(20), Desc: "MADE UP SHOP", Type: "DEB", Out: 2000},
	}
}

// halifaxPDF is a generated Halifax statement for the whole of month in 2030.
func halifaxPDF(month time.Month, opening int64, rows []importertest.Row, sortCode, account string) []byte {
	g := importertest.New(opening, rows)
	g.Start = time.Date(2030, month, 1, 0, 0, 0, 0, time.UTC)
	g.End = g.Start.AddDate(0, 1, -1)
	g.SortCode, g.Account = sortCode, account
	return g.PDF()
}

func monthEnd(month time.Month) time.Time {
	return time.Date(2030, month+1, 0, 0, 0, 0, 0, time.UTC)
}

// coverageState is the account's reconciliation_state in agent.coverage.
func (e *env) coverageState(slug string) string {
	e.t.Helper()
	var s string
	if err := e.app.DB().QueryRow(context.Background(),
		"SELECT reconciliation_state FROM agent.coverage WHERE account_slug = $1", slug).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) gaps(account string) []store.FileGap {
	e.t.Helper()
	g, err := e.core().ListFileGaps(context.Background(), wantAccountID(ledger.SourceManual, account))
	if err != nil {
		e.t.Fatal(err)
	}
	return g
}

func (e *env) addFileAccount(slug string) {
	e.operator(ledger.EntryAccountAdd, ledger.AccountAdd{Slug: slug, Type: "current", Currency: "GBP"})
}

// (a)
func TestEarlierStatementImportedAfterTheLaterOneJoinsWithoutAGap(t *testing.T) {
	e := newEnv(t)
	e.addFileAccount("halifax-current")
	// October closes at 500.00 + 100.00; November opens there.
	e.importFile("halifax-current", "halifax-pdf", halifaxPDF(time.November, 60000, halifaxRows(time.November), "11-22-33", "12345678"))
	e.catchUp()
	e.importFile("halifax-current", "halifax-pdf", halifaxPDF(time.October, 50000, halifaxRows(time.October), "11-22-33", "12345678"))
	if st := e.catchUp(); st.FilesRejected != 0 || st.TransactionsCreated != 2 {
		t.Fatalf("stats %+v", st)
	}
	if g := e.gaps("halifax-current"); len(g) != 0 {
		t.Fatalf("gaps %+v", g)
	}
	r := e.recon(wantAccountID(ledger.SourceManual, "halifax-current"))
	if r.BreakCount != 0 || !sameDay(r.ReconciledThrough, monthEnd(time.November)) || r.LastCheckOK == nil || !*r.LastCheckOK {
		t.Fatalf("reconciliation %+v", r)
	}
	if s := e.coverageState("halifax-current"); s != "reconciled" {
		t.Fatalf("coverage state %q", s)
	}
}

// (b)
func TestMissingMiddleStatementIsAGapUntilItArrivesInEitherOrder(t *testing.T) {
	for name, order := range map[string][][]byte{"september first": {sepCSV, novCSV}, "november first": {novCSV, sepCSV}} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.addFileAccount("bank-current")
			for _, f := range order {
				e.importFile("bank-current", "manual-csv", f)
				e.catchUp()
			}
			g := e.gaps("bank-current")
			if len(g) != 1 || !g[0].AfterDate.Equal(date("2030-09-28")) || !g[0].BeforeDate.Equal(date("2030-11-10")) ||
				g[0].DiffMinor != 9600-9700 {
				t.Fatalf("gaps %+v", g)
			}
			accID := wantAccountID(ledger.SourceManual, "bank-current")
			if r := e.recon(accID); r.BreakCount != 1 || !sameDay(r.ReconciledThrough, date("2030-09-28")) {
				t.Fatalf("with the gap: %+v", r)
			}
			if s := e.coverageState("bank-current"); s != "mismatch" {
				t.Fatalf("coverage state %q with a gap", s)
			}

			e.importFile("bank-current", "manual-csv", octCSV)
			e.catchUp()
			if g := e.gaps("bank-current"); len(g) != 0 {
				t.Fatalf("October did not close the gap: %+v", g)
			}
			if r := e.recon(accID); r.BreakCount != 0 || !sameDay(r.ReconciledThrough, date("2030-11-10")) || !*r.LastCheckOK {
				t.Fatalf("after the heal: %+v", r)
			}
			if s := e.coverageState("bank-current"); s != "reconciled" {
				t.Fatalf("coverage state %q after the heal", s)
			}
		})
	}
}

// (c)
func TestPartAndFullStatementInEitherOrderLeaveNoGapOrDuplicates(t *testing.T) {
	rows := halifaxRows(time.January)
	rows = append(rows, importertest.Row{Date: "28 Jan 30", Desc: "MADE UP BILL", Type: "DD", Out: 500})
	partPDF := func() []byte {
		g := importertest.New(40000, rows[:2])
		g.Start, g.End = date("2030-01-01"), date("2030-01-20")
		g.SortCode, g.Account = "11-22-33", "12345678"
		return g.PDF()
	}()
	fullPDF := halifaxPDF(time.January, 40000, rows, "11-22-33", "12345678")
	csvRows := []csvRow{{"2030-01-03", "DEB", -1250, "MADE UP"}, {"2030-01-05", "DEB", -1250, "MADE UP"}, {"2030-01-20", "BGC", 100000, "MADE UP"}}
	partCSV, fullCSV := csvFile(50000, csvRows[:2]...), csvFile(50000, csvRows...)

	cases := []struct {
		name, format string
		files        [][]byte
		through      time.Time
	}{
		{"pdf part then full", "halifax-pdf", [][]byte{partPDF, fullPDF}, date("2030-01-31")},
		{"pdf full then part", "halifax-pdf", [][]byte{fullPDF, partPDF}, date("2030-01-31")},
		{"csv part then full", "manual-csv", [][]byte{partCSV, fullCSV}, date("2030-01-20")},
		{"csv full then part", "manual-csv", [][]byte{fullCSV, partCSV}, date("2030-01-20")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.addFileAccount("bank-current")
			for _, f := range c.files {
				e.importFile("bank-current", c.format, f)
				e.catchUp()
			}
			accID := wantAccountID(ledger.SourceManual, "bank-current")
			if n := e.countTxns(accID); n != 3 {
				t.Fatalf("%d transactions, want 3", n)
			}
			if g := e.gaps("bank-current"); len(g) != 0 {
				t.Fatalf("gaps %+v", g)
			}
			if r := e.recon(accID); r.BreakCount != 0 || !sameDay(r.ReconciledThrough, c.through) {
				t.Fatalf("reconciliation %+v", r)
			}
		})
	}
}

// (d)
func TestStatementThatNeitherOverlapsNorFollowsIsAGapAndAMismatch(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addFileAccount("bank-current")
	e.importFile("bank-current", "manual-csv", csvFile(10000, csvRow{"2030-01-03", "DEB", -500, "MADE UP"}, csvRow{"2030-01-28", "BGC", 200, "MADE UP"}))
	e.catchUp()
	// January closed at 97.00; March opens at 120.00.
	e.importFile("bank-current", "manual-csv", csvFile(12000, csvRow{"2030-03-02", "DD", -100, "MADE UP"}))
	if st := e.catchUp(); st.FilesRejected != 0 || st.TransactionsCreated != 1 {
		t.Fatalf("stats %+v", st)
	}
	accID := wantAccountID(ledger.SourceManual, "bank-current")
	gaps, err := e.core().ListFileGaps(ctx, accID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 1 || !gaps[0].AfterDate.Equal(date("2030-01-28")) || !gaps[0].BeforeDate.Equal(date("2030-03-02")) ||
		gaps[0].DiffMinor != 2300 || gaps[0].RawSeq != e.lastSeq() {
		t.Fatalf("gaps %+v", gaps)
	}
	r := e.recon(accID)
	if r.BreakCount != 1 || !sameDay(r.ReconciledThrough, date("2030-01-28")) || *r.LastCheckOK || *r.LastDiffMinor != 2300 {
		t.Fatalf("reconciliation %+v", r)
	}
	if s := e.coverageState("bank-current"); s != "mismatch" {
		t.Fatalf("coverage state %q", s)
	}
}

func TestStatementForAnotherBankAccountIsRejectedLiveAndOnReplay(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addFileAccount("halifax-current")
	e.importFile("halifax-current", "halifax-pdf", halifaxPDF(time.January, 40000, halifaxRows(time.January), "11-22-33", "1234 5678"))
	e.catchUp()
	accID := wantAccountID(ledger.SourceManual, "halifax-current")
	if a := e.account("halifax-current"); a.BankRef == nil || *a.BankRef != "112233 12345678" {
		t.Fatalf("bank_ref %v, want the first statement's", a.BankRef)
	}

	e.importFile("halifax-current", "halifax-pdf", halifaxPDF(time.February, 50000, halifaxRows(time.February), "11-22-33", "87654321"))
	if st := e.catchUp(); st.FilesRejected != 1 || st.TransactionsCreated != 0 || len(st.Rejections) != 1 || st.Rejections[0] != "account_mismatch" {
		t.Fatalf("stats %+v", st)
	}
	if n := e.countTxns(accID); n != 2 {
		t.Fatalf("%d transactions, want only January's 2", n)
	}
	stmts, err := e.core().ListStatements(ctx, accID)
	if err != nil || len(stmts) != 1 {
		t.Fatalf("statements %+v, %v", stmts, err)
	}
	if a := e.account("halifax-current"); *a.BankRef != "112233 12345678" {
		t.Fatalf("a rejected statement changed bank_ref to %s", *a.BankRef)
	}
	logged := e.logs.String()
	if !strings.Contains(logged, "account_mismatch") || strings.Contains(logged, "87654321") || strings.Contains(logged, "12345678") {
		t.Fatalf("rejection log: %s", logged)
	}

	if diffs := e.renormalise(true); len(diffs) != 0 {
		t.Fatalf("rebuild differs: %+v", diffs)
	}
	e.renormalise(false)
	if a := e.account("halifax-current"); a.BankRef == nil || *a.BankRef != "112233 12345678" {
		t.Fatalf("bank_ref after rebuild %v", a.BankRef)
	}
	if n := e.countTxns(accID); n != 2 {
		t.Fatalf("%d transactions after rebuild, want 2", n)
	}
}

func TestStatementFailingItsOwnCheckStoresNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addFileAccount("bank-current")
	bad := []byte("date,type,amount,balance_after,description\n2030-01-03,DEB,-5.00,95.00,MADE UP\n2030-01-04,DEB,-5.00,91.00,MADE UP\n")
	e.importFile("bank-current", "manual-csv", bad)
	st := e.catchUp()
	if st.FilesRejected != 1 || st.TransactionsCreated != 0 {
		t.Fatalf("stats %+v", st)
	}
	accID := wantAccountID(ledger.SourceManual, "bank-current")
	if n := e.countTxns(accID); n != 0 {
		t.Fatalf("%d rows stored from a rejected file", n)
	}
	if _, err := e.core().LatestBalanceSnapshot(ctx, accID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a rejected file stored a balance: %v", err)
	}
	if stmts, _ := e.core().ListStatements(ctx, accID); len(stmts) != 0 {
		t.Fatalf("a rejected file stored a statement: %+v", stmts)
	}
	if r := e.recon(accID); r.ReconciledThrough != nil || r.LastCheckAt != nil {
		t.Fatalf("a rejected file changed reconciliation: %+v", r)
	}
	if e.lastSeq() != e.maxRawSeq() {
		t.Fatal("last_seq did not advance past the rejected file")
	}
}

func TestImportForAnAPIAccountOrUnknownSlugIsRejected(t *testing.T) {
	e := newEnv(t)
	e.accounts(fAcc{ID: "acc_1", Type: "current"})
	e.catchUp()
	f := csvFile(0, csvRow{"2030-01-03", "BGC", 500, "MADE UP"})
	e.importFile("fake-current", "manual-csv", f)
	e.importFile("no-such-account", "manual-csv", f)
	st := e.catchUp()
	if st.FilesRejected != 2 || st.TransactionsCreated != 0 {
		t.Fatalf("stats %+v", st)
	}
	if n := e.countTxns(wantAccountID(fakeSource, "acc_1")); n != 0 {
		t.Fatalf("%d rows stored on an API account from a file", n)
	}
}

func TestCompletedFetchWithoutPeriodOrAccountDisappearsNothing(t *testing.T) {
	e := newEnv(t)
	e.accounts(fAcc{ID: "acc_1", Type: "current"})
	e.respond(e.fetch(1, "acc_1", nil), "/transactions", body(fBody{Transactions: []json.RawMessage{txn("tx_1", "acc_1", -100, jan5)}}))
	e.catchUp()
	jan := &period{date("2030-01-01"), date("2030-02-01")}
	// Completed and empty, but with no period, then with no account.
	e.complete(e.fetch(2, "acc_1", nil))
	e.complete(e.fetch(3, "", jan))
	if st := e.catchUp(); st.Disappeared != 0 {
		t.Fatalf("stats %+v", st)
	}
	accID := wantAccountID(fakeSource, "acc_1")
	if got := e.txn(wantTxnID(accID, fakeSource, "tx_1")); got.DisappearedAt != nil {
		t.Fatalf("disappeared_at set by a fetch with no period or no account: %+v", got)
	}
	if r := e.recon(accID); r.RunningSumMinor != -100 {
		t.Fatalf("running sum %d", r.RunningSumMinor)
	}
	// The same empty fetch with an account and a period does disappear it, so the two above
	// were refused for the right reason.
	e.complete(e.fetch(4, "acc_1", jan))
	if st := e.catchUp(); st.Disappeared != 1 {
		t.Fatalf("control: stats %+v", st)
	}
}

// flatRows is two made-up rows in month of 2030 whose money in equals money out.
func flatRows(month time.Month) []importertest.Row {
	day := func(d int) string { return time.Date(2030, month, d, 0, 0, 0, 0, time.UTC).Format("02 Jan 06") }
	return []importertest.Row{
		{Date: day(4), Desc: "MADE UP REFUND", Type: "FPI", In: 3000},
		{Date: day(9), Desc: "MADE UP SHOP", Type: "DEB", Out: 3000},
	}
}

func TestSkippedMonthWithEqualBalancesIsAGapBetweenExactPeriods(t *testing.T) {
	e := newEnv(t)
	e.addFileAccount("halifax-current")
	// January closes at 500.00; March opens there. February, money in equal to money out, is
	// missing.
	e.importFile("halifax-current", "halifax-pdf", halifaxPDF(time.January, 40000, halifaxRows(time.January), "11-22-33", "12345678"))
	e.importFile("halifax-current", "halifax-pdf", halifaxPDF(time.March, 50000, halifaxRows(time.March), "11-22-33", "12345678"))
	e.catchUp()
	g := e.gaps("halifax-current")
	if len(g) != 1 || !g[0].AfterDate.Equal(date("2030-01-31")) || !g[0].BeforeDate.Equal(date("2030-03-01")) || g[0].DiffMinor != 0 {
		t.Fatalf("gaps %+v, want one from 31 January to 1 March with difference 0", g)
	}
	if s := e.coverageState("halifax-current"); s != "mismatch" {
		t.Fatalf("coverage state %q with February missing", s)
	}
	e.importFile("halifax-current", "halifax-pdf", halifaxPDF(time.February, 50000, flatRows(time.February), "11-22-33", "12345678"))
	e.catchUp()
	if g := e.gaps("halifax-current"); len(g) != 0 {
		t.Fatalf("February did not close the gap: %+v", g)
	}
	accID := wantAccountID(ledger.SourceManual, "halifax-current")
	if r := e.recon(accID); r.BreakCount != 0 || !sameDay(r.ReconciledThrough, monthEnd(time.March)) {
		t.Fatalf("after February: %+v", r)
	}
	if s := e.coverageState("halifax-current"); s != "reconciled" {
		t.Fatalf("coverage state %q after February", s)
	}
}

func TestPartMonthStatementThenNextFullMonthIsAGapFromThePartsEnd(t *testing.T) {
	e := newEnv(t)
	e.addFileAccount("halifax-current")
	part := importertest.New(40000, halifaxRows(time.January)[:1])
	part.Start, part.End = date("2030-01-01"), date("2030-01-10")
	part.SortCode, part.Account = "11-22-33", "12345678"
	e.importFile("halifax-current", "halifax-pdf", part.PDF())
	// February opens at the part statement's closing balance, 520.00.
	e.importFile("halifax-current", "halifax-pdf", halifaxPDF(time.February, 52000, flatRows(time.February), "11-22-33", "12345678"))
	e.catchUp()
	g := e.gaps("halifax-current")
	if len(g) != 1 || !g[0].AfterDate.Equal(date("2030-01-10")) || !g[0].BeforeDate.Equal(date("2030-02-01")) || g[0].DiffMinor != 0 {
		t.Fatalf("gaps %+v, want one from 10 January to 1 February", g)
	}
}

func TestRowDatedStatementsWithJoiningBalancesAndADateGapRecordNoGap(t *testing.T) {
	e := newEnv(t)
	e.addFileAccount("bank-current")
	// September closes at 97.00; this statement opens there, five weeks later. Row dates cannot
	// say whether anything happened in between.
	e.importFile("bank-current", "manual-csv", sepCSV)
	e.importFile("bank-current", "manual-csv", csvFile(9700, csvRow{"2030-11-10", "DEB", -50, "MADE UP"}))
	e.catchUp()
	if g := e.gaps("bank-current"); len(g) != 0 {
		t.Fatalf("gaps %+v", g)
	}
}

package ledger_test

import (
	"context"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/ledger"
)

// coverageGaps is the number of gaps agent.coverage reports for the account.
func (e *env) coverageGaps(slug string) int {
	e.t.Helper()
	var n int
	if err := e.app.DB().QueryRow(context.Background(),
		"SELECT jsonb_array_length(gaps) FROM agent.coverage WHERE account_slug = $1", slug).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// January and March of a made-up Halifax account; March opens at marchOpening. January closes at
// 500.00, so an opening of 50000 joins it and leaves a gap with a difference of 0.
func (e *env) janAndMarch(marchOpening int64) {
	e.t.Helper()
	e.addFileAccount("halifax-current")
	e.importFile("halifax-current", "halifax-pdf", halifaxPDF(time.January, 40000, halifaxRows(time.January), "11-22-33", "12345678"))
	e.importFile("halifax-current", "halifax-pdf", halifaxPDF(time.March, marchOpening, halifaxRows(time.March), "11-22-33", "12345678"))
	e.catchUp()
}

var febGap = ledger.AcceptGap{Slug: "halifax-current", AfterDate: "2030-01-31", BeforeDate: "2030-03-01", Reason: "made-up: no statement for an idle month"}

// An accepted gap whose statements join is no longer a break: the account reconciles through its
// last statement and agent.coverage reports no gap. The reason is kept on the gap, and a rebuild
// from raw gives the same.
func TestAcceptedGapWithNoDifferenceIsNoLongerAMismatchAndIsReplayedFromRaw(t *testing.T) {
	e := newEnv(t)
	e.janAndMarch(50000)
	if s := e.coverageState("halifax-current"); s != "mismatch" {
		t.Fatalf("coverage state %q before the gap is accepted", s)
	}
	e.operator(ledger.EntryAcceptGap, febGap)
	if st := e.catchUp(); st.Unparseable != 0 {
		t.Fatalf("the accept_gap entry was skipped: %+v", st)
	}
	accID := wantAccountID(ledger.SourceManual, "halifax-current")
	check := func(when string) {
		t.Helper()
		g := e.gaps("halifax-current")
		if len(g) != 1 || g[0].AcceptedReason == nil || *g[0].AcceptedReason != febGap.Reason {
			t.Fatalf("%s: gaps %+v, want the February gap with its accepted reason", when, g)
		}
		r := e.recon(accID)
		if r.BreakCount != 0 || r.LastCheckOK == nil || !*r.LastCheckOK || !sameDay(r.ReconciledThrough, monthEnd(time.March)) {
			t.Fatalf("%s: reconciliation %+v, want a pass through the end of March", when, r)
		}
		if s := e.coverageState("halifax-current"); s != "reconciled" {
			t.Fatalf("%s: coverage state %q", when, s)
		}
		if n := e.coverageGaps("halifax-current"); n != 0 {
			t.Fatalf("%s: agent.coverage reports %d gaps", when, n)
		}
	}
	check("live")
	if diffs := e.renormalise(true); len(diffs) != 0 {
		t.Fatalf("verify: %+v", diffs)
	}
	e.renormalise(false)
	check("after a rebuild")

	// A later statement after another idle month is a new gap the acceptance does not cover.
	e.importFile("halifax-current", "halifax-pdf", halifaxPDF(time.May, 60000, halifaxRows(time.May), "11-22-33", "12345678"))
	e.catchUp()
	if s := e.coverageState("halifax-current"); s != "mismatch" {
		t.Fatalf("coverage state %q with April missing", s)
	}
	if r := e.recon(accID); r.BreakCount != 1 || !sameDay(r.ReconciledThrough, monthEnd(time.March)) {
		t.Fatalf("with April missing: %+v", r)
	}
}

// A gap whose statements do not join (the closing balance before it differs from the opening
// balance after it) stays open whatever the operator entry says.
func TestAcceptGapForAGapWithADifferenceChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.janAndMarch(60000)
	e.operator(ledger.EntryAcceptGap, febGap)
	e.catchUp()
	g := e.gaps("halifax-current")
	if len(g) != 1 || g[0].DiffMinor != 10000 || g[0].AcceptedReason != nil {
		t.Fatalf("gaps %+v, want the February gap open with a difference of 10000", g)
	}
	if r := e.recon(wantAccountID(ledger.SourceManual, "halifax-current")); r.BreakCount != 1 || r.LastCheckOK == nil || *r.LastCheckOK {
		t.Fatalf("reconciliation %+v", r)
	}
	if s := e.coverageState("halifax-current"); s != "mismatch" {
		t.Fatalf("coverage state %q", s)
	}
}

// An accept_gap entry for an unknown account, an account an API feeds, or with a bad payload is
// counted unparseable and skipped.
func TestAcceptGapForAnUnknownOrAPIAccountOrABadPayloadIsSkipped(t *testing.T) {
	e := newEnv(t)
	e.accounts(fAcc{ID: "acc_1", Type: "current"})
	e.catchUp()
	e.operator(ledger.EntryAcceptGap, ledger.AcceptGap{Slug: "no-such-account", AfterDate: "2030-01-31", BeforeDate: "2030-03-01", Reason: "made-up"})
	e.operator(ledger.EntryAcceptGap, ledger.AcceptGap{Slug: "fake-current", AfterDate: "2030-01-31", BeforeDate: "2030-03-01", Reason: "made-up"})
	e.operator(ledger.EntryAcceptGap, ledger.AcceptGap{Slug: "fake-current", AfterDate: "31/01/2030", BeforeDate: "2030-03-01", Reason: "made-up"})
	e.operator(ledger.EntryAcceptGap, ledger.AcceptGap{Slug: "fake-current", AfterDate: "2030-01-31", BeforeDate: "2030-03-01"})
	if st := e.catchUp(); st.Unparseable != 4 {
		t.Fatalf("stats %+v, want 4 unparseable", st)
	}
}

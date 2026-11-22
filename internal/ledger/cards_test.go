package ledger_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/store"
)

// cardAccounts stores a card list response of the Lloyds source naming the cards as accounts.
func (e *env) cardAccounts(keys ...string) {
	e.t.Helper()
	var accs []fAcc
	for _, k := range keys {
		accs = append(accs, fAcc{ID: k, Type: "credit_card", Name: "made-up card " + k})
	}
	e.respondAs(e.fetchAs(lloydsSource, 1, "", nil), lloydsSource, "/cards", 200,
		body(fBody{Accounts: accs, CardLists: []fCardList{{Source: lloydsSource, Keys: keys}}}))
}

// A credit card is slugged <source prefix>-card-<last four> from its key, whatever the order cards
// arrive in, and is checked by balance_movement.
func TestCreditCardAccountsAreSluggedByTheirKeyAndCheckedByBalanceMovement(t *testing.T) {
	e := newEnv(t)
	e.cardAccounts("card:9012", "card:5678")
	e.accounts(fAcc{ID: "card:1111", Type: "credit_card"})
	stats := e.catchUp()
	if stats.AccountsCreated != 3 {
		t.Fatalf("%d accounts created, want 3", stats.AccountsCreated)
	}
	for slug, key := range map[string]string{"lloyds-card-5678": "card:5678", "lloyds-card-9012": "card:9012", "fake-card-1111": "card:1111"} {
		a := e.account(slug)
		if a.SourceKey != key || a.Type != "credit_card" || a.BankRef != nil {
			t.Errorf("%s = %+v", slug, a)
		}
		if r := e.recon(a.ID); r.Method != "balance_movement" {
			t.Errorf("%s reconciliation method %q, want balance_movement", slug, r.Method)
		}
	}
	// One balance reading is recorded and is too little to check anything against.
	acc := wantAccountID(lloydsSource, "card:5678")
	e.cardRun(1, "card:5678", -12345)
	if r := e.recon(acc); r.LastCheckAt != nil || r.LastCheckOK != nil {
		t.Errorf("a card was checked on one reading: %+v", r)
	}
	if row := e.coverageRow("lloyds-card-5678"); row.ReconciliationState != store.StateNotChecked || len(row.Gaps) != 0 {
		t.Errorf("coverage %+v", row)
	}
}

// cardRun stores one run of a Lloyds card as a TrueLayer run makes it, the card's transactions
// (when there are any) before its balance reading, and catches up.
func (e *env) cardRun(run int64, key string, balance int64, txns ...json.RawMessage) {
	e.t.Helper()
	if len(txns) > 0 {
		e.respondAs(e.fetchAs(lloydsSource, run, key, nil), lloydsSource, "/card-transactions", 200, body(fBody{Transactions: txns}))
	}
	e.respondAs(e.fetchAs(lloydsSource, run, key, nil), lloydsSource, "/card-balance", 200,
		body(fBody{Balances: []fBal{{Account: key, Balance: balance}}}))
	e.catchUp()
}

func (e *env) cardState(slug string) (string, *int64) {
	e.t.Helper()
	row := e.coverageRow(slug)
	return row.ReconciliationState, row.LastDiffMinor
}

var cardDay = time.Date(2030, 1, 10, 0, 0, 0, 0, time.UTC)

// A card's history may not reach back to its opening, so its balance is not the sum of its
// transactions. The check is anchored on the difference between the two once two readings in a row
// agree on it, and from then on the balance must move by exactly the transactions the ledger took
// in between readings. Until two readings agree the card is not_checked, never reconciled. A
// reading that is ahead of the transactions (a purchase in the balance before it is listed) is a
// difference only when the next reading shows the same one.
func TestCardBalanceMustMoveByItsTransactions(t *testing.T) {
	e := newEnv(t)
	e.cardAccounts("card:5678")
	const slug = "lloyds-card-5678"
	acc := wantAccountID(lloydsSource, "card:5678")

	// History that does not reach the card's opening: -10.00 and +5.00 against 123.45 owed.
	e.cardRun(1, "card:5678", -12345,
		txn("c1", "card:5678", -1000, cardDay), txn("c2", "card:5678", 500, cardDay.AddDate(0, 0, 2)))
	if s, d := e.cardState(slug); s != store.StateNotChecked || d != nil {
		t.Fatalf("after one reading: %s %v, want not_checked", s, d)
	}
	// Two readings that disagree are still too little.
	e.cardRun(2, "card:5678", -12000)
	if s, _ := e.cardState(slug); s != store.StateNotChecked {
		t.Fatalf("after two readings that disagree: %s, want not_checked", s)
	}
	// The next one agrees with the last: anchored, and nothing has moved.
	e.cardRun(3, "card:5678", -12000)
	if s, d := e.cardState(slug); s != store.StateReconciled || d == nil || *d != 0 {
		t.Fatalf("after two readings that agree: %s %v, want reconciled with 0", s, d)
	}
	if r := e.recon(acc); r.LastCheckOK == nil || !*r.LastCheckOK || r.LastPassAt == nil {
		t.Fatalf("reconciliation %+v", r)
	}
	// A purchase of 24.25: the debt and the transactions move together.
	e.cardRun(4, "card:5678", -14425, txn("c3", "card:5678", -2425, cardDay.AddDate(0, 0, 3)))
	if s, d := e.cardState(slug); s != store.StateReconciled || *d != 0 {
		t.Fatalf("after a purchase: %s %v", s, *d)
	}
	// A purchase of 3.00 in the balance before the bank lists it: a difference, not yet a mismatch.
	e.cardRun(5, "card:5678", -14725)
	if s, d := e.cardState(slug); s != store.StateReconciled || *d != -300 {
		t.Fatalf("a reading ahead of the transactions: %s %v, want reconciled with -300", s, *d)
	}
	// The next run lists it, and a repayment of 50.00.
	e.cardRun(6, "card:5678", -9725, txn("c4", "card:5678", -300, cardDay.AddDate(0, 0, 4)),
		txn("c5", "card:5678", 5000, cardDay.AddDate(0, 0, 5)))
	if s, d := e.cardState(slug); s != store.StateReconciled || *d != 0 {
		t.Fatalf("after the purchase is listed: %s %v", s, *d)
	}
}

// AC2 of the card check: a card transaction whose sign is flipped (a purchase the ledger holds as a
// credit) is a mismatch once two readings show it, next to a card whose same purchase is signed
// right. The difference is twice the amount, it reaches the alert through agent.coverage, an
// operator can accept it, and a rebuild from raw gives the same state.
func TestCardCheckFailsWhenATransactionSignIsFlipped(t *testing.T) {
	e := newEnv(t)
	e.cardAccounts("card:5678", "card:9012")
	for run := int64(1); run <= 2; run++ {
		e.cardRun(run, "card:5678", -1000, txn("f1", "card:5678", -1000, cardDay))
		e.cardRun(run, "card:9012", -1000, txn("r1", "card:9012", -1000, cardDay))
	}
	for _, slug := range []string{"lloyds-card-5678", "lloyds-card-9012"} {
		if s, _ := e.cardState(slug); s != store.StateReconciled {
			t.Fatalf("%s before the purchase: %s", slug, s)
		}
	}
	// Each card spends 24.25 and owes 34.25; 5678's purchase is stored as +24.25.
	for run := int64(3); run <= 4; run++ {
		e.cardRun(run, "card:5678", -3425, txn("f2", "card:5678", 2425, cardDay.AddDate(0, 0, 1)))
		e.cardRun(run, "card:9012", -3425, txn("r2", "card:9012", -2425, cardDay.AddDate(0, 0, 1)))
	}
	if s, d := e.cardState("lloyds-card-5678"); s != store.StateMismatch || d == nil || *d != -4850 {
		t.Fatalf("the flipped card: %s %v, want mismatch with -4850", s, d)
	}
	if r := e.recon(wantAccountID(lloydsSource, "card:5678")); r.LastCheckOK == nil || *r.LastCheckOK {
		t.Fatalf("the flipped card's last check: %+v", r)
	}
	if s, d := e.cardState("lloyds-card-9012"); s != store.StateReconciled || *d != 0 {
		t.Fatalf("the card signed right: %s %v", s, *d)
	}
	// A rebuild from raw gives the same state.
	if diffs := e.renormalise(true); len(diffs) != 0 {
		t.Fatalf("verify after the card checks: %+v", diffs)
	}
	// An operator can accept the difference.
	e.operator(ledger.EntryAcceptDifference, ledger.AcceptDifference{Slug: "lloyds-card-5678", DiffMinor: ptr(int64(-4850)), Reason: "made-up"})
	e.catchUp()
	if s, _ := e.cardState("lloyds-card-5678"); s != store.StateReconciled {
		t.Fatalf("after accepting the difference: %s", s)
	}
}

// The applier compares a card list with the source's card accounts that have not ended: each one
// missing gets one warning with the source and a count, and the count is in the stats. A card that ended is not
// missing, and a source whose newest list is complete reports 0.
func TestCardListWarnsOfHeldCardsThatWentMissing(t *testing.T) {
	e := newEnv(t)
	e.cardAccounts("card:5678", "card:9012", "card:3456")
	if stats := e.catchUp(); stats.CardsUnlisted[lloydsSource] != 0 {
		t.Fatalf("a complete list: %v", stats.CardsUnlisted)
	}
	if strings.Contains(e.logs.String(), "card accounts not in the newest card list") {
		t.Fatalf("a warning for a complete list: %s", e.logs)
	}
	last := "2030-01-31"
	e.end("lloyds-card-3456", &last)
	e.logs.Reset()

	// The newest list holds only 9012: 5678 is missing, 3456 has ended.
	e.respondAs(e.fetchAs(lloydsSource, 2, "", nil), lloydsSource, "/cards", 200,
		body(fBody{CardLists: []fCardList{{Source: lloydsSource, Keys: []string{"card:9012"}}}}))
	stats := e.catchUp()
	if got := stats.CardsUnlisted[lloydsSource]; got != 1 {
		t.Fatalf("CardsUnlisted = %v, want 1 for the source", stats.CardsUnlisted)
	}
	logged := e.logs.String()
	if n := strings.Count(logged, "card accounts not in the newest card list"); n != 1 {
		t.Fatalf("%d warnings: %s", n, logged)
	}
	if !strings.Contains(logged, `"count":1`) || !strings.Contains(logged, `"source":"`+lloydsSource+`"`) ||
		!strings.Contains(logged, `"level":"WARN"`) || strings.Contains(logged, "lloyds-card") || strings.Contains(logged, "5678") {
		t.Errorf("warning is not the source and a count only: %s", logged)
	}
	// An empty list: every card that has not ended is missing.
	e.respondAs(e.fetchAs(lloydsSource, 3, "", nil), lloydsSource, "/cards", 200,
		body(fBody{CardLists: []fCardList{{Source: lloydsSource, Keys: []string{}}}}))
	if got := e.catchUp().CardsUnlisted[lloydsSource]; got != 2 {
		t.Errorf("CardsUnlisted after an empty list = %d, want 2", got)
	}
	// The held accounts are left open: only an operator ends one.
	if a := e.account("lloyds-card-5678"); a.EndedOn != nil {
		t.Errorf("the missing card's account was ended: %+v", a)
	}
	// A list with no source cannot be applied.
	e.respondAs(e.fetchAs(lloydsSource, 4, "", nil), lloydsSource, "/cards", 200,
		body(fBody{CardLists: []fCardList{{Keys: []string{"card:9012"}}}}))
	if stats := e.catchUp(); stats.Unparseable != 1 {
		t.Errorf("a card list with no source: %+v", stats)
	}
}

package ledger_test

import (
	"strings"
	"testing"
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
// arrive in, and is balances_only: nothing is checked against its balance.
func TestCreditCardAccountsAreSluggedByTheirKeyAndBalancesOnly(t *testing.T) {
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
		if r := e.recon(a.ID); r.Method != "balances_only" {
			t.Errorf("%s reconciliation method %q, want balances_only", slug, r.Method)
		}
	}
	// A balance reading is recorded and never checked.
	acc := wantAccountID(lloydsSource, "card:5678")
	e.balance(1, "card:5678", -12345)
	e.catchUp()
	if r := e.recon(acc); r.LastCheckAt != nil || r.LastCheckOK != nil {
		t.Errorf("a card balance was checked: %+v", r)
	}
	// agent.coverage reports no gaps for a balances_only account.
	if row := e.coverageRow("lloyds-card-5678"); row.ReconciliationState != "balances_only" || len(row.Gaps) != 0 {
		t.Errorf("coverage %+v", row)
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

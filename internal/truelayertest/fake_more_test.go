package truelayertest

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// /me lists the granted scopes, every one by default, and the token reply carries the same list.
// SetScopes narrows them for the provider, and a path whose scope is not granted answers 403
// access_denied.
func TestScopesAreServedAndGateTheirPaths(t *testing.T) {
	f, c := setup(t)
	f.RegisterCode(lloyds, "code-a")
	st, doc := c.do("POST", TokenPath, url.Values{"grant_type": {"authorization_code"}, "code": {"code-a"},
		"client_id": {DefaultClientID}, "client_secret": {DefaultClientSecret}, "redirect_uri": {"x"}}, "")
	if st != 200 || doc["scope"] != strings.Join(AllScopes, " ") {
		t.Fatalf("token reply scope %v, want %q", doc["scope"], strings.Join(AllScopes, " "))
	}
	access := doc["access_token"].(string)
	scopes := func() []string {
		_, doc := c.do("GET", "/data/v1/me", nil, access)
		var out []string
		for _, s := range list(doc)[0]["scopes"].([]any) {
			out = append(out, s.(string))
		}
		return out
	}
	if got := scopes(); strings.Join(got, " ") != strings.Join(AllScopes, " ") {
		t.Fatalf("/me scopes %v, want every scope", got)
	}
	for _, path := range []string{"/data/v1/cards", "/data/v1/info"} {
		if st, _ := c.do("GET", path, nil, access); st != 200 {
			t.Errorf("%s with every scope: %d", path, st)
		}
	}

	f.SetScopes(lloyds, "accounts", "balance", "transactions")
	if got := scopes(); strings.Join(got, " ") != "accounts balance transactions" {
		t.Fatalf("/me scopes after SetScopes: %v", got)
	}
	for _, path := range []string{"/data/v1/cards", "/data/v1/info", "/data/v1/accounts/x/standing_orders"} {
		if st, doc := c.do("GET", path, nil, access); st != 403 || doc["error"] != "access_denied" {
			t.Errorf("%s without its scope: %d %v", path, st, doc)
		}
	}
	if st, _ := c.do("GET", "/data/v1/accounts", nil, access); st != 200 {
		t.Errorf("/accounts with its scope: %d", st)
	}
	// A code made by RegisterCode gets the provider's scopes in its token reply.
	f.RegisterCode(lloyds, "code-b")
	if _, doc := c.do("POST", TokenPath, url.Values{"grant_type": {"authorization_code"}, "code": {"code-b"},
		"client_id": {DefaultClientID}, "client_secret": {DefaultClientSecret}, "redirect_uri": {"x"}}, ""); doc["scope"] != "accounts balance transactions" {
		t.Errorf("token scope after SetScopes: %v", doc["scope"])
	}
}

// Cards are listed with a fresh id each time, their balance and transactions are served under the
// id of any listing, pending transactions are served for an account and for a card, and each card
// path has its own daily count per card.
func TestCardsPendingAndInfo(t *testing.T) {
	f, c := setup(t)
	card := f.AddCard(lloyds, Card{PartialNumber: "5678", DisplayName: "MADE UP CARD"})
	f.AddCard(lloyds, Card{PartialNumber: "9012", DisplayName: "OTHER MADE UP CARD"})
	f.SetCardBalance(card, 10000, 90000)
	f.AddCardTransaction(card, CardTxn{Type: "DEBIT", Amount: "24.25", Date: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), Description: "MADE UP SHOP"})
	f.AddCardTransaction(card, CardTxn{Type: "DEBIT", Amount: "-3.00", Date: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), Description: "MADE UP CAFE"})
	f.AddCardTransaction(card, CardTxn{Amount: "1.00", Date: time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC), Description: "NO TYPE"})
	f.AddPending(lloyds, "acc-joint", PendingTxn{Date: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC), Amount: "-7.50", Description: "MADE UP PENDING"})
	f.AddPending(lloyds, card.ID, PendingTxn{Date: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC), Type: "DEBIT", Amount: "5.00", Description: "CARD PENDING"})
	f.RegisterCode(lloyds, "code-a")
	access, _ := c.exchange("code-a")

	_, d1 := c.do("GET", "/data/v1/cards", nil, access)
	_, d2 := c.do("GET", "/data/v1/cards", nil, access)
	cards1, cards2 := list(d1), list(d2)
	if len(cards1) != 2 || cards1[0]["partial_card_number"] != "5678" || cards1[0]["display_name"] != "MADE UP CARD" ||
		cards1[0]["card_type"] != "CREDIT" || cards1[0]["currency"] != "GBP" || cards1[0]["account_id"] == cards2[0]["account_id"] {
		t.Fatalf("cards %v / %v", cards1, cards2)
	}
	id := cards1[0]["account_id"].(string)
	st, doc := c.do("GET", "/data/v1/cards/"+id+"/balance", nil, access)
	bal := list(doc)
	if st != 200 || len(bal) != 1 || bal[0]["current"] != json.Number("100.00") || bal[0]["available"] != json.Number("900.00") ||
		bal[0]["credit_limit"] == nil {
		t.Fatalf("card balance %d %v", st, doc)
	}
	st, doc = c.do("GET", "/data/v1/cards/"+cards2[0]["account_id"].(string)+"/transactions?from=2021-01-01&to=2026-11-11", nil, access)
	txs := list(doc)
	if st != 200 || len(txs) != 3 || txs[0]["amount"] != json.Number("24.25") || txs[0]["transaction_type"] != "DEBIT" ||
		txs[1]["amount"] != json.Number("-3.00") || txs[2]["transaction_type"] != nil || txs[0]["meta"].(map[string]any)["cardNumber"] == nil {
		t.Fatalf("card transactions %d %v", st, doc)
	}
	if st, doc := c.do("GET", "/data/v1/cards/"+id+"/transactions?from=2020-12-31&to=2026-11-11", nil, access); st != 400 || doc["error"] != "invalid_date_range" {
		t.Errorf("card history before the earliest date: %d %v", st, doc)
	}
	st, doc = c.do("GET", "/data/v1/cards/"+id+"/transactions/pending", nil, access)
	if p := list(doc); st != 200 || len(p) != 1 || p[0]["description"] != "CARD PENDING" || p[0]["transaction_type"] != "DEBIT" {
		t.Fatalf("card pending %d %v", st, doc)
	}
	_, accts := c.do("GET", "/data/v1/accounts", nil, access)
	st, doc = c.do("GET", "/data/v1/accounts/"+list(accts)[0]["account_id"].(string)+"/transactions/pending", nil, access)
	if p := list(doc); st != 200 || len(p) != 1 || p[0]["description"] != "MADE UP PENDING" || p[0]["amount"] != json.Number("-7.50") {
		t.Fatalf("account pending %d %v", st, doc)
	}
	st, doc = c.do("GET", "/data/v1/info", nil, access)
	if r := list(doc); st != 200 || len(r) != 1 || r[0]["full_name"] == nil {
		t.Fatalf("info %d %v", st, doc)
	}
	if st, _ := c.do("GET", "/data/v1/cards/not-served/balance", nil, access); st != 404 {
		t.Errorf("a card id never served: %d", st)
	}

	// Four balance calls a day for each card, counted apart: one was made above, so three more fit
	// and the next is refused, while the other card still has all four.
	for i := range DailyLimit - 1 {
		if st, _ := c.do("GET", "/data/v1/cards/"+id+"/balance", nil, access); st != 200 {
			t.Fatalf("balance call %d: %d", i+2, st)
		}
	}
	if st, _ := c.do("GET", "/data/v1/cards/"+id+"/balance", nil, access); st != 429 {
		t.Errorf("card balance past its daily limit: %d", st)
	}
	_, d3 := c.do("GET", "/data/v1/cards", nil, access)
	if st, _ := c.do("GET", "/data/v1/cards/"+list(d3)[1]["account_id"].(string)+"/balance", nil, access); st != 200 {
		t.Errorf("the other card's balance after the first card's limit: %d", st)
	}
	if n := f.Count("/data/v1/cards/{id}/transactions/pending"); n != 1 {
		t.Errorf("Count of the card pending template = %d, want 1", n)
	}
}

// FailWith answers the next n calls to a template with the status and error code given; a plain
// failure carries no code.
func TestFailWithStatusAndCode(t *testing.T) {
	f, c := setup(t)
	f.RegisterCode(lloyds, "code-a")
	access, _ := c.exchange("code-a")
	f.FailWith(lloyds, "/data/v1/info", http.StatusNotImplemented, "endpoint_not_supported", 1)
	if st, doc := c.do("GET", "/data/v1/info", nil, access); st != 501 || doc["error"] != "endpoint_not_supported" {
		t.Errorf("501: %d %v", st, doc)
	}
	if st, _ := c.do("GET", "/data/v1/info", nil, access); st != 200 {
		t.Errorf("after the one failure: %d", st)
	}
	f.FailWith(lloyds, "/data/v1/info", http.StatusForbidden, "", 1)
	if st, doc := c.do("GET", "/data/v1/info", nil, access); st != 403 || doc["error"] != "" {
		t.Errorf("plain 403: %d %v", st, doc)
	}
	f.FailWith(lloyds, "/data/v1/info", http.StatusTooManyRequests, "provider_too_many_requests", 1)
	if st, _ := c.do("GET", "/data/v1/info", nil, access); st != 429 {
		t.Errorf("429: %d", st)
	}
}

// SetSCAWindow makes the two mandate reads answer 403 sca_exceeded once the window after the code
// exchange has passed, and nothing else; ClearSCAWindow undoes it.
func TestSCAWindowRefusesOnlyTheMandateReadsAfterTheWindow(t *testing.T) {
	f, c := setup(t)
	f.SetSCAWindow(lloyds, 10*time.Minute)
	f.RegisterCode(lloyds, "code-a")
	access, _ := c.exchange("code-a")
	accountID := func() string {
		_, d := c.do("GET", "/data/v1/accounts", nil, access)
		return list(d)[0]["account_id"].(string)
	}
	id := accountID()
	for _, path := range []string{"/standing_orders", "/direct_debits"} {
		if st, _ := c.do("GET", "/data/v1/accounts/"+id+path, nil, access); st != 200 {
			t.Errorf("%s inside the window: %d", path, st)
		}
	}
	f.Advance(11 * time.Minute)
	id = accountID()
	for _, path := range []string{"/standing_orders", "/direct_debits"} {
		st, doc := c.do("GET", "/data/v1/accounts/"+id+path, nil, access)
		desc, _ := doc["error_description"].(string)
		if st != 403 || doc["error"] != "sca_exceeded" || !strings.Contains(desc, "SCA exemption has expired") {
			t.Errorf("%s after the window: %d %v", path, st, doc)
		}
	}
	for _, path := range []string{"/balance", "/transactions?from=2024-08-01&to=2026-11-11"} {
		if st, _ := c.do("GET", "/data/v1/accounts/"+id+path, nil, access); st != 200 {
			t.Errorf("%s is not a mandate read: %d", path, st)
		}
	}
	f.ClearSCAWindow(lloyds)
	if st, _ := c.do("GET", "/data/v1/accounts/"+id+"/direct_debits", nil, access); st != 200 {
		t.Errorf("after ClearSCAWindow: %d", st)
	}
	// A window of zero refuses every read, and a new code exchange restarts the window.
	f.SetSCAWindow(lloyds, 0)
	if st, _ := c.do("GET", "/data/v1/accounts/"+id+"/direct_debits", nil, access); st != 403 {
		t.Errorf("zero window: %d", st)
	}
	f.SetSCAWindow(lloyds, time.Hour)
	f.RegisterCode(lloyds, "code-b")
	access, _ = c.exchange("code-b")
	id = accountID()
	if st, _ := c.do("GET", "/data/v1/accounts/"+id+"/direct_debits", nil, access); st != 200 {
		t.Errorf("after a fresh exchange: %d", st)
	}
}

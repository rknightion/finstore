package truelayertest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const lloyds = "ob-lloyds"

type client struct {
	t   *testing.T
	srv *httptest.Server
}

func (c client) do(method, path string, form url.Values, bearer string) (int, map[string]any) {
	c.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, c.srv.URL+path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc map[string]any
	if resp.StatusCode == http.StatusFound {
		return resp.StatusCode, map[string]any{"location": resp.Header.Get("Location")}
	}
	d := json.NewDecoder(resp.Body)
	d.UseNumber()
	if err := d.Decode(&doc); err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp.StatusCode, doc
}

func (c client) exchange(code string) (access, refresh string) {
	c.t.Helper()
	st, doc := c.do("POST", TokenPath, url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"client_id": {DefaultClientID}, "client_secret": {DefaultClientSecret}, "redirect_uri": {"https://finstore.example.test/callback/truelayer"}}, "")
	if st != 200 {
		c.t.Fatalf("exchange: %d %v", st, doc)
	}
	return doc["access_token"].(string), doc["refresh_token"].(string)
}

func list(doc map[string]any) []map[string]any {
	var out []map[string]any
	for _, x := range doc["results"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

func setup(t *testing.T, opts ...Option) (*Fake, client) {
	f := New(opts...)
	f.AddAccount(lloyds, Account{ID: "acc-joint", Type: "TRANSACTION", DisplayName: "Joint", SortCode: "112233", Number: "12345678", Opening: 10000})
	f.AddTransaction(lloyds, "acc-joint", Transaction{Date: time.Date(2024, 8, 22, 0, 0, 0, 0, time.UTC), Amount: -1230, Description: "MADE UP SHOP"})
	f.AddTransaction(lloyds, "acc-joint", Transaction{Date: time.Date(2024, 9, 1, 0, 0, 0, 0, time.UTC), Amount: 5000, Description: "MADE UP PAY"})
	f.AddDirectDebit(lloyds, "acc-joint", DirectDebit{Name: "MADE UP CO", Status: "Active", MandateID: "MANDATE-1",
		PreviousDate: time.Date(2024, 9, 1, 0, 0, 0, 0, time.UTC), PreviousAmount: -2500})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, client{t: t, srv: srv}
}

// The auth dialog sends the owner back with a code and the state; the code buys one token pair,
// which reads the data and reports the consent's end.
func TestAuthTokenAndData(t *testing.T) {
	f, c := setup(t)
	link := c.srv.URL + "/?" + url.Values{"response_type": {"code"}, "client_id": {DefaultClientID},
		"redirect_uri": {"https://finstore.example.test/callback/truelayer"}, "provider_id": {lloyds},
		"providers": {"uk-ob-all"}, "state": {"made-up-state"}, "scope": {"info accounts"}}.Encode()
	st, doc := c.do("GET", strings.TrimPrefix(link, c.srv.URL), nil, "")
	if st != http.StatusFound {
		t.Fatalf("auth dialog: %d %v", st, doc)
	}
	back, _ := url.Parse(doc["location"].(string))
	if back.Query().Get("state") != "made-up-state" || back.Query().Get("code") == "" || back.Host != "finstore.example.test" {
		t.Fatalf("redirect %v", back)
	}
	access, _ := c.exchange(back.Query().Get("code"))
	if st, _ := c.do("POST", TokenPath, url.Values{"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")},
		"client_id": {DefaultClientID}, "client_secret": {DefaultClientSecret}, "redirect_uri": {"x"}}, ""); st != 400 {
		t.Errorf("a used code was accepted: %d", st)
	}
	st, doc = c.do("GET", "/data/v1/me", nil, access)
	if st != 200 || list(doc)[0]["consent_expires_at"] != f.ConsentEnd(lloyds).Format(time.RFC3339) {
		t.Fatalf("/me: %d %v", st, doc)
	}
	st, doc = c.do("GET", "/data/v1/accounts", nil, access)
	accts := list(doc)
	if st != 200 || len(accts) != 1 || accts[0]["display_name"] != "Joint" ||
		accts[0]["account_number"].(map[string]any)["sort_code"] != "112233" {
		t.Fatalf("/accounts: %d %v", st, doc)
	}
	id := accts[0]["account_id"].(string)
	st, doc = c.do("GET", "/data/v1/accounts/"+id+"/transactions?from=2024-08-01&to=2026-11-11", nil, access)
	txs := list(doc)
	if st != 200 || len(txs) != 2 || txs[0]["amount"] != json.Number("-12.30") ||
		txs[1]["running_balance"].(map[string]any)["amount"] != json.Number("137.70") {
		t.Fatalf("transactions: %d %v", st, doc)
	}
	st, doc = c.do("GET", "/data/v1/accounts/"+id+"/balance", nil, access)
	if st != 200 || list(doc)[0]["current"] != json.Number("137.70") {
		t.Fatalf("balance: %d %v", st, doc)
	}
	if st, _ := c.do("POST", "/data/v1/accounts", nil, access); st != http.StatusMethodNotAllowed || len(f.Violations()) != 1 {
		t.Errorf("a POST to a data path: %d, violations %d", st, len(f.Violations()))
	}
	if st, _ := c.do("GET", "/data/v1/accounts", nil, "not-a-token"); st != 401 {
		t.Errorf("unknown token: %d", st)
	}
}

// A range starting before the earliest date is refused; a failing transactions call fails once.
func TestTransactionsRefusalsAndFailures(t *testing.T) {
	f, c := setup(t, WithEarliestFrom(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)))
	f.RegisterCode(lloyds, "code-a")
	access, _ := c.exchange("code-a")
	_, doc := c.do("GET", "/data/v1/accounts", nil, access)
	id := list(doc)[0]["account_id"].(string)
	path := "/data/v1/accounts/" + id + "/transactions"
	if st, doc := c.do("GET", path+"?from=2023-12-31&to=2026-11-11", nil, access); st != 400 || doc["error"] != "invalid_date_range" {
		t.Errorf("early range: %d %v", st, doc)
	}
	f.FailTransactions(lloyds, 1)
	if st, _ := c.do("GET", path+"?from=2024-01-01&to=2026-11-11", nil, access); st != 500 {
		t.Errorf("failing call: %d", st)
	}
	f.SetHistoryStart(lloyds, time.Date(2024, 9, 1, 0, 0, 0, 0, time.UTC))
	if st, doc := c.do("GET", path+"?from=2024-01-01&to=2026-11-11", nil, access); st != 200 || len(list(doc)) != 1 {
		t.Errorf("shorter history: %d %v", st, doc)
	}
}

// Four calls a day per endpoint and account, then 429 until the next day.
func TestDailyLimit(t *testing.T) {
	f, c := setup(t)
	f.RegisterCode(lloyds, "code-a")
	access, _ := c.exchange("code-a")
	_, doc := c.do("GET", "/data/v1/accounts", nil, access)
	id := list(doc)[0]["account_id"].(string)
	path := "/data/v1/accounts/" + id + "/balance"
	for i := range DailyLimit {
		if st, _ := c.do("GET", path, nil, access); st != 200 {
			t.Fatalf("call %d: %d", i+1, st)
		}
	}
	if st, _ := c.do("GET", path, nil, access); st != 429 {
		t.Fatalf("fifth call: %d", st)
	}
	// Another endpoint of the same account has its own count.
	if st, _ := c.do("GET", "/data/v1/accounts/"+id+"/direct_debits", nil, access); st != 200 {
		t.Fatalf("other endpoint: %d", st)
	}
	f.Advance(12 * time.Hour) // 2026-11-12 00:00 in London
	f.RegisterCode(lloyds, "code-b")
	access, _ = c.exchange("code-b")
	if st, _ := c.do("GET", path, nil, access); st != 200 {
		t.Fatalf("next day: %d", st)
	}
	f.Fail429(lloyds, "/data/v1/accounts/{id}/balance", 1)
	if st, _ := c.do("GET", path, nil, access); st != 429 {
		t.Fatalf("forced 429: %d", st)
	}
}

// Transaction and direct debit ids change on every call and account ids on every listing; the
// stable ids and older account ids keep working.
func TestIDsChangeBetweenCalls(t *testing.T) {
	f, c := setup(t)
	f.RegisterCode(lloyds, "code-a")
	access, _ := c.exchange("code-a")
	_, d1 := c.do("GET", "/data/v1/accounts", nil, access)
	_, d2 := c.do("GET", "/data/v1/accounts", nil, access)
	id1, id2 := list(d1)[0]["account_id"].(string), list(d2)[0]["account_id"].(string)
	if id1 == id2 {
		t.Fatalf("account id did not change: %s", id1)
	}
	get := func(kind string) map[string]any {
		_, doc := c.do("GET", "/data/v1/accounts/"+id1+"/"+kind+"?from=2024-08-01&to=2026-11-11", nil, access)
		return list(doc)[0]
	}
	t1, t2 := get("transactions"), get("transactions")
	if t1["transaction_id"] == t2["transaction_id"] || t1["normalised_provider_transaction_id"] != t2["normalised_provider_transaction_id"] {
		t.Errorf("transaction ids %v / %v", t1, t2)
	}
	dd1, dd2 := get("direct_debits"), get("direct_debits")
	if dd1["direct_debit_id"] == dd2["direct_debit_id"] ||
		dd1["meta"].(map[string]any)["provider_mandate_identification"] != "MANDATE-1" {
		t.Errorf("direct debit ids %v / %v", dd1, dd2)
	}
}

// An expired consent refuses data with access_denied; a revoked one refuses the refresh with
// invalid_grant and data with 401.
func TestConsentExpiryAndRevocation(t *testing.T) {
	f, c := setup(t)
	f.RegisterCode(lloyds, "code-a")
	access, _ := c.exchange("code-a")
	f.ExpireConsent(lloyds)
	if st, doc := c.do("GET", "/data/v1/accounts", nil, access); st != 403 || doc["error"] != "access_denied" {
		t.Errorf("expired consent: %d %v", st, doc)
	}
	f.RegisterCode(lloyds, "code-b")
	access, refresh := c.exchange("code-b")
	refreshForm := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh},
		"client_id": {DefaultClientID}, "client_secret": {DefaultClientSecret}}
	st, doc := c.do("POST", TokenPath, refreshForm, "")
	if st != 200 || doc["refresh_token"] != refresh || doc["access_token"] == access {
		t.Fatalf("refresh: %d %v", st, doc)
	}
	f.RevokeConsent(lloyds)
	if st, doc := c.do("POST", TokenPath, refreshForm, ""); st != 400 || doc["error"] != "invalid_grant" {
		t.Errorf("refresh after revocation: %d %v", st, doc)
	}
	if st, _ := c.do("GET", "/data/v1/accounts", nil, doc["access_token"].(string)); st != 401 {
		t.Errorf("data after revocation: %d", st)
	}
}

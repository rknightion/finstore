package monzotest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

const acct = "acc_test_0001"

type tokens struct {
	Access    string `json:"access_token"`
	Refresh   string `json:"refresh_token"`
	ExpiresIn int    `json:"expires_in"`
}

type client struct {
	t     *testing.T
	base  string
	token string
}

func (c *client) do(method, path string, q url.Values) (int, []byte) {
	c.t.Helper()
	u := c.base + path
	if q != nil {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (c *client) get(path string, q url.Values) (int, []byte) {
	c.t.Helper()
	return c.do(http.MethodGet, path, q)
}

// code returns the "code" field of a JSON error body.
func code(t *testing.T, b []byte) string {
	t.Helper()
	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("not JSON: %q", b)
	}
	return e.Code
}

func postToken(t *testing.T, base string, form url.Values) (int, []byte, error) {
	t.Helper()
	if form.Get("client_id") == "" {
		form.Set("client_id", DefaultClientID)
		form.Set("client_secret", DefaultClientSecret)
	}
	resp, err := http.PostForm(base+TokenPath, form)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

func exchange(t *testing.T, f *Fake, c string) tokens {
	t.Helper()
	f.RegisterCode(c)
	status, b, err := postToken(t, f.URL(), url.Values{"grant_type": {"authorization_code"}, "code": {c}})
	if err != nil || status != 200 {
		t.Fatalf("exchange: %d %v %s", status, err, b)
	}
	var tk tokens
	if err := json.Unmarshal(b, &tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

// setup returns a started fake with one open account, an issued and approved token.
func setup(t *testing.T, opts ...Option) (*Fake, *client) {
	t.Helper()
	f := New(opts...)
	srv := f.Start()
	t.Cleanup(srv.Close)
	f.AddAccount(Account{ID: acct, Type: "uk_retail", Created: f.Now().Add(-1000 * 24 * time.Hour)})
	tk := exchange(t, f, "code_test_0001")
	f.Approve()
	return f, &client{t: t, base: f.URL(), token: tk.Access}
}

func txIDs(t *testing.T, b []byte) (ids []string, last Transaction) {
	t.Helper()
	var r struct {
		Transactions []struct {
			ID      string    `json:"id"`
			Created time.Time `json:"created"`
		} `json:"transactions"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	for _, x := range r.Transactions {
		ids = append(ids, x.ID)
		last = Transaction{ID: x.ID, Created: x.Created}
	}
	return ids, last
}

func TestIDCursorSkipsSharedTimestamps(t *testing.T) {
	f, c := setup(t)
	now := f.Now()
	// 12 rows in 3 groups of 4 that share a timestamp; limit 6 ends mid-group.
	f.GenerateTransactions(acct, 12, now.Add(-30*24*time.Hour), now.Add(-10*24*time.Hour), 4)

	page := func(since string) ([]string, Transaction) {
		q := url.Values{"account_id": {acct}, "limit": {"6"}}
		if since != "" {
			q.Set("since", since)
		}
		status, b := c.get("/transactions", q)
		if status != 200 {
			t.Fatalf("status %d %s", status, b)
		}
		return txIDs(t, b)
	}

	// Id cursor.
	got := map[string]bool{}
	ids, last := page("")
	for len(ids) > 0 {
		for _, id := range ids {
			got[id] = true
		}
		ids, last = page(last.ID)
	}
	if len(got) != 10 {
		t.Errorf("id cursor returned %d rows, want 10 (2 skipped)", len(got))
	}

	// Timestamp cursor with de-duplication.
	got = map[string]bool{}
	ids, last = page("")
	for {
		fresh := 0
		for _, id := range ids {
			if !got[id] {
				got[id] = true
				fresh++
			}
		}
		if fresh == 0 {
			break
		}
		ids, last = page(last.Created.Format(time.RFC3339Nano))
	}
	if len(got) != 12 {
		t.Errorf("timestamp cursor returned %d rows, want 12", len(got))
	}
}

func TestOrderingLimitAndBounds(t *testing.T) {
	f, c := setup(t)
	now := f.Now()
	f.GenerateTransactions(acct, 150, now.Add(-20*24*time.Hour), now.Add(-10*24*time.Hour), 1)

	_, b := c.get("/transactions", url.Values{"account_id": {acct}})
	if ids, _ := txIDs(t, b); len(ids) != 30 || ids[0] != "tx_test_000001" {
		t.Errorf("default page: %d rows, first %v", len(ids), ids)
	}
	_, b = c.get("/transactions", url.Values{"account_id": {acct}, "limit": {"500"}})
	if ids, _ := txIDs(t, b); len(ids) != 100 {
		t.Errorf("limit clamp: %d rows", len(ids))
	}
	before := now.Add(-15 * 24 * time.Hour).Format(time.RFC3339)
	_, b = c.get("/transactions", url.Values{"account_id": {acct}, "limit": {"100"}, "before": {before}})
	ids, last := txIDs(t, b)
	if len(ids) != 75 || !last.Created.Before(now.Add(-15*24*time.Hour)) {
		t.Errorf("before: %d rows, last %v", len(ids), last.Created)
	}
}

func TestRangeOver365DaysRejected(t *testing.T) {
	f, c := setup(t)
	now := f.Now()
	since := now.Add(-80 * 24 * time.Hour)
	q := func(before time.Time) url.Values {
		return url.Values{"account_id": {acct}, "since": {since.Format(time.RFC3339)}, "before": {before.Format(time.RFC3339)}}
	}
	if status, b := c.get("/transactions", q(since.Add(MaxRange))); status != 200 {
		t.Errorf("exactly 8760h: %d %s", status, b)
	}
	status, b := c.get("/transactions", q(since.Add(MaxRange+time.Second)))
	if status != 400 || code(t, b) != "bad_request.invalid_time_range" {
		t.Errorf("over 8760h: %d %s", status, b)
	}
}

func TestFullHistoryWindowCloses(t *testing.T) {
	f, c := setup(t)
	now := f.Now()
	f.AddTransaction(Transaction{AccountID: acct, Created: now.Add(-200 * 24 * time.Hour), Amount: -100})
	f.AddTransaction(Transaction{AccountID: acct, Created: now.Add(-5 * 24 * time.Hour), Amount: -200})
	old := url.Values{"account_id": {acct}, "since": {now.Add(-300 * 24 * time.Hour).Format(time.RFC3339)}}

	if status, b := c.get("/transactions", old); status != 200 {
		t.Fatalf("inside window: %d %s", status, b)
	}
	f.Advance(299 * time.Second)
	if status, _ := c.get("/transactions", old); status != 200 {
		t.Fatalf("299s: %d", status)
	}
	f.Advance(time.Second)
	status, b := c.get("/transactions", old)
	if status != 403 || code(t, b) != "forbidden.verification_required" {
		t.Fatalf("300s: %d %s", status, b)
	}
	if status, _ := c.get("/transactions", url.Values{"account_id": {acct}}); status != 403 {
		t.Errorf("unbounded start after window: %d", status)
	}
	recent := url.Values{"account_id": {acct}, "since": {f.Now().Add(-90 * 24 * time.Hour).Format(time.RFC3339)}}
	status, b = c.get("/transactions", recent)
	if ids, _ := txIDs(t, b); status != 200 || len(ids) != 1 {
		t.Errorf("recent range: %d %s", status, b)
	}
}

func TestApproveAndRevoke(t *testing.T) {
	f := New()
	srv := f.Start()
	defer srv.Close()
	f.AddAccount(Account{ID: acct, Type: "uk_retail"})
	c := &client{t: t, base: f.URL(), token: exchange(t, f, "c1").Access}

	status, b := c.get("/accounts", nil)
	if status != 403 || code(t, b) != "forbidden.insufficient_permissions" {
		t.Fatalf("before approval: %d %s", status, b)
	}
	if status, _ := c.get("/ping/whoami", nil); status != 200 {
		t.Errorf("whoami before approval: %d", status)
	}
	f.Approve()
	if status, _ := c.get("/accounts", nil); status != 200 {
		t.Errorf("after approval: %d", status)
	}
	f.Revoke()
	if status, _ := c.get("/accounts", nil); status != 403 {
		t.Errorf("after revoke: %d", status)
	}
	// A brand new token is unapproved.
	f.Approve()
	c.token = exchange(t, f, "c2").Access
	if status, _ := c.get("/balance", url.Values{"account_id": {acct}}); status != 403 {
		t.Errorf("new token: %d", status)
	}
}

func TestTokenRotation(t *testing.T) {
	f := New()
	srv := f.Start()
	defer srv.Close()
	f.AddAccount(Account{ID: acct, Type: "uk_retail"})

	first := exchange(t, f, "c1")
	if first.ExpiresIn != 21600 {
		t.Errorf("expires_in %d", first.ExpiresIn)
	}
	if status, _, _ := postToken(t, f.URL(), url.Values{"grant_type": {"authorization_code"}, "code": {"c1"}}); status != 400 {
		t.Errorf("code reuse: %d", status)
	}
	if status, _, _ := postToken(t, f.URL(), url.Values{"grant_type": {"authorization_code"}, "code": {"c1"}, "client_id": {"x"}, "client_secret": {"y"}}); status != 401 {
		t.Errorf("bad client: %d", status)
	}

	// A fresh authorization code invalidates the first access token.
	second := exchange(t, f, "c2")
	if status, _ := (&client{t: t, base: f.URL(), token: first.Access}).get("/ping/whoami", nil); status != 401 {
		t.Errorf("superseded access token: %d", status)
	}
	if status, _ := (&client{t: t, base: f.URL(), token: second.Access}).get("/ping/whoami", nil); status != 200 {
		t.Errorf("current access token: %d", status)
	}

	// Refresh rotates both tokens.
	status, b, _ := postToken(t, f.URL(), url.Values{"grant_type": {"refresh_token"}, "refresh_token": {second.Refresh}})
	var third tokens
	if err := json.Unmarshal(b, &third); status != 200 || err != nil {
		t.Fatalf("refresh: %d %s", status, b)
	}
	status, b, _ = postToken(t, f.URL(), url.Values{"grant_type": {"refresh_token"}, "refresh_token": {second.Refresh}})
	if status != 400 || !json.Valid(b) {
		t.Errorf("spent refresh token: %d %s", status, b)
	}
	if status, _ := (&client{t: t, base: f.URL(), token: second.Access}).get("/ping/whoami", nil); status != 401 {
		t.Errorf("rotated-out access token: %d", status)
	}

	// A lost response: the server rotates, the client sees a dropped connection.
	f.DropNextTokenResponse()
	if _, _, err := postToken(t, f.URL(), url.Values{"grant_type": {"refresh_token"}, "refresh_token": {third.Refresh}}); err == nil {
		t.Error("expected a dropped connection")
	}
	status, _, _ = postToken(t, f.URL(), url.Values{"grant_type": {"refresh_token"}, "refresh_token": {third.Refresh}})
	if status != 400 {
		t.Errorf("refresh token after lost response: %d", status)
	}

	f.FailNextToken(500)
	status, b, _ = postToken(t, f.URL(), url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"nope"}})
	if status != 500 || !json.Valid(b) {
		t.Errorf("FailNextToken: %d %s", status, b)
	}
}

func TestFaultInjection(t *testing.T) {
	f, c := setup(t)

	f.Fail429("/accounts", 2)
	for i := range 2 {
		if status, _ := c.get("/accounts", nil); status != 429 {
			t.Errorf("request %d: %d", i, status)
		}
	}
	if status, _ := c.get("/accounts", nil); status != 200 {
		t.Errorf("after 429s: %d", status)
	}

	f.FailNonJSON("/accounts", 1)
	status, b := c.get("/accounts", nil)
	if status != 502 || json.Valid(b) || !strings.Contains(string(b), "<html>") {
		t.Errorf("non-JSON: %d %q", status, b)
	}
	if status, b := c.get("/accounts", nil); status != 200 || !json.Valid(b) {
		t.Errorf("after non-JSON: %d", status)
	}

	f.WithDelay(2 * time.Second)
	t0 := f.Now()
	c.get("/ping/whoami", nil)
	c.get("/ping/whoami", nil)
	if d := f.Now().Sub(t0); d != 4*time.Second {
		t.Errorf("delay advanced clock by %v", d)
	}
	reqs := f.Requests()
	if n := len(reqs); reqs[n-1].Time.Sub(reqs[n-2].Time) != 2*time.Second {
		t.Errorf("logged times %v %v", reqs[n-2].Time, reqs[n-1].Time)
	}
}

func TestViolationLog(t *testing.T) {
	f, c := setup(t)
	c.get("/accounts", nil)
	if v := f.Violations(); len(v) != 0 {
		t.Fatalf("violations after reads: %v", v)
	}
	if status, _ := c.do(http.MethodPut, "/accounts", nil); status != 405 {
		t.Errorf("PUT status %d", status)
	}
	v := f.Violations()
	if len(v) != 1 || v[0].Method != http.MethodPut || v[0].Path != "/accounts" {
		t.Errorf("violations: %v", v)
	}
	if f.Count("/accounts") != 2 || f.Count(TokenPath) != 1 {
		t.Errorf("counts: accounts %d token %d", f.Count("/accounts"), f.Count(TokenPath))
	}
}

func TestAccountsBalancesPotsAndMerchant(t *testing.T) {
	f, c := setup(t)
	now := f.Now()
	f.AddAccount(Account{ID: "acc_test_0002", Type: "uk_loan", Closed: true})
	f.AddTransaction(Transaction{ID: "tx_test_a", AccountID: acct, Created: now.Add(-time.Hour), Amount: 1000})
	f.AddTransaction(Transaction{ID: "tx_test_b", AccountID: acct, Created: now.Add(-time.Hour), Amount: -300,
		Merchant: &Merchant{ID: "merch_test_0001", Name: "Example Merchant"}, CanSplitTheBill: true})
	f.AddTransaction(Transaction{ID: "tx_test_c", AccountID: acct, Created: now.Add(-time.Minute), Amount: -999, DeclineReason: "INSUFFICIENT_FUNDS"})
	f.AddPot(acct, Pot{ID: "pot_test_0001", Name: "Example Pot", Balance: 50})
	f.AddPot(acct, Pot{ID: "pot_test_0002", Name: "Old Pot", Balance: 7, Deleted: true})

	_, b := c.get("/accounts", nil)
	var accs struct {
		Accounts []struct{ ID string } `json:"accounts"`
	}
	_ = json.Unmarshal(b, &accs)
	if len(accs.Accounts) != 2 {
		t.Errorf("accounts: %s", b)
	}
	_, b = c.get("/accounts", url.Values{"account_type": {"uk_loan"}})
	_ = json.Unmarshal(b, &accs)
	if len(accs.Accounts) != 1 || accs.Accounts[0].ID != "acc_test_0002" {
		t.Errorf("filtered accounts: %s", b)
	}

	bal := func() (int64, int64) {
		_, b := c.get("/balance", url.Values{"account_id": {acct}})
		var r struct{ Balance, TotalBalance int64 }
		var raw map[string]any
		_ = json.Unmarshal(b, &raw)
		r.Balance, r.TotalBalance = int64(raw["balance"].(float64)), int64(raw["total_balance"].(float64))
		return r.Balance, r.TotalBalance
	}
	if b, tb := bal(); b != 700 || tb != 750 {
		t.Errorf("balance %d total %d", b, tb)
	}
	f.SetBalance(acct, 123)
	if b, _ := bal(); b != 123 {
		t.Errorf("override %d", b)
	}
	f.ClearBalance(acct)
	f.SetTransactionAmount("tx_test_a", 2000)
	if b, _ := bal(); b != 1700 {
		t.Errorf("after amount change %d", b)
	}

	_, b = c.get("/pots", url.Values{"current_account_id": {acct}})
	if strings.Count(string(b), "pot_test_") != 2 || !strings.Contains(string(b), `"deleted":true`) {
		t.Errorf("pots: %s", b)
	}
	_, b = c.get("/webhooks", url.Values{"account_id": {acct}})
	if string(b) != `{"webhooks":[]}` {
		t.Errorf("webhooks: %s", b)
	}

	_, b = c.get("/transactions", url.Values{"account_id": {acct}})
	if !strings.Contains(string(b), `"merchant":"merch_test_0001"`) || strings.Contains(string(b), "Example Merchant") {
		t.Errorf("unexpanded merchant: %s", b)
	}
	_, b = c.get("/transactions", url.Values{"account_id": {acct}, "expand[]": {"merchant"}})
	if !strings.Contains(string(b), `"name":"Example Merchant"`) || !strings.Contains(string(b), `"decline_reason":"INSUFFICIENT_FUNDS"`) {
		t.Errorf("expanded merchant: %s", b)
	}

	if !f.SettleTransaction("tx_test_b", now) || !f.RemoveTransaction("tx_test_c") || f.RemoveTransaction("tx_test_c") {
		t.Error("settle/remove helpers")
	}
	_, b = c.get("/transactions", url.Values{"account_id": {acct}})
	if strings.Contains(string(b), "tx_test_c") || !strings.Contains(string(b), `"settled":"`+now.UTC().Format(time.RFC3339Nano)+`"`) {
		t.Errorf("after helpers: %s", b)
	}
}

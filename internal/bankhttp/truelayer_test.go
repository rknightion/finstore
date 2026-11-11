package bankhttp

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

var templateRules = []Rule{
	{Method: "POST", Path: "/connect/token"},
	{Method: "GET", Path: "/data/v1/accounts", Record: true},
	{Method: "GET", Path: "/data/v1/accounts/{id}/balance", Record: true},
}

// A path-template rule allows one segment of letters, digits and hyphens in place of {id}, and the
// template, never the concrete path, is what the recorder and the error carry: the id comes from a
// bank response.
func TestPathTemplateRules(t *testing.T) {
	c, rec, _, n := newTest(t, []int{200, 404}, `{}`, templateRules)
	if _, err := c.Do(t.Context(), Request{Method: "GET", Path: "/data/v1/accounts/Ab-12-cd/balance"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.recs) != 1 || rec.recs[0].Endpoint != "/data/v1/accounts/{id}/balance" {
		t.Fatalf("recorded %+v", rec.recs)
	}
	_, err := c.Do(t.Context(), Request{Method: "GET", Path: "/data/v1/accounts/ID-MARKER/balance"})
	var e *Error
	if !errors.As(err, &e) || e.Path != "/data/v1/accounts/{id}/balance" || strings.Contains(err.Error(), "ID-MARKER") {
		t.Fatalf("err = %v", err)
	}
	for _, p := range []string{
		"/data/v1/accounts//balance", "/data/v1/accounts/a_b/balance", "/data/v1/accounts/a/b/balance",
		"/data/v1/accounts/a.b/balance", "/data/v1/accounts/{id}/balance", "/data/v1/accounts/a/balancex",
		"/data/v1/accounts/a%2Fb/balance", "/data/v1/accounts/a/balance/",
	} {
		if _, err := c.Do(t.Context(), Request{Method: "GET", Path: p}); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("%s: err = %v, want ErrNotAllowed", p, err)
		}
	}
	if _, err := c.Do(t.Context(), Request{Method: "POST", Path: "/data/v1/accounts/a/balance"}); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("POST on a GET template: err = %v", err)
	}
	if n.Load() != 2 {
		t.Errorf("server saw %d requests, want 2", n.Load())
	}
}

// The token path is set per client: a client whose token path is /connect/token treats a POST
// there as the token call, which is never repeated after a 5xx or a transport failure.
func TestTokenPathPerClient(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	sl := &sleeps{}
	c, _ := New(srv.URL, templateRules, WithTokenPath("/connect/token"), WithSleep(sl.sleep))
	if _, err := c.Do(t.Context(), Request{Method: "POST", Path: "/connect/token", Form: url.Values{}}); err == nil {
		t.Fatal("no error")
	}
	if n.Load() != 1 || len(sl.got) != 0 {
		t.Fatalf("token call made %d requests with sleeps %v, want 1 and none", n.Load(), sl.got)
	}
	netC, _ := New(closedURL(t), templateRules, WithTokenPath("/connect/token"), WithSleep(sl.sleep))
	if _, err := netC.Do(t.Context(), Request{Method: "POST", Path: "/connect/token", Form: url.Values{}}); err == nil {
		t.Fatal("no error")
	}
	if len(sl.got) != 0 {
		t.Fatalf("token call retried after a transport failure: %v", sl.got)
	}
}

// With retries turned off every call is exactly one attempt and one recorded response, whatever
// the failure: a 429, a 5xx or a transport failure.
func TestWithoutRetries(t *testing.T) {
	for _, status := range []int{429, 500, 503} {
		c, rec, sl, n := newTest(t, []int{status, 200}, `{}`, testRules)
		WithoutRetries()(c)
		if _, err := c.Do(t.Context(), Request{Method: "GET", Path: "/accounts"}); err == nil {
			t.Fatalf("%d: no error", status)
		}
		if n.Load() != 1 || len(rec.recs) != 1 || len(sl.got) != 0 {
			t.Errorf("%d: %d requests, %d recorded, sleeps %v; want one attempt", status, n.Load(), len(rec.recs), sl.got)
		}
	}
	sl := &sleeps{}
	c, _ := New(closedURL(t), testRules, WithRecorder(&fakeRecorder{}), WithSleep(sl.sleep), WithoutRetries())
	if _, err := c.Do(t.Context(), Request{Method: "GET", Path: "/accounts"}); err == nil {
		t.Fatal("no error")
	}
	if len(sl.got) != 0 {
		t.Errorf("transport failure retried: %v", sl.got)
	}
}

// An OAuth-style error body names its code in "error"; "code" wins when both are present, and a
// value that is not a well-formed code string is ignored.
func TestBankCodeFromErrorField(t *testing.T) {
	for body, want := range map[string]string{
		`{"error":"invalid_grant","error_description":"BODY-MARKER"}`: "invalid_grant",
		`{"error":"access_denied"}`:                                   "access_denied",
		`{"code":"from.code","error":"from_error"}`:                   "from.code",
		`{"code":"has spaces","error":"from_error"}`:                  "from_error",
		`{"error":{"nested":"object"}}`:                               "",
		`{"error":"has spaces BODY-MARKER"}`:                          "",
		`{"error":7,"code":"from.code"}`:                              "from.code",
	} {
		c, _, _, _ := newTest(t, []int{400}, body, testRules)
		_, err := c.Do(t.Context(), Request{Method: "GET", Path: "/accounts"})
		var e *Error
		if !errors.As(err, &e) || e.BankCode != want {
			t.Errorf("%s: got %+v, want code %q", body, e, want)
		}
	}
}

// Request parameters carried by the context reach the recorder with the response.
func TestRequestParamsFromContext(t *testing.T) {
	c, rec, _, _ := newTest(t, []int{200}, `{}`, templateRules)
	ctx := WithRequestParams(t.Context(), map[string]string{"account_id": "acc-1", "bank_ref": "112233 12345678"})
	if _, err := c.Do(ctx, Request{Method: "GET", Path: "/data/v1/accounts/acc-1/balance"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.recs) != 1 || rec.recs[0].Params["account_id"] != "acc-1" || rec.recs[0].Params["bank_ref"] != "112233 12345678" {
		t.Fatalf("recorded %+v", rec.recs)
	}
	if _, err := c.Do(t.Context(), Request{Method: "GET", Path: "/data/v1/accounts"}); err != nil {
		t.Fatal(err)
	}
	if rec.recs[1].Params != nil {
		t.Errorf("a request with no parameters recorded %v", rec.recs[1].Params)
	}
}

func TestTrueLayerRules(t *testing.T) {
	collect := func(rules []Rule) map[string]bool {
		got := map[string]bool{}
		for _, r := range rules {
			got[r.Method+" "+r.Path] = r.Record
		}
		return got
	}
	auth := collect(TrueLayerAuthRules())
	if rec, ok := auth["POST /connect/token"]; len(auth) != 1 || !ok || rec {
		t.Errorf("auth rules = %v", auth)
	}
	api := collect(TrueLayerAPIRules())
	want := []string{"GET /data/v1/me", "GET /data/v1/accounts", "GET /data/v1/accounts/{id}/balance",
		"GET /data/v1/accounts/{id}/transactions", "GET /data/v1/accounts/{id}/standing_orders",
		"GET /data/v1/accounts/{id}/direct_debits",
		"GET /data/v1/accounts/{id}/transactions/pending", "GET /data/v1/cards", "GET /data/v1/cards/{id}/balance",
		"GET /data/v1/cards/{id}/transactions", "GET /data/v1/cards/{id}/transactions/pending", "GET /data/v1/info"}
	if len(api) != len(want) {
		t.Fatalf("api rules = %v, want %d", api, len(want))
	}
	for _, k := range want {
		if rec, ok := api[k]; !ok || !rec {
			t.Errorf("%s: present=%v record=%v", k, ok, rec)
		}
	}
}

// Each of the six paths added for pending transactions, cards and identity is allowed as a GET with
// a one-segment id, and its neighbours are refused before anything is sent: a write, a longer or
// shorter path, a missing or nested id, and the paths TrueLayer has that finstore never calls.
func TestTrueLayerRulesAllowTheNewPathsAndNotTheirNeighbours(t *testing.T) {
	c, rec, _, n := newTest(t, []int{200, 200, 200, 200, 200, 200}, `{}`, TrueLayerAPIRules())
	for _, p := range []string{
		"/data/v1/accounts/Ab-12/transactions/pending", "/data/v1/cards", "/data/v1/cards/Cd-34/balance",
		"/data/v1/cards/Cd-34/transactions", "/data/v1/cards/Cd-34/transactions/pending", "/data/v1/info",
	} {
		if _, err := c.Do(t.Context(), Request{Method: "GET", Path: p}); err != nil {
			t.Errorf("GET %s: %v", p, err)
		}
	}
	if len(rec.recs) != 6 {
		t.Fatalf("recorded %d responses, want 6", len(rec.recs))
	}
	for _, e := range []string{"/data/v1/accounts/{id}/transactions/pending", "/data/v1/cards", "/data/v1/cards/{id}/balance",
		"/data/v1/cards/{id}/transactions", "/data/v1/cards/{id}/transactions/pending", "/data/v1/info"} {
		found := false
		for _, r := range rec.recs {
			found = found || r.Endpoint == e
		}
		if !found {
			t.Errorf("no response recorded under the template %s", e)
		}
	}
	seen := n.Load()
	for _, p := range []string{
		// a write on every new path
		"POST:/data/v1/cards", "POST:/data/v1/info", "POST:/data/v1/cards/a/transactions/pending",
		"POST:/data/v1/accounts/a/transactions/pending", "PUT:/data/v1/cards/a/balance", "DELETE:/data/v1/cards/a/transactions",
		// neighbours finstore never calls
		"GET:/data/v1/cards/a", "GET:/data/v1/cards/a/", "GET:/data/v1/cards//balance", "GET:/data/v1/cards/a/b/balance",
		"GET:/data/v1/cards/a/direct_debits", "GET:/data/v1/cards/a/standing_orders", "GET:/data/v1/cards/a/transactions/other",
		"GET:/data/v1/cards/a/transactions/pending/x", "GET:/data/v1/accounts/a/transactions/pending/x",
		"GET:/data/v1/accounts/a/pending", "GET:/data/v1/accounts/a/transactions/other", "GET:/data/v1/cards/",
		"GET:/data/v1/info/", "GET:/data/v1/info/x", "GET:/data/v1/identity", "GET:/data/v1/cards/%7Bid%7D/balance",
		"GET:/data/v1/cards/{id}/balance", "GET:/data/v1/cards/a_b/balance",
	} {
		method, path, _ := strings.Cut(p, ":")
		if _, err := c.Do(t.Context(), Request{Method: method, Path: path}); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("%s: err = %v, want ErrNotAllowed", p, err)
		}
	}
	if n.Load() != seen {
		t.Errorf("the server saw %d requests from refused calls", n.Load()-seen)
	}
}

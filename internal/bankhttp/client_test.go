package bankhttp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeRecorder struct {
	mu   sync.Mutex
	recs []Recorded
}

func (f *fakeRecorder) Record(_ context.Context, r Recorded) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, r)
	return nil
}

type sleeps struct{ got []time.Duration }

func (s *sleeps) sleep(_ context.Context, d time.Duration) error {
	s.got = append(s.got, d)
	return nil
}

func secs(n ...int) []time.Duration {
	out := make([]time.Duration, len(n))
	for i, v := range n {
		out[i] = time.Duration(v) * time.Second
	}
	return out
}

// newTest returns a client on a server that answers with the statuses in
// order (the last repeats), plus the request counter.
func newTest(t *testing.T, statuses []int, body string, rules []Rule) (*Client, *fakeRecorder, *sleeps, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := int(n.Add(1)) - 1
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statuses[min(i, len(statuses)-1)])
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	rec, sl := &fakeRecorder{}, &sleeps{}
	c, err := New(srv.URL, rules, WithRecorder(rec), WithSleep(sl.sleep))
	if err != nil {
		t.Fatal(err)
	}
	return c, rec, sl, &n
}

var testRules = []Rule{
	{Method: "POST", Path: "/oauth2/token"},
	{Method: "GET", Path: "/accounts", Record: true},
}

func TestNotAllowed(t *testing.T) {
	c, _, _, n := newTest(t, []int{200}, `{}`, testRules)
	for _, r := range []Request{
		{Method: "POST", Path: "/accounts"},
		{Method: "DELETE", Path: "/accounts"},
		{Method: "GET", Path: "/accounts/"},
		{Method: "GET", Path: "/accounts/acc_1"},
		{Method: "GET", Path: "/accountsx"},
		{Method: "GET", Path: "/"},
		{Method: "GET", Path: "/oauth2/token"},
		{Method: "get", Path: "/accounts"},
	} {
		if _, err := c.Do(t.Context(), r); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("%s %s: err = %v, want ErrNotAllowed", r.Method, r.Path, err)
		}
	}
	if n.Load() != 0 {
		t.Fatalf("server saw %d requests", n.Load())
	}
}

func TestMonzoRules(t *testing.T) {
	got := map[string]bool{}
	for _, r := range MonzoRules() {
		got[r.Method+" "+r.Path] = r.Record
	}
	want := map[string]bool{
		"POST /oauth2/token": false, "GET /ping/whoami": true, "GET /accounts": true,
		"GET /balance": true, "GET /pots": true, "GET /transactions": true, "GET /webhooks": true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if rec, ok := got[k]; !ok || rec != v {
			t.Errorf("%s: present=%v record=%v", k, ok, rec)
		}
	}
}

func TestNoRecorderRefusedBeforeSend(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { n.Add(1) }))
	defer srv.Close()
	c, _ := New(srv.URL, testRules)
	if _, err := c.Do(t.Context(), Request{Method: "GET", Path: "/accounts"}); !errors.Is(err, ErrNoRecorder) {
		t.Fatalf("err = %v", err)
	}
	if n.Load() != 0 {
		t.Fatal("request was sent")
	}
}

func TestRecordsEveryAttemptAndBodyExact(t *testing.T) {
	c, rec, sl, n := newTest(t, []int{429, 429, 200}, "  {\"a\":1}\n", testRules)
	q := url.Values{"account_id": {"acc_x"}}
	resp, err := c.Do(t.Context(), Request{Method: "GET", Path: "/accounts", Query: q, Bearer: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "  {\"a\":1}\n" || resp.Status != 200 || resp.ContentType != "application/json" {
		t.Fatalf("resp = %+v", resp)
	}
	if n.Load() != 3 || len(rec.recs) != 3 {
		t.Fatalf("requests %d, recorded %d", n.Load(), len(rec.recs))
	}
	for i, want := range []int{429, 429, 200} {
		r := rec.recs[i]
		if r.Status != want || r.Endpoint != "/accounts" || r.Query["account_id"][0] != "acc_x" || string(r.Body) != "  {\"a\":1}\n" || r.FetchedAt.IsZero() {
			t.Errorf("record %d = %+v", i, r)
		}
	}
	if !slices.Equal(sl.got, secs(2, 4)) {
		t.Errorf("sleeps = %v", sl.got)
	}
}

func TestRecordsErrorStatus(t *testing.T) {
	c, rec, _, _ := newTest(t, []int{404}, `{}`, testRules)
	resp, err := c.Do(t.Context(), Request{Method: "GET", Path: "/accounts"})
	if resp == nil || err == nil || len(rec.recs) != 1 || rec.recs[0].Status != 404 {
		t.Fatalf("resp=%v err=%v recs=%d", resp, err, len(rec.recs))
	}
}

func TestTokenRecordsNothingAndSendsForm(t *testing.T) {
	var gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		_ = r.ParseForm()
		gotBody = r.PostForm.Get("grant_type")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	rec := &fakeRecorder{}
	c, _ := New(srv.URL, testRules, WithRecorder(rec))
	_, err := c.Do(t.Context(), Request{Method: "POST", Path: "/oauth2/token", Form: url.Values{"grant_type": {"refresh_token"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.recs) != 0 || gotCT != "application/x-www-form-urlencoded" || gotBody != "refresh_token" {
		t.Fatalf("recs=%d ct=%q grant=%q", len(rec.recs), gotCT, gotBody)
	}
}

func TestRetry429GivesUpAfterSix(t *testing.T) {
	c, rec, sl, n := newTest(t, []int{429}, `{}`, testRules)
	resp, err := c.Do(t.Context(), Request{Method: "GET", Path: "/accounts"})
	var e *Error
	if !errors.As(err, &e) || e.Category != CategoryRateLimited || resp == nil || resp.Status != 429 {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if n.Load() != 7 || len(rec.recs) != 7 {
		t.Errorf("requests %d, recorded %d, want 7", n.Load(), len(rec.recs))
	}
	if !slices.Equal(sl.got, secs(2, 4, 8, 16, 30, 30)) {
		t.Errorf("sleeps = %v", sl.got)
	}
}

func TestRetry5xxThreeTimes(t *testing.T) {
	c, rec, sl, n := newTest(t, []int{503}, `{}`, testRules)
	_, err := c.Do(t.Context(), Request{Method: "GET", Path: "/accounts"})
	var e *Error
	if !errors.As(err, &e) || e.Category != CategoryServerError {
		t.Fatalf("err = %v", err)
	}
	if n.Load() != 4 || len(rec.recs) != 4 || !slices.Equal(sl.got, secs(2, 4, 8)) {
		t.Errorf("requests %d recorded %d sleeps %v", n.Load(), len(rec.recs), sl.got)
	}
}

func TestRetry5xxThenSuccess(t *testing.T) {
	c, _, sl, _ := newTest(t, []int{500, 502, 200}, `{}`, testRules)
	if _, err := c.Do(t.Context(), Request{Method: "GET", Path: "/accounts"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sl.got, secs(2, 4)) {
		t.Errorf("sleeps = %v", sl.got)
	}
}

// closedURL returns the URL of a port nothing listens on.
func closedURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := "http://" + l.Addr().String()
	_ = l.Close()
	return u
}

func TestNetworkErrorRetriedExceptToken(t *testing.T) {
	sl := &sleeps{}
	c, _ := New(closedURL(t), testRules, WithRecorder(&fakeRecorder{}), WithSleep(sl.sleep))

	_, err := c.Do(t.Context(), Request{Method: "GET", Path: "/accounts"})
	var e *Error
	if !errors.As(err, &e) || e.Category != CategoryNetwork || e.Status != 0 {
		t.Fatalf("err = %v", err)
	}
	if !slices.Equal(sl.got, secs(2, 4, 8)) {
		t.Errorf("sleeps = %v", sl.got)
	}

	sl.got = nil
	_, err = c.Do(t.Context(), Request{Method: "POST", Path: "/oauth2/token", Form: url.Values{}})
	if !errors.As(err, &e) || e.Category != CategoryNetwork {
		t.Fatalf("token err = %v", err)
	}
	if len(sl.got) != 0 {
		t.Errorf("token call retried: %v", sl.got)
	}
}

func TestTokenIsNotRetriedOn5xx(t *testing.T) {
	c, _, sl, n := newTest(t, []int{500}, `{}`, testRules)
	_, err := c.Do(t.Context(), Request{Method: "POST", Path: "/oauth2/token"})
	var e *Error
	if !errors.As(err, &e) || e.Category != CategoryServerError || n.Load() != 1 || len(sl.got) != 0 {
		t.Fatalf("err=%v requests=%d sleeps=%v", err, n.Load(), sl.got)
	}
}

func TestTimeoutCategory(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	sl := &sleeps{}
	c, _ := New(srv.URL, testRules, WithRecorder(&fakeRecorder{}), WithSleep(sl.sleep),
		WithTimeouts(time.Second, 20*time.Millisecond))
	_, err := c.Do(t.Context(), Request{Method: "POST", Path: "/oauth2/token"})
	var e *Error
	if !errors.As(err, &e) || e.Category != CategoryTimeout {
		t.Fatalf("err = %v", err)
	}
}

func TestStatusCategories(t *testing.T) {
	tests := []struct {
		status int
		body   string
		want   Category
		code   string
	}{
		{400, `{"code":"bad_request.invalid_time_range","message":"BODY-MARKER"}`, CategoryBadRequest, "bad_request.invalid_time_range"},
		{401, `{"code":"unauthorized.bad_access_token"}`, CategoryUnauthorised, "unauthorized.bad_access_token"},
		{403, `{"code":"forbidden.insufficient_permissions"}`, CategoryForbiddenInsufficientPermissions, "forbidden.insufficient_permissions"},
		{403, `{"code":"forbidden.verification_required"}`, CategoryForbiddenVerificationRequired, "forbidden.verification_required"},
		{403, `{"code":"forbidden.other"}`, CategoryUnexpectedStatus, "forbidden.other"},
		{403, `not json`, CategoryUnexpectedStatus, ""},
		{404, `{}`, CategoryUnexpectedStatus, ""},
		{302, ``, CategoryUnexpectedStatus, ""},
		{400, `{"code":"has spaces and BODY-MARKER"}`, CategoryBadRequest, ""},
		{429, `{}`, CategoryRateLimited, ""},
		{500, `{}`, CategoryServerError, ""},
	}
	for _, tc := range tests {
		c, _, _, _ := newTest(t, []int{tc.status}, tc.body, testRules)
		resp, err := c.Do(t.Context(), Request{Method: "GET", Path: "/accounts"})
		var e *Error
		if !errors.As(err, &e) {
			t.Fatalf("%d: err = %v", tc.status, err)
		}
		if e.Category != tc.want || e.BankCode != tc.code || e.Status != tc.status || resp == nil || resp.Status != tc.status {
			t.Errorf("%d %q: got %+v", tc.status, tc.body, e)
		}
	}
}

func TestErrorTextHasNoQueryBodyOrToken(t *testing.T) {
	const bearer = "BEARER-MARKER"
	q := url.Values{"account_id": {"ACCOUNT-MARKER"}}
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"code":"bad_request","message":"BODY-MARKER ACCOUNT-MARKER"}`))
	}))
	defer srv.Close()
	sl := &sleeps{}
	httpC, _ := New(srv.URL, testRules, WithRecorder(&fakeRecorder{}), WithSleep(sl.sleep))
	netC, _ := New(closedURL(t), testRules, WithRecorder(&fakeRecorder{}), WithSleep(sl.sleep))

	for name, c := range map[string]*Client{"http": httpC, "network": netC} {
		_, err := c.Do(t.Context(), Request{Method: "GET", Path: "/accounts", Query: q, Bearer: bearer})
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		for _, m := range []string{"ACCOUNT-MARKER", "BODY-MARKER", bearer, "http://", "127.0.0.1", "account_id"} {
			if strings.Contains(err.Error(), m) {
				t.Errorf("%s: %q contains %q", name, err.Error(), m)
			}
		}
		if want := "bankhttp: GET /accounts: "; !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s: %q", name, err.Error())
		}
	}
	if gotAuth != "Bearer "+bearer {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, testRules, WithRecorder(&fakeRecorder{}))
	_, err := c.Do(t.Context(), Request{Method: "GET", Path: "/accounts", Bearer: "x"})
	var e *Error
	if !errors.As(err, &e) || e.Category != CategoryUnexpectedStatus || hits.Load() != 0 {
		t.Fatalf("err=%v hits=%d", err, hits.Load())
	}
}

func TestResponseTiming(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	calls := 0
	clock := func() time.Time { calls++; return now.Add(time.Duration(calls-1) * time.Second) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer srv.Close()
	c, _ := New(srv.URL, testRules, WithRecorder(&fakeRecorder{}), WithClock(clock))
	resp, err := c.Do(t.Context(), Request{Method: "GET", Path: "/accounts"})
	if err != nil || !resp.StartedAt.Equal(now) || resp.Duration != time.Second {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

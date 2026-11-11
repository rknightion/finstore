package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry"
)

const (
	allowedEmail = "owner@example.test"
	authHeader   = "X-Auth-Request-Email"
)

var errRefused = errors.New("refused")

type fakeConn struct {
	authoriseCalls int
	authoriseErr   error
	callbackCalls  int
	callbackErr    error
	gotState       string
	gotCode        string
}

func (f *fakeConn) AuthoriseURL(context.Context) (string, error) {
	f.authoriseCalls++
	return "https://auth.bank.test/?client_id=c&state=s", f.authoriseErr
}

func (f *fakeConn) HandleCallback(_ context.Context, state, code string) error {
	f.callbackCalls++
	f.gotState, f.gotCode = state, code
	return f.callbackErr
}

type fakeReader struct {
	pingErr  error
	sources  []store.SyncStatusRow
	coverage []store.CoverageRow
	readErr  error
}

func (f *fakeReader) Ping(context.Context) error { return f.pingErr }

func (f *fakeReader) SyncStatus(context.Context) ([]store.SyncStatusRow, error) {
	return f.sources, f.readErr
}

func (f *fakeReader) Coverage(context.Context) ([]store.CoverageRow, error) {
	return f.coverage, f.readErr
}

func newServer(t *testing.T, conn *fakeConn, read *fakeReader, logTo io.Writer) http.Handler {
	t.Helper()
	if logTo == nil {
		logTo = io.Discard
	}
	s, err := New(Options{
		AllowedEmails: []string{allowedEmail},
		AuthHeader:    authHeader,
		BankAuthURL:   "https://auth.bank.test/",
		IsRefusal:     func(err error) bool { return errors.Is(err, errRefused) },
		Location:      time.UTC,
		Now:           func() time.Time { return time.Date(2030, 3, 4, 5, 6, 0, 0, time.UTC) },
		Log:           telemetry.NewLogger(logTo, slog.LevelDebug),
	}, conn, read)
	if err != nil {
		t.Fatal(err)
	}
	return s.Handler()
}

func do(h http.Handler, method, target string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func signedIn(extra map[string]string) map[string]string {
	h := map[string]string{authHeader: allowedEmail}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func ptr[T any](v T) *T { return &v }

// The proxy in front turns 401 and 403 into a login redirect, so anything but 404 here would loop
// a signed-in user who is not on the list.
func TestPagesAnswer404ToAnyoneNotOnTheAllowList(t *testing.T) {
	cases := map[string]map[string]string{
		"no header":       {},
		"other address":   {authHeader: "someone@example.test"},
		"empty":           {authHeader: ""},
		"prefix of entry": {authHeader: "owner@example.tes"},
		"entry as suffix": {authHeader: "x" + allowedEmail},
		"list of two":     {authHeader: allowedEmail + ",someone@example.test"},
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			conn := &fakeConn{}
			h := newServer(t, conn, &fakeReader{}, nil)
			if rec := do(h, http.MethodGet, "/", header); rec.Code != http.StatusNotFound {
				t.Errorf("GET / = %d, want 404", rec.Code)
			}
			post := map[string]string{"Sec-Fetch-Site": "same-origin"}
			for k, v := range header {
				post[k] = v
			}
			if rec := do(h, http.MethodPost, "/connect/monzo", post); rec.Code != http.StatusNotFound {
				t.Errorf("POST /connect/monzo = %d, want 404", rec.Code)
			}
			if conn.authoriseCalls != 0 {
				t.Errorf("a connect attempt was started for a caller not on the allow-list")
			}
		})
	}
}

func TestAllowListIgnoresCaseAndSpacing(t *testing.T) {
	h := newServer(t, &fakeConn{}, &fakeReader{}, nil)
	rec := do(h, http.MethodGet, "/", map[string]string{authHeader: " Owner@Example.Test "})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
}

// Two header lines could come from a client that sends its own copy alongside the proxy's.
func TestTwoAuthHeaderLinesAreRefused(t *testing.T) {
	h := newServer(t, &fakeConn{}, &fakeReader{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Add(authHeader, allowedEmail)
	req.Header.Add(authHeader, "someone@example.test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET / = %d, want 404", rec.Code)
	}
}

func TestAnEmptyAllowListIsRefusedAtStart(t *testing.T) {
	if _, err := New(Options{AuthHeader: authHeader}, &fakeConn{}, &fakeReader{}); err == nil {
		t.Fatal("New accepted an empty allow-list")
	}
}

func TestHealthzNeedsNoSignInAndReflectsTheDatabase(t *testing.T) {
	read := &fakeReader{}
	h := newServer(t, &fakeConn{}, read, nil)
	rec := do(h, http.MethodGet, "/healthz", nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Fatalf("healthy: %d %q", rec.Code, rec.Body.String())
	}
	read.pingErr = errors.New("connection refused to db.internal.test")
	rec = do(h, http.MethodGet, "/healthz", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unhealthy: %d, want 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "db.internal.test") {
		t.Fatalf("the health route leaked the database error: %q", rec.Body.String())
	}
}

func TestConnectRedirectsOnlyASameOriginPost(t *testing.T) {
	for _, site := range []string{"", "cross-site", "same-site", "none"} {
		conn := &fakeConn{}
		h := newServer(t, conn, &fakeReader{}, nil)
		rec := do(h, http.MethodPost, "/connect/monzo", signedIn(map[string]string{"Sec-Fetch-Site": site}))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("Sec-Fetch-Site %q: %d, want 400", site, rec.Code)
		}
		if conn.authoriseCalls != 0 {
			t.Errorf("Sec-Fetch-Site %q started a connect attempt", site)
		}
	}
	conn := &fakeConn{}
	h := newServer(t, conn, &fakeReader{}, nil)
	rec := do(h, http.MethodPost, "/connect/monzo", signedIn(map[string]string{"Sec-Fetch-Site": "same-origin"}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("same-origin post: %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); !strings.HasPrefix(got, "https://auth.bank.test/") {
		t.Fatalf("Location = %q", got)
	}
	if conn.authoriseCalls != 1 {
		t.Fatalf("authorise calls = %d, want 1", conn.authoriseCalls)
	}
}

func TestConnectIsNotReachableByGet(t *testing.T) {
	conn := &fakeConn{}
	h := newServer(t, conn, &fakeReader{}, nil)
	rec := do(h, http.MethodGet, "/connect/monzo", signedIn(map[string]string{"Sec-Fetch-Site": "same-origin"}))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /connect/monzo = %d, want 405", rec.Code)
	}
	if conn.authoriseCalls != 0 {
		t.Fatal("a GET started a connect attempt")
	}
}

func TestCallbackNeedsNoSignInAndPassesStateAndCode(t *testing.T) {
	conn := &fakeConn{}
	h := newServer(t, conn, &fakeReader{}, nil)
	rec := do(h, http.MethodGet, "/callback/monzo?code=CODE-MARKER&state=STATE-MARKER", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback: %d to %q, want 303 to /", rec.Code, rec.Header().Get("Location"))
	}
	if conn.gotState != "STATE-MARKER" || conn.gotCode != "CODE-MARKER" {
		t.Fatalf("connector got state %q code %q", conn.gotState, conn.gotCode)
	}
}

func TestCallbackRefusalsAre400AndOtherFailuresAre502(t *testing.T) {
	conn := &fakeConn{callbackErr: errRefused}
	h := newServer(t, conn, &fakeReader{}, nil)
	if rec := do(h, http.MethodGet, "/callback/monzo?code=c&state=s", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("refused state: %d, want 400", rec.Code)
	}
	conn.callbackErr = errors.New("token exchange failed for CODE-MARKER")
	rec := do(h, http.MethodGet, "/callback/monzo?code=c&state=s", nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("other failure: %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "CODE-MARKER") {
		t.Errorf("the callback leaked an error's text: %q", rec.Body.String())
	}
}

func TestCallbackWithoutStateOrCodeNeverReachesTheConnector(t *testing.T) {
	for _, target := range []string{"/callback/monzo", "/callback/monzo?code=c", "/callback/monzo?state=s"} {
		conn := &fakeConn{}
		h := newServer(t, conn, &fakeReader{}, nil)
		if rec := do(h, http.MethodGet, target, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", target, rec.Code)
		}
		if conn.callbackCalls != 0 {
			t.Errorf("%s reached the connector", target)
		}
	}
}

// The callback URL carries the authorisation code and the state value; neither may reach a log.
func TestRequestLogCarriesTheRouteNameAndNeverTheURL(t *testing.T) {
	var buf bytes.Buffer
	conn := &fakeConn{callbackErr: errors.New("failure mentioning CODE-MARKER")}
	h := newServer(t, conn, &fakeReader{}, &buf)
	do(h, http.MethodGet, "/callback/monzo?code=CODE-MARKER&state=STATE-MARKER", nil)
	out := buf.String()
	if !strings.Contains(out, `"endpoint":"/callback/monzo"`) {
		t.Fatalf("no request line with the route name in: %s", out)
	}
	for _, marker := range []string{"CODE-MARKER", "STATE-MARKER"} {
		if strings.Contains(out, marker) {
			t.Fatalf("log contains %s: %s", marker, out)
		}
	}
}

func TestStatusPageShowsStatesDatesAndCountsButNoAmount(t *testing.T) {
	approved := time.Date(2030, 1, 2, 10, 0, 0, 0, time.UTC)
	imported := time.Date(2030, 2, 1, 9, 30, 0, 0, time.UTC)
	read := &fakeReader{
		sources: []store.SyncStatusRow{
			{
				Source: "monzo", ConsentState: ptr("awaiting_approval"), ApprovedAt: &approved,
				DaysToExpiry: ptr(41), LastRunOutcome: ptr("ok"),
			},
			{Source: "import", LastImportAt: map[string]*time.Time{"bank-joint": &imported}},
		},
		coverage: []store.CoverageRow{
			{
				AccountSlug: "bank-joint", Feed: "file", TransactionCount: 1234,
				EarliestDate:        ptr(time.Date(2029, 5, 1, 0, 0, 0, 0, time.UTC)),
				LatestDate:          ptr(time.Date(2030, 1, 31, 0, 0, 0, 0, time.UTC)),
				ReconciliationState: store.StateMismatch,
				LastDiffMinor:       ptr(int64(987654321)),
				Gaps: []store.Gap{{
					From: ptr(time.Date(2029, 8, 31, 0, 0, 0, 0, time.UTC)),
					To:   time.Date(2029, 10, 1, 0, 0, 0, 0, time.UTC), Reason: store.GapNoFile,
					DiffMinor: ptr(int64(555444333)),
				}},
			},
		},
	}
	h := newServer(t, &fakeConn{}, read, nil)
	rec := do(h, http.MethodGet, "/", signedIn(nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"awaiting approval", "2030-01-02 10:00", ">41<", "bank-joint", "1234", "2029-05-01", "2030-01-31",
		"mismatch", "2029-08-31 to 2029-10-01", store.GapNoFile, "2030-02-01 09:30", "Reconnect Monzo",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, amount := range []string{"987654321", "555444333"} {
		if strings.Contains(body, amount) {
			t.Errorf("page shows the amount %s", amount)
		}
	}
}

func TestStatusPageOffersConnectBeforeAnyConsentExists(t *testing.T) {
	h := newServer(t, &fakeConn{}, &fakeReader{}, nil)
	body := do(h, http.MethodGet, "/", signedIn(nil)).Body.String()
	if !strings.Contains(body, "Connect Monzo") || strings.Contains(body, "Reconnect Monzo") {
		t.Fatalf("page does not offer a first connection: %s", body)
	}
	if !strings.Contains(body, "not connected") {
		t.Fatalf("page does not say the source is not connected")
	}
}

func TestStatusPageAnswers503WhenTheDatabaseCannotBeRead(t *testing.T) {
	h := newServer(t, &fakeConn{}, &fakeReader{readErr: errors.New("relation missing at db.internal.test")}, nil)
	rec := do(h, http.MethodGet, "/", signedIn(nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET / = %d, want 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "db.internal.test") {
		t.Fatalf("page leaked the database error")
	}
}

// The style sheet is allowed by its hash, so a page whose inline style differs from the hashed
// text renders unstyled, and a policy without the bank's origin blocks the connect redirect.
func TestContentSecurityPolicyMatchesThePage(t *testing.T) {
	h := newServer(t, &fakeConn{}, &fakeReader{}, nil)
	rec := do(h, http.MethodGet, "/", signedIn(nil))
	csp := rec.Header().Get("Content-Security-Policy")
	body := rec.Body.String()
	start, end := strings.Index(body, "<style>"), strings.Index(body, "</style>")
	if start < 0 || end < 0 {
		t.Fatal("no style element")
	}
	sum := sha256.Sum256([]byte(body[start+len("<style>") : end]))
	want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	if !strings.Contains(csp, want) {
		t.Fatalf("policy %q does not allow the page's style %s", csp, want)
	}
	for _, part := range []string{"default-src 'none'", "form-action 'self' https://auth.bank.test", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, part) {
			t.Errorf("policy %q lacks %q", csp, part)
		}
	}
	for header, want := range map[string]string{
		"Cache-Control": "no-store", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestUnknownPathsAre404(t *testing.T) {
	h := newServer(t, &fakeConn{}, &fakeReader{}, nil)
	for _, target := range []string{"/status", "/callback/other", "/connect/monzo/x", "/.env"} {
		if rec := do(h, http.MethodGet, target, signedIn(nil)); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, rec.Code)
		}
	}
}

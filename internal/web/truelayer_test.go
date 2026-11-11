package web

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/store"
)

func TestSourcesAreValidatedAtStart(t *testing.T) {
	good := monzoSource(&fakeConn{})
	with := func(mod func(s *Source)) Source {
		s := good
		mod(&s)
		return s
	}
	cases := map[string][]Source{
		"none":             nil,
		"no connector":     {with(func(s *Source) { s.Connector = nil })},
		"no name":          {with(func(s *Source) { s.Name = "" })},
		"relative auth":    {with(func(s *Source) { s.AuthURL = "/login" })},
		"no connect route": {with(func(s *Source) { s.ConnectPath = "" })},
		"no callback":      {with(func(s *Source) { s.CallbackPath = "" })},
		"duplicate name":   {good, with(func(s *Source) { s.ConnectPath, s.CallbackPath = "/connect/x", "/callback/x" })},
		"duplicate route":  {good, with(func(s *Source) { s.Name, s.CallbackPath = "other", "/callback/x" })},
	}
	for name, sources := range cases {
		if _, err := New(options(io.Discard, sources...), &fakeReader{}); err == nil {
			t.Errorf("%s: New accepted the sources", name)
		}
	}
}

func TestTrueLayerConnectRoutesRedirectToTheirBank(t *testing.T) {
	for path, want := range map[string]string{
		"/connect/truelayer/ob-lloyds": "provider_id=ob-lloyds",
		"/connect/truelayer/ob-monzo":  "provider_id=ob-monzo",
	} {
		ts := newThree(t, nil)
		rec := do(ts.h, http.MethodPost, path, signedIn(map[string]string{"Sec-Fetch-Site": "same-origin"}))
		if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), want) {
			t.Errorf("%s: %d to %q, want 303 to the %s link", path, rec.Code, rec.Header().Get("Location"), want)
		}
		if n := ts.lloyds.authoriseCalls + ts.tlMonzo.authoriseCalls + ts.monzo.authoriseCalls; n != 1 {
			t.Errorf("%s: %d attempts started, want 1", path, n)
		}
	}
}

func TestTrueLayerConnectRoutesApplyTheSameChecksAsMonzo(t *testing.T) {
	for _, path := range []string{"/connect/truelayer/ob-lloyds", "/connect/truelayer/ob-monzo"} {
		ts := newThree(t, nil)
		for _, site := range []string{"", "cross-site", "same-site", "none"} {
			if rec := do(ts.h, http.MethodPost, path, signedIn(map[string]string{"Sec-Fetch-Site": site})); rec.Code != http.StatusBadRequest {
				t.Errorf("%s with Sec-Fetch-Site %q: %d, want 400", path, site, rec.Code)
			}
		}
		other := map[string]string{authHeader: "someone@example.com", "Sec-Fetch-Site": "same-origin"}
		if rec := do(ts.h, http.MethodPost, path, other); rec.Code != http.StatusNotFound {
			t.Errorf("%s by an address not on the list: %d, want 404", path, rec.Code)
		}
		if rec := do(ts.h, http.MethodGet, path, signedIn(map[string]string{"Sec-Fetch-Site": "same-origin"})); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: %d, want 405", path, rec.Code)
		}
		if ts.lloyds.authoriseCalls+ts.tlMonzo.authoriseCalls != 0 {
			t.Errorf("%s: a refused request started a connect attempt", path)
		}
	}
}

func TestPolicyAllowsEveryBankOriginOnce(t *testing.T) {
	ts := newThree(t, nil)
	csp := do(ts.h, http.MethodGet, "/", signedIn(nil)).Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "form-action 'self' https://auth.bank.test https://auth.truelayer.test;") {
		t.Fatalf("policy %q does not allow both bank origins once each", csp)
	}
}

func TestTrueLayerCallbackHandsTheStateToTheAttemptsConnection(t *testing.T) {
	ts := newThree(t, nil)
	state := ts.attempt("STATE-LLOYDS", srcLloyds)
	rec := do(ts.h, http.MethodGet, "/callback/truelayer?code=CODE-1&state="+state, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback: %d to %q, want 303 to /", rec.Code, rec.Header().Get("Location"))
	}
	if ts.lloyds.callbackCalls != 1 || ts.lloyds.gotState != state || ts.lloyds.gotCode != "CODE-1" {
		t.Fatalf("Lloyds connector: %d calls, state %q code %q", ts.lloyds.callbackCalls, ts.lloyds.gotState, ts.lloyds.gotCode)
	}
	if ts.tlMonzo.callbackCalls+ts.monzo.callbackCalls != 0 {
		t.Fatal("another connector was called")
	}
	state = ts.attempt("STATE-MONZO", srcTLMonzo)
	if rec := do(ts.h, http.MethodGet, "/callback/truelayer?code=CODE-2&state="+state, nil); rec.Code != http.StatusSeeOther || ts.tlMonzo.callbackCalls != 1 {
		t.Fatalf("Monzo (TrueLayer) callback: %d, %d calls", rec.Code, ts.tlMonzo.callbackCalls)
	}
}

// An unknown, used or expired state reads as unusable, and a Monzo state is not TrueLayer's. Each
// is refused before any connector sees it, so nothing is consumed or exchanged.
func TestTrueLayerCallbackRefusesWithoutEffect(t *testing.T) {
	ts := newThree(t, nil)
	monzoState := ts.attempt("STATE-OF-MONZO", "monzo")
	for name, target := range map[string]string{
		"unusable state":    "/callback/truelayer?code=c&state=never-issued",
		"other source":      "/callback/truelayer?code=c&state=" + monzoState,
		"no state":          "/callback/truelayer?code=c",
		"no code":           "/callback/truelayer?state=" + monzoState,
		"bank error":        "/callback/truelayer?error=access_denied&state=" + monzoState,
		"bank error + code": "/callback/truelayer?error=access_denied&code=c&state=" + monzoState,
	} {
		if rec := do(ts.h, http.MethodGet, target, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, rec.Code)
		}
	}
	if n := ts.callbackCalls(); n != 0 {
		t.Fatalf("%d connector calls for refused callbacks", n)
	}
}

func TestTrueLayerCallbackBankErrorSaysSoAndReadsNothing(t *testing.T) {
	ts := newThree(t, nil)
	rec := do(ts.h, http.MethodGet, "/callback/truelayer?error=access_denied&state=s", nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "did not give access") {
		t.Fatalf("bank error: %d %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "access_denied") {
		t.Fatal("the page echoes the bank's error code")
	}
	if ts.read.attemptCalls != 0 {
		t.Fatal("a bank error read the attempt")
	}
}

func TestTrueLayerCallbackFailures(t *testing.T) {
	ts := newThree(t, nil)
	state := ts.attempt("STATE-LLOYDS", srcLloyds)
	ts.lloyds.callbackErr = errRefused
	if rec := do(ts.h, http.MethodGet, "/callback/truelayer?code=c&state="+state, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("connector refusal: %d, want 400", rec.Code)
	}
	ts.lloyds.callbackErr = errors.New("exchange failed for CODE-MARKER")
	rec := do(ts.h, http.MethodGet, "/callback/truelayer?code=c&state="+state, nil)
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "CODE-MARKER") {
		t.Errorf("exchange failure: %d %q, want 502 without the error text", rec.Code, rec.Body.String())
	}
	ts.read.attemptErr = errors.New("db.internal.test unreachable")
	calls := ts.callbackCalls()
	rec = do(ts.h, http.MethodGet, "/callback/truelayer?code=c&state="+state, nil)
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "db.internal.test") || ts.callbackCalls() != calls {
		t.Errorf("unreadable attempt: %d %q, %d new connector calls", rec.Code, rec.Body.String(), ts.callbackCalls()-calls)
	}
}

func TestTrueLayerCallbackLogNeverCarriesTheURL(t *testing.T) {
	var buf bytes.Buffer
	ts := newThree(t, &buf)
	state := ts.attempt("STATE-MARKER", srcLloyds)
	ts.lloyds.callbackErr = errors.New("failure mentioning CODE-MARKER")
	do(ts.h, http.MethodGet, "/callback/truelayer?code=CODE-MARKER&state="+state, nil)
	out := buf.String()
	if !strings.Contains(out, `"endpoint":"/callback/truelayer"`) || !strings.Contains(out, srcLloyds) {
		t.Fatalf("no lines with the route and the source in: %s", out)
	}
	for _, marker := range []string{"CODE-MARKER", "STATE-MARKER"} {
		if strings.Contains(out, marker) {
			t.Fatalf("log contains %s: %s", marker, out)
		}
	}
}

func TestStatusPageHasASectionPerConfiguredSource(t *testing.T) {
	ts := newThree(t, nil)
	approved := time.Date(2030, 1, 2, 10, 0, 0, 0, time.UTC)
	synced := time.Date(2030, 3, 1, 6, 21, 0, 0, time.UTC)
	ts.read.sources = []store.SyncStatusRow{
		{Source: "monzo", ConsentState: ptr("active")},
		{
			Source: srcLloyds, ConsentState: ptr("active"), ApprovedAt: &approved, DaysToExpiry: ptr(77),
			LastSyncOKAt: &synced, LastRunOutcome: ptr("partial"),
		},
		{Source: "unconfigured-source", ConsentState: ptr("lost")},
	}
	body := do(ts.h, http.MethodGet, "/", signedIn(nil)).Body.String()
	for _, want := range []string{
		"Monzo connection", "Reconnect Monzo<", `action="/connect/monzo"`,
		"Lloyds (TrueLayer) connection", "Reconnect Lloyds (TrueLayer)", `action="/connect/truelayer/ob-lloyds"`,
		"2030-01-02 10:00", ">77<", "2030-03-01 06:21", ">partial<",
		"Monzo (TrueLayer) connection", "Connect Monzo (TrueLayer)", `action="/connect/truelayer/ob-monzo"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "Reconnect Monzo (TrueLayer)") || strings.Contains(body, "unconfigured-source") || strings.Contains(body, ">lost<") {
		t.Errorf("page shows a section it should not:\n%s", body)
	}
	a := strings.Index(body, "Monzo connection")
	b := strings.Index(body, "Lloyds (TrueLayer) connection")
	c := strings.Index(body, "Monzo (TrueLayer) connection")
	if a >= b || b >= c {
		t.Errorf("sections out of the configured order: %d %d %d", a, b, c)
	}
}

func TestMonzoOnlyHasNoTrueLayerRoutesOrSection(t *testing.T) {
	h := newServer(t, &fakeConn{}, &fakeReader{}, nil)
	body := do(h, http.MethodGet, "/", signedIn(nil)).Body.String()
	if strings.Contains(body, "TrueLayer") {
		t.Fatalf("a Monzo-only page mentions TrueLayer:\n%s", body)
	}
	for _, r := range []struct{ method, target string }{
		{http.MethodPost, "/connect/truelayer/ob-lloyds"},
		{http.MethodGet, "/callback/truelayer?code=c&state=s"},
	} {
		if rec := do(h, r.method, r.target, signedIn(map[string]string{"Sec-Fetch-Site": "same-origin"})); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: %d, want 404", r.method, r.target, rec.Code)
		}
	}
}

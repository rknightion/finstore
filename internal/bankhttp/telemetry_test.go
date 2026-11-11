package bankhttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/rknightion/finstore/internal/semconv"
)

// obsRecorder is an Observer that keeps what it is told.
type obsRecorder struct {
	mu       sync.Mutex
	attempts []observedAttempt
	retries  []observedRetry
}

type observedAttempt struct{ endpoint, class string }
type observedRetry struct{ endpoint, reason string }

func (o *obsRecorder) Attempt(_ context.Context, endpoint, class string, d time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if d < 0 {
		panic("negative duration")
	}
	o.attempts = append(o.attempts, observedAttempt{endpoint, class})
}

func (o *obsRecorder) Retry(_ context.Context, endpoint, reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.retries = append(o.retries, observedRetry{endpoint, reason})
}

// tracedBank is a fake bank that answers with the statuses in order (the last repeats) and keeps
// the headers of every request, plus a client wired to a tracer and an observer.
type tracedBank struct {
	srv     *httptest.Server
	c       *Client
	spans   *tracetest.InMemoryExporter
	obs     *obsRecorder
	mu      sync.Mutex
	headers []http.Header
	paths   []string
	queries []string
}

func newTracedBank(t *testing.T, statuses []int, body string, rules []Rule, basePath string, opts ...Option) *tracedBank {
	t.Helper()
	b := &tracedBank{spans: tracetest.NewInMemoryExporter(), obs: &obsRecorder{}}
	var n atomic.Int32
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.headers = append(b.headers, r.Header.Clone())
		b.paths = append(b.paths, r.URL.Path)
		b.queries = append(b.queries, r.URL.RawQuery)
		b.mu.Unlock()
		i := int(n.Add(1)) - 1
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statuses[min(i, len(statuses)-1)])
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(b.srv.Close)
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(b.spans))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	opts = append([]Option{
		WithSleep(func(context.Context, time.Duration) error { return nil }),
		WithTelemetry(tp.Tracer("test"), "testbank", b.obs),
	}, opts...)
	c, err := New(b.srv.URL+basePath, rules, opts...)
	if err != nil {
		t.Fatal(err)
	}
	b.c = c
	return b
}

func attrMap(s tracetest.SpanStub) map[string]attribute.Value {
	m := map[string]attribute.Value{}
	for _, kv := range s.Attributes {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

func spanNamed(t *testing.T, spans tracetest.SpanStubs, name string, kind trace.SpanKind) []tracetest.SpanStub {
	t.Helper()
	var out []tracetest.SpanStub
	for _, s := range spans {
		if s.Name == name && s.SpanKind == kind {
			out = append(out, s)
		}
	}
	return out
}

var spanRules = []Rule{
	{Method: "POST", Path: "/oauth2/token"},
	{Method: "GET", Path: "/accounts"},
	{Method: "GET", Path: "/data/v1/accounts/{id}/transactions"},
}

// AC1: a 429 then a 200 is one logical span and two attempt spans, the second marked as a resend,
// and one retry for the reason rate_limited.
func TestRateLimitedThenOKIsOneLogicalSpanAndTwoAttempts(t *testing.T) {
	b := newTracedBank(t, []int{429, 200}, `{}`, spanRules, "")
	if _, err := b.c.Do(t.Context(), Request{Method: "GET", Path: "/accounts"}); err != nil {
		t.Fatal(err)
	}
	spans := b.spans.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("%d spans, want 3: %v", len(spans), spans)
	}
	logical := spanNamed(t, spans, "bank GET /accounts", trace.SpanKindInternal)
	attempts := spanNamed(t, spans, "GET /accounts", trace.SpanKindClient)
	if len(logical) != 1 || len(attempts) != 2 {
		t.Fatalf("logical %d, attempts %d", len(logical), len(attempts))
	}
	la := attrMap(logical[0])
	if la[semconv.SpanSource].AsString() != "testbank" || la[semconv.SpanURLTemplate].AsString() != "/accounts" ||
		la[semconv.SpanHTTPRequestMethod].AsString() != "GET" {
		t.Errorf("logical attributes = %v", la)
	}
	if logical[0].Status.Code == codes.Error {
		t.Errorf("a call that ended 200 is an error: %v", logical[0].Status)
	}
	first, second := attempts[0], attempts[1]
	if first.StartTime.After(second.StartTime) {
		first, second = second, first
	}
	for i, a := range []tracetest.SpanStub{first, second} {
		if a.Parent.SpanID() != logical[0].SpanContext.SpanID() {
			t.Errorf("attempt %d is not a child of the logical span", i)
		}
	}
	fa, sa := attrMap(first), attrMap(second)
	if _, ok := fa[semconv.SpanHTTPRequestResendCount]; ok {
		t.Errorf("the first attempt has a resend count: %v", fa)
	}
	if got := sa[semconv.SpanHTTPRequestResendCount]; got.Type() != attribute.INT64 || got.AsInt64() != 1 {
		t.Errorf("the second attempt resend count = %v, want 1", got)
	}
	if fa[semconv.SpanHTTPResponseStatusCode].AsInt64() != 429 || sa[semconv.SpanHTTPResponseStatusCode].AsInt64() != 200 {
		t.Errorf("status codes = %v, %v", fa[semconv.SpanHTTPResponseStatusCode], sa[semconv.SpanHTTPResponseStatusCode])
	}
	if first.Status.Code != codes.Error || first.Status.Description != "rate_limited" || fa[semconv.SpanErrorType].AsString() != "rate_limited" {
		t.Errorf("first attempt status = %v, error.type = %v", first.Status, fa[semconv.SpanErrorType])
	}
	if second.Status.Code == codes.Error {
		t.Errorf("second attempt status = %v", second.Status)
	}
	if got := b.obs.retries; len(got) != 1 || got[0] != (observedRetry{"/accounts", "rate_limited"}) {
		t.Errorf("retries = %v, want one rate_limited", got)
	}
	if got := b.obs.attempts; len(got) != 2 || got[0] != (observedAttempt{"/accounts", "4xx"}) || got[1] != (observedAttempt{"/accounts", "2xx"}) {
		t.Errorf("attempts = %v", got)
	}
}

func TestServerErrorsAreRetriedWithTheirOwnReason(t *testing.T) {
	b := newTracedBank(t, []int{502, 503, 200}, `{}`, spanRules, "")
	if _, err := b.c.Do(t.Context(), Request{Method: "GET", Path: "/accounts"}); err != nil {
		t.Fatal(err)
	}
	want := []observedRetry{{"/accounts", "server_error"}, {"/accounts", "server_error"}}
	if got := b.obs.retries; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("retries = %v, want %v", got, want)
	}
	attempts := spanNamed(t, b.spans.GetSpans(), "GET /accounts", trace.SpanKindClient)
	counts := map[int64]bool{}
	for _, a := range attempts {
		if v, ok := attrMap(a)[semconv.SpanHTTPRequestResendCount]; ok {
			counts[v.AsInt64()] = true
		}
	}
	if len(attempts) != 3 || !counts[1] || !counts[2] || len(counts) != 2 {
		t.Errorf("%d attempts, resend counts %v", len(attempts), counts)
	}
}

// AC2: an attempt that got no HTTP response is reported as no_response, and each repeat of a
// transport failure as a retry for its reason.
func TestTransportFailureIsNoResponse(t *testing.T) {
	b := newTracedBank(t, []int{200}, `{}`, spanRules, "", WithoutRetries())
	b.srv.Close() // connection refused
	_, err := b.c.Do(t.Context(), Request{Method: "GET", Path: "/accounts"})
	var e *Error
	if !errors.As(err, &e) || e.Category != CategoryNetwork {
		t.Fatalf("err = %v", err)
	}
	if got := b.obs.attempts; len(got) != 1 || got[0] != (observedAttempt{"/accounts", "no_response"}) {
		t.Errorf("attempts = %v, want one no_response", got)
	}
	if len(b.obs.retries) != 0 {
		t.Errorf("retries = %v with retries off", b.obs.retries)
	}
	spans := b.spans.GetSpans()
	logical := spanNamed(t, spans, "bank GET /accounts", trace.SpanKindInternal)
	attempts := spanNamed(t, spans, "GET /accounts", trace.SpanKindClient)
	if len(logical) != 1 || len(attempts) != 1 {
		t.Fatalf("logical %d, attempts %d", len(logical), len(attempts))
	}
	aa := attrMap(attempts[0])
	if _, ok := aa[semconv.SpanHTTPResponseStatusCode]; ok {
		t.Errorf("an attempt with no response has a status code: %v", aa)
	}
	if aa[semconv.SpanErrorType].AsString() != "network" || attempts[0].Status.Code != codes.Error || attempts[0].Status.Description != "network" {
		t.Errorf("attempt error.type %v, status %v", aa[semconv.SpanErrorType], attempts[0].Status)
	}
	if la := attrMap(logical[0]); la[semconv.SpanErrorType].AsString() != "network" || logical[0].Status.Description != "network" {
		t.Errorf("logical error.type %v, status %v", la[semconv.SpanErrorType], logical[0].Status)
	}
}

func TestTransportFailuresAreRetriedAsNetworkAndEachAttemptIsNoResponse(t *testing.T) {
	b := newTracedBank(t, []int{200}, `{}`, spanRules, "")
	b.srv.Close()
	if _, err := b.c.Do(t.Context(), Request{Method: "GET", Path: "/accounts"}); err == nil {
		t.Fatal("no error")
	}
	if len(b.obs.attempts) != 4 || len(b.obs.retries) != 3 {
		t.Fatalf("attempts %d retries %d, want 4 and 3", len(b.obs.attempts), len(b.obs.retries))
	}
	for _, a := range b.obs.attempts {
		if a.class != "no_response" {
			t.Errorf("attempt class = %q", a.class)
		}
	}
	for _, r := range b.obs.retries {
		if r.reason != "network" {
			t.Errorf("retry reason = %q", r.reason)
		}
	}
}

func TestTimeoutIsNoResponseWithTheTimeoutReason(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer slow.Close()
	defer close(release)
	obs := &obsRecorder{}
	c, err := New(slow.URL, spanRules, WithTelemetry(nil, "testbank", obs), WithTimeouts(time.Second, 20*time.Millisecond),
		WithSleep(func(context.Context, time.Duration) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(t.Context(), Request{Method: "GET", Path: "/accounts"}); err == nil {
		t.Fatal("no error")
	}
	if len(obs.attempts) != 4 || len(obs.retries) != 3 || obs.retries[0].reason != "timeout" || obs.attempts[0].class != "no_response" {
		t.Errorf("attempts %v retries %v", obs.attempts, obs.retries)
	}
}

// AC3: the attempt's url.full is the scheme, host, base path and the rule's template, with no
// query and no concrete path, and no span carries the query or the id a bank response supplied.
func TestURLFullHasNoQueryAndUsesTheTemplate(t *testing.T) {
	const canaryID, canaryQuery = "acc-canaryid-1", "CANARYQUERYVALUE"
	b := newTracedBank(t, []int{200}, `{}`, spanRules, "/base/")
	_, err := b.c.Do(t.Context(), Request{
		Method: "GET", Path: "/data/v1/accounts/" + canaryID + "/transactions",
		Query: url.Values{"from": {canaryQuery}, "account_id": {canaryID}}, Bearer: "bearer-canary",
	})
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	if len(b.paths) != 1 || b.paths[0] != "/base/data/v1/accounts/"+canaryID+"/transactions" || !strings.Contains(b.queries[0], canaryQuery) {
		t.Fatalf("the bank was sent path %v query %v", b.paths, b.queries)
	}
	b.mu.Unlock()

	spans := b.spans.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("%d spans", len(spans))
	}
	attempts := spanNamed(t, spans, "GET /data/v1/accounts/{id}/transactions", trace.SpanKindClient)
	if len(attempts) != 1 {
		t.Fatalf("spans: %v", spans)
	}
	aa := attrMap(attempts[0])
	wantFull := b.srv.URL + "/base/data/v1/accounts/{id}/transactions"
	if got := aa[semconv.SpanURLFull].AsString(); got != wantFull {
		t.Errorf("url.full = %q, want %q", got, wantFull)
	}
	if got := aa[semconv.SpanURLTemplate].AsString(); got != "/data/v1/accounts/{id}/transactions" {
		t.Errorf("url.template = %q", got)
	}
	host := strings.TrimPrefix(b.srv.URL, "http://")
	if got := aa[semconv.SpanServerAddress].AsString(); got != strings.Split(host, ":")[0] {
		t.Errorf("server.address = %q", got)
	}
	if got := aa[semconv.SpanServerPort]; got.Type() != attribute.INT64 || got.AsInt64() == 0 {
		t.Errorf("server.port = %v", got)
	}
	for _, s := range spans {
		for _, text := range append([]string{s.Name, s.Status.Description}, attrStrings(s)...) {
			if strings.Contains(text, canaryID) || strings.Contains(text, canaryQuery) || strings.Contains(text, "bearer-canary") || strings.Contains(text, "?") {
				t.Errorf("span %q leaks %q", s.Name, text)
			}
		}
		if len(s.Events) != 0 {
			t.Errorf("span %q has events", s.Name)
		}
	}
}

func attrStrings(s tracetest.SpanStub) []string {
	var out []string
	for _, kv := range s.Attributes {
		out = append(out, string(kv.Key), kv.Value.String())
	}
	return out
}

// Test 9 of the design: with a W3C trace context and baggage propagator installed, and a span and
// baggage on the context, the bank still sees no propagation header. The client is not otelhttp.
func TestBankSeesNoTracePropagationHeader(t *testing.T) {
	prev := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })

	b := newTracedBank(t, []int{429, 200}, `{}`, spanRules, "")
	member, err := baggage.NewMember("canary", "baggage-canary")
	if err != nil {
		t.Fatal(err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatal(err)
	}
	ctx := baggage.ContextWithBaggage(t.Context(), bag)
	tp := sdktrace.NewTracerProvider()
	ctx, parent := tp.Tracer("test").Start(ctx, "parent")
	defer parent.End()
	// Prove the propagator would inject on this context: otherwise the test proves nothing.
	probe := http.Header{}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(probe))
	if probe.Get("traceparent") == "" || probe.Get("baggage") == "" {
		t.Fatalf("the probe injected %v", probe)
	}

	if _, err := b.c.Do(ctx, Request{Method: "GET", Path: "/accounts", Bearer: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Do(ctx, Request{Method: "POST", Path: "/oauth2/token", Form: url.Values{"grant_type": {"refresh_token"}}}); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.headers) != 3 {
		t.Fatalf("the bank saw %d requests", len(b.headers))
	}
	for i, h := range b.headers {
		for _, name := range []string{"Traceparent", "Tracestate", "Baggage", "Uber-Trace-Id", "B3", "X-B3-Traceid"} {
			if v := h.Values(name); len(v) != 0 {
				t.Errorf("request %d carries %s: %v", i, name, v)
			}
		}
	}
}

// A final failure puts the category, and a well-formed bank code, on the logical span; the status
// description is the category and nothing from the response.
func TestFailedCallCarriesTheCategoryAndBankCode(t *testing.T) {
	const body = `{"code":"forbidden.insufficient_permissions","message":"CANARYMESSAGE"}`
	b := newTracedBank(t, []int{403}, body, spanRules, "")
	_, err := b.c.Do(t.Context(), Request{Method: "GET", Path: "/accounts"})
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v", err)
	}
	logical := spanNamed(t, b.spans.GetSpans(), "bank GET /accounts", trace.SpanKindInternal)
	if len(logical) != 1 {
		t.Fatalf("%d logical spans", len(logical))
	}
	la := attrMap(logical[0])
	if la[semconv.SpanErrorType].AsString() != "forbidden_insufficient_permissions" ||
		la[semconv.SpanBankCode].AsString() != "forbidden.insufficient_permissions" ||
		logical[0].Status.Code != codes.Error || logical[0].Status.Description != "forbidden_insufficient_permissions" {
		t.Errorf("logical span: attrs %v status %v", la, logical[0].Status)
	}
	for _, s := range b.spans.GetSpans() {
		for _, text := range append([]string{s.Name, s.Status.Description}, attrStrings(s)...) {
			if strings.Contains(text, "CANARYMESSAGE") {
				t.Errorf("span %q leaks the response text: %q", s.Name, text)
			}
		}
	}
}

func TestACallTheAllowListRefusesStartsNoSpan(t *testing.T) {
	b := newTracedBank(t, []int{200}, `{}`, spanRules, "")
	if _, err := b.c.Do(t.Context(), Request{Method: "DELETE", Path: "/accounts"}); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("err = %v", err)
	}
	if n := len(b.spans.GetSpans()); n != 0 || len(b.obs.attempts) != 0 {
		t.Errorf("%d spans, %d attempts for a refused call", n, len(b.obs.attempts))
	}
}

// A client without WithTelemetry works as before: no panic on the nil observer.
func TestNoTelemetryOptionIsHarmless(t *testing.T) {
	c, _, _, _ := newTest(t, []int{429, 200}, `{}`, testRules)
	if _, err := c.Do(t.Context(), Request{Method: "GET", Path: "/accounts"}); err != nil {
		t.Fatal(err)
	}
}

// A status outside 100 to 599 is not a usable response: the attempt is reported with the same
// class as an attempt that got none.
func TestStatusOutsideTheHTTPRangeIsNoResponse(t *testing.T) {
	for _, status := range []int{0, 99, 600, 999, -1} {
		if got := statusClass(status); got != semconv.StatusClassNoResponse {
			t.Errorf("statusClass(%d) = %q, want %q", status, got, semconv.StatusClassNoResponse)
		}
	}
	for status, want := range map[int]string{100: "1xx", 204: "2xx", 302: "3xx", 429: "4xx", 599: "5xx"} {
		if got := statusClass(status); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", status, got, want)
		}
	}
	b := newTracedBank(t, []int{600}, `{}`, spanRules, "", WithoutRetries())
	if _, err := b.c.Do(t.Context(), Request{Method: "GET", Path: "/accounts"}); err == nil {
		t.Fatal("a 600 was a success")
	}
	if got := b.obs.attempts; len(got) != 1 || got[0] != (observedAttempt{"/accounts", semconv.StatusClassNoResponse}) {
		t.Errorf("attempts = %v, want one no_response", got)
	}
}

// A response whose body cannot be read is reported as no_response although a status arrived: the
// call fails with a transport category, as it always has.
func TestUnreadableBodyIsNoResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{"))
		panic(http.ErrAbortHandler) // closes the connection mid-body
	}))
	defer srv.Close()
	obs := &obsRecorder{}
	c, err := New(srv.URL, spanRules, WithoutRetries(), WithTelemetry(nil, "testbank", obs))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Do(t.Context(), Request{Method: "GET", Path: "/accounts"})
	var e *Error
	if !errors.As(err, &e) || e.Category != CategoryNetwork || e.Status != 0 {
		t.Fatalf("err = %v", err)
	}
	if got := obs.attempts; len(got) != 1 || got[0].class != semconv.StatusClassNoResponse {
		t.Errorf("attempts = %v, want one no_response", got)
	}
}

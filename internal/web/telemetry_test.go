package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/telemetry"
)

// traced is a web server behind the real telemetry.HTTPHandler whose spans and metrics go to
// memory. The spans are the raw ones, before the exporter allow-list, so a value that is absent
// here is absent for any exporter.
type traced struct {
	h      http.Handler
	mem    *tracetest.InMemoryExporter
	reader *sdkmetric.ManualReader
	logs   *bytes.Buffer
	three  *threeSources
}

// newTraced builds the traced server. Each extraAllowed address is added to the allow-list.
func newTraced(t *testing.T, extraAllowed ...string) *traced {
	t.Helper()
	mem := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(mem))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()); _ = mp.Shutdown(context.Background()) })

	logs := &bytes.Buffer{}
	three := &threeSources{
		monzo:   &fakeConn{},
		lloyds:  &fakeConn{url: "https://auth.truelayer.test/?provider_id=ob-lloyds&state=s"},
		tlMonzo: &fakeConn{url: "https://auth.truelayer.test/?provider_id=ob-monzo&state=s"},
		read:    &fakeReader{attempts: map[string]string{}},
	}
	tl := func(name, label string, conn *fakeConn) Source {
		return Source{
			Name: name, Label: label, ConnectPath: "/connect/" + strings.ReplaceAll(name, ":", "/"),
			CallbackPath: "/callback/truelayer", AuthURL: "https://auth.truelayer.test", Connector: conn,
			IsRefusal: func(err error) bool { return errors.Is(err, errRefused) },
		}
	}
	opts := options(logs, monzoSource(three.monzo),
		tl(srcLloyds, "Lloyds (TrueLayer)", three.lloyds), tl(srcTLMonzo, "Monzo (TrueLayer)", three.tlMonzo))
	opts.Tracer = tp.Tracer("test")
	opts.AllowedEmails = append(opts.AllowedEmails, extraAllowed...)
	s, err := New(opts, three.read)
	if err != nil {
		t.Fatal(err)
	}
	three.h = s.Handler()
	p := &telemetry.Providers{Meter: mp.Meter("test"), MeterProvider: mp, TracerProvider: tp}
	return &traced{h: telemetry.HTTPHandler(three.h, p), mem: mem, reader: reader, logs: logs, three: three}
}

func (tr *traced) do(method, target string, header map[string]string) *httptest.ResponseRecorder {
	return do(tr.h, method, target, header)
}

// spanNamed returns the one span with this name, failing if there is not exactly one.
func (tr *traced) spanNamed(t *testing.T, name string) tracetest.SpanStub {
	t.Helper()
	var found []tracetest.SpanStub
	for _, s := range tr.mem.GetSpans() {
		if s.Name == name {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d spans named %q, want 1; all: %v", len(found), name, spanNames(tr.mem.GetSpans()))
	}
	return found[0]
}

func spanNames(spans tracetest.SpanStubs) []string {
	var out []string
	for _, s := range spans {
		out = append(out, s.Name)
	}
	return out
}

func attrs(s tracetest.SpanStub) map[string]string {
	out := map[string]string{}
	for _, kv := range s.Attributes {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

func sameOrigin() map[string]string {
	return signedIn(map[string]string{"Sec-Fetch-Site": "same-origin"})
}

// AC1: each row of the design's connect and callback outcome table yields its outcome and source,
// `unknown` on the shared TrueLayer callback until the attempt has been read.
func TestConnectAndCallbackSpansCarryTheOutcomeTableValues(t *testing.T) {
	const monzo = "monzo"
	cases := []struct {
		name      string
		span      string
		method    string
		target    string
		header    map[string]string
		setup     func(tr *traced)
		status    int
		outcome   string
		source    string
		errorType string
	}{
		{name: "connect from another site", span: semconv.SpanConnect, method: http.MethodPost, target: "/connect/monzo",
			header: signedIn(nil), status: 400, outcome: "cross_site", source: monzo},
		{name: "connect cannot start", span: semconv.SpanConnect, method: http.MethodPost, target: "/connect/monzo",
			header: sameOrigin(), setup: func(tr *traced) { tr.three.monzo.authoriseErr = errors.New("db down") },
			status: 500, outcome: "start_failed", source: monzo},
		{name: "connect gives an unusable link", span: semconv.SpanConnect, method: http.MethodPost, target: "/connect/monzo",
			header: sameOrigin(), setup: func(tr *traced) { tr.three.monzo.url = "ftp://auth.bank.test/" },
			status: 500, outcome: "start_failed", source: monzo},
		{name: "connect page served", span: semconv.SpanConnect, method: http.MethodPost, target: "/connect/monzo",
			header: sameOrigin(), status: 200, outcome: "started", source: monzo},
		{name: "TrueLayer connect page served", span: semconv.SpanConnect, method: http.MethodPost, target: "/connect/truelayer/ob-lloyds",
			header: sameOrigin(), status: 200, outcome: "started", source: srcLloyds},

		{name: "callback without state", span: semconv.SpanCallback, method: http.MethodGet, target: "/callback/monzo?code=c",
			status: 400, outcome: "invalid_link", source: monzo},
		{name: "callback without code", span: semconv.SpanCallback, method: http.MethodGet, target: "/callback/monzo?state=s",
			status: 400, outcome: "invalid_link", source: monzo},
		{name: "callback exchanged", span: semconv.SpanCallback, method: http.MethodGet, target: "/callback/monzo?code=c&state=s",
			status: 303, outcome: "exchanged", source: monzo},
		{name: "callback refused by the connector", span: semconv.SpanCallback, method: http.MethodGet, target: "/callback/monzo?code=c&state=s",
			setup:  func(tr *traced) { tr.three.monzo.callbackErr = errRefused },
			status: 400, outcome: "refused", source: monzo},
		{name: "callback exchange failed", span: semconv.SpanCallback, method: http.MethodGet, target: "/callback/monzo?code=c&state=s",
			setup:  func(tr *traced) { tr.three.monzo.callbackErr = errors.New("exchange failed") },
			status: 502, outcome: "failed", source: monzo, errorType: "exchange"},

		{name: "shared callback without state", span: semconv.SpanCallback, method: http.MethodGet, target: "/callback/truelayer?code=c",
			status: 400, outcome: "invalid_link", source: "unknown"},
		{name: "shared callback bank error", span: semconv.SpanCallback, method: http.MethodGet, target: "/callback/truelayer?error=access_denied&state=s",
			status: 400, outcome: "bank_declined", source: "unknown"},
		{name: "shared callback attempt unusable", span: semconv.SpanCallback, method: http.MethodGet, target: "/callback/truelayer?code=c&state=never-issued",
			status: 400, outcome: "refused", source: "unknown"},
		{name: "shared callback attempt unreadable", span: semconv.SpanCallback, method: http.MethodGet, target: "/callback/truelayer?code=c&state=s",
			setup:  func(tr *traced) { tr.three.read.attemptErr = errors.New("db down") },
			status: 503, outcome: "failed", source: "unknown", errorType: "attempt_read"},
		{name: "shared callback attempt of a source outside the group", span: semconv.SpanCallback, method: http.MethodGet,
			target: "/callback/truelayer?code=c&state=MONZO-STATE",
			setup:  func(tr *traced) { tr.three.attempt("MONZO-STATE", monzo) },
			status: 400, outcome: "refused", source: monzo},
		{name: "shared callback exchanged", span: semconv.SpanCallback, method: http.MethodGet, target: "/callback/truelayer?code=c&state=LLOYDS-STATE",
			setup:  func(tr *traced) { tr.three.attempt("LLOYDS-STATE", srcLloyds) },
			status: 303, outcome: "exchanged", source: srcLloyds},
		{name: "shared callback refused by the connector", span: semconv.SpanCallback, method: http.MethodGet, target: "/callback/truelayer?code=c&state=LLOYDS-STATE",
			setup: func(tr *traced) {
				tr.three.attempt("LLOYDS-STATE", srcLloyds)
				tr.three.lloyds.callbackErr = errRefused
			},
			status: 400, outcome: "refused", source: srcLloyds},
		{name: "shared callback exchange failed", span: semconv.SpanCallback, method: http.MethodGet, target: "/callback/truelayer?code=c&state=MONZO-TL-STATE",
			setup: func(tr *traced) {
				tr.three.attempt("MONZO-TL-STATE", srcTLMonzo)
				tr.three.tlMonzo.callbackErr = errors.New("exchange failed")
			},
			status: 502, outcome: "failed", source: srcTLMonzo, errorType: "exchange"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := newTraced(t)
			if c.setup != nil {
				c.setup(tr)
			}
			rec := tr.do(c.method, c.target, c.header)
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d", rec.Code, c.status)
			}
			span := tr.spanNamed(t, c.span)
			a := attrs(span)
			if a[semconv.SpanConnectOutcome] != c.outcome {
				t.Errorf("%s = %q, want %q", semconv.SpanConnectOutcome, a[semconv.SpanConnectOutcome], c.outcome)
			}
			if a[semconv.SpanSource] != c.source {
				t.Errorf("%s = %q, want %q", semconv.SpanSource, a[semconv.SpanSource], c.source)
			}
			if a[semconv.SpanErrorType] != c.errorType {
				t.Errorf("%s = %q, want %q", semconv.SpanErrorType, a[semconv.SpanErrorType], c.errorType)
			}
			if c.errorType != "" && (span.Status.Code != codes.Error || span.Status.Description != c.errorType) {
				t.Errorf("status = %v %q, want an error with the category %q", span.Status.Code, span.Status.Description, c.errorType)
			}
			if c.errorType == "" && c.outcome == "exchanged" && span.Status.Code == codes.Error {
				t.Errorf("an exchanged callback is marked failed")
			}
			if span.SpanKind != trace.SpanKindInternal {
				t.Errorf("kind = %v, want internal", span.SpanKind)
			}
			// The span is a child of the request's server span.
			var server tracetest.SpanStub
			for _, s := range tr.mem.GetSpans() {
				if s.SpanKind == trace.SpanKindServer {
					server = s
				}
			}
			if !server.SpanContext.IsValid() || span.Parent.SpanID() != server.SpanContext.SpanID() ||
				span.SpanContext.TraceID() != server.SpanContext.TraceID() {
				t.Errorf("span is not a child of the server span: parent %v, server %v", span.Parent, server.SpanContext)
			}
			for k := range a {
				if !semconv.IsSpanAttribute(k) {
					t.Errorf("attribute %q is not on the exporter allow-list", k)
				}
			}
		})
	}
}

// A request the sign-in refuses never reaches the connect handler: it has its server span only.
func TestAConnectTheSignInRefusesHasNoConnectSpan(t *testing.T) {
	tr := newTraced(t)
	rec := tr.do(http.MethodPost, "/connect/monzo", map[string]string{"Sec-Fetch-Site": "same-origin"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	for _, s := range tr.mem.GetSpans() {
		if s.Name == semconv.SpanConnect || s.Name == semconv.SpanCallback {
			t.Fatalf("a refused sign-in made a %s span", s.Name)
		}
	}
	if len(tr.mem.GetSpans()) != 1 {
		t.Fatalf("spans = %v, want the server span only", spanNames(tr.mem.GetSpans()))
	}
}

// The connector's context carries the callback span, so work a callback starts can link to it.
func TestTheConnectorRunsInsideTheCallbackSpan(t *testing.T) {
	tr := newTraced(t)
	var inside trace.SpanContext
	tr.three.monzo.onCallback = func(ctx context.Context) { inside = trace.SpanContextFromContext(ctx) }
	if rec := tr.do(http.MethodGet, "/callback/monzo?code=c&state=s", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d", rec.Code)
	}
	span := tr.spanNamed(t, semconv.SpanCallback)
	if inside.SpanID() != span.SpanContext.SpanID() {
		t.Errorf("the connector's span = %v, want the callback span %v", inside.SpanID(), span.SpanContext.SpanID())
	}
}

// Leak tests 1 and 2 through the whole handler, for both callback routes: the authorisation code
// and the state a callback carries are in no span, log line or metric.
func TestACallbackWithCanaryCodeAndStateLeaksNothing(t *testing.T) {
	const canaryCode, canaryState = "CANARYCODE-7c2e", "CANARYSTATE-91ab"
	for _, route := range []string{"/callback/monzo", "/callback/truelayer"} {
		t.Run(route, func(t *testing.T) {
			tr := newTraced(t)
			tr.three.attempt(canaryState, srcLloyds)
			header := map[string]string{
				"Referer":       "https://auth.bank.test/?code=CANARYREFERER",
				"Authorization": "Bearer CANARYBEARER",
				"Cookie":        "session=CANARYCOOKIE",
			}
			rec := tr.do(http.MethodGet, route+"?code="+canaryCode+"&state="+canaryState, header)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			span := tr.spanNamed(t, semconv.SpanCallback)
			if got := attrs(span)[semconv.SpanConnectOutcome]; got != "exchanged" {
				t.Fatalf("outcome = %q, want exchanged", got)
			}

			var rm metricdata.ResourceMetrics
			if err := tr.reader.Collect(context.Background(), &rm); err != nil {
				t.Fatal(err)
			}
			everything := map[string]string{
				"spans":   fmt.Sprintf("%#v", tr.mem.GetSpans()),
				"logs":    tr.logs.String(),
				"metrics": fmt.Sprintf("%#v", rm),
			}
			for what, text := range everything {
				if text == "" {
					t.Errorf("%s is empty: the test would prove nothing", what)
				}
				if strings.Contains(text, "CANARY") || strings.Contains(text, "7c2e") || strings.Contains(text, "91ab") {
					t.Errorf("a canary reached the %s: %s", what, text)
				}
			}
		})
	}
}

// Leak tests 1 and 2 for a callback whose exchange fails (502): the failure path records neither
// the code and state nor the connector's error text.
func TestAFailedExchangeWithCanaryValuesLeaksNothing(t *testing.T) {
	tr := newTraced(t)
	tr.three.monzo.callbackErr = errors.New("exchange failed quoting CANARYERR")
	rec := tr.do(http.MethodGet, "/callback/monzo?code=CANARYCODE&state=CANARYSTATE", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d", rec.Code)
	}
	if dump := fmt.Sprintf("%#v %s", tr.mem.GetSpans(), tr.logs.String()); strings.Contains(dump, "CANARY") {
		t.Fatalf("a canary reached a span or a log line: %s", dump)
	}
}

// Leak test 2: the signed-in address the proxy passes in the configured identity header reaches
// the connect handler, and is in no span, log line or metric, for a connect that starts, one the
// same-origin check refuses, and the status page.
func TestTheSignedInAddressLeaksNothing(t *testing.T) {
	const canary = "canary-ident-8e31@example.com"
	tr := newTraced(t, canary)
	for name, c := range map[string]struct {
		method, path string
		header       map[string]string
		status       int
	}{
		"connect starts":          {http.MethodPost, "/connect/monzo", map[string]string{authHeader: canary, "Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		"connect cross site":      {http.MethodPost, "/connect/monzo", map[string]string{authHeader: canary}, http.StatusBadRequest},
		"TrueLayer connect":       {http.MethodPost, "/connect/truelayer/ob-lloyds", map[string]string{authHeader: canary, "Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		"status page":             {http.MethodGet, "/", map[string]string{authHeader: canary}, http.StatusOK},
		"another identity header": {http.MethodPost, "/connect/monzo", map[string]string{authHeader: canary, "Sec-Fetch-Site": "same-origin", "X-Forwarded-User": canary}, http.StatusOK},
	} {
		if rec := tr.do(c.method, c.path, c.header); rec.Code != c.status {
			t.Fatalf("%s: status %d, want %d", name, rec.Code, c.status)
		}
	}
	if n := len(tr.spanNamedAll(semconv.SpanConnect)); n != 4 {
		t.Fatalf("%d connect spans, want 4: the requests did not reach the handler", n)
	}
	var rm metricdata.ResourceMetrics
	if err := tr.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	everything := map[string]string{
		"spans":   fmt.Sprintf("%#v", tr.mem.GetSpans()),
		"logs":    tr.logs.String(),
		"metrics": fmt.Sprintf("%#v", rm),
	}
	for what, text := range everything {
		if text == "" {
			t.Errorf("%s is empty: the test would prove nothing", what)
		}
		if strings.Contains(text, "canary-ident") || strings.Contains(text, "8e31") {
			t.Errorf("the signed-in address reached the %s: %s", what, text)
		}
	}
}

// spanNamedAll returns every span with this name.
func (tr *traced) spanNamedAll(name string) []tracetest.SpanStub {
	var found []tracetest.SpanStub
	for _, s := range tr.mem.GetSpans() {
		if s.Name == name {
			found = append(found, s)
		}
	}
	return found
}

// Without a tracer the pages work and make no span.
func TestNoTracerMakesNoSpansAndChangesNothing(t *testing.T) {
	conn := &fakeConn{}
	s, err := New(options(io.Discard, monzoSource(conn)), &fakeReader{})
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	if rec := do(h, http.MethodGet, "/callback/monzo?code=c&state=s", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("callback %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/connect/monzo", sameOrigin()); rec.Code != http.StatusOK {
		t.Fatalf("connect %d", rec.Code)
	}
}

// The shared callback knows no source until the attempt is read, so a request that fails before
// that has the source unknown, and one that gets past it has the attempt's source even when the
// exchange then fails. This pins the transition on one request.
func TestSharedCallbackSourceIsUnknownUntilTheAttemptIsRead(t *testing.T) {
	tr := newTraced(t)
	var during string
	// The attempt is read before the connector runs: inside HandleCallback the span already has
	// the attempt's source. The span is still open here, so read it from the live context.
	tr.three.lloyds.onCallback = func(ctx context.Context) {
		if ro, ok := trace.SpanFromContext(ctx).(sdktrace.ReadOnlySpan); ok {
			for _, kv := range ro.Attributes() {
				if string(kv.Key) == semconv.SpanSource {
					during = kv.Value.String()
				}
			}
		}
	}
	tr.three.attempt("STATE-1", srcLloyds)
	if rec := tr.do(http.MethodGet, "/callback/truelayer?code=c&state=STATE-1", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d", rec.Code)
	}
	if during != srcLloyds {
		t.Errorf("source while the connector ran = %q, want %q", during, srcLloyds)
	}
	if got := attrs(tr.spanNamed(t, semconv.SpanCallback))[semconv.SpanSource]; got != srcLloyds {
		t.Errorf("final source = %q", got)
	}
}

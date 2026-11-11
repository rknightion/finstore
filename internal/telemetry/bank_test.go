package telemetry

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rknightion/finstore/internal/bankhttp"
	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/telemetry/telemetrytest"
)

// Canary values: none may appear anywhere in what a process exports.
const (
	canaryClientSecret = "CANARY-CLIENT-SECRET"
	canaryAccess       = "CANARY-ACCESS-TOKEN"
	canaryRefresh      = "CANARY-REFRESH-TOKEN"
	canaryBearer       = "CANARY-BEARER"
	canaryQuery        = "CANARY-QUERY-VALUE"
	canaryBankText     = "CANARY-BANK-TEXT"
	canaryAccountID    = "acc-canary-id"
)

// canaryBank is a fake bank for the exporter tests: it answers the token call with canary tokens,
// rate limits the first transactions call, and keeps every request's headers.
type canaryBank struct {
	srv *httptest.Server
	mu  sync.Mutex
	// headers is the headers of every request, in order.
	headers []http.Header
	txCalls int
}

func newCanaryBank(t *testing.T) *canaryBank {
	t.Helper()
	b := &canaryBank{}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.headers = append(b.headers, r.Header.Clone())
		var tx int
		if strings.HasSuffix(r.URL.Path, "/transactions") {
			b.txCalls++
			tx = b.txCalls
		}
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/oauth2/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": canaryAccess, "refresh_token": canaryRefresh, "expires_in": 21600,
			})
		case tx == 1:
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":"too_many_requests","message":"` + canaryBankText + `"}`))
		default:
			_, _ = w.Write([]byte(`{"transactions":[],"note":"` + canaryBankText + `"}`))
		}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

var canaryRules = []bankhttp.Rule{
	{Method: "POST", Path: "/oauth2/token"},
	{Method: "GET", Path: "/accounts/{id}/transactions"},
}

// exportedBankCalls runs a token exchange, a rate-limited then successful data call and a
// transport failure through the real providers and the real span allow-list, and returns what
// reached the collector.
func exportedBankCalls(t *testing.T) (*telemetrytest.Sink, *canaryBank, string) {
	t.Helper()
	// A propagator that would inject on every outgoing request, so a client that used the
	// global one would show it.
	prev := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })

	sink := telemetrytest.New(t)
	ctx := context.Background()
	p, err := Setup(ctx, config.Config{OTLPEndpoint: sink.URL(), Environment: "test", LogLevel: slog.LevelInfo},
		Identity{Role: RoleServe, Version: "main-abc1234", InstanceID: ServeInstanceID})
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewMetrics(p.Meter)
	if err != nil {
		t.Fatal(err)
	}
	bank := newCanaryBank(t)
	noSleep := bankhttp.WithSleep(func(context.Context, time.Duration) error { return nil })
	c, err := bankhttp.New(bank.srv.URL+"/api", canaryRules, noSleep,
		bankhttp.WithTelemetry(p.Tracer(semconv.ServiceName), "canarybank", m.BankObserver("canarybank")))
	if err != nil {
		t.Fatal(err)
	}
	ctx, root := p.Tracer(semconv.ServiceName).Start(ctx, "work")
	if _, err := c.Do(ctx, bankhttp.Request{Method: "POST", Path: "/oauth2/token", Form: url.Values{
		"grant_type": {"refresh_token"}, "client_id": {"made-up-client"}, "client_secret": {canaryClientSecret},
		"refresh_token": {canaryRefresh},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(ctx, bankhttp.Request{
		Method: "GET", Path: "/accounts/" + canaryAccountID + "/transactions", Bearer: canaryBearer,
		Query: url.Values{"since": {canaryQuery}},
	}); err != nil {
		t.Fatal(err)
	}
	root.End()
	// A transport failure on a client pointed at nothing.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	c2, err := bankhttp.New(deadURL, canaryRules, noSleep, bankhttp.WithoutRetries(),
		bankhttp.WithTelemetry(p.Tracer(semconv.ServiceName), "canarybank", m.BankObserver("canarybank")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Do(context.Background(), bankhttp.Request{Method: "GET", Path: "/accounts/" + canaryAccountID + "/transactions"}); err == nil {
		t.Fatal("a call to a closed port succeeded")
	}
	if err := p.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	return sink, bank, bank.srv.URL
}

func TestBankCallSpansAndMetricsReachTheCollectorThroughTheAllowList(t *testing.T) {
	sink, _, bankURL := exportedBankCalls(t)
	spans := sink.Spans()
	byName := map[string][]telemetrytest.Span{}
	for _, s := range spans {
		byName[s.Name] = append(byName[s.Name], s)
	}
	logical := byName["bank GET /accounts/{id}/transactions"]
	attempts := byName["GET /accounts/{id}/transactions"]
	// The data call and the closed-port call are logical spans; the data call has two attempts
	// (the 429 and the 200) and the closed-port call one.
	if len(logical) != 2 || len(attempts) != 3 {
		t.Fatalf("logical %d attempts %d: %v", len(logical), len(attempts), names(spans))
	}
	// AC1 through the real exporter: the resend attempt, with its count.
	var resent []telemetrytest.Span
	for _, a := range attempts {
		if _, ok := a.Attrs[semconv.SpanHTTPRequestResendCount]; ok {
			resent = append(resent, a)
		}
	}
	if len(resent) != 1 || resent[0].Attrs[semconv.SpanHTTPRequestResendCount] != "1" || resent[0].Attrs[semconv.SpanHTTPResponseStatusCode] != "200" {
		t.Errorf("resent attempts = %v", resent)
	}
	// AC3 through the real exporter: url.full survives the filter and is the bare template.
	want := bankURL + "/api/accounts/{id}/transactions"
	var full int
	for _, a := range attempts {
		if u, ok := a.Attrs[semconv.SpanURLFull]; ok && strings.HasPrefix(u, bankURL) {
			full++
			if u != want {
				t.Errorf("url.full = %q, want %q", u, want)
			}
		}
	}
	if full != 2 { // the 429 and the 200; the closed-port attempt has another host
		t.Errorf("%d attempts kept a url.full on the bank", full)
	}
	// Every attribute these spans set is on the allow-list: nothing was dropped.
	for _, s := range spans {
		if s.Dropped != 0 {
			t.Errorf("span %q had %d attributes dropped by the allow-list: %v", s.Name, s.Dropped, s.Attrs)
		}
		if s.Events != 0 {
			t.Errorf("span %q exported %d events", s.Name, s.Events)
		}
	}

	metrics := sink.Metrics()
	// AC1: one rate_limited retry. The failed call adds none, with retries off.
	retries := metrics[semconv.MetricBankRetries]
	if len(retries.Points) != 1 || retries.Points[0].Value != 1 ||
		retries.Points[0].Attrs[semconv.AttrReason] != "rate_limited" ||
		retries.Points[0].Attrs[semconv.AttrSource] != "canarybank" ||
		retries.Points[0].Attrs[semconv.AttrEndpoint] != "/accounts/{id}/transactions" {
		t.Errorf("retries = %+v", retries.Points)
	}
	// AC2: the transport failure is a no_response point, beside the 4xx and 2xx ones.
	classes := map[string]float64{}
	for _, p := range metrics[semconv.MetricBankRequestDuration].Points {
		if p.Attrs[semconv.AttrSource] != "canarybank" {
			t.Errorf("duration point source = %q", p.Attrs[semconv.AttrSource])
		}
		classes[p.Attrs[semconv.AttrStatusClass]] += p.Value
		if len(p.Boundaries) != 10 || p.Boundaries[0] != 0.05 || p.Boundaries[9] != 30 {
			t.Errorf("buckets = %v", p.Boundaries)
		}
	}
	if classes["no_response"] != 1 || classes["4xx"] != 1 || classes["2xx"] != 2 || len(classes) != 3 {
		t.Errorf("status classes = %v, want 2xx 2, 4xx 1, no_response 1", classes)
	}
}

func names(spans []telemetrytest.Span) []string {
	var out []string
	for _, s := range spans {
		out = append(out, s.Name)
	}
	return out
}

// Leak test 3: a canary client secret, access token, refresh token, bearer, query value, response
// text and the id a bank supplied appear in no exported span name, attribute, status, link or
// resource, and in no metric attribute.
func TestNoCanaryReachesTheCollectorFromBankCalls(t *testing.T) {
	sink, _, _ := exportedBankCalls(t)
	canaries := []string{canaryClientSecret, canaryAccess, canaryRefresh, canaryBearer, canaryQuery, canaryBankText, canaryAccountID}
	check := func(where, text string) {
		for _, c := range canaries {
			if strings.Contains(text, c) {
				t.Errorf("%s carries %q: %q", where, c, text)
			}
		}
	}
	if len(sink.Spans()) == 0 {
		t.Fatal("no spans were exported")
	}
	for _, s := range sink.Spans() {
		check("span name", s.Name)
		check("span status of "+s.Name, s.StatusMessage)
		for k, v := range s.Attrs {
			check("span "+s.Name+" key "+k, k)
			check("span "+s.Name+" attribute "+k, v)
		}
		for k, v := range s.Resource {
			check("resource "+k, v)
		}
	}
	for _, r := range sink.Requests() {
		check("a raw export body", string(r.Body))
	}
	for name, m := range sink.Metrics() {
		for _, p := range m.Points {
			for k, v := range p.Attrs {
				check("metric "+name+" attribute "+k, v)
			}
		}
	}
}

// Leak test 9: the bank sees no traceparent, tracestate or baggage, with a propagator installed
// that would inject them.
func TestFakeBankSeesNoPropagationHeader(t *testing.T) {
	_, bank, _ := exportedBankCalls(t)
	bank.mu.Lock()
	defer bank.mu.Unlock()
	if len(bank.headers) != 3 {
		t.Fatalf("the bank saw %d requests, want 3", len(bank.headers))
	}
	for i, h := range bank.headers {
		for _, name := range []string{"Traceparent", "Tracestate", "Baggage"} {
			if v := h.Values(name); len(v) != 0 {
				t.Errorf("request %d carries %s: %v", i, name, v)
			}
		}
	}
}

// BankObserver records the duration in seconds and the retry count with fixed attribute keys,
// through any meter provider.
func TestBankObserverRecordsDurationAndRetries(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithView(histogramViews()...))
	m, err := NewMetrics(mp.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	obs := m.BankObserver("monzo")
	ctx := context.Background()
	obs.Attempt(ctx, "/transactions", "2xx", 1500*time.Millisecond)
	obs.Attempt(ctx, "/transactions", semconv.StatusClassNoResponse, 20*time.Millisecond)
	obs.Retry(ctx, "/transactions", semconv.RetryReasonServerError)
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	var sawHist, sawRetry bool
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			switch d := mt.Data.(type) {
			case metricdata.Histogram[float64]:
				if mt.Name != semconv.MetricBankRequestDuration || mt.Unit != "s" || len(d.DataPoints) != 2 {
					t.Errorf("histogram %s unit %s points %d", mt.Name, mt.Unit, len(d.DataPoints))
				}
				for _, dp := range d.DataPoints {
					if v, _ := dp.Attributes.Value(semconv.AttrStatusClass); v.AsString() == "2xx" && dp.Sum != 1.5 {
						t.Errorf("2xx sum = %v, want 1.5", dp.Sum)
					}
				}
				sawHist = true
			case metricdata.Sum[int64]:
				if mt.Name == semconv.MetricBankRetries {
					sawRetry = len(d.DataPoints) == 1 && d.DataPoints[0].Value == 1
				}
			}
		}
	}
	if !sawHist || !sawRetry {
		t.Errorf("histogram seen %v, retry seen %v", sawHist, sawRetry)
	}
}

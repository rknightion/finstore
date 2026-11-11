package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rknightion/finstore/internal/semconv"
)

// httpFixture is providers whose spans pass the real exporter filter into memory, and whose
// metrics can be collected.
func httpFixture(t *testing.T) (*Providers, *tracetest.InMemoryExporter, *sdkmetric.ManualReader) {
	t.Helper()
	mem := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(newFilteringExporter(mem, semconv.IsSpanAttribute, &countingDrops{})))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithView(histogramViews()...))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()); _ = mp.Shutdown(context.Background()) })
	return &Providers{Meter: mp.Meter("test"), MeterProvider: mp, TracerProvider: tp, LogHandler: slog.DiscardHandler}, mem, reader
}

func serveMux() http.Handler {
	mux := http.NewServeMux()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.Handle("GET /healthz", ok)
	mux.Handle("GET /{$}", ok)
	mux.Handle("GET /callback/monzo", ok)
	return mux
}

func TestFailSetsAnErrorStatusWithTheCategoryAndNothingElse(t *testing.T) {
	var rec tracetest.SpanRecorder
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(&rec))
	_, span := tp.Tracer("t").Start(context.Background(), "work")
	Fail(span, "rate_limited")
	span.End()
	got := rec.Ended()
	if len(got) != 1 {
		t.Fatalf("%d spans", len(got))
	}
	if st := got[0].Status(); st.Code != codes.Error || st.Description != "rate_limited" {
		t.Errorf("status = %v %q, want Error rate_limited", st.Code, st.Description)
	}
	if len(got[0].Events()) != 0 {
		t.Errorf("Fail recorded an event: %v", got[0].Events())
	}
}

func TestHTTPHandlerSkipsHealthzAndTracesTheRoutes(t *testing.T) {
	p, mem, reader := httpFixture(t)
	h := HTTPHandler(serveMux(), p)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("healthz answered %d %q: the wrapper must pass the request through", rec.Code, rec.Body.String())
	}
	if n := len(mem.GetSpans()); n != 0 {
		t.Fatalf("/healthz made %d spans, want none", n)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/ answered %d", rec.Code)
	}
	spans := mem.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("/ made %d spans, want 1", len(spans))
	}
	attrs := attrMap(spans[0].Attributes)
	if attrs[semconv.SpanHTTPRoute] != "/{$}" {
		t.Errorf("http.route = %q, want the mux pattern's path: %v", attrs[semconv.SpanHTTPRoute], attrs)
	}
	if spans[0].Name != "GET /{$}" {
		t.Errorf("span name = %q, want the route", spans[0].Name)
	}
	if attrs[semconv.SpanHTTPResponseStatusCode] != "200" || attrs[semconv.SpanHTTPRequestMethod] != "GET" {
		t.Errorf("attributes = %v", attrs)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "http.server.request.duration" {
				found = true
				h, ok := m.Data.(metricdata.Histogram[float64])
				if !ok || len(h.DataPoints) != 1 {
					t.Fatalf("request duration = %#v, want one point (healthz is not recorded)", m.Data)
				}
				if got := h.DataPoints[0].Bounds; len(got) != 14 || got[0] != 0.005 || got[13] != 90 {
					t.Errorf("buckets = %v, want 0.005 to 90", got)
				}
			}
		}
	}
	if !found {
		t.Error("no http.server.request.duration metric")
	}
}

// An incoming traceparent is a link, never the parent, on a public endpoint.
func TestHTTPHandlerLinksAnIncomingTraceContext(t *testing.T) {
	p, mem, _ := httpFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	const incoming = "0af7651916cd43dd8448eb211c80319c"
	req.Header.Set("Traceparent", "00-"+incoming+"-b7ad6b7169203331-01")
	HTTPHandler(serveMux(), p).ServeHTTP(httptest.NewRecorder(), req)

	spans := mem.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("%d spans", len(spans))
	}
	s := spans[0]
	if s.SpanContext.TraceID().String() == incoming || s.Parent.IsValid() {
		t.Errorf("the incoming trace became the parent: trace %s parent %v", s.SpanContext.TraceID(), s.Parent)
	}
	if len(s.Links) != 1 || s.Links[0].SpanContext.TraceID().String() != incoming {
		t.Errorf("links = %#v, want one to the incoming trace", s.Links)
	}
}

// Leak tests 1 and 2 at the handler: a callback carrying an authorisation code and state, and the
// sign-in header, never appear in a span name, attribute, status, event or link.
func TestHTTPHandlerExportsNoQueryHeaderOrCookie(t *testing.T) {
	p, mem, _ := httpFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/callback/monzo?code=CANARYCODE&state=CANARYSTATE", nil)
	req.Header.Set("X-Auth-Request-Email", "canary-CANARYMAIL@example.com")
	req.Header.Set("Authorization", "Bearer CANARYBEARER")
	req.Header.Set("Cookie", "session=CANARYCOOKIE")
	req.Header.Set("Referer", "https://bank.example.test/?code=CANARYREFERER")
	HTTPHandler(serveMux(), p).ServeHTTP(httptest.NewRecorder(), req)

	spans := mem.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("%d spans", len(spans))
	}
	if dump := fmt.Sprintf("%#v", spans); strings.Contains(dump, "CANARY") {
		t.Fatalf("a canary reached the exporter: %s", dump)
	}
	attrs := attrMap(spans[0].Attributes)
	if attrs[semconv.SpanURLPath] != "/callback/monzo" || spans[0].Name != "GET /callback/monzo" {
		t.Errorf("span = %q %v, want the route and path without the query", spans[0].Name, attrs)
	}
	for k := range attrs {
		if !semconv.IsSpanAttribute(k) {
			t.Errorf("attribute %q is not on the allow-list", k)
		}
	}
}

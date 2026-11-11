package telemetry

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	otellog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/telemetry/telemetrytest"
)

var serveID = Identity{Role: RoleServe, Version: "main-abc1234", InstanceID: ServeInstanceID}

// lockedBuffer is a bytes.Buffer safe to write from the SDK's goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// exportOneSpan sets providers up against a plain-HTTP sink, makes one span, shuts down, and
// returns the span as the sink saw it.
func exportOneSpan(t *testing.T, id Identity) telemetrytest.Span {
	t.Helper()
	sink := telemetrytest.New(t)
	ctx := context.Background()
	p, err := Setup(ctx, config.Config{OTLPEndpoint: sink.URL(), Environment: "test", LogLevel: slog.LevelInfo}, id)
	if err != nil {
		t.Fatal(err)
	}
	_, span := p.Tracer("test").Start(ctx, "work")
	span.End()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	spans := sink.Spans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
	return spans[0]
}

func TestSetupExportsTracesToTheTracesPathWithBasicAuth(t *testing.T) {
	sink := telemetrytest.NewTLS(t)
	ctx := context.Background()
	cfg := config.Config{
		OTLPEndpoint: sink.URL() + "/otlp/", OTLPInstanceID: "12345", OTLPToken: "made-up-token",
		Environment: "test", LogLevel: slog.LevelInfo,
	}
	p, err := setup(ctx, cfg, serveID, sink.TLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	_, span := p.Tracer("test").Start(ctx, "work")
	span.SetAttributes(attribute.String(semconv.SpanSource, "monzo"))
	span.End()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	spans := sink.Spans()
	if len(spans) != 1 || spans[0].Name != "work" || spans[0].Attrs[semconv.SpanSource] != "monzo" {
		t.Fatalf("spans at the sink = %#v", spans)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("12345:made-up-token"))
	sawTraces := false
	for _, r := range sink.Requests() {
		if got := r.Header.Get("Authorization"); got != want {
			t.Errorf("%s: Authorization = %q, want %q", r.Path, got, want)
		}
		sawTraces = sawTraces || r.Path == "/otlp/v1/traces"
	}
	if !sawTraces {
		t.Errorf("no request reached /otlp/v1/traces: %v", sink.Paths())
	}
}

func TestSetupServiceVersionIsTheInjectedOne(t *testing.T) {
	id := serveID
	id.Version = "main-1234567"
	if got := exportOneSpan(t, id).Resource[semconv.AttrServiceVersion]; got != "main-1234567" {
		t.Errorf("service.version = %q, want the injected version (today it is the build-info (devel))", got)
	}
}

// serve's identity is fixed in code: an OTEL_RESOURCE_ATTRIBUTES that a docker exec inherits can
// add attributes but cannot change finstore's own.
func TestSetupServeIdentityIsFixedAgainstTheEnvironment(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.instance.id=env-instance,service.name=env-name,"+
		"service.version=env-version,deployment.environment.name=env-env,finstore.process.role=env-role,"+
		"team=ops")
	t.Setenv("OTEL_SERVICE_NAME", "env-service")
	res := exportOneSpan(t, serveID).Resource
	for k, want := range map[string]string{
		semconv.AttrServiceName: "finstore", semconv.AttrServiceVersion: "main-abc1234",
		semconv.AttrServiceInstanceID: "serve", semconv.AttrDeploymentEnvironment: "test",
		semconv.AttrProcessRole: "serve", "team": "ops",
	} {
		if res[k] != want {
			t.Errorf("resource %s = %q, want %q", k, res[k], want)
		}
	}
	if v, ok := res[semconv.AttrCommand]; ok {
		t.Errorf("serve carries finstore.command = %q, want it absent", v)
	}
	if ServeInstanceID != "serve" {
		t.Errorf("ServeInstanceID = %q", ServeInstanceID)
	}
}

// A malformed OTEL_RESOURCE_ATTRIBUTES pair is dropped, not fatal: before telemetry existed the
// variable was ignored, so it must not stop serve from starting. The valid pairs still apply.
func TestSetupToleratesAMalformedResourceAttribute(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "team=ops,secretish")
	res := exportOneSpan(t, serveID).Resource
	if res[semconv.AttrServiceName] != "finstore" || res[semconv.AttrServiceInstanceID] != "serve" || res["team"] != "ops" {
		t.Errorf("resource = %v", res)
	}
}

func TestSetupCLIIdentityCarriesItsCommandAndOwnInstance(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.instance.id=serve,finstore.command=other")
	id := Identity{Role: RoleCLI, Command: "account add", Version: "main-abc1234", InstanceID: "5b1b4b7e-0000-4000-8000-000000000001"}
	res := exportOneSpan(t, id).Resource
	if res[semconv.AttrProcessRole] != "cli" || res[semconv.AttrCommand] != "account add" ||
		res[semconv.AttrServiceInstanceID] != id.InstanceID || res[semconv.AttrServiceName] != "finstore" {
		t.Errorf("resource = %v", res)
	}
}

// With no instance id given, a CLI process gets a fresh UUID each time and serve gets its
// constant.
func TestSetupDefaultsTheInstanceID(t *testing.T) {
	uuidRE := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	cli := Identity{Role: RoleCLI, Command: "coverage", Version: "v"}
	a, b := exportOneSpan(t, cli).Resource[semconv.AttrServiceInstanceID], exportOneSpan(t, cli).Resource[semconv.AttrServiceInstanceID]
	if !uuidRE.MatchString(a) || !uuidRE.MatchString(b) || a == b {
		t.Errorf("CLI instance ids = %q and %q, want two different UUIDs", a, b)
	}
	if got := exportOneSpan(t, Identity{Role: RoleServe, Version: "v"}).Resource[semconv.AttrServiceInstanceID]; got != ServeInstanceID {
		t.Errorf("serve with no id = %q, want %q", got, ServeInstanceID)
	}
}

func TestSetupDisabledAndNoEndpointAreNoOps(t *testing.T) {
	sink := telemetrytest.New(t)
	ctx := context.Background()
	for name, cfg := range map[string]config.Config{
		"no endpoint": {},
		"disabled":    {OTLPEndpoint: sink.URL(), OTelSDKDisabled: true},
	} {
		p, err := Setup(ctx, cfg, serveID)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if p.Meter == nil || p.MeterProvider == nil || p.TracerProvider == nil || p.LogHandler == nil || p.Tracer("x") == nil {
			t.Fatalf("%s: a provider is nil", name)
		}
		_, span := p.Tracer("x").Start(ctx, "work")
		if span.SpanContext().IsValid() {
			t.Errorf("%s: a no-op tracer made a valid span", name)
		}
		span.End()
		if err := p.ForceFlush(ctx); err != nil {
			t.Errorf("%s: flush: %v", name, err)
		}
		if err := p.Shutdown(ctx); err != nil {
			t.Errorf("%s: shutdown: %v", name, err)
		}
	}
	if n := len(sink.Requests()); n != 0 {
		t.Errorf("a no-op provider sent %d requests", n)
	}
}

// Contrib libraries default to the globals, so Setup installs the providers and the trace
// context propagator, and no baggage.
func TestSetupInstallsGlobals(t *testing.T) {
	sink := telemetrytest.New(t)
	p, err := Setup(context.Background(), config.Config{OTLPEndpoint: sink.URL()}, serveID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	if otel.GetTracerProvider() != p.TracerProvider {
		t.Error("the global tracer provider is not the one Setup built")
	}
	if otel.GetMeterProvider() != p.MeterProvider {
		t.Error("the global meter provider is not the one Setup built")
	}
	fields := strings.Join(otel.GetTextMapPropagator().Fields(), ",")
	if !strings.Contains(fields, "traceparent") || strings.Contains(fields, "baggage") {
		t.Errorf("propagator fields = %q, want traceparent and no baggage", fields)
	}
}

// A CLI command must not hang on a dead collector: shutdown flushes a pending span within the
// budget it is given.
func TestShutdownFlushesAPendingSpanWithinTheCLIBudget(t *testing.T) {
	sink := telemetrytest.New(t)
	ctx := context.Background()
	p, err := Setup(ctx, config.Config{OTLPEndpoint: sink.URL()}, Identity{Role: RoleCLI, Command: "coverage", Version: "v"})
	if err != nil {
		t.Fatal(err)
	}
	_, span := p.Tracer("t").Start(ctx, "pending")
	span.End() // the batch processor is holding it for its 5 s timer
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := p.Shutdown(sctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("shutdown took %s", time.Since(start))
	}
	if n := len(sink.Spans()); n != 1 {
		t.Fatalf("%d spans at the sink after shutdown, want the pending one", n)
	}
}

func TestShutdownGivesUpWhenTheEndpointHangs(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	ctx := context.Background()
	p, err := Setup(ctx, config.Config{OTLPEndpoint: srv.URL}, Identity{Role: RoleCLI, Command: "coverage", Version: "v"})
	if err != nil {
		t.Fatal(err)
	}
	_, span := p.Tracer("t").Start(ctx, "pending")
	span.End()
	sctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_ = p.Shutdown(sctx)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("shutdown took %s against a dead endpoint, want it bound by its context", d)
	}
}

func TestSetupExportsRuntimeMetricsAndTheStartTime(t *testing.T) {
	sink := telemetrytest.New(t)
	ctx := context.Background()
	p, err := Setup(ctx, config.Config{OTLPEndpoint: sink.URL()}, serveID)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewMetrics(p.Meter)
	if err != nil {
		t.Fatal(err)
	}
	m.ProcessStarted(ctx, time.Unix(1_700_000_000, 0))
	if err := p.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	got := sink.Metrics()
	for _, name := range []string{"go.goroutine.count", "go.memory.used", semconv.MetricProcessStartTime} {
		if len(got[name].Points) == 0 {
			t.Errorf("metric %s was not exported; got %d metrics", name, len(got))
		}
	}
	if pts := got[semconv.MetricProcessStartTime].Points; len(pts) != 1 || pts[0].Value != 1_700_000_000 || len(pts[0].Attrs) != 0 {
		t.Errorf("start time points = %#v", pts)
	}
}

func TestHistogramViewsSetTheExplicitBuckets(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader), metric.WithView(histogramViews()...))
	meter := mp.Meter("test")
	runBuckets := []float64{1, 5, 15, 30, 60, 120, 300, 600, 900, 1800, 2700, 3600}
	want := map[string][]float64{
		semconv.MetricSyncRunDuration:      runBuckets,
		semconv.MetricSchedulerJobDuration: runBuckets,
		semconv.MetricBankRequestDuration:  {0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30},
		"http.server.request.duration":     {0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 90},
	}
	for name := range want {
		h, err := meter.Float64Histogram(name, otelmetric.WithUnit("s"))
		if err != nil {
			t.Fatal(err)
		}
		h.Record(context.Background(), 0.5)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			h, ok := mt.Data.(metricdata.Histogram[float64])
			if !ok {
				continue
			}
			seen++
			got := h.DataPoints[0].Bounds
			if len(got) != len(want[mt.Name]) {
				t.Errorf("%s buckets = %v, want %v", mt.Name, got, want[mt.Name])
				continue
			}
			for i := range got {
				if got[i] != want[mt.Name][i] {
					t.Errorf("%s buckets = %v, want %v", mt.Name, got, want[mt.Name])
					break
				}
			}
		}
	}
	if seen != len(want) {
		t.Errorf("collected %d histograms, want %d", seen, len(want))
	}
}

func TestProcessStartedIsAGaugeThatSurvivesCollections(t *testing.T) {
	m, read := collect(t)
	m.ProcessStarted(context.Background(), time.Unix(1_700_000_000, 0))
	for i := 1; i <= 2; i++ {
		mt, ok := read()[semconv.MetricProcessStartTime]
		if !ok {
			t.Fatalf("collection %d has no start time gauge", i)
		}
		g, ok := mt.Data.(metricdata.Gauge[int64])
		if !ok || len(g.DataPoints) != 1 || g.DataPoints[0].Value != 1_700_000_000 || g.DataPoints[0].Attributes.Len() != 0 {
			t.Fatalf("collection %d: %#v", i, mt.Data)
		}
		if mt.Unit != "s" {
			t.Errorf("unit = %q, want s", mt.Unit)
		}
	}
}

func TestSelfMetricsCountDropsAndFailures(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	s, err := newSelfMetrics(mp.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s.SpanAttributeDropped(ctx, "canary.key")
	s.SpanAttributeDropped(ctx, "canary.key")
	s.ExportFailed(ctx, semconv.SignalLogs)
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			sum, ok := mt.Data.(metricdata.Sum[int64])
			if !ok || !sum.IsMonotonic {
				continue
			}
			for _, dp := range sum.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					counts[mt.Name+"/"+string(kv.Key)+"="+kv.Value.String()] += dp.Value
				}
			}
		}
	}
	if counts[semconv.MetricTelemetrySpanAttributesDropped+"/key=canary.key"] != 2 {
		t.Errorf("dropped counts = %v", counts)
	}
	if counts[semconv.MetricTelemetryExportFailures+"/signal=logs"] != 1 {
		t.Errorf("failure counts = %v", counts)
	}
}

type failingSpans struct{ err error }

func (f failingSpans) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return f.err }
func (failingSpans) Shutdown(context.Context) error                               { return nil }

type failingMetrics struct {
	metric.Exporter
	err error
}

func (f failingMetrics) Export(context.Context, *metricdata.ResourceMetrics) error { return f.err }

type failingLogs struct {
	otellog.Exporter
	err error
}

func (f failingLogs) Export(context.Context, []otellog.Record) error { return f.err }

// The error handler cannot tell which exporter failed, so each exporter tags its own error and
// counts it; the handler then writes one fixed line and never the error text, which can carry an
// address.
func TestExportFailuresAreTaggedCountedAndReportedWithFixedText(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	s, err := newSelfMetrics(mp.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("Post https://collector.example.test/v1/x?token=SECRET-MARKER: dial failed")
	ctx := context.Background()
	errs := map[string]error{
		semconv.SignalTraces:  traceFailureCounter(failingSpans{boom}, s).ExportSpans(ctx, nil),
		semconv.SignalMetrics: metricFailureCounter(failingMetrics{err: boom}, s).Export(ctx, nil),
		semconv.SignalLogs:    logFailureCounter(failingLogs{err: boom}, s).Export(ctx, nil),
	}
	var out bytes.Buffer
	handler := newErrorHandler(&out)
	for signal, err := range errs {
		var se *signalError
		if !errors.As(err, &se) || se.signal != signal || !errors.Is(err, boom) {
			t.Errorf("%s: export error = %v, want it tagged with the signal and wrapping the cause", signal, err)
			continue
		}
		handler.Handle(err)
	}
	handler.Handle(errors.New("SECRET-MARKER from somewhere else"))
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("handler wrote %d lines, want 4: %q", len(lines), out.String())
	}
	if strings.Contains(out.String(), "SECRET-MARKER") || strings.Contains(out.String(), "example.test") {
		t.Errorf("the handler wrote error text: %q", out.String())
	}
	for _, signal := range []string{"traces", "metrics", "logs"} {
		if !strings.Contains(out.String(), "finstore: telemetry export failed ("+signal+")") {
			t.Errorf("no line for %s in %q", signal, out.String())
		}
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	total := int64(0)
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if sum, ok := mt.Data.(metricdata.Sum[int64]); ok && mt.Name == semconv.MetricTelemetryExportFailures {
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
			}
		}
	}
	if total != 3 {
		t.Errorf("failures counted = %d, want 3", total)
	}
	if err := traceFailureCounter(failingSpans{nil}, s).ExportSpans(ctx, nil); err != nil {
		t.Errorf("a successful export returned %v", err)
	}
}

// End to end: a collector that refuses traces shows up as one fixed stderr line and a failures
// counter that the metrics export carries.
func TestARefusedTraceExportIsCountedAndReported(t *testing.T) {
	sink := telemetrytest.New(t)
	sink.FailPath("/v1/traces", http.StatusBadRequest)
	var out lockedBuffer
	prev := errorOut
	errorOut = &out
	t.Cleanup(func() { errorOut = prev })
	ctx := context.Background()
	p, err := Setup(ctx, config.Config{OTLPEndpoint: sink.URL()}, serveID)
	if err != nil {
		t.Fatal(err)
	}
	_, span := p.Tracer("t").Start(ctx, "work")
	span.End()
	_ = p.Shutdown(ctx)

	if !strings.Contains(out.String(), "finstore: telemetry export failed (traces)") || strings.Contains(out.String(), sink.URL()) {
		t.Errorf("stderr line = %q", out.String())
	}
	pts := sink.Metrics()[semconv.MetricTelemetryExportFailures].Points
	if len(pts) == 0 || pts[len(pts)-1].Attrs[semconv.AttrSignal] != "traces" || pts[len(pts)-1].Value < 1 {
		t.Errorf("failure counter points = %#v", pts)
	}
}

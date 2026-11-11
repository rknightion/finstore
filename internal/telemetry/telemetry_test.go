package telemetry

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/semconv"
)

func collect(t *testing.T) (*Metrics, func() map[string]metricdata.Metrics) {
	t.Helper()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	m, err := NewMetrics(mp.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	return m, func() map[string]metricdata.Metrics {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatal(err)
		}
		out := map[string]metricdata.Metrics{}
		for _, sm := range rm.ScopeMetrics {
			for _, mt := range sm.Metrics {
				out[mt.Name] = mt
			}
		}
		return out
	}
}

func TestMetricsRecordContractNamesAndAttributes(t *testing.T) {
	m, read := collect(t)
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0)
	m.SyncSucceeded(ctx, "monzo", at)
	m.SyncRun(ctx, "monzo", "scheduled", "ok")
	m.SyncRun(ctx, "monzo", "scheduled", "ok")
	m.BankRequest(ctx, "monzo", "transactions", "2xx")
	m.ConsentState(ctx, "monzo", "active", []string{"active", "lost"})
	m.ConsentExpiry(ctx, "monzo", at)
	m.ReconciliationMismatch(ctx, "current", true)
	m.ImportLast(ctx, "current", at)
	m.DumpSucceeded(ctx, at)
	m.Up(ctx)
	got := read()

	gauge := func(name string) []metricdata.DataPoint[int64] {
		t.Helper()
		g, ok := got[name].Data.(metricdata.Gauge[int64])
		if !ok {
			t.Fatalf("%s missing or not an int gauge: %#v", name, got[name].Data)
		}
		return g.DataPoints
	}
	value := func(name string, attrs map[string]string) int64 {
		t.Helper()
		for _, dp := range gauge(name) {
			match := dp.Attributes.Len() == len(attrs)
			for k, v := range attrs {
				if got, _ := dp.Attributes.Value(attrKey(k)); got.AsString() != v {
					match = false
				}
			}
			if match {
				return dp.Value
			}
		}
		t.Fatalf("%s has no point with attributes %v", name, attrs)
		return 0
	}
	const ts = 1_700_000_000
	if v := value(semconv.MetricSyncLastSuccessTime, map[string]string{"source": "monzo"}); v != ts {
		t.Errorf("sync last success = %d", v)
	}
	if v := value(semconv.MetricConsentState, map[string]string{"source": "monzo", "state": "active"}); v != 1 {
		t.Errorf("active consent = %d", v)
	}
	if v := value(semconv.MetricConsentState, map[string]string{"source": "monzo", "state": "lost"}); v != 0 {
		t.Errorf("lost consent = %d", v)
	}
	if v := value(semconv.MetricConsentExpiryTime, map[string]string{"source": "monzo"}); v != ts {
		t.Errorf("consent expiry = %d", v)
	}
	if v := value(semconv.MetricReconciliationMismatch, map[string]string{"account": "current"}); v != 1 {
		t.Errorf("mismatch = %d", v)
	}
	if v := value(semconv.MetricImportLastTime, map[string]string{"account": "current"}); v != ts {
		t.Errorf("import last = %d", v)
	}
	if v := value(semconv.MetricDumpLastSuccessTime, nil); v != ts {
		t.Errorf("dump last success = %d", v)
	}
	if v := value(semconv.MetricUp, nil); v != 1 {
		t.Errorf("up = %d", v)
	}

	sum := func(name string) metricdata.DataPoint[int64] {
		t.Helper()
		s, ok := got[name].Data.(metricdata.Sum[int64])
		if !ok || !s.IsMonotonic || len(s.DataPoints) != 1 {
			t.Fatalf("%s is not a single-series monotonic counter: %#v", name, got[name].Data)
		}
		return s.DataPoints[0]
	}
	if dp := sum(semconv.MetricSyncRuns); dp.Value != 2 || dp.Attributes.Len() != 3 {
		t.Errorf("sync runs = %d with %d attributes", dp.Value, dp.Attributes.Len())
	}
	if dp := sum(semconv.MetricBankRequests); dp.Value != 1 || dp.Attributes.Len() != 3 {
		t.Errorf("bank requests = %d with %d attributes", dp.Value, dp.Attributes.Len())
	}
}

func TestSetupWithoutEndpointIsNoOp(t *testing.T) {
	p, err := Setup(context.Background(), config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewMetrics(p.Meter)
	if err != nil {
		t.Fatal(err)
	}
	m.Up(context.Background())
	var buf bytes.Buffer
	NewLogger(&buf, slog.LevelInfo, p.LogHandler).Info("hello", "source", "monzo")
	if !strings.Contains(buf.String(), "hello") {
		t.Errorf("stdout sink lost the record: %s", buf.String())
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func TestSetupRejectsBadEndpoints(t *testing.T) {
	const token = "SECRET-MARKER-tok"
	tests := []struct {
		name string
		cfg  config.Config
	}{
		{"credentials over http", config.Config{OTLPEndpoint: "http://collector.example.test", OTLPInstanceID: "1", OTLPToken: token}},
		{"not a url", config.Config{OTLPEndpoint: "collector"}},
		{"no host", config.Config{OTLPEndpoint: "https:///otlp"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Setup(context.Background(), tt.cfg)
			if err == nil {
				t.Fatal("want error")
			}
			if strings.Contains(err.Error(), token) {
				t.Errorf("error leaks the token: %v", err)
			}
		})
	}
}

func TestSetupExportsFilteredLogsAndMetrics(t *testing.T) {
	var mu sync.Mutex
	bodies := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies[r.URL.Path] = append(bodies[r.URL.Path], b...)
		mu.Unlock()
	}))
	defer srv.Close()

	ctx := context.Background()
	p, err := Setup(ctx, config.Config{OTLPEndpoint: srv.URL + "/otlp/", Environment: "test", LogLevel: slog.LevelInfo})
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewMetrics(p.Meter)
	if err != nil {
		t.Fatal(err)
	}
	m.Up(ctx)
	var stdout bytes.Buffer
	NewLogger(&stdout, slog.LevelInfo, p.LogHandler).Info("exported-line", "source", "monzo", "token", "SECRET-MARKER-otlp")
	if err := p.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	logs, metrics := string(bodies["/otlp/v1/logs"]), string(bodies["/otlp/v1/metrics"])
	for _, want := range []string{"exported-line", "monzo", "service.name", "finstore", "deployment.environment.name", "test"} {
		if !strings.Contains(logs, want) {
			t.Errorf("OTLP logs lack %q", want)
		}
	}
	if strings.Contains(logs, "SECRET-MARKER") || strings.Contains(stdout.String(), "SECRET-MARKER") {
		t.Error("a disallowed attribute reached a sink")
	}
	if !strings.Contains(metrics, semconv.MetricUp) {
		t.Errorf("OTLP metrics lack %s", semconv.MetricUp)
	}
}

func attrKey(k string) attribute.Key { return attribute.Key(k) }

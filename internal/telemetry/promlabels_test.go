package telemetry

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rknightion/finstore/internal/semconv"
)

// Grafana Cloud's OTLP gateway turns the service resource into the Prometheus labels job (from
// service.name) and instance (from service.instance.id), and the histogram and summary types own le
// and quantile. A metric attribute that converts to one of these overwrites or collides with it, so
// every series of the metric reads the same value. Names that start with two underscores are
// reserved by Prometheus itself.
var promReserved = []string{"job", "instance", "le", "quantile", "exported_job", "exported_instance"}

var nonLabelChars = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// promLabel is the label name an OTLP attribute key becomes: every character outside
// [a-zA-Z0-9_] is replaced by an underscore.
func promLabel(key string) string { return nonLabelChars.ReplaceAllString(key, "_") }

func reservedLabel(label string) bool {
	return slices.Contains(promReserved, label) || strings.HasPrefix(label, "__")
}

// The scheduler metrics name the job with finstore.job, which Prometheus reads as finstore_job:
// the plain key job is overwritten by the service's own job label and made every series one. Seven
// jobs are seven series on each of the three metrics, and none carries the key job.
func TestSchedulerMetricsCarryFinstoreJobAndSevenJobsGiveSevenSeries(t *testing.T) {
	obs, _, read := schedulerFixture(t)
	jobs := []string{"status", "monzo 06:00", "monzo 18:00", "truelayer:ob-lloyds 06:20", "truelayer:ob-lloyds 18:20",
		"truelayer:ob-monzo 06:20", "truelayer:ob-monzo 18:20"}
	for _, job := range jobs {
		_, finish := obs.Start(context.Background(), job)
		finish(semconv.JobOK)
	}
	got := read()

	if semconv.AttrJob != "finstore.job" {
		t.Errorf("AttrJob = %q, want finstore.job", semconv.AttrJob)
	}
	for _, name := range []string{semconv.MetricSchedulerJobRuns, semconv.MetricSchedulerJobDuration, semconv.MetricSchedulerJobLastOK} {
		m, ok := got[name]
		if !ok {
			t.Fatalf("no %s", name)
		}
		var sets [][]string
		switch d := m.Data.(type) {
		case metricdata.Sum[int64]:
			for _, dp := range d.DataPoints {
				sets = append(sets, keysOf(dp.Attributes.ToSlice()))
				if dp.Attributes.HasValue("job") {
					t.Errorf("%s carries the attribute job", name)
				}
			}
		case metricdata.Histogram[float64]:
			for _, dp := range d.DataPoints {
				sets = append(sets, keysOf(dp.Attributes.ToSlice()))
				if dp.Attributes.HasValue("job") {
					t.Errorf("%s carries the attribute job", name)
				}
			}
		case metricdata.Gauge[int64]:
			for _, dp := range d.DataPoints {
				sets = append(sets, keysOf(dp.Attributes.ToSlice()))
				if dp.Attributes.HasValue("job") {
					t.Errorf("%s carries the attribute job", name)
				}
			}
		default:
			t.Fatalf("%s is %T", name, m.Data)
		}
		if len(sets) != len(jobs) {
			t.Errorf("%s has %d series, want %d (one per job)", name, len(sets), len(jobs))
		}
		for _, keys := range sets {
			if !slices.Contains(keys, "finstore.job") {
				t.Errorf("%s has a series without finstore.job: %v", name, keys)
			}
		}
	}
	runs := got[semconv.MetricSchedulerJobRuns]
	for _, job := range jobs {
		if n, found := sumPoint(t, runs, map[string]string{"finstore.job": job, "outcome": "ok"}); !found || n != 1 {
			t.Errorf("runs{finstore.job=%q, outcome=ok} = %d (found %v), want 1", job, n, found)
		}
	}
	// What a Prometheus query sees: the label finstore_job.
	if promLabel(semconv.AttrJob) != "finstore_job" {
		t.Errorf("finstore.job becomes the label %q, want finstore_job", promLabel(semconv.AttrJob))
	}
}

func keysOf(kvs []attribute.KeyValue) []string {
	out := make([]string, 0, len(kvs))
	for _, kv := range kvs {
		out = append(out, string(kv.Key))
	}
	return out
}

// Every attribute key finstore declares for a metric, after Prometheus' name conversion, is clear
// of the labels the OTLP gateway sets or reserves.
func TestMetricAttributeKeysDoNotCollideWithPrometheusLabels(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "../semconv/semconv.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Every string constant of the file, so an Attr constant that aliases another (AttrJob is
	// SpanJob) resolves to its key.
	consts, aliases := map[string]string{}, map[string]string{}
	var attrs []string
	ast.Inspect(f, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			if i >= len(spec.Values) {
				continue
			}
			switch v := spec.Values[i].(type) {
			case *ast.BasicLit:
				key, err := strconv.Unquote(v.Value)
				if err != nil {
					continue
				}
				consts[name.Name] = key
			case *ast.Ident:
				aliases[name.Name] = v.Name
			}
			// The Attr constants are the metric attribute keys and the resource keys (AttrService...,
			// AttrDeployment...); a resource key set on every series must be clear of them too.
			if strings.HasPrefix(name.Name, "Attr") {
				attrs = append(attrs, name.Name)
			}
		}
		return true
	})
	for name, target := range aliases {
		if key, ok := consts[target]; ok {
			consts[name] = key
		}
	}
	n := 0
	for _, name := range attrs {
		key, ok := consts[name]
		if !ok {
			t.Errorf("%s: could not read its key from semconv.go", name)
			continue
		}
		n++
		if label := promLabel(key); reservedLabel(label) {
			t.Errorf("%s = %q becomes the Prometheus label %q, which the OTLP conversion sets or reserves", name, key, label)
		}
	}
	if n < 15 {
		t.Fatalf("read only %d Attr constants from semconv.go; the scan is broken", n)
	}
}

// Every attribute finstore actually records on a metric, from every instrument of Metrics and the
// telemetry self-metrics, has a key that converts to a label clear of job, instance and the other
// reserved names, and no two keys of one series convert to the same label.
func TestNoRecordedMetricAttributeCollidesWithPrometheusLabels(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithView(histogramViews()...))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	meter := mp.Meter("test")
	m, err := NewMetrics(meter)
	if err != nil {
		t.Fatal(err)
	}
	self, err := newSelfMetrics(meter)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	m.SyncSucceeded(ctx, "monzo", now)
	m.SyncRun(ctx, "monzo", "sync", "ok")
	m.SyncRunWithCategory(ctx, "monzo", "sync", "partial", "rate_limited")
	m.LastRun(ctx, "monzo", "sync", true, now)
	m.LastRateLimited(ctx, "monzo", now)
	m.ConsentHistory(ctx, "monzo", true)
	m.ConnectsUnanswered(ctx, "monzo", 1)
	m.MandatesUnresolved(ctx, "monzo", 1)
	m.OptionalRead(ctx, "truelayer:ob-lloyds", "/data/v1/info", "ok")
	m.CardsUnlisted(ctx, "truelayer:ob-lloyds", 1)
	m.BankRequest(ctx, "monzo", "/accounts", "2xx")
	m.ConsentState(ctx, "monzo", "active", []string{"active", "lost"})
	m.ConsentExpiry(ctx, "monzo", now)
	m.ReconciliationMismatch(ctx, "monzo-current", false)
	m.ImportLast(ctx, "lloyds-joint", now)
	m.DumpSucceeded(ctx, now)
	m.Up(ctx)
	m.ProcessStarted(ctx, now)
	if err := m.ObserveWriterLock(func() (string, time.Time, bool) { return "test", now, true }); err != nil {
		t.Fatal(err)
	}
	obs := m.BankObserver("monzo")
	obs.Attempt(ctx, "/accounts", "2xx", time.Second)
	obs.Retry(ctx, "/accounts", "timeout")
	sched := m.SchedulerObserver(nil)
	_, finish := sched.Start(ctx, "status")
	finish(semconv.JobOK)
	runCtx, run := m.StartRun(ctx, nil, "monzo", "sync", 1)
	run.Finish(runCtx, "ok", "", 1, 1)
	self.SpanAttributeDropped(ctx, "some.key")
	self.ExportFailed(ctx, semconv.SignalTraces)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	check := func(name string, kvs []attribute.KeyValue) {
		labels := map[string]string{}
		for _, kv := range kvs {
			key := string(kv.Key)
			label := promLabel(key)
			if reservedLabel(label) {
				t.Errorf("%s records the attribute %q, which becomes the reserved Prometheus label %q", name, key, label)
			}
			if prev, dup := labels[label]; dup {
				t.Errorf("%s: attributes %q and %q both become the label %q", name, prev, key, label)
			}
			labels[label] = key
		}
	}
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			seen[mt.Name] = true
			switch d := mt.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					check(mt.Name, dp.Attributes.ToSlice())
				}
			case metricdata.Gauge[int64]:
				for _, dp := range d.DataPoints {
					check(mt.Name, dp.Attributes.ToSlice())
				}
			case metricdata.Gauge[float64]:
				for _, dp := range d.DataPoints {
					check(mt.Name, dp.Attributes.ToSlice())
				}
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					check(mt.Name, dp.Attributes.ToSlice())
				}
			default:
				t.Errorf("%s is %T, which this test does not read", mt.Name, mt.Data)
			}
		}
	}
	// Every instrument that takes an attribute was recorded, so the check above covered it.
	want := []string{
		semconv.MetricSyncLastSuccessTime, semconv.MetricSyncRuns, semconv.MetricSyncRunDuration, semconv.MetricBankRequests,
		semconv.MetricConsentState, semconv.MetricConsentExpiryTime, semconv.MetricReconciliationMismatch,
		semconv.MetricImportLastTime, semconv.MetricMandatesUnresolved, semconv.MetricTrueLayerOptionalReads,
		semconv.MetricTrueLayerCardsUnlisted, semconv.MetricBankRequestDuration, semconv.MetricBankRetries,
		semconv.MetricSchedulerJobRuns, semconv.MetricSchedulerJobDuration, semconv.MetricSchedulerJobLastOK,
		semconv.MetricWriterLockHeld, semconv.MetricTelemetryExportFailures, semconv.MetricTelemetrySpanAttributesDropped,
		semconv.MetricSyncLastRunOK, semconv.MetricSyncLastRunTime, semconv.MetricSyncLastRateLimitedTime,
		semconv.MetricConsentHistoryOK, semconv.MetricConnectUnanswered,
	}
	for _, name := range want {
		if !seen[name] {
			t.Errorf("%s was not recorded by the test, so its attributes were not checked (have %s)", name, fmt.Sprint(len(seen)))
		}
	}
}

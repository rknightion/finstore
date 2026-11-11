package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/rknightion/finstore/internal/scheduler"
	"github.com/rknightion/finstore/internal/semconv"
)

// schedulerFixture is a Metrics and tracer on in-memory pipelines, the spans passing the real
// exporter filter.
func schedulerFixture(t *testing.T) (scheduler.Observer, *tracetest.InMemoryExporter, func() map[string]metricdata.Metrics) {
	t.Helper()
	p, mem, reader := httpFixture(t)
	m, err := NewMetrics(p.Meter)
	if err != nil {
		t.Fatal(err)
	}
	read := func() map[string]metricdata.Metrics {
		t.Helper()
		return collectFrom(t, reader)
	}
	return m.SchedulerObserver(p.Tracer("test")), mem, read
}

func collectFrom(t *testing.T, reader sdkmetric.Reader) map[string]metricdata.Metrics {
	t.Helper()
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

// sumPoint returns the value of the counter point whose attributes include every pair in want.
func sumPoint(t *testing.T, m metricdata.Metrics, want map[string]string) (int64, bool) {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("%s is %T, want an int64 sum", m.Name, m.Data)
	}
	for _, dp := range sum.DataPoints {
		if hasAttrs(dp.Attributes.ToSlice(), want) {
			return dp.Value, true
		}
	}
	return 0, false
}

// hasAttrs reports whether kvs holds every key and string value in want.
func hasAttrs(kvs []attribute.KeyValue, want map[string]string) bool {
	have := map[string]string{}
	for _, kv := range kvs {
		have[string(kv.Key)] = kv.Value.String()
	}
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func gaugePoint(t *testing.T, m metricdata.Metrics, want map[string]string) (int64, bool) {
	t.Helper()
	g, ok := m.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("%s is %T, want an int64 gauge", m.Name, m.Data)
	}
	for _, dp := range g.DataPoints {
		if hasAttrs(dp.Attributes.ToSlice(), want) {
			return dp.Value, true
		}
	}
	return 0, false
}

func TestSchedulerObserverMakesANewRootSpanPerRun(t *testing.T) {
	obs, mem, _ := schedulerFixture(t)
	// A context that already carries a span: the job span must still be a new root.
	ambientTP := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = ambientTP.Shutdown(context.Background()) })
	ambient, parent := ambientTP.Tracer("ambient").Start(context.Background(), "ambient")
	defer parent.End()

	for _, job := range []string{"status", "truelayer:ob-lloyds 06:20"} {
		ctx, finish := obs.Start(ambient, job)
		if !trace.SpanContextFromContext(ctx).IsValid() {
			t.Fatal("the job's context carries no span")
		}
		finish(semconv.JobOK)
	}
	spans := mem.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("%d spans, want 2", len(spans))
	}
	if spans[0].SpanContext.TraceID() == spans[1].SpanContext.TraceID() {
		t.Error("two runs share a trace")
	}
	for i, job := range []string{"status", "truelayer:ob-lloyds 06:20"} {
		s := spans[i]
		if s.Name != semconv.SpanSchedulerJob {
			t.Errorf("name = %q", s.Name)
		}
		if s.Parent.IsValid() || s.SpanContext.TraceID() == parent.SpanContext().TraceID() {
			t.Errorf("run %d is not a new root: parent %v", i, s.Parent)
		}
		a := attrMap(s.Attributes)
		if a[semconv.SpanJob] != job || a[semconv.SpanJobOutcome] != "ok" {
			t.Errorf("attributes = %v", a)
		}
		if s.Status.Code == codes.Error {
			t.Errorf("an ok run is marked failed")
		}
	}
}

func TestSchedulerObserverRecordsRunsDurationAndLastOK(t *testing.T) {
	obs, mem, read := schedulerFixture(t)
	for _, outcome := range []string{semconv.JobOK, semconv.JobOK, semconv.JobError} {
		_, finish := obs.Start(context.Background(), "hourly")
		finish(outcome)
	}
	got := read()

	runs, ok := got[semconv.MetricSchedulerJobRuns]
	if !ok {
		t.Fatalf("no %s; have %v", semconv.MetricSchedulerJobRuns, got)
	}
	if n, _ := sumPoint(t, runs, map[string]string{"job": "hourly", "outcome": "ok"}); n != 2 {
		t.Errorf("ok runs = %d, want 2", n)
	}
	if n, _ := sumPoint(t, runs, map[string]string{"job": "hourly", "outcome": "error"}); n != 1 {
		t.Errorf("error runs = %d, want 1", n)
	}

	dur, ok := got[semconv.MetricSchedulerJobDuration]
	if !ok {
		t.Fatalf("no %s", semconv.MetricSchedulerJobDuration)
	}
	h, isHist := dur.Data.(metricdata.Histogram[float64])
	if !isHist || len(h.DataPoints) != 1 || h.DataPoints[0].Count != 3 {
		t.Fatalf("duration = %#v, want one histogram point of 3 runs", dur.Data)
	}
	if dur.Unit != "s" {
		t.Errorf("duration unit = %q, want s", dur.Unit)
	}
	if b := h.DataPoints[0].Bounds; len(b) != 12 || b[0] != 1 || b[11] != 3600 {
		t.Errorf("buckets = %v, want the run buckets 1 to 3600", b)
	}
	if len(h.DataPoints[0].Attributes.ToSlice()) != 1 {
		t.Errorf("duration attributes = %v, want job only", h.DataPoints[0].Attributes.ToSlice())
	}

	last, ok := got[semconv.MetricSchedulerJobLastOK]
	if !ok {
		t.Fatalf("no %s", semconv.MetricSchedulerJobLastOK)
	}
	if v, found := gaugePoint(t, last, map[string]string{"job": "hourly"}); !found || v != 0 {
		t.Errorf("last_ok after an error = %d (found %v), want 0", v, found)
	}
	// The last run decides: an ok run after the error sets it back to 1.
	_, finish := obs.Start(context.Background(), "hourly")
	finish(semconv.JobOK)
	if v, _ := gaugePoint(t, read()[semconv.MetricSchedulerJobLastOK], map[string]string{"job": "hourly"}); v != 1 {
		t.Errorf("last_ok after an ok run = %d, want 1", v)
	}

	// A failed run marks its span failed with a category word, and says so in its outcome.
	var errSpan tracetest.SpanStub
	for _, s := range mem.GetSpans() {
		if attrMap(s.Attributes)[semconv.SpanJobOutcome] == "error" {
			errSpan = s
		}
	}
	if errSpan.Status.Code != codes.Error || errSpan.Status.Description != "error" {
		t.Errorf("failed run status = %v %q", errSpan.Status.Code, errSpan.Status.Description)
	}
}

// AC4: a job that panics is recovered by the scheduler and reports outcome panic.
func TestAPanickingJobYieldsOutcomePanicOneRunAndLastOKZero(t *testing.T) {
	p, mem, reader := httpFixture(t)
	m, err := NewMetrics(p.Meter)
	if err != nil {
		t.Fatal(err)
	}
	s := scheduler.New(time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)), m.SchedulerObserver(p.Tracer("test")))
	var ran atomic.Int32
	s.Every("boom", time.Hour, 0, func(context.Context) error {
		ran.Add(1)
		panic("job exploded with a value that must not leave")
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for len(mem.GetSpans()) == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the job span was never exported")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	spans := mem.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("%d spans, want 1", len(spans))
	}
	a := attrMap(spans[0].Attributes)
	if a[semconv.SpanJob] != "boom" || a[semconv.SpanJobOutcome] != "panic" {
		t.Errorf("attributes = %v, want job boom with outcome panic", a)
	}
	if spans[0].Status.Code != codes.Error {
		t.Errorf("a panicked run is not marked failed")
	}
	got := collectFrom(t, reader)
	if n, _ := sumPoint(t, got[semconv.MetricSchedulerJobRuns], map[string]string{"job": "boom", "outcome": "panic"}); n != 1 {
		t.Errorf("panic runs = %d, want 1", n)
	}
	if v, found := gaugePoint(t, got[semconv.MetricSchedulerJobLastOK], map[string]string{"job": "boom"}); !found || v != 0 {
		t.Errorf("last_ok = %d (found %v), want 0", v, found)
	}
	for _, s := range mem.GetSpans() {
		if strings.Contains(s.Status.Description, "exploded") {
			t.Errorf("the panic value reached the span status: %q", s.Status.Description)
		}
	}
	if n := ran.Load(); n != 1 {
		t.Errorf("the job ran %d times, want 1", n)
	}
}

// A job that returns an error reports outcome error; the error text is never recorded.
func TestAFailingJobYieldsOutcomeErrorWithoutItsText(t *testing.T) {
	p, mem, _ := httpFixture(t)
	m, err := NewMetrics(p.Meter)
	if err != nil {
		t.Fatal(err)
	}
	s := scheduler.New(time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)), m.SchedulerObserver(p.Tracer("test")))
	s.Every("fails", time.Hour, 0, func(context.Context) error { return errors.New("failure quoting CANARYERR") })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for len(mem.GetSpans()) == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the job span was never exported")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	span := mem.GetSpans()[0]
	if attrMap(span.Attributes)[semconv.SpanJobOutcome] != "error" || strings.Contains(span.Status.Description, "CANARYERR") {
		t.Errorf("span = %v %+v", attrMap(span.Attributes), span.Status)
	}
}

// A run that ends because serve is shutting down is not a failure: it is counted as cancelled,
// leaves last_ok as the run before it set it, and does not mark its span failed. Otherwise every
// deploy that interrupted a running job would trip the Scheduled Job Failed alert.
func TestACancelledRunLeavesLastOKAloneAndIsNoFailure(t *testing.T) {
	obs, mem, read := schedulerFixture(t)
	_, finish := obs.Start(context.Background(), "hourly")
	finish(semconv.JobOK)
	_, finish = obs.Start(context.Background(), "hourly")
	finish(semconv.JobCancelled)

	got := read()
	if v, found := gaugePoint(t, got[semconv.MetricSchedulerJobLastOK], map[string]string{"job": "hourly"}); !found || v != 1 {
		t.Errorf("last_ok after a cancelled run = %d (found %v), want the earlier 1", v, found)
	}
	runs := got[semconv.MetricSchedulerJobRuns]
	if n, _ := sumPoint(t, runs, map[string]string{"job": "hourly", "outcome": "cancelled"}); n != 1 {
		t.Errorf("cancelled runs = %d, want 1", n)
	}
	if n, found := sumPoint(t, runs, map[string]string{"job": "hourly", "outcome": "error"}); found || n != 0 {
		t.Errorf("a cancelled run counted as an error run: %d", n)
	}
	var cancelled tracetest.SpanStub
	for _, s := range mem.GetSpans() {
		if attrMap(s.Attributes)[semconv.SpanJobOutcome] == "cancelled" {
			cancelled = s
		}
	}
	if cancelled.Name == "" || cancelled.Status.Code == codes.Error {
		t.Errorf("cancelled span = %+v, want one that is not marked failed", cancelled)
	}
}

// The same through the real scheduler: a job that returns its context's error at shutdown.
func TestAJobCancelledByShutdownReportsCancelledNotError(t *testing.T) {
	p, mem, reader := httpFixture(t)
	m, err := NewMetrics(p.Meter)
	if err != nil {
		t.Fatal(err)
	}
	s := scheduler.New(time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)), m.SchedulerObserver(p.Tracer("test")))
	started := make(chan struct{})
	s.Every("long", time.Hour, 0, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("the job never started")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	spans := mem.GetSpans()
	if len(spans) != 1 || attrMap(spans[0].Attributes)[semconv.SpanJobOutcome] != "cancelled" || spans[0].Status.Code == codes.Error {
		t.Fatalf("spans = %+v, want one cancelled span that is not failed", spans)
	}
	got := collectFrom(t, reader)
	if n, _ := sumPoint(t, got[semconv.MetricSchedulerJobRuns], map[string]string{"job": "long", "outcome": "cancelled"}); n != 1 {
		t.Errorf("cancelled runs = %d, want 1", n)
	}
	if n, found := sumPoint(t, got[semconv.MetricSchedulerJobRuns], map[string]string{"job": "long", "outcome": "error"}); found || n != 0 {
		t.Errorf("shutdown counted an error run: %d", n)
	}
	if lastOK, ok := got[semconv.MetricSchedulerJobLastOK]; ok {
		if v, found := gaugePoint(t, lastOK, map[string]string{"job": "long"}); found {
			t.Errorf("last_ok = %d for a job that only ever ran into shutdown, want no point", v)
		}
	}
}

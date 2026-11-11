package monzo

import (
	"context"
	"strconv"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/rknightion/finstore/internal/monzotest"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
)

// spanRig is an in-memory span recorder whose tracer is handed to the worker and to the store.
type spanRig struct {
	mem *tracetest.InMemoryExporter
	tp  *sdktrace.TracerProvider
}

func newSpanRig(t *testing.T) *spanRig {
	t.Helper()
	mem := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(mem))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return &spanRig{mem: mem, tp: tp}
}

// harness is a harness whose worker and store both trace into the rig.
func (r *spanRig) harness(t *testing.T, mod func(*Options)) *harness {
	t.Helper()
	return newHarnessWith(t, nil, func(o *Options) {
		o.Tracer = r.tp.Tracer("test")
		if mod != nil {
			mod(o)
		}
	}, store.WithTelemetry(r.tp, metricnoop.NewMeterProvider()))
}

func (r *spanRig) named(name string) []tracetest.SpanStub {
	var out []tracetest.SpanStub
	for _, s := range r.mem.GetSpans() {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

func spanAttrs(s tracetest.SpanStub) map[string]string {
	out := map[string]string{}
	for _, kv := range s.Attributes {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

// runs returns the sync.run spans of kind.
func (r *spanRig) runs(kind string) []tracetest.SpanStub {
	var out []tracetest.SpanStub
	for _, s := range r.named(semconv.SpanSyncRun) {
		if spanAttrs(s)[semconv.SpanRunKind] == kind {
			out = append(out, s)
		}
	}
	return out
}

func isRoot(s tracetest.SpanStub) bool { return !s.Parent.IsValid() }

// openAccount adds an account created daysAgo days ago with a few transactions.
func openAccount(h *harness, id string, daysAgo int) {
	now := h.fake.Now()
	h.fake.AddAccount(monzotest.Account{ID: id, Type: "uk_retail", Created: now.AddDate(0, 0, -daysAgo)})
	h.fake.GenerateTransactions(id, 30, now.AddDate(0, 0, -daysAgo+1), now.Add(-time.Hour), 1)
}

// A scheduled run is one sync.run root with the run's bank calls and queries under it, carrying
// the source, kind, run id, outcome, pages and rows, and its duration is measured.
func TestScheduledRunIsOneSyncRunRootWithBankAndQueryChildren(t *testing.T) {
	t.Parallel()
	rig := newSpanRig(t)
	h := rig.harness(t, nil)
	openAccount(h, "acc_test_1", 90)
	h.connect(nil)
	h.fake.Advance(time.Hour)
	rig.mem.Reset()

	if err := h.w.RunScheduled(context.Background(), KindHourly); err != nil {
		t.Fatalf("RunScheduled: %v", err)
	}
	runs := rig.runs(KindHourly)
	if len(runs) != 1 {
		t.Fatalf("%d sync.run spans for the hourly run, want 1", len(runs))
	}
	run := runs[0]
	if !isRoot(run) {
		t.Errorf("sync.run has parent %v, want a root", run.Parent)
	}
	row := h.lastRun(KindHourly)
	attrs := spanAttrs(run)
	for k, want := range map[string]string{
		semconv.SpanSource:     Source,
		semconv.SpanRunKind:    KindHourly,
		semconv.SpanRunID:      strconv.FormatInt(row.ID, 10),
		semconv.SpanRunOutcome: outcomeOK,
		semconv.SpanRunRows:    strconv.Itoa(row.RowsSeen),
	} {
		if attrs[k] != want {
			t.Errorf("sync.run %s = %q, want %q (all %v)", k, attrs[k], want, attrs)
		}
	}
	if p, err := strconv.Atoi(attrs[semconv.SpanRunPages]); err != nil || p < 1 {
		t.Errorf("sync.run pages = %q, want at least 1", attrs[semconv.SpanRunPages])
	}
	if run.Status.Code == codes.Error {
		t.Errorf("an ok run has status %v", run.Status)
	}

	var bank, query int
	for _, s := range rig.mem.GetSpans() {
		if s.SpanContext.TraceID() != run.SpanContext.TraceID() {
			continue
		}
		a := spanAttrs(s)
		switch {
		case s.Name == "bank GET /transactions" && s.Parent.SpanID() == run.SpanContext.SpanID():
			bank++
		case a[semconv.SpanDBSystemName] != "" && s.Parent.SpanID() == run.SpanContext.SpanID():
			query++
		}
	}
	if bank == 0 || query == 0 {
		t.Errorf("sync.run has %d bank-call children and %d query children, want both; spans %v", bank, query, names(rig.mem.GetSpans()))
	}
	if n := len(rig.named(semconv.SpanMonzoFetchRange)); n != 0 {
		t.Errorf("a scheduled run made %d monzo.fetch_range spans, want 0", n)
	}

	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	if !histogramHas(rm, semconv.MetricSyncRunDuration, map[string]string{"source": Source, "kind": KindHourly, "outcome": outcomeOK}) {
		t.Error("no finstore.sync.run.duration point for the hourly run")
	}
}

func names(spans tracetest.SpanStubs) []string {
	var out []string
	for _, s := range spans {
		out = append(out, s.Name)
	}
	return out
}

func histogramHas(rm metricdata.ResourceMetrics, name string, attrs map[string]string) bool {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok || m.Name != name {
				continue
			}
			for _, dp := range h.DataPoints {
				match := dp.Count > 0
				for k, v := range attrs {
					if got, _ := dp.Attributes.Value(attribute.Key(k)); got.AsString() != v {
						match = false
					}
				}
				if match {
					return true
				}
			}
		}
	}
	return false
}

// A backfill is one sync.run root and one monzo.fetch_range root for each range it fetches, each
// linked to the run; the range's bank calls are in the range's trace, not the run's.
func TestBackfillYieldsOneRootPerFetchedRange(t *testing.T) {
	t.Parallel()
	rig := newSpanRig(t)
	h := rig.harness(t, nil)
	openAccount(h, "acc_test_long", 300)
	h.connect(nil)

	wins := h.windows("acc_test_long")
	if len(wins) < 3 {
		t.Fatalf("the scenario has %d ranges, want several", len(wins))
	}
	runs := rig.runs(kindBackfill)
	if len(runs) != 1 {
		t.Fatalf("%d backfill sync.run spans, want 1", len(runs))
	}
	run := runs[0]
	ranges := rig.named(semconv.SpanMonzoFetchRange)
	if len(ranges) != len(wins) {
		t.Fatalf("%d monzo.fetch_range spans for %d ranges", len(ranges), len(wins))
	}
	byID := map[trace.SpanID]bool{}
	for _, s := range ranges {
		byID[s.SpanContext.SpanID()] = true
		if !isRoot(s) {
			t.Errorf("fetch_range has parent %v, want a root", s.Parent)
		}
		if s.SpanContext.TraceID() == run.SpanContext.TraceID() {
			t.Error("fetch_range shares the run's trace, want its own")
		}
		if len(s.Links) != 1 || s.Links[0].SpanContext.SpanID() != run.SpanContext.SpanID() ||
			s.Links[0].SpanContext.TraceID() != run.SpanContext.TraceID() {
			t.Errorf("fetch_range links %v, want the sync.run span", s.Links)
		}
		a := spanAttrs(s)
		if a[semconv.SpanSource] != Source {
			t.Errorf("fetch_range source %q", a[semconv.SpanSource])
		}
		for _, k := range []string{semconv.SpanRangeStart, semconv.SpanRangeEnd} {
			if _, err := time.Parse(time.DateOnly, a[k]); err != nil {
				t.Errorf("fetch_range %s = %q, want a date", k, a[k])
			}
		}
	}
	// Each range's transaction calls are under its own root; the run's trace has none.
	for _, s := range rig.named("bank GET /transactions") {
		if s.SpanContext.TraceID() == run.SpanContext.TraceID() {
			t.Errorf("a transactions call is in the run's trace")
		}
		if !byID[s.Parent.SpanID()] {
			t.Errorf("transactions call %v is not a child of a fetch_range root", s.Parent)
		}
	}
}

// A callback trace and the history-fetch trace are linked: the fetch that a callback starts is a
// new root whose link points at the callback span, and it does not join the callback's trace.
func TestHistoryFetchStartedByACallbackLinksToTheCallbackSpan(t *testing.T) {
	t.Parallel()
	rig := newSpanRig(t)
	h := rig.harness(t, nil)
	openAccount(h, "acc_test_1", 90)
	ctx, callback := rig.tp.Tracer("test").Start(context.Background(), semconv.SpanCallback)
	h.connectCtx(ctx, nil)
	callback.End()

	runs := rig.runs(kindBackfill)
	if len(runs) != 1 {
		t.Fatalf("%d backfill runs, want 1", len(runs))
	}
	run := runs[0]
	if !isRoot(run) {
		t.Errorf("history fetch has parent %v, want a new root", run.Parent)
	}
	cb := callback.SpanContext()
	if run.SpanContext.TraceID() == cb.TraceID() {
		t.Error("history fetch is in the callback's trace, want its own")
	}
	if len(run.Links) != 1 || run.Links[0].SpanContext.TraceID() != cb.TraceID() || run.Links[0].SpanContext.SpanID() != cb.SpanID() {
		t.Errorf("history fetch links %v, want the callback span %v", run.Links, cb)
	}
}

// Resume's background work links to the resume span: a restart that picks up the approval wait
// and the history fetch records one resume root, and the fetch it leads to links to it.
func TestResumeBackgroundWorkLinksToTheResumeSpan(t *testing.T) {
	t.Parallel()
	rig := newSpanRig(t)
	h := rig.harness(t, func(o *Options) { o.Concurrency = 1 })
	openAccount(h, "acc_test_1", 90)
	// The first worker stops in its first approval pause, as a process would on shutdown.
	h.setOnSleep(func(ctx context.Context, _ int, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	})
	h.callback(h.w)
	h.w.Close()
	h.setOnSleep(nil)
	h.fake.Advance(10 * time.Second)
	h.fake.Approve()
	rig.mem.Reset()

	w2 := h.newWorker(func(o *Options) { o.Concurrency = 1; o.Tracer = rig.tp.Tracer("test") })
	t.Cleanup(w2.Close)
	if err := w2.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	w2.Wait()

	resumes := rig.named(semconv.SpanResume)
	if len(resumes) != 1 {
		t.Fatalf("%d resume spans, want 1", len(resumes))
	}
	resume := resumes[0]
	if a := spanAttrs(resume); !isRoot(resume) || a[semconv.SpanSource] != Source || a[semconv.SpanRunOutcome] != outcomeOK {
		t.Errorf("resume span %v parent %v, want a root for monzo with outcome ok", a, resume.Parent)
	}
	runs := rig.runs(kindBackfill)
	if len(runs) != 1 {
		t.Fatalf("%d backfill runs after Resume, want 1", len(runs))
	}
	run := runs[0]
	if !isRoot(run) || len(run.Links) != 1 || run.Links[0].SpanContext.SpanID() != resume.SpanContext.SpanID() {
		t.Errorf("backfill after Resume: parent %v links %v, want a root linked to the resume span", run.Parent, run.Links)
	}
}

// A rate-limited run counts under error_category on finstore.sync.runs (so Monzo is visible to the
// rate-limited alert) and its span carries the category and a failed status.
func TestRateLimitedRunCarriesItsErrorCategoryOnTheCounterAndTheSpan(t *testing.T) {
	t.Parallel()
	rig := newSpanRig(t)
	h := rig.harness(t, nil)
	openAccount(h, "acc_test_1", 90)
	h.connect(nil)
	h.fake.Advance(time.Hour)
	rig.mem.Reset()
	h.fake.Fail429(pathTransactions, 1000)

	if err := h.w.RunScheduled(context.Background(), KindHourly); err != nil {
		t.Fatalf("RunScheduled: %v", err)
	}
	run := h.lastRun(KindHourly)
	if run.Outcome == nil || *run.Outcome != outcomePartial || run.ErrorCategory == nil || *run.ErrorCategory != "rate_limited" {
		t.Fatalf("run = outcome %v category %v, want partial rate_limited", run.Outcome, run.ErrorCategory)
	}

	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	want := attribute.NewSet(
		attribute.String(semconv.AttrSource, Source), attribute.String(semconv.AttrKind, KindHourly),
		attribute.String(semconv.AttrOutcome, outcomePartial), attribute.String(semconv.AttrErrorCategory, "rate_limited"))
	var got int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == semconv.MetricSyncRuns {
				for _, dp := range sum.DataPoints {
					if dp.Attributes.Equals(&want) {
						got += dp.Value
					}
				}
			}
		}
	}
	if got != 1 {
		t.Errorf("finstore.sync.runs with error_category=rate_limited = %d, want 1", got)
	}

	runs := rig.runs(KindHourly)
	if len(runs) != 1 {
		t.Fatalf("%d sync.run spans, want 1", len(runs))
	}
	a := spanAttrs(runs[0])
	if a[semconv.SpanRunOutcome] != outcomePartial || a[semconv.SpanErrorType] != "rate_limited" {
		t.Errorf("sync.run attributes %v, want outcome partial and error.type rate_limited", a)
	}
	if runs[0].Status.Code != codes.Error || runs[0].Status.Description != "rate_limited" {
		t.Errorf("sync.run status %v, want an error with the category", runs[0].Status)
	}
}

// A run's category is its span's status description, and the exporter keeps only known category
// words: every category a Monzo run can end with must be one.
func TestMonzoRunCategoriesMayBeSpanStatusDescriptions(t *testing.T) {
	t.Parallel()
	for _, c := range []string{categoryWindowClosed, categoryTimestampGroup, categoryBackfillStarted, "consent_lost", "cancelled", "internal"} {
		if !semconv.IsStatusCategory(c) {
			t.Errorf("run category %q is not a status category, so its span would read \"error\"", c)
		}
	}
}

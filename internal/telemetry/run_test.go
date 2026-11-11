package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/rknightion/finstore/internal/semconv"
)

func runFixture(t *testing.T) (*Metrics, trace.Tracer, *tracetest.InMemoryExporter, *countingDrops, *sdkmetric.ManualReader) {
	t.Helper()
	drops := &countingDrops{}
	tr, mem := filtered(t, drops)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithView(histogramViews()...))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	m, err := NewMetrics(mp.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	return m, tr, mem, drops, reader
}

func attrsOf(s tracetest.SpanStub) map[string]string {
	out := map[string]string{}
	for _, kv := range s.Attributes {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

// A run's span keeps every attribute through the exporter allow-list (none is dropped), takes the
// run's outcome and category, and fails with the category as its status when the run did not end
// ok. The duration is recorded under source, kind and outcome.
func TestRunSpanSurvivesTheAllowListAndRecordsItsDuration(t *testing.T) {
	m, tr, mem, drops, reader := runFixture(t)
	ctx, run := m.StartRun(context.Background(), tr, "monzo", "hourly", 0)
	run.SetRunID(42)
	run.Finish(ctx, "partial", "rate_limited", 7, 40)
	run.End()

	spans := mem.GetSpans()
	if len(spans) != 1 || spans[0].Name != semconv.SpanSyncRun {
		t.Fatalf("spans = %v, want one sync.run", spans)
	}
	got := attrsOf(spans[0])
	for k, want := range map[string]string{
		semconv.SpanSource: "monzo", semconv.SpanRunKind: "hourly", semconv.SpanRunID: "42",
		semconv.SpanRunOutcome: "partial", semconv.SpanErrorType: "rate_limited",
		semconv.SpanRunPages: "7", semconv.SpanRunRows: "40",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q (all %v)", k, got[k], want, got)
		}
	}
	if st := spans[0].Status; st.Code != codes.Error || st.Description != "rate_limited" {
		t.Errorf("status = %v %q, want Error rate_limited after the filter", st.Code, st.Description)
	}
	for _, k := range semconv.SpanAttributes() {
		if drops.count(k) != 0 {
			t.Errorf("the filter dropped %s from a run span", k)
		}
	}
	if RunFromContext(ctx) != run {
		t.Error("StartRun's context does not carry the run")
	}

	h, ok := collectFrom(t, reader)[semconv.MetricSyncRunDuration].Data.(metricdata.Histogram[float64])
	if !ok || len(h.DataPoints) != 1 || h.DataPoints[0].Count != 1 {
		t.Fatalf("finstore.sync.run.duration = %+v, want one point", h)
	}
	if !hasAttrs(h.DataPoints[0].Attributes.ToSlice(), map[string]string{"source": "monzo", "kind": "hourly", "outcome": "partial"}) {
		t.Errorf("duration attributes %v", h.DataPoints[0].Attributes.ToSlice())
	}
}

func TestRunSpanWithNoPagesOrCategoryLeavesThemOff(t *testing.T) {
	m, tr, mem, _, _ := runFixture(t)
	ctx, run := m.StartRun(context.Background(), tr, "truelayer:ob-lloyds", "sync", 5)
	run.Finish(ctx, "ok", "", -1, 12)
	run.End()
	s := mem.GetSpans()[0]
	got := attrsOf(s)
	if _, ok := got[semconv.SpanRunPages]; ok {
		t.Errorf("pages present: %v", got)
	}
	if _, ok := got[semconv.SpanErrorType]; ok {
		t.Errorf("error.type present on an ok run: %v", got)
	}
	if s.Status.Code == codes.Error {
		t.Errorf("an ok run has status %v", s.Status)
	}
}

// A failed run with no category still fails its span, with the generic word.
func TestFailedRunWithNoCategoryFailsWithTheGenericWord(t *testing.T) {
	m, tr, mem, _, _ := runFixture(t)
	ctx, run := m.StartRun(context.Background(), tr, "monzo", "nightly", 1)
	run.Finish(ctx, "failed", "", 0, 0)
	run.End()
	if st := mem.GetSpans()[0].Status; st.Code != codes.Error || st.Description != "error" {
		t.Errorf("status = %v %q, want Error error", st.Code, st.Description)
	}
}

// A run that never produced an outcome is abandoned: failed with its category and no duration. A
// run that finished is left as it was.
func TestAbandonFailsARunThatNeverFinishedAndLeavesAFinishedOne(t *testing.T) {
	m, tr, mem, _, reader := runFixture(t)
	_, never := m.StartRun(context.Background(), tr, "monzo", "backfill", 0)
	never.Abandon("cancelled")
	never.End()
	ctx, done := m.StartRun(context.Background(), tr, "monzo", "hourly", 1)
	done.Finish(ctx, "ok", "", 1, 2)
	done.Abandon("cancelled")
	done.End()

	for _, s := range mem.GetSpans() {
		a := attrsOf(s)
		switch a[semconv.SpanRunKind] {
		case "backfill":
			if a[semconv.SpanRunOutcome] != "failed" || a[semconv.SpanErrorType] != "cancelled" ||
				s.Status.Code != codes.Error || s.Status.Description != "cancelled" {
				t.Errorf("abandoned run %v status %v, want failed and cancelled", a, s.Status)
			}
		case "hourly":
			if a[semconv.SpanRunOutcome] != "ok" || s.Status.Code == codes.Error {
				t.Errorf("finished run %v status %v was changed by Abandon", a, s.Status)
			}
		}
	}
	h, ok := collectFrom(t, reader)[semconv.MetricSyncRunDuration].Data.(metricdata.Histogram[float64])
	if !ok || len(h.DataPoints) != 1 {
		t.Errorf("duration points = %+v, want only the finished run's", h)
	}
}

// A nil run (code that is not inside one) does nothing and never panics.
func TestNilRunSpanDoesNothing(t *testing.T) {
	var run *RunSpan
	run.SetRunID(1)
	run.Finish(context.Background(), "ok", "", 0, 0)
	run.Abandon("cancelled")
	run.End()
	if RunFromContext(context.Background()) != nil {
		t.Error("an empty context carries a run")
	}
}

// Work started with a link becomes a new root that links to the span that began it; an invalid
// link (no tracer behind it) is ignored and the span is an ordinary one.
func TestRunStartedWithALinkIsANewRootLinkedToTheStarter(t *testing.T) {
	m, tr, mem, _, _ := runFixture(t)
	starterCtx, starter := tr.Start(context.Background(), semconv.SpanCallback)
	link := trace.LinkFromContext(starterCtx)
	starter.End()

	bg := WithLink(context.Background(), link)
	ctx, run := m.StartRun(bg, tr, "monzo", "backfill", 0)
	run.End()
	_ = ctx
	var got tracetest.SpanStub
	for _, s := range mem.GetSpans() {
		if s.Name == semconv.SpanSyncRun {
			got = s
		}
	}
	if got.Parent.IsValid() || len(got.Links) != 1 || got.Links[0].SpanContext.SpanID() != starter.SpanContext().SpanID() {
		t.Errorf("run parent %v links %v, want a root linked to the starter", got.Parent, got.Links)
	}

	// An invalid link carries nothing.
	ictx := WithLink(context.Background(), trace.Link{})
	if _, ok := linkFrom(ictx); ok {
		t.Error("an invalid link was carried")
	}
}

// Without a link a run is a child of the span it is started in, so a scheduled run sits inside
// its scheduler.job span.
func TestRunWithoutALinkIsAChildOfTheCurrentSpan(t *testing.T) {
	m, tr, mem, _, _ := runFixture(t)
	jobCtx, job := tr.Start(context.Background(), semconv.SpanSchedulerJob)
	_, run := m.StartRun(jobCtx, tr, "monzo", "hourly", 1)
	run.End()
	job.End()
	for _, s := range mem.GetSpans() {
		if s.Name == semconv.SpanSyncRun && s.Parent.SpanID() != job.SpanContext().SpanID() {
			t.Errorf("run parent %v, want the job span", s.Parent)
		}
	}
}

func TestResumeSpanIsARootWithItsOutcome(t *testing.T) {
	_, tr, mem, _, _ := runFixture(t)
	outer, outerSpan := tr.Start(context.Background(), "outer")
	_, end := StartResume(outer, tr, "monzo")
	end("failed", "consent_lost")
	outerSpan.End()
	for _, s := range mem.GetSpans() {
		if s.Name != semconv.SpanResume {
			continue
		}
		a := attrsOf(s)
		if s.Parent.IsValid() || a[semconv.SpanSource] != "monzo" || a[semconv.SpanRunOutcome] != "failed" ||
			a[semconv.SpanErrorType] != "consent_lost" || s.Status.Code != codes.Error || s.Status.Description != "consent_lost" {
			t.Errorf("resume span %v parent %v status %v", a, s.Parent, s.Status)
		}
		return
	}
	t.Fatal("no resume span")
}

func TestRangeSpanIsARootLinkedToTheRunItIsStartedIn(t *testing.T) {
	m, tr, mem, _, _ := runFixture(t)
	ctx, run := m.StartRun(context.Background(), tr, "monzo", "backfill", 1)
	_, span := StartRange(ctx, tr, semconv.SpanMonzoFetchRange, attribute.String(semconv.SpanRangeStart, "2030-01-01"))
	span.End()
	run.End()
	for _, s := range mem.GetSpans() {
		if s.Name != semconv.SpanMonzoFetchRange {
			continue
		}
		if s.Parent.IsValid() || len(s.Links) != 1 || s.Links[0].SpanContext.SpanID() != run.span.SpanContext().SpanID() {
			t.Errorf("range parent %v links %v, want a root linked to the run", s.Parent, s.Links)
		}
		if attrsOf(s)[semconv.SpanRangeStart] != "2030-01-01" {
			t.Errorf("range attributes %v", attrsOf(s))
		}
		return
	}
	t.Fatal("no range span")
}

// The state gauges and the duration histogram exist and carry the labels the alerts select on.
func TestStateGaugeRecorders(t *testing.T) {
	m, _, _, _, reader := runFixture(t)
	ctx := context.Background()
	at := time.Unix(1900000000, 0)
	m.LastRun(ctx, "monzo", "hourly", false, at)
	m.LastRateLimited(ctx, "monzo", at)
	m.ConsentHistory(ctx, "monzo", true)
	m.ConnectsUnanswered(ctx, "monzo", 2)
	got := collectFrom(t, reader)
	src := map[string]string{"source": "monzo"}
	for name, c := range map[string]struct {
		attrs map[string]string
		want  int64
	}{
		semconv.MetricSyncLastRunOK:           {map[string]string{"source": "monzo", "kind": "hourly"}, 0},
		semconv.MetricSyncLastRunTime:         {map[string]string{"source": "monzo", "kind": "hourly"}, 1900000000},
		semconv.MetricSyncLastRateLimitedTime: {src, 1900000000},
		semconv.MetricConsentHistoryOK:        {src, 1},
		semconv.MetricConnectUnanswered:       {src, 2},
	} {
		if v, ok := gaugePoint(t, got[name], c.attrs); !ok || v != c.want {
			t.Errorf("%s%v = %d (present %v), want %d", name, c.attrs, v, ok, c.want)
		}
	}
}

package truelayer

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
)

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

// harness is a harness whose store traces into the rig; workers get its tracer from tracing().
func (r *spanRig) harness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, []store.OpenOption{store.WithTelemetry(r.tp, metricnoop.NewMeterProvider())})
}

func (r *spanRig) tracing(o *Options) { o.Tracer = r.tp.Tracer("test") }

func (r *spanRig) runs(kind string) []tracetest.SpanStub {
	var out []tracetest.SpanStub
	for _, s := range r.mem.GetSpans() {
		if s.Name == semconv.SpanSyncRun && spanAttrs(s)[semconv.SpanRunKind] == kind {
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

// A scheduled TrueLayer sync is one sync.run root with its bank calls and queries under it.
func TestScheduledSyncIsOneSyncRunRootWithBankAndQueryChildren(t *testing.T) {
	rig := newSpanRig(t)
	h := rig.harness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, rig.tracing)
	h.connect(w)
	rig.mem.Reset()

	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatalf("RunScheduled: %v", err)
	}
	runs := rig.runs(KindSync)
	if len(runs) != 1 {
		t.Fatalf("%d sync.run spans for the scheduled run, want 1", len(runs))
	}
	run := runs[0]
	if run.Parent.IsValid() {
		t.Errorf("sync.run has parent %v, want a root", run.Parent)
	}
	row := h.lastRun(Source(ConnLloyds))
	a := spanAttrs(run)
	for k, want := range map[string]string{
		semconv.SpanSource:     Source(ConnLloyds),
		semconv.SpanRunKind:    KindSync,
		semconv.SpanRunID:      strconv.FormatInt(row.ID, 10),
		semconv.SpanRunOutcome: outcomeOK,
		semconv.SpanRunRows:    strconv.Itoa(row.RowsSeen),
	} {
		if a[k] != want {
			t.Errorf("sync.run %s = %q, want %q (all %v)", k, a[k], want, a)
		}
	}
	if _, ok := a[semconv.SpanRunPages]; ok {
		t.Errorf("a TrueLayer run has no pages, but sync.run carries %q", a[semconv.SpanRunPages])
	}
	if run.Status.Code == codes.Error {
		t.Errorf("an ok run has status %v", run.Status)
	}
	var bank, query int
	for _, s := range rig.mem.GetSpans() {
		if s.Parent.SpanID() != run.SpanContext.SpanID() {
			continue
		}
		switch {
		case strings.HasPrefix(s.Name, semconv.SpanBankCallPrefix):
			bank++
		case spanAttrs(s)[semconv.SpanDBSystemName] != "":
			query++
		}
	}
	if bank == 0 || query == 0 {
		t.Errorf("sync.run has %d bank-call children and %d query children, want both", bank, query)
	}
	if got := h.counter(semconv.MetricSyncRuns); got < 1 {
		t.Errorf("no finstore.sync.runs counted")
	}
}

// A partial run marks its sync.run span failed with the run's category.
func TestRateLimitedSyncFailsItsSpanWithTheCategory(t *testing.T) {
	rig := newSpanRig(t)
	h := rig.harness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, rig.tracing)
	h.connect(w)
	h.fake.Fail429(ConnLloyds, pathTransactions, 1)
	rig.mem.Reset()

	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatalf("RunScheduled: %v", err)
	}
	runs := rig.runs(KindSync)
	if len(runs) != 1 {
		t.Fatalf("%d sync.run spans, want 1", len(runs))
	}
	a := spanAttrs(runs[0])
	if a[semconv.SpanRunOutcome] != outcomePartial || a[semconv.SpanErrorType] != categoryRateLimited {
		t.Errorf("sync.run attributes %v, want partial and rate_limited", a)
	}
	if runs[0].Status.Code != codes.Error || runs[0].Status.Description != categoryRateLimited {
		t.Errorf("sync.run status %v, want an error with the category", runs[0].Status)
	}
}

// The connect-time fetch a callback starts is a new root linked to the callback span, and it
// covers the approval and the wait for the lock as well as the fetch.
func TestConnectRunStartedByACallbackLinksToTheCallbackSpan(t *testing.T) {
	rig := newSpanRig(t)
	h := rig.harness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, rig.tracing)
	ctx, callback := rig.tp.Tracer("test").Start(context.Background(), semconv.SpanCallback)
	h.connectCtx(ctx, w)
	callback.End()

	runs := rig.runs(KindConnect)
	if len(runs) != 1 {
		t.Fatalf("%d connect sync.run spans, want 1", len(runs))
	}
	run := runs[0]
	cb := callback.SpanContext()
	if run.Parent.IsValid() || run.SpanContext.TraceID() == cb.TraceID() {
		t.Errorf("connect run: parent %v trace %v, want a new root outside the callback's trace", run.Parent, run.SpanContext.TraceID())
	}
	if len(run.Links) != 1 || run.Links[0].SpanContext.SpanID() != cb.SpanID() || run.Links[0].SpanContext.TraceID() != cb.TraceID() {
		t.Errorf("connect run links %v, want the callback span %v", run.Links, cb)
	}
	row := h.lastRun(Source(ConnLloyds))
	if a := spanAttrs(run); a[semconv.SpanRunID] != strconv.FormatInt(row.ID, 10) || a[semconv.SpanRunOutcome] != outcomeOf(row) {
		t.Errorf("connect run attributes %v, want run %d outcome %s", a, row.ID, outcomeOf(row))
	}
	// The approval's writer lock is inside the run's trace, not an orphan.
	var inTrace bool
	for _, s := range rig.mem.GetSpans() {
		if s.Name == semconv.SpanWriterLock && s.SpanContext.TraceID() == run.SpanContext.TraceID() {
			inTrace = true
		}
	}
	if !inTrace {
		t.Error("no store.writer_lock span in the connect run's trace")
	}
}

func outcomeOf(r store.Run) string {
	if r.Outcome == nil {
		return ""
	}
	return *r.Outcome
}

// Resume is one resume span; the outcome is ok when it finished, and failed with the category
// when the consent had no token.
func TestResumeIsOneRootSpanWithItsOutcome(t *testing.T) {
	rig := newSpanRig(t)
	h := rig.harness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, rig.tracing)
	h.connect(w)
	rig.mem.Reset()

	if err := w.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	var resumes []tracetest.SpanStub
	for _, s := range rig.mem.GetSpans() {
		if s.Name == semconv.SpanResume {
			resumes = append(resumes, s)
		}
	}
	if len(resumes) != 1 {
		t.Fatalf("%d resume spans, want 1", len(resumes))
	}
	a := spanAttrs(resumes[0])
	if resumes[0].Parent.IsValid() || a[semconv.SpanSource] != Source(ConnLloyds) || a[semconv.SpanRunOutcome] != outcomeOK {
		t.Errorf("resume span %v parent %v, want a root for the source with outcome ok", a, resumes[0].Parent)
	}
}

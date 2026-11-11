package telemetry

import (
	"context"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/rknightion/finstore/internal/semconv"
)

func tracerOrNoop(tr trace.Tracer) trace.Tracer {
	if tr == nil {
		return tracenoop.NewTracerProvider().Tracer(semconv.ServiceName)
	}
	return tr
}

type linkKey struct{}

// WithLink returns ctx carrying link, for the next run span started from it to link to.
// Background work that outlives the span that started it (an approval wait, a history fetch) is
// given the link of that span this way, so its own trace points back at the callback or the
// restart that began it. An invalid link, such as one from a no-op tracer, is ignored.
func WithLink(ctx context.Context, link trace.Link) context.Context {
	if !link.SpanContext.IsValid() {
		return ctx
	}
	return context.WithValue(ctx, linkKey{}, link)
}

func linkFrom(ctx context.Context) (trace.Link, bool) {
	l, ok := ctx.Value(linkKey{}).(trace.Link)
	return l, ok
}

type runKey struct{}

// RunSpan is the sync.run span of one worker run, with the clock the run's duration is measured
// by. A nil *RunSpan does nothing, so code that may or may not be inside a run can call it.
type RunSpan struct {
	m      *Metrics
	span   trace.Span
	source string
	kind   string
	began  time.Time
	done   atomic.Bool // Finish or Abandon has recorded the run's end
}

// StartRun starts the sync.run span of a run of kind for source and returns the context that
// carries it; the code that records the run's end finds the span again with RunFromContext. The
// span is a child of whatever span ctx holds (a scheduled run is inside its scheduler.job span)
// unless ctx carries a link from WithLink, which makes it a new root linked to the span that
// started the work. A runID of zero (not recorded yet) leaves finstore.run.id off; SetRunID adds
// it. A nil tr makes no spans.
func (m *Metrics) StartRun(ctx context.Context, tr trace.Tracer, source, kind string, runID int64) (context.Context, *RunSpan) {
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attribute.String(semconv.SpanSource, source), attribute.String(semconv.SpanRunKind, kind)),
	}
	if link, ok := linkFrom(ctx); ok {
		opts = append(opts, trace.WithNewRoot(), trace.WithLinks(link))
	}
	ctx, span := tracerOrNoop(tr).Start(ctx, semconv.SpanSyncRun, opts...)
	r := &RunSpan{m: m, span: span, source: source, kind: kind, began: time.Now()}
	r.SetRunID(runID)
	return context.WithValue(ctx, runKey{}, r), r
}

// RunFromContext returns the run span ctx carries, or nil.
func RunFromContext(ctx context.Context) *RunSpan {
	r, _ := ctx.Value(runKey{}).(*RunSpan)
	return r
}

// SetRunID records the run's sync.run row id on the span.
func (r *RunSpan) SetRunID(id int64) {
	if r == nil || id <= 0 {
		return
	}
	r.span.SetAttributes(attribute.Int64(semconv.SpanRunID, id))
}

// Finish records how the run ended: on the span its outcome, category (as error.type), pages and
// rows, a failed status for an outcome other than ok, and in finstore.sync.run.duration the time
// since StartRun. A pages below zero means the source has no pages to count. The category is a
// fixed word, never text from a bank. It does not end the span: the code that started it does.
func (r *RunSpan) Finish(ctx context.Context, outcome, category string, pages, rows int) {
	if r == nil {
		return
	}
	r.done.Store(true)
	r.span.SetAttributes(
		attribute.String(semconv.SpanRunOutcome, outcome),
		attribute.Int(semconv.SpanRunRows, rows))
	if pages >= 0 {
		r.span.SetAttributes(attribute.Int(semconv.SpanRunPages, pages))
	}
	if category != "" {
		r.span.SetAttributes(attribute.String(semconv.SpanErrorType, category))
	}
	if outcome != "ok" {
		if category == "" {
			category = "error"
		}
		Fail(r.span, category)
	}
	// Recorded inside the span's context, so the point carries the span as its exemplar.
	r.m.syncRunDuration.Record(ctx, time.Since(r.began).Seconds(), metric.WithAttributes(
		attribute.String(semconv.AttrSource, r.source),
		attribute.String(semconv.AttrKind, r.kind),
		attribute.String(semconv.AttrOutcome, outcome)))
}

// Abandon records a run that never produced an outcome (it never got the writer lock, or its row
// could not be written): outcome failed, the category as error.type, and a failed status. It does
// nothing when Finish has already recorded the run's end, and records no duration, because no run
// was timed.
func (r *RunSpan) Abandon(category string) {
	if r == nil || r.done.Swap(true) {
		return
	}
	if category == "" {
		category = "error"
	}
	r.span.SetAttributes(
		attribute.String(semconv.SpanRunOutcome, "failed"),
		attribute.String(semconv.SpanErrorType, category))
	Fail(r.span, category)
}

// End ends the span.
func (r *RunSpan) End() {
	if r != nil {
		r.span.End()
	}
}

// StartResume starts the resume span of a worker's Resume: a new root, so what Resume starts in
// the background can link to it. The returned end function records the outcome (ok or failed) and
// the failure's category, then ends the span. A nil tr makes no spans.
func StartResume(ctx context.Context, tr trace.Tracer, source string) (context.Context, func(outcome, category string)) {
	ctx, span := tracerOrNoop(tr).Start(ctx, semconv.SpanResume,
		trace.WithNewRoot(),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attribute.String(semconv.SpanSource, source)))
	return ctx, func(outcome, category string) {
		span.SetAttributes(attribute.String(semconv.SpanRunOutcome, outcome))
		if outcome != "ok" {
			if category == "" {
				category = "error"
			}
			span.SetAttributes(attribute.String(semconv.SpanErrorType, category))
			Fail(span, category)
		}
		span.End()
	}
}

// StartRange starts a new root span named name, linked to the span ctx holds, for a unit of work
// inside a run that gets its own trace (a Monzo history range), so the run's trace stays small.
func StartRange(ctx context.Context, tr trace.Tracer, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return tracerOrNoop(tr).Start(ctx, name,
		trace.WithNewRoot(),
		trace.WithLinks(trace.LinkFromContext(ctx)),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...))
}

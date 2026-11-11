// Package telemetry builds the service's log and metric pipelines. Log
// attributes pass an allow-list owned by internal/semconv before any sink sees
// them.
package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"

	"github.com/rknightion/finstore/internal/semconv"
)

// filterHandler drops every attribute whose key is not an allowed log field,
// then delegates to next.
type filterHandler struct{ next slog.Handler }

func newFilterHandler(next slog.Handler) slog.Handler { return filterHandler{next: next} }

func (h filterHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h filterHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		if a, ok := filterAttr(a); ok {
			out.AddAttrs(a)
		}
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h filterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	kept := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		if a, ok := filterAttr(a); ok {
			kept = append(kept, a)
		}
	}
	return filterHandler{next: h.next.WithAttrs(kept)}
}

func (h filterHandler) WithGroup(name string) slog.Handler {
	return filterHandler{next: h.next.WithGroup(name)}
}

// filterAttr applies the allow-list to a, recursing into groups. A group
// itself is kept only if some member survives.
func filterAttr(a slog.Attr) (slog.Attr, bool) {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		var kept []slog.Attr
		for _, m := range a.Value.Group() {
			if m, ok := filterAttr(m); ok {
				kept = append(kept, m)
			}
		}
		if len(kept) == 0 {
			return slog.Attr{}, false
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(kept...)}, true
	}
	if !semconv.IsLogField(a.Key) {
		return slog.Attr{}, false
	}
	return a, true
}

// fanOut sends each record to every handler that is enabled for its level.
type fanOut []slog.Handler

func (f fanOut) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f fanOut) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f {
		if h.Enabled(ctx, r.Level) {
			errs = append(errs, h.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

func (f fanOut) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(fanOut, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (f fanOut) WithGroup(name string) slog.Handler {
	out := make(fanOut, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}

// Keys the stderr sink adds from the span in the context. They are added after the allow-list,
// so they are not log fields; the OTLP sink derives the same ids from the context itself.
const (
	logTraceID = "trace_id"
	logSpanID  = "span_id"
)

// traceIDHandler adds trace_id and span_id to records that carry a valid span in their context.
// It sits on the stderr sink only, which serves docker logs. The keys must reach the top level of
// the line, not a group a caller opened, so it keeps what was applied to base (WithAttrs and
// WithGroup, in order) and replays it after adding the ids to a fresh base.
type traceIDHandler struct {
	base slog.Handler
	ops  []func(slog.Handler) slog.Handler
	cur  slog.Handler // base with ops applied
}

func newTraceIDHandler(base slog.Handler) *traceIDHandler {
	return &traceIDHandler{base: base, cur: base}
}

func (h *traceIDHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.cur.Enabled(ctx, l)
}

func (h *traceIDHandler) Handle(ctx context.Context, r slog.Record) error {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return h.cur.Handle(ctx, r)
	}
	inner := h.base.WithAttrs([]slog.Attr{
		slog.String(logTraceID, sc.TraceID().String()),
		slog.String(logSpanID, sc.SpanID().String()),
	})
	for _, op := range h.ops {
		inner = op(inner)
	}
	return inner.Handle(ctx, r)
}

func (h *traceIDHandler) with(op func(slog.Handler) slog.Handler) *traceIDHandler {
	return &traceIDHandler{base: h.base, ops: append(append([]func(slog.Handler) slog.Handler(nil), h.ops...), op), cur: op(h.cur)}
}

func (h *traceIDHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.with(func(x slog.Handler) slog.Handler { return x.WithAttrs(attrs) })
}

func (h *traceIDHandler) WithGroup(name string) slog.Handler {
	return h.with(func(x slog.Handler) slog.Handler { return x.WithGroup(name) })
}

// NewLogger returns a logger that writes JSON to w at level and also sends
// every record to the extra handlers, all behind the allow-list filter. The JSON line carries
// the trace and span id of the span in the record's context.
func NewLogger(w io.Writer, level slog.Level, extra ...slog.Handler) *slog.Logger {
	sinks := fanOut{newTraceIDHandler(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))}
	sinks = append(sinks, extra...)
	return slog.New(newFilterHandler(sinks))
}

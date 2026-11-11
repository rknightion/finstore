// Package telemetry builds the service's log and metric pipelines. Log
// attributes pass an allow-list owned by internal/semconv before any sink sees
// them.
package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"

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

// NewLogger returns a logger that writes JSON to w at level and also sends
// every record to the extra handlers, all behind the allow-list filter.
func NewLogger(w io.Writer, level slog.Level, extra ...slog.Handler) *slog.Logger {
	sinks := fanOut{slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})}
	sinks = append(sinks, extra...)
	return slog.New(newFilterHandler(sinks))
}

package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
)

// otelHandler forwards slog records to an OpenTelemetry logger. It does no
// filtering of its own: it is always used behind the allow-list filter.
type otelHandler struct {
	logger otellog.Logger
	level  slog.Level
	attrs  []attribute.KeyValue // from WithAttrs, group-qualified
	group  string               // dotted prefix from WithGroup
}

func newOTelHandler(l otellog.Logger, level slog.Level) *otelHandler {
	return &otelHandler{logger: l, level: level}
}

func (h *otelHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *otelHandler) Handle(ctx context.Context, r slog.Record) error {
	var rec otellog.Record
	rec.SetTimestamp(r.Time)
	rec.SetObservedTimestamp(time.Now())
	rec.SetSeverity(severity(r.Level))
	rec.SetSeverityText(r.Level.String())
	rec.SetBody(attribute.StringValue(r.Message))
	rec.AddAttributes(h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		rec.AddAttributes(flatten(h.group, a)...)
		return true
	})
	h.logger.Emit(ctx, rec)
	return nil
}

func (h *otelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append([]attribute.KeyValue(nil), h.attrs...)
	for _, a := range attrs {
		c.attrs = append(c.attrs, flatten(h.group, a)...)
	}
	return &c
}

func (h *otelHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.group = h.group + name + "."
	return &c
}

func severity(l slog.Level) otellog.Severity {
	switch {
	case l >= slog.LevelError:
		return otellog.SeverityError
	case l >= slog.LevelWarn:
		return otellog.SeverityWarn
	case l >= slog.LevelInfo:
		return otellog.SeverityInfo
	default:
		return otellog.SeverityDebug
	}
}

func flatten(prefix string, a slog.Attr) []attribute.KeyValue {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		var out []attribute.KeyValue
		for _, m := range v.Group() {
			out = append(out, flatten(p, m)...)
		}
		return out
	}
	return []attribute.KeyValue{{Key: attribute.Key(prefix + a.Key), Value: value(v)}}
}

func value(v slog.Value) attribute.Value {
	switch v.Kind() {
	case slog.KindBool:
		return attribute.BoolValue(v.Bool())
	case slog.KindInt64:
		return attribute.Int64Value(v.Int64())
	case slog.KindUint64:
		if u := v.Uint64(); u <= math.MaxInt64 {
			return attribute.Int64Value(int64(u))
		}
	case slog.KindFloat64:
		return attribute.Float64Value(v.Float64())
	case slog.KindString:
		return attribute.StringValue(v.String())
	case slog.KindDuration:
		return attribute.Int64Value(v.Duration().Milliseconds())
	case slog.KindTime:
		return attribute.StringValue(v.Time().Format(time.RFC3339Nano))
	}
	return attribute.StringValue(fmt.Sprint(v.Any()))
}

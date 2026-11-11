package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/rknightion/finstore/internal/semconv"
)

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("output is not one JSON object: %v: %s", err, buf)
	}
	return m
}

func TestFilterDropsDisallowedKeys(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, slog.LevelInfo)
	l.Info("sync done",
		"source", "monzo", "count", 3,
		"authorization", "SECRET-MARKER-1",
		"body", slog.GroupValue(slog.String("note", "SECRET-MARKER-2")),
		"err", fmt.Errorf("Get %q: dial failed", "https://bank.example.test/path?token=SECRET-MARKER-3"),
		slog.Group("nested", slog.String("endpoint", "accounts"), slog.String("password", "SECRET-MARKER-4")),
	)
	out := buf.String()
	if strings.Contains(out, "SECRET-MARKER") || strings.Contains(out, "bank.example.test") {
		t.Fatalf("a disallowed value reached the output: %s", out)
	}
	m := decode(t, &buf)
	if m["source"] != "monzo" || m["count"] != float64(3) || m["msg"] != "sync done" {
		t.Errorf("allowed fields missing: %s", out)
	}
	for _, k := range []string{"authorization", "body", "err"} {
		if _, ok := m[k]; ok {
			t.Errorf("key %q survived", k)
		}
	}
	if g, _ := m["nested"].(map[string]any); g["endpoint"] != "accounts" || len(g) != 1 {
		t.Errorf("group not filtered to its allowed member: %s", out)
	}
}

func TestFilterWithAttrsAndWithGroup(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, slog.LevelInfo).
		With("run_id", 7, "token", "SECRET-MARKER-5").
		WithGroup("req").
		With("fetch_id", 9, "url", "SECRET-MARKER-6")
	l.Info("x", "status", 200, "header", errors.New("SECRET-MARKER-7"))
	out := buf.String()
	if strings.Contains(out, "SECRET-MARKER") {
		t.Fatalf("marker leaked: %s", out)
	}
	m := decode(t, &buf)
	if m["run_id"] != float64(7) {
		t.Errorf("WithAttrs allowed field lost: %s", out)
	}
	g, _ := m["req"].(map[string]any)
	if g["fetch_id"] != float64(9) || g["status"] != float64(200) || len(g) != 2 {
		t.Errorf("grouped fields wrong: %s", out)
	}
}

func TestFilterResolvesLogValuer(t *testing.T) {
	var buf bytes.Buffer
	NewLogger(&buf, slog.LevelInfo).Info("x", "secret", leaky{}, "state", "ok")
	if strings.Contains(buf.String(), "SECRET-MARKER") {
		t.Fatalf("lazy value leaked: %s", buf.String())
	}
}

type leaky struct{}

func (leaky) LogValue() slog.Value { return slog.StringValue("SECRET-MARKER-8") }

func TestNewLoggerLevelAndExtraSink(t *testing.T) {
	var buf, extraBuf bytes.Buffer
	extra := slog.NewJSONHandler(&extraBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	l := NewLogger(&buf, slog.LevelWarn, extra)
	l.Info("below", "source", "monzo")
	l.Warn("above", "source", "monzo", "token", "SECRET-MARKER-9")
	if strings.Contains(buf.String(), "below") || !strings.Contains(buf.String(), "above") {
		t.Errorf("primary sink level not applied: %s", buf.String())
	}
	if !strings.Contains(extraBuf.String(), "below") || strings.Contains(extraBuf.String(), "SECRET-MARKER") {
		t.Errorf("extra sink wrong: %s", extraBuf.String())
	}
}

func spanContext() trace.SpanContext {
	tid, _ := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	sid, _ := trace.SpanIDFromHex("b7ad6b7169203331")
	return trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
}

// The stderr sink, which serves docker logs, gets the trace and span id from the context. The
// OTLP sink derives them itself, so only stderr adds the keys, after the allow-list.
func TestStderrSinkCarriesTraceIDsAndNoOtherSinkDoes(t *testing.T) {
	var buf, extraBuf bytes.Buffer
	extra := slog.NewJSONHandler(&extraBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	l := NewLogger(&buf, slog.LevelInfo, extra)
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext())

	l.InfoContext(ctx, "with span", "source", "monzo", "token", "SECRET-MARKER-10")
	m := decode(t, &buf)
	if m["trace_id"] != "0af7651916cd43dd8448eb211c80319c" || m["span_id"] != "b7ad6b7169203331" || m["source"] != "monzo" {
		t.Errorf("stderr line lacks the trace ids: %s", buf.String())
	}
	if strings.Contains(extraBuf.String(), "trace_id") || strings.Contains(extraBuf.String(), "span_id") {
		t.Errorf("the extra sink got the trace keys: %s", extraBuf.String())
	}
	if strings.Contains(buf.String(), "SECRET-MARKER") {
		t.Errorf("a disallowed key reached stderr: %s", buf.String())
	}
	if semconv.IsLogField("trace_id") || semconv.IsLogField("span_id") {
		t.Error("the trace keys must stay off the log allow-list: they are added after it")
	}

	buf.Reset()
	l.Info("no span", "source", "monzo")
	if out := buf.String(); strings.Contains(out, "trace_id") || strings.Contains(out, "span_id") {
		t.Errorf("a record with no span carries trace keys: %s", out)
	}
}

func TestStderrTraceIDsStayAtTheTopLevelUnderGroupsAndAttrs(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, slog.LevelInfo).With("run_id", 7).WithGroup("req").With("fetch_id", 9)
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext())
	l.InfoContext(ctx, "x", "status", 200)

	m := decode(t, &buf)
	if m["trace_id"] != "0af7651916cd43dd8448eb211c80319c" || m["span_id"] != "b7ad6b7169203331" || m["run_id"] != float64(7) {
		t.Errorf("top-level keys wrong: %s", buf.String())
	}
	g, _ := m["req"].(map[string]any)
	if g["fetch_id"] != float64(9) || g["status"] != float64(200) || len(g) != 2 {
		t.Errorf("the group picked up a trace key or lost a member: %s", buf.String())
	}
}

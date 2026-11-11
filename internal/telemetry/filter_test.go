package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/rknightion/finstore/internal/semconv"
)

// countingDrops records the keys the filter reports as dropped.
type countingDrops struct {
	mu   sync.Mutex
	keys map[string]int
}

func (c *countingDrops) SpanAttributeDropped(_ context.Context, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.keys == nil {
		c.keys = map[string]int{}
	}
	c.keys[key]++
}

func (c *countingDrops) count(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.keys[key]
}

// filtered returns a tracer whose spans go through the real exporter filter to an in-memory
// exporter, and the exporter to read what got through.
func filtered(t *testing.T, drops dropRecorder) (trace.Tracer, *tracetest.InMemoryExporter) {
	t.Helper()
	mem := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(newFilteringExporter(mem, semconv.IsSpanAttribute, drops)))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp.Tracer("test"), mem
}

func onlySpan(t *testing.T, mem *tracetest.InMemoryExporter) tracetest.SpanStub {
	t.Helper()
	spans := mem.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
	return spans[0]
}

func attrMap(kvs []attribute.KeyValue) map[string]string {
	m := map[string]string{}
	for _, kv := range kvs {
		m[string(kv.Key)] = kv.Value.String()
	}
	return m
}

// Leak test 8: a key set by test code that is not on the list is dropped and counted, while the
// allowed keys pass untouched.
func TestFilterDropsAnUnknownKeyAndCountsIt(t *testing.T) {
	drops := &countingDrops{}
	tr, mem := filtered(t, drops)
	_, span := tr.Start(context.Background(), "work")
	span.SetAttributes(
		attribute.String(semconv.SpanSource, "monzo"),
		attribute.Int(semconv.SpanHTTPResponseStatusCode, 200),
		attribute.String("canary.key", "CANARY-VALUE"),
		attribute.String("pgx.query.parameters", "CANARY-PARAM"),
		attribute.String("http.request.header.authorization", "CANARY-AUTH"),
	)
	span.End()

	got := onlySpan(t, mem)
	attrs := attrMap(got.Attributes)
	if attrs[semconv.SpanSource] != "monzo" || attrs[semconv.SpanHTTPResponseStatusCode] != "200" {
		t.Errorf("allowed attributes lost: %v", attrs)
	}
	for _, k := range []string{"canary.key", "pgx.query.parameters", "http.request.header.authorization"} {
		if _, ok := attrs[k]; ok {
			t.Errorf("key %q survived the filter", k)
		}
	}
	if strings.Contains(fmt.Sprintf("%#v", got), "CANARY") {
		t.Errorf("a canary value reached the exporter: %#v", got)
	}
	for _, k := range []string{"canary.key", "pgx.query.parameters", "http.request.header.authorization"} {
		if drops.count(k) != 1 {
			t.Errorf("dropped key %q counted %d times, want 1", k, drops.count(k))
		}
	}
	if drops.count(semconv.SpanSource) != 0 {
		t.Error("an allowed key was counted as dropped")
	}
	if got.DroppedAttributes != 3 {
		t.Errorf("DroppedAttributes = %d, want 3", got.DroppedAttributes)
	}
}

// Events never leave: an exception event carries error text, which can quote a value.
func TestFilterRemovesEveryEvent(t *testing.T) {
	tr, mem := filtered(t, &countingDrops{})
	_, span := tr.Start(context.Background(), "work")
	span.RecordError(errors.New(`invalid input syntax for type integer: "CANARY-IN-ERROR"`))
	span.AddEvent("note", trace.WithAttributes(attribute.String(semconv.SpanSource, "CANARY-IN-EVENT")))
	span.End()

	got := onlySpan(t, mem)
	if len(got.Events) != 0 {
		t.Fatalf("%d events left the process: %#v", len(got.Events), got.Events)
	}
	if got.DroppedEvents != 2 {
		t.Errorf("DroppedEvents = %d, want 2", got.DroppedEvents)
	}
	if strings.Contains(fmt.Sprintf("%#v", got), "CANARY") {
		t.Errorf("a canary reached the exporter: %#v", got)
	}
}

// A status description is kept only when it is a fixed category or a SQLSTATE; free text, which
// is how otelpgx records a database error, becomes "error".
func TestFilterStatusDescription(t *testing.T) {
	tests := []struct {
		name string
		set  func(trace.Span)
		code codes.Code
		want string
	}{
		{"category", func(s trace.Span) { s.SetStatus(codes.Error, "rate_limited") }, codes.Error, "rate_limited"},
		{"database_error", func(s trace.Span) { s.SetStatus(codes.Error, "database_error") }, codes.Error, "database_error"},
		{"sqlstate", func(s trace.Span) { s.SetStatus(codes.Error, "22P02") }, codes.Error, "22P02"},
		{"free text", func(s trace.Span) {
			s.SetStatus(codes.Error, `ERROR: invalid input syntax for type integer: "CANARY-STATUS" (SQLSTATE 22P02)`)
		}, codes.Error, "error"},
		{"lower-case five letters", func(s trace.Span) { s.SetStatus(codes.Error, "abcde") }, codes.Error, "error"},
		{"empty", func(s trace.Span) { s.SetStatus(codes.Error, "") }, codes.Error, ""},
		{"unset", func(s trace.Span) {}, codes.Unset, ""},
		{"ok has no description", func(s trace.Span) { s.SetStatus(codes.Ok, "CANARY-OK-TEXT") }, codes.Ok, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr, mem := filtered(t, &countingDrops{})
			_, span := tr.Start(context.Background(), "work")
			tt.set(span)
			span.End()
			got := onlySpan(t, mem)
			if got.Status.Code != tt.code || got.Status.Description != tt.want {
				t.Errorf("status = %v %q, want %v %q", got.Status.Code, got.Status.Description, tt.code, tt.want)
			}
			if strings.Contains(got.Status.Description, "CANARY") {
				t.Error("a canary survived in the status")
			}
		})
	}
}

// url.full is kept when it is a bare address, and dropped and counted when it carries a query, a
// fragment or credentials.
func TestFilterURLFull(t *testing.T) {
	tests := []struct {
		name, url string
		kept      bool
	}{
		{"bare", "https://api.example.test/v1/accounts", true},
		{"query", "https://api.example.test/v1/accounts?code=CANARY-CODE", false},
		{"empty query marker", "https://api.example.test/v1/accounts?", false},
		{"fragment", "https://api.example.test/v1/accounts#CANARY-FRAG", false},
		{"userinfo", (&url.URL{Scheme: "https", User: url.UserPassword("user", "CANARY-PASS"), Host: "api.example.test", Path: "/v1"}).String(), false},
		{"not a url", "::CANARY::", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drops := &countingDrops{}
			tr, mem := filtered(t, drops)
			_, span := tr.Start(context.Background(), "work")
			span.SetAttributes(attribute.String(semconv.SpanURLFull, tt.url))
			span.End()
			got := attrMap(onlySpan(t, mem).Attributes)
			_, kept := got[semconv.SpanURLFull]
			if kept != tt.kept {
				t.Fatalf("url.full kept = %t, want %t (%v)", kept, tt.kept, got)
			}
			if !tt.kept && drops.count(semconv.SpanURLFull) != 1 {
				t.Errorf("url.full counted %d times, want 1", drops.count(semconv.SpanURLFull))
			}
			if strings.Contains(fmt.Sprint(got), "CANARY") {
				t.Errorf("canary survived: %v", got)
			}
		})
	}
}

// Link attributes pass the same list; the link itself, which holds ids only, stays.
func TestFilterLinkAttributes(t *testing.T) {
	drops := &countingDrops{}
	tr, mem := filtered(t, drops)
	_, linked := tr.Start(context.Background(), "starter")
	linked.End()
	mem.Reset()

	_, span := tr.Start(context.Background(), "work", trace.WithLinks(trace.Link{
		SpanContext: linked.SpanContext(),
		Attributes: []attribute.KeyValue{
			attribute.String(semconv.SpanSource, "monzo"),
			attribute.String("canary.link", "CANARY-LINK"),
		},
	}))
	span.End()

	got := onlySpan(t, mem)
	if len(got.Links) != 1 || got.Links[0].SpanContext.TraceID() != linked.SpanContext().TraceID() {
		t.Fatalf("link lost: %#v", got.Links)
	}
	attrs := attrMap(got.Links[0].Attributes)
	if attrs[semconv.SpanSource] != "monzo" || len(attrs) != 1 {
		t.Errorf("link attributes = %v, want only the allowed one", attrs)
	}
	if drops.count("canary.link") != 1 {
		t.Errorf("link key counted %d times, want 1", drops.count("canary.link"))
	}
}

// Everything the filter does not touch passes through as it was.
func TestFilterKeepsNameKindParentAndTimes(t *testing.T) {
	tr, mem := filtered(t, &countingDrops{})
	ctx, parent := tr.Start(context.Background(), "parent")
	_, child := tr.Start(ctx, "child", trace.WithSpanKind(trace.SpanKindClient))
	child.SetAttributes(attribute.String(semconv.SpanSource, "monzo"))
	child.End()
	parent.End()

	spans := mem.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("%d spans", len(spans))
	}
	c := spans[0]
	if c.Name != "child" || c.SpanKind != trace.SpanKindClient || c.Parent.SpanID() != parent.SpanContext().SpanID() ||
		c.SpanContext.TraceID() != parent.SpanContext().TraceID() || c.StartTime.IsZero() || c.EndTime.Before(c.StartTime) {
		t.Errorf("span changed: %#v", c)
	}
	if spans[1].Name != "parent" {
		t.Errorf("second span = %q", spans[1].Name)
	}
}

// The filter must never mutate a span another processor still reads.
func TestFilterDoesNotMutateTheOriginalSpan(t *testing.T) {
	var rec tracetest.SpanRecorder
	mem := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(&rec),
		sdktrace.WithSyncer(newFilteringExporter(mem, semconv.IsSpanAttribute, &countingDrops{})),
	)
	_, span := tp.Tracer("t").Start(context.Background(), "work")
	span.SetAttributes(attribute.String("canary.key", "v"))
	span.RecordError(errors.New("boom"))
	span.End()
	ended := rec.Ended()
	if len(ended) != 1 || len(ended[0].Attributes()) != 1 || len(ended[0].Events()) != 1 {
		t.Fatalf("original span altered: %d attributes, %d events", len(ended[0].Attributes()), len(ended[0].Events()))
	}
	if got := onlySpan(t, mem); len(got.Attributes) != 0 || len(got.Events) != 0 {
		t.Errorf("exported span not filtered: %#v", got)
	}
}

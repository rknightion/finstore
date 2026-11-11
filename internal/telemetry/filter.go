package telemetry

import (
	"context"
	"net/url"
	"regexp"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/rknightion/finstore/internal/semconv"
)

// dropRecorder counts a span attribute key the allow-list removed.
type dropRecorder interface {
	SpanAttributeDropped(ctx context.Context, key string)
}

// sqlStateRE matches a five-character SQLSTATE, the one piece of a database error that is safe
// to keep: it names a class of failure and quotes no value.
var sqlStateRE = regexp.MustCompile(`^[0-9A-Z]{5}$`)

// newFilteringExporter wraps a span exporter so that nothing leaves the process except what the
// allow-list names. It is the single redaction layer for spans. For each span it exports a view
// that embeds the original and overrides:
//
//   - Attributes: only keys allow accepts survive, and url.full only without a query, a fragment
//     or credentials. Each dropped key is counted through drops and added to the span's dropped
//     attribute count.
//   - Events: none, so an exception event, whose message can quote a database value, never
//     leaves.
//   - Links: the same attribute filtering; the link itself holds ids only.
//   - Status: the description survives only as a fixed category word or a SQLSTATE; any other
//     text, such as the error text otelpgx records, becomes "error".
//
// It inspects no value except url.full. The original span is never changed.
func newFilteringExporter(inner sdktrace.SpanExporter, allow func(string) bool, drops dropRecorder) sdktrace.SpanExporter {
	return &filteringExporter{inner: inner, allow: allow, drops: drops}
}

type filteringExporter struct {
	inner sdktrace.SpanExporter
	allow func(string) bool
	drops dropRecorder
}

func (f *filteringExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		out[i] = f.view(ctx, s)
	}
	return f.inner.ExportSpans(ctx, out)
}

func (f *filteringExporter) Shutdown(ctx context.Context) error { return f.inner.Shutdown(ctx) }

func (f *filteringExporter) view(ctx context.Context, s sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	attrs, dropped := f.filter(ctx, s.Attributes())
	links := s.Links()
	if len(links) > 0 {
		kept := make([]sdktrace.Link, len(links))
		for i, l := range links {
			a, d := f.filter(ctx, l.Attributes)
			kept[i] = sdktrace.Link{SpanContext: l.SpanContext, Attributes: a, DroppedAttributeCount: l.DroppedAttributeCount + d}
		}
		links = kept
	}
	return &filteredSpan{
		ReadOnlySpan:  s,
		attrs:         attrs,
		droppedAttrs:  s.DroppedAttributes() + dropped,
		links:         links,
		droppedEvents: s.DroppedEvents() + len(s.Events()),
		status:        sanitiseStatus(s.Status()),
	}
}

// filter returns the attributes that may leave and how many it removed.
func (f *filteringExporter) filter(ctx context.Context, in []attribute.KeyValue) ([]attribute.KeyValue, int) {
	out := make([]attribute.KeyValue, 0, len(in))
	dropped := 0
	for _, kv := range in {
		key := string(kv.Key)
		if !f.allow(key) || (key == semconv.SpanURLFull && !bareURL(kv.Value)) {
			dropped++
			if f.drops != nil {
				f.drops.SpanAttributeDropped(ctx, key)
			}
			continue
		}
		out = append(out, kv)
	}
	return out, dropped
}

// bareURL reports whether v is a string URL with no query, fragment or credentials. Anything
// that does not parse, or is not a string, is refused.
func bareURL(v attribute.Value) bool {
	if v.Type() != attribute.STRING {
		return false
	}
	s := v.AsString()
	if s == "" || strings.ContainsAny(s, "?#") {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.User == nil
}

// sanitiseStatus keeps a description only for an error status and only when it is a known
// category or a SQLSTATE.
func sanitiseStatus(st sdktrace.Status) sdktrace.Status {
	if st.Code != codes.Error {
		return sdktrace.Status{Code: st.Code}
	}
	if d := st.Description; d == "" || semconv.IsStatusCategory(d) || sqlStateRE.MatchString(d) {
		return st
	}
	return sdktrace.Status{Code: codes.Error, Description: "error"}
}

// filteredSpan is a span as the exporter sees it after the allow-list.
type filteredSpan struct {
	sdktrace.ReadOnlySpan
	attrs         []attribute.KeyValue
	droppedAttrs  int
	links         []sdktrace.Link
	droppedEvents int
	status        sdktrace.Status
}

func (s *filteredSpan) Attributes() []attribute.KeyValue { return s.attrs }
func (s *filteredSpan) DroppedAttributes() int           { return s.droppedAttrs }
func (s *filteredSpan) Events() []sdktrace.Event         { return nil }
func (s *filteredSpan) DroppedEvents() int               { return s.droppedEvents }
func (s *filteredSpan) Links() []sdktrace.Link           { return s.links }
func (s *filteredSpan) Status() sdktrace.Status          { return s.status }

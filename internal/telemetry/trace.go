package telemetry

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/rknightion/finstore/internal/semconv"
)

// Fail marks span as failed with a fixed category word, such as a run or bank error category. It
// never records the error itself: an error's text can quote a value. A status description that is
// not a known category is replaced by the span exporter's allow-list.
func Fail(span trace.Span, category string) { span.SetStatus(codes.Error, category) }

// HTTPHandler wraps h so that every request but GET /healthz is a server span and a duration
// measurement. The span is named after the route the mux matched, and carries the request's path,
// never its query string or any header (otelhttp records neither). A traceparent on an incoming
// request becomes a link, never the parent: the service is reachable by anyone who can reach the
// public address.
func HTTPHandler(h http.Handler, p *Providers) http.Handler {
	opts := []otelhttp.Option{
		otelhttp.WithPropagators(propagation.TraceContext{}),
		otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return true }),
		otelhttp.WithFilter(func(r *http.Request) bool { return r.Method != http.MethodGet || r.URL.Path != "/healthz" }),
	}
	if p != nil && p.TracerProvider != nil {
		opts = append(opts, otelhttp.WithTracerProvider(p.TracerProvider))
	} else {
		opts = append(opts, otelhttp.WithTracerProvider(tracenoop.NewTracerProvider()))
	}
	if p != nil && p.MeterProvider != nil {
		opts = append(opts, otelhttp.WithMeterProvider(p.MeterProvider))
	}
	// otelhttp renames the span after the mux has run but records http.route only when the pattern
	// is known at the start, which an outermost wrapper never has. Set it here, on the span that
	// otelhttp put in the request's context, once the mux has set the pattern on that request.
	routed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		if i := strings.IndexByte(r.Pattern, '/'); i >= 0 {
			trace.SpanFromContext(r.Context()).SetAttributes(attribute.String(semconv.SpanHTTPRoute, r.Pattern[i:]))
		}
	})
	return otelhttp.NewHandler(routed, "finstore", opts...)
}

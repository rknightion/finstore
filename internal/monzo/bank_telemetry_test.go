package monzo

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/rknightion/finstore/internal/monzotest"
	"github.com/rknightion/finstore/internal/semconv"
)

// Options.Tracer reaches the bank client: a connect makes a logical span for the token exchange
// and for each data call, named for the source, with an attempt span under each, and the stored
// tokens, the client secret and the account id the bank supplied appear in none of them. The
// attempt durations are measured under source=monzo.
func TestBankCallsAreTracedAndLeakNothing(t *testing.T) {
	t.Parallel()
	mem := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(mem))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	h := connected(t, func(o *Options) { o.Tracer = tp.Tracer("test") })
	tok := h.token()

	spans := mem.GetSpans()
	logical := map[string]int{}
	attempts := 0
	canaries := []string{monzotest.DefaultClientSecret, tok.AccessToken, tok.RefreshToken, "acc_test_1"}
	for _, s := range spans {
		attrs := map[string]string{}
		for _, kv := range s.Attributes {
			attrs[string(kv.Key)] = kv.Value.String()
		}
		switch s.SpanKind {
		case trace.SpanKindInternal:
			logical[s.Name]++
			if attrs[semconv.SpanSource] != Source {
				t.Errorf("logical span %q has source %q", s.Name, attrs[semconv.SpanSource])
			}
		case trace.SpanKindClient:
			attempts++
			if u := attrs[semconv.SpanURLFull]; strings.ContainsAny(u, "?#") || !strings.HasSuffix(u, attrs[semconv.SpanURLTemplate]) {
				t.Errorf("attempt %q url.full = %q", s.Name, u)
			}
		}
		for _, text := range append([]string{s.Name, s.Status.Description}, flatten(attrs)...) {
			for _, c := range canaries {
				if strings.Contains(text, c) {
					t.Errorf("span %q carries a canary: %q", s.Name, text)
				}
			}
		}
	}
	for _, want := range []string{"bank POST /oauth2/token", "bank GET /accounts", "bank GET /transactions"} {
		if logical[want] == 0 {
			t.Errorf("no %q span; have %v", want, logical)
		}
	}
	if attempts < len(logical) {
		t.Errorf("%d attempt spans for %d logical names", attempts, len(logical))
	}

	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if hist, ok := mt.Data.(metricdata.Histogram[float64]); ok && mt.Name == semconv.MetricBankRequestDuration {
				for _, dp := range hist.DataPoints {
					if v, _ := dp.Attributes.Value(semconv.AttrSource); v.AsString() == Source {
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Error("no finstore.bank.request.duration point for source monzo")
	}
}

func flatten(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k, v)
	}
	return out
}

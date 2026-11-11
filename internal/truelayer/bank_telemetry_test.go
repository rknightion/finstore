package truelayer

import (
	"context"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/truelayertest"
)

// Options.Tracer reaches both bank clients of a connection, the auth server's and the Data API's:
// a connect makes a logical span for the token exchange and for each data call, named for the
// connection's source, and the stored tokens, the client secret, and the account ids and bank
// references the bank supplied appear in no span. The data calls name the template, never the
// concrete path.
func TestBankCallsAreTracedForBothClientsAndLeakNothing(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	mem := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(mem))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	w := h.worker(ConnLloyds, func(o *Options) { o.Tracer = tp.Tracer("test") })
	h.connect(w)
	tok, err := store.GetOAuthToken(context.Background(), h.st.DB(), Source(ConnLloyds))
	if err != nil {
		t.Fatal(err)
	}
	canaries := []string{truelayertest.DefaultClientSecret, tok.AccessToken, tok.RefreshToken, "acc-joint", "acc-current", refJoint, refCurrent}

	logical := map[string]int{}
	for _, s := range mem.GetSpans() {
		attrs := map[string]string{}
		var texts []string
		for _, kv := range s.Attributes {
			attrs[string(kv.Key)] = kv.Value.String()
			texts = append(texts, string(kv.Key), kv.Value.String())
		}
		switch s.SpanKind {
		case trace.SpanKindInternal:
			logical[s.Name]++
			if attrs[semconv.SpanSource] != Source(ConnLloyds) {
				t.Errorf("logical span %q has source %q", s.Name, attrs[semconv.SpanSource])
			}
		case trace.SpanKindClient:
			if u := attrs[semconv.SpanURLFull]; strings.ContainsAny(u, "?#") || !strings.HasSuffix(u, attrs[semconv.SpanURLTemplate]) {
				t.Errorf("attempt %q url.full = %q", s.Name, u)
			}
		}
		for _, text := range append(texts, s.Name, s.Status.Description) {
			for _, c := range canaries {
				if c != "" && strings.Contains(text, c) {
					t.Errorf("span %q carries a canary: %q", s.Name, text)
				}
			}
		}
	}
	for _, want := range []string{
		"bank POST /connect/token", "bank GET /data/v1/me", "bank GET /data/v1/accounts",
		"bank GET /data/v1/accounts/{id}/transactions",
	} {
		if logical[want] == 0 {
			t.Errorf("no %q span; have %v", want, logical)
		}
	}
}

package truelayer

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry"
	"github.com/rknightion/finstore/internal/telemetry/telemetrytest"
)

// Bank data runs through a traced store: the response bodies the worker stores in raw.api_response
// are written by queries that are spans of their own, under the bank call spans, and exported
// through the real providers and the real span allow-list. None of what a bank answered (a
// description, a payee, a mandate name, an account number) may reach the collector, in a span, a
// metric or a log. The bank-only harness opens its store without telemetry, so a body that leaked
// into a query span would never be seen there.
func TestBankResponseBodiesStoredThroughATracedStoreReachNoExport(t *testing.T) {
	sink := telemetrytest.New(t)
	ctx := context.Background()
	p, err := telemetry.Setup(ctx, config.Config{OTLPEndpoint: sink.URL(), Environment: "test", LogLevel: slog.LevelInfo},
		telemetry.Identity{Role: telemetry.RoleServe, Version: "main-abc1234", InstanceID: telemetry.ServeInstanceID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	h := newHarnessWith(t, []store.OpenOption{store.WithTelemetry(p.TracerProvider, p.MeterProvider)})
	h.cardFixture()
	w := h.worker(ConnLloyds, func(o *Options) { o.Tracer = p.Tracer(semconv.ServiceName) })
	h.connect(w)
	h.fake.Advance(24 * time.Hour)
	if err := w.RunScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	// What the bank said, all of it made up by the fixture: transaction descriptions, a payee, a
	// mandate name, a card name and the account numbers.
	canaries := []string{
		"MADE UP SHOP", "MADE UP SALARY", "MADE UP MORTGAGE", "MADE UP REFUND", "MADE UP CAFE",
		"MADE UP SAVER", "MADE UP ENERGY", "MADE UP CARD", "MANDATE-0001", "12345678", "87654321",
	}
	// The canaries are real: the bodies went into the store, so a leak under a bank span is possible.
	var stored int
	if err := h.st.DB().QueryRow(ctx, `SELECT count(*) FROM raw.api_response
		WHERE convert_from(body, 'UTF8') LIKE '%MADE UP SALARY%'`).Scan(&stored); err != nil || stored == 0 {
		t.Fatalf("no stored response holds the canary (count %d, err %v)", stored, err)
	}

	var bankSpans, insertSpans int
	for _, s := range sink.Spans() {
		if strings.HasPrefix(s.Name, "bank ") {
			bankSpans++
		}
		if strings.Contains(s.Attrs[semconv.SpanDBQueryText], "raw.api_response") {
			insertSpans++
		}
	}
	if bankSpans == 0 || insertSpans == 0 {
		t.Fatalf("the export holds %d bank spans and %d spans of the response insert: the test does not observe the surface", bankSpans, insertSpans)
	}
	for _, r := range sink.Requests() {
		for _, c := range canaries {
			if bytes.Contains(r.Body, []byte(c)) {
				t.Errorf("export %s carries bank data %q", r.Path, c)
			}
		}
	}
}

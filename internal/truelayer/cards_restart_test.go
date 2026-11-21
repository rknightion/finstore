package truelayer

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/telemetry"
)

// freshMetrics gives the harness a new, empty meter, as a restarted process has.
func (h *harness) freshMetrics() {
	h.t.Helper()
	h.reader = sdkmetric.NewManualReader()
	m, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(h.reader)).Meter("test"))
	if err != nil {
		h.t.Fatal(err)
	}
	h.m = m
}

// #27 AC5: finstore.truelayer.cards_unlisted has a value after a restart, before the next card
// list. Resume seeds it from the newest stored /cards body and the card accounts the ledger holds.
func TestCardsUnlistedHasAValueAfterARestartBeforeTheNextCardList(t *testing.T) {
	h := newHarness(t)
	card, _ := h.cardFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	src := attribute.String(semconv.AttrSource, Source(ConnLloyds))

	// A restart with every held card still listed: 0, not absent.
	h.freshMetrics()
	restarted := h.worker(ConnLloyds, nil)
	if _, ok := h.gauge(semconv.MetricTrueLayerCardsUnlisted, src); ok {
		t.Fatal("cards_unlisted has a value before the restarted worker did anything")
	}
	if err := restarted.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	restarted.Wait()
	if v, ok := h.gauge(semconv.MetricTrueLayerCardsUnlisted, src); !ok || v != 0 {
		t.Fatalf("cards_unlisted after a restart = %d (set %v), want 0", v, ok)
	}

	// The bank stops listing a card, a run stores the new list, and the process restarts before
	// anything else reads it: the gauge reads the card that is missing, with no new card list.
	h.fake.RemoveCard(ConnLloyds, card)
	h.fake.Advance(24 * time.Hour)
	if err := restarted.RunScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.freshMetrics()
	again := h.worker(ConnLloyds, nil)
	calls := h.fake.Count(pathCards)
	if err := again.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	again.Wait()
	if v, ok := h.gauge(semconv.MetricTrueLayerCardsUnlisted, src); !ok || v != 1 {
		t.Fatalf("cards_unlisted after a restart with a card missing = %d (set %v), want 1", v, ok)
	}
	if n := h.fake.Count(pathCards) - calls; n != 0 {
		t.Fatalf("seeding the gauge called /cards %d times, want it read from what is stored", n)
	}
}

// A connection that has never stored a card list has no value to seed.
func TestCardsUnlistedIsNotSeededWithoutAStoredCardList(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	if err := w.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	src := attribute.String(semconv.AttrSource, Source(ConnLloyds))
	if v, ok := h.gauge(semconv.MetricTrueLayerCardsUnlisted, src); ok {
		t.Fatalf("cards_unlisted = %d with no stored card list, want no value", v)
	}
}

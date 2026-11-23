package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rknightion/finstore/internal/store"
)

// A unit count keeps its exact digits through the store, and a version is added for a change only.
func TestInvestmentEventRoundTripAndVersions(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	app := openStore(t, c.app)
	core, err := store.NewCore(app.DB(), store.SchemaCore)
	if err != nil {
		t.Fatal(err)
	}
	accID, instID := uuid.New(), uuid.New()
	if err := core.UpsertAccount(ctx, store.Account{ID: accID, Source: "investment:made-up", SourceKey: "k", Slug: "made-up-child",
		Type: "pension", Feed: "file", Currency: "GBP", Name: "Made-up"}); err != nil {
		t.Fatal(err)
	}
	if err := core.EnsureInstrument(ctx, store.Instrument{ID: instID, Provider: "made-up", ProviderKey: "EXMP"}); err != nil {
		t.Fatal(err)
	}
	if err := core.EnsureInstrument(ctx, store.Instrument{ID: instID, Provider: "made-up", ProviderKey: "EXMP"}); err != nil {
		t.Fatalf("a second ensure: %v", err)
	}
	units, price, unit := "12345678901234.12345", "0.00001", "GBX"
	order := time.Date(2030, 1, 5, 0, 0, 0, 0, time.UTC)
	settle := order.AddDate(0, 0, 2)
	e := store.InvestmentEvent{ID: uuid.New(), AccountID: accID, Source: "investment:made-up", SourceKey: "R1", EventType: "buy",
		ProviderType: "Buy", InstrumentID: &instID, OrderDate: order, SettleDate: &settle, Units: &units, Price: &price, PriceUnit: &unit,
		GrossMinor: 100, Currency: "GBP"}
	t1 := time.Date(2030, 2, 1, 9, 0, 0, 0, time.UTC)
	content := json.RawMessage(`{"a":1}`)
	apply := func(content json.RawMessage, hash string, at time.Time, seq int64) store.ApplyOutcome {
		t.Helper()
		out, err := core.ApplyInvestmentEvent(ctx, e, content, []byte(hash), at, seq)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := apply(content, "h1", t1, 10); out != store.TransactionCreated {
		t.Fatalf("first: %s", out)
	}
	got, err := core.GetInvestmentEvent(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *got.Units != units || *got.Price != price || *got.PriceUnit != unit || !got.EffectiveDate().Equal(settle) || got.VersionCount != 1 {
		t.Fatalf("read back %+v", got)
	}
	if out := apply(content, "h1", t1.Add(time.Hour), 11); out != store.TransactionUnchanged {
		t.Fatalf("repeat: %s", out)
	}
	if out := apply(json.RawMessage(`{"a":2}`), "h2", t1.Add(2*time.Hour), 12); out != store.TransactionChanged {
		t.Fatalf("change: %s", out)
	}
	if out := apply(json.RawMessage(`{"a":3}`), "h3", t1.Add(-time.Hour), 13); out != store.TransactionOlderVersion {
		t.Fatalf("a late older sighting: %s", out)
	}
	got, _ = core.GetInvestmentEvent(ctx, e.ID)
	if string(got.CurrentHash) != "h2" || got.VersionCount != 3 {
		t.Fatalf("after the older sighting: hash %q, %d versions", got.CurrentHash, got.VersionCount)
	}

	// Disappearance: a newly gone row takes the time, a row still gone keeps it, a returned row clears.
	if _, err := app.DB().Exec(ctx, `UPDATE core.investment_event SET event_type = 'contribution' WHERE id = $1`, e.ID); err != nil {
		t.Fatal(err)
	}
	if err := core.SetInvestmentEventsDisappeared(ctx, accID, []uuid.UUID{e.ID}, t1); err != nil {
		t.Fatal(err)
	}
	if err := core.SetInvestmentEventsDisappeared(ctx, accID, []uuid.UUID{e.ID}, t1.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = core.GetInvestmentEvent(ctx, e.ID)
	if got.DisappearedAt == nil || !got.DisappearedAt.Equal(t1) {
		t.Fatalf("disappeared_at %v, want the first time", got.DisappearedAt)
	}
	if err := core.SetInvestmentEventsDisappeared(ctx, accID, nil, t1); err != nil {
		t.Fatal(err)
	}
	got, _ = core.GetInvestmentEvent(ctx, e.ID)
	if got.DisappearedAt != nil {
		t.Fatalf("disappeared_at %v after it returned", got.DisappearedAt)
	}

	if _, err := core.GetInvestmentImport(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a missing import: %v", err)
	}
	if _, err := store.GetImportFile(ctx, app.DB(), 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a missing file: %v", err)
	}
}

// The new tables are in core and core_next alike, and no existing table was changed.
func TestInvestmentTablesExistInEveryBuild(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	owner := openStore(t, c.owner)
	if err := store.CreateCoreNext(ctx, owner.DB(), testRev); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.DropCoreNext(ctx, owner.DB()) }()
	for _, schema := range []string{"core", "core_next"} {
		for _, table := range []string{"instrument", "investment_import", "investment_event", "investment_event_version",
			"holding_snapshot", "valuation", "pension_statement", "trade_note", "holding_check"} {
			var ok bool
			if err := owner.DB().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = $1 AND tablename = $2)`,
				schema, table).Scan(&ok); err != nil || !ok {
				t.Errorf("%s.%s: exists %v, %v", schema, table, ok, err)
			}
		}
	}
	diffs, err := store.DiffCore(ctx, owner.DB())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diffs {
		if d.Kind == store.DiffTable {
			t.Errorf("table shape differs: %+v", d)
		}
	}
}

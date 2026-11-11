package app

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry"
	"github.com/rknightion/finstore/internal/testpg"
)

// The status job publishes how many mandates of each source have no account, so the alert sees a
// mandate that attached to nothing; a source whose mandates have all attached reads 0.
func TestStatusPublisherReportsUnresolvedMandates(t *testing.T) {
	ctx := context.Background()
	super := testpg.Start(t)
	if err := store.Bootstrap(ctx, super, "owner-secret", "app-secret"); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, testpg.UserURL(t, super, store.RoleOwner), ledger.NormaliserRev); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, testpg.UserURL(t, super, store.RoleApp))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	core, err := store.NewCore(st.DB(), store.SchemaCore)
	if err != nil {
		t.Fatal(err)
	}
	ns := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	ref := "112233 12345678"
	acc := store.Account{ID: uuid.NewSHA1(ns, []byte("acc")), Source: "truelayer:ob-lloyds", SourceKey: ref,
		Slug: "lloyds-joint", Type: "joint", Feed: "api", Currency: "GBP", Name: "made-up", BankRef: &ref}
	if err := core.UpsertAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	name := "MADE UP COMPANY"
	for i, dd := range []store.DirectDebit{
		{ID: uuid.NewSHA1(ns, []byte("dd1")), BankRef: ref, Source: "truelayer:ob-lloyds", MandateKey: "K1", Name: &name, AccountID: &acc.ID},
		{ID: uuid.NewSHA1(ns, []byte("dd2")), BankRef: "999999 00000001", Source: "truelayer:ob-monzo", MandateKey: "K2", Name: &name},
		{ID: uuid.NewSHA1(ns, []byte("dd3")), BankRef: "999999 00000002", Source: "truelayer:ob-monzo", MandateKey: "K3", Name: &name},
	} {
		if _, err := core.ApplyDirectDebit(ctx, dd, []byte(`{}`), []byte{byte(i)}, at, int64(i+1)); err != nil {
			t.Fatal(err)
		}
	}

	reader := sdkmetric.NewManualReader()
	metrics, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	p := &statusPublisher{st: st, m: metrics, log: slog.New(slog.DiscardHandler), now: func() time.Time { return at }}
	if err := p.run(ctx); err != nil {
		t.Fatalf("status job: %v", err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != semconv.MetricMandatesUnresolved {
				continue
			}
			for _, dp := range m.Data.(metricdata.Gauge[int64]).DataPoints {
				src, _ := dp.Attributes.Value(attribute.Key(semconv.AttrSource))
				got[src.AsString()] = dp.Value
			}
		}
	}
	if len(got) != 2 || got["truelayer:ob-monzo"] != 2 || got["truelayer:ob-lloyds"] != 0 {
		t.Fatalf("%s = %v, want ob-monzo 2 and ob-lloyds 0", semconv.MetricMandatesUnresolved, got)
	}
	if _, ok := got["truelayer:ob-lloyds"]; !ok {
		t.Fatal("a source with every mandate attached has no value")
	}
}

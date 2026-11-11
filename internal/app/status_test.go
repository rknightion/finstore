package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry"
	"github.com/rknightion/finstore/internal/testpg"
)

// A dump status file that is absent, half-written or without a time must read as "no dump yet",
// which the "dump stale" alert reports; it must never stop the status job or report a wrong time.
func TestReadDumpStatus(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, ok := readDumpStatus(""); ok {
		t.Error("an unset path reads as a dump")
	}
	if _, ok := readDumpStatus(filepath.Join(dir, "absent.json")); ok {
		t.Error("an absent file reads as a dump")
	}
	for name, content := range map[string]string{
		"empty.json":   "",
		"partial.json": `{"finished_at":"2030-01-02T03`,
		"notime.json":  `{"bytes":12}`,
		"badtime.json": `{"finished_at":"yesterday","bytes":12}`,
	} {
		if _, ok := readDumpStatus(write(name, content)); ok {
			t.Errorf("%s reads as a dump", name)
		}
	}
	at, ok := readDumpStatus(write("good.json", `{"finished_at":"2030-01-02T03:04:05Z","bytes":12}`+"\n"))
	if !ok || !at.Equal(time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("good file read as %v, %v", at, ok)
	}
}

// TestStatusPublisherReportsTheDatabasesState runs the once-a-minute job against a real database
// as the app role and reads the gauges back. The alerts are written against these values, so a
// gauge that is missing, mislabelled or computed from the wrong date is an alert that never fires
// or fires wrongly.
func TestStatusPublisherReportsTheDatabasesState(t *testing.T) {
	ctx := context.Background()
	super := testpg.Start(t)
	env := map[string]string{
		config.EnvSuperuserDatabaseURL: super,
		config.EnvOwnerDatabaseURL:     testpg.UserURL(t, super, store.RoleOwner),
		config.EnvDatabaseURL:          testpg.UserURL(t, super, store.RoleApp),
		config.EnvOwnerPassword:        "owner-password",
		config.EnvAppPassword:          "app-password",
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	run := func(stdin string, fn func(context.Context, Env) int) {
		t.Helper()
		if code := fn(ctx, Env{Cfg: cfg, Stdin: strings.NewReader(stdin), Stdout: io.Discard, Stderr: &stderr}); code != ExitOK {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
	}
	run("", Bootstrap)
	run("", Migrate)

	// Two statement-fed accounts: one that expects imports, whose newest statement ends later than
	// its newest transaction would suggest only through the statement's own dates, and one that
	// expects none.
	run("", func(ctx context.Context, e Env) int {
		return AccountAdd(ctx, e, AccountSpec{Slug: "bank-joint", Type: "joint", Currency: "GBP", Name: "made-up", ExpectsImports: true})
	})
	run("", func(ctx context.Context, e Env) int {
		return AccountAdd(ctx, e, AccountSpec{Slug: "bank-loan", Type: "mortgage", Currency: "GBP", Name: "made-up"})
	})
	statement := "date,type,amount,balance_after,description\n" +
		"2030-03-01,PAY,10.00,10.00,made-up\n2030-03-09,PAY,15.00,25.00,made-up\n"
	for _, slug := range []string{"bank-joint", "bank-loan"} {
		run(statement, func(ctx context.Context, e Env) int { return Import(ctx, e, slug, FormatManualCSV, "made-up.csv") })
	}

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2030, 4, 1, 12, 0, 0, 0, time.UTC)
	approved := now.Add(-24 * time.Hour)
	consentID, err := store.CreateConsent(ctx, st.DB(), "monzo", approved)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentConsent(ctx, st.DB(), consentID); err != nil {
		t.Fatal(err)
	}
	expiry := approved.Add(90 * 24 * time.Hour)
	if _, err := store.TransitionConsent(ctx, st.DB(), consentID, store.ConsentTransition{
		To: store.ConsentActive, At: approved, From: []string{store.ConsentAwaitingApproval}, ExpectedExpiryAt: &expiry,
	}); err != nil {
		t.Fatal(err)
	}
	synced := now.Add(-30 * time.Minute)
	if err := store.SetConsentLastSyncOK(ctx, st.DB(), consentID, synced); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.CreateConnectAttempt(ctx, st.DB(), "monzo", []byte("made-up-hash"), now.Add(-2*time.Hour), now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	dumped := now.Add(-3 * time.Hour)
	dumpFile := filepath.Join(t.TempDir(), "dump.json")
	if err := os.WriteFile(dumpFile, []byte(`{"finished_at":"`+dumped.Format(time.RFC3339)+`","bytes":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	reader := sdkmetric.NewManualReader()
	metrics, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	p := &statusPublisher{st: st, m: metrics, log: slog.New(slog.DiscardHandler), dumpFile: dumpFile, now: func() time.Time { return now }}
	if err := p.run(ctx); err != nil {
		t.Fatalf("status job: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	points := map[string][]metricdata.DataPoint[int64]{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if g, ok := m.Data.(metricdata.Gauge[int64]); ok {
				points[m.Name] = g.DataPoints
			}
		}
	}
	value := func(name string, attrs map[string]string) (int64, bool) {
		for _, dp := range points[name] {
			match := dp.Attributes.Len() == len(attrs)
			for k, v := range attrs {
				if got, _ := dp.Attributes.Value(attribute.Key(k)); got.AsString() != v {
					match = false
				}
			}
			if match {
				return dp.Value, true
			}
		}
		return 0, false
	}
	want := func(name string, attrs map[string]string, v int64) {
		t.Helper()
		if got, ok := value(name, attrs); !ok || got != v {
			t.Errorf("%s%v = %d (present %v), want %d", name, attrs, got, ok, v)
		}
	}
	want(semconv.MetricUp, nil, 1)
	want(semconv.MetricDumpLastSuccessTime, nil, dumped.Unix())
	want(semconv.MetricConsentState, map[string]string{"source": "monzo", "state": "active"}, 1)
	want(semconv.MetricConsentState, map[string]string{"source": "monzo", "state": "lost"}, 0)
	want(semconv.MetricConsentState, map[string]string{"source": "monzo", "state": "expired"}, 0)
	want(semconv.MetricConsentExpiryTime, map[string]string{"source": "monzo"}, expiry.Unix())
	want(semconv.MetricSyncLastSuccessTime, map[string]string{"source": "monzo"}, synced.Unix())
	want(semconv.MetricReconciliationMismatch, map[string]string{"account": "bank-joint"}, 0)
	want(semconv.MetricImportLastTime, map[string]string{"account": "bank-joint"}, time.Date(2030, 3, 9, 0, 0, 0, 0, time.UTC).Unix())
	if _, ok := value(semconv.MetricImportLastTime, map[string]string{"account": "bank-loan"}); ok {
		t.Error("an account that expects no imports has an import time, so it would raise \"import due\"")
	}

	// The job also retires connect attempts whose hour has passed.
	var outcome *string
	if err := st.DB().QueryRow(ctx, `SELECT outcome FROM sync.connect_attempt WHERE id = $1`, attempt).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if outcome == nil || *outcome != "expired" {
		t.Errorf("the overdue connect attempt has outcome %v, want expired", outcome)
	}
}

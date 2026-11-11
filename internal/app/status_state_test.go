package app

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry"
	"github.com/rknightion/finstore/internal/testpg"
)

// stateRig is a migrated database, a status publisher whose clock the test sets, and the reader
// its gauges are collected from. The alerts read these gauges, so each test names the alert it
// serves.
type stateRig struct {
	t      *testing.T
	st     *store.Store
	now    time.Time
	reader *sdkmetric.ManualReader
	p      *statusPublisher
}

func newStateRig(t *testing.T, now time.Time) *stateRig {
	t.Helper()
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
	t.Cleanup(st.Close)
	reader := sdkmetric.NewManualReader()
	metrics, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	r := &stateRig{t: t, st: st, now: now, reader: reader}
	r.p = &statusPublisher{st: st, m: metrics, log: slog.New(slog.DiscardHandler), now: func() time.Time { return r.now }}
	return r
}

// status runs the once-a-minute job.
func (r *stateRig) status() {
	r.t.Helper()
	if err := r.p.run(context.Background()); err != nil {
		r.t.Fatalf("status job: %v", err)
	}
}

// run records a finished run.
func (r *stateRig) run(source, kind, outcome string, category *string, finished time.Time) {
	r.t.Helper()
	ctx := context.Background()
	id, err := store.StartRun(ctx, r.st.DB(), source, kind, finished.Add(-time.Minute))
	if err != nil {
		r.t.Fatal(err)
	}
	if err := store.FinishRun(ctx, r.st.DB(), id, store.RunResult{FinishedAt: finished, Outcome: outcome, ErrorCategory: category}); err != nil {
		r.t.Fatal(err)
	}
}

// consent records a current active consent approved at approved.
func (r *stateRig) consent(source string, approved time.Time) {
	r.t.Helper()
	ctx := context.Background()
	q := r.st.DB()
	id, err := store.CreateConsent(ctx, q, source, approved.Add(-time.Minute))
	if err != nil {
		r.t.Fatal(err)
	}
	if err := store.SetCurrentConsent(ctx, q, id); err != nil {
		r.t.Fatal(err)
	}
	expiry := approved.AddDate(0, 0, 90)
	if _, err := store.TransitionConsent(ctx, q, id, store.ConsentTransition{
		To: store.ConsentActive, At: approved, From: []string{store.ConsentAwaitingApproval}, ExpectedExpiryAt: &expiry,
	}); err != nil {
		r.t.Fatal(err)
	}
}

// attempt records a connect attempt that started at started and expired an hour later.
func (r *stateRig) attempt(source, label string, started time.Time) {
	r.t.Helper()
	hash := sha256.Sum256([]byte(label))
	if _, err := store.CreateConnectAttempt(context.Background(), r.st.DB(), source, hash[:], started, started.Add(time.Hour)); err != nil {
		r.t.Fatal(err)
	}
}

// gauge collects the metrics and returns the int64 gauge point with exactly attrs.
func (r *stateRig) gauge(name string, attrs map[string]string) (int64, bool) {
	r.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(context.Background(), &rm); err != nil {
		r.t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok || m.Name != name {
				continue
			}
			for _, dp := range g.DataPoints {
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
		}
	}
	return 0, false
}

// points counts the gauge points a metric has, whatever their attributes.
func (r *stateRig) points(name string) int {
	r.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(context.Background(), &rm); err != nil {
		r.t.Fatal(err)
	}
	n := 0
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if g, ok := m.Data.(metricdata.Gauge[int64]); ok && m.Name == name {
				n += len(g.DataPoints)
			}
		}
	}
	return n
}

func (r *stateRig) want(name string, attrs map[string]string, v int64) {
	r.t.Helper()
	if got, ok := r.gauge(name, attrs); !ok || got != v {
		r.t.Errorf("%s%v = %d (present %v), want %d", name, attrs, got, ok, v)
	}
}

// Sync Run Not OK reads finstore.sync.last_run.ok: a partial run must read 0 on the next status
// run, a later ok run must read 1 again, and a failed capture or import run, which no alert
// covers, must publish nothing.
func TestStatusPublishesTheLatestRunOfEachKind(t *testing.T) {
	now := time.Date(2030, 4, 1, 12, 0, 0, 0, time.UTC)
	r := newStateRig(t, now)
	hourly := map[string]string{"source": "monzo", "kind": "hourly"}

	r.run("monzo", "hourly", "ok", nil, now.Add(-2*time.Hour))
	r.status()
	r.want(semconv.MetricSyncLastRunOK, hourly, 1)
	r.want(semconv.MetricSyncLastRunTime, hourly, now.Add(-2*time.Hour).Unix())

	timeout := "timeout"
	r.run("monzo", "hourly", "partial", &timeout, now.Add(-time.Hour))
	r.status()
	r.want(semconv.MetricSyncLastRunOK, hourly, 0)
	r.want(semconv.MetricSyncLastRunTime, hourly, now.Add(-time.Hour).Unix())

	r.run("monzo", "hourly", "ok", nil, now.Add(-time.Minute))
	r.status()
	r.want(semconv.MetricSyncLastRunOK, hourly, 1)

	// Serve stopping under a run (the deploy case) leaves the run recorded failed and cancelled. It
	// must not read as a failure: the previous result stands.
	cancelled := "cancelled"
	r.run("monzo", "hourly", "failed", &cancelled, now)
	r.status()
	r.want(semconv.MetricSyncLastRunOK, hourly, 1)
	r.want(semconv.MetricSyncLastRunTime, hourly, now.Add(-time.Minute).Unix())

	// A run that hit its own deadline is a real failure, unlike a shutdown: it reads 0.
	deadline := "timeout"
	r.run("monzo", "hourly", "failed", &deadline, now.Add(time.Second))
	r.status()
	r.want(semconv.MetricSyncLastRunOK, hourly, 0)
	r.want(semconv.MetricSyncLastRunTime, hourly, now.Add(time.Second).Unix())

	// An operator command that failed is not published, whatever its kind.
	r.run("halifax", "import", "failed", nil, now)
	r.run("capture", "capture", "failed", nil, now)
	r.status()
	if got, want := r.points(semconv.MetricSyncLastRunOK), 1; got != want {
		t.Errorf("last_run.ok has %d points, want %d (only monzo hourly): a capture or import run is published", got, want)
	}
	if _, ok := r.gauge(semconv.MetricSyncLastRunOK, map[string]string{"source": "capture", "kind": "capture"}); ok {
		t.Error("a failed capture run publishes last_run.ok")
	}
	if _, ok := r.gauge(semconv.MetricSyncLastRunOK, map[string]string{"source": "halifax", "kind": "import"}); ok {
		t.Error("a failed import run publishes last_run.ok")
	}
}

// Sync Rate Limited reads finstore.sync.last_rate_limited.time: a source with no rate-limited run
// publishes nothing, so the alert has no series for it.
func TestStatusPublishesWhenASourceWasLastRateLimited(t *testing.T) {
	now := time.Date(2030, 4, 1, 12, 0, 0, 0, time.UTC)
	r := newStateRig(t, now)
	limited, timeout := "rate_limited", "timeout"
	r.run("monzo", "hourly", "partial", &timeout, now.Add(-time.Hour))
	r.run("truelayer:ob-lloyds", "sync", "partial", &limited, now.Add(-3*time.Hour))
	r.status()

	r.want(semconv.MetricSyncLastRateLimitedTime, map[string]string{"source": "truelayer:ob-lloyds"}, now.Add(-3*time.Hour).Unix())
	if _, ok := r.gauge(semconv.MetricSyncLastRateLimitedTime, map[string]string{"source": "monzo"}); ok {
		t.Error("a source with no rate-limited run publishes last_rate_limited.time")
	}
	if got := r.points(semconv.MetricSyncLastRateLimitedTime); got != 1 {
		t.Errorf("last_rate_limited.time has %d points, want 1", got)
	}
}

// Connect Not Completed reads finstore.connect.unanswered: an attempt that expired unused within
// the last 24 hours reads 1, and one older than that reads 0.
func TestStatusPublishesConnectAttemptsThatExpiredUnused(t *testing.T) {
	now := time.Date(2030, 4, 1, 12, 0, 0, 0, time.UTC)
	r := newStateRig(t, now)
	r.attempt("monzo", "recent", now.Add(-3*time.Hour))
	r.attempt("truelayer:ob-lloyds", "old", now.Add(-30*time.Hour))
	// An attempt still open (its hour has not passed) is not expired and is not counted.
	r.attempt("truelayer:ob-monzo", "open", now.Add(-10*time.Minute))
	r.status()

	r.want(semconv.MetricConnectUnanswered, map[string]string{"source": "monzo"}, 1)
	r.want(semconv.MetricConnectUnanswered, map[string]string{"source": "truelayer:ob-lloyds"}, 0)
	r.want(semconv.MetricConnectUnanswered, map[string]string{"source": "truelayer:ob-monzo"}, 0)

	// A day later the recent attempt has aged out and the gauge reads 0 again.
	r.now = now.Add(22 * time.Hour)
	r.status()
	r.want(semconv.MetricConnectUnanswered, map[string]string{"source": "monzo"}, 0)
}

// History Missing reads finstore.consent.history_ok: an active consent with no ok connect or
// backfill run since its approval reads 0, and an ok one makes it 1.
func TestStatusPublishesWhetherTheCurrentConsentHasItsHistory(t *testing.T) {
	now := time.Date(2030, 4, 1, 12, 0, 0, 0, time.UTC)
	r := newStateRig(t, now)
	approved := now.Add(-48 * time.Hour)
	r.consent("truelayer:ob-lloyds", approved)
	r.consent("monzo", approved)
	// Monzo's backfill ended ok, but for the consent before this one.
	r.run("monzo", "backfill", "ok", nil, approved.Add(-24*time.Hour))
	// Lloyds' connect ended partial.
	r.run("truelayer:ob-lloyds", "connect", "partial", nil, approved.Add(time.Minute))
	r.status()

	r.want(semconv.MetricConsentHistoryOK, map[string]string{"source": "truelayer:ob-lloyds"}, 0)
	r.want(semconv.MetricConsentHistoryOK, map[string]string{"source": "monzo"}, 0)

	r.run("monzo", "backfill", "ok", nil, approved.Add(5*time.Minute))
	r.status()
	r.want(semconv.MetricConsentHistoryOK, map[string]string{"source": "monzo"}, 1)
	r.want(semconv.MetricConsentHistoryOK, map[string]string{"source": "truelayer:ob-lloyds"}, 0)
}

package monzo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/monzotest"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry"
	"github.com/rknightion/finstore/internal/testpg"
)

// stubProjector records CatchUp calls; the real ledger is tested in its own package.
type stubProjector struct {
	mu      sync.Mutex
	schemas []string
}

func (p *stubProjector) CatchUp(_ context.Context, _ store.DBTX, schema string) (ledger.Stats, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.schemas = append(p.schemas, schema)
	return ledger.Stats{}, nil
}

func (p *stubProjector) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.schemas)
}

// harness is one scenario: a private database, the fake API, and a worker whose clock is the
// fake's and whose sleeps advance it.
type harness struct {
	t      *testing.T
	fake   *monzotest.Fake
	srv    *httptest.Server
	st     *store.Store
	proj   *stubProjector
	reader *sdkmetric.ManualReader
	m      *telemetry.Metrics
	w      *Worker

	mu      sync.Mutex
	codes   int
	sleeps  []time.Duration
	onSleep func(ctx context.Context, n int, d time.Duration) error
	// onRequest, when set, runs before the fake handles each request.
	onRequest func(r *http.Request)
}

func newHarness(t *testing.T, fakeOpts []monzotest.Option, mod func(*Options)) *harness {
	t.Helper()
	ctx := context.Background()
	super := testpg.Start(t)
	if err := store.Bootstrap(ctx, super, "owner-secret", "app-secret"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := store.Migrate(ctx, testpg.UserURL(t, super, store.RoleOwner), ledger.NormaliserRev); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	st, err := store.Open(ctx, testpg.UserURL(t, super, store.RoleApp))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(st.Close)

	h := &harness{t: t, fake: monzotest.New(fakeOpts...), st: st, proj: &stubProjector{}}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		hook := h.onRequest
		h.mu.Unlock()
		if hook != nil {
			hook(r)
		}
		h.fake.ServeHTTP(w, r)
	}))
	t.Cleanup(h.srv.Close)
	h.reader = sdkmetric.NewManualReader()
	h.m, err = telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(h.reader)).Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	h.w = h.newWorker(mod)
	t.Cleanup(func() {
		h.w.Close()
		if v := h.fake.Violations(); len(v) != 0 {
			t.Errorf("the fake saw %d requests that were not a GET or a token POST: %+v", len(v), v)
		}
	})
	return h
}

func (h *harness) options(mod func(*Options)) Options {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		h.t.Fatal(err)
	}
	o := Options{
		ClientID: monzotest.DefaultClientID, ClientSecret: monzotest.DefaultClientSecret,
		APIURL: h.srv.URL, AuthURL: "https://auth.example.test/", PublicURL: "https://finstore.example.test",
		Location: london, Now: h.fake.Now, Sleep: h.sleep,
	}
	if mod != nil {
		mod(&o)
	}
	return o
}

func (h *harness) newWorker(mod func(*Options)) *Worker {
	h.t.Helper()
	w, err := New(h.options(mod), h.st, h.proj, h.m, nil)
	if err != nil {
		h.t.Fatalf("New: %v", err)
	}
	return w
}

// sleep records the pause, runs the hook, then advances the fake clock by d.
func (h *harness) sleep(ctx context.Context, d time.Duration) error {
	h.mu.Lock()
	n := len(h.sleeps)
	h.sleeps = append(h.sleeps, d)
	hook := h.onSleep
	h.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, n, d); err != nil {
			return err
		}
	}
	h.fake.Advance(d)
	return ctx.Err()
}

func (h *harness) setOnSleep(fn func(ctx context.Context, n int, d time.Duration) error) {
	h.mu.Lock()
	h.onSleep = fn
	h.mu.Unlock()
}

func (h *harness) sleepsFrom(i int) []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.sleeps[i:])
}

func (h *harness) sleepCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sleeps)
}

// callback starts an attempt and completes the callback with a fresh code.
func (h *harness) callback(w *Worker) {
	h.t.Helper()
	ctx := context.Background()
	u, err := w.AuthoriseURL(ctx)
	if err != nil {
		h.t.Fatalf("AuthoriseURL: %v", err)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		h.t.Fatal(err)
	}
	h.mu.Lock()
	h.codes++
	code := "code_test_" + strconv.Itoa(h.codes)
	h.mu.Unlock()
	h.fake.RegisterCode(code)
	if err := w.HandleCallback(ctx, parsed.Query().Get("state"), code); err != nil {
		h.t.Fatalf("HandleCallback: %v", err)
	}
}

// connect runs a callback, approves at the first poll pause (so the first poll sees the expected
// 403), runs extra on approval, and waits for the full-history fetch to finish.
func (h *harness) connect(onApprove func()) {
	h.t.Helper()
	start := h.sleepCount()
	h.setOnSleep(func(_ context.Context, n int, _ time.Duration) error {
		if n == start {
			h.fake.Approve()
			if onApprove != nil {
				onApprove()
			}
		}
		return nil
	})
	h.callback(h.w)
	h.w.Wait()
	h.setOnSleep(nil)
}

func (h *harness) consent() store.Consent {
	h.t.Helper()
	c, err := store.CurrentConsent(context.Background(), h.st.DB(), Source)
	if err != nil {
		h.t.Fatalf("CurrentConsent: %v", err)
	}
	return c
}

// rawRows returns every raw row.
func (h *harness) rawRows() []store.RawRow {
	h.t.Helper()
	var out []store.RawRow
	for r, err := range store.RawRowsAfter(context.Background(), h.st.DB(), 0) {
		if err != nil {
			h.t.Fatalf("RawRowsAfter: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func (h *harness) windows(account string) []store.BackfillWindow {
	h.t.Helper()
	ws, err := store.ListBackfillWindows(context.Background(), h.st.DB(), account)
	if err != nil {
		h.t.Fatal(err)
	}
	return ws
}

// txRequests returns the fake's /transactions requests from index from of its log on.
func (h *harness) txRequests(from int) []monzotest.Request {
	var out []monzotest.Request
	for _, r := range h.fake.Requests()[from:] {
		if r.Path == pathTransactions {
			out = append(out, r)
		}
	}
	return out
}

// gauge returns the last recorded value of an int64 gauge with exactly attrs.
func (h *harness) gauge(name string, attrs ...attribute.KeyValue) (int64, bool) {
	h.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		h.t.Fatal(err)
	}
	want := attribute.NewSet(attrs...)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				continue
			}
			for _, dp := range g.DataPoints {
				if dp.Attributes.Equals(&want) {
					return dp.Value, true
				}
			}
		}
	}
	return 0, false
}

func mustRun(t *testing.T, st *store.Store, id int64) store.Run {
	t.Helper()
	r, err := store.GetRun(context.Background(), st.DB(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// lastRun returns the run of the newest fetch of kind.
func (h *harness) lastRun(kind string) store.Run {
	h.t.Helper()
	var id int64
	for _, r := range h.rawRows() {
		if r.Kind == store.RawFetch && r.Fetch.Kind == kind {
			id = r.Fetch.RunID
		}
	}
	if id == 0 {
		h.t.Fatalf("no fetch of kind %s", kind)
	}
	return mustRun(h.t, h.st, id)
}

package monzo

import (
	"context"
	"encoding/json"
	"errors"
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

// stubProjector records CatchUp calls and runs the real ledger, so every scenario also checks
// what the projection makes of the raw rows written.
type stubProjector struct {
	mu      sync.Mutex
	schemas []string
	real    *ledger.Ledger
}

func (p *stubProjector) CatchUp(ctx context.Context, q store.DBTX, schema string) (ledger.Stats, error) {
	p.mu.Lock()
	p.schemas = append(p.schemas, schema)
	p.mu.Unlock()
	return p.real.CatchUp(ctx, q, schema)
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
	// around, when set, handles each request in place of the fake; serve is the fake.
	around func(w http.ResponseWriter, r *http.Request, serve http.Handler)
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

	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	real := ledger.New(map[string]ledger.SourceNormaliser{Source: Normaliser{}}, nil, loc, nil)
	h := &harness{t: t, fake: monzotest.New(fakeOpts...), st: st, proj: &stubProjector{real: real}}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		hook, around := h.onRequest, h.around
		h.mu.Unlock()
		if hook != nil {
			hook(r)
		}
		if around != nil {
			around(w, r, h.fake)
			return
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
		h.checkCompletedFetchPeriods()
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

// coreTxn returns the core row of a fake transaction, and false when the ledger has none.
func (h *harness) coreTxn(account, id string) (store.Transaction, bool) {
	h.t.Helper()
	c, err := store.NewCore(h.st.DB(), "core")
	if err != nil {
		h.t.Fatal(err)
	}
	tx, err := c.GetTransaction(context.Background(), ledger.TransactionID(ledger.AccountID(Source, account), Source, id))
	if errors.Is(err, store.ErrNotFound) {
		return store.Transaction{}, false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return tx, true
}

// requireAllLive fails unless every given transaction is in core and none has disappeared.
func (h *harness) requireAllLive(account string, txs []monzotest.Transaction) {
	h.t.Helper()
	for _, x := range txs {
		tx, ok := h.coreTxn(account, x.ID)
		if !ok {
			h.t.Fatalf("transaction %s is not in core", x.ID)
		}
		if tx.DisappearedAt != nil {
			h.t.Fatalf("transaction %s was marked disappeared although the bank still holds it", x.ID)
		}
	}
}

// reconciliation returns an account's reconciliation row.
func (h *harness) reconciliation(account string) store.Reconciliation {
	h.t.Helper()
	c, err := store.NewCore(h.st.DB(), "core")
	if err != nil {
		h.t.Fatal(err)
	}
	r, err := c.GetReconciliation(context.Background(), ledger.AccountID(Source, account))
	if err != nil {
		h.t.Fatalf("reconciliation of %s: %v", account, err)
	}
	return r
}

// requireReconciled fails unless the account's last check passed with no difference.
func (h *harness) requireReconciled(account string) {
	h.t.Helper()
	r := h.reconciliation(account)
	if r.LastCheckOK == nil || !*r.LastCheckOK || r.LastDiffMinor == nil || *r.LastDiffMinor != 0 {
		h.t.Fatalf("%s not reconciled: ok %v diff %v", account, r.LastCheckOK, r.LastDiffMinor)
	}
}

// checkCompletedFetchPeriods enforces, after every scenario, the invariant the ledger relies on:
// a fetch with a raw.fetch_complete covers exactly what it listed. Its PeriodStart is the since of
// its first page and its PeriodEnd the before of every page, and no page went by id.
func (h *harness) checkCompletedFetchPeriods() {
	h.t.Helper()
	ctx := context.Background()
	for _, r := range h.rawRows() {
		if r.Kind != store.RawFetchComplete {
			continue
		}
		f, err := store.GetFetch(ctx, h.st.DB(), r.FetchComplete.FetchID)
		if err != nil {
			h.t.Errorf("completed fetch: %v", err)
			return
		}
		if f.PeriodStart == nil || f.PeriodEnd == nil {
			h.t.Errorf("completed fetch %d has no period", f.ID)
			continue
		}
		resps, err := store.APIResponsesForFetch(ctx, h.st.DB(), f.ID)
		if err != nil {
			h.t.Errorf("responses: %v", err)
			return
		}
		first := true
		for _, a := range resps {
			if a.Endpoint != pathTransactions || a.Seq > r.Seq {
				continue
			}
			var p map[string]any
			if err := json.Unmarshal(a.Params, &p); err != nil {
				h.t.Errorf("params: %v", err)
				return
			}
			since, _ := p["since"].(string)
			if _, isTime := parseTime(since); !isTime {
				h.t.Errorf("completed fetch %d stepped by id", f.ID)
			}
			if first && since != formatTime(*f.PeriodStart) {
				h.t.Errorf("completed fetch %d starts %v but first listed since %s", f.ID, *f.PeriodStart, since)
			}
			if p["before"] != formatTime(*f.PeriodEnd) {
				h.t.Errorf("completed fetch %d ends %v but listed before %v", f.ID, *f.PeriodEnd, p["before"])
			}
			first = false
		}
	}
}

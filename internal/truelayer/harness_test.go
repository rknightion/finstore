package truelayer

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry"
	"github.com/rknightion/finstore/internal/testpg"
	"github.com/rknightion/finstore/internal/truelayertest"
)

// Made-up bank references for the fixtures.
const (
	refJoint    = "112233 12345678"
	refCurrent  = "112233 87654321"
	refPersonal = "223344 11111111"
	refMonzoJnt = "223344 22222222"
	refPot      = "223344 33333333"
)

func date(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// harness is one scenario: a private database, the fake TrueLayer, the real ledger with both
// TrueLayer normalisers, and workers whose clock is the fake's.
type harness struct {
	t      *testing.T
	fake   *truelayertest.Fake
	srv    *httptest.Server
	st     *store.Store
	ledger *ledger.Ledger
	reader *sdkmetric.ManualReader
	m      *telemetry.Metrics
	loc    *time.Location
	// tokenDelay, when set, holds every token response for that long in real time, so concurrent
	// refreshes overlap.
	tokenDelay atomic.Int64
	// onRequest, when set, sees each request first; returning true means it answered (or dropped)
	// the request itself.
	onRequest atomic.Pointer[func(w http.ResponseWriter, r *http.Request) bool]
	// logs records every log line the workers write.
	logs *logRecorder
}

// logRecorder is a slog handler that keeps the level and message of every record.
type logRecorder struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (l *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	l.recs = append(l.recs, r)
	l.mu.Unlock()
	return nil
}
func (l *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logRecorder) WithGroup(string) slog.Handler      { return l }

// errors returns the messages logged at ERROR.
func (l *logRecorder) errors() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, r := range l.recs {
		if r.Level >= slog.LevelError {
			out = append(out, r.Message)
		}
	}
	return out
}

// holdWriterLock takes the writer lock in the background and returns once it is held; the returned
// function releases it.
func (h *harness) holdWriterLock() func() {
	h.t.Helper()
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_ = h.st.HoldWriterLock(context.Background(), func(context.Context, *pgx.Conn) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	var once sync.Once
	rel := func() { once.Do(func() { close(release); <-done }) }
	h.t.Cleanup(rel)
	return rel
}

// runs returns the source's runs of kind, oldest first.
func (h *harness) runs(source, kind string) []store.Run {
	h.t.Helper()
	rows, err := h.st.DB().Query(context.Background(), `SELECT id FROM sync.run WHERE source = $1 AND kind = $2 ORDER BY id`, source, kind)
	if err != nil {
		h.t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		h.t.Fatal(err)
	}
	var out []store.Run
	for _, id := range ids {
		r, err := store.GetRun(context.Background(), h.st.DB(), id)
		if err != nil {
			h.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func newHarness(t *testing.T, fakeOpts ...truelayertest.Option) *harness {
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
	h := &harness{t: t, fake: truelayertest.New(fakeOpts...), st: st, loc: loc, logs: &logRecorder{}}
	h.ledger = ledger.New(map[string]ledger.SourceNormaliser{
		Source(ConnLloyds): Normaliser{Connection: ConnLloyds},
		Source(ConnMonzo):  Normaliser{Connection: ConnMonzo},
	}, nil, loc, nil)
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d := h.tokenDelay.Load(); d > 0 && r.URL.Path == truelayertest.TokenPath {
			time.Sleep(time.Duration(d))
		}
		if hook := h.onRequest.Load(); hook != nil && (*hook)(w, r) {
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
	t.Cleanup(func() {
		if v := h.fake.Violations(); len(v) != 0 {
			t.Errorf("the fake saw %d requests that were not a GET or a token POST: %+v", len(v), v)
		}
	})
	return h
}

// lloydsFixture is a joint account with history from 2024-08-22, a same-day refund pair, a
// standing order and a direct debit, and a current account with nothing on it.
func (h *harness) lloydsFixture() {
	f := h.fake
	f.AddAccount(ConnLloyds, truelayertest.Account{ID: "acc-joint", Type: "TRANSACTION", DisplayName: "Joint",
		SortCode: "11-22-33", Number: "12345678", Opening: 50000})
	f.AddAccount(ConnLloyds, truelayertest.Account{ID: "acc-current", Type: "TRANSACTION", DisplayName: "Classic",
		SortCode: "112233", Number: "87654321"})
	for _, x := range []struct {
		d      time.Time
		amount int64
		desc   string
	}{
		{date(2024, 8, 22), -1230, "MADE UP SHOP"},
		{date(2024, 9, 1), 250000, "MADE UP SALARY"},
		{date(2024, 9, 1), -45000, "MADE UP MORTGAGE"},
		{date(2025, 3, 10), 3000, "MADE UP REFUND"},
		{date(2025, 3, 10), -3000, "MADE UP REFUND REVERSED"},
		{date(2026, 11, 2), -999, "MADE UP CAFE"},
	} {
		f.AddTransaction(ConnLloyds, "acc-joint", truelayertest.Transaction{Date: x.d, Amount: x.amount, Description: x.desc})
	}
	f.AddStandingOrder(ConnLloyds, "acc-joint", truelayertest.StandingOrder{Payee: "MADE UP SAVER", Reference: "MADE UP REF",
		Frequency: "MONTHLY", Status: "Active", FirstDate: date(2025, 1, 5), FirstAmount: 10000, NextDate: date(2026, 12, 5), NextAmount: 10000})
	f.AddDirectDebit(ConnLloyds, "acc-joint", truelayertest.DirectDebit{Name: "MADE UP ENERGY", Status: "Active",
		MandateID: "MANDATE-0001", PreviousDate: date(2026, 10, 15), PreviousAmount: -6000})
}

// monzoFixture is a personal and a joint account, each with mandates, and a pot whose mandates must
// never be asked for.
func (h *harness) monzoFixture() {
	f := h.fake
	f.AddAccount(ConnMonzo, truelayertest.Account{ID: "mz-personal", Type: "TRANSACTION", DisplayName: "Personal",
		SortCode: "223344", Number: "11111111"})
	f.AddAccount(ConnMonzo, truelayertest.Account{ID: "mz-joint", Type: "TRANSACTION", DisplayName: "Joint",
		SortCode: "223344", Number: "22222222"})
	f.AddAccount(ConnMonzo, truelayertest.Account{ID: "mz-pot", Type: "SAVINGS", DisplayName: "Pot",
		SortCode: "223344", Number: "33333333"})
	f.AddStandingOrder(ConnMonzo, "mz-personal", truelayertest.StandingOrder{Payee: "MADE UP LANDLORD", Reference: "RENT",
		Frequency: "MONTHLY", Status: "Active", NextDate: date(2026, 12, 1), NextAmount: 90000})
	f.AddDirectDebit(ConnMonzo, "mz-personal", truelayertest.DirectDebit{Name: "MADE UP PHONE", Status: "Active",
		PreviousDate: date(2026, 10, 20), PreviousAmount: -1500})
	f.AddDirectDebit(ConnMonzo, "mz-joint", truelayertest.DirectDebit{Name: "MADE UP COUNCIL", Status: "Active", MandateID: "MANDATE-0002"})
	f.AddStandingOrder(ConnMonzo, "mz-pot", truelayertest.StandingOrder{Payee: "NEVER ASKED FOR", Frequency: "WEEKLY", Status: "Active"})
}

// projector runs the real ledger.
type projector struct{ l *ledger.Ledger }

func (p projector) CatchUp(ctx context.Context, q store.DBTX, schema string) (ledger.Stats, error) {
	return p.l.CatchUp(ctx, q, schema)
}

func (h *harness) worker(conn string, mod func(*Options)) *Worker {
	h.t.Helper()
	o := Options{
		Connection: conn, ClientID: truelayertest.DefaultClientID, ClientSecret: truelayertest.DefaultClientSecret,
		AuthURL: h.srv.URL, APIURL: h.srv.URL, PublicURL: "https://finstore.example.test",
		Location: h.loc, Now: h.fake.Now,
		Sleep: func(ctx context.Context, d time.Duration) error { h.fake.Advance(d); return ctx.Err() },
	}
	if mod != nil {
		mod(&o)
	}
	w, err := New(o, h.st, projector{h.ledger}, h.m, slog.New(h.logs))
	if err != nil {
		h.t.Fatalf("New: %v", err)
	}
	h.t.Cleanup(w.Close)
	return w
}

// connect runs the auth link, the bank's approval and the callback, and waits for the connect-time
// fetch.
func (h *harness) connect(w *Worker) {
	h.t.Helper()
	ctx := context.Background()
	link, err := w.AuthoriseURL(ctx)
	if err != nil {
		h.t.Fatalf("AuthoriseURL: %v", err)
	}
	code, state, err := h.fake.Authorise(link)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := w.HandleCallback(ctx, state, code); err != nil {
		h.t.Fatalf("HandleCallback: %v", err)
	}
	w.Wait()
}

func (h *harness) consent(source string) store.Consent {
	h.t.Helper()
	c, err := store.CurrentConsent(context.Background(), h.st.DB(), source)
	if err != nil {
		h.t.Fatalf("CurrentConsent: %v", err)
	}
	return c
}

func (h *harness) core() store.Core {
	h.t.Helper()
	c, err := store.NewCore(h.st.DB(), store.SchemaCore)
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

// lastRun returns the source's newest run.
func (h *harness) lastRun(source string) store.Run {
	h.t.Helper()
	var id int64
	if err := h.st.DB().QueryRow(context.Background(), `SELECT max(id) FROM sync.run WHERE source = $1`, source).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	r, err := store.GetRun(context.Background(), h.st.DB(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// requireRun fails unless run r has the outcome and error category given ("" for none).
func requireRun(t *testing.T, r store.Run, kind, outcome, cat string) {
	t.Helper()
	got := ""
	if r.ErrorCategory != nil {
		got = *r.ErrorCategory
	}
	if r.Kind != kind || r.Outcome == nil || *r.Outcome != outcome || got != cat || r.FinishedAt == nil {
		t.Fatalf("run %d: kind %s outcome %v category %q finished %v; want %s %s %q", r.ID, r.Kind, r.Outcome, got,
			r.FinishedAt, kind, outcome, cat)
	}
}

// requests returns the fake's log from index from on.
func (h *harness) requests(from int) []truelayertest.Request {
	return h.fake.Requests()[from:]
}

// txn returns the core row of a Lloyds transaction by its normalised provider id.
func (h *harness) txn(key string) (store.Transaction, bool) {
	h.t.Helper()
	acc := ledger.AccountID(Source(ConnLloyds), refJoint)
	tx, err := h.core().GetTransaction(context.Background(), ledger.TransactionID(acc, Source(ConnLloyds), key))
	if errors.Is(err, store.ErrNotFound) {
		return store.Transaction{}, false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return tx, true
}

// counter returns the sum of a counter's points whose attributes include attrs.
func (h *harness) counter(name string, attrs ...attribute.KeyValue) int64 {
	h.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		h.t.Fatal(err)
	}
	var n int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
		points:
			for _, dp := range sum.DataPoints {
				for _, a := range attrs {
					if v, ok := dp.Attributes.Value(a.Key); !ok || v != a.Value {
						continue points
					}
				}
				n += dp.Value
			}
		}
	}
	return n
}

func (h *harness) syncRuns(outcome, cat string) int64 {
	attrs := []attribute.KeyValue{attribute.String(semconv.AttrOutcome, outcome)}
	if cat != "" {
		attrs = append(attrs, attribute.String(semconv.AttrErrorCategory, cat))
	}
	return h.counter(semconv.MetricSyncRuns, attrs...)
}

// addMonzoAccounts puts the Monzo accounts Monzo's own API would have created into core, with their
// bank references.
func (h *harness) addMonzoAccounts() map[string]uuid.UUID {
	h.t.Helper()
	out := map[string]uuid.UUID{}
	for slug, ref := range map[string]string{"monzo-current": refPersonal, "monzo-joint": refMonzoJnt, "monzo-pot": refPot} {
		r := ref
		acc := store.Account{ID: ledger.AccountID("monzo", "acc_"+slug), Source: "monzo", SourceKey: "acc_" + slug, Slug: slug,
			Type: "current", Feed: "api", Currency: "GBP", Name: "made-up", BankRef: &r}
		if err := h.core().UpsertAccount(context.Background(), acc); err != nil {
			h.t.Fatal(err)
		}
		out[ref] = acc.ID
	}
	return out
}

func params(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// tokenCalls counts POSTs to the token path with grant_type grant.
func (h *harness) tokenCalls() int {
	n := 0
	for _, r := range h.fake.Requests() {
		if r.Method == http.MethodPost && r.Path == truelayertest.TokenPath {
			n++
		}
	}
	return n
}

package monzo

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/rknightion/finstore/internal/consent"
	"github.com/rknightion/finstore/internal/monzotest"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
)

// parkSleeps makes every approval pause wait until the worker stops, so a test's assertions are
// not raced by a background approval wait.
func (h *harness) parkSleeps() {
	h.setOnSleep(func(ctx context.Context, _ int, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	})
}

// stateFor starts an attempt and registers a fresh code, returning both.
func (h *harness) stateFor(w *Worker) (state, code string) {
	h.t.Helper()
	u, err := w.AuthoriseURL(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	p, err := url.Parse(u)
	if err != nil {
		h.t.Fatal(err)
	}
	h.mu.Lock()
	h.codes++
	code = "code_test_r" + string(rune('a'+h.codes))
	h.mu.Unlock()
	h.fake.RegisterCode(code)
	return p.Query().Get("state"), code
}

// storedTokenWorks asks the bank, outside the worker, whether the stored access token is its
// current one.
func (h *harness) storedTokenWorks() bool {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/ping/whoami", nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token().AccessToken)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// waitFor polls cond for up to limit and reports whether it became true.
func waitFor(limit time.Duration, cond func() bool) bool {
	end := time.Now().Add(limit)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func TestResumedRangeDoesNotMarkItsEarlierRowsDisappeared(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, func(o *Options) { o.Concurrency = 1 })
	all := slowHistory(h)
	// The first connect is cut short by the deadline, leaving a range with a cursor.
	h.connect(func() { h.fake.WithDelay(10 * time.Second) })
	resumable := false
	for _, w := range h.windows("acc_test_slow") {
		if w.CompletedAt == nil && w.Cursor != nil {
			resumable = true
		}
	}
	if !resumable {
		t.Fatal("the first connect left no range with a cursor; the scenario does not resume")
	}
	h.fake.WithDelay(0)
	h.fake.Advance(time.Hour)
	h.connect(nil)

	for _, w := range h.windows("acc_test_slow") {
		if w.CompletedAt == nil {
			t.Fatalf("range %v unfinished after an unhurried second connect", w.PeriodStart)
		}
	}
	// The real projection: nothing the bank holds disappeared, and the balance check passes.
	h.requireAllLive("acc_test_slow", all)
	h.requireReconciled("acc_test_slow")
}

// raceSetup is a connected harness, near token expiry, with approval pauses parked and the state
// and code for a reconnect ready.
func raceSetup(t *testing.T) (h *harness, oldConsent store.Consent, state, code string) {
	h = connected(t, nil)
	oldConsent = h.consent()
	h.parkSleeps()
	h.nearExpiry()
	state, code = h.stateFor(h.w)
	return h, oldConsent, state, code
}

// race runs a refresh and, while the refresh's token request is held by hold, a reconnect. It
// lets the reconnect's exchange reach the bank if nothing stops it, then releases the refresh.
func race(t *testing.T, h *harness, state, code string, hold func(w http.ResponseWriter, r *http.Request, serve http.Handler, arrived chan<- struct{}, release <-chan struct{})) {
	t.Helper()
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var first sync.Once
	h.mu.Lock()
	h.around = func(w http.ResponseWriter, r *http.Request, serve http.Handler) {
		held := false
		if r.URL.Path == monzotest.TokenPath {
			first.Do(func() { held = true })
		}
		if held {
			hold(w, r, serve, arrived, release)
			return
		}
		serve.ServeHTTP(w, r)
	}
	h.mu.Unlock()

	tokensBefore := h.fake.Count(monzotest.TokenPath)
	var wg sync.WaitGroup
	wg.Go(func() { _, _ = h.w.tokens.access(context.Background()) })
	<-arrived
	var cbErr error
	wg.Go(func() { cbErr = h.w.HandleCallback(context.Background(), state, code) })
	// Unprotected, the exchange reaches the bank while the refresh is held; protected, it waits.
	waitFor(2*time.Second, func() bool { return h.fake.Count(monzotest.TokenPath) > tokensBefore+1 })
	once.Do(func() { close(release) })
	wg.Wait()
	if cbErr != nil {
		t.Fatalf("HandleCallback: %v", cbErr)
	}
}

func TestRefreshRejectedAfterAReconnectDoesNotEndTheNewConsent(t *testing.T) {
	t.Parallel()
	h, old, state, code := raceSetup(t)
	// The refresh is held before the bank sees it, so an exchange that is not kept out runs first
	// and spends the refresh token.
	race(t, h, state, code, func(w http.ResponseWriter, r *http.Request, serve http.Handler, arrived chan<- struct{}, release <-chan struct{}) {
		close(arrived)
		<-release
		serve.ServeHTTP(w, r)
	})
	c := h.consent()
	if c.ID == old.ID || c.State != store.ConsentAwaitingApproval {
		t.Fatalf("current consent %d %s, want the new consent awaiting approval", c.ID, c.State)
	}
	if !h.storedTokenWorks() {
		t.Fatal("the stored access token is not the bank's current one")
	}
}

func TestRefreshAnsweredAfterAReconnectDoesNotOverwriteTheNewPair(t *testing.T) {
	t.Parallel()
	h, old, state, code := raceSetup(t)
	// The bank processes the refresh, but its answer is held until after the reconnect could run.
	race(t, h, state, code, func(w http.ResponseWriter, r *http.Request, serve http.Handler, arrived chan<- struct{}, release <-chan struct{}) {
		rec := httptest.NewRecorder()
		serve.ServeHTTP(rec, r)
		close(arrived)
		<-release
		maps.Copy(w.Header(), rec.Header())
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
	if c := h.consent(); c.ID == old.ID || c.State != store.ConsentAwaitingApproval {
		t.Fatalf("current consent %d %s, want the new consent awaiting approval", c.ID, c.State)
	}
	if !h.storedTokenWorks() {
		t.Fatal("the stored access token is not the bank's current one: a stale refresh overwrote the new pair")
	}
}

func TestRateLimitedRefreshKeepsTheConsentAndThePair(t *testing.T) {
	t.Parallel()
	h := connected(t, nil)
	old := h.token()
	h.nearExpiry()
	h.fake.Fail429(monzotest.TokenPath, 7)
	err := h.w.RunScheduled(context.Background(), KindHourly)
	if err == nil || errors.Is(err, ErrConsentLost) {
		t.Fatalf("RunScheduled = %v, want a transient failure", err)
	}
	if c := h.consent(); c.State != store.ConsentActive {
		t.Fatalf("consent %s after a rate-limited refresh, want active", c.State)
	}
	if tok := h.token(); tok.RefreshToken != old.RefreshToken {
		t.Fatal("the stored pair changed")
	}
	if err := h.w.RunScheduled(context.Background(), KindHourly); err != nil {
		t.Fatalf("run after the rate limit cleared: %v", err)
	}
}

func TestCallbackCancelledAsTheTokenResponseArrivesStillStoresThePairAndConsent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	h.parkSleeps()
	state, code := h.stateFor(h.w)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.mu.Lock()
	h.around = func(w http.ResponseWriter, r *http.Request, serve http.Handler) {
		if r.URL.Path != monzotest.TokenPath {
			serve.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		serve.ServeHTTP(rec, r)
		// The browser goes away just as the bank has issued the pair.
		cancel()
		time.Sleep(100 * time.Millisecond)
		maps.Copy(w.Header(), rec.Header())
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}
	h.mu.Unlock()
	if err := h.w.HandleCallback(ctx, state, code); err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	if !h.storedTokenWorks() {
		t.Fatal("the issued pair was not stored")
	}
	if c := h.consent(); c.State != store.ConsentAwaitingApproval {
		t.Fatalf("consent %s, want awaiting approval", c.State)
	}
}

func TestRangesArePlannedOnAFixedGridSoALaterRunFindsTheSameRows(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	created := h.fake.Now().AddDate(-1, 0, 0)
	h.fake.AddAccount(monzotest.Account{ID: "acc_test_1", Type: "uk_retail", Created: created})
	h.fake.GenerateTransactions("acc_test_1", 200, created, h.fake.Now().Add(-time.Hour), 1)
	h.connect(nil)
	first := h.windows("acc_test_1")
	h.fake.Advance(36 * time.Hour)
	h.connect(nil)
	second := h.windows("acc_test_1")
	if len(second) != len(first) {
		t.Fatalf("%d ranges after the second run, %d after the first: a run planned a new row for an existing slot", len(second), len(first))
	}
	starts := map[time.Time]bool{}
	for _, w := range second {
		if starts[w.PeriodStart] {
			t.Fatalf("two ranges start at %v", w.PeriodStart)
		}
		starts[w.PeriodStart] = true
		if w.PeriodEnd.Sub(w.PeriodStart) != RangeLength {
			t.Fatalf("range %v..%v is not one grid slot", w.PeriodStart, w.PeriodEnd)
		}
		if w.CompletedAt == nil {
			t.Fatalf("range %v unfinished", w.PeriodStart)
		}
	}
	// The fetch of the last slot ends at the before it used, never the slot's future end.
	for _, r := range h.rawRows() {
		if r.Kind == store.RawFetch && r.Fetch.PeriodEnd != nil && r.Fetch.PeriodEnd.After(r.Fetch.StartedAt) {
			t.Fatalf("fetch %d's period ends at %v, after it started at %v", r.Fetch.ID, *r.Fetch.PeriodEnd, r.Fetch.StartedAt)
		}
	}
}

// addOnFirstBalance adds a transaction created at the moment the first /balance request reaches
// the bank, and returns it once added.
func addOnFirstBalance(h *harness, account string) func() monzotest.Transaction {
	var once sync.Once
	var added monzotest.Transaction
	h.mu.Lock()
	h.onRequest = func(r *http.Request) {
		if r.URL.Path == pathBalance {
			once.Do(func() {
				added = h.fake.AddTransaction(monzotest.Transaction{AccountID: account, Amount: -500,
					Description: "Late Purchase", Settled: h.fake.Now().Format(time.RFC3339Nano)})
			})
		}
	}
	h.mu.Unlock()
	return func() monzotest.Transaction { return added }
}

func TestTransactionCreatedBeforeTheOpeningBalanceIsFetchedInTheSameScheduledRun(t *testing.T) {
	t.Parallel()
	h := connected(t, nil)
	h.fake.Advance(time.Hour)
	h.fake.WithDelay(time.Second)
	added := addOnFirstBalance(h, "acc_test_1")
	if err := h.w.RunScheduled(context.Background(), KindHourly); err != nil {
		t.Fatal(err)
	}
	tx := added()
	if tx.ID == "" {
		t.Fatal("no balance was read")
	}
	if !storedTxIDs(t, h)[tx.ID] {
		t.Fatal("a transaction the opening balance includes was not fetched in the same run")
	}
	h.requireReconciled("acc_test_1")
}

func TestTransactionCreatedBeforeTheOpeningBalanceIsFetchedInTheFullHistoryFetch(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	created := h.fake.Now().AddDate(0, -2, 0)
	h.fake.AddAccount(monzotest.Account{ID: "acc_test_1", Type: "uk_retail", Created: created})
	h.fake.GenerateTransactions("acc_test_1", 50, created, h.fake.Now().Add(-time.Hour), 1)
	var added func() monzotest.Transaction
	h.connect(func() {
		h.fake.WithDelay(time.Second)
		added = addOnFirstBalance(h, "acc_test_1")
	})
	tx := added()
	if tx.ID == "" {
		t.Fatal("no balance was read")
	}
	if !storedTxIDs(t, h)[tx.ID] {
		t.Fatal("a transaction the opening balance includes was not fetched in the full-history fetch")
	}
	h.requireReconciled("acc_test_1")
}

// restoredConsent records an active consent with no token row, as a database restore leaves it.
func restoredConsent(t *testing.T, h *harness, approve bool) store.Consent {
	t.Helper()
	ctx := context.Background()
	m := consent.New(Source, h.fake.Now)
	state, err := m.StartAttempt(ctx, h.st.DB())
	if err != nil {
		t.Fatal(err)
	}
	a, err := m.ConsumeAttempt(ctx, h.st.DB(), state)
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.Exchanged(ctx, h.st.DB(), a.ID, h.fake.Now())
	if err != nil {
		t.Fatal(err)
	}
	if approve {
		if c, err = m.Approved(ctx, h.st.DB(), c.ID); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func TestCurrentConsentWithNoStoredTokenIsLostOnTheNextScheduledRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	h.fake.AddAccount(monzotest.Account{ID: "acc_test_1", Type: "uk_retail", Created: h.fake.Now().AddDate(0, -1, 0)})
	restored := restoredConsent(t, h, true)

	err := h.w.RunScheduled(context.Background(), KindHourly)
	if !errors.Is(err, ErrConsentLost) {
		t.Fatalf("RunScheduled = %v, want ErrConsentLost", err)
	}
	c := h.consent()
	if c.ID != restored.ID || c.State != store.ConsentLost || c.EndReason == nil || *c.EndReason != string(consent.ReasonTokenMissing) {
		t.Fatalf("consent %d %s %v, want lost token_missing", c.ID, c.State, c.EndReason)
	}
	if v, ok := h.gauge(semconv.MetricConsentState, attribute.String(semconv.AttrSource, Source),
		attribute.String(semconv.AttrState, store.ConsentLost)); !ok || v != 1 {
		t.Fatalf("consent state metric for lost = %d %v, want 1", v, ok)
	}
	if n := len(h.fake.Requests()); n != 0 {
		t.Fatalf("%d requests with no token", n)
	}

	// A fresh connect afterwards works.
	h.connect(nil)
	if c := h.consent(); c.ID == restored.ID || c.State != store.ConsentActive {
		t.Fatalf("consent after reconnect %d %s, want a new active one", c.ID, c.State)
	}
	if err := h.w.RunScheduled(context.Background(), KindHourly); err != nil {
		t.Fatalf("run after reconnect: %v", err)
	}
}

func TestResumeEndsAConsentAwaitingApprovalWithNoStoredToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	restored := restoredConsent(t, h, false)
	if err := h.w.Resume(context.Background()); !errors.Is(err, ErrConsentLost) {
		t.Fatalf("Resume = %v, want ErrConsentLost", err)
	}
	h.w.Wait()
	c := h.consent()
	if c.ID != restored.ID || c.State != store.ConsentLost || *c.EndReason != string(consent.ReasonTokenMissing) {
		t.Fatalf("consent %s %v, want lost token_missing", c.State, c.EndReason)
	}
	if n := len(h.fake.Requests()); n != 0 {
		t.Fatalf("%d requests with no token", n)
	}
}

func TestScheduledRunThatStepsByIDWritesNoFetchCompleteAndIsPartial(t *testing.T) {
	t.Parallel()
	h := connected(t, nil)
	h.fake.Advance(time.Hour)
	// 150 rows share one instant inside the hourly period: more than a page.
	at := h.fake.Now().Add(-48 * time.Hour)
	h.fake.GenerateTransactions("acc_test_1", 150, at, at.Add(time.Second), 150)
	if err := h.w.RunScheduled(context.Background(), KindHourly); err != nil {
		t.Fatal(err)
	}
	for _, r := range h.rawRows() {
		if r.Kind == store.RawFetch && r.Fetch.Kind == KindHourly && r.Fetch.PeriodStart != nil {
			if done, err := store.FetchCompleted(context.Background(), h.st.DB(), r.Fetch.ID); err != nil || done {
				t.Fatalf("a scheduled fetch that skipped rows has a completion: %v %v", done, err)
			}
		}
	}
	run := h.lastRun(KindHourly)
	if run.Outcome == nil || *run.Outcome != outcomePartial || run.ErrorCategory == nil || *run.ErrorCategory != categoryTimestampGroup {
		t.Fatalf("run %v %v, want partial %s", run.Outcome, run.ErrorCategory, categoryTimestampGroup)
	}
}

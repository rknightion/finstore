package monzo

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/rknightion/finstore/internal/consent"
	"github.com/rknightion/finstore/internal/monzotest"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
)

func TestRateLimitedPageIsRetriedWithAGrowingPauseAndTheRunSucceeds(t *testing.T) {
	t.Parallel()
	h := connected(t, nil)
	from := h.sleepCount()
	h.fake.Fail429(pathTransactions, 3)
	if err := h.w.RunScheduled(context.Background(), KindHourly); err != nil {
		t.Fatalf("RunScheduled: %v", err)
	}
	if got, want := h.sleepsFrom(from), []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}; !slices.Equal(got, want) {
		t.Fatalf("pauses %v, want %v", got, want)
	}
	run := h.lastRun(KindHourly)
	if run.Outcome == nil || *run.Outcome != outcomeOK {
		t.Fatalf("run outcome %v, want ok", run.Outcome)
	}
	c := h.consent()
	if c.LastSyncOKAt == nil {
		t.Fatal("last_sync_ok_at not set after a successful run")
	}
	if v, ok := h.gauge(semconv.MetricSyncLastSuccessTime, attribute.String(semconv.AttrSource, Source)); !ok || v != c.LastSyncOKAt.Unix() {
		t.Fatalf("last success metric = %d %v, want %d", v, ok, c.LastSyncOKAt.Unix())
	}
	if v, ok := h.gauge(semconv.MetricConsentState, attribute.String(semconv.AttrSource, Source),
		attribute.String(semconv.AttrState, store.ConsentActive)); !ok || v != 1 {
		t.Fatalf("consent state metric for active = %d %v, want 1", v, ok)
	}
	if v, ok := h.gauge(semconv.MetricConsentExpiryTime, attribute.String(semconv.AttrSource, Source)); !ok || v != c.ExpectedExpiryAt.Unix() {
		t.Fatalf("consent expiry metric = %d %v", v, ok)
	}
}

func TestScheduledRunFetchesItsPeriodForOpenAccountsAndCompletesTheFetch(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	now := h.fake.Now()
	h.fake.AddAccount(monzotest.Account{ID: "acc_test_open", Type: "uk_retail", Created: now.AddDate(-1, 0, 0)})
	h.fake.AddAccount(monzotest.Account{ID: "acc_test_closed", Type: "uk_prepaid", Created: now.AddDate(-3, 0, 0), Closed: true})
	h.fake.GenerateTransactions("acc_test_open", 250, now.AddDate(0, 0, -100), now.Add(-time.Minute), 1)
	h.connect(nil)
	h.fake.Advance(time.Hour)
	from := len(h.fake.Requests())

	for _, tc := range []struct {
		kind   string
		period time.Duration
	}{{KindHourly, HourlyPeriod}, {KindNightly, NightlyPeriod}} {
		if err := h.w.RunScheduled(context.Background(), tc.kind); err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		var fetch *store.Fetch
		var complete bool
		for _, r := range h.rawRows() {
			if r.Kind == store.RawFetch && r.Fetch.Kind == tc.kind && r.Fetch.PeriodStart != nil {
				fetch = r.Fetch
			}
			if r.Kind == store.RawFetchComplete && fetch != nil && r.FetchComplete.FetchID == fetch.ID {
				complete = true
			}
		}
		if fetch == nil || *fetch.AccountKey != "acc_test_open" || fetch.PeriodEnd.Sub(*fetch.PeriodStart) != tc.period {
			t.Fatalf("%s fetch = %+v", tc.kind, fetch)
		}
		if !complete {
			t.Fatalf("%s fetch has no completion although every page arrived", tc.kind)
		}
		responses, err := store.APIResponsesForFetch(context.Background(), h.st.DB(), fetch.ID)
		if err != nil {
			t.Fatal(err)
		}
		var order []string
		for _, r := range responses {
			order = append(order, r.Endpoint)
		}
		if order[0] != pathTransactions || order[len(order)-2] != pathBalance || order[len(order)-1] != pathPots {
			t.Fatalf("%s period fetch responses in order %v, want transactions, balance, pots", tc.kind, order)
		}
		// The period is exactly what was listed: since of the first page, before of every page.
		for i, r := range responses {
			if r.Endpoint != pathTransactions {
				continue
			}
			var p map[string]any
			if err := json.Unmarshal(r.Params, &p); err != nil {
				t.Fatal(err)
			}
			if i == 0 && p["since"] != formatTime(*fetch.PeriodStart) {
				t.Fatalf("%s first page since %v, fetch period starts %v", tc.kind, p["since"], *fetch.PeriodStart)
			}
			if p["before"] != formatTime(*fetch.PeriodEnd) {
				t.Fatalf("%s page before %v, fetch period ends %v", tc.kind, p["before"], *fetch.PeriodEnd)
			}
		}
		// The opening balance is under the run's period-less fetch for the account, read first.
		var opening *store.Fetch
		for _, r := range h.rawRows() {
			if r.Kind == store.RawFetch && r.Fetch.Kind == tc.kind && r.Fetch.RunID == fetch.RunID &&
				r.Fetch.AccountKey != nil && r.Fetch.PeriodStart == nil {
				opening = r.Fetch
			}
		}
		if opening == nil || opening.ID > fetch.ID {
			t.Fatalf("%s: no opening balance fetch before the period fetch", tc.kind)
		}
	}
	for _, r := range h.fake.Requests()[from:] {
		if r.Query.Get("account_id") == "acc_test_closed" || r.Query.Get("current_account_id") == "acc_test_closed" {
			t.Fatalf("scheduled run read the closed account: %s", r.Path)
		}
	}
	if calls := h.proj.calls(); len(calls) != 3 {
		t.Fatalf("projector called %d times, want once per run", len(calls))
	}
}

func TestScheduledRunCutShortWritesNoFetchComplete(t *testing.T) {
	t.Parallel()
	h := connected(t, nil)
	h.fake.Advance(time.Hour)
	// Every attempt at a page fails, retries included.
	h.fake.FailNonJSON(pathTransactions, 4)
	if err := h.w.RunScheduled(context.Background(), KindHourly); err != nil {
		t.Fatalf("RunScheduled: %v", err)
	}
	for _, r := range h.rawRows() {
		if r.Kind == store.RawFetch && r.Fetch.Kind == KindHourly && r.Fetch.PeriodStart != nil {
			if done, err := store.FetchCompleted(context.Background(), h.st.DB(), r.Fetch.ID); err != nil || done {
				t.Fatalf("fetch cut short has a completion: %v %v", done, err)
			}
		}
	}
	run := h.lastRun(KindHourly)
	if run.Outcome == nil || *run.Outcome != outcomePartial || run.ErrorCategory == nil || *run.ErrorCategory != "server_error" {
		t.Fatalf("run = %v %v, want partial server_error", run.Outcome, run.ErrorCategory)
	}
	if c := h.consent(); c.LastSyncOKAt != nil && c.LastSyncOKAt.After(run.StartedAt) {
		t.Fatal("a partial run set last_sync_ok_at")
	}
}

func TestScheduledRunWithoutActiveConsentDoesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	if err := h.w.RunScheduled(context.Background(), KindHourly); err != nil {
		t.Fatalf("no consent: %v", err)
	}
	// Awaiting approval: the first worker stops in its first pause.
	h.setOnSleep(func(ctx context.Context, _ int, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	})
	h.callback(h.w)
	requests := len(h.fake.Requests())
	if err := h.w.RunScheduled(context.Background(), KindHourly); err != nil {
		t.Fatalf("awaiting approval: %v", err)
	}
	if n := len(h.fake.Requests()); n != requests {
		t.Fatalf("scheduled run made %d requests without an active consent", n-requests)
	}
	if err := h.w.RunScheduled(context.Background(), "weekly"); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestRevokedActiveConsentIsMarkedLostByTheScheduledRun(t *testing.T) {
	t.Parallel()
	h := connected(t, nil)
	h.fake.Revoke()
	err := h.w.RunScheduled(context.Background(), KindHourly)
	if !errors.Is(err, ErrConsentLost) {
		t.Fatalf("RunScheduled = %v, want ErrConsentLost", err)
	}
	c := h.consent()
	if c.State != store.ConsentLost || c.EndReason == nil || *c.EndReason != string(consent.ReasonInsufficientPermissions) {
		t.Fatalf("consent %s %v, want lost insufficient_permissions", c.State, c.EndReason)
	}
	if run := h.lastRun(KindHourly); run.Outcome == nil || *run.Outcome != outcomeFailed {
		t.Fatalf("run outcome %v, want failed", run.Outcome)
	}
	if v, ok := h.gauge(semconv.MetricConsentState, attribute.String(semconv.AttrSource, Source),
		attribute.String(semconv.AttrState, store.ConsentLost)); !ok || v != 1 {
		t.Fatalf("consent state metric for lost = %d %v, want 1", v, ok)
	}
	requests := len(h.fake.Requests())
	if err := h.w.RunScheduled(context.Background(), KindHourly); err != nil {
		t.Fatalf("run after loss: %v", err)
	}
	if n := len(h.fake.Requests()); n != requests {
		t.Fatal("a run after the loss contacted the bank")
	}
}

func TestExpiredConsentStopsScheduledRuns(t *testing.T) {
	t.Parallel()
	h := connected(t, nil)
	h.fake.Advance(consent.Lifetime)
	requests := len(h.fake.Requests())
	if err := h.w.RunScheduled(context.Background(), KindHourly); err != nil {
		t.Fatal(err)
	}
	if c := h.consent(); c.State != store.ConsentExpired {
		t.Fatalf("consent %s after its expected expiry, want expired", c.State)
	}
	if n := len(h.fake.Requests()); n != requests {
		t.Fatal("a run on an expired consent contacted the bank")
	}
}

func TestForbiddenWhileAwaitingApprovalDoesNotLoseTheConsent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, func(o *Options) { o.ApprovalFastFor = 10 * time.Second })
	h.fake.AddAccount(monzotest.Account{ID: "acc_test_1", Type: "uk_retail", Created: h.fake.Now().AddDate(0, -1, 0)})
	var states []string
	h.setOnSleep(func(_ context.Context, n int, _ time.Duration) error {
		c, err := store.CurrentConsent(context.Background(), h.st.DB(), Source)
		if err == nil {
			states = append(states, c.State)
		}
		if n == 7 {
			h.fake.Approve()
		}
		return nil
	})
	h.callback(h.w)
	h.w.Wait()

	for i, s := range states {
		if s != store.ConsentAwaitingApproval {
			t.Fatalf("consent %s at poll pause %d, want awaiting_approval", s, i)
		}
	}
	got := h.sleepsFrom(0)
	want := []time.Duration{2 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second,
		time.Minute, time.Minute, time.Minute}
	if !slices.Equal(got, want) {
		t.Fatalf("poll pauses %v, want %v", got, want)
	}
	if c := h.consent(); c.State != store.ConsentActive {
		t.Fatalf("consent %s after approval, want active", c.State)
	}
	if n := h.fake.Count(pathAccounts); n < 9 {
		t.Fatalf("%d account list requests, want the polls and the fetch's own", n)
	}
}

func TestNoApprovalWithinTheGiveUpTimeLosesTheConsent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, func(o *Options) {
		o.ApprovalFastFor = 20 * time.Second
		o.ApprovalGiveUp = 30 * time.Minute
	})
	h.callback(h.w)
	h.w.Wait()
	c := h.consent()
	if c.State != store.ConsentLost || c.EndReason == nil || *c.EndReason != string(consent.ReasonApprovalTimeout) {
		t.Fatalf("consent %s %v, want lost approval_timeout", c.State, c.EndReason)
	}
	issued, _ := h.fake.TokenIssuedAt()
	if !c.EndedAt.Equal(issued.Add(30 * time.Minute)) {
		t.Fatalf("lost after %v, want 30m", c.EndedAt.Sub(issued))
	}
	if n := h.fake.Count(pathTransactions); n != 0 {
		t.Fatalf("%d transaction requests without approval", n)
	}
	if calls := h.proj.calls(); len(calls) != 0 {
		t.Fatal("projector ran without a fetch")
	}
}

func TestNewConnectReplacesTheConsentAndStopsTheOldApprovalWait(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	h.fake.AddAccount(monzotest.Account{ID: "acc_test_1", Type: "uk_retail", Created: h.fake.Now().AddDate(0, -1, 0)})
	parked := make(chan struct{})
	var parkOnce sync.Once
	h.setOnSleep(func(ctx context.Context, _ int, _ time.Duration) error {
		parkOnce.Do(func() { close(parked) })
		<-ctx.Done()
		return ctx.Err()
	})
	h.callback(h.w)
	first := h.consent()
	// The first wait must be parked in its pause before the hook changes. A first wait that
	// reached its first pause only after connect installed its hook would take connect's approval
	// for the first token, which the second exchange then withdraws, and the second consent would
	// wait unapproved until its give-up time.
	select {
	case <-parked:
	case <-time.After(30 * time.Second):
		t.Fatal("the first approval wait never paused")
	}
	h.setOnSleep(nil)
	h.connect(nil)
	old, err := store.GetConsent(context.Background(), h.st.DB(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.State != store.ConsentReplaced {
		t.Fatalf("first consent %s, want replaced", old.State)
	}
	if c := h.consent(); c.ID == first.ID || c.State != store.ConsentActive {
		t.Fatalf("current consent %d %s, want the new one active", c.ID, c.State)
	}
}

// completionSeqs maps each fetch id to its raw.fetch_complete seq.
func completionSeqs(rows []store.RawRow) map[int64]int64 {
	out := map[int64]int64{}
	for _, r := range rows {
		if r.Kind == store.RawFetchComplete {
			out[r.FetchComplete.FetchID] = r.Seq
		}
	}
	return out
}

func TestFetchCompleteIsWrittenBeforeTheClosingBalanceSoTheCheckSeesDisappearances(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	now := h.fake.Now()
	h.fake.AddAccount(monzotest.Account{ID: "acc_test_1", Type: "uk_retail", Created: now.AddDate(-1, 0, 0)})
	generate(h, "acc_test_1", 900, now.AddDate(-1, 0, 0), now.Add(-time.Minute), 1)
	h.connect(nil)

	// Full-history fetch: every range's completion precedes the account's closing balance.
	rows := h.rawRows()
	done := completionSeqs(rows)
	var lastBalance, lastCompletion int64
	for _, r := range rows {
		if r.Kind == store.RawAPIResponse && r.APIResponse.Endpoint == pathBalance {
			lastBalance = r.Seq
		}
	}
	for _, seq := range done {
		lastCompletion = max(lastCompletion, seq)
	}
	if len(done) == 0 || lastCompletion > lastBalance {
		t.Fatalf("full-history fetch: last completion seq %d, closing balance seq %d", lastCompletion, lastBalance)
	}

	h.fake.Advance(time.Hour)
	if err := h.w.RunScheduled(context.Background(), KindNightly); err != nil {
		t.Fatal(err)
	}
	rows = h.rawRows()
	done = completionSeqs(rows)
	var fetch *store.Fetch
	for _, r := range rows {
		if r.Kind == store.RawFetch && r.Fetch.Kind == KindNightly && r.Fetch.PeriodStart != nil {
			fetch = r.Fetch
		}
	}
	if fetch == nil {
		t.Fatal("no nightly fetch")
	}
	seq, ok := done[fetch.ID]
	if !ok {
		t.Fatal("nightly fetch has no completion")
	}
	responses, err := store.APIResponsesForFetch(context.Background(), h.st.DB(), fetch.ID)
	if err != nil {
		t.Fatal(err)
	}
	var balances, pages int
	for _, r := range responses {
		switch r.Endpoint {
		case pathTransactions:
			pages++
			if r.Seq > seq {
				t.Fatalf("transaction page seq %d after the completion %d", r.Seq, seq)
			}
		case pathBalance:
			balances++
			if r.Seq < seq {
				t.Fatalf("closing balance seq %d before the completion %d: the check would run before disappearances", r.Seq, seq)
			}
		}
	}
	if pages < 2 || balances != 1 {
		t.Fatalf("%d pages and %d closing balances in the nightly period fetch", pages, balances)
	}
	h.requireReconciled("acc_test_1")
}

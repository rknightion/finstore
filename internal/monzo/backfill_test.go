package monzo

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/monzotest"
	"github.com/rknightion/finstore/internal/store"
)

// generate adds n transactions in groups of groupSize spread over [from, to), in chunks of at
// most 30 days. The fake's GenerateTransactions multiplies the span by the group index as a
// time.Duration, which overflows int64 for long spans with many groups.
func generate(h *harness, account string, n int, from, to time.Time, groupSize int) []monzotest.Transaction {
	const chunk = 30 * 24 * time.Hour
	k := max(groupSize, 1)
	chunks := max(int((to.Sub(from)+chunk-1)/chunk), 1)
	groups := (n + k - 1) / k
	var out []monzotest.Transaction
	done := 0
	for c := range chunks {
		g := groups*(c+1)/chunks - groups*c/chunks
		m := min(g*k, n-done)
		if m <= 0 {
			continue
		}
		s := from.Add(time.Duration(c) * chunk)
		e := s.Add(chunk)
		if e.After(to) {
			e = to
		}
		out = append(out, h.fake.GenerateTransactions(account, m, s, e, k)...)
		done += m
	}
	return out
}

// storedTxIDs returns the transaction ids in every stored 2xx /transactions page.
func storedTxIDs(t *testing.T, h *harness) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, r := range h.rawRows() {
		if r.Kind != store.RawAPIResponse || r.APIResponse.Endpoint != pathTransactions || r.APIResponse.Status != 200 {
			continue
		}
		var doc struct {
			Transactions []struct {
				ID string `json:"id"`
			} `json:"transactions"`
		}
		if err := json.Unmarshal(r.APIResponse.Body, &doc); err != nil {
			t.Fatalf("stored page is not JSON: %v", err)
		}
		for _, x := range doc.Transactions {
			out[x.ID] = true
		}
	}
	return out
}

// windowOf finds the stored range a /transactions request was for.
func windowOf(t *testing.T, ws []store.BackfillWindow, r monzotest.Request) store.BackfillWindow {
	t.Helper()
	for _, w := range ws {
		if w.AccountKey == r.Query.Get("account_id") && formatTime(w.PeriodEnd) == r.Query.Get("before") {
			return w
		}
	}
	t.Fatalf("request for account %q before %q matches no stored range", r.Query.Get("account_id"), r.Query.Get("before"))
	return store.BackfillWindow{}
}

func TestBackfillNeverRequestsMoreThan365DaysAndStoresEveryRowOfTimestampGroupsAcrossPages(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	now := h.fake.Now()
	// Created over seven years ago, so the history spans two leap years.
	h.fake.AddAccount(monzotest.Account{ID: "acc_test_current", Type: "uk_retail", Created: now.AddDate(-7, -2, 0)})
	h.fake.AddAccount(monzotest.Account{ID: "acc_test_prepaid", Type: "uk_prepaid", Created: now.AddDate(-9, 0, 0), Closed: true})
	h.fake.AddPot("acc_test_current", monzotest.Pot{ID: "pot_test_1", Name: "Pot", Balance: 500})
	// Groups of seven rows share a timestamp, so groups straddle the 100-row page boundaries.
	// About 190 rows per 120-day range, so every range has two or more pages.
	want := generate(h, "acc_test_current", 4200, now.AddDate(-7, -2, 0), now.Add(-time.Hour), 7)
	want = append(want, generate(h, "acc_test_prepaid", 230, now.AddDate(-9, 0, 0), now.AddDate(-7, 0, 0), 3)...)

	h.connect(nil)

	reqs := h.txRequests(0)
	if len(reqs) == 0 {
		t.Fatal("no transaction pages were requested")
	}
	for _, r := range reqs {
		since, err := time.Parse(time.RFC3339Nano, r.Query.Get("since"))
		if err != nil {
			continue // an id cursor carries no range
		}
		before, err := time.Parse(time.RFC3339Nano, r.Query.Get("before"))
		if err != nil {
			t.Fatalf("request with a time cursor has no valid before: %q", r.Query.Get("before"))
		}
		if span := before.Sub(since); span > monzotest.MaxRange || span > RangeLength {
			t.Fatalf("requested a range of %v", span)
		}
	}
	got := storedTxIDs(t, h)
	for _, tx := range want {
		if !got[tx.ID] {
			t.Fatalf("transaction %s was never stored", tx.ID)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("stored %d distinct transactions, want %d", len(got), len(want))
	}
	for _, acct := range []string{"acc_test_current", "acc_test_prepaid"} {
		ws := h.windows(acct)
		if len(ws) < 2 {
			t.Fatalf("%s has %d ranges", acct, len(ws))
		}
		for _, w := range ws {
			if w.CompletedAt == nil {
				t.Fatalf("range %v..%v of %s is unfinished", w.PeriodStart, w.PeriodEnd, acct)
			}
			if w.PeriodEnd.Sub(w.PeriodStart) > RangeLength {
				t.Fatalf("stored range of %v", w.PeriodEnd.Sub(w.PeriodStart))
			}
		}
	}

	// One backfill fetch with a period per range attempt, each completed.
	var rangeFetches, completes int
	for _, r := range h.rawRows() {
		switch {
		case r.Kind == store.RawFetch && r.Fetch.PeriodStart != nil:
			if r.Fetch.Kind != kindBackfill {
				t.Fatalf("range fetch of kind %q", r.Fetch.Kind)
			}
			rangeFetches++
		case r.Kind == store.RawFetchComplete:
			completes++
		}
	}
	if total := len(h.windows("acc_test_current")) + len(h.windows("acc_test_prepaid")); rangeFetches != total || completes != total {
		t.Fatalf("%d range fetches and %d completions for %d ranges", rangeFetches, completes, total)
	}
	if run := h.lastRun(kindBackfill); run.Outcome == nil || *run.Outcome != outcomeOK || run.RowsSeen != len(want) {
		t.Fatalf("backfill run = %+v, want ok with %d rows", run, len(want))
	}
	if calls := h.proj.calls(); len(calls) != 1 || calls[0] != "core" {
		t.Fatalf("projector calls = %v, want one for core", calls)
	}
	// Balance before and after for the open account, once for the closed one; pots for both;
	// webhooks only for the open one.
	if n := h.fake.Count(pathBalance); n != 3 {
		t.Fatalf("%d balance reads, want 3", n)
	}
	if n := h.fake.Count(pathPots); n != 2 {
		t.Fatalf("%d pot reads, want 2", n)
	}
	if n := h.fake.Count(pathWebhooks); n != 1 {
		t.Fatalf("%d webhook reads, want 1", n)
	}
}

func TestBackfillCompletesWhenMoreThan100RowsShareOneTimestamp(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	now := h.fake.Now()
	h.fake.AddAccount(monzotest.Account{ID: "acc_test_1", Type: "uk_retail", Created: now.AddDate(0, -2, 0)})
	// Two groups: 150 rows at one instant, then 100 at another.
	rows := h.fake.GenerateTransactions("acc_test_1", 250, now.AddDate(0, -1, 0), now.AddDate(0, 0, -1), 150)

	h.connect(nil)

	for _, w := range h.windows("acc_test_1") {
		if w.CompletedAt == nil {
			t.Fatalf("range %v..%v did not complete", w.PeriodStart, w.PeriodEnd)
		}
	}
	if n := len(h.txRequests(0)); n > 10 {
		t.Fatalf("%d page requests for 250 rows: paging did not move past the shared timestamp", n)
	}
	got := storedTxIDs(t, h)
	// The API cannot page inside a group larger than a page: since=<id> skips the rest of the
	// group. The first 100 of the large group and the whole second group are stored.
	for _, tx := range rows[150:] {
		if !got[tx.ID] {
			t.Fatalf("row %s of the second group was not stored", tx.ID)
		}
	}
	if len(got) < 200 {
		t.Fatalf("stored %d rows, want at least 200", len(got))
	}
}

// slowHistory adds an account with enough history that fetching it takes several minutes of
// fake time when every request advances the clock.
func slowHistory(h *harness) {
	now := h.fake.Now()
	created := time.Date(2023, 6, 1, 9, 0, 0, 0, time.UTC)
	h.fake.AddAccount(monzotest.Account{ID: "acc_test_slow", Type: "uk_retail", Created: created})
	generate(h, "acc_test_slow", 5000, created, now.Add(-time.Hour), 1)
}

func TestPageArrivingAfterDeadlineIsDiscardedAndRangeStaysUnfinishedAtItsCursor(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, func(o *Options) { o.Concurrency = 1 })
	slowHistory(h)
	issued := time.Time{}
	h.connect(func() {
		issued, _ = h.fake.TokenIssuedAt()
		h.fake.WithDelay(10 * time.Second)
	})
	deadline := issued.Add(DefaultWindow)

	reqs := h.txRequests(0)
	for _, r := range reqs {
		if !r.Time.Before(deadline) {
			t.Fatalf("a page was requested at %v, at or after the deadline %v", r.Time, deadline)
		}
	}
	last := reqs[len(reqs)-1]
	if !last.Time.Add(10 * time.Second).After(deadline) {
		t.Fatalf("the last page arrived before the deadline; the scenario does not exercise a late page")
	}
	ws := h.windows("acc_test_slow")
	win := windowOf(t, ws, last)
	if win.CompletedAt != nil {
		t.Fatal("the range whose page arrived late was completed")
	}
	want := formatTime(win.PeriodStart)
	if win.Cursor != nil {
		want = *win.Cursor
	}
	if got := last.Query.Get("since"); got != want {
		t.Fatalf("late page asked since %q but the stored cursor is %q: the late page advanced the cursor", got, want)
	}
	var unfinished int
	for _, w := range ws {
		if w.CompletedAt == nil {
			unfinished++
		}
	}
	if unfinished == 0 {
		t.Fatal("every range completed although the deadline passed")
	}
	if run := h.lastRun(kindBackfill); run.Outcome == nil || *run.Outcome != outcomePartial {
		t.Fatalf("backfill run outcome = %v, want partial", run.Outcome)
	}
}

func TestVerificationRequiredEndsTheRangeUnfinished(t *testing.T) {
	t.Parallel()
	// The bank's window closes at 30 seconds, well before the service's deadline.
	h := newHarness(t, []monzotest.Option{monzotest.WithWindowSeconds(30)}, func(o *Options) { o.Concurrency = 1 })
	slowHistory(h)
	h.connect(func() { h.fake.WithDelay(10 * time.Second) })

	ws := h.windows("acc_test_slow")
	var unfinished []store.BackfillWindow
	for _, w := range ws {
		if w.CompletedAt == nil {
			unfinished = append(unfinished, w)
		}
	}
	if len(unfinished) == 0 {
		t.Fatal("no range was left unfinished")
	}
	// The run moved on to other ranges after the first refusal instead of stopping.
	var refusedAccounts int
	for _, r := range h.rawRows() {
		if r.Kind == store.RawAPIResponse && r.APIResponse.Endpoint == pathTransactions && r.APIResponse.Status == 403 {
			refusedAccounts++
		}
	}
	if refusedAccounts < 2 {
		t.Fatalf("%d refused pages; the run stopped at the first refusal", refusedAccounts)
	}
	run := h.lastRun(kindBackfill)
	if run.Outcome == nil || *run.Outcome != outcomePartial || run.ErrorCategory == nil || *run.ErrorCategory != "forbidden_verification_required" {
		t.Fatalf("run = outcome %v category %v, want partial forbidden_verification_required", run.Outcome, run.ErrorCategory)
	}
	if c := h.consent(); c.State != store.ConsentActive {
		t.Fatalf("consent %s after verification_required, want active", c.State)
	}
}

func TestSecondApprovalContinuesUnfinishedRangesFromTheirCursorsBeforeRefetchingCompleteOnes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, func(o *Options) { o.Concurrency = 1 })
	slowHistory(h)
	h.connect(func() { h.fake.WithDelay(10 * time.Second) })

	before := h.windows("acc_test_slow")
	var withCursor bool
	for _, w := range before {
		if w.CompletedAt == nil && w.Cursor != nil {
			withCursor = true
		}
	}
	if !withCursor {
		t.Fatal("the first run left no unfinished range with a cursor; the scenario does not exercise resuming")
	}

	h.fake.WithDelay(0)
	h.fake.Advance(time.Hour)
	from := len(h.fake.Requests())
	h.connect(nil)

	after := h.windows("acc_test_slow")
	byID := map[int64]store.BackfillWindow{}
	for _, w := range before {
		byID[w.ID] = w
	}
	// The order in which ranges were first asked for in the second run.
	var order []store.BackfillWindow
	firstSince := map[int64]string{}
	for _, r := range h.txRequests(from) {
		w := windowOf(t, after, r)
		if _, ok := firstSince[w.ID]; !ok {
			firstSince[w.ID] = r.Query.Get("since")
			order = append(order, w)
		}
	}
	wasDone := func(w store.BackfillWindow) bool {
		old, ok := byID[w.ID]
		return ok && old.CompletedAt != nil
	}
	// Unfinished ranges first, oldest first; then complete ones, newest first.
	k := slices.IndexFunc(order, wasDone)
	if k < 0 {
		t.Fatal("no complete range was refetched although the window allowed it")
	}
	for i, w := range order {
		if i >= k && !wasDone(w) {
			t.Fatalf("unfinished range %v was fetched after a complete one was refetched", w.PeriodStart)
		}
		if i > 0 && i < k && w.PeriodStart.Before(order[i-1].PeriodStart) {
			t.Fatal("unfinished ranges were not fetched oldest first")
		}
		if i > k && w.PeriodStart.After(order[i-1].PeriodStart) {
			t.Fatal("complete ranges were not refetched newest first")
		}
		old, existed := byID[w.ID]
		want := formatTime(w.PeriodStart)
		if existed && old.CompletedAt == nil && old.Cursor != nil {
			want = *old.Cursor
		}
		if firstSince[w.ID] != want {
			t.Fatalf("range %v was first asked since %q, want %q", w.PeriodStart, firstSince[w.ID], want)
		}
	}
	for _, w := range after {
		if w.CompletedAt == nil {
			t.Fatalf("range %v..%v unfinished after an unhurried second run", w.PeriodStart, w.PeriodEnd)
		}
	}
	if calls := h.proj.calls(); len(calls) != 2 {
		t.Fatalf("projector called %d times, want once per run", len(calls))
	}
}

func TestWorkerRestartedInsideTheWindowKeepsTheSameDeadline(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, func(o *Options) { o.Concurrency = 1 })
	slowHistory(h)
	// The first worker stops in its first approval pause, as a process would on shutdown.
	h.setOnSleep(func(ctx context.Context, _ int, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	})
	h.callback(h.w)
	h.w.Close()
	h.setOnSleep(nil)
	issued, _ := h.fake.TokenIssuedAt()

	h.fake.Advance(100 * time.Second)
	h.fake.Approve()
	h.fake.WithDelay(10 * time.Second)
	w2 := h.newWorker(func(o *Options) { o.Concurrency = 1 })
	t.Cleanup(w2.Close)
	if err := w2.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	w2.Wait()

	if c := h.consent(); c.State != store.ConsentActive {
		t.Fatalf("consent %s after the restarted approval wait, want active", c.State)
	}
	deadline := issued.Add(DefaultWindow)
	reqs := h.txRequests(0)
	if len(reqs) == 0 {
		t.Fatal("the restarted worker fetched no history")
	}
	for _, r := range reqs {
		if !r.Time.Before(deadline) {
			t.Fatalf("page requested at %v: the restart moved the deadline past %v", r.Time.Sub(issued), DefaultWindow)
		}
	}
	unfinished := slices.ContainsFunc(h.windows("acc_test_slow"), func(w store.BackfillWindow) bool { return w.CompletedAt == nil })
	if !unfinished {
		t.Fatal("every range completed; the scenario does not reach the deadline")
	}
}

func TestScheduledRunDoesNotStartWhileFullHistoryFetchIsInFlight(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	now := h.fake.Now()
	h.fake.AddAccount(monzotest.Account{ID: "acc_test_1", Type: "uk_retail", Created: now.AddDate(-1, 0, 0)})
	generate(h, "acc_test_1", 300, now.AddDate(-1, 0, 0), now.Add(-time.Hour), 1)
	// The first page of history is rate limited; the retry pause holds the fetch in flight.
	h.fake.Fail429(pathTransactions, 1)
	blocked, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	// Runs before the harness's cleanup, so a failed assertion does not leave the fetch parked.
	t.Cleanup(unblock)
	h.setOnSleep(func(_ context.Context, n int, _ time.Duration) error {
		switch n {
		case 0:
			h.fake.Approve()
		case 1:
			close(blocked)
			<-release
		}
		return nil
	})
	h.callback(h.w)
	<-blocked

	requests := len(h.fake.Requests())
	// Bounded: a run that waited for the fetch's writer lock would block until the fetch ends.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.w.RunScheduled(ctx, KindHourly); err != nil {
		t.Fatalf("RunScheduled during the fetch: %v", err)
	}
	if n := len(h.fake.Requests()); n != requests {
		t.Fatalf("the scheduled run made %d requests during the full-history fetch", n-requests)
	}
	for _, r := range h.rawRows() {
		if r.Kind == store.RawFetch && r.Fetch.Kind == KindHourly {
			t.Fatal("the scheduled run recorded a fetch during the full-history fetch")
		}
	}
	unblock()
	h.w.Wait()

	if err := h.w.RunScheduled(context.Background(), KindHourly); err != nil {
		t.Fatalf("RunScheduled after the fetch: %v", err)
	}
	if run := h.lastRun(KindHourly); run.Outcome == nil || *run.Outcome != outcomeOK {
		t.Fatalf("hourly run after the fetch = %v, want ok", run.Outcome)
	}
}

func TestRecorderRefusesAResponseOutsideAFetch(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	h.fake.AddAccount(monzotest.Account{ID: "acc_test_1", Type: "uk_retail", Created: h.fake.Now().AddDate(-1, 0, 0)})
	h.connect(nil)
	if _, err := h.w.api.get(context.Background(), pathAccounts, nil); !errors.Is(err, errNoFetch) {
		t.Fatalf("get outside a fetch: %v, want errNoFetch", err)
	}
}

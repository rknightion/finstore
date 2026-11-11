package store_test

import (
	"context"
	"crypto/sha256"
	"strconv"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/store"
)

// finishedRun records a run of kind that started an hour before it finished at.
func finishedRun(t *testing.T, q store.DBTX, source, kind, outcome string, category *string, finished time.Time) {
	t.Helper()
	ctx := context.Background()
	id, err := store.StartRun(ctx, q, source, kind, finished.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, q, id, store.RunResult{FinishedAt: finished, Outcome: outcome, ErrorCategory: category}); err != nil {
		t.Fatal(err)
	}
}

// activeConsent makes a current active consent for source, approved at approved.
func activeConsent(t *testing.T, q store.DBTX, source string, approved time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := store.CreateConsent(ctx, q, source, approved.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentConsent(ctx, q, id); err != nil {
		t.Fatal(err)
	}
	expiry := approved.AddDate(0, 0, 90)
	if _, err := store.TransitionConsent(ctx, q, id, store.ConsentTransition{
		To: store.ConsentActive, At: approved, From: []string{store.ConsentAwaitingApproval}, ExpectedExpiryAt: &expiry,
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestLatestRunsReportsTheNewestFinishedRunOfEachSourceAndKind(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	q := openStore(t, c.app).DB()
	now := time.Date(2030, 5, 1, 12, 0, 0, 0, time.UTC)
	cancelled, rateLimited := "cancelled", "rate_limited"

	// monzo hourly: an old ok run, then a partial one. The partial one is what the gauge shows.
	finishedRun(t, q, "monzo", "hourly", "ok", nil, now.Add(-3*time.Hour))
	finishedRun(t, q, "monzo", "hourly", "partial", &rateLimited, now.Add(-2*time.Hour))
	// A run still running, and one that serve's shutdown ended, are not the latest result.
	if _, err := store.StartRun(ctx, q, "monzo", "hourly", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	finishedRun(t, q, "monzo", "hourly", "failed", &cancelled, now.Add(-time.Hour))
	finishedRun(t, q, "monzo", "nightly", "ok", nil, now.Add(-5*time.Hour))
	finishedRun(t, q, "monzo", "backfill", "ok", nil, now.Add(-9*time.Hour))
	finishedRun(t, q, "truelayer:ob-lloyds", "connect", "partial", nil, now.Add(-8*time.Hour))
	finishedRun(t, q, "truelayer:ob-lloyds", "sync", "failed", nil, now.Add(-4*time.Hour))
	// Statement imports and capture loads are operator commands: never reported, even failed.
	finishedRun(t, q, "halifax", "import", "failed", nil, now.Add(-time.Hour))
	finishedRun(t, q, "capture", "capture", "failed", nil, now.Add(-time.Hour))

	got, err := store.LatestRuns(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.LatestRun{
		{Source: "monzo", Kind: "backfill", OK: true, FinishedAt: now.Add(-9 * time.Hour)},
		{Source: "monzo", Kind: "hourly", OK: false, FinishedAt: now.Add(-2 * time.Hour)},
		{Source: "monzo", Kind: "nightly", OK: true, FinishedAt: now.Add(-5 * time.Hour)},
		{Source: "truelayer:ob-lloyds", Kind: "connect", OK: false, FinishedAt: now.Add(-8 * time.Hour)},
		{Source: "truelayer:ob-lloyds", Kind: "sync", OK: false, FinishedAt: now.Add(-4 * time.Hour)},
	}
	if len(got) != len(want) {
		t.Fatalf("LatestRuns = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Source != want[i].Source || got[i].Kind != want[i].Kind || got[i].OK != want[i].OK ||
			!got[i].FinishedAt.Equal(want[i].FinishedAt) {
			t.Errorf("LatestRuns[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A run that hit its deadline (category timeout) is a real failure and is the latest result; only
// a run serve's shutdown cancelled is skipped.
func TestLatestRunsDoesNotSkipARunThatHitItsDeadline(t *testing.T) {
	c := newCluster(t)
	q := openStore(t, c.app).DB()
	now := time.Date(2030, 5, 1, 12, 0, 0, 0, time.UTC)
	timeout := "timeout"
	finishedRun(t, q, "monzo", "hourly", "ok", nil, now.Add(-2*time.Hour))
	finishedRun(t, q, "monzo", "hourly", "failed", &timeout, now.Add(-time.Hour))

	got, err := store.LatestRuns(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].OK || !got[0].FinishedAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("LatestRuns = %+v, want the failed timeout run at -1h", got)
	}
}

func TestLastRateLimitedListsOnlySourcesWithARateLimitedRun(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	q := openStore(t, c.app).DB()
	now := time.Date(2030, 5, 1, 12, 0, 0, 0, time.UTC)
	rateLimited, timeout := "rate_limited", "timeout"

	if got, err := store.LastRateLimited(ctx, q); err != nil || len(got) != 0 {
		t.Fatalf("empty database: %+v, %v", got, err)
	}
	finishedRun(t, q, "truelayer:ob-lloyds", "sync", "partial", &rateLimited, now.Add(-30*time.Hour))
	finishedRun(t, q, "truelayer:ob-lloyds", "sync", "partial", &rateLimited, now.Add(-6*time.Hour))
	finishedRun(t, q, "truelayer:ob-lloyds", "sync", "ok", nil, now.Add(-time.Hour))
	// Another source whose runs ended for other reasons has no rate-limited run.
	finishedRun(t, q, "monzo", "hourly", "partial", &timeout, now.Add(-time.Hour))
	finishedRun(t, q, "monzo", "hourly", "ok", nil, now.Add(-30*time.Minute))

	got, err := store.LastRateLimited(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Source != "truelayer:ob-lloyds" || !got[0].FinishedAt.Equal(now.Add(-6*time.Hour)) {
		t.Fatalf("LastRateLimited = %+v, want only truelayer:ob-lloyds at -6h", got)
	}
}

func TestConsentHistoryNeedsAnOkFetchRunSinceApproval(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	q := openStore(t, c.app).DB()
	approved := time.Date(2030, 5, 1, 9, 0, 0, 0, time.UTC)

	// Monzo: an ok backfill from before this consent's approval proves nothing about this one.
	finishedRun(t, q, "monzo", "backfill", "ok", nil, approved.Add(-24*time.Hour))
	activeConsent(t, q, "monzo", approved)
	// TrueLayer Lloyds: the connect run was recorded before the approval and ended after it.
	activeConsent(t, q, "truelayer:ob-lloyds", approved)
	finishedRun(t, q, "truelayer:ob-lloyds", "connect", "ok", nil, approved.Add(time.Minute))
	// TrueLayer Monzo: its connect ended partial.
	activeConsent(t, q, "truelayer:ob-monzo", approved)
	finishedRun(t, q, "truelayer:ob-monzo", "connect", "partial", nil, approved.Add(time.Minute))
	// A consent still awaiting approval and one that was lost are not reported.
	awaiting, err := store.CreateConsent(ctx, q, "truelayer:ob-other", approved)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentConsent(ctx, q, awaiting); err != nil {
		t.Fatal(err)
	}
	lost := activeConsent(t, q, "truelayer:ob-lost", approved)
	reason := "access_denied"
	if _, err := store.TransitionConsent(ctx, q, lost, store.ConsentTransition{To: store.ConsentLost, At: approved.Add(time.Hour), Reason: &reason}); err != nil {
		t.Fatal(err)
	}

	read := func() map[string]bool {
		t.Helper()
		rows, err := store.ConsentHistory(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, r := range rows {
			out[r.Source] = r.OK
		}
		return out
	}
	got := read()
	if len(got) != 3 || got["monzo"] || !got["truelayer:ob-lloyds"] || got["truelayer:ob-monzo"] {
		t.Fatalf("ConsentHistory = %v, want monzo false, lloyds true, ob-monzo false and nothing else", got)
	}
	// The backfill that ends ok after the approval makes Monzo's history whole.
	finishedRun(t, q, "monzo", "backfill", "ok", nil, approved.Add(4*time.Minute))
	if got := read(); !got["monzo"] {
		t.Fatalf("after an ok backfill since approval: %v", got)
	}
}

func TestUnansweredConnectsCountsExpiredAttemptsOfTheLastDay(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	q := openStore(t, c.app).DB()
	now := time.Date(2030, 5, 1, 12, 0, 0, 0, time.UTC)
	n := 0
	attempt := func(source string, started time.Time) (id int64, hash []byte) {
		t.Helper()
		n++
		sum := sha256.Sum256([]byte("state-" + strconv.Itoa(n)))
		id, err := store.CreateConnectAttempt(ctx, q, source, sum[:], started, started.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return id, sum[:]
	}
	attempt("monzo", now.Add(-3*time.Hour))                // expired unused, inside the day: 1
	attempt("monzo", now.Add(-30*time.Hour))               // expired unused, older than a day: not counted
	attempt("truelayer:ob-lloyds", now.Add(-26*time.Hour)) // only an old one: reads 0
	// An attempt the bank answered is not unanswered.
	answered, hash := attempt("truelayer:ob-monzo", now.Add(-2*time.Hour))
	if _, err := store.ConsumeConnectAttempt(ctx, q, "truelayer:ob-monzo", hash, now.Add(-2*time.Hour+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetConnectAttemptOutcome(ctx, q, answered, store.AttemptExchanged); err != nil {
		t.Fatal(err)
	}
	attempt("truelayer:ob-monzo", now.Add(-10*time.Minute)) // still open: not expired, not counted
	if _, err := store.ExpireConnectAttempts(ctx, q, now); err != nil {
		t.Fatal(err)
	}

	read := func(at time.Time) map[string]int {
		t.Helper()
		rows, err := store.UnansweredConnects(ctx, q, at)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int{}
		for _, r := range rows {
			out[r.Source] = r.Count
		}
		return out
	}
	got := read(now)
	want := map[string]int{"monzo": 1, "truelayer:ob-lloyds": 0, "truelayer:ob-monzo": 0}
	if len(got) != len(want) {
		t.Fatalf("UnansweredConnects = %v, want %v", got, want)
	}
	for s, v := range want {
		if got[s] != v {
			t.Errorf("%s = %d, want %d (all: %v)", s, got[s], v, got)
		}
	}
	// A day later the same attempt no longer counts, and its source reads 0.
	if later := read(now.Add(25 * time.Hour)); later["monzo"] != 0 {
		t.Errorf("monzo still counts an attempt from over 24 hours ago: %v", later)
	}
}

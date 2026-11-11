package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/store"
)

func TestConnectAttemptIsConsumedOnce(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	q := openStore(t, c.app).DB()
	now := time.Now().UTC()
	hash := sha256.Sum256([]byte("state-value"))
	if _, err := store.CreateConnectAttempt(ctx, q, "monzo", hash[:], now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Two callbacks race with the same state: exactly one wins.
	var wg sync.WaitGroup
	results := make([]error, 8)
	for i := range results {
		wg.Go(func() {
			_, results[i] = store.ConsumeConnectAttempt(ctx, q, hash[:], now.Add(time.Minute))
		})
	}
	wg.Wait()
	wins := 0
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, store.ErrAttemptUnusable):
			t.Fatalf("consume: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d of %d consumers succeeded, want 1", wins, len(results))
	}

	unknown := sha256.Sum256([]byte("never issued"))
	if _, err := store.ConsumeConnectAttempt(ctx, q, unknown[:], now); !errors.Is(err, store.ErrAttemptUnusable) {
		t.Fatalf("unknown state: %v", err)
	}
	stale := sha256.Sum256([]byte("stale"))
	if _, err := store.CreateConnectAttempt(ctx, q, "monzo", stale[:], now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConsumeConnectAttempt(ctx, q, stale[:], now.Add(time.Hour)); !errors.Is(err, store.ErrAttemptUnusable) {
		t.Fatalf("expired state: %v", err)
	}
	if n, err := store.ExpireConnectAttempts(ctx, q, now.Add(2*time.Hour)); err != nil || n != 1 {
		t.Fatalf("ExpireConnectAttempts = %d, %v; want the one unused attempt", n, err)
	}
}

func TestConsentCurrentAndTransitions(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	q := openStore(t, c.app).DB()
	t0 := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)

	first, err := store.CreateConsent(ctx, q, "monzo", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentConsent(ctx, q, first); err != nil {
		t.Fatal(err)
	}
	expiry := t0.AddDate(0, 0, 90)
	got, err := store.TransitionConsent(ctx, q, first, store.ConsentTransition{To: store.ConsentActive, At: t0,
		From: []string{store.ConsentAwaitingApproval}, ExpectedExpiryAt: &expiry})
	if err != nil || got.ApprovedAt == nil || !got.ApprovedAt.Equal(t0) || !got.ExpectedExpiryAt.Equal(expiry) {
		t.Fatalf("approve: %+v %v", got, err)
	}
	if _, err := store.TransitionConsent(ctx, q, first, store.ConsentTransition{To: store.ConsentActive, At: t0,
		From: []string{store.ConsentAwaitingApproval}}); !errors.Is(err, store.ErrStateConflict) {
		t.Fatalf("second approval: %v", err)
	}

	second, err := store.CreateConsent(ctx, q, "monzo", t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	err = pgx.BeginFunc(ctx, q, func(tx pgx.Tx) error {
		if _, err := store.TransitionConsent(ctx, tx, first, store.ConsentTransition{To: store.ConsentReplaced,
			At: t0.Add(time.Hour), Reason: ptr("reconnected")}); err != nil {
			return err
		}
		return store.SetCurrentConsent(ctx, tx, second)
	})
	if err != nil {
		t.Fatal(err)
	}
	cur, err := store.CurrentConsent(ctx, q, "monzo")
	if err != nil || cur.ID != second || cur.State != store.ConsentAwaitingApproval {
		t.Fatalf("current: %+v %v", cur, err)
	}
	old, err := store.GetConsent(ctx, q, first)
	if err != nil || old.IsCurrent || old.State != store.ConsentReplaced || old.EndedAt == nil || *old.EndReason != "reconnected" {
		t.Fatalf("replaced consent: %+v %v", old, err)
	}
	if err := store.SetConsentLastSyncOK(ctx, q, second, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CurrentConsent(ctx, q, "other"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no consent: %v", err)
	}
}

func TestBackfillWindowsAndRuns(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	q := openStore(t, c.app).DB()
	s := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	w, err := store.UpsertBackfillWindow(ctx, q, "acc", s, s.AddDate(0, 0, 120))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetBackfillCursor(ctx, q, w.ID, "2025-02-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	again, err := store.UpsertBackfillWindow(ctx, q, "acc", s, s.AddDate(0, 0, 120))
	if err != nil || again.ID != w.ID || again.Cursor == nil || *again.Cursor != "2025-02-01T00:00:00Z" {
		t.Fatalf("upsert of a known window lost its cursor: %+v %v", again, err)
	}
	w2, err := store.UpsertBackfillWindow(ctx, q, "acc", s.AddDate(0, 0, 120), s.AddDate(0, 0, 240))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteBackfillWindow(ctx, q, w2.ID, s); err != nil {
		t.Fatal(err)
	}
	inc, err := store.ListIncompleteBackfillWindows(ctx, q)
	if err != nil || len(inc) != 1 || inc[0].ID != w.ID {
		t.Fatalf("incomplete: %+v %v", inc, err)
	}
	all, err := store.ListBackfillWindows(ctx, q, "acc")
	if err != nil || len(all) != 2 || all[0].ID != w.ID {
		t.Fatalf("all: %+v %v", all, err)
	}

	id, err := store.StartRun(ctx, q, "monzo", "hourly", s)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, q, id, store.RunResult{FinishedAt: s.Add(time.Minute), Outcome: "failed",
		Requests: 3, RowsSeen: 9, ErrorCategory: ptr("timeout")}); err != nil {
		t.Fatal(err)
	}
	r, err := store.GetRun(ctx, q, id)
	if err != nil || *r.Outcome != "failed" || r.Requests != 3 || *r.ErrorCategory != "timeout" {
		t.Fatalf("run: %+v %v", r, err)
	}
}

func TestOAuthTokenReplace(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	q := openStore(t, c.app).DB()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := store.GetOAuthToken(ctx, q, "monzo"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("empty: %v", err)
	}
	for _, pair := range [][2]string{{"access-1", "refresh-1"}, {"access-2", "refresh-2"}} {
		if err := store.PutOAuthToken(ctx, q, store.OAuthToken{Source: "monzo", AccessToken: pair[0], RefreshToken: pair[1],
			IssuedAt: now, ExpiresAt: now.Add(6 * time.Hour), UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		got, err := store.GetOAuthToken(ctx, q, "monzo")
		if err != nil || got.AccessToken != pair[0] || got.RefreshToken != pair[1] || !got.IssuedAt.Equal(now) {
			t.Fatalf("after put: %v", err)
		}
	}
}

func TestWriterLockExcludesOtherWriters(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	a := openStore(t, c.app)
	b := openStore(t, c.app)
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.WithWriterLock(ctx, func(ctx context.Context, tx pgx.Tx) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock) // a failing check must not leave the holder blocked
	var got bool
	if err := b.DB().QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", store.WriterLockKey).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got {
		t.Fatal("another session took the writer lock while it was held")
	}
	second := make(chan error, 1)
	entered := make(chan struct{})
	go func() {
		second <- b.WithWriterLock(ctx, func(ctx context.Context, tx pgx.Tx) error {
			close(entered)
			return nil
		})
	}()
	select {
	case <-entered:
		t.Fatal("a second writer ran while the first held the lock")
	case <-time.After(300 * time.Millisecond):
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
}

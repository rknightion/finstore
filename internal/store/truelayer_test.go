package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/store"
)

// The callback reads which source an attempt belongs to before any connector consumes it: the
// read changes nothing, and refuses an attempt that is unknown, used, given an outcome or expired.
func TestConnectAttemptSourceIsReadOnlyAndRefusesUnusableAttempts(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	q := openStore(t, c.app).DB()
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	hash := func(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

	if _, err := store.CreateConnectAttempt(ctx, q, "truelayer:ob-lloyds", hash("live"), now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		src, err := store.ConnectAttemptSource(ctx, q, hash("live"), now.Add(time.Minute))
		if err != nil || src != "truelayer:ob-lloyds" {
			t.Fatalf("ConnectAttemptSource = %q, %v", src, err)
		}
	}
	// Reading did not consume it.
	if _, err := store.ConsumeConnectAttempt(ctx, q, hash("live"), now.Add(time.Minute)); err != nil {
		t.Fatalf("consume after read: %v", err)
	}
	if _, err := store.ConnectAttemptSource(ctx, q, hash("live"), now.Add(time.Minute)); !errors.Is(err, store.ErrAttemptUnusable) {
		t.Errorf("used attempt: %v, want ErrAttemptUnusable", err)
	}
	if _, err := store.ConnectAttemptSource(ctx, q, hash("never issued"), now); !errors.Is(err, store.ErrAttemptUnusable) {
		t.Errorf("unknown attempt: %v, want ErrAttemptUnusable", err)
	}
	if _, err := store.CreateConnectAttempt(ctx, q, "monzo", hash("stale"), now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConnectAttemptSource(ctx, q, hash("stale"), now.Add(time.Hour)); !errors.Is(err, store.ErrAttemptUnusable) {
		t.Errorf("expired attempt: %v, want ErrAttemptUnusable", err)
	}
	id, err := store.CreateConnectAttempt(ctx, q, "monzo", hash("refused"), now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetConnectAttemptOutcome(ctx, q, id, store.AttemptRefused); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConnectAttemptSource(ctx, q, hash("refused"), now); !errors.Is(err, store.ErrAttemptUnusable) {
		t.Errorf("attempt with an outcome: %v, want ErrAttemptUnusable", err)
	}
}

// UnfinishedRuns lists only a source's runs of a kind that never recorded an end.
func TestUnfinishedRuns(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	q := openStore(t, c.app).DB()
	at := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	open, err := store.StartRun(ctx, q, "truelayer:ob-lloyds", "connect", at)
	if err != nil {
		t.Fatal(err)
	}
	done, err := store.StartRun(ctx, q, "truelayer:ob-lloyds", "connect", at)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, q, done, store.RunResult{FinishedAt: at, Outcome: "ok"}); err != nil {
		t.Fatal(err)
	}
	for _, other := range [][2]string{{"truelayer:ob-lloyds", "sync"}, {"truelayer:ob-monzo", "connect"}} {
		if _, err := store.StartRun(ctx, q, other[0], other[1], at); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := store.UnfinishedRuns(ctx, q, "truelayer:ob-lloyds", "connect")
	if err != nil || len(ids) != 1 || ids[0] != open {
		t.Fatalf("UnfinishedRuns = %v, %v; want [%d]", ids, err, open)
	}
}

// The unresolved-mandates gauge reads a count per source of every source that holds mandates, so
// a source whose mandates have all resolved reads 0 rather than keeping its last value.
func TestUnresolvedMandatesBySource(t *testing.T) {
	f := newMandateFixture(t)
	core := mustCore(t, f.q, store.SchemaCore)
	got, err := core.UnresolvedMandates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"truelayer:ob-lloyds": 0, "truelayer:ob-monzo": 1}
	if len(got) != len(want) {
		t.Fatalf("UnresolvedMandates = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("UnresolvedMandates[%s] = %d, want %d", k, got[k], v)
		}
	}
}

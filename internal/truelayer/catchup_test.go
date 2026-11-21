package truelayer

import (
	"context"
	"testing"
	"time"
)

// SyncMissedSince is the start-up question "has this connection missed a slot?": only an active
// consent whose newest successful sync (or, with none, its approval) is before the slot says yes,
// and asking never calls the bank or writes anything.
func TestSyncMissedSinceAnswersOnlyForAnActiveConsentBehindTheSlot(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	ctx := context.Background()
	asked := func(slot time.Time) bool {
		t.Helper()
		before := len(h.fake.Requests())
		got, err := w.SyncMissedSince(ctx, slot)
		if err != nil {
			t.Fatalf("SyncMissedSince: %v", err)
		}
		if n := len(h.fake.Requests()) - before; n != 0 {
			t.Fatalf("SyncMissedSince made %d bank calls", n)
		}
		return got
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := h.st.DB().Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	source := Source(ConnLloyds)

	if asked(h.fake.Now()) {
		t.Fatal("a connection with no consent has a missed slot")
	}

	h.connect(w)
	connected := h.fake.Now()
	if asked(connected.Add(-time.Hour)) {
		t.Error("a sync after the slot (the connect run) still reads as missed")
	}
	if asked(connected) {
		t.Error("a sync at the slot itself reads as missed")
	}
	if !asked(connected.Add(time.Second)) {
		t.Error("a sync before the slot does not read as missed")
	}

	// A successful scheduled run settles it again.
	h.fake.Advance(13 * time.Hour)
	slot := h.fake.Now().Add(-time.Hour)
	if !asked(slot) {
		t.Fatal("the last sync is 13 hours old and the slot an hour ago, and the slot is not missed")
	}
	if err := w.RunScheduled(ctx); err != nil {
		t.Fatalf("RunScheduled: %v", err)
	}
	if asked(slot) {
		t.Error("the slot is still missed after a successful run")
	}

	// With no successful sync at all, the approval stands in for it.
	exec(`UPDATE sync.consent SET last_sync_ok_at = NULL WHERE source = $1 AND is_current`, source)
	approved := *h.consent(source).ApprovedAt
	if asked(approved.Add(-time.Second)) {
		t.Error("a consent approved after the slot with no sync reads as missed")
	}
	if !asked(approved.Add(time.Second)) {
		t.Error("a consent approved before the slot with no sync does not read as missed")
	}
	exec(`UPDATE sync.consent SET last_sync_ok_at = $2 WHERE source = $1 AND is_current`, source, connected)

	// Past its expected expiry the consent is not caught up, and asking leaves it as it was.
	expiry := *h.consent(source).ExpectedExpiryAt
	h.fake.Advance(expiry.Sub(h.fake.Now()))
	if asked(h.fake.Now().Add(time.Hour)) {
		t.Error("a consent past its expected expiry reads as missed")
	}
	if c := h.consent(source); c.State != "active" {
		t.Errorf("asking changed the consent to %s", c.State)
	}

	// Not approved, or ended: nothing to catch up whatever the sync times say.
	for _, state := range []string{"awaiting_approval", "lost", "expired", "replaced"} {
		exec(`UPDATE sync.consent SET state = $2, expected_expiry_at = $3 WHERE source = $1 AND is_current`, source, state, h.fake.Now().Add(30*24*time.Hour))
		if asked(h.fake.Now().Add(100 * 24 * time.Hour)) {
			t.Errorf("a %s consent reads as missed", state)
		}
	}

	// The other connection has none.
	other := h.worker(ConnMonzo, nil)
	if got, err := other.SyncMissedSince(ctx, h.fake.Now().Add(time.Hour)); err != nil || got {
		t.Errorf("a connection with no consent: %v, %v", got, err)
	}
}

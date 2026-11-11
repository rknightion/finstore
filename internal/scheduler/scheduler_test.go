package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a clock the tests move by hand; after fires at once and records the delays asked.
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	delays []time.Duration
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (c *fakeClock) after(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.delays = append(c.delays, d)
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch
}

func (c *fakeClock) delaysCopy() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.delays...)
}

func newTest(t *testing.T) (*Scheduler, *fakeClock, *bytes.Buffer) {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	var logs syncBuffer
	s := New(clk.now, slog.New(slog.NewTextHandler(&logs, nil)))
	s.after = clk.after
	s.rnd = func(n int64) int64 { return n - 1 } // the largest jitter
	return s, clk, &logs.Buffer
}

type syncBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

// runUntil runs s until done reports true (polled), then cancels and waits for Run.
func runUntil(t *testing.T, s *Scheduler, done func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestEveryRunsRepeatedlyAndNeverOverlaps(t *testing.T) {
	s, clk, _ := newTest(t)
	const interval = time.Hour
	var active, maxActive, runs atomic.Int32
	s.Every("slow", interval, 10*time.Minute, func(context.Context) error {
		n := active.Add(1)
		maxActive.Store(max(maxActive.Load(), n))
		time.Sleep(time.Millisecond) // long enough for a second goroutine to overlap, if there were one
		// The first run outlasts the interval; later runs take a quarter of it.
		if runs.Add(1) == 1 {
			clk.advance(3 * interval)
		} else {
			clk.advance(interval / 4)
		}
		active.Add(-1)
		return nil
	})
	runUntil(t, s, func() bool { return runs.Load() >= 4 })

	if maxActive.Load() != 1 {
		t.Fatalf("job overlapped itself: %d at once", maxActive.Load())
	}
	d := clk.delaysCopy()
	if len(d) < 4 {
		t.Fatalf("delays = %v", d)
	}
	want := []time.Duration{10 * time.Minute, 0, 3 * interval / 4, 3 * interval / 4}
	for i, w := range want {
		if d[i] != w {
			t.Fatalf("delay %d = %v, want %v (all %v)", i, d[i], w, d)
		}
	}
	if s.LastTick().IsZero() {
		t.Fatal("LastTick not set")
	}
}

func TestEveryWithoutJitterStartsAtOnce(t *testing.T) {
	s, clk, _ := newTest(t)
	var runs atomic.Int32
	s.Every("now", time.Minute, 0, func(context.Context) error { runs.Add(1); return nil })
	runUntil(t, s, func() bool { return runs.Load() >= 1 })
	if d := clk.delaysCopy(); d[0] != 0 {
		t.Fatalf("first delay = %v", d[0])
	}
}

func TestPanickingJobIsRecoveredAndRunsAgain(t *testing.T) {
	s, _, logs := newTest(t)
	var bad, good atomic.Int32
	s.Every("bad", time.Minute, 0, func(context.Context) error {
		switch bad.Add(1) {
		case 1:
			panic("PANIC-MARKER")
		case 2:
			return errors.New("ERROR-MARKER")
		}
		return nil
	})
	s.Every("good", time.Minute, 0, func(context.Context) error { good.Add(1); return nil })
	runUntil(t, s, func() bool { return bad.Load() >= 3 && good.Load() >= 3 })

	out := logs.String()
	if !strings.Contains(out, "kind=bad") || !strings.Contains(out, "outcome=panic") || !strings.Contains(out, "outcome=error") {
		t.Fatalf("log lacks the failures: %q", out)
	}
	if strings.Contains(out, "PANIC-MARKER") || strings.Contains(out, "ERROR-MARKER") || strings.Contains(out, "kind=good") {
		t.Fatalf("log carries more than the job name and outcome: %q", out)
	}
}

func TestRunWaitsForRunningJob(t *testing.T) {
	s, clk, _ := newTest(t)
	// Fire once, then never.
	var fired atomic.Bool
	s.after = func(d time.Duration) <-chan time.Time {
		if fired.CompareAndSwap(false, true) {
			return clk.after(d)
		}
		return nil
	}
	started, release := make(chan struct{}), make(chan struct{})
	var finished atomic.Bool
	s.Every("long", time.Minute, 0, func(context.Context) error {
		close(started)
		<-release
		finished.Store(true)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	<-started
	cancel()
	select {
	case <-done:
		t.Fatal("Run returned while a job was running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil || !finished.Load() {
			t.Fatalf("Run = %v, finished %v", err, finished.Load())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the job finished")
	}
}

func TestRunReturnsWhenIdleJobsAreCancelled(t *testing.T) {
	s, _, _ := newTest(t)
	s.after = func(time.Duration) <-chan time.Time { return nil } // never fires
	s.Every("idle", time.Hour, time.Minute, func(context.Context) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !s.LastTick().IsZero() {
		t.Fatal("job started although the context was cancelled first")
	}
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("second Run succeeded")
	}
}

func TestDailyFirstDelayAndNext(t *testing.T) {
	s, clk, _ := newTest(t) // 12:00 UTC on 2026-11-11, London is on GMT
	london := mustLoc(t, "Europe/London")
	var runs atomic.Int32
	s.Daily("nightly", 3, 0, london, 0, func(context.Context) error {
		runs.Add(1)
		clk.advance(time.Minute)
		return nil
	})
	runUntil(t, s, func() bool { return runs.Load() >= 1 })
	d := clk.delaysCopy()
	if d[0] != 15*time.Hour { // 12:00 -> 03:00 tomorrow
		t.Fatalf("first delay = %v, want 15h", d[0])
	}
}

// A jittered daily job runs a random delay of up to the jitter after its slot, each day, and the
// next day's slot is counted from the slot, not from the late start.
func TestDailyJitterDelaysEachRun(t *testing.T) {
	s, clk, _ := newTest(t) // 12:00 UTC on 2026-11-11; rnd gives the largest jitter
	london := mustLoc(t, "Europe/London")
	var runs atomic.Int32
	s.Daily("truelayer", 18, 20, london, 5*time.Minute, func(context.Context) error {
		runs.Add(1)
		clk.advance(time.Minute)
		return nil
	})
	// The fake after does not move the clock, so move it by each delay asked.
	inner := s.after
	s.after = func(d time.Duration) <-chan time.Time {
		clk.advance(d)
		return inner(d)
	}
	runUntil(t, s, func() bool { return runs.Load() >= 2 })
	d := clk.delaysCopy()
	if want := 6*time.Hour + 25*time.Minute; d[0] != want { // 12:00 -> 18:20 + 5m
		t.Fatalf("first delay = %v, want %v", d[0], want)
	}
	// Started 18:25 and ran a minute: the next run is 18:20 tomorrow plus the jitter again.
	if want := 24*time.Hour - 6*time.Minute + 5*time.Minute; d[1] != want {
		t.Fatalf("second delay = %v, want %v", d[1], want)
	}
}

func TestNamesListsTheRegisteredJobsInOrder(t *testing.T) {
	s, _, _ := newTest(t)
	s.Every("a", time.Minute, 0, func(context.Context) error { return nil })
	s.Daily("b", 6, 20, time.UTC, time.Minute, func(context.Context) error { return nil })
	if got := strings.Join(s.Names(), ","); got != "a,b" {
		t.Fatalf("Names = %q, want a,b", got)
	}
}

func TestDailyRefusesANegativeJitter(t *testing.T) {
	s, _, _ := newTest(t)
	defer func() {
		if recover() == nil {
			t.Fatal("Daily accepted a negative jitter")
		}
	}()
	s.Daily("bad", 6, 20, time.UTC, -time.Second, func(context.Context) error { return nil })
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("no tz database: %v", err)
	}
	return loc
}

func TestNextDaily(t *testing.T) {
	london := mustLoc(t, "Europe/London")
	utc := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	tests := []struct {
		name         string
		after        string
		hour, minute int
		want         string
	}{
		{"later today, summer", "2026-06-01T01:00:00Z", 3, 0, "2026-06-01T02:00:00Z"},
		{"later today, winter", "2026-01-15T01:00:00Z", 3, 0, "2026-01-15T03:00:00Z"},
		{"exactly on the slot goes to tomorrow", "2026-01-15T03:00:00Z", 3, 0, "2026-01-16T03:00:00Z"},
		{"day before clocks go forward, 23h day follows", "2026-03-28T03:00:00Z", 3, 0, "2026-03-29T02:00:00Z"},
		{"on the day clocks go forward", "2026-03-29T00:10:00Z", 3, 0, "2026-03-29T02:00:00Z"},
		{"the day after clocks went forward", "2026-03-29T02:00:00Z", 3, 0, "2026-03-30T02:00:00Z"},
		{"time inside the spring gap runs at the shifted time", "2026-03-28T12:00:00Z", 1, 30, "2026-03-29T01:30:00Z"},
		{"day before clocks go back, 25h day follows", "2026-10-24T02:00:00Z", 3, 0, "2026-10-25T03:00:00Z"},
		{"on the day clocks go back", "2026-10-25T00:10:00Z", 3, 0, "2026-10-25T03:00:00Z"},
		{"repeated hour runs at its first occurrence", "2026-10-24T12:00:00Z", 1, 30, "2026-10-25T00:30:00Z"},
		{"repeated hour does not run a second time", "2026-10-25T00:30:00Z", 1, 30, "2026-10-26T01:30:00Z"},
		{"midnight across the change", "2026-03-28T23:00:00Z", 0, 0, "2026-03-29T00:00:00Z"},
		{"late evening in London is next day in UTC terms", "2026-06-01T23:30:00Z", 0, 15, "2026-06-02T23:15:00Z"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := nextDaily(utc(tc.after), tc.hour, tc.minute, london)
			if !got.Equal(utc(tc.want)) {
				t.Fatalf("nextDaily(%s, %02d:%02d) = %s, want %s", tc.after, tc.hour, tc.minute,
					got.UTC().Format(time.RFC3339), tc.want)
			}
		})
	}
}

package monzo

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/codes"

	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
)

// Only a category that stopping the run can itself produce (network, cancelled, internal, or none)
// is rewritten to cancelled when serve stops mid-run. A real finding earlier in the run (a 500, a
// rate limit, a closed window) is kept, so a deploy landing mid-run cannot turn it into a
// cancelled run that the state gauges skip.
func TestFinishRunOfACancelledContextKeepsARealFinding(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	for cat, want := range map[string]string{
		"network":                         "cancelled",
		"cancelled":                       "cancelled",
		"internal":                        "cancelled",
		"":                                "cancelled",
		"server_error":                    "server_error",
		"rate_limited":                    "rate_limited",
		"timeout":                         "timeout",
		categoryWindowClosed:              categoryWindowClosed,
		categoryTimestampGroup:            categoryTimestampGroup,
		categoryBackfillStarted:           categoryBackfillStarted,
		"forbidden_verification_required": "forbidden_verification_required",
	} {
		id, err := store.StartRun(context.Background(), h.st.DB(), Source, KindHourly, h.fake.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := h.w.finishRun(cancelled, id, KindHourly, outcomePartial, &runTally{category: cat, partial: true}); err != nil {
			t.Fatal(err)
		}
		r := mustRun(t, h.st, id)
		got := ""
		if r.ErrorCategory != nil {
			got = *r.ErrorCategory
		}
		if got != want {
			t.Errorf("partial %q run on a cancelled context = %q, want %q", cat, got, want)
		}
	}
}

// A full-history fetch whose wait for the writer lock is cut by serve stopping ends its span failed
// with the category cancelled, but with no error status: a deploy is not a failure, as for a
// scheduler job cancelled by shutdown. (A wait that runs out of time is a timeout and does set it.)
func TestBackfillWhoseLockWaitIsCutByShutdownSetsNoErrorStatus(t *testing.T) {
	t.Parallel()
	rig := newSpanRig(t)
	h := rig.harness(t, nil)
	openAccount(h, "acc_test_1", 90)
	h.connect(nil)
	rig.mem.Reset()
	id := h.consent().ID

	held, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_ = h.st.HoldWriterLock(context.Background(), store.HolderTest, func(context.Context, *pgx.Conn) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer func() { close(release); <-done }()

	ctx, stop := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, stop)
	defer stop()
	if err := h.w.runBackfill(ctx, id); err == nil {
		t.Fatal("a backfill that never got the lock reported no error")
	}
	runs := rig.runs(kindBackfill)
	if len(runs) != 1 {
		t.Fatalf("%d backfill sync.run spans, want 1", len(runs))
	}
	a := spanAttrs(runs[0])
	if a[semconv.SpanRunOutcome] != outcomeFailed || a[semconv.SpanErrorType] != "cancelled" {
		t.Errorf("sync.run attributes %v, want outcome failed and error.type cancelled", a)
	}
	if runs[0].Status.Code == codes.Error {
		t.Errorf("sync.run status %v, want no error status for a run shutdown cut short", runs[0].Status)
	}
}

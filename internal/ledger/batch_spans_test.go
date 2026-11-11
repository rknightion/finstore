package ledger_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
)

// recordedTracer returns a tracer and the recorder behind it. The spans are read before any
// exporter filter: this is what the ledger itself makes.
func recordedTracer() (trace.Tracer, *tracetest.SpanRecorder) {
	rec := tracetest.NewSpanRecorder()
	return sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)).Tracer("test"), rec
}

// rawRowCount is how many raw rows a replay from empty reads.
func rawRowCount(t *testing.T, e *env) int64 {
	t.Helper()
	var n int64
	for _, err := range store.RawRowsAfter(context.Background(), e.app.DB(), 0) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	return n
}

func endedNamed(rec *tracetest.SpanRecorder, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

func countOf(t *testing.T, s sdktrace.ReadOnlySpan) int64 {
	t.Helper()
	for _, kv := range s.Attributes() {
		if kv.Key == semconv.SpanCount {
			return kv.Value.AsInt64()
		}
	}
	t.Fatalf("span %q has no %s", s.Name(), semconv.SpanCount)
	return 0
}

// A renormalise is a handful of batch spans under the caller's span, never one for a row.
func TestRenormaliseIsBatchStepSpansUnderTheCallersSpan(t *testing.T) {
	e := newEnv(t)
	scenario(e)
	tr, rec := recordedTracer()
	e.tracer = tr
	e.l = e.ledger(true)

	ctx, root := tr.Start(context.Background(), "root")
	err := e.owner.HoldWriterLock(ctx, store.HolderTest, func(ctx context.Context, conn *pgx.Conn) error {
		_, err := e.l.Renormalise(ctx, conn, false)
		return err
	})
	root.End()
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{semconv.SpanLedgerCatchUp, semconv.SpanRenormaliseCreate,
		semconv.SpanRenormaliseCatch, semconv.SpanRenormaliseSwap} {
		got := endedNamed(rec, name)
		if len(got) != 1 {
			t.Fatalf("%d %s spans, want 1", len(got), name)
		}
		if got[0].Parent().SpanID() != root.SpanContext().SpanID() || got[0].SpanKind() != trace.SpanKindInternal {
			t.Errorf("%s: parent %s kind %s, want an INTERNAL child of the root", name, got[0].Parent().SpanID(), got[0].SpanKind())
		}
	}
	if got := endedNamed(rec, semconv.SpanRenormaliseDiff); len(got) != 0 {
		t.Errorf("a swap made %d diff spans", len(got))
	}
	// The replay into core_next counts every raw row.
	if n, want := countOf(t, endedNamed(rec, semconv.SpanRenormaliseCatch)[0]), rawRowCount(t, e); n == 0 || n != want {
		t.Errorf("the replay span counts %d rows, want %d", n, want)
	}
	// The root and four steps, not one span for each of the scenario's rows.
	if n := len(rec.Ended()); n != 5 {
		t.Errorf("%d spans in all, want 5", n)
	}
}

// A renormalise over a core built in another zone tolerates the live catch-up's ErrZoneChanged
// (the live core is about to be compared or replaced), so that step's span is not an error.
func TestRenormaliseDoesNotFailTheLiveCatchUpSpanForATolerableZoneChange(t *testing.T) {
	e := newEnv(t)
	scenario(e) // projected in UTC
	tr, rec := recordedTracer()
	e.tracer = tr
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	e.loc = london
	e.l = e.ledger(true)

	ctx, root := tr.Start(context.Background(), "root")
	err = e.owner.HoldWriterLock(ctx, store.HolderTest, func(ctx context.Context, conn *pgx.Conn) error {
		_, err := e.l.Renormalise(ctx, conn, true)
		return err
	})
	root.End()
	if err != nil {
		t.Fatal(err)
	}
	live := endedNamed(rec, semconv.SpanLedgerCatchUp)
	if len(live) != 1 {
		t.Fatalf("%d live catch-up spans, want 1", len(live))
	}
	if live[0].Status().Code != codes.Unset {
		t.Errorf("the live catch-up span is %+v for a zone change the renormalise tolerates", live[0].Status())
	}
	// A zone change anywhere else is still a failure.
	rec.Reset()
	_, err = e.l.CatchUp(context.Background(), e.owner.DB(), store.SchemaCore)
	if !errors.Is(err, ledger.ErrZoneChanged) {
		t.Fatalf("CatchUp in another zone: %v", err)
	}
	if got := endedNamed(rec, semconv.SpanLedgerCatchUp); len(got) != 1 || got[0].Status().Code != codes.Error {
		t.Errorf("a direct catch-up in another zone must fail its span: %+v", got)
	}
}

func TestRenormaliseVerifyHasADiffSpanWithItsCountAndNoSwap(t *testing.T) {
	e := newEnv(t)
	scenario(e)
	tr, rec := recordedTracer()
	e.tracer = tr
	e.l = e.ledger(true)
	if _, err := e.core().AddToRunningSum(context.Background(), wantAccountID(fakeSource, "acc_1"), 1); err != nil {
		t.Fatal(err)
	}

	ctx, root := tr.Start(context.Background(), "root")
	var diffs []store.CoreDiff
	err := e.owner.HoldWriterLock(ctx, store.HolderTest, func(ctx context.Context, conn *pgx.Conn) error {
		var err error
		diffs, err = e.l.Renormalise(ctx, conn, true)
		return err
	})
	root.End()
	if err != nil {
		t.Fatal(err)
	}
	diff := endedNamed(rec, semconv.SpanRenormaliseDiff)
	if len(diffs) == 0 || len(diff) != 1 || countOf(t, diff[0]) != int64(len(diffs)) {
		t.Fatalf("%d diff spans for %d differences", len(diff), len(diffs))
	}
	if got := endedNamed(rec, semconv.SpanRenormaliseSwap); len(got) != 0 {
		t.Errorf("a verify made %d swap spans", len(got))
	}
}

func TestCatchUpIsOneSpanWithTheRowsItAppliedAndAFailureCarriesNoText(t *testing.T) {
	e := newEnv(t)
	tr, rec := recordedTracer()
	e.tracer = tr
	e.l = e.ledger(true)
	e.accounts(fAcc{ID: "acc_1", Type: "current", Opened: "2029-01-01"})
	rec.Reset()

	ctx, root := tr.Start(context.Background(), "root")
	var stats ledger.Stats
	err := e.app.WithWriterLock(ctx, store.HolderTest, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stats, err = e.l.CatchUp(ctx, tx, store.SchemaCore)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	got := endedNamed(rec, semconv.SpanLedgerCatchUp)
	if len(got) != 1 || stats.RowsProcessed == 0 || countOf(t, got[0]) != int64(stats.RowsProcessed) {
		t.Fatalf("%d catch-up spans for %d rows", len(got), stats.RowsProcessed)
	}
	if got[0].Status().Code != codes.Unset {
		t.Errorf("status %v", got[0].Status())
	}

	// A schema that is not allowed fails the step, and the span says only "error".
	rec.Reset()
	_, err = e.l.CatchUp(ctx, e.app.DB(), "no_such_schema_for_the_canary")
	root.End()
	if err == nil {
		t.Fatal("catch-up into an unknown schema succeeded")
	}
	failed := endedNamed(rec, semconv.SpanLedgerCatchUp)
	if len(failed) != 1 || failed[0].Status().Code != codes.Error || failed[0].Status().Description != "error" {
		t.Fatalf("failed spans %+v", failed)
	}
	if len(failed[0].Events()) != 0 {
		t.Error("the failed span records the error")
	}
}

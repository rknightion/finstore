package capture_test

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rknightion/finstore/internal/capture"
	"github.com/rknightion/finstore/internal/semconv"
)

func countAttr(t *testing.T, s sdktrace.ReadOnlySpan) int64 {
	t.Helper()
	for _, kv := range s.Attributes() {
		if kv.Key == semconv.SpanCount {
			return kv.Value.AsInt64()
		}
	}
	t.Fatalf("span %q has no %s", s.Name(), semconv.SpanCount)
	return 0
}

// A load is one span for each directory read and one for each transactions fetch written, under
// the caller's span, each counting its responses.
func TestLoadMakesDirectoryAndFetchSpansUnderTheCallersSpan(t *testing.T) {
	st := newStore(t)
	dirA, esA := mainDir(t)
	dirB, esB := refetchDir(t)
	rec := tracetest.NewSpanRecorder()
	tr := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)).Tracer("test")

	ctx, root := tr.Start(context.Background(), "root")
	if _, err := capture.Load(ctx, st, newProjector(t), []string{dirA, dirB}, fixedNow, tr); err != nil {
		t.Fatal(err)
	}
	root.End()

	var dirs, fetches []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		switch s.Name() {
		case semconv.SpanCaptureDirectory:
			dirs = append(dirs, s)
		case semconv.SpanCaptureFetch:
			fetches = append(fetches, s)
		}
	}
	if len(dirs) != 2 {
		t.Fatalf("%d %s spans, want 2", len(dirs), semconv.SpanCaptureDirectory)
	}
	if got := countAttr(t, dirs[0]) + countAttr(t, dirs[1]); got != int64(len(esA)+len(esB)) {
		t.Errorf("the directory spans count %d responses, want %d", got, len(esA)+len(esB))
	}
	// Four transaction fetches: three in the first directory, one in the second.
	if len(fetches) != 4 {
		t.Fatalf("%d %s spans, want 4", len(fetches), semconv.SpanCaptureFetch)
	}
	var pages int64
	for _, s := range append(dirs, fetches...) {
		if s.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Errorf("%s span is not a child of the root", s.Name())
		}
	}
	for _, s := range fetches {
		pages += countAttr(t, s)
	}
	if pages != 6 {
		t.Errorf("the fetch spans count %d pages, want 6", pages)
	}
}

func TestLoadFailureEndsItsDirectorySpanWithACategoryNotText(t *testing.T) {
	st := newStore(t)
	rec := tracetest.NewSpanRecorder()
	tr := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)).Tracer("test")
	missing := t.TempDir() // no index.jsonl, no run.json

	if _, err := capture.Load(context.Background(), st, newProjector(t), []string{missing}, fixedNow, tr); err == nil {
		t.Fatal("loading an empty directory succeeded")
	}
	var found bool
	for _, s := range rec.Ended() {
		if s.Name() != semconv.SpanCaptureDirectory {
			continue
		}
		found = true
		if s.Status().Description != "error" || len(s.Events()) != 0 {
			t.Errorf("status %+v events %d: the error's text names a path", s.Status(), len(s.Events()))
		}
	}
	if !found {
		t.Fatal("no directory span")
	}
}

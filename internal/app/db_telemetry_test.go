package app_test

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/app"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/telemetry/telemetrytest"
)

func descendsFrom(spans []telemetrytest.Span, span telemetrytest.Span, ancestor string) bool {
	byID := map[string]telemetrytest.Span{}
	for _, s := range spans {
		byID[s.SpanID] = s
	}
	for id := span.ParentSpanID; id != ""; {
		if id == ancestor {
			return true
		}
		p, ok := byID[id]
		if !ok {
			return false
		}
		id = p.ParentSpanID
	}
	return false
}

func queryText(spans []telemetrytest.Span, text string) []telemetrytest.Span {
	var out []telemetrytest.Span
	for _, s := range spans {
		if strings.Contains(s.Attrs[semconv.SpanDBQueryText], text) {
			out = append(out, s)
		}
	}
	return out
}

func hasDBSpans(spans []telemetrytest.Span) bool {
	for _, s := range spans {
		if s.Attrs[semconv.SpanDBSystemName] != "" {
			return true
		}
	}
	return false
}

// serve opens its store with telemetry: the pool's statistics and the query durations reach the
// metric pipeline, the duration by statement type, and the writer-lock gauge has nothing to say
// while the lock is free.
func TestServeExportsPoolStatisticsAndQueryDurations(t *testing.T) {
	sink := telemetrytest.New(t)
	_, cfg := newTelemetryWorld(t, sink, nil)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	listening := make(chan net.Addr, 1)
	done := make(chan int, 1)
	var stderr syncBuffer
	go func() {
		done <- app.Serve(ctx, app.Env{Cfg: cfg, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: &stderr, Version: testVersion},
			&app.Hooks{NoSchedule: true, Listening: func(a net.Addr) { listening <- a }})
	}()
	select {
	case <-listening:
	case code := <-done:
		t.Fatalf("serve exited %d before listening: %s", code, stderr.String())
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not start listening")
	}
	stop()
	select {
	case code := <-done:
		if code != app.ExitOK {
			t.Fatalf("serve exit %d: %s", code, stderr.String())
		}
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not stop")
	}

	metrics := sink.Metrics()
	if len(metrics["pgxpool.max_connections"].Points) == 0 {
		t.Errorf("no pgxpool.max_connections point; have %d metrics", len(metrics))
	}
	var selects bool
	for _, p := range metrics["db.client.operation.duration"].Points {
		if p.Attrs[semconv.SpanDBOperationName] == "SELECT" && p.Attrs[semconv.SpanPgxOperationType] == "query" {
			selects = true
		}
	}
	if !selects {
		t.Errorf("db.client.operation.duration has no SELECT query point: %+v", metrics["db.client.operation.duration"].Points)
	}
	if pts := metrics[semconv.MetricWriterLockHeld].Points; len(pts) != 0 {
		t.Errorf("the held gauge reported %+v while nothing held the lock", pts)
	}
}

// Renormalise replays every raw row, so its store traces no queries; what it exports is its root
// span and the batch steps beneath it.
func TestRenormaliseExportsBatchStepSpansUnderItsRootAndNoQuerySpans(t *testing.T) {
	sink := telemetrytest.New(t)
	_, cfg := newTelemetryWorld(t, sink, nil)

	if code, errOut := runCLI(cfg, nil, func(ctx context.Context, e app.Env) int { return app.Renormalise(ctx, e, false) }); code != app.ExitOK {
		t.Fatalf("renormalise exit %d: %s", code, errOut)
	}
	if code, errOut := runCLI(cfg, nil, func(ctx context.Context, e app.Env) int { return app.Renormalise(ctx, e, true) }); code != app.ExitOK {
		t.Fatalf("renormalise --verify exit %d: %s", code, errOut)
	}

	spans := sink.Spans()
	roots := spansNamed(spans, semconv.SpanCommandPrefix+"renormalise")
	if len(roots) != 2 {
		t.Fatalf("%d renormalise root spans, want 2", len(roots))
	}
	if hasDBSpans(spans) {
		t.Error("renormalise exported a query span")
	}
	if n := len(spansNamed(spans, semconv.SpanWriterLock)); n != 0 {
		t.Errorf("renormalise exported %d writer-lock spans: its store is opened without telemetry", n)
	}
	for _, root := range roots {
		want := map[string]int{semconv.SpanLedgerCatchUp: 1, semconv.SpanRenormaliseCreate: 1, semconv.SpanRenormaliseCatch: 1}
		if root.Attrs[semconv.SpanProcessExitCode] != "0" {
			t.Fatalf("root %+v", root)
		}
		got := map[string]int{}
		for _, s := range spans {
			if s.TraceID == root.TraceID && s.SpanID != root.SpanID {
				got[s.Name]++
				if s.ParentSpanID != root.SpanID || s.Kind != 1 {
					t.Errorf("%s: parent %q kind %d, want an INTERNAL child of the root", s.Name, s.ParentSpanID, s.Kind)
				}
			}
		}
		// The first run swaps; the second, a verify, diffs.
		if got[semconv.SpanRenormaliseSwap] == 1 {
			want[semconv.SpanRenormaliseSwap] = 1
		} else {
			want[semconv.SpanRenormaliseDiff] = 1
		}
		for name, n := range want {
			if got[name] != n {
				t.Errorf("%d %s spans under a root, want %d (all: %v)", got[name], name, n, got)
			}
		}
		if len(got) != len(want) {
			t.Errorf("spans under a root: %v, want only %v", got, want)
		}
	}
}

// A command that opens its store with telemetry has its queries and its lock under its root span.
func TestAccountCommandsExportQueryAndLockSpansBeneathTheirRoot(t *testing.T) {
	sink := telemetrytest.New(t)
	_, cfg := newTelemetryWorld(t, sink, nil)

	if code, errOut := runCLI(cfg, nil, func(ctx context.Context, e app.Env) int {
		return app.AccountAdd(ctx, e, app.AccountSpec{Slug: "bank-joint", Type: "joint", Currency: "GBP", Name: "made-up joint"})
	}); code != app.ExitOK {
		t.Fatalf("account add exit %d: %s", code, errOut)
	}
	if code, errOut := runCLI(cfg, nil, app.AccountList); code != app.ExitOK {
		t.Fatalf("account list exit %d: %s", code, errOut)
	}

	spans := sink.Spans()
	add := spansNamed(spans, semconv.SpanCommandPrefix+"account add")[0]
	list := spansNamed(spans, semconv.SpanCommandPrefix+"account list")[0]

	locks := spansNamed(spans, semconv.SpanWriterLock)
	if len(locks) != 1 || locks[0].Attrs[semconv.SpanLockHolder] != "account" || locks[0].Attrs[semconv.SpanLockWaitMS] == "" ||
		!descendsFrom(spans, locks[0], add.SpanID) {
		t.Fatalf("lock spans %+v, want one for holder account under account add", locks)
	}
	// The work done under the lock belongs to the command, not to the lock's span, so its spans and
	// log lines name the command as their parent.
	catchUp := spansNamed(spans, semconv.SpanLedgerCatchUp)
	if len(catchUp) != 1 || catchUp[0].ParentSpanID != add.SpanID {
		t.Errorf("catch-up spans %+v, want one that is a child of account add", catchUp)
	}
	inserts := queryText(spans, "operator_entry")
	if len(inserts) == 0 {
		t.Fatal("no query span for the operator entry")
	}
	for _, s := range inserts {
		if s.Kind != 3 || !descendsFrom(spans, s, add.SpanID) {
			t.Errorf("span %q: kind %d, not a CLIENT descendant of account add", s.Name, s.Kind)
		}
	}
	// account list: queries, no lock.
	reads := queryText(spans, "core.account")
	var underList int
	for _, s := range reads {
		if descendsFrom(spans, s, list.SpanID) {
			underList++
		}
	}
	if underList == 0 {
		t.Error("no query span beneath account list")
	}
}

// Migrate's statements, goose's included, are query spans beneath the command's root.
func TestMigrateCommandExportsGooseStatementSpans(t *testing.T) {
	sink := telemetrytest.New(t)
	_, cfg := newTelemetryWorld(t, sink, nil)

	if code, errOut := runCLI(cfg, nil, app.Migrate); code != app.ExitOK {
		t.Fatalf("migrate exit %d: %s", code, errOut)
	}
	spans := sink.Spans()
	root := spansNamed(spans, semconv.SpanCommandPrefix+"migrate")[0]
	goose := queryText(spans, "goose_db_version")
	if len(goose) == 0 {
		t.Fatalf("no span for a goose statement among %d spans", len(spans))
	}
	for _, s := range goose {
		if s.Kind != 3 || !descendsFrom(spans, s, root.SpanID) {
			t.Errorf("span %q: kind %d, not a CLIENT descendant of the migrate root", s.Name, s.Kind)
		}
	}
}

// load-capture, like renormalise, opens its store without telemetry: batch spans, no queries.
func TestLoadCaptureExportsBatchSpansAndNoQuerySpans(t *testing.T) {
	sink := telemetrytest.New(t)
	_, cfg := newTelemetryWorld(t, sink, nil)
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	since, before := "2025-01-01T00:00:00Z", "2025-05-01T00:00:00Z"
	dir := writeCapture(t, []captureEntry{
		{endpoint: "/accounts", status: 200, at: t0, body: `{"accounts":[{"id":"acc_span_one","type":"uk_retail","description":"made-up","created":"2024-12-31T09:00:00Z","closed":false,"currency":"GBP"}]}`},
		{endpoint: "/transactions", status: 200, at: t0.Add(time.Second), since: since, before: before,
			params: map[string]any{"account_id": "acc_span_one", "since": since, "before": before},
			body:   `{"transactions":[` + txn("tx_span_1", "acc_span_one", "2025-02-01T10:00:00Z", 500, "") + `]}`},
	}, []captureRange{{Account: "acc_span_one", Since: since, Before: before, Complete: true}})

	if code, errOut := runCLI(cfg, nil, func(ctx context.Context, e app.Env) int { return app.LoadCapture(ctx, e, []string{dir}) }); code != app.ExitOK {
		t.Fatalf("load-capture exit %d: %s", code, errOut)
	}
	spans := sink.Spans()
	root := spansNamed(spans, semconv.SpanCommandPrefix+"load-capture")[0]
	if hasDBSpans(spans) {
		t.Error("load-capture exported a query span")
	}
	for name, n := range map[string]int{semconv.SpanCaptureDirectory: 1, semconv.SpanCaptureFetch: 1, semconv.SpanLedgerCatchUp: 1} {
		got := spansNamed(spans, name)
		if len(got) != n {
			t.Fatalf("%d %s spans, want %d", len(got), name, n)
		}
		if got[0].ParentSpanID != root.SpanID {
			t.Errorf("%s is not a child of the load-capture root", name)
		}
	}
}

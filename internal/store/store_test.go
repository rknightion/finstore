package store_test

import (
	"context"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry"
	"github.com/rknightion/finstore/internal/telemetry/telemetrytest"
	"github.com/rknightion/finstore/internal/testpg"
)

// canary is a value no span, metric or log record may ever carry. It is made up.
const canary = "CANARY-0d3c71-never-export"

// otlpStatusError is the OTLP status code of an error; the SDK's codes.Error is another number.
const otlpStatusError = 2

// traced is real telemetry (the real exporter allow-list included) in front of a stand-in
// collector, so a test reads what would have left the process.
type traced struct {
	p    *telemetry.Providers
	sink *telemetrytest.Sink
}

func newTraced(t *testing.T) traced {
	t.Helper()
	sink := telemetrytest.New(t)
	p, err := telemetry.Setup(context.Background(),
		config.Config{OTLPEndpoint: sink.URL(), Environment: "test", LogLevel: slog.LevelInfo},
		telemetry.Identity{Role: telemetry.RoleCLI, Command: "test", Version: "test", InstanceID: "test-instance"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return traced{p: p, sink: sink}
}

// spans flushes every pipeline and returns the spans the collector holds.
func (tr traced) spans(t *testing.T) []telemetrytest.Span {
	t.Helper()
	tr.flush(t)
	return tr.sink.Spans()
}

func (tr traced) flush(t *testing.T) {
	t.Helper()
	if err := tr.p.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// everything is every byte the collector received, whatever the signal.
func (tr traced) everything() string {
	var b strings.Builder
	for _, r := range tr.sink.Requests() {
		b.Write(r.Body)
	}
	return b.String()
}

func (tr traced) open(t *testing.T, url string) *store.Store {
	t.Helper()
	return openStoreWith(t, url, store.WithTelemetry(tr.p.TracerProvider, tr.p.MeterProvider))
}

func openStoreWith(t *testing.T, url string, opts ...store.OpenOption) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), url, opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func named(spans []telemetrytest.Span, name string) []telemetrytest.Span {
	var out []telemetrytest.Span
	for _, s := range spans {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

func withText(spans []telemetrytest.Span, text string) []telemetrytest.Span {
	var out []telemetrytest.Span
	for _, s := range spans {
		if strings.Contains(s.Attrs[semconv.SpanDBQueryText], text) {
			out = append(out, s)
		}
	}
	return out
}

// descendsFrom reports whether span has root among its ancestors.
func descendsFrom(spans []telemetrytest.Span, span telemetrytest.Span, root string) bool {
	byID := map[string]telemetrytest.Span{}
	for _, s := range spans {
		byID[s.SpanID] = s
	}
	for id := span.ParentSpanID; id != ""; {
		if id == root {
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

func putToken(t *testing.T, ctx context.Context, st *store.Store) {
	t.Helper()
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := store.PutOAuthToken(ctx, st.DB(), store.OAuthToken{Source: "monzo",
		AccessToken: canary + "-access", RefreshToken: canary + "-refresh",
		IssuedAt: now, ExpiresAt: now.Add(6 * time.Hour), UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
}

func TestQueryInsideASpanIsAClientChildWithPlaceholdersAndNoParameters(t *testing.T) {
	c := newCluster(t)
	tr := newTraced(t)
	st := tr.open(t, c.app)
	ctx, root := tr.p.Tracer("test").Start(context.Background(), "root")
	putToken(t, ctx, st)
	root.End()

	spans := tr.spans(t)
	hits := withText(spans, "oauth_token")
	if len(hits) == 0 {
		t.Fatalf("no query span for the token write among %d spans", len(spans))
	}
	// The write's statement has one span that is a direct child of the root (pgx prepares the
	// statement on its first use, which adds a span beneath it).
	var q telemetrytest.Span
	for _, h := range hits {
		if h.ParentSpanID == root.SpanContext().SpanID().String() {
			q = h
		}
	}
	if q.SpanID == "" || q.Kind != 3 {
		t.Fatalf("no CLIENT span for the write is a child of the root: %+v", hits)
	}
	if !strings.Contains(q.Attrs[semconv.SpanDBQueryText], "$1") {
		t.Errorf("db.query.text = %q, want placeholders", q.Attrs[semconv.SpanDBQueryText])
	}
	if q.Attrs[semconv.SpanDBSystemName] != "postgresql" || q.Attrs[semconv.SpanDBNamespace] != testpg.Database {
		t.Errorf("attributes = %v", q.Attrs)
	}
	if _, ok := q.Attrs["pgx.query.parameters"]; ok {
		t.Error("the span carries the query parameters")
	}
	if q.Name == q.Attrs[semconv.SpanDBQueryText] {
		t.Errorf("the span is named for its whole statement: %q", q.Name)
	}
	// The write's values (leak test 4) are in no signal at all.
	if got := tr.everything(); strings.Contains(got, canary) {
		t.Error("a stored token's canary reached the collector")
	}
}

// otelpgx must never be given the query parameters, whatever the exporter would do to them: the
// first layer is not recording them, the second is the allow-list.
func TestOpenNeverRecordsQueryParameters(t *testing.T) {
	c := newCluster(t)
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	st := openStoreWith(t, c.app, store.WithTelemetry(tp, metricnoop.NewMeterProvider()))
	ctx, root := tp.Tracer("test").Start(context.Background(), "root")
	putToken(t, ctx, st)
	root.End()

	var queries int
	for _, s := range rec.Ended() {
		for _, kv := range s.Attributes() {
			if strings.Contains(kv.Value.String(), canary) {
				t.Errorf("span %q attribute %s carries a parameter", s.Name(), kv.Key)
			}
			if kv.Key == "pgx.query.parameters" {
				t.Errorf("span %q records query parameters", s.Name())
			}
			if kv.Key == semconv.SpanDBQueryText {
				queries++
			}
		}
	}
	if queries == 0 {
		t.Fatal("no query span was recorded, so the check proved nothing")
	}
}

func TestDatabaseErrorTextNeverLeavesTheProcess(t *testing.T) {
	c := newCluster(t)
	failing := func(ctx context.Context, st *store.Store) error {
		var n int
		return st.DB().QueryRow(ctx, "SELECT $1::int", canary).Scan(&n)
	}

	// Without the exporter: the library puts the error text, which quotes the value, on the span.
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	st := openStoreWith(t, c.app, store.WithTelemetry(tp, metricnoop.NewMeterProvider()))
	ctx, root := tp.Tracer("test").Start(context.Background(), "root")
	err := failing(ctx, st)
	root.End()
	if err == nil || !strings.Contains(err.Error(), canary) {
		t.Fatalf("the database error must quote the canary for this test to prove anything: %v", err)
	}
	var raw bool
	for _, s := range rec.Ended() {
		if strings.Contains(s.Status().Description, canary) {
			raw = true
		}
	}
	if !raw {
		t.Fatal("otelpgx no longer records the error text on the span: this test proves nothing")
	}

	// Through the real exporter: gone from every signal.
	tr := newTraced(t)
	traced := tr.open(t, c.app)
	ctx, root = tr.p.Tracer("test").Start(context.Background(), "root")
	if err := failing(ctx, traced); err == nil {
		t.Fatal("no error")
	}
	root.End()
	spans := tr.spans(t)
	if got := tr.everything(); strings.Contains(got, canary) {
		t.Fatal("a database error's canary reached the collector")
	}
	var failed []telemetrytest.Span
	for _, s := range spans {
		if s.StatusCode == otlpStatusError && s.Attrs[semconv.SpanDBQueryText] != "" {
			failed = append(failed, s)
		}
	}
	if len(failed) == 0 {
		t.Fatalf("no failed query span among %d spans", len(spans))
	}
	if f := failed[0]; f.StatusMessage != "error" && f.StatusMessage != "22P02" {
		t.Errorf("status message %q, want a category or SQLSTATE", f.StatusMessage)
	} else if f.Events != 0 || f.Attrs[semconv.SpanPgxSQLState] != "22P02" {
		t.Errorf("events %d, sql state %q: the SQLSTATE stays and the exception event goes", f.Events, f.Attrs[semconv.SpanPgxSQLState])
	}
}

// A password in the database URL reaches the driver and nothing else: not a span, a metric, a log
// record, or an error that a caller might print.
func TestDatabasePasswordInTheURLAppearsNowhere(t *testing.T) {
	c := newCluster(t)
	withPassword := func(rawURL string) string {
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(u.User.Username(), canary)
		return u.String()
	}
	tr := newTraced(t)
	ctx, root := tr.p.Tracer("test").Start(context.Background(), "root")
	st := tr.open(t, withPassword(c.app))
	if err := st.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	putToken(t, ctx, st)
	if err := store.Migrate(ctx, withPassword(c.owner), testRev, store.WithTelemetry(tr.p.TracerProvider, tr.p.MeterProvider)); err != nil {
		t.Fatal(err)
	}
	// A connection that fails takes the error path of the connect span.
	bad, err := url.Parse(withPassword(c.app))
	if err != nil {
		t.Fatal(err)
	}
	bad.Path = "/no_such_database"
	_, openErr := store.Open(ctx, bad.String(), store.WithTelemetry(tr.p.TracerProvider, tr.p.MeterProvider))
	if openErr == nil {
		t.Fatal("opening a database that does not exist succeeded")
	}
	if strings.Contains(openErr.Error(), canary) {
		t.Errorf("the open error repeats the password: %v", openErr)
	}
	root.End()

	spans := tr.spans(t)
	if len(spans) < 5 {
		t.Fatalf("only %d spans: the check proved nothing", len(spans))
	}
	if strings.Contains(tr.everything(), canary) {
		t.Fatal("the URL's password reached the collector")
	}
}

func TestMigrateYieldsGooseStatementSpans(t *testing.T) {
	ctx := context.Background()
	super := testpg.Start(t)
	if err := store.Bootstrap(ctx, super, "owner-secret", "app-secret"); err != nil {
		t.Fatal(err)
	}
	tr := newTraced(t)
	ctx, root := tr.p.Tracer("test").Start(ctx, "root")
	if err := store.Migrate(ctx, testpg.UserURL(t, super, store.RoleOwner), testRev,
		store.WithTelemetry(tr.p.TracerProvider, tr.p.MeterProvider)); err != nil {
		t.Fatal(err)
	}
	root.End()

	spans := tr.spans(t)
	rootID := root.SpanContext().SpanID().String()
	goose := withText(spans, "goose_db_version")
	if len(goose) == 0 {
		t.Fatalf("no span for a goose statement among %d spans", len(spans))
	}
	ddl := withText(spans, "CREATE TABLE")
	if len(ddl) == 0 {
		t.Fatalf("no span for a migration's DDL among %d spans", len(spans))
	}
	for _, s := range append(goose, ddl...) {
		if s.Kind != 3 || !descendsFrom(spans, s, rootID) {
			t.Errorf("span %q: kind %d, not a CLIENT descendant of the root", s.Name, s.Kind)
		}
	}
}

func TestAStoreOpenedWithoutTelemetryYieldsNoQuerySpan(t *testing.T) {
	c := newCluster(t)
	tr := newTraced(t)
	st := openStoreWith(t, c.app)
	ctx, root := tr.p.Tracer("test").Start(context.Background(), "root")
	putToken(t, ctx, st)
	if err := st.WithWriterLock(ctx, store.HolderTest, func(ctx context.Context, tx pgx.Tx) error {
		return store.PutOAuthToken(ctx, tx, store.OAuthToken{Source: "monzo", AccessToken: "a", RefreshToken: "r"})
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, c.owner, testRev); err != nil {
		t.Fatal(err)
	}
	root.End()

	spans := tr.spans(t)
	if len(spans) != 1 || spans[0].Name != "root" {
		var names []string
		for _, s := range spans {
			names = append(names, s.Name)
		}
		t.Fatalf("spans = %v, want only the root: no query, connect or lock span", names)
	}
}

func TestWriterLockHoldYieldsASpanWithItsWait(t *testing.T) {
	c := newCluster(t)
	tr := newTraced(t)
	a, b := tr.open(t, c.app), tr.open(t, c.app)
	ctx, root := tr.p.Tracer("test").Start(context.Background(), "root")

	held, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	aDone := make(chan error, 1)
	go func() {
		aDone <- a.HoldWriterLock(ctx, store.HolderMonzoSync, func(fctx context.Context, _ *pgx.Conn) error {
			workSpan(tr, fctx)
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	bDone := make(chan error, 1)
	go func() {
		bDone <- b.WithWriterLock(ctx, store.HolderTrueLayerSync, func(fctx context.Context, _ pgx.Tx) error {
			workSpan(tr, fctx)
			return nil
		})
	}()
	time.Sleep(200 * time.Millisecond) // b is waiting
	unblock()
	for _, ch := range []chan error{aDone, bDone} {
		if err := <-ch; err != nil {
			t.Fatal(err)
		}
	}
	root.End()

	spans := tr.spans(t)
	locks := named(spans, semconv.SpanWriterLock)
	if len(locks) != 2 {
		t.Fatalf("%d %s spans, want 2", len(locks), semconv.SpanWriterLock)
	}
	byHolder := map[string]telemetrytest.Span{}
	for _, s := range locks {
		byHolder[s.Attrs[semconv.SpanLockHolder]] = s
		if s.Kind != 1 || s.ParentSpanID != root.SpanContext().SpanID().String() {
			t.Errorf("lock span kind %d parent %q, want an INTERNAL child of the root", s.Kind, s.ParentSpanID)
		}
	}
	for holder, minWait := range map[string]int64{store.HolderMonzoSync: 0, store.HolderTrueLayerSync: 150} {
		s, ok := byHolder[holder]
		if !ok {
			t.Fatalf("no lock span for holder %q: %v", holder, byHolder)
		}
		wait, err := strconv.ParseInt(s.Attrs[semconv.SpanLockWaitMS], 10, 64)
		if err != nil {
			t.Fatalf("holder %q: %s = %q", holder, semconv.SpanLockWaitMS, s.Attrs[semconv.SpanLockWaitMS])
		}
		if wait < minWait {
			t.Errorf("holder %q waited %d ms, want at least %d", holder, wait, minWait)
		}
	}
	// The lock's own statements are beneath its span; the work done under the lock is not: it is a
	// sibling of the lock span, a child of the span that asked.
	unlocks := withText(spans, "pg_advisory_unlock")
	if len(unlocks) == 0 {
		t.Error("no span for the unlock")
	}
	for _, u := range unlocks {
		var under bool
		for _, l := range locks {
			under = under || descendsFrom(spans, u, l.SpanID)
		}
		if !under {
			t.Errorf("the unlock span %q is not beneath a lock span", u.Name)
		}
	}
	work := named(spans, "work")
	if len(work) != 2 {
		t.Fatalf("%d work spans, want 2", len(work))
	}
	for _, w := range work {
		if w.ParentSpanID != root.SpanContext().SpanID().String() {
			t.Errorf("work span parent %q, want the caller's span %s (a sibling of the lock span)", w.ParentSpanID, root.SpanContext().SpanID())
		}
	}
}

// workSpan makes the span a writer would make while it holds the lock.
func workSpan(tr traced, ctx context.Context) {
	_, s := tr.p.Tracer("test").Start(ctx, "work")
	s.End()
}

// A release that fails (here: the lock is already gone, so the unlock answers false) fails the lock
// span with a fixed category, though the work itself succeeded.
func TestWriterLockSpanFailsWhenTheReleaseDoes(t *testing.T) {
	c := newCluster(t)
	tr := newTraced(t)
	st := tr.open(t, c.app)
	err := st.HoldWriterLock(context.Background(), store.HolderTest, func(ctx context.Context, conn *pgx.Conn) error {
		var released bool
		return conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", store.WriterLockKey).Scan(&released)
	})
	if err != nil {
		t.Fatal(err)
	}
	locks := named(tr.spans(t), semconv.SpanWriterLock)
	if len(locks) != 1 || locks[0].StatusCode != otlpStatusError || locks[0].StatusMessage != "database_error" {
		t.Fatalf("lock spans %+v, want one with status error database_error", locks)
	}
}

// The driver's own attributes on every query and prepare are allowed: nothing is dropped, so the
// dropped-keys counter means something when it does count.
func TestPlainQueriesDropNoSpanAttribute(t *testing.T) {
	c := newCluster(t)
	tr := newTraced(t)
	st := tr.open(t, c.app)
	ctx, root := tr.p.Tracer("test").Start(context.Background(), "root")
	putToken(t, ctx, st)
	if err := st.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	root.End()

	spans := tr.spans(t)
	var rows, prepared bool
	for _, s := range spans {
		if s.Attrs[semconv.SpanPgxRowsAffected] != "" {
			rows = true
		}
		if strings.HasPrefix(s.Attrs[semconv.SpanPgxPrepareStmtName], "stmtcache_") {
			prepared = true
		}
		if s.Dropped != 0 {
			t.Errorf("span %q dropped %d attributes", s.Name, s.Dropped)
		}
	}
	if !rows || !prepared {
		t.Errorf("rows affected exported: %v, prepared statement name exported: %v", rows, prepared)
	}
	if pts := tr.sink.Metrics()[semconv.MetricTelemetrySpanAttributesDropped].Points; len(pts) != 0 {
		t.Errorf("the dropped-keys counter has points %+v", pts)
	}
}

func TestWriterLockHeldGaugeNamesTheHolderOnlyWhileHeld(t *testing.T) {
	c := newCluster(t)
	tr := newTraced(t)
	st := tr.open(t, c.app)
	m, err := telemetry.NewMetrics(tr.p.Meter)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ObserveWriterLock(st.CurrentHold); err != nil {
		t.Fatal(err)
	}
	held := func() []telemetrytest.Point { return tr.sink.Metrics()[semconv.MetricWriterLockHeld].Points }

	if _, _, ok := st.CurrentHold(); ok {
		t.Fatal("a hold before any lock")
	}
	tr.flush(t)
	if n := len(held()); n != 0 {
		t.Fatalf("%d held points while nothing holds the lock", n)
	}
	err = st.HoldWriterLock(context.Background(), store.HolderMonzoBackfill, func(ctx context.Context, _ *pgx.Conn) error {
		holder, since, ok := st.CurrentHold()
		if !ok || holder != store.HolderMonzoBackfill || time.Since(since) > time.Minute {
			t.Errorf("CurrentHold = %q %v %v", holder, since, ok)
		}
		tr.flush(t)
		pts := held()
		if len(pts) == 0 || pts[len(pts)-1].Attrs[semconv.AttrHolder] != store.HolderMonzoBackfill {
			t.Errorf("held points while held: %+v", pts)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := st.CurrentHold(); ok {
		t.Error("CurrentHold still reports a hold after release")
	}
	before := len(held())
	tr.flush(t)
	if after := len(held()); after != before {
		t.Errorf("the gauge reported %d new points after release", after-before)
	}
}

// The hold is tracked whether or not tracing is on: the gauge serves renormalise and load-capture,
// whose stores trace nothing.
func TestCurrentHoldWorksWithoutTelemetry(t *testing.T) {
	c := newCluster(t)
	st := openStoreWith(t, c.app)
	if err := st.HoldWriterLock(context.Background(), store.HolderRenormalise, func(context.Context, *pgx.Conn) error {
		if holder, _, ok := st.CurrentHold(); !ok || holder != store.HolderRenormalise {
			t.Errorf("CurrentHold = %q %v while held", holder, ok)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := st.CurrentHold(); ok {
		t.Error("a hold after release")
	}
}

func TestWriterLockSpanOfARequestThatGivesUpFailsWithACategory(t *testing.T) {
	c := newCluster(t)
	tr := newTraced(t)
	a, b := tr.open(t, c.app), tr.open(t, c.app)
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.HoldWriterLock(context.Background(), store.HolderTest, func(context.Context, *pgx.Conn) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err := b.HoldWriterLock(ctx, store.HolderTrueLayerSync, func(context.Context, *pgx.Conn) error {
		t.Error("ran without the lock")
		return nil
	})
	close(release)
	if err == nil {
		t.Fatal("a request that timed out got the lock")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, _, ok := b.CurrentHold(); ok {
		t.Error("a hold was recorded for a request that never got the lock")
	}
	var failed int
	for _, s := range named(tr.spans(t), semconv.SpanWriterLock) {
		if s.Attrs[semconv.SpanLockHolder] != store.HolderTrueLayerSync {
			continue
		}
		failed++
		if s.StatusCode != otlpStatusError || s.StatusMessage != "database_error" {
			t.Errorf("status %d %q, want error database_error", s.StatusCode, s.StatusMessage)
		}
		if _, ok := s.Attrs[semconv.SpanLockWaitMS]; ok {
			t.Error("a wait is recorded for a lock that was never granted")
		}
	}
	if failed != 1 {
		t.Fatalf("%d lock spans for the request that gave up, want 1", failed)
	}
}

// Package store owns every SQL statement of finstore: the schemas, the roles and grants, and typed
// accessors for the raw, core, sync and secret tables and the agent views.
//
// Accessors take a DBTX, which a *pgxpool.Pool, a *pgx.Conn and a pgx.Tx all satisfy, so the same
// call works on its own or inside a transaction such as the one WithWriterLock opens. Accessors for
// core are methods on Core, which is bound to the schema it writes (core, or core_next during a
// rebuild).
package store

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/rknightion/finstore/internal/semconv"
)

// DBTX is what every accessor runs its SQL on. *pgxpool.Pool, *pgx.Conn and pgx.Tx satisfy it.
// Begin on a pgx.Tx opens a savepoint, so an accessor that needs its own transaction nests safely.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Advisory lock keys. They are fixed for the life of the database: changing one lets an old and a
// new binary write at the same time.
const (
	// WriterLockKey serialises every writer of raw and core (serve's sync and backfill, import,
	// account add, load-capture, renormalise).
	WriterLockKey int64 = 0x66696e7374720001
	// MigrateLockKey serialises migrate runs.
	MigrateLockKey int64 = 0x66696e7374720002
)

// ErrNotFound is returned by an accessor whose target row does not exist.
var ErrNotFound = errors.New("store: not found")

// Writer-lock holders. HoldWriterLock and WithWriterLock take one, so the lock's span and its
// held gauge say who has it. The values are fixed words chosen here, never data.
const (
	HolderMonzoSync        = "monzo.sync"
	HolderMonzoBackfill    = "monzo.backfill"
	HolderMonzoApproval    = "monzo.approval"
	HolderTrueLayerSync    = "truelayer.sync"
	HolderTrueLayerConnect = "truelayer.connect"
	HolderTrueLayerApprove = "truelayer.approve"
	HolderRenormalise      = "renormalise"
	HolderCapture          = "load-capture"
	// HolderTest is what a test passes.
	HolderTest = "test"
)

// Store is a connection pool to the finstore database.
type Store struct {
	pool *pgxpool.Pool
	// tracer makes the writer-lock span; nil when the store was opened without WithTelemetry.
	tracer trace.Tracer
	// hold is the writer lock this store holds, or nil. It is kept whether or not tracing is on.
	hold atomic.Pointer[writerHold]
}

type writerHold struct {
	holder string
	since  time.Time
}

// OpenOption changes how Open and Migrate connect.
type OpenOption func(*openConfig)

type openConfig struct {
	tp trace.TracerProvider
	mp metric.MeterProvider
	// agentSQL is set by ForAgentSQL: the connections run SQL an agent wrote.
	agentSQL bool
}

// WithTelemetry makes a store trace what it does: every query, connect and pool acquire becomes a
// span when it runs under a recording span, the query durations and the pool's statistics go to
// mp, and a writer-lock hold becomes a span. Without it the store traces nothing, which is what
// renormalise and load-capture want: they run hundreds of thousands of queries.
//
// Query parameters are never recorded. The text of an error that otelpgx puts on a span can quote
// a value; the span exporter's allow-list removes it (see internal/telemetry).
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) OpenOption {
	return func(c *openConfig) { c.tp, c.mp = tp, mp }
}

// ForAgentSQL marks the connections of OpenReader as running SQL an agent wrote, so that SQL never
// reaches telemetry: no span carries db.query.text, every database span is named
// semconv.SpanMCPQuery whatever the statement (and so is its db.operation.name), and the query
// duration metrics carry no operation name (otelpgx's default span name and metric operation name
// are the statement's first word, which for a statement written without spaces is the whole
// statement). It has an effect only together with WithTelemetry, and only on OpenReader.
func ForAgentSQL() OpenOption {
	return func(c *openConfig) { c.agentSQL = true }
}

func newOpenConfig(opts []OpenOption) openConfig {
	var c openConfig
	for _, o := range opts {
		o(&c)
	}
	return c
}

// enabled reports whether WithTelemetry was given a provider.
func (c openConfig) enabled() bool { return c.tp != nil || c.mp != nil }

func (c openConfig) providers() (trace.TracerProvider, metric.MeterProvider) {
	tp, mp := c.tp, c.mp
	if tp == nil {
		tp = tracenoop.NewTracerProvider()
	}
	if mp == nil {
		mp = metricnoop.NewMeterProvider()
	}
	return tp, mp
}

// queryTracer is the pgx tracer for c. It is never given WithIncludeQueryParameters, so parameters
// are not recorded, and span names are the statement's first word (the library's default), not the
// statement.
func (c openConfig) queryTracer() *otelpgx.Tracer {
	tp, mp := c.providers()
	if c.agentSQL {
		// Agent SQL: nothing derived from the statement may leave. No statement attribute, a
		// constant span name (which is also db.operation.name), no metric operation name.
		return otelpgx.NewTracer(
			otelpgx.WithTracerProvider(tp),
			otelpgx.WithMeterProvider(mp),
			otelpgx.WithDisableSQLStatementInAttributes(),
			otelpgx.WithSpanNameFunc(func(string) string { return semconv.SpanMCPQuery }),
		)
	}
	return otelpgx.NewTracer(
		otelpgx.WithTracerProvider(tp),
		otelpgx.WithMeterProvider(mp),
		// db.operation.name on the duration histogram: the statement's first word, so a handful of values.
		otelpgx.WithMetricOperationName(otelpgx.SQLOperationName),
	)
}

// Open connects a pool to url and checks it with a ping.
func Open(ctx context.Context, url string, opts ...OpenOption) (*Store, error) {
	oc := newOpenConfig(opts)
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("store: open pool: %w", err)
	}
	if oc.enabled() {
		cfg.ConnConfig.Tracer = oc.queryTracer()
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: open pool: %w", err)
	}
	s := &Store{pool: pool}
	if oc.enabled() {
		tp, mp := oc.providers()
		s.tracer = tp.Tracer(semconv.ServiceName)
		if err := otelpgx.RecordStats(pool, otelpgx.WithStatsMeterProvider(mp)); err != nil {
			pool.Close()
			return nil, fmt.Errorf("store: pool statistics: %w", err)
		}
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return s, nil
}

// Close closes every connection of the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping runs a round trip to the database.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.pool.QueryRow(ctx, "SELECT 1").Scan(&one)
}

// DB returns the pool as a DBTX, for reads and for writes that need neither a transaction nor the
// writer lock.
func (s *Store) DB() DBTX { return s.pool }

// InTx runs fn in a transaction on the pool, committing when fn returns nil and rolling back
// otherwise.
func (s *Store) InTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return fn(ctx, tx) })
}

// WithWriterLock takes the session-level advisory lock WriterLockKey on one connection, runs fn in
// a transaction on that connection, commits when fn returns nil, and releases the lock. Two
// writers in any process never run at once. Raw rows inserted and normalised inside fn are
// therefore normalised in seq order.
func (s *Store) WithWriterLock(ctx context.Context, holder string, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return s.HoldWriterLock(ctx, holder, func(ctx context.Context, conn *pgx.Conn) error {
		return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error { return fn(ctx, tx) })
	})
}

// HoldWriterLock takes the same lock as WithWriterLock but runs fn on the locked connection with no
// transaction, for a writer that commits in several steps (such as a rebuild that replays raw into
// core_next in batches and then swaps). Other connections may insert raw rows while the lock is
// held; they must be committed before fn normalises them.
//
// holder says who asks (one of the Holder constants). While the lock is held it is the answer of
// CurrentHold, and on a store opened WithTelemetry the request becomes a store.writer_lock span
// from the request to the release, with the wait in finstore.lock.wait_ms once the lock is
// granted. The span is a sibling of the work done under the lock, not its parent: fn gets ctx
// unchanged, so its spans and the log lines it writes belong to the caller's span, and only the
// lock's own statements (acquire, lock, unlock) are beneath the lock span.
func (s *Store) HoldWriterLock(ctx context.Context, holder string, fn func(ctx context.Context, conn *pgx.Conn) error) (err error) {
	requested := time.Now()
	lockCtx := ctx
	var span trace.Span
	if s.tracer != nil {
		lockCtx, span = s.tracer.Start(ctx, semconv.SpanWriterLock,
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(attribute.String(semconv.SpanLockHolder, holder)))
		defer span.End()
	}
	// A request that never gets the lock, or a release that fails, fails the span with a fixed
	// category, never the error.
	failSpan := func() {
		if span != nil {
			span.SetStatus(codes.Error, "database_error")
		}
	}
	conn, err := s.pool.Acquire(lockCtx)
	if err != nil {
		failSpan()
		return fmt.Errorf("store: acquire connection for writer lock: %w", err)
	}
	if _, err := conn.Exec(lockCtx, "SELECT pg_advisory_lock($1)", WriterLockKey); err != nil {
		conn.Release()
		failSpan()
		return fmt.Errorf("store: take writer lock: %w", err)
	}
	h := &writerHold{holder: holder, since: time.Now()}
	s.hold.Store(h)
	if span != nil {
		span.SetAttributes(attribute.Int64(semconv.SpanLockWaitMS, h.since.Sub(requested).Milliseconds()))
	}
	// Cleared after the release below (defers run last in, first out), and only if no later hold
	// has replaced it.
	defer s.hold.CompareAndSwap(h, nil)
	defer func() {
		var unlocked bool
		uerr := conn.QueryRow(context.WithoutCancel(lockCtx), "SELECT pg_advisory_unlock($1)", WriterLockKey).Scan(&unlocked)
		if uerr != nil || !unlocked {
			// The hold did not end cleanly: the span says so, with the same fixed category.
			failSpan()
			// Closing the session is the only other way to release the lock.
			raw := conn.Hijack()
			_ = raw.Close(context.WithoutCancel(lockCtx))
			if uerr != nil && err == nil {
				err = fmt.Errorf("store: release writer lock: %w", uerr)
			}
			return
		}
		conn.Release()
	}()
	return fn(ctx, conn.Conn())
}

// CurrentHold reports who holds the writer lock through this store and since when. ok is false
// while no hold is in progress. It is kept whether or not the store was opened WithTelemetry.
func (s *Store) CurrentHold() (holder string, since time.Time, ok bool) {
	h := s.hold.Load()
	if h == nil {
		return "", time.Time{}, false
	}
	return h.holder, h.since, true
}

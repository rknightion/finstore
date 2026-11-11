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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
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

// Store is a connection pool to the finstore database.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects a pool to url and checks it with a ping.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("store: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{pool: pool}, nil
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
func (s *Store) WithWriterLock(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return s.HoldWriterLock(ctx, func(ctx context.Context, conn *pgx.Conn) error {
		return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error { return fn(ctx, tx) })
	})
}

// HoldWriterLock takes the same lock as WithWriterLock but runs fn on the locked connection with no
// transaction, for a writer that commits in several steps (such as a rebuild that replays raw into
// core_next in batches and then swaps). Other connections may insert raw rows while the lock is
// held; they must be committed before fn normalises them.
func (s *Store) HoldWriterLock(ctx context.Context, fn func(ctx context.Context, conn *pgx.Conn) error) (err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("store: acquire connection for writer lock: %w", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", WriterLockKey); err != nil {
		conn.Release()
		return fmt.Errorf("store: take writer lock: %w", err)
	}
	defer func() {
		var unlocked bool
		uerr := conn.QueryRow(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", WriterLockKey).Scan(&unlocked)
		if uerr != nil || !unlocked {
			// Closing the session is the only other way to release the lock.
			raw := conn.Hijack()
			_ = raw.Close(context.WithoutCancel(ctx))
			if uerr != nil && err == nil {
				err = fmt.Errorf("store: release writer lock: %w", uerr)
			}
			return
		}
		conn.Release()
	}()
	return fn(ctx, conn.Conn())
}

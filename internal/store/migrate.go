package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/rknightion/finstore/internal/store/migrations"
)

//go:embed core_schema.sql
var coreSchemaSQL string

//go:embed agent_views.sql
var agentViewsSQL string

//go:embed grants.sql
var grantsSQL string

// GooseTable is goose's version table. It lives in sync, the only durable schema that is not
// append-only, and grants.sql withholds it from finstore_app.
const GooseTable = "sync.goose_db_version"

// ErrRenormaliseRequired is returned by Migrate when core was built by a different normaliser
// revision than the running binary's. Everything else Migrate does has been done when it returns
// this; the caller must run renormalise before serving.
var ErrRenormaliseRequired = errors.New("store: core was built by another normaliser revision; renormalise is required")

// Migrate connects as finstore_owner and, under the advisory lock MigrateLockKey: runs the goose
// migrations for raw, sync and secret; creates core from the embedded DDL when core has no
// tables; drops and recreates the agent views; applies the grants; and seeds
// core.projection_state with last_seq 0 and normaliserRev when it is empty. When the stored
// revision differs from normaliserRev it returns an error wrapping ErrRenormaliseRequired.
func Migrate(ctx context.Context, ownerURL string, normaliserRev int) error {
	cfg, err := pgx.ParseConfig(ownerURL)
	if err != nil {
		return fmt.Errorf("store: migrate: parse URL: %w", err)
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("store: migrate: connect: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", MigrateLockKey); err != nil {
		return fmt.Errorf("store: migrate: lock: %w", err)
	}
	// Closing the connection releases the lock on every path.

	if err := runGoose(ctx, cfg); err != nil {
		return err
	}

	var stored int
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		var tables int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM pg_tables WHERE schemaname = 'core'").Scan(&tables); err != nil {
			return fmt.Errorf("store: migrate: inspect core: %w", err)
		}
		if tables == 0 {
			if err := createCoreTables(ctx, tx, SchemaCore); err != nil {
				return err
			}
		}
		if err := applyViewsAndGrants(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO core.projection_state (only_row, last_seq, normaliser_rev) VALUES (true, 0, $1) ON CONFLICT DO NOTHING",
			normaliserRev); err != nil {
			return fmt.Errorf("store: migrate: seed projection state: %w", err)
		}
		return tx.QueryRow(ctx, "SELECT normaliser_rev FROM core.projection_state").Scan(&stored)
	})
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	if stored != normaliserRev {
		return fmt.Errorf("%w (core %d, binary %d)", ErrRenormaliseRequired, stored, normaliserRev)
	}
	return nil
}

func runGoose(ctx context.Context, cfg *pgx.ConnConfig) error {
	db := stdlib.OpenDB(*cfg)
	defer func() { _ = db.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS,
		goose.WithTableName(GooseTable), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return fmt.Errorf("store: migrate: goose: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("store: migrate: goose up: %w", err)
	}
	return nil
}

// createCoreTables runs the embedded core DDL into schema, which must exist and be allow-listed.
func createCoreTables(ctx context.Context, q DBTX, schema string) error {
	if err := checkSchema(schema); err != nil {
		return err
	}
	ddl := strings.ReplaceAll(coreSchemaSQL, "{{schema}}", pgx.Identifier{schema}.Sanitize())
	if _, err := q.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("store: create %s tables: %w", schema, err)
	}
	return nil
}

// applyViewsAndGrants drops every view in agent, creates the agent views on core and applies the
// grant matrix.
func applyViewsAndGrants(ctx context.Context, q DBTX) error {
	if err := dropAgentViews(ctx, q); err != nil {
		return err
	}
	if _, err := q.Exec(ctx, agentViewsSQL); err != nil {
		return fmt.Errorf("store: create agent views: %w", err)
	}
	if _, err := q.Exec(ctx, grantsSQL); err != nil {
		return fmt.Errorf("store: apply grants: %w", err)
	}
	return nil
}

func dropAgentViews(ctx context.Context, q DBTX) error {
	rows, err := q.Query(ctx, "SELECT viewname FROM pg_views WHERE schemaname = 'agent'")
	if err != nil {
		return fmt.Errorf("store: list agent views: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("store: list agent views: %w", err)
	}
	for _, n := range names {
		if _, err := q.Exec(ctx, "DROP VIEW IF EXISTS "+pgx.Identifier{"agent", n}.Sanitize()+" CASCADE"); err != nil {
			return fmt.Errorf("store: drop agent view: %w", err)
		}
	}
	return nil
}

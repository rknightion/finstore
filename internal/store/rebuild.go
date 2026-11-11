package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The rebuild operations of renormalise. They run as finstore_owner (which owns core and has
// CREATE on the database) and, by the caller, under the writer lock.

// CreateCoreNext drops any core_next left by an earlier rebuild, creates core_next from the
// embedded DDL and seeds its projection state with last_seq 0 and normaliserRev. Bind a Core to
// SchemaCoreNext to fill it.
func CreateCoreNext(ctx context.Context, q DBTX, normaliserRev int) error {
	return pgx.BeginFunc(ctx, q, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DROP SCHEMA IF EXISTS core_next CASCADE"); err != nil {
			return fmt.Errorf("store: drop old core_next: %w", err)
		}
		if _, err := tx.Exec(ctx, "CREATE SCHEMA core_next"); err != nil {
			return fmt.Errorf("store: create core_next: %w", err)
		}
		if err := createCoreTables(ctx, tx, SchemaCoreNext); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO core_next.projection_state (only_row, last_seq, normaliser_rev) VALUES (true, 0, $1)",
			normaliserRev); err != nil {
			return fmt.Errorf("store: seed core_next projection state: %w", err)
		}
		return nil
	})
}

// DropCoreNext drops core_next if it exists.
func DropCoreNext(ctx context.Context, q DBTX) error {
	if _, err := q.Exec(ctx, "DROP SCHEMA IF EXISTS core_next CASCADE"); err != nil {
		return fmt.Errorf("store: drop core_next: %w", err)
	}
	return nil
}

// ErrNoCoreNext is returned by SwapCore and DiffCore when core_next does not exist.
var ErrNoCoreNext = errors.New("store: core_next does not exist")

func coreNextExists(ctx context.Context, q DBTX) error {
	var ok bool
	if err := q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'core_next')").Scan(&ok); err != nil {
		return fmt.Errorf("store: look up core_next: %w", err)
	}
	if !ok {
		return ErrNoCoreNext
	}
	return nil
}

// SwapCore replaces core with core_next in one transaction: it drops the agent views, renames core
// to core_old and core_next to core, recreates the views, reapplies the grants, drops core_old and
// stores normaliserRev in the new core's projection state. On a pgx.Tx it runs in a savepoint and
// takes effect when that transaction commits.
func SwapCore(ctx context.Context, q DBTX, normaliserRev int) error {
	return pgx.BeginFunc(ctx, q, func(tx pgx.Tx) error {
		if err := coreNextExists(ctx, tx); err != nil {
			return err
		}
		if err := dropAgentViews(ctx, tx); err != nil {
			return err
		}
		for _, stmt := range []string{
			"DROP SCHEMA IF EXISTS core_old CASCADE",
			"ALTER SCHEMA core RENAME TO core_old",
			"ALTER SCHEMA core_next RENAME TO core",
		} {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("store: swap core: %w", err)
			}
		}
		if err := applyViewsAndGrants(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DROP SCHEMA core_old CASCADE"); err != nil {
			return fmt.Errorf("store: drop core_old: %w", err)
		}
		tag, err := tx.Exec(ctx, "UPDATE core.projection_state SET normaliser_rev = $1", normaliserRev)
		if err != nil {
			return fmt.Errorf("store: store normaliser revision: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return errors.New("store: swap core: core_next had no projection state")
		}
		return nil
	})
}

// Kinds of CoreDiff.
const (
	DiffChanged        = "changed"           // the row is in both schemas and Column differs
	DiffOnlyInCore     = "only_in_core"      // the row is missing from core_next
	DiffOnlyInCoreNext = "only_in_core_next" // the row is missing from core
	DiffTable          = "table"             // the table exists in only one schema, or its columns differ
)

// CoreDiff is one difference between core and core_next.
type CoreDiff struct {
	Table  string
	Key    string // the primary key's values as text, joined with "|"; empty for DiffTable
	Column string // the differing column for DiffChanged; empty otherwise
	Kind   string
}

// diffSkipColumns are left out of DiffCore: core_next carries the new binary's revision.
var diffSkipColumns = map[string]map[string]bool{
	"projection_state": {"normaliser_rev": true},
}

// DiffCore compares every table of core with the table of the same name in core_next, row by row
// on the primary key, and returns every difference ordered by table, key and column. An empty
// result means the two schemas hold the same rows. projection_state.normaliser_rev is not
// compared.
func DiffCore(ctx context.Context, q DBTX) ([]CoreDiff, error) {
	if err := coreNextExists(ctx, q); err != nil {
		return nil, err
	}
	live, err := tableColumns(ctx, q, SchemaCore)
	if err != nil {
		return nil, err
	}
	next, err := tableColumns(ctx, q, SchemaCoreNext)
	if err != nil {
		return nil, err
	}
	var diffs []CoreDiff
	for _, t := range sortedKeys(live, next) {
		lt, inLive := live[t]
		nt, inNext := next[t]
		if !inLive || !inNext || strings.Join(lt.cols, ",") != strings.Join(nt.cols, ",") ||
			strings.Join(lt.pk, ",") != strings.Join(nt.pk, ",") || len(lt.pk) == 0 {
			diffs = append(diffs, CoreDiff{Table: t, Kind: DiffTable})
			continue
		}
		d, err := diffTable(ctx, q, t, lt)
		if err != nil {
			return nil, err
		}
		diffs = append(diffs, d...)
	}
	return diffs, nil
}

type tableShape struct {
	cols []string // in ordinal order
	pk   []string // in key order
}

func tableColumns(ctx context.Context, q DBTX, schema string) (map[string]tableShape, error) {
	rows, err := q.Query(ctx, `
		SELECT c.relname,
		       array(SELECT a.attname::text FROM pg_attribute a
		             WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum),
		       coalesce((SELECT array(SELECT a.attname::text FROM unnest(i.indkey) WITH ORDINALITY k(attnum, n)
		                              JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum ORDER BY k.n)
		                 FROM pg_index i WHERE i.indrelid = c.oid AND i.indisprimary), '{}')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind = 'r'`, schema)
	if err != nil {
		return nil, fmt.Errorf("store: list %s tables: %w", schema, err)
	}
	out := make(map[string]tableShape)
	var name string
	var shape tableShape
	_, err = pgx.ForEachRow(rows, []any{&name, &shape.cols, &shape.pk}, func() error {
		out[name] = tableShape{cols: append([]string(nil), shape.cols...), pk: append([]string(nil), shape.pk...)}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: list %s tables: %w", schema, err)
	}
	return out, nil
}

func sortedKeys(a, b map[string]tableShape) []string {
	seen := make(map[string]bool)
	for _, m := range []map[string]tableShape{a, b} {
		for k := range m {
			seen[k] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func diffTable(ctx context.Context, q DBTX, table string, shape tableShape) ([]CoreDiff, error) {
	ident := func(s string) string { return pgx.Identifier{s}.Sanitize() }
	isPK := make(map[string]bool)
	var join, keyParts []string
	for _, k := range shape.pk {
		isPK[k] = true
		join = append(join, "a."+ident(k)+" = b."+ident(k))
		keyParts = append(keyParts, "coalesce(a."+ident(k)+", b."+ident(k)+")::text")
	}
	var checks []string
	for _, col := range shape.cols {
		if isPK[col] || diffSkipColumns[table][col] {
			continue
		}
		lit := "'" + strings.ReplaceAll(col, "'", "''") + "'"
		checks = append(checks, "CASE WHEN a."+ident(col)+" IS DISTINCT FROM b."+ident(col)+" THEN "+lit+" END")
	}
	cols := "ARRAY[]::text[]"
	if len(checks) > 0 {
		cols = "ARRAY[" + strings.Join(checks, ", ") + "]::text[]"
	}
	first := ident(shape.pk[0])
	// Identifiers come from the catalog of the two allow-listed schemas and are quoted.
	stmt := `SELECT key, kind, col FROM (
		  SELECT concat_ws('|', ` + strings.Join(keyParts, ", ") + `) AS key,
		         CASE WHEN a.` + first + ` IS NULL THEN 'only_in_core_next'
		              WHEN b.` + first + ` IS NULL THEN 'only_in_core' ELSE 'changed' END AS kind,
		         ` + cols + ` AS cols
		  FROM core.` + ident(table) + ` a FULL JOIN core_next.` + ident(table) + ` b ON ` + strings.Join(join, " AND ") + `
		) d, LATERAL unnest(CASE WHEN d.kind = 'changed' THEN d.cols ELSE ARRAY[''] END) AS u(col)
		WHERE u.col IS NOT NULL
		ORDER BY key, col`
	rows, err := q.Query(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("store: diff %s: %w", table, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CoreDiff, error) {
		d := CoreDiff{Table: table}
		err := r.Scan(&d.Key, &d.Kind, &d.Column)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: diff %s: %w", table, err)
	}
	return out, nil
}

package store_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// The documented restore (docs/operations.md, Restore) must restore a dump that grants to roles
// finstore does not create. These tests run both procedures against a dump that does.

// rawTables are the append-only tables whose row counts tell a complete restore from a partial one.
var rawTables = []string{"fetch", "api_response", "fetch_complete", "import_file", "operator_entry"}

// restoreFixture is the custom-format dump of a source cluster that has two operator roles granted
// on it and some raw rows, with the row counts of the source.
type restoreFixture struct {
	dump   string
	counts map[string]int
}

func runPG(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("native PostgreSQL prerequisite: %s: %v", name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...) // #nosec G204 -- binaries found on PATH, fixed arguments
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "PG") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func execAll(t *testing.T, url string, stmts ...string) {
	t.Helper()
	conn := connect(t, url)
	for _, s := range stmts {
		if _, err := conn.Exec(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func rawCounts(t *testing.T, url string) map[string]int {
	t.Helper()
	conn := connect(t, url)
	counts := make(map[string]int)
	for _, name := range rawTables {
		var n int
		if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM raw."+name).Scan(&n); err != nil {
			t.Fatalf("count raw.%s: %v", name, err)
		}
		counts[name] = n
	}
	return counts
}

func newRestoreFixture(t *testing.T) restoreFixture {
	t.Helper()
	c := newCluster(t)
	// Operator roles finstore knows nothing about: a monitoring login with a read grant, and a
	// reporting login that is a member of finstore_agent with a grant of its own.
	execAll(t, c.super,
		"CREATE ROLE ops_monitor LOGIN",
		"GRANT USAGE ON SCHEMA raw TO ops_monitor",
		"GRANT SELECT ON ALL TABLES IN SCHEMA raw TO ops_monitor",
		"CREATE ROLE ops_report LOGIN",
		"GRANT "+store.RoleAgent+" TO ops_report",
		"GRANT SELECT ON ALL TABLES IN SCHEMA agent TO ops_report",
		"INSERT INTO raw.fetch (source, kind, run_id, started_at) VALUES ('test', 'sync', 1, now()), ('test', 'sync', 2, now())",
		"INSERT INTO raw.fetch_complete (fetch_id, completed_at, pages) SELECT id, now(), 1 FROM raw.fetch",
		"INSERT INTO raw.api_response (fetch_id, source, endpoint, params, status, content_type, fetched_at, body, body_sha256) "+
			"SELECT id, 'test', '/x', '{}', 200, 'application/json', now(), '\\x7b7d', '\\x00' FROM raw.fetch",
		"INSERT INTO raw.operator_entry (kind, payload, entered_at) VALUES ('note', '{}', now())",
	)
	dump := filepath.Join(t.TempDir(), "finstore.dump")
	if out, err := runPG(t, "pg_dump", "-Fc", "-f", dump, "-d", c.super); err != nil {
		t.Fatalf("pg_dump: %v\n%s", err, out)
	}
	counts := rawCounts(t, c.super)
	if counts["fetch"] != 2 || counts["api_response"] != 2 || counts["operator_entry"] != 1 {
		t.Fatalf("fixture counts: %v", counts)
	}
	return restoreFixture{dump: dump, counts: counts}
}

// emptyTarget is a fresh cluster with bootstrap run on it: the state of the documented restore
// before pg_restore. It has none of the operator's roles.
func emptyTarget(t *testing.T) string {
	t.Helper()
	super := testpg.Start(t)
	if err := store.Bootstrap(context.Background(), super, "owner-secret", "app-secret"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return super
}

func TestRestoreWithoutPrivilegesSurvivesGrantsToOtherRoles(t *testing.T) {
	ctx := context.Background()
	fx := newRestoreFixture(t)
	super := emptyTarget(t)

	// The documented command line. --no-owner would be wrong: the objects must stay owned by
	// finstore_owner, which bootstrap created, or migrate could not alter them.
	out, err := runPG(t, "pg_restore", "-d", super, "--clean", "--if-exists", "--exit-on-error", "--no-privileges", fx.dump)
	if err != nil {
		t.Fatalf("pg_restore: %v\n%s", err, out)
	}
	if err := store.Migrate(ctx, testpg.UserURL(t, super, store.RoleOwner), testRev); err != nil {
		t.Fatalf("Migrate after restore: %v", err)
	}

	got := rawCounts(t, super)
	for _, name := range rawTables {
		if got[name] != fx.counts[name] {
			t.Errorf("raw.%s: %d rows after restore, %d in the source", name, got[name], fx.counts[name])
		}
	}

	conn := connect(t, super)
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname IN ('ops_monitor', 'ops_report'))").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("the target must not have the operator's roles, or the test proves nothing")
	}
	// finstore's own grants are back, applied by migrate.
	for _, q := range []struct {
		what, sql string
		want      bool
	}{
		{"app inserts into raw", "SELECT has_table_privilege('" + store.RoleApp + "', 'raw.api_response', 'INSERT')", true},
		{"app updates core", "SELECT has_table_privilege('" + store.RoleApp + "', 'core.projection_state', 'UPDATE')", true},
		{"agent uses agent", "SELECT has_schema_privilege('" + store.RoleAgent + "', 'agent', 'USAGE')", true},
		{"mcp reads raw", "SELECT has_table_privilege('" + store.RoleMCP + "', 'raw.api_response', 'SELECT')", true},
		{"agent cannot use secret", "SELECT has_schema_privilege('" + store.RoleAgent + "', 'secret', 'USAGE')", false},
		{"app cannot update raw", "SELECT has_table_privilege('" + store.RoleApp + "', 'raw.api_response', 'UPDATE')", false},
	} {
		var got bool
		if err := conn.QueryRow(ctx, q.sql).Scan(&got); err != nil {
			t.Fatalf("%s: %v", q.what, err)
		}
		if got != q.want {
			t.Errorf("%s: got %v after restore and migrate, want %v", q.what, got, q.want)
		}
	}
}

// The procedure this replaces, kept to show what it did: pg_restore exits non-zero at the first
// grant to a role the target lacks. Row counts alone do not show it: the data is restored before
// the grants are, so the exit status is the check that cannot be skipped.
func TestRestoreWithPrivilegesFailsOnGrantsToOtherRoles(t *testing.T) {
	fx := newRestoreFixture(t)
	super := emptyTarget(t)

	out, err := runPG(t, "pg_restore", "-d", super, "--clean", "--if-exists", "--exit-on-error", fx.dump)
	if err == nil {
		t.Fatal("pg_restore with privileges succeeded into a cluster without the operator's roles")
	}
	if !strings.Contains(out, "does not exist") {
		t.Fatalf("pg_restore failed for another reason:\n%s", out)
	}
}

package store_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// mcpURL turns the finstore_mcp login on (as bootstrap does with FINSTORE_MCP_PASSWORD) and returns
// a URL that connects as it. The test cluster trusts every local connection, so the URL carries no
// password.
func (c cluster) mcpURL(t *testing.T) string {
	t.Helper()
	if err := store.EnableMCPLogin(context.Background(), c.super, "mcp-secret"); err != nil {
		t.Fatalf("EnableMCPLogin: %v", err)
	}
	return testpg.UserURL(t, c.super, store.RoleMCP)
}

func connect(t *testing.T, url string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// relation is a table or view of the four readable schemas, or of secret.
type relation struct{ schema, name, kind string }

func (r relation) String() string { return r.schema + "." + r.name }

// relations lists every table (r, p), view (v) and materialised view (m) in the given schemas.
func relations(t *testing.T, superURL string, schemas ...string) []relation {
	t.Helper()
	conn := connect(t, superURL)
	rows, err := conn.Query(context.Background(), `SELECT n.nspname::text, c.relname::text, c.relkind::text
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = ANY($1) AND c.relkind IN ('r', 'p', 'v', 'm') ORDER BY 1, 2`, schemas)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (relation, error) {
		var x relation
		return x, r.Scan(&x.schema, &x.name, &x.kind)
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// firstColumn is a relation's first column, for an UPDATE that sets it to itself.
func firstColumn(t *testing.T, superURL string, r relation) string {
	t.Helper()
	var col string
	if err := connect(t, superURL).QueryRow(context.Background(), `SELECT a.attname::text FROM pg_attribute a
		WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum LIMIT 1`,
		pgx.Identifier{r.schema, r.name}.Sanitize()).Scan(&col); err != nil {
		t.Fatal(err)
	}
	return col
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// requireWriteRefused fails unless err is a refusal: 42501 (no privilege) or, while the session is
// read only, 25006 (read-only transaction).
func requireWriteRefused(t *testing.T, err error, what string, readOnly bool) {
	t.Helper()
	code := pgCode(err)
	if code == "42501" || (readOnly && code == "25006") {
		return
	}
	t.Errorf("%s: want 42501%s, got %v", what, map[bool]string{true: " or 25006", false: ""}[readOnly], err)
}

func TestMCPRoleReadsEverythingButSecret(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	fillCore(t, mustCore(t, openStore(t, c.app).DB(), store.SchemaCore))
	mcp := connect(t, c.mcpURL(t))

	readable := relations(t, c.super, "agent", "core", "raw", "sync")
	// The matrix is built from the catalog, so a table added later is covered. These must be in it.
	for _, want := range []string{"agent.identity", "agent.coverage", "core.account", "core.transaction",
		"raw.api_response", "raw.import_file", "sync.run", "sync.consent", store.GooseTable} {
		if !slices.ContainsFunc(readable, func(r relation) bool { return r.String() == want }) {
			t.Fatalf("catalog lacks %s: %v", want, readable)
		}
	}
	for _, r := range readable {
		rows, err := mcp.Query(ctx, "SELECT * FROM "+pgx.Identifier{r.schema, r.name}.Sanitize()+" LIMIT 1")
		if err == nil {
			rows.Close()
			err = rows.Err()
		}
		if err != nil {
			t.Errorf("finstore_mcp SELECT %s: %v", r, err)
		}
	}

	// secret: no schema USAGE, so no table, whatever the statement.
	secret := relations(t, c.super, "secret")
	if len(secret) == 0 {
		t.Fatal("secret has no relation to test")
	}
	for _, r := range secret {
		_, err := mcp.Exec(ctx, "SELECT 1 FROM "+pgx.Identifier{r.schema, r.name}.Sanitize())
		requireDenied(t, err, "finstore_mcp SELECT "+r.String())
	}
	_, err := mcp.Exec(ctx, "SELECT * FROM secret.oauth_token")
	requireDenied(t, err, "finstore_mcp SELECT secret.oauth_token")
	var usage bool
	if err := mcp.QueryRow(ctx, "SELECT has_schema_privilege('secret', 'USAGE')").Scan(&usage); err != nil || usage {
		t.Errorf("finstore_mcp has USAGE on secret: %v %v", usage, err)
	}
	// The grants never reach secret through a schema-wide shortcut either.
	var leaked int
	if err := connect(t, c.super).QueryRow(ctx, `SELECT count(*) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace, LATERAL aclexplode(c.relacl) a
		WHERE n.nspname = 'secret' AND a.grantee = 'finstore_mcp'::regrole`).Scan(&leaked); err != nil || leaked != 0 {
		t.Errorf("a secret relation has a grant to finstore_mcp: %d %v", leaked, err)
	}
}

func TestMCPRoleCannotWriteOrCreate(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	fillCore(t, mustCore(t, openStore(t, c.app).DB(), store.SchemaCore))
	mcp := connect(t, c.mcpURL(t))

	tables := relations(t, c.super, "raw", "core", "sync", "secret")
	views := relations(t, c.super, "agent")
	schemas := []string{"agent", "core", "raw", "sync", "secret", "public"}

	// The role's own default makes the session read only, and it is on without the test asking.
	var ro string
	if err := mcp.QueryRow(ctx, "SHOW default_transaction_read_only").Scan(&ro); err != nil || ro != "on" {
		t.Fatalf("default_transaction_read_only = %q (%v), want on", ro, err)
	}

	attempt := func(readOnly bool) {
		t.Helper()
		for _, r := range tables {
			id := pgx.Identifier{r.schema, r.name}.Sanitize()
			col := pgx.Identifier{firstColumn(t, c.super, r)}.Sanitize()
			for _, stmt := range []string{
				// Selecting no rows still needs the privilege, so a grant is what these test, and
				// no row can be written (or lost) when one is wrongly there. An INSERT that
				// supplies no values would fail on a sequence's privilege instead.
				"INSERT INTO " + id + " SELECT * FROM " + id + " WHERE false",
				"UPDATE " + id + " SET " + col + " = " + col + " WHERE false",
				"DELETE FROM " + id + " WHERE false",
				"TRUNCATE " + id,
			} {
				_, err := mcp.Exec(ctx, stmt)
				requireWriteRefused(t, err, stmt, readOnly)
			}
		}
		// A view can be auto-updatable (agent.transaction_history is): the write is refused too.
		for _, r := range views {
			id := pgx.Identifier{r.schema, r.name}.Sanitize()
			for _, stmt := range []string{"INSERT INTO " + id + " DEFAULT VALUES", "DELETE FROM " + id} {
				if _, err := mcp.Exec(ctx, stmt); err == nil {
					t.Errorf("finstore_mcp ran %s", stmt)
				}
			}
		}
		for _, s := range schemas {
			for _, stmt := range []string{
				"CREATE TABLE " + s + ".intruder (x int)",
				"CREATE VIEW " + s + ".intruder AS SELECT 1 AS x",
				"CREATE FUNCTION " + s + ".intruder() RETURNS int LANGUAGE sql AS 'SELECT 1'",
				"CREATE SEQUENCE " + s + ".intruder",
			} {
				_, err := mcp.Exec(ctx, stmt)
				requireWriteRefused(t, err, stmt, readOnly)
			}
		}
		_, err := mcp.Exec(ctx, "CREATE SCHEMA intruder")
		requireWriteRefused(t, err, "CREATE SCHEMA", readOnly)
	}

	// In the default, read-only session.
	attempt(true)
	// After the session turns read-only off, only the grants stand in the way: every refusal is a
	// privilege error.
	if _, err := mcp.Exec(ctx, "SET default_transaction_read_only = off"); err != nil {
		t.Fatal(err)
	}
	var ro2 string
	if err := mcp.QueryRow(ctx, "SHOW default_transaction_read_only").Scan(&ro2); err != nil || ro2 != "off" {
		t.Fatalf("could not turn read-only off for the test: %q %v", ro2, err)
	}
	attempt(false)

	// Nothing was written or created.
	var intruders int
	if err := connect(t, c.super).QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relname = 'intruder' AND n.nspname = ANY($1)`, schemas).Scan(&intruders); err != nil || intruders != 0 {
		t.Errorf("%d intruder relations exist: %v", intruders, err)
	}
	var accounts int
	if err := connect(t, c.super).QueryRow(ctx, "SELECT count(*) FROM core.account").Scan(&accounts); err != nil || accounts != 1 {
		t.Errorf("core.account holds %d rows after the write attempts, want 1 (%v)", accounts, err)
	}
}

func TestMCPRoleSettings(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	mcp := connect(t, c.mcpURL(t))

	for setting, want := range map[string]string{
		"temp_file_limit":                     "1GB",
		"statement_timeout":                   "15s",
		"lock_timeout":                        "2s",
		"idle_in_transaction_session_timeout": "30s",
		"default_transaction_read_only":       "on",
	} {
		var got string
		if err := mcp.QueryRow(ctx, "SELECT current_setting($1)", setting).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %q (%v), want %q", setting, got, err, want)
		}
	}
	// temp_file_limit is superuser only: the role cannot raise it, by SET or set_config.
	_, err := mcp.Exec(ctx, "SET temp_file_limit = '10GB'")
	requireDenied(t, err, "SET temp_file_limit")
	_, err = mcp.Exec(ctx, "SELECT set_config('temp_file_limit', '10GB', false)")
	requireDenied(t, err, "set_config temp_file_limit")
	var limit string
	if err := mcp.QueryRow(ctx, "SHOW temp_file_limit").Scan(&limit); err != nil || limit != "1GB" {
		t.Errorf("temp_file_limit after the attempts = %q (%v), want 1GB", limit, err)
	}
}

func TestMCPRoleReadsCoreAfterSwapCore(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	app := openStore(t, c.app)
	owner := openStore(t, c.owner)
	fillCore(t, mustCore(t, app.DB(), store.SchemaCore))
	url := c.mcpURL(t)

	const newRev = testRev + 1
	if err := owner.WithWriterLock(ctx, store.HolderTest, func(ctx context.Context, tx pgx.Tx) error {
		if err := store.CreateCoreNext(ctx, tx, newRev); err != nil {
			return err
		}
		acc, _ := fillCore(t, mustCore(t, tx, store.SchemaCoreNext))
		acc.Name = "rebuilt"
		if err := mustCore(t, tx, store.SchemaCoreNext).UpsertAccount(ctx, acc); err != nil {
			return err
		}
		return store.SwapCore(ctx, tx, newRev)
	}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	// A connection made after the swap reads the new core, its tables and the views over it.
	mcp := connect(t, url)
	var name string
	if err := mcp.QueryRow(ctx, "SELECT name FROM core.account").Scan(&name); err != nil || name != "rebuilt" {
		t.Fatalf("finstore_mcp reading the swapped core: %q %v", name, err)
	}
	for _, r := range relations(t, c.super, "agent", "core", "raw", "sync") {
		rows, err := mcp.Query(ctx, "SELECT * FROM "+pgx.Identifier{r.schema, r.name}.Sanitize()+" LIMIT 1")
		if err == nil {
			rows.Close()
			err = rows.Err()
		}
		if err != nil {
			t.Errorf("finstore_mcp SELECT %s after the swap: %v", r, err)
		}
	}
	_, err := mcp.Exec(ctx, "DELETE FROM core.account")
	requireWriteRefused(t, err, "DELETE after the swap", true)
}

// boundaryViolations lists what breaks the rule that nothing readable depends on secret: a view or
// function in agent, core, raw or sync that reaches a relation of secret (directly, through
// another view or function, or by naming it in a function body), and every SECURITY DEFINER
// function in those schemas.
func boundaryViolations(t *testing.T, superURL string) []string {
	t.Helper()
	conn := connect(t, superURL)
	rows, err := conn.Query(context.Background(), `
		WITH RECURSIVE edge(fc, f, tc, t) AS (
		  -- a view's rewrite rule depends on what the view reads
		  SELECT 'pg_class'::regclass::oid, r.ev_class, d.refclassid, d.refobjid
		  FROM pg_depend d JOIN pg_rewrite r ON d.classid = 'pg_rewrite'::regclass AND d.objid = r.oid
		  WHERE d.refclassid IN ('pg_class'::regclass, 'pg_proc'::regclass) AND d.deptype = 'n'
		  UNION
		  -- a SQL-standard function body depends on what it reads
		  SELECT d.classid, d.objid, d.refclassid, d.refobjid FROM pg_depend d
		  WHERE d.classid = 'pg_proc'::regclass AND d.refclassid IN ('pg_class'::regclass, 'pg_proc'::regclass)
		    AND d.deptype = 'n'
		), reach(rc, r, c, o) AS (
		  SELECT fc, f, tc, t FROM edge
		  UNION
		  SELECT reach.rc, reach.r, e.tc, e.t FROM reach JOIN edge e ON e.fc = reach.c AND e.f = reach.o
		)
		SELECT DISTINCT 'depends on secret: ' || n.nspname || '.' || coalesce(v.relname, p.proname)
		FROM reach
		LEFT JOIN pg_class v ON reach.rc = 'pg_class'::regclass::oid AND v.oid = reach.r
		LEFT JOIN pg_proc p ON reach.rc = 'pg_proc'::regclass::oid AND p.oid = reach.r
		JOIN pg_namespace n ON n.oid = coalesce(v.relnamespace, p.pronamespace)
		JOIN pg_class t ON reach.c = 'pg_class'::regclass::oid AND t.oid = reach.o
		JOIN pg_namespace tn ON tn.oid = t.relnamespace
		WHERE n.nspname IN ('agent', 'core', 'raw', 'sync') AND tn.nspname = 'secret'
		UNION
		SELECT 'security definer: ' || n.nspname || '.' || p.proname
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname IN ('agent', 'core', 'raw', 'sync') AND p.prosecdef
		UNION
		-- a function body that names the schema (plpgsql bodies carry no dependency)
		SELECT 'names secret: ' || n.nspname || '.' || p.proname
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname IN ('agent', 'core', 'raw', 'sync') AND p.prosrc ~* '\msecret\M'
		ORDER BY 1`)
	if err != nil {
		t.Fatalf("boundary query: %v", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("boundary query: %v", err)
	}
	return out
}

func TestNothingReadableDependsOnSecretAndNothingIsSecurityDefiner(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	if got := boundaryViolations(t, c.super); len(got) != 0 {
		t.Fatalf("the migrated database breaks the secret boundary: %v", got)
	}

	// The check can fail: each of these, made as the owner, is reported.
	owner := connect(t, c.owner)
	for _, tt := range []struct {
		ddl, want string
	}{
		{`CREATE VIEW agent.leak AS SELECT source FROM secret.oauth_token`, "depends on secret: agent.leak"},
		{`CREATE VIEW core.leak_via AS SELECT * FROM agent.leak`, "depends on secret: core.leak_via"},
		{`CREATE FUNCTION raw.leak_sql() RETURNS bigint LANGUAGE sql BEGIN ATOMIC SELECT count(*) FROM secret.oauth_token; END`,
			"depends on secret: raw.leak_sql"},
		{`CREATE FUNCTION raw.leak_body() RETURNS bigint LANGUAGE plpgsql AS $$ BEGIN RETURN (SELECT count(*) FROM secret.oauth_token); END $$`,
			"names secret: raw.leak_body"},
		{`CREATE FUNCTION sync.definer() RETURNS int LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'`, "security definer: sync.definer"},
	} {
		if _, err := owner.Exec(ctx, tt.ddl); err != nil {
			t.Fatalf("%s: %v", tt.ddl, err)
		}
		got := boundaryViolations(t, c.super)
		if !slices.Contains(got, tt.want) {
			t.Errorf("after %q the check reports %v, want %q among them", tt.ddl, got, tt.want)
		}
	}
	if got := boundaryViolations(t, c.super); len(got) != 5 {
		t.Errorf("violations after the five mutations: %s", fmt.Sprint(strings.Join(got, "; ")))
	}
}

package store_test

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// catalogSnapshot describes everything Bootstrap and Migrate create: roles, schemas, relations
// with their owners and privileges, view definitions, applied migrations and the projection state.
func catalogSnapshot(t *testing.T, superURL string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, superURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var s string
	err = conn.QueryRow(ctx, `SELECT string_agg(x, E'\n' ORDER BY x) FROM (
		SELECT format('role %s super=%s login=%s createrole=%s createdb=%s password=%s connlimit=%s replication=%s bypassrls=%s inherit=%s',
		              rolname, rolsuper, rolcanlogin, rolcreaterole, rolcreatedb, rolpassword IS NOT NULL,
		              rolconnlimit, rolreplication, rolbypassrls, rolinherit)
		FROM pg_authid WHERE rolname IN ('finstore_owner', 'finstore_app', 'finstore_agent', 'finstore_mcp')
		UNION ALL
		SELECT format('setting role=%s database=%s %s', pg_get_userbyid(setrole), setdatabase,
		              (SELECT string_agg(s, ',' ORDER BY s) FROM unnest(setconfig) s))
		FROM pg_db_role_setting
		UNION ALL
		SELECT format('database acl=%s', datacl) FROM pg_database WHERE datname = current_database()
		UNION ALL
		SELECT format('schema %s owner=%s acl=%s', nspname, pg_get_userbyid(nspowner), nspacl)
		FROM pg_namespace WHERE nspname IN ('raw', 'core', 'core_next', 'sync', 'secret', 'agent', 'public')
		UNION ALL
		SELECT format('rel %s.%s kind=%s owner=%s acl=%s', n.nspname, c.relname, c.relkind,
		              pg_get_userbyid(c.relowner), c.relacl)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname IN ('raw', 'core', 'sync', 'secret', 'agent')
		UNION ALL
		SELECT format('view %s %s', viewname, definition) FROM pg_views WHERE schemaname = 'agent'
		UNION ALL
		SELECT format('goose %s %s', version_id, is_applied) FROM sync.goose_db_version
		UNION ALL
		SELECT format('projection %s %s', last_seq, normaliser_rev) FROM core.projection_state
	) s(x)`).Scan(&s)
	if err != nil {
		t.Fatalf("catalog snapshot: %v", err)
	}
	return s
}

// scramMatches reports whether a stored SCRAM-SHA-256 verifier was made from password.
func scramMatches(t *testing.T, verifier, password string) bool {
	t.Helper()
	// SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>
	method, rest, ok := strings.Cut(verifier, "$")
	if !ok || method != "SCRAM-SHA-256" {
		t.Fatalf("not a SCRAM verifier")
	}
	iterSalt, keys, _ := strings.Cut(rest, "$")
	iterText, saltText, _ := strings.Cut(iterSalt, ":")
	_, serverText, _ := strings.Cut(keys, ":")
	iter, err := strconv.Atoi(iterText)
	if err != nil {
		t.Fatal(err)
	}
	salt, err := base64.StdEncoding.DecodeString(saltText)
	if err != nil {
		t.Fatal(err)
	}
	want, err := base64.StdEncoding.DecodeString(serverText)
	if err != nil {
		t.Fatal(err)
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, iter, sha256.Size)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, salted)
	mac.Write([]byte("Server Key"))
	return hmac.Equal(mac.Sum(nil), want)
}

func TestBootstrapAndMigrateAreIdempotent(t *testing.T) {
	ctx := context.Background()
	super := testpg.Start(t)
	ownerPW := `own'er"; DROP ROLE finstore_app; --%L$1`
	appPW := `app pass\with'quotes`
	if err := store.Bootstrap(ctx, super, ownerPW, appPW); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	owner := testpg.UserURL(t, super, store.RoleOwner)
	if err := store.Migrate(ctx, owner, testRev); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	first := catalogSnapshot(t, super)

	for _, want := range []string{
		"role finstore_owner super=f login=t createrole=f createdb=f password=t",
		"role finstore_app super=f login=t createrole=f createdb=f password=t",
		"role finstore_agent super=f login=f createrole=f createdb=f password=f",
		"role finstore_mcp super=f login=f createrole=f createdb=f password=f connlimit=4 replication=f bypassrls=f inherit=t",
		"setting role=finstore_mcp database=0 default_transaction_read_only=on,idle_in_transaction_session_timeout=30s," +
			"lock_timeout=2s,statement_timeout=15s,temp_file_limit=1GB",
		"schema raw owner=finstore_owner", "schema core owner=finstore_owner", "schema sync owner=finstore_owner",
		"schema secret owner=finstore_owner", "schema agent owner=finstore_owner",
		"projection 0 1",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("catalog lacks %q", want)
		}
	}
	for _, v := range agentViews {
		if !strings.Contains(first, "rel agent."+v+" kind=v owner=finstore_owner") {
			t.Errorf("agent.%s is not a view owned by finstore_owner", v)
		}
	}

	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	for role, pw := range map[string]string{store.RoleOwner: ownerPW, store.RoleApp: appPW} {
		var verifier string
		if err := conn.QueryRow(ctx, "SELECT rolpassword FROM pg_authid WHERE rolname = $1", role).Scan(&verifier); err != nil {
			t.Fatal(err)
		}
		if !scramMatches(t, verifier, pw) {
			t.Errorf("%s: stored password is not the one given", role)
		}
	}

	if err := store.Bootstrap(ctx, super, ownerPW, appPW); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
	if err := store.Migrate(ctx, owner, testRev); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if second := catalogSnapshot(t, super); second != first {
		t.Errorf("second run changed the catalog:\nbefore:\n%s\nafter:\n%s", first, second)
	}
}

func TestBootstrapRefusesEmptyPassword(t *testing.T) {
	// No cluster: the check comes before any connection.
	if err := store.Bootstrap(context.Background(), "postgresql://nobody@/nowhere?host=/nonexistent", "", "x"); err == nil {
		t.Fatal("Bootstrap accepted an empty owner password")
	}
}

func TestMigrateReportsRenormaliseRequired(t *testing.T) {
	c := newCluster(t)
	err := store.Migrate(context.Background(), c.owner, testRev+1)
	if !errors.Is(err, store.ErrRenormaliseRequired) {
		t.Fatalf("Migrate with another revision: want ErrRenormaliseRequired, got %v", err)
	}
	// The stored revision is unchanged, so the original binary still migrates cleanly.
	if err := store.Migrate(context.Background(), c.owner, testRev); err != nil {
		t.Fatalf("Migrate with the stored revision: %v", err)
	}
}

// A binary whose core shape differs from the stored core's (a revision that renamed a column)
// migrates without touching the views, which would not compile against the old core, and asks for
// a renormalise; the swap then recreates the views on the new core.
func TestMigrateWithAnotherRevisionLeavesTheViewsForTheSwap(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	owner := openStore(t, c.owner)
	// Make the stored core look like one built by an older binary: a column under its old name.
	if _, err := owner.DB().Exec(ctx, `DROP VIEW agent.accounts CASCADE;
		ALTER TABLE core.account RENAME COLUMN bank_ref TO old_ref`); err != nil {
		t.Fatal(err)
	}
	// A table a migration added alongside the revision change, not yet granted to anyone.
	if _, err := owner.DB().Exec(ctx, `CREATE TABLE raw.made_up_new (x int)`); err != nil {
		t.Fatal(err)
	}
	err := store.Migrate(ctx, c.owner, testRev+1)
	if !errors.Is(err, store.ErrRenormaliseRequired) {
		t.Fatalf("Migrate over an older core: want ErrRenormaliseRequired, got %v", err)
	}
	// The grants are applied even though the views are left for the swap.
	if _, err := connAs(t, c.super, store.RoleApp).Exec(ctx, `SELECT 1 FROM raw.made_up_new`); err != nil {
		t.Fatalf("finstore_app reading a table added with the revision change: %v", err)
	}
	if err := store.CreateCoreNext(ctx, owner.DB(), testRev+1); err != nil {
		t.Fatal(err)
	}
	if err := store.SwapCore(ctx, owner.DB(), testRev+1); err != nil {
		t.Fatalf("SwapCore: %v", err)
	}
	if err := store.Migrate(ctx, c.owner, testRev+1); err != nil {
		t.Fatalf("Migrate after the swap: %v", err)
	}
	assertAgentBoundary(t, c.super)
}

func durableTables(t *testing.T, conn *pgx.Conn) map[string][]string {
	t.Helper()
	rows, err := conn.Query(context.Background(),
		`SELECT schemaname, tablename FROM pg_tables WHERE schemaname IN ('raw', 'core', 'sync', 'secret') ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string][]string)
	var s, n string
	if _, err := pgx.ForEachRow(rows, []any{&s, &n}, func() error {
		out[s] = append(out[s], s+"."+n)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

var agentViews = []string{"accounts", "transactions", "transaction_history", "balances", "coverage", "sync_status",
	"pot_transactions", "pot_reconciliation", "standing_orders", "direct_debits", "pending_transactions", "identity"}

// assertAgentBoundary checks that finstore_agent can read every agent view and no table.
func assertAgentBoundary(t *testing.T, superURL string) {
	t.Helper()
	ctx := context.Background()
	super, err := pgx.Connect(ctx, superURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = super.Close(ctx) }()
	tables := durableTables(t, super)
	for _, schema := range []string{"raw", "core", "sync", "secret"} {
		if len(tables[schema]) == 0 {
			t.Fatalf("schema %s has no tables to test", schema)
		}
	}

	agent := connAs(t, superURL, store.RoleAgent)
	for _, v := range agentViews {
		rows, err := agent.Query(ctx, "SELECT * FROM agent."+v)
		if err == nil {
			rows.Close()
			err = rows.Err()
		}
		if err != nil {
			t.Errorf("finstore_agent SELECT agent.%s: %v", v, err)
		}
	}
	for _, schema := range []string{"raw", "core", "sync", "secret"} {
		for _, table := range tables[schema] {
			_, err := agent.Exec(ctx, "SELECT 1 FROM "+table+" LIMIT 1")
			requireDenied(t, err, "finstore_agent SELECT "+table)
		}
	}
}

func TestAgentRoleReadsViewsOnly(t *testing.T) {
	c := newCluster(t)
	assertAgentBoundary(t, c.super)
}

func TestAppRoleCannotAlterSchemaOrRaw(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	app, err := pgx.Connect(ctx, c.app)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Close(ctx) }()

	for _, schema := range []string{"raw", "core", "sync", "secret", "agent", "public"} {
		_, err := app.Exec(ctx, "CREATE TABLE "+schema+".intruder (x int)")
		requireDenied(t, err, "finstore_app CREATE TABLE in "+schema)
	}

	// INSERT and SELECT on raw work, through the accessors.
	id, err := store.InsertFetch(ctx, app, store.Fetch{Source: "monzo", Kind: "meta", RunID: 1, StartedAt: time.Now()})
	if err != nil {
		t.Fatalf("finstore_app InsertFetch: %v", err)
	}
	if _, err := store.InsertOperatorEntry(ctx, app, store.OperatorEntry{Kind: "account_add", Payload: []byte(`{}`), EnteredAt: time.Now()}); err != nil {
		t.Fatalf("finstore_app InsertOperatorEntry: %v", err)
	}
	if _, err := store.GetFetch(ctx, app, id); err != nil {
		t.Fatalf("finstore_app GetFetch: %v", err)
	}

	super, err := pgx.Connect(ctx, c.super)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = super.Close(ctx) }()
	for _, table := range durableTables(t, super)["raw"] {
		key := "seq"
		if table == "raw.fetch" {
			key = "id"
		}
		_, err := app.Exec(ctx, "UPDATE "+table+" SET "+key+" = "+key)
		requireDenied(t, err, "finstore_app UPDATE "+table)
		_, err = app.Exec(ctx, "DELETE FROM "+table)
		requireDenied(t, err, "finstore_app DELETE "+table)
		_, err = app.Exec(ctx, "TRUNCATE "+table)
		requireDenied(t, err, "finstore_app TRUNCATE "+table)
	}
	_, err = app.Exec(ctx, "DELETE FROM "+store.GooseTable)
	requireDenied(t, err, "finstore_app DELETE goose table")

	// sync's bigserial keys and secret work for the app.
	if _, err := store.StartRun(ctx, app, "monzo", "hourly", time.Now()); err != nil {
		t.Fatalf("finstore_app StartRun: %v", err)
	}
	if err := store.PutOAuthToken(ctx, app, store.OAuthToken{Source: "monzo", AccessToken: "a", RefreshToken: "r",
		IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now()}); err != nil {
		t.Fatalf("finstore_app PutOAuthToken: %v", err)
	}
	if _, err := store.Coverage(ctx, app); err != nil {
		t.Fatalf("finstore_app Coverage: %v", err)
	}
}

// mcpRole reads finstore_mcp's login flag and password verifier.
func mcpRole(t *testing.T, superURL string) (login bool, verifier *string) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), superURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if err := conn.QueryRow(context.Background(),
		"SELECT rolcanlogin, rolpassword FROM pg_authid WHERE rolname = $1", store.RoleMCP).Scan(&login, &verifier); err != nil {
		t.Fatalf("read finstore_mcp: %v", err)
	}
	return login, verifier
}

func TestEnableMCPLoginWithAndWithoutAPasswordSurvivesBootstrap(t *testing.T) {
	ctx := context.Background()
	super := testpg.Start(t)
	if err := store.Bootstrap(ctx, super, "owner-secret", "app-secret"); err != nil {
		t.Fatal(err)
	}
	if login, pw := mcpRole(t, super); login || pw != nil {
		t.Fatalf("after Bootstrap the role has login=%v password=%v, want NOLOGIN and none", login, pw != nil)
	}
	if _, err := pgx.Connect(ctx, testpg.UserURL(t, super, store.RoleMCP)); err == nil {
		t.Fatal("finstore_mcp logged in before EnableMCPLogin")
	}

	// A password with every character that breaks naive quoting.
	const password = `mcp'pass"; DROP ROLE finstore_app; --%L$1\`
	if err := store.EnableMCPLogin(ctx, super, password); err != nil {
		t.Fatalf("EnableMCPLogin: %v", err)
	}
	login, verifier := mcpRole(t, super)
	if !login || verifier == nil || !scramMatches(t, *verifier, password) {
		t.Fatalf("after EnableMCPLogin: login=%v, password set=%v, matching=%v", login, verifier != nil,
			verifier != nil && scramMatches(t, *verifier, password))
	}
	conn, err := pgx.Connect(ctx, testpg.UserURL(t, super, store.RoleMCP))
	if err != nil {
		t.Fatalf("finstore_mcp cannot log in after EnableMCPLogin: %v", err)
	}
	_ = conn.Close(ctx)

	// Bootstrap again, and again after Migrate: the login and the password stay.
	if err := store.Bootstrap(ctx, super, "owner-secret", "app-secret"); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
	if err := store.Migrate(ctx, testpg.UserURL(t, super, store.RoleOwner), testRev); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := store.Bootstrap(ctx, super, "owner-secret", "app-secret"); err != nil {
		t.Fatalf("third Bootstrap: %v", err)
	}
	login2, verifier2 := mcpRole(t, super)
	if !login2 || verifier2 == nil || *verifier2 != *verifier {
		t.Fatalf("Bootstrap changed the MCP login: login=%v, password unchanged=%v", login2, verifier2 != nil && *verifier2 == *verifier)
	}

	// An empty password turns the login and the password off; EnableMCPLogin is repeatable.
	for range 2 {
		if err := store.EnableMCPLogin(ctx, super, ""); err != nil {
			t.Fatalf("EnableMCPLogin with no password: %v", err)
		}
		if login, pw := mcpRole(t, super); login || pw != nil {
			t.Fatalf("with no password: login=%v password=%v, want NOLOGIN and none", login, pw != nil)
		}
	}
	if _, err := pgx.Connect(ctx, testpg.UserURL(t, super, store.RoleMCP)); err == nil {
		t.Fatal("finstore_mcp logged in after EnableMCPLogin with no password")
	}
	if err := store.EnableMCPLogin(ctx, super, "another"); err != nil {
		t.Fatal(err)
	}
	if login, pw := mcpRole(t, super); !login || pw == nil {
		t.Fatalf("EnableMCPLogin did not turn the login back on: login=%v", login)
	}
}

func TestEnableMCPLoginNeedsBootstrapFirst(t *testing.T) {
	err := store.EnableMCPLogin(context.Background(), testpg.Start(t), "x")
	if err == nil || !strings.Contains(err.Error(), store.RoleMCP) {
		t.Fatalf("EnableMCPLogin before Bootstrap: want an error naming %s, got %v", store.RoleMCP, err)
	}
}

// Bootstrap puts the role's fixed attributes right on every run, so a role someone loosened is
// tightened again, while its login stays as it was.
func TestBootstrapRestoresTheMCPRoleAttributes(t *testing.T) {
	ctx := context.Background()
	super := testpg.Start(t)
	if err := store.Bootstrap(ctx, super, "o", "a"); err != nil {
		t.Fatal(err)
	}
	if err := store.EnableMCPLogin(ctx, super, "pw"); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, testpg.UserURL(t, super, store.RoleOwner), testRev); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, `ALTER ROLE finstore_mcp CREATEDB CREATEROLE CONNECTION LIMIT 99`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `ALTER ROLE finstore_mcp RESET temp_file_limit`); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(ctx, super, "o", "a"); err != nil {
		t.Fatal(err)
	}
	want := "role finstore_mcp super=f login=t createrole=f createdb=f password=t connlimit=4 replication=f bypassrls=f inherit=t"
	if snap := catalogSnapshot(t, super); !strings.Contains(snap, want) || !strings.Contains(snap, "temp_file_limit=1GB") {
		t.Errorf("Bootstrap did not restore the role:\n%s", snap)
	}
}

// A login as finstore_mcp can rewrite the role's own settings, role-wide and for the database (which
// a role-wide setting does not override), and so remove its limits for the next session. Bootstrap
// puts the role back on every run: the catalog is as it was, and a fresh session has the limits.
func TestBootstrapRestoresSettingsTheMCPRoleChangedForItself(t *testing.T) {
	ctx := context.Background()
	super := testpg.Start(t)
	if err := store.Bootstrap(ctx, super, "o", "a"); err != nil {
		t.Fatal(err)
	}
	if err := store.EnableMCPLogin(ctx, super, "pw"); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, testpg.UserURL(t, super, store.RoleOwner), testRev); err != nil {
		t.Fatal(err)
	}
	before := catalogSnapshot(t, super)
	url := testpg.UserURL(t, super, store.RoleMCP)
	show := func(setting string) string {
		t.Helper()
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close(ctx) }()
		var v string
		if err := conn.QueryRow(ctx, "SELECT current_setting($1)", setting).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	wantDefaults := map[string]string{"statement_timeout": "15s", "lock_timeout": "2s",
		"idle_in_transaction_session_timeout": "30s", "default_transaction_read_only": "on", "temp_file_limit": "1GB", "work_mem": "4MB"}
	for k, want := range wantDefaults {
		if got := show(k); got != want {
			t.Fatalf("%s = %q before the tamper, want %q", k, got, want)
		}
	}

	self, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = self.Close(ctx) }()
	for _, stmt := range []string{
		"SET default_transaction_read_only = off",
		"ALTER ROLE finstore_mcp IN DATABASE " + testpg.Database + " SET statement_timeout = 0",
		"ALTER ROLE finstore_mcp IN DATABASE " + testpg.Database + " SET default_transaction_read_only = off",
		"ALTER ROLE finstore_mcp SET statement_timeout = 0",
		"ALTER ROLE finstore_mcp SET work_mem = '1GB'",
	} {
		if _, err := self.Exec(ctx, stmt); err != nil {
			t.Fatalf("the role could not change its own settings (%s): %v", stmt, err)
		}
	}
	// The one it cannot.
	_, err = self.Exec(ctx, "ALTER ROLE finstore_mcp SET temp_file_limit = '10GB'")
	requireDenied(t, err, "ALTER ROLE ... SET temp_file_limit")
	if got := show("statement_timeout"); got != "0" || show("default_transaction_read_only") != "off" {
		t.Fatalf("the tamper did not reach a new session: statement_timeout=%q", got)
	}

	if err := store.Bootstrap(ctx, super, "o", "a"); err != nil {
		t.Fatal(err)
	}
	for k, want := range wantDefaults {
		if got := show(k); got != want {
			t.Errorf("%s = %q after Bootstrap, want %q", k, got, want)
		}
	}
	if after := catalogSnapshot(t, super); after != before {
		t.Errorf("Bootstrap did not restore the catalog:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// The grants name finstore_mcp, so a database whose role is missing cannot be migrated, and the
// error says which role: bootstrap must run first.
func TestMigrateFailsNamingTheMissingMCPRole(t *testing.T) {
	ctx := context.Background()
	super := testpg.Start(t)
	if err := store.Bootstrap(ctx, super, "owner-secret", "app-secret"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "DROP ROLE finstore_mcp"); err != nil {
		t.Fatal(err)
	}
	err = store.Migrate(ctx, testpg.UserURL(t, super, store.RoleOwner), testRev)
	if err == nil || !strings.Contains(err.Error(), store.RoleMCP) {
		t.Fatalf("Migrate without the role: want an error naming %s, got %v", store.RoleMCP, err)
	}
	// Bootstrap puts it back, and Migrate then works.
	if err := store.Bootstrap(ctx, super, "owner-secret", "app-secret"); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, testpg.UserURL(t, super, store.RoleOwner), testRev); err != nil {
		t.Fatalf("Migrate after Bootstrap: %v", err)
	}
}

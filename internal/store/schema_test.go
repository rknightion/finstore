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
		SELECT format('role %s super=%s login=%s createrole=%s createdb=%s password=%s',
		              rolname, rolsuper, rolcanlogin, rolcreaterole, rolcreatedb, rolpassword IS NOT NULL)
		FROM pg_authid WHERE rolname IN ('finstore_owner', 'finstore_app', 'finstore_agent')
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
	"pot_transactions", "pot_reconciliation"}

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

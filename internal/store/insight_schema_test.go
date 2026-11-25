package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rknightion/finstore/internal/store"
)

// The four rule tables and the account scope column are in core and in a rebuilt core_next, with
// the frozen checks.
func TestInsightTablesAndScopeExistInEveryBuild(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	owner := openStore(t, c.owner)
	if err := store.CreateCoreNext(ctx, owner.DB(), testRev); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.DropCoreNext(ctx, owner.DB()) }()
	for _, schema := range []string{"core", "core_next"} {
		for _, table := range []string{"payee_alias", "payee_setting", "series_setting", "category_name"} {
			var ok bool
			if err := owner.DB().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = $1 AND tablename = $2)`,
				schema, table).Scan(&ok); err != nil || !ok {
				t.Errorf("%s.%s: exists %v, %v", schema, table, ok, err)
			}
		}
		var typ, nullable string
		if err := owner.DB().QueryRow(ctx, `SELECT data_type, is_nullable FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = 'account' AND column_name = 'scope'`, schema).Scan(&typ, &nullable); err != nil {
			t.Errorf("%s.account.scope: %v", schema, err)
		} else if typ != "text" || nullable != "YES" {
			t.Errorf("%s.account.scope is %s, nullable %s", schema, typ, nullable)
		}
		// The frozen checks hold in both schemas.
		bad := []string{
			`INSERT INTO ` + schema + `.payee_alias (payee_key, entry_seq, entered_at) VALUES ('n:made up', 1, now())`,
			`INSERT INTO ` + schema + `.payee_setting (payee_key, flow_class, entry_seq, entered_at) VALUES ('n:made up', 'bogus', 1, now())`,
			`INSERT INTO ` + schema + `.series_setting (series_key, recurring, entry_seq, entered_at) VALUES ('sub:made up', 'bogus', 1, now())`,
		}
		for _, q := range bad {
			if _, err := owner.DB().Exec(ctx, q); err == nil {
				t.Errorf("%s: accepted %q", schema, q)
			}
		}
		good := []string{
			`INSERT INTO ` + schema + `.payee_alias (payee_key, separate, entry_seq, entered_at) VALUES ('n:made up', true, 1, now())`,
			`INSERT INTO ` + schema + `.payee_setting (payee_key, flow_class, employer, entry_seq, entered_at) VALUES ('n:made up', 'income', true, 1, now())`,
			`INSERT INTO ` + schema + `.series_setting (series_key, recurring, entry_seq, entered_at) VALUES ('sub:made up', 'confirm', 1, now())`,
			`INSERT INTO ` + schema + `.category_name (category, name, entry_seq, entered_at) VALUES ('eating_out', 'Eating out', 1, now())`,
		}
		for _, q := range good {
			if _, err := owner.DB().Exec(ctx, q); err != nil {
				t.Errorf("%s: %q: %v", schema, q, err)
			}
		}
	}
}

func TestAccountScopeCheck(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	owner := openStore(t, c.owner)
	acc := testAccount("scope-check", "current", "api")
	if err := mustCore(t, owner.DB(), store.SchemaCore).UpsertAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.DB().Exec(ctx, `UPDATE core.account SET scope = 'family' WHERE slug = 'scope-check'`); err == nil {
		t.Error("scope 'family' was accepted")
	}
	for _, s := range []string{"personal", "joint", "business"} {
		if _, err := owner.DB().Exec(ctx, `UPDATE core.account SET scope = $1 WHERE slug = 'scope-check'`, s); err != nil {
			t.Errorf("scope %q: %v", s, err)
		}
	}
}

// UpsertAccount writes scope, and a path that reads an account and writes it back keeps it.
func TestUpsertAccountReadsAndKeepsScope(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	core := mustCore(t, openStore(t, c.app).DB(), store.SchemaCore)
	acc := testAccount("scope-keep", "current", "api")
	if err := core.UpsertAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	if got, err := core.GetAccount(ctx, acc.ID); err != nil || got.Scope != nil {
		t.Fatalf("a new account has scope %v (%v), want nil", got.Scope, err)
	}
	acc.Scope = ptr("joint")
	if err := core.UpsertAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	read, err := core.GetAccount(ctx, acc.ID)
	if err != nil || read.Scope == nil || *read.Scope != "joint" {
		t.Fatalf("scope after the upsert: %v (%v), want joint", read.Scope, err)
	}
	// The API sync path: read, change what the source owns, write back.
	read.Name = "renamed by the source"
	if err := core.UpsertAccount(ctx, read); err != nil {
		t.Fatal(err)
	}
	again, err := core.GetAccount(ctx, acc.ID)
	if err != nil || again.Scope == nil || *again.Scope != "joint" || again.Name != "renamed by the source" {
		t.Fatalf("after a sync-style upsert: scope %v name %q (%v)", again.Scope, again.Name, err)
	}
	// An operator clear writes nil.
	again.Scope = nil
	if err := core.UpsertAccount(ctx, again); err != nil {
		t.Fatal(err)
	}
	if cleared, err := core.GetAccount(ctx, acc.ID); err != nil || cleared.Scope != nil {
		t.Fatalf("scope after a clear: %v (%v)", cleared.Scope, err)
	}
}

// A database built by revision 6, so with no payee function and no insight tables, migrates to
// "renormalise required" without error, leaves the function absent, and the swap then creates it.
func TestMigrateOverRevisionSixWithoutTheFunction(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	owner := openStore(t, c.owner)
	if _, err := owner.DB().Exec(ctx, `DROP FUNCTION agent.normalise_payee_name(text);
		DROP TABLE core.payee_alias, core.payee_setting, core.series_setting, core.category_name;
		ALTER TABLE core.account DROP COLUMN scope;
		UPDATE core.projection_state SET normaliser_rev = 6`); err != nil {
		t.Fatal(err)
	}
	err := store.Migrate(ctx, c.owner, 7)
	if !errors.Is(err, store.ErrRenormaliseRequired) {
		t.Fatalf("Migrate over a revision 6 core: want ErrRenormaliseRequired, got %v", err)
	}
	var n int
	if err := owner.DB().QueryRow(ctx, `SELECT count(*) FROM pg_proc WHERE proname = 'normalise_payee_name'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the function after a revision-mismatch migrate: %d (%v), want 0", n, err)
	}
	if err := store.CreateCoreNext(ctx, owner.DB(), 7); err != nil {
		t.Fatal(err)
	}
	if err := store.SwapCore(ctx, owner.DB(), 7); err != nil {
		t.Fatalf("SwapCore: %v", err)
	}
	if err := owner.DB().QueryRow(ctx, `SELECT count(*) FROM pg_proc WHERE proname = 'normalise_payee_name'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the function after the swap: %d (%v), want 1", n, err)
	}
}

// The function on hand-written names. A name that normalises to nothing gives NULL, so a view
// can fall through to key order 4 with COALESCE.
func TestNormalisePayeeName(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	owner := openStore(t, c.owner)
	cases := []struct {
		in   *string
		want *string
	}{
		{ptr("Acme Widgets"), ptr("acme widgets")},
		{ptr("  ACME   Widgets!! "), ptr("acme widgets")},
		{ptr("Joe's Cafe & Bar"), ptr("joe's cafe & bar")},
		{ptr("Acme*Widgets/London"), ptr("acme widgets london")},
		// reference tokens: six or more characters with a digit
		{ptr("Acme Widgets REF123456"), ptr("acme widgets")},
		{ptr("Acme AB12CD Widgets"), ptr("acme widgets")},
		{ptr("Acme ab123 Widgets"), ptr("acme ab123 widgets")},
		// date tokens
		{ptr("Acme Widgets 01NOV26"), ptr("acme widgets")},
		{ptr("Acme Widgets 2026-11-01"), ptr("acme widgets")},
		{ptr("Acme 01/11/2026 Widgets"), ptr("acme widgets")},
		{ptr("Acme Widgets 1/1/26"), ptr("acme widgets")},
		// a trailing run of digits
		{ptr("Acme Widgets 4821"), ptr("acme widgets")},
		{ptr("Acme Widgets 12 345"), ptr("acme widgets")},
		{ptr("Acme 24 Widgets"), ptr("acme 24 widgets")},
		// collector prefix
		{ptr("Collector Re Acme Widgets"), ptr("acme widgets")},
		{ptr("collector re acme widgets ltd"), ptr("acme widgets")},
		{ptr("Acme Re Widgets"), ptr("widgets")},
		{ptr("Collector Re"), ptr("collector re")},
		// company suffixes
		{ptr("Acme Widgets Ltd"), ptr("acme widgets")},
		{ptr("Acme Widgets Limited"), ptr("acme widgets")},
		{ptr("Acme Widgets PLC"), ptr("acme widgets")},
		{ptr("Acme Widgets LLP"), ptr("acme widgets")},
		{ptr("Acme Widgets Inc."), ptr("acme widgets")},
		{ptr("Acme Widgets UK Ltd"), ptr("acme widgets")},
		{ptr("Ltd Acme"), ptr("ltd acme")},
		// the rules compose
		{ptr("Collector RE Acme Widgets Ltd REF99887766 01NOV26 123"), ptr("acme widgets")},
		// nothing left
		{ptr(""), nil},
		{ptr("   "), nil},
		{ptr("!!! ---"), nil},
		{ptr("REF123456 01NOV26 7788"), nil},
		{ptr("Ltd"), nil},
		{nil, nil},
	}
	for _, tc := range cases {
		var got *string
		if err := owner.DB().QueryRow(ctx, `SELECT agent.normalise_payee_name($1)`, tc.in).Scan(&got); err != nil {
			t.Errorf("%s: %v", show(tc.in), err)
			continue
		}
		switch {
		case got == nil && tc.want == nil:
		case got == nil || tc.want == nil || *got != *tc.want:
			t.Errorf("normalise(%s) = %s, want %s", show(tc.in), show(got), show(tc.want))
		}
	}
}

func show(s *string) string {
	if s == nil {
		return "NULL"
	}
	return `"` + *s + `"`
}

// Every role that reads the views can execute the function, by its explicit grant: with the
// default PUBLIC grant revoked it still works, and a later migrate keeps it so.
func TestNormalisePayeeNameIsExecutableByEveryRole(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	roles := []string{store.RoleApp, store.RoleAgent, store.RoleMCP}
	if _, err := openStore(t, c.owner).DB().Exec(ctx, `REVOKE EXECUTE ON FUNCTION agent.normalise_payee_name(text) FROM PUBLIC`); err != nil {
		t.Fatal(err)
	}
	// Migrate regrants in the same file as the function, so the explicit grants are what is left.
	if err := store.Migrate(ctx, c.owner, testRev); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(t, c.owner).DB().Exec(ctx, `REVOKE EXECUTE ON FUNCTION agent.normalise_payee_name(text) FROM PUBLIC`); err != nil {
		t.Fatal(err)
	}
	for _, role := range roles {
		var got *string
		if err := connAs(t, c.super, role).QueryRow(ctx, `SELECT agent.normalise_payee_name('Acme Widgets Ltd')`).Scan(&got); err != nil {
			t.Errorf("%s: %v", role, err)
		} else if got == nil || *got != "acme widgets" {
			t.Errorf("%s: got %v", role, got)
		}
	}
}

package app

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// The writing commands of payee, category and account scope validate before they write: a refused
// command exits non-zero with its reason and leaves no raw row, and a valid one is normalised in
// the same transaction.
func TestPayeeCategoryAndScopeCommandsWriteOrRefuseWithoutARawRow(t *testing.T) {
	ctx := context.Background()
	super := testpg.Start(t)
	env := map[string]string{
		config.EnvSuperuserDatabaseURL: super,
		config.EnvOwnerDatabaseURL:     testpg.UserURL(t, super, store.RoleOwner),
		config.EnvDatabaseURL:          testpg.UserURL(t, super, store.RoleApp),
		config.EnvOwnerPassword:        "owner-password",
		config.EnvAppPassword:          "app-password",
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	run := func(fn func(context.Context, Env) int) (int, string, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := fn(ctx, Env{Cfg: cfg, Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr})
		return code, stdout.String(), stderr.String()
	}
	mustRun := func(fn func(context.Context, Env) int) string {
		t.Helper()
		code, out, stderr := run(fn)
		if code != ExitOK {
			t.Fatalf("exit %d: %s", code, stderr)
		}
		return out
	}
	mustRun(Bootstrap)
	mustRun(Migrate)
	mustRun(func(ctx context.Context, e Env) int {
		return AccountAdd(ctx, e, AccountSpec{Slug: "bank-joint", Type: "joint", Currency: "GBP", Name: "made-up"})
	})
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	core, err := store.NewCore(st.DB(), store.SchemaCore)
	if err != nil {
		t.Fatal(err)
	}
	// A pot, which takes its parent's scope.
	if err := core.UpsertAccount(ctx, store.Account{ID: uuid.New(), Source: "manual", SourceKey: "pot-1", Slug: "made-up-pot",
		Type: "pot", Feed: "file", Currency: "GBP", Name: "made-up pot"}); err != nil {
		t.Fatal(err)
	}
	entries := func() int {
		t.Helper()
		var n int
		if err := st.DB().QueryRow(ctx, `SELECT count(*) FROM raw.operator_entry`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	count := func(table string) int {
		t.Helper()
		var n int
		if err := st.DB().QueryRow(ctx, "SELECT count(*) FROM core."+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Each refusal says why, exits non-zero and writes nothing.
	before := entries()
	refused := []struct {
		name string
		fn   func(context.Context, Env) int
		why  string
	}{
		{"employer without income", func(ctx context.Context, e Env) int { return PayeeClass(ctx, e, "n:made-up", "spending", true) }, "employer"},
		{"bad key", func(ctx context.Context, e Env) int { return PayeeSeparate(ctx, e, "made-up") }, "payee key"},
		{"bad target", func(ctx context.Context, e Env) int { return PayeeMerge(ctx, e, "n:made-up", "zz:other") }, "into_key"},
		{"merge into itself", func(ctx context.Context, e Env) int { return PayeeMerge(ctx, e, "n:made-up", "n:made-up") }, "itself"},
		{"unknown class", func(ctx context.Context, e Env) int { return PayeeClass(ctx, e, "n:made-up", "gift", false) }, "flow_class"},
		{"bad decision", func(ctx context.Context, e Env) int { return PayeeRecurring(ctx, e, "sub:n:made-up", "maybe") }, "recurring"},
		{"bad series", func(ctx context.Context, e Env) int { return PayeeRecurring(ctx, e, "n:made-up", "confirm") }, "series_key"},
		{"clear what", func(ctx context.Context, e Env) int { return PayeeClear(ctx, e, "n:made-up", "everything", "") }, "clears"},
		{"blank name", func(ctx context.Context, e Env) int { return PayeeRename(ctx, e, "n:made-up", " ") }, "name"},
		{"blank category name", func(ctx context.Context, e Env) int { return CategoryName(ctx, e, "groceries", "") }, "name"},
		{"scope on a pot", func(ctx context.Context, e Env) int { return AccountScope(ctx, e, "made-up-pot", ptr("joint")) }, "pot"},
		{"scope on an unknown account", func(ctx context.Context, e Env) int { return AccountScope(ctx, e, "no-such", ptr("joint")) }, "no account"},
		{"unknown scope", func(ctx context.Context, e Env) int { return AccountScope(ctx, e, "bank-joint", ptr("family")) }, "scope"},
	}
	for _, c := range refused {
		code, out, stderr := run(c.fn)
		if code == ExitOK || !strings.Contains(stderr, c.why) || out != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, want a refusal naming %q", c.name, code, out, stderr, c.why)
		}
		if n := entries(); n != before {
			t.Fatalf("%s left %d raw operator entries", c.name, n-before)
		}
	}

	// Valid commands write one entry each and normalise at once.
	mustRun(func(ctx context.Context, e Env) int { return PayeeMerge(ctx, e, "n:made-up", "mg:grp_1") })
	mustRun(func(ctx context.Context, e Env) int { return PayeeSeparate(ctx, e, "n:made-up") })
	mustRun(func(ctx context.Context, e Env) int { return PayeeRename(ctx, e, "mg:grp_1", "Made Up") })
	mustRun(func(ctx context.Context, e Env) int { return PayeeClass(ctx, e, "mg:grp_1", "income", true) })
	mustRun(func(ctx context.Context, e Env) int { return PayeeCategory(ctx, e, "mg:grp_1", "salary") })
	mustRun(func(ctx context.Context, e Env) int { return PayeeRecurring(ctx, e, "sub:mg:grp_1", "not-recurring") })
	mustRun(func(ctx context.Context, e Env) int { return CategoryName(ctx, e, "salary", "Pay") })
	out := mustRun(func(ctx context.Context, e Env) int { return AccountScope(ctx, e, "bank-joint", ptr("business")) })
	if !strings.Contains(out, "business") {
		t.Fatalf("confirmation %q", out)
	}
	if n := entries(); n != before+8 {
		t.Fatalf("%d entries, want %d", n, before+8)
	}
	a, err := core.GetPayeeAlias(ctx, "n:made-up")
	if err != nil || !a.Separate || a.IntoKey == nil || *a.IntoKey != "mg:grp_1" {
		t.Fatalf("alias %+v, %v", a, err)
	}
	s, err := core.GetPayeeSetting(ctx, "mg:grp_1")
	if err != nil || s.Name == nil || *s.Name != "Made Up" || s.FlowClass == nil || *s.FlowClass != "income" ||
		s.Employer == nil || !*s.Employer || s.Category == nil || *s.Category != "salary" {
		t.Fatalf("setting %+v, %v", s, err)
	}
	var recurring string
	if err := st.DB().QueryRow(ctx, `SELECT recurring FROM core.series_setting WHERE series_key = 'sub:mg:grp_1'`).Scan(&recurring); err != nil || recurring != "not_recurring" {
		t.Fatalf("series decision %q, %v", recurring, err)
	}
	acc, err := core.GetAccountBySlug(ctx, "bank-joint")
	if err != nil || acc.Scope == nil || *acc.Scope != "business" {
		t.Fatalf("account %+v, %v", acc, err)
	}
	if count("category_name") != 1 {
		t.Fatal("category name not written")
	}

	// Each clear writes an entry and takes effect.
	mustRun(func(ctx context.Context, e Env) int { return PayeeClear(ctx, e, "n:made-up", "merge", "") })
	if a, err := core.GetPayeeAlias(ctx, "n:made-up"); err != nil || !a.Separate || a.IntoKey != nil {
		t.Fatalf("alias after clearing the merge %+v, %v", a, err)
	}
	mustRun(func(ctx context.Context, e Env) int { return PayeeClear(ctx, e, "n:made-up", "separate", "") })
	mustRun(func(ctx context.Context, e Env) int { return PayeeClear(ctx, e, "mg:grp_1", "class", "") })
	mustRun(func(ctx context.Context, e Env) int { return PayeeClear(ctx, e, "", "", "sub:mg:grp_1") })
	mustRun(func(ctx context.Context, e Env) int { return CategoryClear(ctx, e, "salary") })
	mustRun(func(ctx context.Context, e Env) int { return AccountScope(ctx, e, "bank-joint", nil) })
	if count("payee_alias") != 0 || count("series_setting") != 0 || count("category_name") != 0 {
		t.Fatal("a clear left its row")
	}
	if s, err := core.GetPayeeSetting(ctx, "mg:grp_1"); err != nil || s.FlowClass != nil || s.Employer != nil || s.Name == nil {
		t.Fatalf("setting after clearing the class %+v, %v", s, err)
	}
	if acc, _ := core.GetAccountBySlug(ctx, "bank-joint"); acc.Scope != nil {
		t.Fatalf("scope %q after a clear", *acc.Scope)
	}
}

func ptr[T any](v T) *T { return &v }

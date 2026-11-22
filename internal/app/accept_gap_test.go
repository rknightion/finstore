package app

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/importer/importertest"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// account accept-gap records an accept_gap entry only for a gap the account has whose statements
// join; anything else is refused with a reason and records nothing. An accepted gap leaves the
// coverage view, and stays out of it after a rebuild from raw.
func TestAcceptGapAcceptsOnlyAGapWithNoDifferenceAndSurvivesARebuild(t *testing.T) {
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
	command := func(stdin string, fn func(context.Context, Env) int) (int, string) {
		t.Helper()
		var stderr bytes.Buffer
		code := fn(ctx, Env{Cfg: cfg, Stdin: strings.NewReader(stdin), Stdout: io.Discard, Stderr: &stderr})
		return code, stderr.String()
	}
	mustRun := func(fn func(context.Context, Env) int) {
		t.Helper()
		if code, stderr := command("", fn); code != ExitOK {
			t.Fatalf("exit %d: %s", code, stderr)
		}
	}
	// A made-up Halifax statement for the whole of month in 2030 that adds 100.00.
	statement := func(month time.Month, opening int64) string {
		day := func(d int) string { return time.Date(2030, month, d, 0, 0, 0, 0, time.UTC).Format("02 Jan 06") }
		g := importertest.New(opening, []importertest.Row{
			{Date: day(3), Desc: "MADE UP PAYROLL", Type: "FPI", In: 12000},
			{Date: day(20), Desc: "MADE UP SHOP", Type: "DEB", Out: 2000},
		})
		g.Start = time.Date(2030, month, 1, 0, 0, 0, 0, time.UTC)
		g.End = g.Start.AddDate(0, 1, -1)
		g.SortCode, g.Account = "11-22-33", "12345678"
		return string(g.PDF())
	}
	mustRun(Bootstrap)
	mustRun(Migrate)
	for slug, marchOpening := range map[string]int64{"bank-joint": 50000, "bank-other": 60000} {
		mustRun(func(ctx context.Context, e Env) int {
			return AccountAdd(ctx, e, AccountSpec{Slug: slug, Type: "joint", Currency: "GBP", Name: "made-up"})
		})
		// January closes at 500.00; February is missing.
		for _, body := range []string{statement(time.January, 40000), statement(time.March, marchOpening)} {
			if code, stderr := command(body, func(ctx context.Context, e Env) int { return Import(ctx, e, slug, FormatHalifaxPDF, "made-up") }); code != ExitOK {
				t.Fatalf("import into %s: exit %d: %s", slug, code, stderr)
			}
		}
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	entries := func() int {
		t.Helper()
		var n int
		if err := st.DB().QueryRow(ctx, `SELECT count(*) FROM raw.operator_entry WHERE kind = 'accept_gap'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	coverage := func(slug string) store.CoverageRow {
		t.Helper()
		rows, err := store.Coverage(ctx, st.DB())
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.AccountSlug == slug {
				return r
			}
		}
		t.Fatalf("no coverage row for %s", slug)
		return store.CoverageRow{}
	}
	jan31, mar1 := time.Date(2030, 1, 31, 0, 0, 0, 0, time.UTC), time.Date(2030, 3, 1, 0, 0, 0, 0, time.UTC)
	accept := func(slug string, from, to time.Time) (int, string) {
		return command("", func(ctx context.Context, e Env) int { return AcceptGap(ctx, e, slug, from, to, "made-up: idle month") })
	}

	for _, tc := range []struct {
		name     string
		slug     string
		from, to time.Time
		words    string
	}{
		{"a gap whose statements do not join", "bank-other", jan31, mar1, "closing balance before it differs from the opening balance after it"},
		{"a gap the account does not have", "bank-joint", jan31, mar1.AddDate(0, 0, 1), "has no gap from 2030-01-31 to 2030-03-02"},
		{"an unknown account", "no-such-account", jan31, mar1, "no account has the slug"},
	} {
		code, stderr := accept(tc.slug, tc.from, tc.to)
		if code != ExitError || !strings.Contains(stderr, tc.words) {
			t.Fatalf("%s: exit %d, stderr %q; want %d and %q", tc.name, code, stderr, ExitError, tc.words)
		}
		if n := entries(); n != 0 {
			t.Fatalf("%s: %d accept_gap entries recorded, want none", tc.name, n)
		}
	}
	if got := coverage("bank-other"); got.ReconciliationState != store.StateMismatch || len(got.Gaps) != 1 {
		t.Fatalf("bank-other after a refused accept-gap: %+v", got)
	}

	if code, stderr := accept("bank-joint", jan31, mar1); code != ExitOK {
		t.Fatalf("accept-gap on a gap with no difference: exit %d: %s", code, stderr)
	}
	if n := entries(); n != 1 {
		t.Fatalf("%d accept_gap entries, want 1", n)
	}
	check := func(when string) {
		t.Helper()
		if got := coverage("bank-joint"); got.ReconciliationState != store.StateReconciled || len(got.Gaps) != 0 {
			t.Fatalf("bank-joint %s: %+v, want reconciled with no gap", when, got)
		}
	}
	check("after accept-gap")
	mustRun(func(ctx context.Context, e Env) int { return Renormalise(ctx, e, false) })
	check("after renormalise")
}

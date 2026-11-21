package app

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/importer/importertest"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// A rejected import says why, to the operator in words and on the run record as a fixed category,
// and never with anything from the file. Each reason has its own words: a test of the exit code
// alone would pass if every rejection read the same.
func TestRejectedImportReportsItsReason(t *testing.T) {
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
	// command runs fn and returns its exit code and what it wrote to standard error.
	command := func(stdin string, fn func(context.Context, Env) int) (int, string) {
		t.Helper()
		var stderr bytes.Buffer
		code := fn(ctx, Env{Cfg: cfg, Stdin: strings.NewReader(stdin), Stdout: io.Discard, Stderr: &stderr})
		return code, stderr.String()
	}
	for _, fn := range []func(context.Context, Env) int{
		Bootstrap, Migrate,
		func(ctx context.Context, e Env) int {
			return AccountAdd(ctx, e, AccountSpec{Slug: "bank-joint", Type: "joint", Currency: "GBP", Name: "made-up", ExpectsImports: true})
		},
	} {
		if code, stderr := command("", fn); code != ExitOK {
			t.Fatalf("setup exit %d: %s", code, stderr)
		}
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// lastRun is the outcome and error category of the newest import run.
	lastRun := func() (outcome string, category *string) {
		t.Helper()
		err := st.DB().QueryRow(ctx, `SELECT outcome, error_category FROM sync.run WHERE kind = 'import' ORDER BY id DESC LIMIT 1`).
			Scan(&outcome, &category)
		if err != nil {
			t.Fatal(err)
		}
		return outcome, category
	}
	importFile := func(format, body string) (int, string) {
		return command(body, func(ctx context.Context, e Env) int { return Import(ctx, e, "bank-joint", format, "made-up") })
	}

	rows := []importertest.Row{{Date: "03 Jan 30", Desc: "MADE-UP EMPLOYER", Type: "FPI", In: 123456}}
	good := importertest.New(100000, rows)
	if code, stderr := importFile(FormatHalifaxPDF, string(good.PDF())); code != ExitOK {
		t.Fatalf("a sound statement: exit %d: %s", code, stderr)
	}

	for _, tc := range []struct {
		name         string
		format, body string
		category     string
		words        string
	}{
		{
			name: "a statement whose balances do not follow", format: FormatManualCSV, category: "check_failed",
			body:  "date,type,amount,balance_after,description\n2030-02-01,PAY,10.00,10.00,made-up\n2030-02-02,PAY,10.00,25.00,made-up\n",
			words: "the file could not be read as this format, or failed one of its checks",
		},
		{
			name: "a statement of another account", format: FormatHalifaxPDF, category: "account_mismatch",
			body: func() string {
				other := importertest.New(100000, []importertest.Row{{Date: "04 Jan 30", Desc: "MADE-UP SHOP", Type: "DEB", Out: 1250}})
				other.Account = "99999999"
				return string(other.PDF())
			}(),
			words: "the sort code and account number in the file are not the ones in the statements already imported",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stderr := importFile(tc.format, tc.body)
			if code != ExitRejected {
				t.Fatalf("exit %d, want %d: %s", code, ExitRejected, stderr)
			}
			if want := "rejected: " + tc.words; !strings.Contains(stderr, want) {
				t.Errorf("stderr %q does not say %q", stderr, want)
			}
			if outcome, category := lastRun(); outcome != "failed" || category == nil || *category != tc.category {
				t.Errorf("run outcome %q, category %v, want failed %q", outcome, category, tc.category)
			}
			// Given again, the same file is rejected for being a repeat, which says so.
			code, stderr = importFile(tc.format, tc.body)
			if code != ExitRejected {
				t.Fatalf("repeat: exit %d, want %d: %s", code, ExitRejected, stderr)
			}
			if want := "rejected: this exact file was given before and rejected"; !strings.Contains(stderr, want) {
				t.Errorf("repeat: stderr %q does not say %q", stderr, want)
			}
			if outcome, category := lastRun(); outcome != "failed" || category == nil || *category != rejectionRepeated {
				t.Errorf("repeat: run outcome %q, category %v, want failed %q", outcome, category, rejectionRepeated)
			}
		})
	}
}

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// payeeRig is a migrated cluster with one account and a few made-up transactions.
type payeeRig struct {
	t   *testing.T
	cfg config.Config
	st  *store.Store
}

func (r *payeeRig) run(fn func(context.Context, Env) int) (int, string, string) {
	r.t.Helper()
	var stdout, stderr bytes.Buffer
	code := fn(context.Background(), Env{Cfg: r.cfg, Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr})
	return code, stdout.String(), stderr.String()
}

func (r *payeeRig) mustRun(fn func(context.Context, Env) int) string {
	r.t.Helper()
	code, out, stderr := r.run(fn)
	if code != ExitOK {
		r.t.Fatalf("exit %d: %s", code, stderr)
	}
	return out
}

func (r *payeeRig) entries() int {
	r.t.Helper()
	var n int
	if err := r.st.DB().QueryRow(context.Background(), `SELECT count(*) FROM raw.operator_entry`).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

// keyOf returns the payee key of the transactions with this description.
func (r *payeeRig) keyOf(desc string) string {
	r.t.Helper()
	var key string
	if err := r.st.DB().QueryRow(context.Background(),
		`SELECT payee_key FROM agent.cash_flows WHERE description = $1 LIMIT 1`, desc).Scan(&key); err != nil {
		r.t.Fatal(err)
	}
	return key
}

func newPayeeRig(t *testing.T) *payeeRig {
	t.Helper()
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
	r := &payeeRig{t: t, cfg: cfg}
	r.mustRun(Bootstrap)
	r.mustRun(Migrate)
	r.mustRun(func(ctx context.Context, e Env) int {
		return AccountAdd(ctx, e, AccountSpec{Slug: "bank-main", Type: "current", Currency: "GBP", Name: "made-up"})
	})
	r.st, err = store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.st.Close)
	core, err := store.NewCore(r.st.DB(), store.SchemaCore)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := core.GetAccountBySlug(ctx, "bank-main")
	if err != nil {
		t.Fatal(err)
	}
	// Three transactions at one made-up shop, two spelled one way and one another, two at a cafe,
	// one at a third payee.
	n := 0
	add := func(desc string, amount int64) {
		n++
		day := time.Date(2026, 3, n, 0, 0, 0, 0, time.UTC)
		tx := store.Transaction{ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("%s/%d", desc, n))), AccountID: acc.ID,
			Source: "manual", SourceKey: fmt.Sprintf("tx_%d", n), AmountMinor: amount, Currency: "GBP", TxnDate: day,
			Status: "settled", Description: desc}
		if _, err := core.ApplyTransaction(ctx, tx, []byte(`{}`), []byte(fmt.Sprintf("h%d", n)), day, int64(n)); err != nil {
			t.Fatal(err)
		}
	}
	add("ALPHA SHOP", -1000)
	add("ALPHA SHOP", -2000)
	add("Alpha Shop Ltd", -500)
	add("BETA CAFE", -300)
	add("BETA CAFE", -400)
	add("GAMMA UTILITIES", -5000)
	return r
}

var stateLine = regexp.MustCompile(`^(.+) "([^"]*)" class=(\S*) category=(\S*) keys=(\d+)\n$`)

func TestPayeeListAndShow(t *testing.T) {
	r := newPayeeRig(t)
	beta := r.keyOf("BETA CAFE")

	// list prints one row per payee and currency with its id, name, class and counts.
	out := r.mustRun(func(ctx context.Context, e Env) int { return PayeeList(ctx, e, "", 50) })
	for _, want := range []string{"PAYEE", "NAME", "CLASS", "TXNS", beta, "GAMMA UTILITIES", "spending"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output lacks %q:\n%s", want, out)
		}
	}
	// A query matches the name case-insensitively and a limit cuts the rows.
	out = r.mustRun(func(ctx context.Context, e Env) int { return PayeeList(ctx, e, "beta c", 50) })
	if !strings.Contains(out, beta) || strings.Contains(out, "GAMMA") {
		t.Errorf("query output:\n%s", out)
	}
	out = r.mustRun(func(ctx context.Context, e Env) int { return PayeeList(ctx, e, "", 1) })
	if lines := strings.Count(out, "\n"); lines != 2 {
		t.Errorf("limit 1 printed %d lines:\n%s", lines, out)
	}

	// show prints the keys, the name variants and the rules with their status.
	r.mustRun(func(ctx context.Context, e Env) int { return PayeeRename(ctx, e, beta, "Beta Coffee") })
	out = r.mustRun(func(ctx context.Context, e Env) int { return PayeeShow(ctx, e, beta) })
	for _, want := range []string{"Beta Coffee", "keys:", beta, "names:", "BETA CAFE", "payee_setting", "matches", "series:"} {
		if !strings.Contains(out, want) {
			t.Errorf("show output lacks %q:\n%s", want, out)
		}
	}
	// A key nothing knows is refused without echoing it.
	code, stdout, stderr := r.run(func(ctx context.Context, e Env) int { return PayeeShow(ctx, e, "n:no-such-payee") })
	if code == ExitOK || stdout != "" || strings.Contains(stderr, "no-such-payee") || !strings.Contains(stderr, "--key") {
		t.Errorf("show of an unknown key: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestPayeeWritesResolveToTheCanonicalKeyAndFillTheNameSample(t *testing.T) {
	r := newPayeeRig(t)
	ctx := context.Background()
	alpha, beta := r.keyOf("ALPHA SHOP"), r.keyOf("BETA CAFE")
	core, err := store.NewCore(r.st.DB(), store.SchemaCore)
	if err != nil {
		t.Fatal(err)
	}

	// A merge fills nothing but the alias; a later setting on the merged key lands on the target.
	r.mustRun(func(ctx context.Context, e Env) int { return PayeeMerge(ctx, e, alpha, beta) })
	r.mustRun(func(ctx context.Context, e Env) int { return PayeeRename(ctx, e, alpha, "Corner Trade") })
	if _, err := core.GetPayeeSetting(ctx, alpha); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a setting was written against the merged key: %v", err)
	}
	s, err := core.GetPayeeSetting(ctx, beta)
	if err != nil || s.Name == nil || *s.Name != "Corner Trade" {
		t.Fatalf("setting on the canonical key %+v, %v", s, err)
	}
	// name_sample is the most frequent raw name of the canonical key, not of the key given.
	if s.NameSample == nil || *s.NameSample != "BETA CAFE" {
		t.Errorf("name_sample %v, want BETA CAFE", s.NameSample)
	}
	a, err := core.GetPayeeAlias(ctx, alpha)
	if err != nil || a.NameSample == nil || *a.NameSample != "ALPHA SHOP" {
		t.Errorf("alias %+v, %v: name_sample should be the most frequent raw name", a, err)
	}

	// The state line names the payee, its class and category, and the keys it holds.
	r.mustRun(func(ctx context.Context, e Env) int { return PayeeClass(ctx, e, alpha, "spending", false) })
	out := r.mustRun(func(ctx context.Context, e Env) int { return PayeeCategory(ctx, e, alpha, "groceries") })
	m := stateLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("state line %q", out)
	}
	if m[1] != beta || m[2] != "Corner Trade" || m[3] != "spending" || m[4] != "groceries" || m[5] != "2" {
		t.Errorf("state line %q", out)
	}
	// Merge and clear print it too.
	out = r.mustRun(func(ctx context.Context, e Env) int { return PayeeClear(ctx, e, beta, "category", "") })
	if m := stateLine.FindStringSubmatch(out); m == nil || m[4] != "-" {
		t.Errorf("state line after a clear %q", out)
	}

	// A key no transaction has is written as given, with no name_sample.
	r.mustRun(func(ctx context.Context, e Env) int { return PayeeRename(ctx, e, "n:never seen", "Unseen") })
	s, err = core.GetPayeeSetting(ctx, "n:never seen")
	if err != nil || s.NameSample != nil || s.Name == nil || *s.Name != "Unseen" {
		t.Errorf("setting of an unseen key %+v, %v", s, err)
	}
}

func TestPayeeMergeRefusals(t *testing.T) {
	r := newPayeeRig(t)
	alpha, beta, gamma := r.keyOf("ALPHA SHOP"), r.keyOf("BETA CAFE"), r.keyOf("GAMMA UTILITIES")
	r.mustRun(func(ctx context.Context, e Env) int { return PayeeMerge(ctx, e, alpha, beta) })
	r.mustRun(func(ctx context.Context, e Env) int { return PayeeMerge(ctx, e, beta, gamma) })
	// Two keys no transaction has, chained to alpha: ghost one into ghost two into alpha.
	const ghost1, ghost2 = "n:ghost one", "n:ghost two"
	r.mustRun(func(ctx context.Context, e Env) int { return PayeeMerge(ctx, e, ghost1, ghost2) })
	r.mustRun(func(ctx context.Context, e Env) int { return PayeeMerge(ctx, e, ghost2, alpha) })
	before := r.entries()

	refused := []struct {
		name      string
		key, into string
		why       string
	}{
		// alpha and beta both resolve to the payee gamma already.
		{"same payee", alpha, beta, "same payee"},
		{"same payee in the other direction", beta, alpha, "same payee"},
		// alpha into ghost one: the target has no transactions, but its merge chain (ghost two,
		// then alpha) returns to the key.
		{"chain returns to the key", alpha, ghost1, "merge chain"},
		{"chain of one step returns to the key", alpha, ghost2, "merge chain"},
	}
	for _, c := range refused {
		code, out, stderr := r.run(func(ctx context.Context, e Env) int { return PayeeMerge(ctx, e, c.key, c.into) })
		if code == ExitOK || out != "" || !strings.Contains(stderr, "--into") || !strings.Contains(stderr, c.why) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", c.name, code, out, stderr)
		}
		for _, k := range []string{alpha, beta, gamma, ghost1, ghost2} {
			if strings.Contains(stderr, k) {
				t.Errorf("%s: the refusal echoes a key: %q", c.name, stderr)
			}
		}
		if n := r.entries(); n != before {
			t.Fatalf("%s wrote %d entries", c.name, n-before)
		}
	}
	// A merge that is neither is accepted.
	r.mustRun(func(ctx context.Context, e Env) int { return PayeeMerge(ctx, e, "n:other", gamma) })
}

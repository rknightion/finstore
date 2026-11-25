package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/rknightion/finstore/internal/store"
)

// payeeReadFixture writes made-up transactions for three n: payees (counterparty or description
// names) and a rule or two.
func payeeReadFixture(t *testing.T) *insightFixture {
	t.Helper()
	f := newInsightFixture(t)
	f.tx(itx{acct: "cur", key: "a1", date: "2026-03-01", amt: -100, cp: "Alpha Store"})
	f.tx(itx{acct: "cur", key: "a2", date: "2026-03-02", amt: -200, cp: "ALPHA STORE"})
	f.tx(itx{acct: "cur", key: "a3", date: "2026-03-03", amt: -300, cp: "ALPHA STORE"})
	f.tx(itx{acct: "cur", key: "b1", date: "2026-03-04", amt: -400, desc: "Beta Cafe"})
	f.tx(itx{acct: "cur", key: "b2", date: "2026-03-05", amt: -500, desc: "Beta Cafe"})
	f.tx(itx{acct: "cur", key: "c1", date: "2026-03-06", amt: -600, desc: "Gamma Power"})
	return f
}

func TestPayeeReads(t *testing.T) {
	ctx := context.Background()
	f := payeeReadFixture(t)
	var alpha, beta, gamma string
	for key, dst := range map[string]*string{"a1": &alpha, "b1": &beta, "c1": &gamma} {
		if err := f.q.QueryRow(ctx, `SELECT payee_key FROM agent.cash_flows WHERE id = $1`, f.ids[key]).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}

	// MostFrequentName: counterparty first, the most frequent spelling, and nothing for no key.
	if got, err := store.MostFrequentName(ctx, f.q, alpha); err != nil || got != "ALPHA STORE" {
		t.Errorf("MostFrequentName(alpha) = %q, %v", got, err)
	}
	if got, err := store.MostFrequentName(ctx, f.q, beta); err != nil || got != "Beta Cafe" {
		t.Errorf("MostFrequentName(beta) = %q, %v", got, err)
	}
	if got, err := store.MostFrequentName(ctx, f.q, "n:no such"); err != nil || got != "" {
		t.Errorf("MostFrequentName(unknown) = %q, %v", got, err)
	}

	// PayeeOf: a key is its own canonical key until a merge moves it; an unknown key has none.
	if id, name, ok, err := store.PayeeOf(ctx, f.q, alpha); err != nil || !ok || id != alpha || name == "" {
		t.Errorf("PayeeOf(alpha) = %q, %q, %v, %v", id, name, ok, err)
	}
	if _, _, ok, err := store.PayeeOf(ctx, f.q, "n:no such"); err != nil || ok {
		t.Errorf("PayeeOf(unknown) ok = %v, %v", ok, err)
	}
	f.alias(alpha, false, beta, "", 1)
	f.setting(beta, "Beta Renamed", "", "", "", nil, 2)
	if id, name, ok, err := store.PayeeOf(ctx, f.q, alpha); err != nil || !ok || id != beta || name != "Beta Renamed" {
		t.Errorf("PayeeOf(alpha) after the merge = %q, %q, %v, %v", id, name, ok, err)
	}

	// ListPayees: a case-insensitive substring of the name, the limit, and no match.
	rows, err := store.ListPayees(ctx, f.q, "RENAMED", 10)
	if err != nil || len(rows) != 1 || rows[0].PayeeID != beta || rows[0].TxnCount != 5 || !slices.Contains(rows[0].Keys, alpha) {
		t.Errorf("ListPayees(RENAMED) = %+v, %v", rows, err)
	}
	if rows, err := store.ListPayees(ctx, f.q, "", 1); err != nil || len(rows) != 1 {
		t.Errorf("ListPayees limit 1 = %d rows, %v", len(rows), err)
	}
	if rows, err := store.ListPayees(ctx, f.q, "no such name", 10); err != nil || len(rows) != 0 {
		t.Errorf("ListPayees(no such name) = %+v, %v", rows, err)
	}
	if rows, err := store.ListPayees(ctx, f.q, "%", 10); err != nil || len(rows) != 0 {
		t.Errorf("a percent sign matched as a wildcard: %+v, %v", rows, err)
	}

	// ShowPayee: keys, variants and the rules that name the payee's keys, from any of its keys.
	d, err := store.ShowPayee(ctx, f.q, alpha)
	if err != nil || d.PayeeID != beta || d.Name != "Beta Renamed" || !slices.Equal(d.Keys, sorted(alpha, beta)) {
		t.Fatalf("ShowPayee = %+v, %v", d, err)
	}
	if len(d.Rows) != 1 || !slices.Contains(d.Rows[0].NameVariants, "Beta Cafe") {
		t.Errorf("rows %+v", d.Rows)
	}
	tables := map[string]bool{}
	for _, r := range d.Rules {
		tables[r.RuleTable+" "+r.Key] = r.Matches
	}
	if !tables["payee_alias "+alpha] || !tables["payee_setting "+beta] || len(tables) != 2 {
		t.Errorf("rules %+v", d.Rules)
	}
	if _, err := store.ShowPayee(ctx, f.q, "n:no such"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ShowPayee(unknown) = %v, want ErrNotFound", err)
	}
	// Series are listed only when the recurring payments view exists.
	var exists bool
	if err := f.q.QueryRow(ctx, `SELECT to_regclass('agent.recurring_payments') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if d.SeriesKnown != exists {
		t.Errorf("SeriesKnown = %v with the view present = %v", d.SeriesKnown, exists)
	}

	// EffectivePayeeState counts the keys of the payee and takes the operator's name.
	st, err := store.EffectivePayeeState(ctx, f.q, alpha)
	if err != nil || st.PayeeID != beta || st.Name != "Beta Renamed" || st.Keys != 2 || st.FlowClass != "spending" {
		t.Errorf("EffectivePayeeState = %+v, %v", st, err)
	}
	if st, err := store.EffectivePayeeState(ctx, f.q, "n:no such"); err != nil || st.PayeeID != "n:no such" || st.Keys != 1 {
		t.Errorf("EffectivePayeeState(unknown) = %+v, %v", st, err)
	}

	// MergeChainReturns: a ghost chain that returns to the key, one that does not, a cap.
	f.alias("n:ghost a", false, "n:ghost b", "", 3)
	f.alias("n:ghost b", false, gamma, "", 4)
	for _, c := range []struct {
		key, into string
		want      bool
	}{
		{gamma, "n:ghost a", true},
		{gamma, "n:ghost b", true},
		{beta, "n:ghost a", false},
		{beta, "n:never seen", false},
		{"n:never seen", "n:never seen", true},
	} {
		got, err := store.MergeChainReturns(ctx, f.q, c.key, c.into)
		if err != nil || got != c.want {
			t.Errorf("MergeChainReturns(%s, %s) = %v, %v; want %v", c.key, c.into, got, err, c.want)
		}
	}
}

func sorted(keys ...string) []string {
	slices.Sort(keys)
	return keys
}

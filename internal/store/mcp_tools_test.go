package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/store"
)

// toolFixture is a made-up ledger written through the store's own accessors: a current account, a
// pot of it, and a closed euro account among the sterling ones.
type toolFixture struct {
	r *store.Reader
	c cluster
}

type fixtureTxn struct {
	key, account, date, created string // created: "HH:MM" on the date
	amount                      int64
	currency, status, desc      string
	merchant, counterparty      string
	category, notes             string
	pot                         string // pot id in the record's metadata
	padding                     int    // bytes of filler in the source record
}

func newToolFixture(t *testing.T, txns []fixtureTxn) toolFixture {
	t.Helper()
	c := newCluster(t)
	ctx := context.Background()
	core := mustCore(t, openStore(t, c.app).DB(), store.SchemaCore)

	current := monzoAccount("monzo-current", "acc_t1", "current", nil)
	pot := monzoAccount("pot-one", "pot_t1", "pot", &current)
	euro := testAccount("euro-card", "credit_card", "file")
	euro.Source, euro.Currency, euro.Closed = "import:made-up", "EUR", true
	euro.EndedOn = ptr(time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC))
	accounts := map[string]store.Account{current.Slug: current, pot.Slug: pot, euro.Slug: euro}
	for _, a := range accounts {
		if err := core.UpsertAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	for i, x := range txns {
		acc, ok := accounts[x.account]
		if !ok {
			t.Fatalf("unknown account %q", x.account)
		}
		day, err := time.Parse(time.DateOnly, x.date)
		if err != nil {
			t.Fatal(err)
		}
		at, err := time.Parse("15:04", x.created)
		if err != nil {
			t.Fatal(err)
		}
		created := time.Date(day.Year(), day.Month(), day.Day(), at.Hour(), at.Minute(), 0, 0, time.UTC)
		tx := testTxn(acc, x.key, x.amount, day)
		tx.Source = acc.Source
		tx.Currency, tx.Status, tx.Description, tx.CreatedAt = x.currency, x.status, x.desc, &created
		tx.MerchantName = nonEmpty(x.merchant)
		tx.Counterparty = nonEmpty(x.counterparty)
		tx.Category = nonEmpty(x.category)
		tx.Notes = nonEmpty(x.notes)
		content := map[string]any{"id": x.key, "amount": x.amount, "metadata": map[string]any{}}
		if x.pot != "" {
			content["metadata"] = map[string]any{"pot_id": x.pot}
		}
		if x.padding > 0 {
			content["padding"] = strings.Repeat("p", x.padding)
		}
		body, err := json.Marshal(content)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := core.ApplyTransaction(ctx, tx, body, []byte(x.key), created, int64(100+i)); err != nil {
			t.Fatal(err)
		}
	}
	return toolFixture{r: openReaderAt(t, c, c.mcpURL(t)), c: c}
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// toolTxns is the ledger most of the tool tests read. Every description is made up.
var toolTxns = []fixtureTxn{
	{key: "coffee", account: "monzo-current", date: "2026-03-02", created: "09:00", amount: -1250, currency: "GBP", status: "settled",
		desc: "Coffee 100% organic", merchant: "Bean Co", category: "eating_out"},
	{key: "coffee2", account: "monzo-current", date: "2026-03-03", created: "09:00", amount: -1000, currency: "GBP", status: "settled",
		desc: "Coffee 1000 organic", merchant: "Bean Co", category: "eating_out"},
	{key: "shop1", account: "monzo-current", date: "2026-03-05", created: "09:00", amount: -4000, currency: "GBP", status: "settled",
		desc: "Weekly shop", merchant: "Mart", category: "groceries"},
	{key: "salary", account: "monzo-current", date: "2026-03-10", created: "09:00", amount: 20000, currency: "GBP", status: "settled",
		desc: "Salary", counterparty: "Employer Ltd", category: "income"},
	{key: "pot", account: "monzo-current", date: "2026-03-11", created: "09:00", amount: -10000, currency: "GBP", status: "settled",
		desc: "Moved to pot", category: "savings", pot: "pot_t1"},
	{key: "declined", account: "monzo-current", date: "2026-03-12", created: "09:00", amount: -999, currency: "GBP", status: "declined",
		desc: "Declined shop", merchant: "Mart", category: "groceries"},
	{key: "tea1", account: "monzo-current", date: "2026-03-14", created: "09:00", amount: -300, currency: "GBP", status: "settled",
		desc: "tea_time", counterparty: "Tea Co", category: "eating_out", notes: "keep the receipt"},
	{key: "tea2", account: "monzo-current", date: "2026-03-14", created: "12:00", amount: -310, currency: "GBP", status: "settled",
		desc: "teaxtime", category: "eating_out"},
	{key: "euro", account: "euro-card", date: "2026-03-15", created: "09:00", amount: -700, currency: "EUR", status: "settled",
		desc: "Cafe abroad", merchant: "Cafe", category: "eating_out"},
	{key: "shop2a", account: "monzo-current", date: "2026-03-20", created: "09:00", amount: -3000, currency: "GBP", status: "settled",
		desc: "Shop again", merchant: "Mart", category: "groceries"},
	{key: "shop2b", account: "monzo-current", date: "2026-03-20", created: "18:00", amount: -500, currency: "GBP", status: "settled",
		desc: "Top up", merchant: "Mart", category: "groceries"},
	{key: "plain", account: "monzo-current", date: "2026-04-01", created: "09:00", amount: -500, currency: "GBP", status: "settled",
		desc: "Unlabelled"},
	{key: "oldrefund", account: "monzo-current", date: "2026-02-10", created: "09:00", amount: 100, currency: "GBP", status: "settled",
		desc: "Old refund", merchant: "Mart", category: "groceries"},
	{key: "refund", account: "monzo-current", date: "2026-04-02", created: "09:00", amount: 150, currency: "GBP", status: "settled",
		desc: "Refund", merchant: "Mart", category: "groceries"},
}

// superDB is a superuser connection, for reading the catalog.
func (c cluster) superDB(t *testing.T) store.DBTX { return connect(t, c.super) }

// descs lists the descriptions of rows, in order.
func descs(rows []store.TransactionRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Description
	}
	return out
}

func TestOverviewHasEveryCoverageAndAccountColumnAndEverySource(t *testing.T) {
	f := newToolFixture(t, toolTxns)
	ctx := context.Background()
	ov, err := f.r.Overview(ctx)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	slugs := make([]string, len(ov.Accounts))
	for i, a := range ov.Accounts {
		slugs[i] = a.AccountSlug
	}
	if want := []string{"euro-card", "monzo-current", "pot-one"}; !slices.Equal(slugs, want) {
		t.Fatalf("accounts %v, want %v", slugs, want)
	}
	byslug := map[string]store.OverviewAccount{}
	for _, a := range ov.Accounts {
		byslug[a.AccountSlug] = a
	}
	cur := byslug["monzo-current"]
	if cur.Feed != "api" || cur.Source != "monzo" || cur.Type != "current" || cur.Name != "monzo-current" || cur.Currency != "GBP" ||
		cur.Closed || cur.EndedOn != nil || cur.TransactionCount != 13 || cur.ReconciliationState != store.StateNotChecked ||
		cur.EarliestDate == nil || *cur.EarliestDate != "2026-02-10" || cur.LatestDate == nil || *cur.LatestDate != "2026-04-02" ||
		cur.Gaps == nil {
		t.Errorf("monzo-current: %+v", cur)
	}
	eur := byslug["euro-card"]
	if eur.Feed != "file" || eur.Source != "import:made-up" || eur.Type != "credit_card" || eur.Currency != "EUR" || !eur.Closed ||
		eur.EndedOn == nil || *eur.EndedOn != "2026-03-31" || eur.TransactionCount != 1 {
		t.Errorf("euro-card: %+v", eur)
	}
	if pot := byslug["pot-one"]; pot.Type != "pot" || pot.TransactionCount != 0 || pot.EarliestDate != nil || pot.ReconciliationState != store.StateBalancesOnly {
		t.Errorf("pot-one: %+v", pot)
	}

	// Every column of agent.coverage appears under its own name, and the five from agent.accounts.
	enc, err := json.Marshal(cur)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(enc, &keys); err != nil {
		t.Fatal(err)
	}
	want := append(viewColumns(t, f.c.superDB(t), "coverage"), "type", "name", "currency", "closed", "ended_on")
	got := make([]string, 0, len(keys))
	for k := range keys {
		got = append(got, k)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("overview account keys %v, want %v", got, want)
	}

	// Sources: every column of agent.sync_status, one row per source, ordered by source.
	srcs := make([]string, len(ov.Sources))
	for i, s := range ov.Sources {
		srcs[i] = s.Source
	}
	if want := []string{"import:made-up", "monzo"}; !slices.Equal(srcs, want) {
		t.Fatalf("sources %v, want %v", srcs, want)
	}
	if imp := ov.Sources[0].LastImportAt; len(imp) != 1 || imp["euro-card"] != nil {
		t.Errorf("last_import_at of the file source: %v", imp)
	}
	senc, _ := json.Marshal(ov.Sources[1])
	var skeys map[string]any
	if err := json.Unmarshal(senc, &skeys); err != nil {
		t.Fatal(err)
	}
	swant := viewColumns(t, f.c.superDB(t), "sync_status")
	var sgot []string
	for k := range skeys {
		sgot = append(sgot, k)
	}
	slices.Sort(sgot)
	slices.Sort(swant)
	if !slices.Equal(sgot, swant) {
		t.Errorf("overview source keys %v, want %v", sgot, swant)
	}
}

func TestOverviewReportsAGapWithItsReason(t *testing.T) {
	// An account that expects imports and has none is an "import due" gap, with a null start.
	f := newToolFixture(t, nil)
	ctx := context.Background()
	core := mustCore(t, openStore(t, f.c.app).DB(), store.SchemaCore)
	acc := testAccount("statement-fed", "current", "file")
	acc.Source, acc.ExpectsImports = "import:made-up", true
	if err := core.UpsertAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	ov, err := f.r.Overview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range ov.Accounts {
		if a.AccountSlug != "statement-fed" {
			continue
		}
		if len(a.Gaps) != 1 || a.Gaps[0].Reason != store.GapImportDue || a.Gaps[0].From != nil || a.Gaps[0].To == nil {
			t.Fatalf("gaps of an account with no import: %+v", a.Gaps)
		}
		return
	}
	t.Fatal("statement-fed is not in the overview")
}

func TestSearchTransactions(t *testing.T) {
	f := newToolFixture(t, toolTxns)
	ctx := context.Background()
	search := func(p store.SearchParams) ([]store.TransactionRow, bool) {
		t.Helper()
		rows, truncated, err := f.r.SearchTransactions(ctx, p)
		if err != nil {
			t.Fatalf("SearchTransactions(%+v): %v", p, err)
		}
		return rows, truncated
	}
	check := func(name string, p store.SearchParams, want ...string) {
		t.Helper()
		rows, truncated := search(p)
		if !slices.Equal(descs(rows), want) || truncated {
			t.Errorf("%s: got %v (truncated %v), want %v", name, descs(rows), truncated, want)
		}
	}

	// Newest txn_date first, then newest created_at: the two on 2026-03-20 and the two on
	// 2026-03-14 are ordered by the time they were created, latest first.
	all, truncated := search(store.SearchParams{})
	wantAll := []string{"Refund", "Unlabelled", "Top up", "Shop again", "Cafe abroad", "teaxtime", "tea_time", "Declined shop",
		"Moved to pot", "Salary", "Weekly shop", "Coffee 1000 organic", "Coffee 100% organic", "Old refund"}
	if !slices.Equal(descs(all), wantAll) || truncated {
		t.Fatalf("unfiltered: %v (truncated %v), want %v", descs(all), truncated, wantAll)
	}
	first := all[0]
	if first.ID == "" || first.AccountSlug != "monzo-current" || first.TxnDate != "2026-04-02" || first.AmountMinor != 150 ||
		first.Currency != "GBP" || first.Status != "settled" || first.MerchantName == nil || *first.MerchantName != "Mart" ||
		first.Category == nil || *first.Category != "groceries" || first.Counterparty != nil || first.Notes != nil ||
		first.PotSlug != nil || first.CreatedAt == nil || first.Detail != nil {
		t.Errorf("first row: %+v", first)
	}
	byDesc := map[string]store.TransactionRow{}
	for _, r := range all {
		byDesc[r.Description] = r
	}
	if p := byDesc["Moved to pot"]; p.PotSlug == nil || *p.PotSlug != "pot-one" {
		t.Errorf("pot transfer: %+v", p)
	}
	if e := byDesc["Cafe abroad"]; e.Currency != "EUR" || e.AmountMinor != -700 || e.AccountSlug != "euro-card" {
		t.Errorf("euro transaction: %+v", e)
	}
	if d := byDesc["Declined shop"]; d.Status != "declined" {
		t.Errorf("declined row: %+v", d)
	}

	check("one account", store.SearchParams{Accounts: []string{"euro-card"}}, "Cafe abroad")
	check("two accounts", store.SearchParams{Accounts: []string{"euro-card", "pot-one"}}, "Cafe abroad")
	// from and to are inclusive at both ends.
	check("date range", store.SearchParams{From: "2026-03-05", To: "2026-03-10"}, "Salary", "Weekly shop")
	check("from only", store.SearchParams{From: "2026-04-01"}, "Refund", "Unlabelled")
	check("to only", store.SearchParams{To: "2026-03-03"}, "Coffee 1000 organic", "Coffee 100% organic", "Old refund")
	check("one day", store.SearchParams{From: "2026-03-20", To: "2026-03-20"}, "Top up", "Shop again")
	// text: a substring of description, counterparty, merchant_name or notes, case-insensitive.
	check("description", store.SearchParams{Text: "weekly"}, "Weekly shop")
	check("merchant", store.SearchParams{Text: "bean CO"}, "Coffee 1000 organic", "Coffee 100% organic")
	check("counterparty", store.SearchParams{Text: "employer"}, "Salary")
	check("notes", store.SearchParams{Text: "RECEIPT"}, "tea_time")
	// A % or _ in the search text matches itself, not any run or any character.
	check("percent is literal", store.SearchParams{Text: "100%"}, "Coffee 100% organic")
	check("underscore is literal", store.SearchParams{Text: "tea_time"}, "tea_time")
	check("backslash is literal", store.SearchParams{Text: `a\b`})
	check("only a percent", store.SearchParams{Text: "%"}, "Coffee 100% organic")
	check("category is exact", store.SearchParams{Category: "groceries"}, "Refund", "Top up", "Shop again", "Declined shop", "Weekly shop", "Old refund")
	check("category is not a substring", store.SearchParams{Category: "grocer"})
	check("direction in", store.SearchParams{Direction: "in"}, "Refund", "Salary", "Old refund")
	check("direction out and an account", store.SearchParams{Direction: "out", Accounts: []string{"euro-card"}}, "Cafe abroad")
	// Signed minor units as stored, inclusive.
	check("min and max amount", store.SearchParams{MinAmount: ptr(int64(-1000)), MaxAmount: ptr(int64(-500))},
		"Unlabelled", "Top up", "Cafe abroad", "Declined shop", "Coffee 1000 organic")
	check("min amount only", store.SearchParams{MinAmount: ptr(int64(1000))}, "Salary")
	check("filters combine", store.SearchParams{Category: "eating_out", Direction: "out", From: "2026-03-14", MaxAmount: ptr(int64(-305))},
		"Cafe abroad", "teaxtime")

	// limit: truncated only when more rows matched.
	rows, truncated := search(store.SearchParams{Limit: 3})
	if !slices.Equal(descs(rows), wantAll[:3]) || !truncated {
		t.Errorf("limit 3: %v (truncated %v)", descs(rows), truncated)
	}
	rows, truncated = search(store.SearchParams{Limit: len(wantAll)})
	if len(rows) != len(wantAll) || truncated {
		t.Errorf("limit equal to the row count: %d rows, truncated %v", len(rows), truncated)
	}
	if rows, _ := search(store.SearchParams{Category: "nothing"}); rows == nil || len(rows) != 0 {
		t.Errorf("no match: %v, want an empty non-nil list", rows)
	}

	// include_detail adds the latest version's record, and only then.
	with, _ := search(store.SearchParams{Text: "Weekly", IncludeDetail: true})
	if len(with) != 1 {
		t.Fatalf("detail search: %d rows", len(with))
	}
	detail, ok := with[0].Detail.(map[string]any)
	if !ok || detail["id"] != "shop1" {
		t.Errorf("detail = %#v, want the source record", with[0].Detail)
	}
	if enc, _ := json.Marshal(byDesc["Weekly shop"]); strings.Contains(string(enc), `"detail"`) {
		t.Errorf("a row without detail encodes a detail key: %s", enc)
	}
}

func TestSearchTransactionsRefusesBadInput(t *testing.T) {
	f := newToolFixture(t, nil)
	for name, p := range map[string]store.SearchParams{
		"slug with a space":      {Accounts: []string{"monzo current"}},
		"empty slug":             {Accounts: []string{""}},
		"date format":            {From: "01/03/2026"},
		"date value":             {To: "2026-13-40"},
		"from after to":          {From: "2026-03-02", To: "2026-03-01"},
		"direction":              {Direction: "sideways"},
		"negative limit":         {Limit: -1},
		"limit over the maximum": {Limit: store.CuratedMaxLimit + 1},
	} {
		_, _, err := f.r.SearchTransactions(context.Background(), p)
		if !errors.Is(err, store.ErrInvalid) || store.Outcome(err) != store.OutcomeInvalid {
			t.Errorf("%s: %v (outcome %q), want an invalid-input error", name, err, store.Outcome(err))
		}
	}
	// The same date as from and to is not "from after to".
	if _, _, err := f.r.SearchTransactions(context.Background(), store.SearchParams{From: "2026-03-01", To: "2026-03-01"}); err != nil {
		t.Errorf("from equal to to: %v", err)
	}
}

func TestSearchTransactionsStopsAtTheByteCap(t *testing.T) {
	fat := func(key, day string) fixtureTxn {
		return fixtureTxn{key: key, account: "monzo-current", date: day, created: "09:00", amount: -100, currency: "GBP",
			status: "settled", desc: key, padding: 300 << 10}
	}
	f := newToolFixture(t, []fixtureTxn{fat("a", "2026-03-01"), fat("b", "2026-03-02"), fat("c", "2026-03-03")})
	ctx := context.Background()
	rows, truncated, err := f.r.SearchTransactions(ctx, store.SearchParams{IncludeDetail: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !truncated || rows[0].Description != "c" {
		t.Errorf("three 300 KiB records against a 512 KiB cap: %v (truncated %v), want just the newest and truncated", descs(rows), truncated)
	}
	rows, truncated, err = f.r.SearchTransactions(ctx, store.SearchParams{})
	if err != nil || len(rows) != 3 || truncated {
		t.Errorf("without detail: %d rows, truncated %v, %v", len(rows), truncated, err)
	}
}

func TestSpendingSummary(t *testing.T) {
	f := newToolFixture(t, toolTxns)
	ctx := context.Background()
	summary := func(p store.SummaryParams) ([]store.SummaryGroup, bool) {
		t.Helper()
		groups, truncated, err := f.r.SpendingSummary(ctx, p)
		if err != nil {
			t.Fatalf("SpendingSummary(%+v): %v", p, err)
		}
		return groups, truncated
	}
	check := func(name string, p store.SummaryParams, want ...store.SummaryGroup) {
		t.Helper()
		got, truncated := summary(p)
		if !slices.Equal(got, want) || truncated {
			t.Errorf("%s:\n got  %+v (truncated %v)\n want %+v", name, got, truncated, want)
		}
	}
	g := func(key, cur string, total, n int64) store.SummaryGroup {
		return store.SummaryGroup{Key: key, Currency: cur, TotalMinor: total, Count: n}
	}

	// Money out is a positive total. Per currency: the euro row is its own, never added to sterling.
	// Declined rows and the pot transfer are left out; largest total first.
	check("by category", store.SummaryParams{GroupBy: store.GroupCategory},
		g("groceries", "GBP", 7500, 3), g("eating_out", "GBP", 2860, 4), g("eating_out", "EUR", 700, 1), g(store.NoneKey, "GBP", 500, 1))
	check("pot transfers kept on request", store.SummaryParams{GroupBy: store.GroupCategory, ExcludePotTransfers: ptr(false)},
		g("savings", "GBP", 10000, 1), g("groceries", "GBP", 7500, 3), g("eating_out", "GBP", 2860, 4),
		g("eating_out", "EUR", 700, 1), g(store.NoneKey, "GBP", 500, 1))
	check("pot transfers excluded explicitly", store.SummaryParams{GroupBy: store.GroupCategory, ExcludePotTransfers: ptr(true)},
		g("groceries", "GBP", 7500, 3), g("eating_out", "GBP", 2860, 4), g("eating_out", "EUR", 700, 1), g(store.NoneKey, "GBP", 500, 1))
	check("money in", store.SummaryParams{GroupBy: store.GroupCategory, Direction: store.DirectionIn},
		g("income", "GBP", 20000, 1), g("groceries", "GBP", 250, 2))
	check("explicit out is the default", store.SummaryParams{GroupBy: store.GroupCategory, Direction: store.DirectionOut},
		g("groceries", "GBP", 7500, 3), g("eating_out", "GBP", 2860, 4), g("eating_out", "EUR", 700, 1), g(store.NoneKey, "GBP", 500, 1))
	// Months come in key order (largest total first within a month), not by total.
	check("by month", store.SummaryParams{GroupBy: store.GroupMonth},
		g("2026-03", "GBP", 10360, 7), g("2026-03", "EUR", 700, 1), g("2026-04", "GBP", 500, 1))
	check("by month, money in", store.SummaryParams{GroupBy: store.GroupMonth, Direction: store.DirectionIn},
		g("2026-02", "GBP", 100, 1), g("2026-03", "GBP", 20000, 1), g("2026-04", "GBP", 150, 1))
	check("by merchant", store.SummaryParams{GroupBy: store.GroupMerchant},
		g("Mart", "GBP", 7500, 3), g("Bean Co", "GBP", 2250, 2), g(store.NoneKey, "GBP", 1110, 3), g("Cafe", "EUR", 700, 1))
	check("by counterparty", store.SummaryParams{GroupBy: store.GroupCounterparty},
		g(store.NoneKey, "GBP", 10560, 7), g(store.NoneKey, "EUR", 700, 1), g("Tea Co", "GBP", 300, 1))
	check("by account", store.SummaryParams{GroupBy: store.GroupAccount},
		g("monzo-current", "GBP", 10860, 8), g("euro-card", "EUR", 700, 1))
	check("an account filter", store.SummaryParams{GroupBy: store.GroupCategory, Accounts: []string{"euro-card"}},
		g("eating_out", "EUR", 700, 1))
	check("a date range, inclusive", store.SummaryParams{GroupBy: store.GroupCategory, From: "2026-03-14", To: "2026-03-20"},
		g("groceries", "GBP", 3500, 2), g("eating_out", "EUR", 700, 1), g("eating_out", "GBP", 610, 2))
	check("nothing matches", store.SummaryParams{GroupBy: store.GroupCategory, From: "2030-01-01"})

	groups, truncated := summary(store.SummaryParams{GroupBy: store.GroupCategory, Limit: 1})
	if !slices.Equal(groups, []store.SummaryGroup{g("groceries", "GBP", 7500, 3)}) || !truncated {
		t.Errorf("limit 1: %+v (truncated %v)", groups, truncated)
	}
	groups, truncated = summary(store.SummaryParams{GroupBy: store.GroupCategory, Limit: 4})
	if len(groups) != 4 || truncated {
		t.Errorf("limit equal to the group count: %d groups, truncated %v", len(groups), truncated)
	}
	if groups, _ := summary(store.SummaryParams{GroupBy: store.GroupCategory, From: "2030-01-01"}); groups == nil {
		t.Error("an empty summary is nil, not an empty list")
	}
}

func TestSpendingSummaryRefusesBadInput(t *testing.T) {
	f := newToolFixture(t, nil)
	for name, p := range map[string]store.SummaryParams{
		"no group_by":      {},
		"unknown group_by": {GroupBy: "colour"},
		"direction":        {GroupBy: store.GroupMonth, Direction: "both"},
		"from after to":    {GroupBy: store.GroupMonth, From: "2026-03-02", To: "2026-03-01"},
		"bad date":         {GroupBy: store.GroupMonth, From: "yesterday"},
		"bad slug":         {GroupBy: store.GroupMonth, Accounts: []string{"NOT A SLUG"}},
		"limit":            {GroupBy: store.GroupMonth, Limit: store.CuratedMaxLimit + 1},
	} {
		_, _, err := f.r.SpendingSummary(context.Background(), p)
		if !errors.Is(err, store.ErrInvalid) || store.Outcome(err) != store.OutcomeInvalid {
			t.Errorf("%s: %v (outcome %q), want an invalid-input error", name, err, store.Outcome(err))
		}
	}
}

func TestDescribeSchema(t *testing.T) {
	f := newToolFixture(t, toolTxns)
	ctx := context.Background()
	// A materialised view, which no migration creates, so the kind is exercised.
	owner := connect(t, f.c.owner)
	if _, err := owner.Exec(ctx, `CREATE MATERIALIZED VIEW sync.made_up_summary AS SELECT 1 AS n;
		COMMENT ON MATERIALIZED VIEW sync.made_up_summary IS 'a made-up note';
		GRANT SELECT ON sync.made_up_summary TO finstore_mcp`); err != nil {
		t.Fatal(err)
	}

	all, err := f.r.DescribeSchema(ctx, "")
	if err != nil {
		t.Fatalf("DescribeSchema: %v", err)
	}
	var names, wantNames []string
	for _, r := range all {
		names = append(names, r.Schema+"."+r.Name)
		if r.Schema == "secret" || r.Schema == "public" || r.Schema == "core_next" {
			t.Errorf("relation %s.%s is listed", r.Schema, r.Name)
		}
		if len(r.Columns) == 0 {
			t.Errorf("%s.%s has no columns", r.Schema, r.Name)
		}
	}
	for _, r := range relations(t, f.c.super, "agent", "core", "raw", "sync") {
		wantNames = append(wantNames, r.String())
	}
	if !slices.Equal(names, wantNames) {
		t.Errorf("relations %v, want the catalog's %v", names, wantNames)
	}
	find := func(name string) store.Relation {
		t.Helper()
		for _, r := range all {
			if r.Schema+"."+r.Name == name {
				return r
			}
		}
		t.Fatalf("%s is not described", name)
		return store.Relation{}
	}
	if k := find("agent.transactions").Kind; k != store.KindView {
		t.Errorf("agent.transactions kind %q", k)
	}
	if k := find("core.account").Kind; k != store.KindTable {
		t.Errorf("core.account kind %q", k)
	}
	if k := find("raw.api_response").Kind; k != store.KindTable {
		t.Errorf("raw.api_response kind %q", k)
	}
	if mv := find("sync.made_up_summary"); mv.Kind != store.KindMaterializedView || mv.Comment != "a made-up note" ||
		!reflect.DeepEqual(mv.Columns, []store.RelationColumn{{Name: "n", Type: "integer", Nullable: true}}) {
		t.Errorf("materialised view: %+v", mv)
	}
	// Comments: every agent view has its rules; a table without one has an empty comment.
	for _, r := range all {
		if r.Schema == "agent" && len(r.Comment) < 40 {
			t.Errorf("agent.%s comment %q", r.Name, r.Comment)
		}
	}
	if c := find("agent.transactions").Comment; !strings.Contains(c, "pence") || !strings.Contains(c, "pot_slug") {
		t.Errorf("agent.transactions comment: %q", c)
	}
	if c := find("core.account").Comment; c != "" {
		t.Errorf("core.account comment %q, want none", c)
	}
	// Columns come in order with their data types and nullability.
	if got := find("agent.balances").Columns; !reflect.DeepEqual(got, []store.RelationColumn{
		{Name: "account_slug", Type: "text", Nullable: true}, {Name: "at", Type: "timestamp with time zone", Nullable: true},
		{Name: "on_date", Type: "date", Nullable: true}, {Name: "balance_minor", Type: "bigint", Nullable: true},
		{Name: "total_balance_minor", Type: "bigint", Nullable: true}, {Name: "currency", Type: "text", Nullable: true}}) {
		t.Errorf("agent.balances columns %+v", got)
	}
	acc := find("core.account").Columns
	if len(acc) < 10 || acc[0] != (store.RelationColumn{Name: "id", Type: "uuid", Nullable: false}) ||
		!slices.Contains(acc, store.RelationColumn{Name: "detail", Type: "jsonb", Nullable: true}) {
		t.Errorf("core.account columns %+v", acc)
	}
	if got := find("raw.api_response").Columns; got[len(got)-2] != (store.RelationColumn{Name: "body", Type: "bytea", Nullable: false}) {
		t.Errorf("raw.api_response columns %+v", got)
	}

	// One schema only.
	agent, err := f.r.DescribeSchema(ctx, "agent")
	if err != nil || len(agent) != len(agentViews) {
		t.Fatalf("agent: %d relations (%v), want %d", len(agent), err, len(agentViews))
	}
	for _, r := range agent {
		if r.Schema != "agent" || r.Kind != store.KindView {
			t.Errorf("agent listing has %+v", r)
		}
	}
	for _, bad := range []string{"secret", "public", "pg_catalog", "core_next", "AGENT", "agent; DROP TABLE x"} {
		if _, err := f.r.DescribeSchema(ctx, bad); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("DescribeSchema(%q): %v, want an invalid-input error", bad, err)
		}
	}
}

// Every instant a curated tool returns is in UTC, as Query encodes them, whatever zone the process
// runs in (pgx decodes into time.Local, whose Location is never time.UTC, so this holds in a UTC
// process too).
func TestCuratedTimesAreUTC(t *testing.T) {
	f := newToolFixture(t, toolTxns)
	ctx := context.Background()
	q := openStore(t, f.c.app).DB()
	now := time.Now().UTC().Truncate(time.Microsecond)
	id, err := store.CreateConsent(ctx, q, "monzo", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentConsent(ctx, q, id); err != nil {
		t.Fatal(err)
	}
	expiry := now.Add(10 * 24 * time.Hour)
	if _, err := store.TransitionConsent(ctx, q, id, store.ConsentTransition{To: store.ConsentActive, At: now, ExpectedExpiryAt: &expiry}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetConsentLastSyncOK(ctx, q, id, now); err != nil {
		t.Fatal(err)
	}
	core := mustCore(t, q, store.SchemaCore)
	file := testAccount("joint", "current", "file")
	file.Source = "file"
	if err := core.UpsertAccount(ctx, file); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.InsertImportFile(ctx, q, store.ImportFile{AccountSlug: "joint", Format: "manual-csv",
		FileName: "x.csv", ImportedAt: now, Body: []byte("date,type,amount,balance_after,description\n")}); err != nil {
		t.Fatal(err)
	}

	ov, err := f.r.Overview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var checked int
	for _, s := range ov.Sources {
		for name, ts := range map[string]*time.Time{"approved_at": s.ApprovedAt, "expected_expiry_at": s.ExpectedExpiryAt, "last_sync_ok_at": s.LastSyncOKAt} {
			if ts == nil {
				continue
			}
			checked++
			if ts.Location() != time.UTC {
				t.Errorf("%s of %s is in %v, want UTC", name, s.Source, ts.Location())
			}
		}
		for slug, ts := range s.LastImportAt {
			if ts != nil {
				checked++
				if ts.Location() != time.UTC {
					t.Errorf("last_import_at of %s is in %v, want UTC", slug, ts.Location())
				}
			}
		}
	}
	if checked < 4 {
		t.Fatalf("only %d source times were present to check: %+v", checked, ov.Sources)
	}
	rows, _, err := f.r.SearchTransactions(ctx, store.SearchParams{Limit: 3})
	if err != nil || len(rows) != 3 {
		t.Fatalf("search: %d rows, %v", len(rows), err)
	}
	for _, r := range rows {
		if r.CreatedAt == nil || r.CreatedAt.Location() != time.UTC {
			t.Errorf("created_at of %q is %v, want UTC", r.Description, r.CreatedAt)
		}
	}
}

// Slugs are not all lower-case hyphenated words: a Monzo account type that is not in the type map
// passes through unchanged into the slug (underscore), and an operator picks a statement account's.
func TestSearchAndSummaryAcceptRealSlugs(t *testing.T) {
	f := newToolFixture(t, nil)
	ctx := context.Background()
	core := mustCore(t, openStore(t, f.c.app).DB(), store.SchemaCore)
	slugs := []string{"monzo-uk_retail_joint", "Halifax.Joint_2"}
	for i, slug := range slugs {
		acc := testAccount(slug, "current", "api")
		if err := core.UpsertAccount(ctx, acc); err != nil {
			t.Fatal(err)
		}
		day := time.Date(2026, 3, 1+i, 0, 0, 0, 0, time.UTC)
		tx := testTxn(acc, "k", -100*int64(i+1), day)
		tx.Description = "made up " + slug
		if _, err := core.ApplyTransaction(ctx, tx, []byte(`{"id":"k"}`), []byte("h"), day, int64(10+i)); err != nil {
			t.Fatal(err)
		}
	}
	for _, slug := range slugs {
		rows, _, err := f.r.SearchTransactions(ctx, store.SearchParams{Accounts: []string{slug}})
		if err != nil || len(rows) != 1 || rows[0].AccountSlug != slug {
			t.Errorf("search %q: %+v %v", slug, rows, err)
		}
	}
	groups, _, err := f.r.SpendingSummary(ctx, store.SummaryParams{GroupBy: store.GroupAccount, Accounts: slugs})
	if err != nil || len(groups) != 2 {
		t.Fatalf("summary: %+v %v", groups, err)
	}
	// What still is not a slug.
	for _, bad := range []string{"", "a b", "a;b", "a'b", "a/b", strings.Repeat("a", 101)} {
		if _, _, err := f.r.SearchTransactions(ctx, store.SearchParams{Accounts: []string{bad}}); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("slug %q: %v, want an invalid-input error", bad, err)
		}
	}
}

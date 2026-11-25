package store_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// The insight tools' reads against the real views. Every value is made up.

// reader opens the MCP reader on the fixture's cluster.
func (f *insightFixture) reader() *store.Reader {
	f.t.Helper()
	ctx := context.Background()
	if err := store.EnableMCPLogin(ctx, f.super, "mcp-secret"); err != nil {
		f.t.Fatalf("EnableMCPLogin: %v", err)
	}
	r, err := store.OpenReader(ctx, testpg.UserURL(f.t, f.super, store.RoleMCP))
	if err != nil {
		f.t.Fatalf("OpenReader: %v", err)
	}
	f.t.Cleanup(r.Close)
	return r
}

func invalid(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, store.ErrInvalid) || store.Outcome(err) != store.OutcomeInvalid {
		t.Errorf("%s: %v (outcome %q), want an invalid-input error", what, err, store.Outcome(err))
	}
}

// --- spending_summary ------------------------------------------------------------------------

func TestSpendingSummaryReadsFlowClasses(t *testing.T) {
	f := newInsightFixture(t)
	today := londonToday(t, f.q)
	at := func(off int) string { return day(today, off).Format("2006-01-02") }
	f.setting("n:debt co", "", "debt_repayment", "", "", nil, 100)
	f.setting("n:broker co", "", "investment", "", "", nil, 101)
	f.setting("n:cafe co", "", "", "dining", "", nil, 102)
	f.tx(itx{acct: "cur", key: "coffee", date: at(-20), amt: -450, scheme: "mastercard", desc: "COFFEE CO", merchant: "Coffee Co", category: "eating_out", detail: merchantDetail("g_coffee", "Coffee Co")})
	f.tx(itx{acct: "cur", key: "shop1", date: at(-19), amt: -1000, desc: "SHOP CO", category: "groceries"})
	f.tx(itx{acct: "cur", key: "shop2", date: at(-18), amt: -500, desc: "SHOP CO", category: "groceries"})
	f.tx(itx{acct: "cur", key: "cafe", date: at(-18), amt: -250, desc: "CAFE CO", category: "eating_out"})
	f.tx(itx{acct: "cur", key: "pay", date: at(-17), amt: 100000, scheme: "bacs", desc: "EMPLOYER CO"})
	f.tx(itx{acct: "cur", key: "refund", date: at(-16), amt: 300, scheme: "mastercard", desc: "SHOP CO", category: "groceries"})
	f.tx(itx{acct: "cur", key: "pot", date: at(-15), amt: -2000, detail: md("pot_id", "pot_cur")})
	f.tx(itx{acct: "cur", key: "debt", date: at(-14), amt: -900, desc: "DEBT CO"})
	f.tx(itx{acct: "cur", key: "broker", date: at(-13), amt: -1500, desc: "BROKER CO"})
	f.tx(itx{acct: "cur", key: "own_out", date: at(-12), amt: -600, cp: "Savings", detail: cpDetail("account_id", "acc_sav")})
	f.tx(itx{acct: "sav", key: "own_in", date: at(-12), amt: 600, cp: "Current", detail: cpDetail("account_id", "acc_cur")})
	f.tx(itx{acct: "cur", key: "abroad", date: at(-11), amt: -1500, currency: "EUR", desc: "ABROAD"})
	f.tx(itx{acct: "cur", key: "pending", date: at(-1), amt: -700, status: "pending", desc: "PENDING SHOP"})
	f.tx(itx{acct: "cur", key: "declined", date: at(-1), amt: -888, status: "declined", desc: "DECLINED SHOP"})
	r := f.reader()
	ctx := context.Background()

	g := func(key, cur string, total, n int64) store.SummaryGroup {
		return store.SummaryGroup{Key: key, Currency: cur, TotalMinor: total, Count: n}
	}
	check := func(name string, p store.SummaryParams, want ...store.SummaryGroup) {
		t.Helper()
		got, truncated, err := r.SpendingSummary(ctx, p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !slices.Equal(got, want) || truncated {
			t.Errorf("%s:\n got  %+v (truncated %v)\n want %+v", name, got, truncated, want)
		}
	}
	p := func(mod func(*store.SummaryParams)) store.SummaryParams {
		s := store.SummaryParams{GroupBy: store.GroupPayee}
		mod(&s)
		return s
	}

	// Money out defaults to class spending: the debt repayment, the investment, the pot move and the
	// transfer between held accounts are not spending, and pending rows are not counted.
	check("payee, out", p(func(*store.SummaryParams) {}),
		g("ABROAD", "EUR", 1500, 1), g("SHOP CO", "GBP", 1500, 2), g("Coffee Co", "GBP", 450, 1), g("CAFE CO", "GBP", 250, 1))
	check("pending rows on request", p(func(s *store.SummaryParams) { s.IncludePending = true }),
		g("ABROAD", "EUR", 1500, 1), g("SHOP CO", "GBP", 1500, 2), g("PENDING SHOP", "GBP", 700, 1),
		g("Coffee Co", "GBP", 450, 1), g("CAFE CO", "GBP", 250, 1))
	// Money in defaults to income and refund.
	check("payee, in", p(func(s *store.SummaryParams) { s.Direction = store.DirectionIn }),
		g("EMPLOYER CO", "GBP", 100000, 1), g("SHOP CO", "GBP", 300, 1))
	// An explicit list replaces the default.
	check("flow_classes debt_repayment", p(func(s *store.SummaryParams) { s.FlowClasses = []string{"debt_repayment"} }),
		g("DEBT CO", "GBP", 900, 1))
	check("flow_classes investment and debt", p(func(s *store.SummaryParams) {
		s.FlowClasses = []string{"investment", "debt_repayment"}
		s.GroupBy = store.GroupAccount
	}), g("cur", "GBP", 2400, 2))
	check("flow_classes internal, by account", p(func(s *store.SummaryParams) {
		s.FlowClasses = []string{"internal"}
		s.GroupBy = store.GroupAccount
	}), g("cur", "GBP", 2600, 2))
	check("flow_classes income, out", p(func(s *store.SummaryParams) { s.FlowClasses = []string{"income"} }))

	// Pot transfers: kept out by default, in with exclude_pot_transfers false when flow_classes is
	// not passed, and ignored when it is.
	byAccount := func(mod func(*store.SummaryParams)) store.SummaryParams {
		return p(func(s *store.SummaryParams) { s.GroupBy = store.GroupAccount; s.Accounts = []string{"cur"}; mod(s) })
	}
	check("pots excluded by default", byAccount(func(*store.SummaryParams) {}),
		g("cur", "GBP", 2200, 4), g("cur", "EUR", 1500, 1))
	check("pots excluded explicitly", byAccount(func(s *store.SummaryParams) { s.ExcludePotTransfers = ptr(true) }),
		g("cur", "GBP", 2200, 4), g("cur", "EUR", 1500, 1))
	check("pots added", byAccount(func(s *store.SummaryParams) { s.ExcludePotTransfers = ptr(false) }),
		g("cur", "GBP", 4200, 5), g("cur", "EUR", 1500, 1))
	check("false is ignored with flow_classes", byAccount(func(s *store.SummaryParams) {
		s.ExcludePotTransfers = ptr(false)
		s.FlowClasses = []string{"spending"}
	}), g("cur", "GBP", 2200, 4), g("cur", "EUR", 1500, 1))
	check("false is ignored with flow_classes, pots not asked for", byAccount(func(s *store.SummaryParams) {
		s.ExcludePotTransfers = ptr(false)
		s.FlowClasses = []string{"debt_repayment"}
	}), g("cur", "GBP", 900, 1))

	// The effective category: the payee's setting first, then the transaction's own.
	check("category", p(func(s *store.SummaryParams) { s.GroupBy = store.GroupCategory }),
		g(store.NoneKey, "EUR", 1500, 1), g("groceries", "GBP", 1500, 2), g("eating_out", "GBP", 450, 1), g("dining", "GBP", 250, 1))
	// merchant and counterparty keep reading the row's own fields.
	check("merchant", p(func(s *store.SummaryParams) { s.GroupBy = store.GroupMerchant }),
		g(store.NoneKey, "GBP", 1750, 3), g(store.NoneKey, "EUR", 1500, 1), g("Coffee Co", "GBP", 450, 1))
	check("month, a date range", p(func(s *store.SummaryParams) {
		s.GroupBy = store.GroupMonth
		s.From, s.To = at(-19), at(-18)
		s.Accounts = []string{"cur"}
	}), g(day(today, -19).Format("2006-01"), "GBP", 1750, 3))

	// Bad input.
	for name, s := range map[string]store.SummaryParams{
		"unknown flow class": {GroupBy: store.GroupPayee, FlowClasses: []string{"spending", "bogus"}},
		"unknown group":      {GroupBy: "colour"},
	} {
		_, _, err := r.SpendingSummary(ctx, s)
		invalid(t, name, err)
	}
}

// --- recurring_payments, payees, cash_flow, finance_profile ----------------------------------

// estate is a made-up ledger in three scopes: recurring series on a personal account, a business
// account with a known run of months, a joint account, and the rows the profile reports as still
// to correct. It tallies what it writes, so tests state their expectations from the rows.
type estate struct {
	*recFix
	r *store.Reader
	// spend tallies settled spending written through spend, for the profile's expectations.
	spendN, spendAmt     map[string]int64 // by currency
	spendBy              map[string]int64 // payee name (lower case) to spending in the last 365 days, GBP and EUR together
	first, last          time.Time
	ignoredKey           string
	monthNow, monthPrev1 string
}

func (e *estate) track(x itx) {
	if x.status == "declined" {
		return
	}
	d := d(x.date)
	if e.first.IsZero() || d.Before(e.first) {
		e.first = d
	}
	if d.After(e.last) {
		e.last = d
	}
}

func (e *estate) tx(x itx) { e.t.Helper(); e.track(x); e.recFix.tx(x) }

// spend writes a plain settled spending row and tallies it.
func (e *estate) spend(x itx) {
	e.t.Helper()
	e.tx(x)
	if x.status != "" && x.status != "settled" {
		return
	}
	cur := x.currency
	if cur == "" {
		cur = "GBP"
	}
	e.spendN[cur]++
	e.spendAmt[cur] -= x.amt
	name := strings.ToLower(x.desc)
	if x.merchant != "" {
		name = strings.ToLower(x.merchant)
	}
	if !d(x.date).Before(day(e.today, -365)) {
		e.spendBy[name+"/"+cur] -= x.amt
	}
}

// series writes a run through put and tallies its rows.
func (e *estate) series(s rs) {
	e.t.Helper()
	e.put(s)
	for i, off := range s.offs {
		e.track(itx{date: e.at(off)})
		if s.in || (s.status != "" && s.status != "settled") {
			continue
		}
		e.spendN["GBP"]++
		e.spendAmt["GBP"] += s.amts[i]
		if off >= -365 {
			e.spendBy[strings.ToLower(s.name)+"/GBP"] += s.amts[i]
		}
	}
}

func (e *estate) monthDate(k, dom int) string {
	return time.Date(e.today.Year(), e.today.Month()-time.Month(k), dom, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
}

func (e *estate) month(k int) string { return e.monthDate(k, 1)[:7] }

func newEstate(t *testing.T) *estate {
	t.Helper()
	e := &estate{recFix: newRecFix(t), spendN: map[string]int64{}, spendAmt: map[string]int64{}, spendBy: map[string]int64{}}
	e.monthNow, e.monthPrev1 = e.month(0), e.month(1)

	// Recurring series on the personal account. The last offsets differ, so the next dates do.
	e.series(rs{name: "Gym Co", amts: flat(3000, 5), offs: steps(5, 30, -10)})
	e.series(rs{name: "Video Co", amts: flat(1000, 4), offs: steps(4, 30, -9), detail: md("subscription_id", "sub_v")})
	e.series(rs{name: "Water Co", amts: flat(4000, 3), offs: steps(3, 91, -30)})
	e.series(rs{name: "Rise Co", amts: repeat(flat(2000, 8), flat(2500, 6)), offs: steps(14, 30, -11)})
	e.series(rs{name: "Drop Co", amts: repeat(flat(5000, 8), flat(4000, 6)), offs: steps(14, 30, -12)})
	e.series(rs{name: "Price Co", amts: repeat(flat(1000, 8), flat(1200, 6)), offs: steps(14, 30, -13)})
	e.series(rs{name: "Due Co", amts: flat(2000, 4), offs: steps(4, 30, -33)})
	e.series(rs{name: "Overdue Co", amts: flat(2600, 4), offs: steps(4, 30, -38)})
	e.series(rs{name: "Lapsed Co", amts: flat(1500, 4), offs: steps(4, 30, -200)})
	e.series(rs{name: "Ignored Co", amts: flat(500, 4), offs: steps(4, 30, -10)})
	e.ignoredKey = e.seriesKey("Ignored Co")
	e.setSeries(e.ignoredKey, "ignore", 500)
	for i, off := range steps(4, 30, -14) {
		e.spend(itx{acct: "cur", key: fmt.Sprintf("Euro Co/%d", i), date: e.at(off), amt: -700, currency: "EUR", desc: "Euro Co"})
	}

	// The business account: three complete months with known figures.
	e.setting("n:biz employer", "", "income", "", "", ptr(true), 101)
	e.tx(itx{acct: "biz", key: "inc1", date: e.monthDate(1, 25), amt: 300000, desc: "Biz Employer"})
	e.tx(itx{acct: "biz", key: "inc2", date: e.monthDate(2, 10), amt: 200000, desc: "Biz Other"})
	e.tx(itx{acct: "biz", key: "inc3", date: e.monthDate(3, 25), amt: 100000, desc: "Biz Employer"})
	e.spend(itx{acct: "biz", key: "m1g1", date: e.monthDate(1, 3), amt: -20000, desc: "Grocer Biz", category: "groceries"})
	e.spend(itx{acct: "biz", key: "m1g2", date: e.monthDate(1, 6), amt: -20000, desc: "Grocer Biz", category: "groceries"})
	e.spend(itx{acct: "biz", key: "m1c", date: e.monthDate(1, 9), amt: -20000, desc: "Cafe Biz", category: "eating_out"})
	e.spend(itx{acct: "biz", key: "m1r", date: e.monthDate(1, 12), amt: -20000, desc: "Rail Biz", category: "travel"})
	e.spend(itx{acct: "biz", key: "m1m", date: e.monthDate(1, 15), amt: -20000, desc: "Misc Biz"})
	for i, dom := range []int{3, 6, 9, 12, 15} {
		e.spend(itx{acct: "biz", key: fmt.Sprintf("m2g%d", i), date: e.monthDate(2, dom), amt: -10000, desc: "Grocer Biz", category: "groceries"})
		e.spend(itx{acct: "biz", key: fmt.Sprintf("m3g%d", i), date: e.monthDate(3, dom), amt: -20000, desc: "Grocer Biz", category: "groceries"})
	}
	// Outside the default range, and the current month (one settled, one pending).
	e.spend(itx{acct: "biz", key: "old", date: e.monthDate(13, 15), amt: -999, desc: "Old Biz"})
	e.spend(itx{acct: "biz", key: "now_settled", date: e.at(0), amt: -300, desc: "Cur Biz"})
	e.tx(itx{acct: "biz", key: "now_pending", date: e.at(0), amt: -700, status: "pending", desc: "Cur Biz Pending"})
	// The joint account.
	e.spend(itx{acct: "joint", key: "jshop", date: e.monthDate(1, 8), amt: -7000, desc: "Joint Shop", category: "groceries"})

	// What the operator could still correct.
	e.spend(itx{acct: "cur", key: "big", date: e.at(-20), amt: -30000, desc: "Big Co"})
	e.spend(itx{acct: "cur", key: "dup1", date: e.at(-5), amt: -1234, desc: "Dup Co"})
	e.spend(itx{acct: "cur", key: "dup2", date: e.at(-4), amt: -1234, desc: "Dup Co"})
	e.spend(itx{acct: "cur", key: "latest", date: e.at(-1), amt: -100, desc: "Latest Co"})
	e.spend(itx{acct: "cur", key: "custom", date: e.at(-15), amt: -500, desc: "Custom Co", category: "category_zz9"})
	e.spend(itx{acct: "cur", key: "named", date: e.at(-16), amt: -400, desc: "Named Co", category: "category_named1"})
	e.exec(`INSERT INTO core.category_name (category, name, entry_seq, entered_at) VALUES ('category_named1', 'A name', 600, now())`)
	e.spend(itx{acct: "cur", key: "abroad", date: e.at(-30), amt: -1500, currency: "EUR", desc: "ABROAD"})
	e.setting("n:rule co", "", "spending", "", "", nil, 102)
	e.tx(itx{acct: "cur", key: "rule", date: e.at(-25), amt: -100, desc: "Rule Co"})
	e.setting("n:nobody co", "", "spending", "", "", nil, 103)
	e.tx(itx{acct: "cur", key: "pending", date: e.at(0), amt: -777, status: "pending", desc: "Pending Shop"})
	e.tx(itx{acct: "cur", key: "declined", date: e.at(0), amt: -888, status: "declined", desc: "Declined Shop"})
	// An Open Banking transfer that pairs with its Monzo side, and one that does not; a card
	// repayment that pairs and a card credit that does not.
	ob := cpDetail("sort_code", "30-99-99", "account_number", "33333333")
	e.tx(itx{acct: "cur", key: "pair_cur", date: e.at(-60), amt: -5123, cp: "My Other Bank", detail: ob})
	e.tx(itx{acct: "ob", key: "pair_ob", date: e.at(-60), amt: 5123, desc: "FASTER PAYMENT FROM SELF", category: "TRANSFER"})
	e.spend(itx{acct: "ob", key: "lone_ob", date: e.at(-61), amt: -4321, desc: "FASTER PAYMENT TO SOMEONE", category: "TRANSFER"})
	e.tx(itx{acct: "card", key: "cc_in", date: e.at(-50), amt: 23456, desc: "PAYMENT RECEIVED THANK YOU"})
	e.tx(itx{acct: "cur", key: "cc_out", date: e.at(-51), amt: -23456, desc: "MADE UP CARD CO"})
	e.tx(itx{acct: "card", key: "cashback", date: e.at(-40), amt: 200, desc: "CASHBACK"})

	e.r = e.reader()
	return e
}

func (e *estate) recurring(t *testing.T, p store.RecurringParams) (store.Recurring, bool) {
	t.Helper()
	got, truncated, err := e.r.RecurringPayments(context.Background(), p)
	if err != nil {
		t.Fatalf("RecurringPayments(%+v): %v", p, err)
	}
	return got, truncated
}

func names(rows []store.RecurringSeries) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.PayeeName
	}
	return out
}

func TestRecurringPaymentsTool(t *testing.T) {
	e := newEstate(t)
	want := func(name string, p store.RecurringParams, wantNames ...string) store.Recurring {
		t.Helper()
		got, truncated := e.recurring(t, p)
		if !slices.Equal(names(got.Series), wantNames) || truncated {
			t.Errorf("%s: %v (truncated %v), want %v", name, names(got.Series), truncated, wantNames)
		}
		return got
	}

	// The default: active, due and overdue series, ignored ones left out, largest annual cost first.
	all := want("default", store.RecurringParams{},
		"Drop Co", "Gym Co", "Overdue Co", "Rise Co", "Due Co", "Water Co", "Price Co", "Video Co", "Euro Co")
	g := all.Series[1]
	if g.Cadence != "monthly" || g.Evidence != "inferred" || g.Status != "active" || g.Occurrences != 5 || g.TypicalAmountMinor != 3000 ||
		g.AnnualCostMinor == nil || *g.AnnualCostMinor != 36000 || g.NextExpectedDate == nil || *g.NextExpectedDate != e.at(20) ||
		g.FirstDate != e.at(-130) || g.LastDate != e.at(-10) || g.Currency != "GBP" || g.PriceChanges == nil || len(g.AccountSlugs) != 1 ||
		g.AccountSlugs[0] != "cur" || g.Confidence != "low" || g.OperatorState != nil || g.FlowClass != "spending" ||
		!strings.HasPrefix(g.SeriesKey, "inf:") || g.AmountKind != "fixed" || g.PayeeID != "n:gym co" || g.LastAmountMinor != 3000 {
		t.Errorf("Gym Co = %+v", g)
	}
	// The annual cost covers every matching series, per currency.
	wantTotals := []store.AnnualCostTotal{{Currency: "EUR", TotalMinor: 8400}, {Currency: "GBP", TotalMinor: 211600}}
	if !slices.Equal(all.AnnualCostTotal, wantTotals) {
		t.Errorf("annual_cost_total = %+v, want %+v", all.AnnualCostTotal, wantTotals)
	}
	if v := all.Series[7]; v.Evidence != "provider_subscription" || !strings.HasPrefix(v.SeriesKey, "sub:") || v.Confidence != "high" {
		t.Errorf("Video Co = %+v", v)
	}

	// Statuses.
	want("status due", store.RecurringParams{Statuses: []string{"due"}}, "Due Co")
	want("status overdue", store.RecurringParams{Statuses: []string{"overdue"}}, "Overdue Co")
	want("status lapsed", store.RecurringParams{Statuses: []string{"lapsed"}}, "Lapsed Co")
	want("status lapsed and due, by annual cost", store.RecurringParams{Statuses: []string{"due", "lapsed"}}, "Due Co", "Lapsed Co")
	// Ignored series.
	want("include_ignored", store.RecurringParams{Statuses: []string{"active"}, IncludeIgnored: true, Query: "ignored"}, "Ignored Co")
	want("ignored are hidden", store.RecurringParams{Statuses: []string{"active"}, Query: "ignored"})
	ig, _ := e.recurring(t, store.RecurringParams{IncludeIgnored: true, Query: "ignored"})
	if len(ig.Series) != 1 || ig.Series[0].OperatorState == nil || *ig.Series[0].OperatorState != "ignored" || ig.Series[0].SeriesKey != e.ignoredKey {
		t.Errorf("ignored = %+v", ig.Series)
	}
	// A total counts the ignored series only when they are included.
	if ig.AnnualCostTotal[0].TotalMinor != 6000 {
		t.Errorf("ignored total = %+v", ig.AnnualCostTotal)
	}
	// Query: a case-insensitive substring of the payee name.
	want("query", store.RecurringParams{Query: "GYM"}, "Gym Co")
	want("query substring", store.RecurringParams{Query: " co"}, "Drop Co", "Gym Co", "Overdue Co", "Rise Co", "Due Co", "Water Co", "Price Co", "Video Co", "Euro Co")
	want("query matches literally", store.RecurringParams{Query: "%"})
	// changed_only keeps the series with a non-zero 12 month change.
	ch := want("changed_only, by absolute change", store.RecurringParams{ChangedOnly: true, Sort: store.SortChange12m}, "Drop Co", "Rise Co", "Price Co")
	for i, wantChange := range []int64{-1000, 500, 200} {
		if ch.Series[i].Change12mMinor == nil || *ch.Series[i].Change12mMinor != wantChange {
			t.Errorf("%s change_12m = %v, want %d", ch.Series[i].PayeeName, ch.Series[i].Change12mMinor, wantChange)
		}
	}
	if ch.Series[0].PriceChanges == nil {
		t.Errorf("price_changes is null, want a list: %+v", ch.Series[0])
	}
	// Sorts. Series without a change or an annual cost come last.
	byChange, _ := e.recurring(t, store.RecurringParams{Sort: store.SortChange12m})
	if !slices.Equal(names(byChange.Series[:3]), []string{"Drop Co", "Rise Co", "Price Co"}) || len(byChange.Series) != 9 {
		t.Errorf("change_12m order = %v", names(byChange.Series))
	}
	rest := byChange.Series[3:]
	if !sort.SliceIsSorted(rest, func(i, j int) bool { return rest[i].SeriesKey < rest[j].SeriesKey }) {
		t.Errorf("series without a change are not in series_key order: %v", names(rest))
	}
	want("sort next_date", store.RecurringParams{Sort: store.SortNextDate},
		"Overdue Co", "Due Co", "Euro Co", "Price Co", "Drop Co", "Rise Co", "Gym Co", "Video Co", "Water Co")
	want("sort annual_cost", store.RecurringParams{Sort: store.SortAnnualCost, Statuses: []string{"active"}},
		"Drop Co", "Gym Co", "Rise Co", "Water Co", "Price Co", "Video Co", "Euro Co")

	// A limit cuts the rows, not the totals.
	cut, truncated := e.recurring(t, store.RecurringParams{Limit: 2})
	if !slices.Equal(names(cut.Series), []string{"Drop Co", "Gym Co"}) || !truncated || !slices.Equal(cut.AnnualCostTotal, wantTotals) {
		t.Errorf("limit 2: %v truncated %v totals %+v", names(cut.Series), truncated, cut.AnnualCostTotal)
	}
	exact, truncated := e.recurring(t, store.RecurringParams{Limit: 9})
	if len(exact.Series) != 9 || truncated {
		t.Errorf("limit equal to the count: %d rows, truncated %v", len(exact.Series), truncated)
	}
	none, truncated := e.recurring(t, store.RecurringParams{Query: "no such payee"})
	if none.Series == nil || none.AnnualCostTotal == nil || len(none.Series) != 0 || len(none.AnnualCostTotal) != 0 || truncated {
		t.Errorf("nothing matches: %+v (truncated %v), want empty lists", none, truncated)
	}

	for name, p := range map[string]store.RecurringParams{
		"status": {Statuses: []string{"active", "bogus"}}, "sort": {Sort: "size"}, "limit": {Limit: -1}, "big limit": {Limit: 1001},
	} {
		_, _, err := e.r.RecurringPayments(context.Background(), p)
		invalid(t, name, err)
	}
}

func TestPayeesTool(t *testing.T) {
	e := newEstate(t)
	ctx := context.Background()
	get := func(p store.PayeesParams) ([]store.PayeeRow, bool) {
		t.Helper()
		rows, truncated, err := e.r.Payees(ctx, p)
		if err != nil {
			t.Fatalf("Payees(%+v): %v", p, err)
		}
		return rows, truncated
	}
	find := func(rows []store.PayeeRow, name string) store.PayeeRow {
		t.Helper()
		for _, r := range rows {
			if r.PayeeName == name {
				return r
			}
		}
		t.Fatalf("no payee %q in %d rows", name, len(rows))
		return store.PayeeRow{}
	}

	// All time, from agent.payees.
	rows, truncated := get(store.PayeesParams{Query: "gym"})
	if len(rows) != 1 || truncated {
		t.Fatalf("gym: %+v", rows)
	}
	gym := rows[0]
	var years int64
	for _, y := range gym.Years {
		years += y.OutMinor
		if y.TxnCount == 0 || y.Year < 2000 {
			t.Errorf("year %+v", y)
		}
	}
	if gym.PayeeID != "n:gym co" || gym.Currency != "GBP" || gym.OutMinor != 15000 || gym.InMinor != 0 || gym.TxnCount != 5 ||
		gym.FirstDate != e.at(-130) || gym.LastDate != e.at(-10) || gym.FlowClass == nil || *gym.FlowClass != "spending" ||
		!slices.Equal(gym.Keys, []string{"n:gym co"}) || !slices.Equal(gym.NameVariants, []string{"Gym Co"}) || years != 15000 ||
		gym.Employer != nil && *gym.Employer {
		t.Errorf("gym = %+v", gym)
	}
	// A date range restricts the totals: the two charges 70 and 40 days ago.
	rows, _ = get(store.PayeesParams{Query: "gym", From: e.at(-75), To: e.at(-35)})
	if len(rows) != 1 || rows[0].OutMinor != 6000 || rows[0].TxnCount != 2 || rows[0].InMinor != 0 {
		t.Errorf("gym in range = %+v", rows)
	}
	// Payees with nothing in the range are left out.
	rows, _ = get(store.PayeesParams{Query: "gym", From: e.at(-5)})
	if len(rows) != 0 {
		t.Errorf("gym after its last charge = %+v", rows)
	}
	// The ranged totals leave out classes internal and scope_transfer; the all-time ones are the
	// view's, every class. The pot move is internal.
	e.tx(itx{acct: "cur", key: "internal1", date: e.at(-3), amt: -500, desc: "Mover Co"})
	e.setting("n:mover co", "", "internal", "", "", nil, 701)
	rows, _ = get(store.PayeesParams{Query: "mover"})
	if len(rows) != 1 || rows[0].OutMinor != 500 || rows[0].FlowClass == nil || *rows[0].FlowClass != "internal" {
		t.Errorf("mover all time = %+v", rows)
	}
	rows, _ = get(store.PayeesParams{Query: "mover", From: e.at(-10)})
	if len(rows) != 0 {
		t.Errorf("mover in a range = %+v, want none (internal is left out)", rows)
	}
	// Query matches a name variant as well as the name; flow_class filters on the payee's class.
	rows, _ = get(store.PayeesParams{FlowClass: "income"})
	if len(rows) == 0 {
		t.Fatal("no income payees")
	}
	for _, r := range rows {
		if r.FlowClass == nil || *r.FlowClass != "income" {
			t.Errorf("income filter returned %+v", r)
		}
	}
	if r := find(rows, "Biz Employer"); r.Employer == nil || !*r.Employer || r.InMinor != 400000 || r.OutMinor != 0 {
		t.Errorf("Biz Employer = %+v", r)
	}

	// Sorts.
	rows, _ = get(store.PayeesParams{})
	if rows[0].PayeeName != "Grocer Biz" || !sort.SliceIsSorted(rows, func(i, j int) bool { return rows[i].OutMinor > rows[j].OutMinor }) {
		t.Errorf("sort out: first %q, %d rows", rows[0].PayeeName, len(rows))
	}
	rows, _ = get(store.PayeesParams{Sort: store.SortIn})
	if rows[0].PayeeName != "Biz Employer" || rows[1].PayeeName != "Biz Other" || !sort.SliceIsSorted(rows, func(i, j int) bool { return rows[i].InMinor > rows[j].InMinor }) {
		t.Errorf("sort in: %q, %q", rows[0].PayeeName, rows[1].PayeeName)
	}
	rows, _ = get(store.PayeesParams{Sort: store.SortLastDate})
	if rows[0].PayeeName != "Cur Biz" && rows[0].PayeeName != "Latest Co" || !sort.SliceIsSorted(rows, func(i, j int) bool { return rows[i].LastDate > rows[j].LastDate }) {
		t.Errorf("sort last_date: first %q %s", rows[0].PayeeName, rows[0].LastDate)
	}
	// In a range, sorted by what the range holds.
	rows, _ = get(store.PayeesParams{From: e.monthDate(3, 1), To: e.monthDate(3, 28)})
	if len(rows) == 0 || rows[0].PayeeName != "Grocer Biz" || rows[0].OutMinor != 100000 || rows[0].TxnCount != 5 {
		t.Errorf("a month's payees = %+v", rows[:1])
	}

	// Truncation.
	rows, truncated = get(store.PayeesParams{Limit: 2})
	if len(rows) != 2 || !truncated {
		t.Errorf("limit 2: %d rows truncated %v", len(rows), truncated)
	}
	for name, p := range map[string]store.PayeesParams{
		"flow_class": {FlowClass: "bogus"}, "sort": {Sort: "alphabet"}, "from": {From: "yesterday"}, "to": {To: "2026-13-01"},
		"from after to": {From: "2026-03-02", To: "2026-03-01"}, "limit": {Limit: 1001},
	} {
		_, _, err := e.r.Payees(ctx, p)
		invalid(t, name, err)
	}
}

func TestPayeesToolStopsAtTheByteCap(t *testing.T) {
	f := newInsightFixture(t)
	// One payee (one merchant group) with 20 raw names of 30 kB each: its row is about 600 kB, past
	// the 512 KiB budget, so no row fits and the result says it was cut.
	today := londonToday(t, f.q)
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("%02d", i) + strings.Repeat("x", 30000)
		f.tx(itx{acct: "cur", key: fmt.Sprintf("big%d", i), date: day(today, -i-1).Format("2006-01-02"), amt: -100, merchant: name,
			detail: merchantDetail("g_big", name)})
	}
	f.tx(itx{acct: "cur", key: "small", date: day(today, -30).Format("2006-01-02"), amt: -50, desc: "Small Co"})
	r := f.reader()
	rows, truncated, err := r.Payees(context.Background(), store.PayeesParams{Sort: store.SortLastDate})
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(rows) != 0 {
		t.Errorf("%d rows, truncated %v, want none and truncated", len(rows), truncated)
	}
	rows, truncated, err = r.Payees(context.Background(), store.PayeesParams{Query: "small"})
	if err != nil || truncated || len(rows) != 1 {
		t.Errorf("small: %d rows, truncated %v, %v", len(rows), truncated, err)
	}
}

func TestCashFlowTool(t *testing.T) {
	e := newEstate(t)
	ctx := context.Background()
	get := func(p store.CashFlowParams) (store.CashFlow, bool) {
		t.Helper()
		got, truncated, err := e.r.CashFlow(ctx, p)
		if err != nil {
			t.Fatalf("CashFlow(%+v): %v", p, err)
		}
		return got, truncated
	}
	type key struct{ month, scope string }
	months := func(cf store.CashFlow) map[key]store.MonthFlow {
		out := map[key]store.MonthFlow{}
		for _, m := range cf.Months {
			if m.Currency == "GBP" {
				out[key{m.Month, m.Scope}] = m
			}
		}
		return out
	}

	// The default range: the last 12 complete months, so not the current month and not 13 months
	// ago. Restrict to the business scope for exact figures.
	cf, truncated := get(store.CashFlowParams{Scopes: []string{"business"}})
	if truncated || len(cf.Months) != 3 {
		t.Fatalf("business months: %+v (truncated %v)", cf.Months, truncated)
	}
	for i, k := range []int{3, 2, 1} {
		if cf.Months[i].Month != e.month(k) || cf.Months[i].Scope != "business" || cf.Months[i].Currency != "GBP" {
			t.Errorf("row %d = %+v, want month %s", i, cf.Months[i], e.month(k))
		}
	}
	m3, m2, m1 := cf.Months[0], cf.Months[1], cf.Months[2]
	if m1.IncomeMinor != 300000 || m1.SalaryMinor != 300000 || m1.SpendingMinor != 100000 || m1.NetMinor != 200000 ||
		m1.SavingsRate == nil || *m1.SavingsRate != 0.6667 || m1.RefundMinor != 0 || m1.InternalOutMinor != 0 {
		t.Errorf("month 1 = %+v", m1)
	}
	if m2.IncomeMinor != 200000 || m2.SalaryMinor != 0 || m2.SpendingMinor != 50000 || m2.NetMinor != 150000 || m2.SavingsRate == nil || *m2.SavingsRate != 0.75 {
		t.Errorf("month 2 = %+v", m2)
	}
	if m3.IncomeMinor != 100000 || m3.SalaryMinor != 100000 || m3.SpendingMinor != 100000 || m3.NetMinor != 0 || m3.SavingsRate == nil || *m3.SavingsRate != 0 {
		t.Errorf("month 3 = %+v", m3)
	}
	// Categories: the top ones by spending in each month and scope, ties by category.
	cat := func(month, scope string) []string {
		var out []string
		for _, c := range cf.Categories {
			if c.Month == month && c.Scope == scope {
				name := "(none)"
				if c.Category != nil {
					name = *c.Category
				}
				out = append(out, fmt.Sprintf("%s=%d/%d", name, c.SpendingMinor, c.TxnCount))
			}
		}
		return out
	}
	if got := cat(e.month(1), "business"); !slices.Equal(got, []string{"groceries=40000/2", "eating_out=20000/1", "travel=20000/1", "(none)=20000/1"}) {
		t.Errorf("month 1 categories = %v", got)
	}
	if got := cat(e.month(2), "business"); !slices.Equal(got, []string{"groceries=50000/5"}) {
		t.Errorf("month 2 categories = %v", got)
	}
	// Months and categories come in month order.
	if !sort.SliceIsSorted(cf.Categories, func(i, j int) bool { return cf.Categories[i].Month < cf.Categories[j].Month }) {
		t.Errorf("categories are not in month order: %+v", cf.Categories)
	}

	// category_limit.
	cf2, _ := get(store.CashFlowParams{Scopes: []string{"business"}, CategoryLimit: 2})
	cf = cf2
	if got := cat(e.month(1), "business"); !slices.Equal(got, []string{"groceries=40000/2", "eating_out=20000/1"}) {
		t.Errorf("category_limit 2: month 1 categories = %v", got)
	}
	// All scopes by default; the joint account is its own scope.
	all, _ := get(store.CashFlowParams{})
	am := months(all)
	if j := am[key{e.month(1), "joint"}]; j.SpendingMinor != 7000 || j.IncomeMinor != 0 || j.SavingsRate != nil || j.NetMinor != -7000 {
		t.Errorf("joint month 1 = %+v", j)
	}
	if p := am[key{e.month(1), "personal"}]; p.Month == "" {
		t.Errorf("no personal row in %+v", all.Months)
	}
	if _, ok := am[key{e.month(13), "business"}]; ok {
		t.Error("the month 13 months ago is in the default range")
	}
	if _, ok := am[key{e.month(0), "business"}]; ok {
		t.Error("the current month is in the default range")
	}
	for i := 1; i < len(all.Months); i++ {
		a, b := all.Months[i-1], all.Months[i]
		if a.Month > b.Month || a.Month == b.Month && a.Scope > b.Scope {
			t.Errorf("months out of order: %s %s before %s %s", a.Month, a.Scope, b.Month, b.Scope)
		}
	}
	joint, _ := get(store.CashFlowParams{Scopes: []string{"joint", "business"}, FromMonth: e.month(1), ToMonth: e.month(1)})
	if len(joint.Months) != 2 || joint.Months[0].Scope != "business" || joint.Months[1].Scope != "joint" {
		t.Errorf("scopes: %+v", joint.Months)
	}

	// An explicit range, the current month, and pending rows.
	cur, _ := get(store.CashFlowParams{Scopes: []string{"business"}, FromMonth: e.month(0), ToMonth: e.month(0)})
	if len(cur.Months) != 1 || cur.Months[0].SpendingMinor != 300 || cur.Months[0].NetMinor != -300 {
		t.Errorf("current month, settled: %+v", cur.Months)
	}
	pend, _ := get(store.CashFlowParams{Scopes: []string{"business"}, FromMonth: e.month(0), ToMonth: e.month(0), IncludePending: true})
	if len(pend.Months) != 1 || pend.Months[0].SpendingMinor != 1000 || pend.Months[0].NetMinor != -1000 {
		t.Errorf("current month, with pending: %+v", pend.Months)
	}
	// Categories stay settled only.
	if len(pend.Categories) != 1 || pend.Categories[0].SpendingMinor != 300 {
		t.Errorf("categories with pending: %+v", pend.Categories)
	}
	// Including pending changes nothing in a month that has none, savings rate included.
	same, _ := get(store.CashFlowParams{Scopes: []string{"business"}, IncludePending: true})
	if !reflect.DeepEqual(same.Months, months2(e, ctx, t)) {
		t.Errorf("pending changed a settled month:\n%+v\n%+v", same.Months, months2(e, ctx, t))
	}
	old, _ := get(store.CashFlowParams{Scopes: []string{"business"}, FromMonth: e.month(13), ToMonth: e.month(13)})
	if len(old.Months) != 1 || old.Months[0].SpendingMinor != 999 {
		t.Errorf("an old month: %+v", old.Months)
	}
	if only, _ := get(store.CashFlowParams{FromMonth: e.month(1)}); len(only.Months) == 0 {
		t.Error("from_month alone gave nothing")
	}

	// Upcoming payments: the views' rows, optionally by scope. A row with no account stays.
	up := all.Upcoming
	if len(up) == 0 || !sort.SliceIsSorted(up, func(i, j int) bool { return up[i].Date < up[j].Date }) {
		t.Fatalf("upcoming = %+v", up)
	}
	var sawGym, sawOverdue bool
	for _, u := range up {
		switch u.PayeeName {
		case "Gym Co":
			sawGym = sawGym || u.Date == e.at(20) && u.AmountMinor == 3000 && u.Source == "inferred" && u.SeriesKey != nil
		case "Overdue Co":
			sawOverdue = sawOverdue || u.Date == e.at(-8)
		case "Ignored Co", "Lapsed Co":
			t.Errorf("upcoming has %s", u.PayeeName)
		}
		if u.Date > e.at(45) {
			t.Errorf("upcoming past the window: %+v", u)
		}
	}
	if !sawGym || !sawOverdue {
		t.Errorf("upcoming misses Gym Co (%v) or Overdue Co (%v): %+v", sawGym, sawOverdue, up)
	}
	if biz, _ := get(store.CashFlowParams{Scopes: []string{"business"}}); len(biz.Upcoming) != 0 {
		t.Errorf("business upcoming = %+v, want none", biz.Upcoming)
	}
	if pers, _ := get(store.CashFlowParams{Scopes: []string{"personal"}}); len(pers.Upcoming) != len(up) {
		t.Errorf("personal upcoming = %d rows, want %d", len(pers.Upcoming), len(up))
	}

	// Each array is cut by the limit, and the call says so.
	lim, truncated := get(store.CashFlowParams{Limit: 1})
	if len(lim.Months) != 1 || len(lim.Categories) != 1 || len(lim.Upcoming) != 1 || !truncated {
		t.Errorf("limit 1: %d months, %d categories, %d upcoming, truncated %v", len(lim.Months), len(lim.Categories), len(lim.Upcoming), truncated)
	}
	empty, truncated := get(store.CashFlowParams{FromMonth: "2001-01", ToMonth: "2001-02", Scopes: []string{"business"}})
	if empty.Months == nil || empty.Categories == nil || empty.Upcoming == nil || len(empty.Months) != 0 || truncated {
		t.Errorf("an empty range: %+v (truncated %v), want empty lists", empty, truncated)
	}

	for name, p := range map[string]store.CashFlowParams{
		"month format": {FromMonth: "2026-13"}, "month text": {ToMonth: "last month"}, "date as month": {FromMonth: "2026-01-01"},
		"from after to": {FromMonth: "2026-03", ToMonth: "2026-02"}, "scope": {Scopes: []string{"business", "household"}},
		"category_limit": {CategoryLimit: store.MaxCategoryLimit + 1}, "negative category_limit": {CategoryLimit: -1},
		"limit": {Limit: 1001},
	} {
		_, _, err := e.r.CashFlow(ctx, p)
		invalid(t, name, err)
	}
}

// months2 reads the business months settled only, the default range.
func months2(e *estate, ctx context.Context, t *testing.T) []store.MonthFlow {
	t.Helper()
	got, _, err := e.r.CashFlow(ctx, store.CashFlowParams{Scopes: []string{"business"}})
	if err != nil {
		t.Fatal(err)
	}
	return got.Months
}

func TestFinanceProfileTool(t *testing.T) {
	e := newEstate(t)
	ctx := context.Background()
	p, truncated, err := e.r.FinanceProfile(ctx, store.FinanceProfileParams{})
	if err != nil || truncated {
		t.Fatalf("FinanceProfile: %v (truncated %v)", err, truncated)
	}

	// The data span.
	if p.Span.FirstDate == nil || *p.Span.FirstDate != e.first.Format("2006-01-02") || p.Span.LastDate == nil || *p.Span.LastDate != e.last.Format("2006-01-02") {
		t.Errorf("span = %+v, want %s to %s", p.Span, e.first.Format("2006-01-02"), e.last.Format("2006-01-02"))
	}
	// Accounts by slug, with their scope and where it came from.
	scopes := map[string]store.ProfileAccount{}
	for _, a := range p.Accounts {
		scopes[a.Slug] = a
	}
	if len(p.Accounts) != len(e.acct) || !sort.SliceIsSorted(p.Accounts, func(i, j int) bool { return p.Accounts[i].Slug < p.Accounts[j].Slug }) {
		t.Errorf("%d accounts, want %d, in slug order", len(p.Accounts), len(e.acct))
	}
	for slug, want := range map[string][2]string{"cur": {"personal", "default"}, "joint": {"joint", "type"}, "biz": {"business", "type"},
		"ob-joint": {"joint", "setting"}, "pot-cur": {"personal", "parent"}} {
		if a := scopes[slug]; a.Scope != want[0] || a.ScopeSource != want[1] || a.Source == "" {
			t.Errorf("account %s = %+v, want scope %s from %s", slug, a, want[0], want[1])
		}
	}
	if scopes["cur"].Source != "monzo" || scopes["ob"].Source != "truelayer" {
		t.Errorf("sources: cur %q ob %q", scopes["cur"].Source, scopes["ob"].Source)
	}

	// Monthly medians over the last 12 complete months, per scope.
	var biz *store.ProfileMedian
	for i := range p.MonthlyMedians {
		if p.MonthlyMedians[i].Scope == "business" && p.MonthlyMedians[i].Currency == "GBP" {
			biz = &p.MonthlyMedians[i]
		}
	}
	if biz == nil || biz.IncomeMinor != 200000 || biz.SalaryMinor != 100000 || biz.SpendingMinor != 100000 ||
		biz.SavingsRate == nil || *biz.SavingsRate != 0.6667 {
		t.Errorf("business medians = %+v", biz)
	}
	if len(p.MonthlyMedians) < 3 {
		t.Errorf("medians = %+v, want rows for each scope with months", p.MonthlyMedians)
	}
	// The salary day: the most common day of the month of employer income.
	if p.SalaryDay == nil || *p.SalaryDay != 25 {
		t.Errorf("salary_day = %v, want 25", p.SalaryDay)
	}
	// Live recurring series by evidence: active, due and overdue, not ignored.
	wantRec := []store.ProfileRecurring{
		{Evidence: "inferred", Currency: "EUR", Count: 1, AnnualCostMinor: 8400},
		{Evidence: "inferred", Currency: "GBP", Count: 7, AnnualCostMinor: 36000 + 16000 + 30000 + 48000 + 14400 + 24000 + 31200},
		{Evidence: "provider_subscription", Currency: "GBP", Count: 1, AnnualCostMinor: 12000},
	}
	if !slices.Equal(p.Recurring, wantRec) {
		t.Errorf("recurring = %+v, want %+v", p.Recurring, wantRec)
	}
	// The ten largest series.
	var ts []string
	for _, s := range p.TopSeries {
		ts = append(ts, fmt.Sprintf("%s=%d", s.PayeeName, s.AnnualCostMinor))
	}
	if want := []string{"Drop Co=48000", "Gym Co=36000", "Overdue Co=31200", "Rise Co=30000", "Due Co=24000", "Water Co=16000",
		"Price Co=14400", "Video Co=12000", "Euro Co=8400"}; !slices.Equal(ts, want) {
		t.Errorf("top_series = %v, want %v", ts, want)
	}
	// The ten largest payees by spending over the last 12 months, from the tally.
	type pay struct {
		name string
		amt  int64
	}
	var wantPay []pay
	for k, v := range e.spendBy {
		wantPay = append(wantPay, pay{k, v})
	}
	sort.Slice(wantPay, func(i, j int) bool {
		if wantPay[i].amt != wantPay[j].amt {
			return wantPay[i].amt > wantPay[j].amt
		}
		return wantPay[i].name < wantPay[j].name
	})
	if len(wantPay) > 10 {
		wantPay = wantPay[:10]
	}
	if len(p.TopPayees) != len(wantPay) || len(wantPay) != 10 {
		t.Fatalf("top_payees has %d rows, want %d", len(p.TopPayees), len(wantPay))
	}
	for i, w := range wantPay {
		got := p.TopPayees[i]
		if g := strings.ToLower(got.PayeeName) + "/" + got.Currency; g != w.name || got.SpendingMinor != w.amt || !strings.HasPrefix(got.PayeeID, "n:") {
			t.Errorf("top_payees[%d] = %+v, want %s %d", i, got, w.name, w.amt)
		}
	}
	// Anomalies in the last 90 days, by kind.
	wantAn := []store.ProfileAnomalyCount{{Kind: "new_large_payee", Count: 1}, {Kind: "possible_duplicate", Count: 2}}
	if !slices.Equal(p.Anomalies90d, wantAn) {
		t.Errorf("anomalies_90d = %+v, want %+v", p.Anomalies90d, wantAn)
	}
	// What the operator could still correct.
	tc := p.ToCorrect
	if !slices.Equal(tc.UnnamedCategories, []string{"category_zz9"}) {
		t.Errorf("unnamed_categories = %v", tc.UnnamedCategories)
	}
	wantUnc := []store.ProfileUnclassified{
		{Currency: "EUR", Count: e.spendN["EUR"], AmountMinor: e.spendAmt["EUR"]},
		{Currency: "GBP", Count: e.spendN["GBP"], AmountMinor: e.spendAmt["GBP"]},
	}
	if !slices.Equal(tc.UnclassifiedSpending, wantUnc) {
		t.Errorf("unclassified_spending = %+v, want %+v", tc.UnclassifiedSpending, wantUnc)
	}
	if tc.UnclassifiedOBTransfers != 1 || tc.UnpairedCardCredits != 1 || tc.RuleProblems != 1 {
		t.Errorf("to_correct counts = %d ob transfers, %d card credits, %d rule problems, want 1 each",
			tc.UnclassifiedOBTransfers, tc.UnpairedCardCredits, tc.RuleProblems)
	}

	// Each array is cut by the limit.
	lim, truncated, err := e.r.FinanceProfile(ctx, store.FinanceProfileParams{Limit: 1})
	if err != nil || !truncated {
		t.Fatalf("limit 1: %v truncated %v", err, truncated)
	}
	if len(lim.Accounts) != 1 || len(lim.Recurring) != 1 || len(lim.MonthlyMedians) != 1 || len(lim.TopPayees) != 1 ||
		len(lim.TopSeries) != 1 || len(lim.Anomalies90d) != 1 || len(lim.ToCorrect.UnclassifiedSpending) != 1 {
		t.Errorf("limit 1: %+v", lim)
	}
	if !reflect.DeepEqual(lim.Span, p.Span) || lim.ToCorrect.RuleProblems != 1 {
		t.Errorf("limit 1 changed a scalar: %+v", lim)
	}
	_, _, err = e.r.FinanceProfile(ctx, store.FinanceProfileParams{Limit: 1001})
	invalid(t, "limit", err)
}

func TestFinanceProfileWithNoData(t *testing.T) {
	f := newInsightFixture(t)
	r := f.reader()
	p, truncated, err := r.FinanceProfile(context.Background(), store.FinanceProfileParams{})
	if err != nil || truncated {
		t.Fatalf("FinanceProfile: %v (truncated %v)", err, truncated)
	}
	if p.Span.FirstDate != nil || p.Span.LastDate != nil || p.SalaryDay != nil {
		t.Errorf("span %+v salary day %v, want null", p.Span, p.SalaryDay)
	}
	// The accounts exist; every other array is an empty list, never null.
	if len(p.Accounts) != len(f.acct) {
		t.Errorf("%d accounts, want %d", len(p.Accounts), len(f.acct))
	}
	if p.MonthlyMedians == nil || p.Recurring == nil || p.TopPayees == nil || p.TopSeries == nil || p.Anomalies90d == nil ||
		p.ToCorrect.UnnamedCategories == nil || p.ToCorrect.UnclassifiedSpending == nil {
		t.Errorf("a nil array: %+v", p)
	}
	if len(p.MonthlyMedians)+len(p.Recurring)+len(p.TopPayees)+len(p.TopSeries)+len(p.Anomalies90d) != 0 ||
		p.ToCorrect.UnclassifiedOBTransfers != 0 || p.ToCorrect.UnpairedCardCredits != 0 || p.ToCorrect.RuleProblems != 0 {
		t.Errorf("an empty ledger has content: %+v", p)
	}
	// The other tools answer an empty ledger with empty lists.
	if rec, _, err := r.RecurringPayments(context.Background(), store.RecurringParams{}); err != nil || rec.Series == nil || rec.AnnualCostTotal == nil {
		t.Errorf("recurring on empty: %+v %v", rec, err)
	}
	if rows, _, err := r.Payees(context.Background(), store.PayeesParams{}); err != nil || rows == nil || len(rows) != 0 {
		t.Errorf("payees on empty: %+v %v", rows, err)
	}
	if cf, _, err := r.CashFlow(context.Background(), store.CashFlowParams{}); err != nil || cf.Months == nil || cf.Categories == nil || cf.Upcoming == nil {
		t.Errorf("cash flow on empty: %+v %v", cf, err)
	}
}

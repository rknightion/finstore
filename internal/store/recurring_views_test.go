package store_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rknightion/finstore/internal/store"
)

// recFix is the insight fixture plus today (Europe/London) and helpers that write whole made-up
// series of one payee at day offsets from today. Every value is invented.
type recFix struct {
	*insightFixture
	today time.Time
}

func newRecFix(t *testing.T) *recFix {
	t.Helper()
	f := newInsightFixture(t)
	return &recFix{insightFixture: f, today: londonToday(t, f.q)}
}

func (r *recFix) at(off int) string { return day(r.today, off).Format("2006-01-02") }

func (r *recFix) date(off int) time.Time { return day(r.today, off) }

// rs describes one payee's run of transactions: amounts are positive pence, offsets are days from
// today in ascending order.
type rs struct {
	name, acct, scheme, status string
	amts                       []int64
	offs                       []int
	detail                     map[string]any
	in                         bool // money in
	viaCP                      bool // the name is the counterparty, not the description
	noName                     bool // no description or counterparty: an x: payee
}

// steps gives n ascending offsets gap days apart, the last at offset last.
func steps(n, gap, last int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = last - gap*(n-1-i)
	}
	return out
}

// fromGaps gives the ascending offsets whose consecutive differences are gaps, the last at last.
func fromGaps(last int, gaps []int) []int {
	out := make([]int, len(gaps)+1)
	out[len(gaps)] = last
	for i := len(gaps) - 1; i >= 0; i-- {
		out[i] = out[i+1] - gaps[i]
	}
	return out
}

func flat(amt int64, n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = amt
	}
	return out
}

func repeat(parts ...[]int64) []int64 {
	var out []int64
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func (r *recFix) key(name string, i int) string { return fmt.Sprintf("%s/%d", name, i) }

// put writes the run and returns the transaction keys.
func (r *recFix) put(s rs) {
	r.t.Helper()
	if len(s.amts) != len(s.offs) {
		r.t.Fatalf("%s: %d amounts for %d offsets", s.name, len(s.amts), len(s.offs))
	}
	acct := s.acct
	if acct == "" {
		acct = "cur"
	}
	for i, off := range s.offs {
		amt := -s.amts[i]
		if s.in {
			amt = s.amts[i]
		}
		x := itx{acct: acct, key: r.key(s.name, i), date: r.at(off), amt: amt, scheme: s.scheme, status: s.status, detail: s.detail}
		switch {
		case s.noName:
			x.blank = true
		case s.viaCP:
			x.cp = s.name
		default:
			x.desc = s.name
		}
		r.tx(x)
	}
}

// seriesKey is the key an inferred series of the run has: its first transaction.
func (r *recFix) seriesKey(name string) string { return "inf:" + r.ids[r.key(name, 0)].String() }

type priceChange struct {
	Date string `json:"date"`
	From int64  `json:"from_minor"`
	To   int64  `json:"to_minor"`
}

type recRow struct {
	key, payeeID, payeeName, evidence, cadence, kind string
	occ                                              int64
	first, last                                      time.Time
	lastAmt, typical                                 int64
	currency                                         string
	annual                                           *int64
	next                                             *time.Time
	status                                           string
	changes                                          []priceChange
	change12                                         *int64
	class                                            string
	category, categoryName                           *string
	slugs                                            []string
	state                                            *string
	conf                                             string
}

// recurring reads agent.recurring_payments, by payee name.
func (r *recFix) recurring() map[string][]recRow {
	r.t.Helper()
	rows, err := r.q.Query(context.Background(), `SELECT series_key, payee_id, payee_name, evidence, cadence, amount_kind,
		occurrences, first_date, last_date, last_amount_minor, typical_amount_minor, currency, annual_cost_minor,
		next_expected_date, status, price_changes, change_12m_minor, flow_class, category, category_name,
		account_slugs, operator_state, confidence FROM agent.recurring_payments ORDER BY series_key`)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][]recRow{}
	for rows.Next() {
		var x recRow
		if err := rows.Scan(&x.key, &x.payeeID, &x.payeeName, &x.evidence, &x.cadence, &x.kind, &x.occ, &x.first, &x.last,
			&x.lastAmt, &x.typical, &x.currency, &x.annual, &x.next, &x.status, &x.changes, &x.change12, &x.class,
			&x.category, &x.categoryName, &x.slugs, &x.state, &x.conf); err != nil {
			r.t.Fatal(err)
		}
		out[x.payeeName] = append(out[x.payeeName], x)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatal(err)
	}
	return out
}

// one returns the only series of the payee, or fails.
func one(t *testing.T, all map[string][]recRow, name string) recRow {
	t.Helper()
	if len(all[name]) != 1 {
		t.Fatalf("%s has %d series, want 1: %+v", name, len(all[name]), all[name])
	}
	return all[name][0]
}

func none(t *testing.T, all map[string][]recRow, name string) {
	t.Helper()
	if len(all[name]) != 0 {
		t.Errorf("%s has %d series, want none: %+v", name, len(all[name]), all[name])
	}
}

func (r *recFix) setSeries(key, decision string, seq int64) {
	r.t.Helper()
	r.exec(`INSERT INTO core.series_setting (series_key, recurring, entry_seq, entered_at) VALUES ($1, $2, $3, now())`, key, decision, seq)
}

func TestRecurringViewColumns(t *testing.T) {
	f := newInsightFixture(t)
	requireColumns(t, f.q, "recurring_payments", []string{"series_key", "payee_id", "payee_name", "evidence", "cadence",
		"amount_kind", "occurrences", "first_date", "last_date", "last_amount_minor", "typical_amount_minor", "currency",
		"annual_cost_minor", "next_expected_date", "status", "price_changes", "change_12m_minor", "flow_class", "category",
		"category_name", "account_slugs", "operator_state", "confidence"})
	requireColumns(t, f.q, "upcoming_payments", []string{"date", "payee_id", "payee_name", "amount_minor", "currency",
		"source", "series_key", "account_slug"})
	requireColumns(t, f.q, "monthly_flows", []string{"month", "scope", "currency", "status", "income_minor", "salary_minor",
		"refund_minor", "spending_minor", "debt_repayment_minor", "investment_out_minor", "investment_in_minor",
		"own_external_out_minor", "own_external_in_minor", "scope_transfer_out_minor", "scope_transfer_in_minor",
		"internal_out_minor", "internal_in_minor", "net_minor", "savings_rate"})
	requireColumns(t, f.q, "category_months", []string{"month", "scope", "currency", "category", "category_name",
		"spending_minor", "txn_count"})
	requireColumns(t, f.q, "anomalies", []string{"transaction_id", "txn_date", "account_slug", "payee_id", "payee_name",
		"amount_minor", "currency", "kind", "reference_minor"})
	// Every new view is empty and readable on an empty database.
	for _, v := range []string{"recurring_payments", "upcoming_payments", "monthly_flows", "category_months", "anomalies"} {
		var n int
		if err := f.q.QueryRow(context.Background(), `SELECT count(*) FROM agent.`+v).Scan(&n); err != nil || n != 0 {
			t.Errorf("agent.%s on an empty database: %d rows, %v", v, n, err)
		}
	}
}

func TestRecurringDeclaredSeries(t *testing.T) {
	r := newRecFix(t)
	// Each declared kind, three monthly charges on the 30 day step.
	r.put(rs{name: "Energy Co", amts: flat(5000, 3), offs: steps(3, 30, -10), detail: md("bacs_direct_debit_instruction_id", "mandate_a")})
	r.put(rs{name: "Landlord Co", amts: flat(90000, 3), offs: steps(3, 30, -10), detail: md("series_id", "so_a")})
	r.put(rs{name: "Video Co", amts: flat(1000, 3), offs: steps(3, 30, -10), detail: md("subscription_id", "sub_a")})
	// A transaction with several ids joins the first in the order dd, so, sub.
	r.put(rs{name: "Both Co", amts: flat(700, 3), offs: steps(3, 30, -10),
		detail: md("bacs_direct_debit_instruction_id", "mandate_b", "series_id", "so_b", "subscription_id", "sub_b")})
	r.put(rs{name: "Order Co", amts: flat(800, 3), offs: steps(3, 30, -10), detail: md("series_id", "so_c", "subscription_id", "sub_c")})
	// A declared series is listed from its first transaction, whatever its cadence, and carries
	// its own key, not an inferred one. Its transactions are not also inferred: the amounts here
	// would make a weekly inferred series.
	r.put(rs{name: "Fresh Co", amts: flat(1500, 1), offs: []int{-3}, detail: md("bacs_direct_debit_instruction_id", "mandate_fresh")})
	r.put(rs{name: "Odd Co", amts: flat(2500, 4), offs: fromGaps(-2, []int{10, 40, 17}), detail: md("subscription_id", "sub_odd")})
	r.put(rs{name: "Mixed Co", amts: flat(300, 6), offs: steps(6, 7, -1),
		detail: md("series_id", "so_mixed")})

	all := r.recurring()
	for _, c := range []struct{ name, key, evidence string }{
		{"Energy Co", "dd:mandate_a", "mandate"},
		{"Landlord Co", "so:so_a", "standing_order"},
		{"Video Co", "sub:sub_a", "provider_subscription"},
		{"Both Co", "dd:mandate_b", "mandate"},
		{"Order Co", "so:so_c", "standing_order"},
		{"Fresh Co", "dd:mandate_fresh", "mandate"},
		{"Odd Co", "sub:sub_odd", "provider_subscription"},
		{"Mixed Co", "so:so_mixed", "standing_order"},
	} {
		x := one(t, all, c.name)
		if x.key != c.key || x.evidence != c.evidence || x.conf != "high" || x.state != nil {
			t.Errorf("%s = key %s evidence %s confidence %s, want %s %s high", c.name, x.key, x.evidence, x.conf, c.key, c.evidence)
		}
	}
	e := one(t, all, "Energy Co")
	if e.cadence != "monthly" || e.occ != 3 || e.typical != 5000 || e.lastAmt != 5000 || e.annual == nil || *e.annual != 60000 ||
		e.kind != "fixed" || e.status != "active" || e.currency != "GBP" || e.class != "spending" {
		t.Errorf("Energy Co = %+v", e)
	}
	if e.next == nil || !e.next.Equal(r.date(20)) || !e.first.Equal(r.date(-70)) || !e.last.Equal(r.date(-10)) {
		t.Errorf("Energy Co dates: first %v last %v next %v, want %s %s %s", e.first, e.last, e.next, r.at(-70), r.at(-10), r.at(20))
	}
	if len(e.slugs) != 1 || e.slugs[0] != "cur" || e.payeeID != "n:energy co" {
		t.Errorf("Energy Co accounts %v payee %s", e.slugs, e.payeeID)
	}
	f := one(t, all, "Fresh Co")
	if f.cadence != "irregular" || f.occ != 1 || f.next != nil || f.annual != nil || f.status != "active" || f.typical != 1500 {
		t.Errorf("Fresh Co = %+v, want a one-transaction irregular series, active, with no next date or annual cost", f)
	}
	if o := one(t, all, "Odd Co"); o.cadence != "irregular" || o.occ != 4 {
		t.Errorf("Odd Co = %+v, want irregular with 4", o)
	}
	// The weekly declared series is one series of six, not also an inferred one.
	if m := one(t, all, "Mixed Co"); m.cadence != "weekly" || m.occ != 6 {
		t.Errorf("Mixed Co = %+v", m)
	}
}

func TestRecurringExclusions(t *testing.T) {
	r := newRecFix(t)
	monthly := func(name string, mod func(*rs)) {
		s := rs{name: name, amts: flat(2000, 5), offs: steps(5, 30, -10)}
		if mod != nil {
			mod(&s)
		}
		r.put(s)
	}
	// Classes that are never recurring payments: money in, internal (a pot move), pending rows, an x: payee.
	monthly("Pay Co", func(s *rs) { s.in = true })
	monthly("Pot Co", func(s *rs) { s.detail = md("pot_id", "pot_cur") })
	monthly("Pending Co", func(s *rs) { s.status = "pending" })
	monthly("Declined Co", func(s *rs) { s.status = "declined" })
	monthly("Xpay", func(s *rs) { s.noName = true; s.scheme = "faster_payments" })
	// A held counterparty in the same scope is internal; in another scope it is a scope_transfer.
	monthly("Own Savings", func(s *rs) { s.viaCP = true; s.detail = cpDetail("account_id", "acc_sav") })
	monthly("Own Joint", func(s *rs) { s.viaCP = true; s.detail = cpDetail("account_id", "acc_joint") })
	// Every other money-out class is listed: own_external, investment, debt_repayment, spending.
	monthly("Self Co", func(s *rs) { s.detail = md("p2p_is_to_self", true) })
	r.setting("n:broker co", "", "investment", "", "", nil, 5)
	monthly("Broker Co", nil)
	monthly("Loan Co", func(s *rs) { s.scheme = "instalment_loan" })
	monthly("Shop Co", nil)
	// An x: payee is never a series, declared or not.
	r.put(rs{name: "Blank Mandate", noName: true, scheme: "faster_payments", amts: flat(900, 3), offs: steps(3, 30, -10),
		detail: md("bacs_direct_debit_instruction_id", "mandate_blank")})

	all := r.recurring()
	for _, n := range []string{"Pay Co", "Pot Co", "Pending Co", "Declined Co", "Own Savings", "Blank Mandate", ""} {
		none(t, all, n)
	}
	for k, v := range all {
		for _, x := range v {
			if strings.HasPrefix(x.payeeID, "x:") {
				t.Errorf("series %s (%s) belongs to the x: payee %s", x.key, k, x.payeeID)
			}
		}
	}
	for name, class := range map[string]string{"Own Joint": "scope_transfer", "Self Co": "own_external", "Broker Co": "investment",
		"Loan Co": "debt_repayment", "Shop Co": "spending"} {
		if x := one(t, all, name); x.class != class || x.cadence != "monthly" {
			t.Errorf("%s class %s cadence %s, want %s monthly", name, x.class, x.cadence, class)
		}
	}
}

func TestRecurringCardChecks(t *testing.T) {
	r := newRecFix(t)
	// A card scheme row of at most 100 pence is a card check and is left out; 101 pence is not.
	r.put(rs{name: "Check Co", scheme: "mastercard", amts: flat(100, 6), offs: steps(6, 30, -10)})
	r.put(rs{name: "Check Gps Co", scheme: "gps_mastercard", amts: flat(50, 6), offs: steps(6, 30, -10)})
	r.put(rs{name: "Edge Card Co", scheme: "mastercard", amts: flat(101, 5), offs: steps(5, 30, -10)})
	// Another scheme is not a card: a small bank payment counts.
	r.put(rs{name: "Bank Co", scheme: "faster_payments", amts: flat(100, 5), offs: steps(5, 30, -10)})
	// Card checks inside a real series do not disturb it: the checks would otherwise form their own band.
	r.put(rs{name: "Gym Co", scheme: "mastercard", amts: flat(3000, 5), offs: steps(5, 30, -12)})
	r.put(rs{name: "Gym Co", scheme: "mastercard", amts: flat(50, 5), offs: steps(5, 30, -27)})

	all := r.recurring()
	none(t, all, "Check Co")
	none(t, all, "Check Gps Co")
	if x := one(t, all, "Edge Card Co"); x.occ != 5 {
		t.Errorf("Edge Card Co = %+v", x)
	}
	one(t, all, "Bank Co")
	if g := one(t, all, "Gym Co"); g.occ != 5 || g.typical != 3000 {
		t.Errorf("Gym Co = %+v, want the five real charges only", g)
	}
}

func TestRecurringBands(t *testing.T) {
	r := newRecFix(t)
	// Two concurrent subscriptions at one merchant: A on the 1st-like step, B 15 days later and larger.
	// A's price rises within the tolerance (1000 to 1200), a step that both bands could take; each
	// charge joins the open band whose latest charge is oldest, so it finds its own.
	const months = 28
	aOffs := make([]int, months)
	bOffs := make([]int, months)
	aAmts := make([]int64, months)
	for k := 0; k < months; k++ {
		aOffs[k] = -30*(months-1-k) - 20
		bOffs[k] = aOffs[k] + 15
		aAmts[k] = 1000
		if k >= 10 {
			aAmts[k] = 1200
		}
	}
	r.put(rs{name: "Cloud Co", amts: aAmts, offs: aOffs})
	// B is written second; the fixture keys must differ from A's.
	for k := 0; k < months; k++ {
		r.tx(itx{acct: "cur", key: fmt.Sprintf("Cloud Co B/%d", k), date: r.at(bOffs[k]), amt: -1400, desc: "Cloud Co"})
	}
	aFirst := "inf:" + r.ids[r.key("Cloud Co", 0)].String()
	bFirst := "inf:" + r.ids["Cloud Co B/0"].String()

	// The tolerance is a quarter of the band's latest amount, inclusive: 1250 follows 1000 into its
	// band, 1251 opens a band of its own.
	r.put(rs{name: "Edge Co", amts: []int64{1000, 1250, 1000, 1250, 1000}, offs: steps(5, 30, -10)})
	r.put(rs{name: "Edge Over Co", amts: []int64{1000, 1251, 1000, 1251, 1000}, offs: steps(5, 30, -10)})

	// A closed band is not rejoined: the same amount again 505 days later is a new series.
	r.put(rs{name: "Closed Co", amts: flat(1000, 8), offs: append(steps(4, 30, -600), steps(4, 30, -5)...)})

	// A variable bill: every amount within the tolerance of the one before (5400 after 7200 is
	// exactly a quarter), one band, a median of the last six for its typical amount.
	r.put(rs{name: "Water Co", amts: []int64{6000, 7200, 5400, 6600, 7000, 5800}, offs: steps(6, 30, -10)})

	all := r.recurring()
	cloud := all["Cloud Co"]
	if len(cloud) != 2 {
		t.Fatalf("Cloud Co has %d series, want 2: %+v", len(cloud), cloud)
	}
	byKey := map[string]recRow{cloud[0].key: cloud[0], cloud[1].key: cloud[1]}
	a, b := byKey[aFirst], byKey[bFirst]
	if a.occ != months || b.occ != months {
		t.Fatalf("Cloud Co series %v have occurrences A %d, B %d, want %d each (keys %s, %s)", byKey, a.occ, b.occ, months, aFirst, bFirst)
	}
	if a.cadence != "monthly" || b.cadence != "monthly" || a.typical != 1200 || b.typical != 1400 || a.kind != "fixed" || b.kind != "fixed" {
		t.Errorf("Cloud Co A = %+v, B = %+v", a, b)
	}
	wantDate := r.date(aOffs[10]).Format("2006-01-02")
	if len(a.changes) != 1 || a.changes[0] != (priceChange{Date: wantDate, From: 1000, To: 1200}) || len(b.changes) != 0 {
		t.Errorf("Cloud Co price changes: A %+v, B %+v, want A one change on %s from 1000 to 1200 and none for B", a.changes, b.changes, wantDate)
	}

	if x := one(t, all, "Edge Co"); x.occ != 5 || x.cadence != "monthly" {
		t.Errorf("Edge Co = %+v, want one band of five (1250 is within the tolerance of 1000)", x)
	}
	none(t, all, "Edge Over Co")

	closed := all["Closed Co"]
	if len(closed) != 2 {
		t.Fatalf("Closed Co has %d series, want 2 (a band closes 400 days after its latest charge): %+v", len(closed), closed)
	}
	sort.Slice(closed, func(i, j int) bool { return closed[i].first.Before(closed[j].first) })
	if closed[0].occ != 4 || closed[1].occ != 4 || closed[0].status != "lapsed" || closed[1].status != "active" {
		t.Errorf("Closed Co = %+v", closed)
	}

	w := one(t, all, "Water Co")
	if w.kind != "variable" || w.cadence != "monthly" || w.occ != 6 || w.typical != 6300 || w.lastAmt != 5800 ||
		w.annual == nil || *w.annual != 75600 || w.conf != "medium" || len(w.changes) != 0 {
		t.Errorf("Water Co = %+v, want variable monthly, typical 6300, annual 75600, medium confidence, no price changes", w)
	}
}

func TestRecurringHandOver(t *testing.T) {
	r := newRecFix(t)
	// A chain of three price steps beyond the tolerance: 1000 x10, 1500 x8, 2300 x5, a charge every
	// 30 days, the last 5 days ago. One series, two hand-overs.
	r.put(rs{name: "Chain Co", amts: repeat(flat(1000, 10), flat(1500, 8), flat(2300, 5)), offs: steps(23, 30, -5)})

	// A step beyond the tolerance that starts too late to be a hand-over: the old band stopped 105
	// days before the new one began (its next charge was due 30 days after its last).
	r.put(rs{name: "Late Co", amts: repeat(flat(1000, 5), flat(1500, 4)), offs: append(steps(5, 30, -200), steps(4, 30, -5)...)})

	// A step to another cadence is not a hand-over, even when it starts on time.
	r.put(rs{name: "Switch Co", amts: repeat(flat(1000, 5), flat(1500, 5)), offs: append(steps(5, 30, -100), steps(5, 14, -2)...)})

	// A new band of one transaction can continue a band.
	r.put(rs{name: "One Co", amts: repeat(flat(1000, 5), flat(1500, 1)), offs: append(steps(5, 30, -60), -30)})

	// Two candidate successors for one band: no link is made.
	r.put(rs{name: "Tie Co", amts: flat(1000, 5), offs: steps(5, 30, -90)})
	r.tx(itx{acct: "cur", key: "Tie Co/b1", date: r.at(-60), amt: -1500, desc: "Tie Co"})
	r.tx(itx{acct: "cur", key: "Tie Co/b2", date: r.at(-60), amt: -2300, desc: "Tie Co"})

	// A price step inside the tolerance, within the last twelve months: no hand-over, and the amount
	// kind is variable, so no price changes are listed, but change_12m_minor shows it.
	r.put(rs{name: "Rise Co", amts: repeat(flat(1000, 7), flat(1200, 7)), offs: steps(14, 30, -5)})

	// A year-old series that never changed, and a series under a year old.
	r.put(rs{name: "Flat Co", amts: flat(1000, 14), offs: steps(14, 30, -5)})
	r.put(rs{name: "Young Co", amts: flat(1000, 6), offs: steps(6, 30, -5)})

	all := r.recurring()

	c := one(t, all, "Chain Co")
	if c.key != r.seriesKey("Chain Co") || c.occ != 23 || c.cadence != "monthly" || c.typical != 2300 || c.kind != "variable" {
		t.Errorf("Chain Co = %+v, want one series keyed by its first transaction with all 23", c)
	}
	d1, d2 := r.at(steps(23, 30, -5)[10]), r.at(steps(23, 30, -5)[18])
	if len(c.changes) != 2 || c.changes[0] != (priceChange{Date: d1, From: 1000, To: 1500}) ||
		c.changes[1] != (priceChange{Date: d2, From: 1500, To: 2300}) {
		t.Errorf("Chain Co price changes = %+v, want 1000 to 1500 on %s then 1500 to 2300 on %s", c.changes, d1, d2)
	}
	// The six amounts up to the charge closest to a year before the last one are five 1000s and a 1500.
	if c.change12 == nil || *c.change12 != 1300 {
		t.Errorf("Chain Co change_12m_minor = %v, want 1300 (2300 now against a median of 1000)", c.change12)
	}

	late := all["Late Co"]
	if len(late) != 2 {
		t.Fatalf("Late Co has %d series, want 2 (no hand-over): %+v", len(late), late)
	}
	switchCo := all["Switch Co"]
	if len(switchCo) != 2 {
		t.Fatalf("Switch Co has %d series, want 2 (the cadence changed): %+v", len(switchCo), switchCo)
	}
	if x := one(t, all, "One Co"); x.occ != 6 || x.key != r.seriesKey("One Co") || len(x.changes) != 1 {
		t.Errorf("One Co = %+v, want one series of six with one hand-over", x)
	}
	if tie := all["Tie Co"]; len(tie) != 1 || tie[0].occ != 5 {
		t.Errorf("Tie Co = %+v, want only the five-charge series (two successors, so no link)", tie)
	}

	rise := one(t, all, "Rise Co")
	if rise.kind != "variable" || len(rise.changes) != 0 || rise.change12 == nil || *rise.change12 != 200 || rise.typical != 1200 {
		t.Errorf("Rise Co = %+v, want variable, no price changes, change_12m_minor 200", rise)
	}
	flatCo := one(t, all, "Flat Co")
	if flatCo.change12 == nil || *flatCo.change12 != 0 || len(flatCo.changes) != 0 {
		t.Errorf("Flat Co = %+v, want change_12m_minor 0", flatCo)
	}
	if y := one(t, all, "Young Co"); y.change12 != nil {
		t.Errorf("Young Co change_12m_minor = %d, want null for a series under a year old", *y.change12)
	}
}

// cadenceCases pin the median gap ranges, minimum occurrences and the cadence names.
var cadenceCases = []struct {
	gap, n int
	want   string // "" when not listed
}{
	{5, 6, ""}, {6, 6, "weekly"}, {8, 6, "weekly"}, {9, 6, ""}, {12, 6, ""}, {13, 6, "fortnightly"}, {15, 6, "fortnightly"},
	{16, 6, ""}, {26, 5, "monthly"}, {27, 5, "four_weekly"}, {28, 5, "four_weekly"}, {29, 5, "four_weekly"}, {30, 5, "monthly"},
	{35, 5, "monthly"}, {36, 5, ""}, {84, 4, ""}, {85, 4, "quarterly"}, {97, 4, "quarterly"}, {98, 4, ""}, {174, 3, ""},
	{175, 3, "half_yearly"}, {190, 3, "half_yearly"}, {191, 3, ""}, {349, 3, ""}, {350, 3, "yearly"}, {380, 3, "yearly"},
	{381, 3, ""},
	// Minimum occurrences: four for weekly, fortnightly and four-weekly, three for monthly and
	// quarterly, two for half-yearly and yearly.
	{7, 3, ""}, {7, 4, "weekly"}, {14, 3, ""}, {14, 4, "fortnightly"}, {28, 3, ""}, {28, 4, "four_weekly"},
	{30, 2, ""}, {30, 3, "monthly"}, {90, 2, ""}, {90, 3, "quarterly"}, {180, 1, ""}, {180, 2, "half_yearly"},
	{365, 1, ""}, {365, 2, "yearly"},
}

var perYear = map[string]int64{"weekly": 52, "fortnightly": 26, "four_weekly": 13, "monthly": 12, "quarterly": 4,
	"half_yearly": 2, "yearly": 1}

func word(n int) string {
	s := ""
	for n > 0 {
		s = string(rune('a'+(n-1)%26)) + s
		n = (n - 1) / 26
	}
	return s
}

func TestRecurringCadences(t *testing.T) {
	r := newRecFix(t)
	names := make([]string, len(cadenceCases))
	for i, c := range cadenceCases {
		names[i] = fmt.Sprintf("Cadence %s Co", word(i+1))
		r.put(rs{name: names[i], amts: flat(500, c.n), offs: steps(c.n, c.gap, -1)})
	}
	// Regularity: the mean absolute deviation from the median gap is at most 20 percent of it.
	// Gaps of 24 and 36 alternate (median 30, deviation 6: exactly the limit); 23 and 37 do not.
	r.put(rs{name: "Even Co", amts: flat(500, 7), offs: fromGaps(-1, []int{24, 36, 24, 36, 24, 36})})
	r.put(rs{name: "Uneven Co", amts: flat(500, 7), offs: fromGaps(-1, []int{23, 37, 23, 37, 23, 37})})
	// Four-weekly needs every gap at most 31: one 32 day gap leaves a 28 day median monthly.
	r.put(rs{name: "Late Four Co", amts: flat(500, 6), offs: fromGaps(-1, []int{28, 28, 28, 28, 32})})

	all := r.recurring()
	for i, c := range cadenceCases {
		got := all[names[i]]
		if c.want == "" {
			if len(got) != 0 {
				t.Errorf("gap %d x%d: listed as %+v, want not listed", c.gap, c.n, got)
			}
			continue
		}
		if len(got) != 1 {
			t.Errorf("gap %d x%d: %d series, want 1 (%s)", c.gap, c.n, len(got), c.want)
			continue
		}
		x := got[0]
		if x.cadence != c.want || x.annual == nil || *x.annual != 500*perYear[c.want] ||
			x.next == nil || !x.next.Equal(r.date(-1+c.gap)) {
			t.Errorf("gap %d x%d: %+v, want %s, annual %d, next %s", c.gap, c.n, x, c.want, 500*perYear[c.want], r.at(-1+c.gap))
		}
	}
	if x := one(t, all, "Even Co"); x.cadence != "monthly" {
		t.Errorf("Even Co = %+v, want monthly (deviation exactly 20 percent)", x)
	}
	none(t, all, "Uneven Co")
	if x := one(t, all, "Late Four Co"); x.cadence != "monthly" {
		t.Errorf("Late Four Co = %+v, want monthly", x)
	}
}

func TestRecurringStatus(t *testing.T) {
	r := newRecFix(t)
	// Status boundaries: active up to the next expected date, due for the grace after it (the larger
	// of 3 days and 10 percent of the gap, rounded up), overdue up to two gaps after the last charge,
	// then lapsed.
	cases := []struct {
		gap, n, last int
		want         string
	}{
		{30, 4, -30, "active"}, {30, 4, -31, "due"}, {30, 4, -33, "due"}, {30, 4, -34, "overdue"}, {30, 4, -60, "overdue"}, {30, 4, -61, "lapsed"},
		{7, 5, -7, "active"}, {7, 5, -8, "due"}, {7, 5, -10, "due"}, {7, 5, -11, "overdue"}, {7, 5, -14, "overdue"}, {7, 5, -15, "lapsed"},
		{34, 4, -38, "due"}, {34, 4, -39, "overdue"},
		{365, 3, -402, "due"}, {365, 3, -403, "overdue"}, {365, 3, -730, "overdue"}, {365, 3, -731, "lapsed"},
	}
	names := make([]string, len(cases))
	for i, c := range cases {
		names[i] = fmt.Sprintf("Status %s Co", word(i+1))
		r.put(rs{name: names[i], amts: flat(1000, c.n), offs: steps(c.n, c.gap, c.last)})
	}
	// A series whose latest transaction is on an account that has ended is ended, however recent.
	for i, d := range []string{"2025-10-01", "2025-10-31", "2025-11-30", "2025-12-30"} {
		r.tx(itx{acct: "old", key: fmt.Sprintf("Ended Co/%d", i), date: d, amt: -1000, desc: "Ended Co"})
	}
	// An irregular series is active while its last charge is under 400 days old.
	r.put(rs{name: "Odd Fresh Co", amts: flat(1000, 3), offs: fromGaps(-399, []int{10, 40}), detail: md("subscription_id", "sub_fresh")})
	r.put(rs{name: "Odd Stale Co", amts: flat(1000, 3), offs: fromGaps(-401, []int{10, 40}), detail: md("subscription_id", "sub_stale")})

	all := r.recurring()
	for i, c := range cases {
		if x := one(t, all, names[i]); x.status != c.want {
			t.Errorf("gap %d, last %d days: status %s, want %s (%+v)", c.gap, c.last, x.status, c.want, x)
		}
	}
	if x := one(t, all, "Ended Co"); x.status != "ended" || x.cadence != "monthly" {
		t.Errorf("Ended Co = %+v, want ended", x)
	}
	if x := one(t, all, "Odd Fresh Co"); x.status != "active" {
		t.Errorf("Odd Fresh Co status %s, want active", x.status)
	}
	if x := one(t, all, "Odd Stale Co"); x.status != "lapsed" {
		t.Errorf("Odd Stale Co status %s, want lapsed", x.status)
	}
}

func TestRecurringDecisions(t *testing.T) {
	r := newRecFix(t)
	monthly := func(name string, n int) { r.put(rs{name: name, amts: flat(2000, n), offs: steps(n, 30, -10)}) }
	monthly("Plain Co", 5)
	monthly("Six Co", 6)
	monthly("Mute Co", 5)
	monthly("Gone Co", 5)
	r.put(rs{name: "Irregular Co", amts: flat(2000, 6), offs: fromGaps(-20, []int{10, 40, 15, 60, 20})})
	r.put(rs{name: "Irregular Ignored Co", amts: flat(2000, 6), offs: fromGaps(-20, []int{10, 40, 15, 60, 20})})
	r.put(rs{name: "Mandate Co", amts: flat(2000, 3), offs: steps(3, 30, -10), detail: md("bacs_direct_debit_instruction_id", "mandate_dec")})

	r.setSeries(r.seriesKey("Irregular Co"), "confirm", 10)
	r.setSeries(r.seriesKey("Mute Co"), "ignore", 11)
	r.setSeries(r.seriesKey("Gone Co"), "not_recurring", 12)
	r.setSeries(r.seriesKey("Irregular Ignored Co"), "ignore", 13)
	r.setSeries("dd:mandate_dec", "ignore", 14)
	// A decision that names a series key that is not one: the key of a later charge of a series.
	r.setSeries("inf:"+r.ids[r.key("Plain Co", 1)].String(), "confirm", 15)

	all := r.recurring()
	p := one(t, all, "Plain Co")
	if p.state != nil || p.conf != "low" || p.cadence != "monthly" {
		t.Errorf("Plain Co = %+v, want no operator state and low confidence", p)
	}
	if s := one(t, all, "Six Co"); s.conf != "medium" {
		t.Errorf("Six Co confidence %s, want medium (six occurrences)", s.conf)
	}
	irr := one(t, all, "Irregular Co")
	if irr.cadence != "irregular" || irr.state == nil || *irr.state != "confirmed" || irr.conf != "medium" || irr.occ != 6 ||
		irr.next != nil || irr.annual != nil || irr.status != "active" {
		t.Errorf("Irregular Co = %+v, want a confirmed irregular series (medium confidence, no next date, no annual cost)", irr)
	}
	if m := one(t, all, "Mute Co"); m.state == nil || *m.state != "ignored" || m.cadence != "monthly" {
		t.Errorf("Mute Co = %+v, want listed with state ignored", m)
	}
	none(t, all, "Gone Co")
	none(t, all, "Irregular Ignored Co")
	if m := one(t, all, "Mandate Co"); m.state == nil || *m.state != "ignored" || m.conf != "high" {
		t.Errorf("Mandate Co = %+v, want ignored, still high confidence", m)
	}

	// agent.payees counts the listed series that are not ignored.
	rows, err := r.q.Query(context.Background(), `SELECT payee_id, recurring_series FROM agent.payees`)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatal(err)
		}
		counts[id] = n
	}
	rows.Close()
	for id, want := range map[string]int64{"n:plain co": 1, "n:six co": 1, "n:mute co": 0, "n:gone co": 0, "n:irregular co": 1,
		"n:mandate co": 0} {
		if counts[id] != want {
			t.Errorf("payees.recurring_series for %s = %d, want %d", id, counts[id], want)
		}
	}

	// agent.payee_rule_status: a decision on a key that is no longer a series key is no_match.
	rows, err = r.q.Query(context.Background(), `SELECT key, matches, problem FROM agent.payee_rule_status WHERE rule_table = 'series_setting'`)
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for rows.Next() {
		var k string
		var m bool
		var p *string
		if err := rows.Scan(&k, &m, &p); err != nil {
			t.Fatal(err)
		}
		if p == nil {
			status[k] = "ok"
		} else {
			status[k] = *p
		}
	}
	rows.Close()
	for k, want := range map[string]string{
		r.seriesKey("Irregular Co"): "ok", r.seriesKey("Mute Co"): "ok", r.seriesKey("Gone Co"): "ok", "dd:mandate_dec": "ok",
		"inf:" + r.ids[r.key("Plain Co", 1)].String(): "no_match",
	} {
		if status[k] != want {
			t.Errorf("series_setting %s status %q, want %q", k, status[k], want)
		}
	}
}

func TestMonthlyFlowsAndCategoryMonths(t *testing.T) {
	r := newRecFix(t)
	// All in one past month, so the figures do not depend on the day the test runs.
	m := time.Date(r.today.Year(), r.today.Month()-2, 1, 0, 0, 0, 0, time.UTC)
	month := m.Format("2006-01")
	at := func(d int) string {
		return time.Date(m.Year(), m.Month(), d, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	}
	tx := func(acct, key string, d int, amt int64, mod func(*itx)) {
		x := itx{acct: acct, key: key, date: at(d), amt: amt, desc: strings.ToUpper(strings.ReplaceAll(key, "_", " "))}
		if mod != nil {
			mod(&x)
		}
		r.tx(x)
	}
	r.setting("n:big employer", "", "income", "", "", ptr(true), 1)
	r.setting("n:broker co", "", "investment", "", "", nil, 2)
	r.setting("n:rent co", "", "", "bills", "", nil, 3)
	r.exec(`INSERT INTO core.category_name (category, name, entry_seq, entered_at) VALUES ('groceries', 'Food shop', 4, now()), ('bills', 'Bills', 5, now())`)

	// Personal scope, settled.
	tx("cur", "big_employer", 10, 300000, nil)
	tx("cur", "friend_co", 11, 5000, nil)
	tx("cur", "some_shop", 12, 2000, func(x *itx) { x.scheme = "mastercard" }) // a card credit: refund
	tx("cur", "supermarket", 13, -40000, func(x *itx) { x.category = "groceries" })
	tx("cur", "rent_co", 14, -10000, func(x *itx) { x.category = "housing" }) // the payee's category setting wins
	tx("cur", "loan_co", 15, -10000, func(x *itx) { x.scheme = "instalment_loan" })
	tx("cur", "broker_out", 16, -15000, func(x *itx) { x.desc = "BROKER CO" })
	tx("cur", "broker_in", 17, 3000, func(x *itx) { x.desc = "BROKER CO" })
	tx("cur", "self_out", 18, -7000, func(x *itx) { x.detail = md("p2p_is_to_self", true) })
	tx("cur", "self_in", 19, 1000, func(x *itx) { x.detail = md("p2p_is_to_self", true) })
	tx("cur", "pot_move", 20, -2000, func(x *itx) { x.detail = md("pot_id", "pot_cur") })
	// Both legs held in one scope: internal, and they cancel.
	tx("cur", "to_sav", 21, -600, func(x *itx) { x.cp = "Savings"; x.detail = cpDetail("account_id", "acc_sav") })
	tx("sav", "from_cur", 21, 600, func(x *itx) { x.cp = "Current"; x.detail = cpDetail("account_id", "acc_cur") })
	// Both legs held in different scopes: scope_transfer, and they cancel across the two rows.
	tx("cur", "to_joint", 22, -9000, func(x *itx) { x.cp = "Joint"; x.detail = cpDetail("account_id", "acc_joint") })
	tx("joint", "from_cur_joint", 22, 9000, func(x *itx) { x.cp = "Current"; x.detail = cpDetail("account_id", "acc_cur") })
	// Pending: kept apart.
	tx("cur", "supermarket_pending", 23, -3000, func(x *itx) { x.status = "pending"; x.desc = "SUPERMARKET"; x.category = "groceries" })
	// Joint scope.
	tx("joint", "joint_payer", 10, 100000, nil)
	tx("joint", "joint_shop", 12, -20000, func(x *itx) { x.category = "groceries" })

	type mrow struct {
		income, salary, refund, spending, debt, invOut, invIn, selfOut, selfIn, scopeOut, scopeIn, intOut, intIn, net int64
		rate                                                                                                          *float64
	}
	rows, err := r.q.Query(context.Background(), `SELECT scope, currency, status, income_minor, salary_minor, refund_minor, spending_minor,
		debt_repayment_minor, investment_out_minor, investment_in_minor, own_external_out_minor, own_external_in_minor,
		scope_transfer_out_minor, scope_transfer_in_minor, internal_out_minor, internal_in_minor, net_minor, savings_rate::float8
		FROM agent.monthly_flows WHERE month = $1`, month)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]mrow{}
	for rows.Next() {
		var scope, cur, status string
		var x mrow
		if err := rows.Scan(&scope, &cur, &status, &x.income, &x.salary, &x.refund, &x.spending, &x.debt, &x.invOut, &x.invIn,
			&x.selfOut, &x.selfIn, &x.scopeOut, &x.scopeIn, &x.intOut, &x.intIn, &x.net, &x.rate); err != nil {
			t.Fatal(err)
		}
		got[scope+"/"+cur+"/"+status] = x
	}
	rows.Close()
	if len(got) != 3 {
		t.Fatalf("monthly_flows has %d rows for %s, want 3 (personal settled, personal pending, joint settled): %+v", len(got), month, got)
	}
	p := got["personal/GBP/settled"]
	want := mrow{income: 305000, salary: 300000, refund: 2000, spending: 50000, debt: 10000, invOut: 15000, invIn: 3000,
		selfOut: 7000, selfIn: 1000, scopeOut: 9000, intOut: 2600, intIn: 600, net: 229000}
	rate := p.rate
	p.rate = nil
	if p != want {
		t.Errorf("personal settled = %+v, want %+v", p, want)
	}
	// (305000 - (50000 - 2000) - 10000) / 305000, to four places.
	if rate == nil || *rate != 0.8098 {
		t.Errorf("personal savings_rate = %v, want 0.8098", rate)
	}
	pend := got["personal/GBP/pending"]
	if pend.spending != 3000 || pend.net != -3000 || pend.income != 0 || pend.rate != nil {
		t.Errorf("personal pending = %+v, want 3000 of spending and a null savings rate", pend)
	}
	j := got["joint/GBP/settled"]
	if j.income != 100000 || j.spending != 20000 || j.scopeIn != 9000 || j.net != 80000 || j.rate == nil || *j.rate != 0.8 {
		t.Errorf("joint settled = %+v, want income 100000, spending 20000, scope transfer in 9000, net 80000, rate 0.8", j)
	}
	if p.scopeOut != j.scopeIn {
		t.Errorf("scope transfers: out %d in %d, want them equal", p.scopeOut, j.scopeIn)
	}

	// Pairs cancel: the internal rows between held accounts, and the scope transfers, sum to zero.
	var internal, scope int64
	if err := r.q.QueryRow(context.Background(), `SELECT
		coalesce(sum(amount_minor) FILTER (WHERE flow_class = 'internal' AND flow_reason IN ('own_account', 'paired', 'flex')), 0),
		coalesce(sum(amount_minor) FILTER (WHERE flow_class = 'scope_transfer'), 0) FROM agent.cash_flows`).Scan(&internal, &scope); err != nil {
		t.Fatal(err)
	}
	if internal != 0 || scope != 0 {
		t.Errorf("internal pairs sum to %d and scope transfers to %d, want 0 and 0", internal, scope)
	}

	// category_months: spending only, settled only, the payee's category and the operator's name.
	rows, err = r.q.Query(context.Background(), `SELECT scope, category, category_name, spending_minor, txn_count
		FROM agent.category_months WHERE month = $1`, month)
	if err != nil {
		t.Fatal(err)
	}
	cats := map[string]string{}
	for rows.Next() {
		var scope string
		var c, cn *string
		var sp, n int64
		if err := rows.Scan(&scope, &c, &cn, &sp, &n); err != nil {
			t.Fatal(err)
		}
		name, cat := "", ""
		if cn != nil {
			name = *cn
		}
		if c != nil {
			cat = *c
		}
		cats[scope+"/"+cat] = fmt.Sprintf("%s %d %d", name, sp, n)
	}
	rows.Close()
	wantCats := map[string]string{"personal/groceries": "Food shop 40000 1", "personal/bills": "Bills 10000 1", "joint/groceries": "Food shop 20000 1"}
	if len(cats) != len(wantCats) {
		t.Errorf("category_months = %v, want %v", cats, wantCats)
	}
	for k, v := range wantCats {
		if cats[k] != v {
			t.Errorf("category_months[%s] = %q, want %q", k, cats[k], v)
		}
	}
}

func (r *recFix) standingOrder(key string, acct *store.Account, nextOff int, amount int64, gone bool) {
	r.t.Helper()
	ctx := context.Background()
	t0 := time.Now().UTC()
	s := store.StandingOrder{ID: uuid.NewSHA1(testNS, []byte("so/"+key)), BankRef: "309999 " + key, Source: "truelayer:ob-test",
		MandateKey: key, Payee: ptr("MADE UP PAYEE"), Reference: ptr("MADE UP REF"), Frequency: ptr("MONTHLY"),
		Status: ptr("ACTIVE"), FirstDate: ptr(r.date(-100)), FirstAmountMinor: ptr(-amount),
		NextDate: ptr(r.date(nextOff)), NextAmountMinor: ptr(-amount), Currency: ptr("GBP")}
	if acct != nil {
		s.AccountID = &acct.ID
	}
	content := []byte(`{"payee":"MADE UP PAYEE"}`)
	if _, err := r.core.ApplyStandingOrder(ctx, s, content, []byte(key), t0, 20); err != nil {
		r.t.Fatal(err)
	}
	if gone {
		if _, _, err := r.core.SetMandatesDisappeared(ctx, store.MandateStandingOrder, s.Source, s.BankRef,
			[]uuid.UUID{uuid.NewSHA1(testNS, []byte("so/kept"))}, t0.Add(time.Minute)); err != nil {
			r.t.Fatal(err)
		}
	}
}

func TestUpcomingPayments(t *testing.T) {
	r := newRecFix(t)
	// Standing orders: the bank's own schedule, from today to 45 days ahead, none that disappeared.
	cur := r.acct["cur"]
	r.standingOrder("inwin", &cur, 5, 1000, false)
	r.standingOrder("edge", &cur, 45, 2000, false)
	r.standingOrder("noacct", nil, 7, 3000, false)
	r.standingOrder("beyond", &cur, 46, 4000, false)
	r.standingOrder("past", &cur, -1, 5000, false)
	r.standingOrder("gone", &cur, 6, 6000, true)

	monthly := func(name string, last int, mod func(*rs)) {
		s := rs{name: name, amts: flat(2500, 4), offs: steps(4, 30, last)}
		if mod != nil {
			mod(&s)
		}
		r.put(s)
	}
	monthly("Gym Co", -20, nil)                                                 // active: next in 10 days
	r.put(rs{name: "Weekly Co", amts: flat(500, 5), offs: steps(5, 7, -3)})     // next in 4 days
	r.put(rs{name: "Fort Co", amts: flat(900, 5), offs: steps(5, 14, -2)})      // next in 12 days
	r.put(rs{name: "Quarter Co", amts: flat(7000, 4), offs: steps(4, 90, -80)}) // next in 10 days, one only
	monthly("Due Co", -32, nil)
	monthly("Over Co", -40, nil)
	monthly("Late Co", -100, nil)
	monthly("Mute Co", -20, nil)
	r.setSeries(r.seriesKey("Mute Co"), "ignore", 1)
	monthly("Standing Co", -20, func(s *rs) { s.detail = md("series_id", "so_up") })
	monthly("Debit Co", -20, func(s *rs) { s.detail = md("bacs_direct_debit_instruction_id", "mandate_up") })
	// Paid from two accounts: no single account to name.
	for i, off := range steps(3, 30, -10) {
		acct := "cur"
		if i%2 == 1 {
			acct = "sav"
		}
		r.tx(itx{acct: acct, key: fmt.Sprintf("Sub Co/%d", i), date: r.at(off), amt: -1000, desc: "Sub Co", detail: md("subscription_id", "sub_up")})
	}
	r.put(rs{name: "Irregular Co", amts: flat(2000, 6), offs: fromGaps(-20, []int{10, 40, 15, 60, 20})})
	r.setSeries(r.seriesKey("Irregular Co"), "confirm", 2)

	type urow struct {
		date   time.Time
		payee  *string
		name   string
		amount int64
		source string
		key    *string
		slug   *string
	}
	rows, err := r.q.Query(context.Background(), `SELECT date, payee_id, payee_name, amount_minor, source, series_key, account_slug
		FROM agent.upcoming_payments ORDER BY date, payee_name, amount_minor`)
	if err != nil {
		t.Fatal(err)
	}
	var all []urow
	byName := map[string][]urow{}
	for rows.Next() {
		var u urow
		if err := rows.Scan(&u.date, &u.payee, &u.name, &u.amount, &u.source, &u.key, &u.slug); err != nil {
			t.Fatal(err)
		}
		all = append(all, u)
		byName[u.name] = append(byName[u.name], u)
	}
	rows.Close()

	so := byName["MADE UP PAYEE"]
	if len(so) != 3 {
		t.Fatalf("%d standing order rows, want 3 (in the window, at its edge, no account): %+v", len(so), so)
	}
	for _, u := range so {
		if u.source != "standing_order" || u.payee != nil || u.key != nil {
			t.Errorf("standing order row %+v, want source standing_order and no payee or series key", u)
		}
	}
	slugOf := map[int64]*string{}
	dateOf := map[int64]time.Time{}
	for _, u := range so {
		slugOf[u.amount] = u.slug
		dateOf[u.amount] = u.date
	}
	if slugOf[1000] == nil || *slugOf[1000] != "cur" || slugOf[3000] != nil || slugOf[2000] == nil {
		t.Errorf("standing order account slugs = %v, want cur for 1000 and null for 3000", slugOf)
	}
	if !dateOf[1000].Equal(r.date(5)) || !dateOf[2000].Equal(r.date(45)) || !dateOf[3000].Equal(r.date(7)) {
		t.Errorf("standing order dates = %v, want today + 5, 45 and 7", dateOf)
	}

	dates := func(name string) []string {
		var out []string
		for _, u := range byName[name] {
			out = append(out, u.date.Format("2006-01-02"))
		}
		return out
	}
	eq := func(name string, want ...int) {
		t.Helper()
		var w []string
		for _, off := range want {
			w = append(w, r.at(off))
		}
		if got := dates(name); strings.Join(got, ",") != strings.Join(w, ",") {
			t.Errorf("%s dates = %v, want %v", name, got, w)
		}
	}
	eq("Weekly Co", 4, 11, 18, 25, 32, 39)
	eq("Fort Co", 12, 26, 40)
	eq("Quarter Co", 10)
	// Monthly series continue by calendar month; compare the span, not the day arithmetic.
	for name, first := range map[string]int{"Gym Co": 10, "Due Co": -2, "Over Co": -10, "Debit Co": 10} {
		rowsOf := byName[name]
		if len(rowsOf) != 2 || !rowsOf[0].date.Equal(r.date(first)) {
			t.Errorf("%s rows = %+v, want two, the first at today %+d", name, rowsOf, first)
			continue
		}
		if span := int(rowsOf[1].date.Sub(rowsOf[0].date).Hours() / 24); span < 28 || span > 31 {
			t.Errorf("%s second row is %d days after the first, want one calendar month", name, span)
		}
	}
	if sub := byName["Sub Co"]; len(sub) != 1 || sub[0].slug != nil || sub[0].source != "provider_subscription" || !sub[0].date.Equal(r.date(20)) {
		t.Errorf("Sub Co rows = %+v, want one row at today + 20 with no single account", sub)
	}
	if d := byName["Debit Co"]; len(d) > 0 && (d[0].source != "mandate" || d[0].key == nil || *d[0].key != "dd:mandate_up" || d[0].payee == nil ||
		*d[0].payee != "n:debit co" || d[0].amount != 2500 || d[0].slug == nil || *d[0].slug != "cur") {
		t.Errorf("Debit Co row = %+v", d[0])
	}
	if g := byName["Gym Co"]; len(g) > 0 && (g[0].source != "inferred" || g[0].key == nil || *g[0].key != r.seriesKey("Gym Co")) {
		t.Errorf("Gym Co row = %+v, want source inferred and its series key", g[0])
	}
	for _, n := range []string{"Late Co", "Mute Co", "Standing Co", "Irregular Co"} {
		if len(byName[n]) != 0 {
			t.Errorf("%s has upcoming rows %+v, want none (lapsed, ignored, standing order evidence, irregular)", n, byName[n])
		}
	}
	// 3 standing orders; Gym 2, Weekly 6, Fort 3, Quarter 1, Due 2, Over 2, Debit 2, Sub 1.
	if len(all) != 3+2+6+3+1+2+2+2+1 {
		t.Errorf("%d upcoming rows, want %d: %+v", len(all), 3+2+6+3+1+2+2+2+1, all)
	}
}

type anomRow struct {
	amount int64
	ref    *int64
}

func (r *recFix) anomalies() map[string][]anomRow {
	r.t.Helper()
	rows, err := r.q.Query(context.Background(), `SELECT payee_name, kind, amount_minor, reference_minor, payee_id
		FROM agent.anomalies ORDER BY payee_name, kind, amount_minor`)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][]anomRow{}
	for rows.Next() {
		var name, kind, id string
		var a anomRow
		if err := rows.Scan(&name, &kind, &a.amount, &a.ref, &id); err != nil {
			r.t.Fatal(err)
		}
		if strings.HasPrefix(id, "x:") {
			r.t.Errorf("an x: payee is flagged: %s %s", name, kind)
		}
		out[name+"|"+kind] = append(out[name+"|"+kind], a)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatal(err)
	}
	return out
}

func TestAnomalies(t *testing.T) {
	r := newRecFix(t)
	spend := func(name string, amts []int64, offs []int) { r.put(rs{name: name, amts: amts, offs: offs}) }

	// unusual_amount: at least twelve earlier spending rows, then at least 5,000 pence, more than five
	// times their median, at least 5,000 pence above it and well clear of their spread.
	// 10000 is exactly five times 2000 (not flagged); 10001 is.
	spend("Edge Cafe", append(flat(2000, 12), 10000, 10001), steps(14, 10, -30))
	spend("Few Cafe", append(flat(2000, 11), 90000), steps(12, 10, -30))
	// Under the 5,000 pence excess: 5499 is 4,999 above the median of 500, 5500 is 5,000 above.
	spend("Small Cafe", append(flat(500, 12), 5499, 5500), steps(14, 10, -30))
	// Half the history at 200 and half at 2000: the median is 1100 and the scaled MAD 1334, so the
	// threshold is 9106. 9000 is within the payee's own spread, 9200 is not.
	mixed := []int64{200, 2000, 200, 2000, 200, 2000, 200, 2000, 200, 2000, 200, 2000}
	spend("Spread Cafe", append(append([]int64{}, mixed...), 9000), steps(13, 10, -30))
	spend("Spread Two Cafe", append(append([]int64{}, mixed...), 9200), steps(13, 10, -30))

	// possible_duplicate: same payee, account and amount created within two hours, and not a habit.
	spend("Dup Co", flat(2500, 2), []int{-20, -20})
	r.tx(itx{acct: "cur", key: "Dup Acct Co/0", date: r.at(-20), amt: -2500, desc: "Dup Acct Co"})
	r.tx(itx{acct: "sav", key: "Dup Acct Co/1", date: r.at(-20), amt: -2500, desc: "Dup Acct Co"})
	r.tx(itx{acct: "cur", key: "Dup Pend Co/0", date: r.at(-20), amt: -2500, desc: "Dup Pend Co"})
	r.tx(itx{acct: "cur", key: "Dup Pend Co/1", date: r.at(-20), amt: -2500, desc: "Dup Pend Co", status: "pending"})
	// The same amount on two days is a repeat purchase, whatever the gap.
	spend("Dup Next Day Co", flat(2500, 2), []int{-30, -29})
	spend("Dup Far Co", flat(2500, 2), []int{-30, -27})
	// The two-hour edge, and a pair that straddles midnight.
	spend("Dup Hours Co", flat(2500, 2), []int{-26, -26})
	spend("Dup Late Co", flat(2500, 2), []int{-26, -26})
	spend("Dup Midnight Co", flat(2500, 2), []int{-25, -24})
	setHour := func(name string, i int, day int, h, m int) {
		t.Helper()
		r.exec(`UPDATE core.transaction SET created_at = $2::date + make_interval(hours => $3::int, mins => $4::int) WHERE source_key = $1`,
			r.key(name, i), r.at(day), h, m)
	}
	setHour("Dup Hours Co", 0, -26, 9, 0)
	setHour("Dup Hours Co", 1, -26, 10, 59)
	setHour("Dup Late Co", 0, -26, 9, 0)
	setHour("Dup Late Co", 1, -26, 11, 1)
	setHour("Dup Midnight Co", 0, -25, 23, 30)
	setHour("Dup Midnight Co", 1, -24, 0, 30)
	// An ordinary repeat purchase: a payee the account has paid the same amount three times before
	// is not flagged for two more on one day; with two earlier it is, and only the pair.
	spend("Takeaway Co", flat(1800, 6), []int{-80, -60, -40, -21, -9, -9})
	spend("Takeaway Few Co", flat(1800, 4), []int{-60, -40, -9, -9})
	// A weekly shop is a habit, so a second charge in one day is not flagged.
	spend("Weekly Co", flat(500, 8), steps(8, 7, -1))
	r.tx(itx{acct: "cur", key: "Weekly Co/extra", date: r.at(-1), amt: -500, desc: "Weekly Co"})

	// new_large_payee: the payee's first settled transaction, spending, at least 25,000 pence.
	spend("Big Co", flat(25000, 1), []int{-15})
	spend("Near Co", flat(24999, 1), []int{-15})
	spend("Second Co", []int64{1000, 50000}, []int{-40, -10})
	r.put(rs{name: "Income Co", amts: flat(30000, 1), offs: []int{-15}, in: true})
	r.put(rs{name: "Pending Big Co", amts: flat(30000, 1), offs: []int{-15}, status: "pending"})

	// foreign_fee: a local currency other than the account's and a non-zero fee in detail.fees.
	fee := func(name, local string, fees any, withFees bool) {
		d := map[string]any{}
		if withFees {
			d["fees"] = fees
		}
		r.tx(itx{acct: "cur", key: name, date: r.at(-5), amt: -1000, desc: name, local: local, detail: d})
	}
	fee("Fee A Co", "EUR", map[string]any{"currency_conversion": 25}, true)
	fee("Fee B Co", "EUR", map[string]any{}, true)
	fee("Fee C Co", "EUR", map[string]any{"currency_conversion": 0}, true)
	fee("Fee D Co", "GBP", map[string]any{"currency_conversion": 25}, true)
	fee("Fee E Co", "", map[string]any{"currency_conversion": 25}, true)
	fee("Fee F Co", "EUR", 25, true)
	fee("Fee G Co", "EUR", nil, false)

	// An x: payee is never flagged: a large first transaction, a duplicate and a foreign fee.
	r.put(rs{name: "x1", noName: true, scheme: "faster_payments", amts: []int64{30000, 2500, 2500}, offs: []int{-15, -12, -12}})
	r.tx(itx{acct: "cur", key: "xfee", date: r.at(-5), amt: -1000, blank: true, scheme: "faster_payments", local: "EUR",
		detail: map[string]any{"fees": map[string]any{"currency_conversion": 25}}})

	got := r.anomalies()
	check := func(key string, want ...anomRow) {
		t.Helper()
		g := got[key]
		if len(g) != len(want) {
			t.Errorf("%s = %+v, want %+v", key, g, want)
			return
		}
		for i := range want {
			if g[i].amount != want[i].amount || (want[i].ref == nil) != (g[i].ref == nil) || (want[i].ref != nil && *g[i].ref != *want[i].ref) {
				t.Errorf("%s = %+v, want %+v", key, g, want)
				return
			}
		}
		delete(got, key)
	}
	check("Edge Cafe|unusual_amount", anomRow{-10001, ptr(int64(-2000))})
	check("Small Cafe|unusual_amount", anomRow{-5500, ptr(int64(-500))})
	check("Spread Two Cafe|unusual_amount", anomRow{-9200, ptr(int64(-1100))})
	dup := anomRow{-2500, ptr(int64(-2500))}
	check("Dup Co|possible_duplicate", dup, dup)
	check("Dup Hours Co|possible_duplicate", dup, dup)
	check("Dup Midnight Co|possible_duplicate", dup, dup)
	pair := anomRow{-1800, ptr(int64(-1800))}
	check("Takeaway Few Co|possible_duplicate", pair, pair)
	check("Big Co|new_large_payee", anomRow{-25000, nil})
	check("Fee A Co|foreign_fee", anomRow{-1000, nil})
	check("Fee F Co|foreign_fee", anomRow{-1000, nil})
	// Second Co's large second transaction is also unusual only with twelve earlier rows: it has one.
	for k, v := range got {
		t.Errorf("unexpected anomaly %s: %+v", k, v)
	}
}

func TestInsightViewCommentsRecordTheParameters(t *testing.T) {
	f := newInsightFixture(t)
	want := map[string][]string{
		"recurring_payments": {"400 days", "25 percent", "100 pence", "20 percent", "3 days and 10 percent", "two gaps", "weekly 6 to 8",
			"four_weekly 27 to 29 with no gap above 31", "monthly 26 to 35", "quarterly 85 to 97", "half_yearly 175 to 190",
			"yearly 350 to 380", "52, 26, 13, 12, 4, 2, 1"},
		"upcoming_payments": {"45 days"},
		"monthly_flows":     {"savings_rate", "four places", "pending"},
		"category_months":   {"settled"},
		"anomalies":         {"twelve earlier", "five times", "5,000 pence", "median absolute deviation", "2 hours", "three or more", "25,000 pence", "x: payees are never flagged"},
	}
	for view, subs := range want {
		var c string
		if err := f.q.QueryRow(context.Background(), `SELECT obj_description(('agent.' || $1)::regclass, 'pg_class')`, view).Scan(&c); err != nil {
			t.Fatal(err)
		}
		for _, s := range subs {
			if !strings.Contains(c, s) {
				t.Errorf("the comment on agent.%s does not record %q", view, s)
			}
		}
	}
}

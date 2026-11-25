package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
)

// fakeStore stands in for the store's reader behind the real adapter.
type fakeStore struct {
	overview func() (store.Overview, error)
	search   func(store.SearchParams) ([]store.TransactionRow, bool, error)
	summary  func(store.SummaryParams) ([]store.SummaryGroup, bool, error)
	describe func(string) ([]store.Relation, error)
	query    func(string, store.Limits) (store.Result, error)

	portfolio func(store.PortfolioParams) ([]store.PortfolioAccount, bool, error)
	pension   func(store.PensionSummaryParams) (store.PensionSummary, bool, error)

	recurring func(store.RecurringParams) (store.Recurring, bool, error)
	payees    func(store.PayeesParams) ([]store.PayeeRow, bool, error)
	cashFlow  func(store.CashFlowParams) (store.CashFlow, bool, error)
	profile   func(store.FinanceProfileParams) (store.FinanceProfile, bool, error)

	gotRecurring store.RecurringParams
	gotPayees    store.PayeesParams
	gotCashFlow  store.CashFlowParams
	gotProfile   store.FinanceProfileParams
	gotSearch    store.SearchParams
	gotSummary   store.SummaryParams
	gotLimits    store.Limits
	gotPortfolio store.PortfolioParams
	gotPension   store.PensionSummaryParams
}

func (f *fakeStore) Overview(context.Context) (store.Overview, error) { return f.overview() }
func (f *fakeStore) SearchTransactions(_ context.Context, p store.SearchParams) ([]store.TransactionRow, bool, error) {
	f.gotSearch = p
	return f.search(p)
}
func (f *fakeStore) SpendingSummary(_ context.Context, p store.SummaryParams) ([]store.SummaryGroup, bool, error) {
	f.gotSummary = p
	return f.summary(p)
}
func (f *fakeStore) DescribeSchema(_ context.Context, s string) ([]store.Relation, error) {
	return f.describe(s)
}
func (f *fakeStore) Query(_ context.Context, sql string, l store.Limits) (store.Result, error) {
	f.gotLimits = l
	return f.query(sql, l)
}
func (f *fakeStore) Portfolio(_ context.Context, p store.PortfolioParams) ([]store.PortfolioAccount, bool, error) {
	f.gotPortfolio = p
	if f.portfolio == nil {
		return nil, false, nil
	}
	return f.portfolio(p)
}
func (f *fakeStore) PensionSummary(_ context.Context, p store.PensionSummaryParams) (store.PensionSummary, bool, error) {
	f.gotPension = p
	if f.pension == nil {
		return store.PensionSummary{}, false, nil
	}
	return f.pension(p)
}

func (f *fakeStore) RecurringPayments(_ context.Context, p store.RecurringParams) (store.Recurring, bool, error) {
	f.gotRecurring = p
	if f.recurring == nil {
		return store.Recurring{}, false, nil
	}
	return f.recurring(p)
}
func (f *fakeStore) Payees(_ context.Context, p store.PayeesParams) ([]store.PayeeRow, bool, error) {
	f.gotPayees = p
	if f.payees == nil {
		return nil, false, nil
	}
	return f.payees(p)
}
func (f *fakeStore) CashFlow(_ context.Context, p store.CashFlowParams) (store.CashFlow, bool, error) {
	f.gotCashFlow = p
	if f.cashFlow == nil {
		return store.CashFlow{}, false, nil
	}
	return f.cashFlow(p)
}
func (f *fakeStore) FinanceProfile(_ context.Context, p store.FinanceProfileParams) (store.FinanceProfile, bool, error) {
	f.gotProfile = p
	if f.profile == nil {
		return store.FinanceProfile{}, false, nil
	}
	return f.profile(p)
}

// The adapter passes the four insight tools' parameters to the store and maps every field of their
// rows. All values are made up.
func TestAdapterMapsTheInsightTools(t *testing.T) {
	str := func(s string) *string { return &s }
	i64 := func(n int64) *int64 { return &n }
	rate := 0.25
	yes := true
	f := &fakeStore{
		summary: func(store.SummaryParams) ([]store.SummaryGroup, bool, error) { return nil, false, nil },
		recurring: func(store.RecurringParams) (store.Recurring, bool, error) {
			return store.Recurring{
				Series: []store.RecurringSeries{{SeriesKey: "inf:1", PayeeID: "n:x", PayeeName: "X", Evidence: "inferred", Cadence: "monthly",
					AmountKind: "variable", Occurrences: 6, FirstDate: "2030-01-01", LastDate: "2030-06-01", LastAmountMinor: 11,
					TypicalAmountMinor: 10, Currency: "GBP", AnnualCostMinor: i64(120), NextExpectedDate: str("2030-07-01"), Status: "due",
					PriceChanges: []store.PriceChange{{Date: "2030-03-01", FromMinor: 9, ToMinor: 10}}, Change12mMinor: i64(2),
					FlowClass: "spending", Category: str("k"), CategoryName: str("K"), AccountSlugs: []string{"a", "b"},
					OperatorState: str("confirmed"), Confidence: "medium"}},
				AnnualCostTotal: []store.AnnualCostTotal{{Currency: "GBP", TotalMinor: 120}},
			}, true, nil
		},
		payees: func(store.PayeesParams) ([]store.PayeeRow, bool, error) {
			return []store.PayeeRow{{PayeeID: "n:x", PayeeName: "X", Currency: "GBP", FlowClass: str("income"), Category: str("k"),
				CategoryName: str("K"), Employer: &yes, FirstDate: "2030-01-01", LastDate: "2030-02-01", TxnCount: 3, OutMinor: 4, InMinor: 5,
				Keys: []string{"n:x", "mg:1"}, NameVariants: []string{"X", "x ltd"},
				Years: []store.PayeeYear{{Year: 2030, OutMinor: 4, InMinor: 5, TxnCount: 3}}}}, false, nil
		},
		cashFlow: func(store.CashFlowParams) (store.CashFlow, bool, error) {
			return store.CashFlow{
				Months: []store.MonthFlow{{Month: "2030-03", Scope: "joint", Currency: "GBP", IncomeMinor: 1, SalaryMinor: 2, RefundMinor: 3,
					SpendingMinor: 4, DebtRepaymentMinor: 5, InvestmentOutMinor: 6, InvestmentInMinor: 7, OwnExternalOutMinor: 8,
					OwnExternalInMinor: 9, ScopeTransferOutMinor: 10, ScopeTransferInMinor: 11, InternalOutMinor: 12, InternalInMinor: 13,
					NetMinor: 14, SavingsRate: &rate}},
				Categories: []store.CategoryMonth{{Month: "2030-03", Scope: "joint", Currency: "GBP", Category: str("k"), CategoryName: str("K"),
					SpendingMinor: 4, TxnCount: 2}},
				Upcoming: []store.UpcomingPayment{{Date: "2030-04-01", PayeeID: str("n:x"), PayeeName: "X", AmountMinor: 7, Currency: "GBP",
					Source: "inferred", SeriesKey: str("inf:1"), AccountSlug: str("a")}},
			}, true, nil
		},
		profile: func(store.FinanceProfileParams) (store.FinanceProfile, bool, error) {
			return store.FinanceProfile{
				Span:           store.ProfileSpan{FirstDate: str("2030-01-01"), LastDate: str("2030-06-01")},
				Accounts:       []store.ProfileAccount{{Slug: "a", Source: "monzo", Scope: "personal", ScopeSource: "default"}},
				MonthlyMedians: []store.ProfileMedian{{Scope: "personal", Currency: "GBP", IncomeMinor: 1, SalaryMinor: 2, SpendingMinor: 3, SavingsRate: &rate}},
				SalaryDay:      i64(25),
				Recurring:      []store.ProfileRecurring{{Evidence: "inferred", Currency: "GBP", Count: 2, AnnualCostMinor: 3}},
				TopPayees:      []store.ProfilePayee{{PayeeID: "n:x", PayeeName: "X", Currency: "GBP", SpendingMinor: 9}},
				TopSeries:      []store.ProfileSeries{{SeriesKey: "inf:1", PayeeName: "X", Currency: "GBP", AnnualCostMinor: 8}},
				Anomalies90d:   []store.ProfileAnomalyCount{{Kind: "possible_duplicate", Count: 2}},
				ToCorrect: store.ProfileToCorrect{UnnamedCategories: []string{"category_1"},
					UnclassifiedSpending:    []store.ProfileUnclassified{{Currency: "GBP", Count: 3, AmountMinor: 4}},
					UnclassifiedOBTransfers: 5, UnpairedCardCredits: 6, RuleProblems: 7},
			}, false, nil
		},
	}
	r := adapterRig(t, f)

	var rec RecurringPaymentsOutput
	decode(t, r.call(semconv.MCPToolRecurringPayments, map[string]any{"status": []string{"due"}, "include_ignored": true,
		"changed_only": true, "query": "x", "sort": "change_12m", "limit": 4}), &rec)
	if g := f.gotRecurring; strings.Join(g.Statuses, ",") != "due" || !g.IncludeIgnored || !g.ChangedOnly || g.Query != "x" ||
		g.Sort != "change_12m" || g.Limit != 4 {
		t.Errorf("store recurring params = %+v", g)
	}
	if !rec.Truncated || len(rec.Series) != 1 || len(rec.AnnualCostTotal) != 1 || rec.AnnualCostTotal[0].TotalMinor != 120 {
		t.Fatalf("recurring = %+v", rec)
	}
	if v := rec.Series[0]; v.SeriesKey != "inf:1" || v.Cadence != "monthly" || v.Occurrences != 6 || *v.AnnualCostMinor != 120 ||
		*v.NextExpectedDate != "2030-07-01" || len(v.PriceChanges) != 1 || v.PriceChanges[0].ToMinor != 10 || *v.Change12mMinor != 2 ||
		strings.Join(v.AccountSlugs, ",") != "a,b" || *v.OperatorState != "confirmed" || *v.CategoryName != "K" || v.Confidence != "medium" ||
		v.LastAmountMinor != 11 || v.TypicalAmountMinor != 10 || v.AmountKind != "variable" || v.Status != "due" {
		t.Errorf("series = %+v", v)
	}

	var pa PayeesOutput
	decode(t, r.call(semconv.MCPToolPayees, map[string]any{"query": "x", "flow_class": "income", "from": "2030-01-02", "to": "2030-03-04",
		"sort": "in", "limit": 6}), &pa)
	if g := f.gotPayees; g.Query != "x" || g.FlowClass != "income" || g.From != "2030-01-02" || g.To != "2030-03-04" || g.Sort != "in" || g.Limit != 6 {
		t.Errorf("store payees params = %+v", g)
	}
	if pa.Truncated || len(pa.Payees) != 1 {
		t.Fatalf("payees = %+v", pa)
	}
	if v := pa.Payees[0]; v.PayeeName != "X" || *v.FlowClass != "income" || !*v.Employer || v.TxnCount != 3 || v.OutMinor != 4 || v.InMinor != 5 ||
		strings.Join(v.Keys, ",") != "n:x,mg:1" || strings.Join(v.NameVariants, ",") != "X,x ltd" || len(v.Years) != 1 || v.Years[0].Year != 2030 ||
		*v.CategoryName != "K" || v.FirstDate != "2030-01-01" || v.LastDate != "2030-02-01" {
		t.Errorf("payee = %+v", v)
	}

	var cf CashFlowOutput
	decode(t, r.call(semconv.MCPToolCashFlow, map[string]any{"from_month": "2030-01", "to_month": "2030-03", "scopes": []string{"joint"},
		"category_limit": 3, "include_pending": true, "limit": 8}), &cf)
	if g := f.gotCashFlow; g.FromMonth != "2030-01" || g.ToMonth != "2030-03" || strings.Join(g.Scopes, ",") != "joint" || g.CategoryLimit != 3 ||
		!g.IncludePending || g.Limit != 8 {
		t.Errorf("store cash flow params = %+v", g)
	}
	if !cf.Truncated || len(cf.Months) != 1 || len(cf.Categories) != 1 || len(cf.Upcoming) != 1 {
		t.Fatalf("cash flow = %+v", cf)
	}
	if m := cf.Months[0]; m.Month != "2030-03" || m.SalaryMinor != 2 || m.InternalInMinor != 13 || m.NetMinor != 14 || *m.SavingsRate != 0.25 ||
		m.ScopeTransferOutMinor != 10 || m.OwnExternalInMinor != 9 || m.InvestmentOutMinor != 6 || m.DebtRepaymentMinor != 5 {
		t.Errorf("month = %+v", m)
	}
	if c := cf.Categories[0]; *c.Category != "k" || *c.CategoryName != "K" || c.TxnCount != 2 {
		t.Errorf("category = %+v", c)
	}
	if u := cf.Upcoming[0]; u.Date != "2030-04-01" || *u.PayeeID != "n:x" || u.AmountMinor != 7 || u.Source != "inferred" || *u.SeriesKey != "inf:1" || *u.AccountSlug != "a" {
		t.Errorf("upcoming = %+v", u)
	}

	var fp FinanceProfileOutput
	decode(t, r.call(semconv.MCPToolFinanceProfile, map[string]any{}), &fp)
	if f.gotProfile.Limit != store.CuratedDefaultLimit {
		t.Errorf("store profile params = %+v", f.gotProfile)
	}
	if fp.Truncated || *fp.Span.FirstDate != "2030-01-01" || *fp.Span.LastDate != "2030-06-01" || len(fp.Accounts) != 1 || fp.Accounts[0].ScopeSource != "default" ||
		len(fp.MonthlyMedians) != 1 || *fp.MonthlyMedians[0].SavingsRate != 0.25 || *fp.SalaryDay != 25 || fp.Recurring[0].Count != 2 ||
		fp.TopPayees[0].SpendingMinor != 9 || fp.TopSeries[0].AnnualCostMinor != 8 || fp.Anomalies90d[0].Count != 2 ||
		fp.ToCorrect.UnnamedCategories[0] != "category_1" || fp.ToCorrect.UnclassifiedSpending[0].AmountMinor != 4 ||
		fp.ToCorrect.UnclassifiedOBTransfers != 5 || fp.ToCorrect.UnpairedCardCredits != 6 || fp.ToCorrect.RuleProblems != 7 {
		t.Errorf("finance profile = %+v", fp)
	}

	// spending_summary passes the new inputs on.
	decode(t, r.call(semconv.MCPToolSpendingSummary, map[string]any{"from": "2030-01-02", "to": "2030-03-04", "group_by": "payee",
		"flow_classes": []string{"spending", "refund"}, "include_pending": true}), &SummaryOutput{})
	if g := f.gotSummary; strings.Join(g.FlowClasses, ",") != "spending,refund" || !g.IncludePending || g.GroupBy != "payee" {
		t.Errorf("store summary params = %+v", g)
	}
}

// The adapter passes the investment tools' parameters to the store and maps every field of their
// rows. All values are made up.
func TestAdapterMapsTheInvestmentTools(t *testing.T) {
	str := func(s string) *string { return &s }
	i64 := func(n int64) *int64 { return &n }
	f := &fakeStore{
		portfolio: func(store.PortfolioParams) ([]store.PortfolioAccount, bool, error) {
			return []store.PortfolioAccount{{AccountSlug: "fid-sipp-1234", Provider: "fidelity", AsOf: "2030-03-01",
				TotalMinor: 12345, CashMinor: 45, Currency: "GBP", ReconciliationState: "reconciled",
				Holdings: []store.PortfolioHolding{{Instrument: "Example Fund", Ticker: str("EXMP"), ISIN: str("GB00EXAMPLE1"),
					Units: str("10.5000"), Price: str("1.2500"), PriceUnit: str("GBP"), ValueMinor: i64(12300), Currency: "GBP",
					BookCostMinor: i64(10000), ExpectedUnits: str("10.5000"), UnitDifference: str("0.0000"), CheckedAsOf: str("2030-02-01")}},
			}}, true, nil
		},
		pension: func(store.PensionSummaryParams) (store.PensionSummary, bool, error) {
			return store.PensionSummary{
				TaxYears: []store.TaxYearFlows{{AccountSlug: "avi-pension-1111", TaxYear: "2030-31", Currency: "GBP", PaidInMinor: 100,
					TransfersInMinor: 2, WithdrawalsMinor: -3, AdjustmentsMinor: 4, SiblingTransfersMinor: -5, FeesMinor: -6, InterestMinor: 7}},
				Returns: []store.InvestmentReturn{{AccountSlug: "avi-pension-1111", IntervalKind: "statement", OpeningDate: str("2030-04-05"),
					ClosingDate: str("2031-04-05"), OpeningMinor: i64(10), ClosingMinor: i64(30), ValueChangeMinor: i64(20),
					NetFlowMinor: i64(5), ReturnMinor: i64(15), TaxYear: str("2030-31"), Currency: "GBP"}},
				StatementCharges: []store.StatementCharge{{AccountSlug: "avi-pension-1111", PeriodStart: "2030-04-06", PeriodEnd: "2031-04-05", Currency: "GBP"}},
				Missing:          []store.MissingData{{AccountSlug: "avi-pension-1111", Reason: "flows_unverified"}},
			}, false, nil
		},
	}
	r := adapterRig(t, f)

	var p PortfolioOutput
	decode(t, r.call(semconv.MCPToolPortfolio, map[string]any{"accounts": []string{"fid"}, "limit": 7}), &p)
	if len(f.gotPortfolio.Accounts) != 1 || f.gotPortfolio.Accounts[0] != "fid" || f.gotPortfolio.Limit != 7 {
		t.Errorf("portfolio params = %+v", f.gotPortfolio)
	}
	if !p.Truncated || len(p.Accounts) != 1 || p.Accounts[0].TotalMinor != 12345 || len(p.Accounts[0].Holdings) != 1 {
		t.Fatalf("portfolio = %+v", p)
	}
	if h := p.Accounts[0].Holdings[0]; h.Instrument != "Example Fund" || *h.Units != "10.5000" || *h.ValueMinor != 12300 ||
		*h.UnitDifference != "0.0000" || *h.CheckedAsOf != "2030-02-01" {
		t.Errorf("holding = %+v", h)
	}

	var s PensionSummaryOutput
	decode(t, r.call(semconv.MCPToolPensionSummary, map[string]any{"from_tax_year": "2030-31", "to_tax_year": "2031-32", "limit": 3}), &s)
	if f.gotPension.FromTaxYear != "2030-31" || f.gotPension.ToTaxYear != "2031-32" || f.gotPension.Limit != 3 {
		t.Errorf("pension params = %+v", f.gotPension)
	}
	if s.Truncated || len(s.TaxYears) != 1 || s.TaxYears[0].PaidInMinor != 100 || s.TaxYears[0].TransfersOutMinor != nil ||
		len(s.Returns) != 1 || *s.Returns[0].ReturnMinor != 15 || len(s.StatementCharges) != 1 ||
		s.StatementCharges[0].ChargesMinor != nil || len(s.Missing) != 1 || s.Missing[0].Reason != "flows_unverified" {
		t.Errorf("pension summary = %+v", s)
	}
}

func adapterRig(t *testing.T, f *fakeStore) *rig {
	t.Helper()
	return newRigWith(t, func(*rig) Reader { return adapter{f} }, nil)
}

// The outcome words of the store and of the telemetry names are one vocabulary: the adapter relies
// on store.Outcome, whose strings are the metric values.
func TestStoreOutcomesAreTheTelemetryOutcomes(t *testing.T) {
	for store, tel := range map[string]string{
		store.OutcomeOK: semconv.MCPOutcomeOK, store.OutcomeInvalid: semconv.MCPOutcomeInvalid,
		store.OutcomeBusy: semconv.MCPOutcomeBusy, store.OutcomeTimeout: semconv.MCPOutcomeTimeout,
		store.OutcomeRefused: semconv.MCPOutcomeRefused, store.OutcomeError: semconv.MCPOutcomeError,
	} {
		if store != tel {
			t.Errorf("store outcome %q is telemetry outcome %q", store, tel)
		}
	}
	if curatedMaxRows != store.CuratedMaxLimit || queryMaxRows != store.QueryMaxRows || defaultDeadline != store.ReaderDeadline {
		t.Error("the tools' limits are not the store's")
	}
	if !errors.Is(ErrBusy, store.ErrBusy) {
		t.Error("ErrBusy is not the store's")
	}
}

// Review 2: the wait for the connection ends with a deadline, and the call is busy, not a timeout,
// whether the error comes bare or wrapped by the store, through the real adapter.
func TestAdapterBusyWaitThatEndsInADeadlineIsBusy(t *testing.T) {
	for name, err := range map[string]error{
		"bare":                store.ErrBusy,
		"with the deadline":   fmt.Errorf("store: %w: %w", store.ErrBusy, context.DeadlineExceeded),
		"wrapped further":     fmt.Errorf("query: %w", fmt.Errorf("%w (waited 30s): %w", store.ErrBusy, context.DeadlineExceeded)),
		"the package's alias": ErrBusy,
		"alias with deadline": fmt.Errorf("%w: %w", ErrBusy, context.DeadlineExceeded),
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeStore{query: func(string, store.Limits) (store.Result, error) { return store.Result{}, err }}
			r := adapterRig(t, f)
			res := r.call(semconv.MCPToolQuerySQL, map[string]any{"sql": "SELECT 1"})
			if !res.IsError || !strings.Contains(resultText(res), "busy") {
				t.Errorf("result = %v %q", res.IsError, resultText(res))
			}
			span, _ := r.onlyCall(semconv.MCPToolQuerySQL, semconv.MCPOutcomeBusy, semconv.MCPClientUnknown)
			if span.Status.Description != semconv.MCPOutcomeBusy {
				t.Errorf("status %+v", span.Status)
			}
		})
	}
	// The same deadline without the busy wait is a timeout, so the two are told apart.
	f := &fakeStore{query: func(string, store.Limits) (store.Result, error) {
		return store.Result{}, fmt.Errorf("store: %w", context.DeadlineExceeded)
	}}
	r := adapterRig(t, f)
	r.call(semconv.MCPToolQuerySQL, map[string]any{"sql": "SELECT 1"})
	r.onlyCall(semconv.MCPToolQuerySQL, semconv.MCPOutcomeTimeout, semconv.MCPClientUnknown)
}

// Review 2: input the store rejects (its errors wrap store.ErrInvalid) is outcome invalid with the
// store's reason as the one line of text, minus its package prefix, for each curated tool.
func TestAdapterStoreInvalidInputIsInvalid(t *testing.T) {
	invalid := fmt.Errorf("%w: from is after to", store.ErrInvalid)
	f := &fakeStore{
		search:   func(store.SearchParams) ([]store.TransactionRow, bool, error) { return nil, false, invalid },
		summary:  func(store.SummaryParams) ([]store.SummaryGroup, bool, error) { return nil, false, invalid },
		describe: func(string) ([]store.Relation, error) { return nil, invalid },
		query:    func(string, store.Limits) (store.Result, error) { return store.Result{}, invalid },
		overview: func() (store.Overview, error) { return store.Overview{}, fmt.Errorf("store: overview: %w", invalid) },
	}
	for tool, args := range map[string]any{
		semconv.MCPToolSearchTransactions: map[string]any{},
		semconv.MCPToolSpendingSummary:    map[string]any{"from": "2030-01-01", "to": "2030-01-02", "group_by": "month"},
		semconv.MCPToolDescribeSchema:     map[string]any{},
		semconv.MCPToolQuerySQL:           map[string]any{"sql": "SELECT 1"},
		semconv.MCPToolOverview:           map[string]any{},
	} {
		t.Run(tool, func(t *testing.T) {
			r := adapterRig(t, f)
			res := r.call(tool, args)
			if !res.IsError || resultText(res) != "from is after to" {
				t.Errorf("result = %v %q, want the reason alone", res.IsError, resultText(res))
			}
			r.onlyCall(tool, semconv.MCPOutcomeInvalid, semconv.MCPClientUnknown)
		})
	}
}

// The adapter hands the store dates as YYYY-MM-DD strings and the row limit as Limit, and maps
// every field of every row type, in UTC.
func TestAdapterMapsTheStoresTypes(t *testing.T) {
	str := func(s string) *string { return &s }
	i64 := func(n int64) *int64 { return &n }
	days := 12
	at := time.Date(2030, 3, 4, 5, 6, 7, 0, time.FixedZone("plus-two", 2*3600))
	created := at
	f := &fakeStore{
		overview: func() (store.Overview, error) {
			return store.Overview{
				Accounts: []store.OverviewAccount{{
					AccountSlug: "uk_retail", Feed: "api", EarliestDate: str("2030-01-01"), LatestDate: str("2030-03-01"), TransactionCount: 7,
					ReconciliationState: "reconciled", ReconciledThrough: str("2030-03-01"), LastDiffMinor: i64(0),
					Gaps:                []store.AccountGap{{From: str("2030-02-01"), To: str("2030-02-02"), Reason: "r", DiffMinor: i64(5)}},
					LatestStatementDate: str("2030-03-01"), FailedOn: str("2030-02-02"), Source: "test", Type: "current", Name: "N",
					Currency: "GBP", Closed: true, EndedOn: str("2030-03-02"),
				}},
				Sources: []store.SyncStatusRow{
					{Source: "monzo", ConsentState: str("active"), ApprovedAt: &at, ExpectedExpiryAt: &at, DaysToExpiry: &days, LastSyncOKAt: &at,
						LastRunOutcome: str("ok"), LastImportAt: map[string]*time.Time{"stmt": &at, "none": nil}},
					{Source: "manual"},
				},
			}, nil
		},
		search: func(p store.SearchParams) ([]store.TransactionRow, bool, error) {
			return []store.TransactionRow{{ID: "id", AccountSlug: "a", TxnDate: "2030-03-01", CreatedAt: &created, AmountMinor: -5, Currency: "GBP",
				Status: "settled", Description: "d", Counterparty: str("c"), MerchantName: str("m"), Category: str("k"), Notes: str("n"),
				PotSlug: str("p"), Detail: map[string]any{"x": 1}}}, true, nil
		},
		summary: func(store.SummaryParams) ([]store.SummaryGroup, bool, error) {
			return []store.SummaryGroup{{Key: "k", Currency: "GBP", TotalMinor: 9, Count: 2}}, true, nil
		},
		describe: func(string) ([]store.Relation, error) {
			return []store.Relation{
				{Schema: "agent", Name: "v", Kind: store.KindView, Comment: "rules", Columns: []store.RelationColumn{{Name: "c", Type: "text", Nullable: true}}},
				{Schema: "raw", Name: "t", Kind: store.KindTable},
			}, nil
		},
		query: func(string, store.Limits) (store.Result, error) {
			return store.Result{Columns: []string{"a"}, Rows: [][]any{{1}}, Truncated: true, Elapsed: 3 * time.Millisecond}, nil
		},
	}
	r := adapterRig(t, f)

	var ov Overview
	decode(t, r.call(semconv.MCPToolOverview, map[string]any{}), &ov)
	a := ov.Accounts[0]
	if a.AccountSlug != "uk_retail" || a.Source != "test" || a.Feed != "api" || a.Type != "current" || a.Name != "N" || a.Currency != "GBP" || !a.Closed ||
		*a.EndedOn != "2030-03-02" || *a.EarliestDate != "2030-01-01" || *a.LatestDate != "2030-03-01" || a.TransactionCount != 7 ||
		a.ReconciliationState != "reconciled" || *a.ReconciledThrough != "2030-03-01" || *a.LastDiffMinor != 0 ||
		*a.LatestStatementDate != "2030-03-01" || *a.FailedOn != "2030-02-02" ||
		len(a.Gaps) != 1 || *a.Gaps[0].From != "2030-02-01" || *a.Gaps[0].To != "2030-02-02" || a.Gaps[0].Reason != "r" || *a.Gaps[0].DiffMinor != 5 {
		t.Errorf("account = %+v", a)
	}
	s0, s1 := ov.Sources[0], ov.Sources[1]
	const utc = "2030-03-04T03:06:07Z"
	if *s0.ApprovedAt != utc || *s0.ExpectedExpiryAt != utc || *s0.LastSyncOKAt != utc || *s0.DaysToExpiry != 12 || *s0.ConsentState != "active" ||
		*s0.LastRunOutcome != "ok" || s0.LastImportAt == nil || *(*s0.LastImportAt)["stmt"] != utc || (*s0.LastImportAt)["none"] != nil {
		t.Errorf("source = %+v", s0)
	}
	if s1.Source != "manual" || s1.ConsentState != nil || s1.LastImportAt != nil || s1.ApprovedAt != nil || s1.DaysToExpiry != nil {
		t.Errorf("a source with nothing = %+v", s1)
	}

	var se SearchOutput
	decode(t, r.call(semconv.MCPToolSearchTransactions, map[string]any{
		"accounts": []string{"a"}, "from": "2030-01-02", "to": "2030-03-04", "text": "t", "category": "k", "direction": "out",
		"min_amount": -10, "max_amount": -1, "include_detail": true, "limit": 7}), &se)
	tx := se.Transactions[0]
	if tx.ID != "id" || tx.TxnDate != "2030-03-01" || *tx.CreatedAt != utc || tx.AmountMinor != -5 || *tx.PotSlug != "p" || *tx.Notes != "n" ||
		*tx.Counterparty != "c" || *tx.MerchantName != "m" || *tx.Category != "k" || !se.Truncated || fmt.Sprint(tx.Detail) != "map[x:1]" {
		t.Errorf("transaction = %+v", tx)
	}
	if g := f.gotSearch; g.From != "2030-01-02" || g.To != "2030-03-04" || g.Limit != 7 || g.Direction != "out" || g.Text != "t" || g.Category != "k" ||
		!g.IncludeDetail || *g.MinAmount != -10 || *g.MaxAmount != -1 || len(g.Accounts) != 1 {
		t.Errorf("store search params = %+v", g)
	}

	var su SummaryOutput
	decode(t, r.call(semconv.MCPToolSpendingSummary, map[string]any{"from": "2030-01-02", "to": "2030-03-04", "group_by": "account", "accounts": []string{"a"}, "direction": "in", "exclude_pot_transfers": false, "limit": 9}), &su)
	if su.Groups[0] != (SummaryGroup{Key: "k", Currency: "GBP", TotalMinor: 9, Count: 2}) || !su.Truncated {
		t.Errorf("summary = %+v", su)
	}
	if g := f.gotSummary; g.From != "2030-01-02" || g.To != "2030-03-04" || g.GroupBy != "account" || g.Direction != "in" || g.Limit != 9 ||
		g.ExcludePotTransfers == nil || *g.ExcludePotTransfers {
		t.Errorf("store summary params = %+v", g)
	}
	r.call(semconv.MCPToolSpendingSummary, map[string]any{"from": "2030-01-02", "to": "2030-03-04", "group_by": "month"})
	if g := f.gotSummary; g.ExcludePotTransfers == nil || !*g.ExcludePotTransfers || g.Direction != "out" || g.Limit != store.CuratedDefaultLimit {
		t.Errorf("store summary defaults = %+v", g)
	}

	var de DescribeOutput
	decode(t, r.call(semconv.MCPToolDescribeSchema, map[string]any{}), &de)
	if len(de.Relations) != 2 || *de.Relations[0].Comment != "rules" || de.Relations[0].Columns[0].Name != "c" || !de.Relations[0].Columns[0].Nullable ||
		de.Relations[1].Comment != nil || de.Relations[1].Columns == nil {
		t.Errorf("relations = %+v", de.Relations)
	}

	var qu QueryOutput
	decode(t, r.call(semconv.MCPToolQuerySQL, map[string]any{"sql": "SELECT 1"}), &qu)
	if qu.ElapsedMS != 3 || !qu.Truncated || len(qu.Rows) != 1 {
		t.Errorf("query = %+v", qu)
	}
	if f.gotLimits != (store.Limits{MaxRows: store.QueryDefaultRows, MaxBytes: store.QueryMaxBytes}) {
		t.Errorf("store limits = %+v", f.gotLimits)
	}
}

// A source with no file-fed account has a null last_import_at, which the output schema allows (it
// failed validation as a plain map).
func TestOverviewSourceWithNullImportsPassesTheOutputSchema(t *testing.T) {
	f := populated()
	f.overview = func(context.Context) (Overview, error) {
		return Overview{Sources: []SyncSource{{Source: "monzo"}}}, nil
	}
	r := newRig(t, f)
	res := r.call(semconv.MCPToolOverview, map[string]any{})
	if res.IsError || !strings.Contains(resultText(res), `"last_import_at":null`) {
		t.Errorf("result = %v %q", res.IsError, resultText(res))
	}
	r.onlyCall(semconv.MCPToolOverview, semconv.MCPOutcomeOK, semconv.MCPClientUnknown)
}

// Review minor 1: a recovered panic logs one fixed line with the stack and never the panic value,
// beside the call's own line.
func TestARecoveredPanicIsLoggedWithItsStackAndNotItsValue(t *testing.T) {
	f := populated()
	f.overview = func(context.Context) (Overview, error) { panic("CANARY-PANIC-VALUE a row") }
	r := newRig(t, f)
	res := r.call(semconv.MCPToolOverview, map[string]any{})
	if !res.IsError {
		t.Fatal("not an error result")
	}
	if strings.Contains(r.logs.String(), "CANARY") || strings.Contains(resultText(res), "CANARY") {
		t.Errorf("the panic value was reported: %s", r.logs.String())
	}
	var panics []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(r.logs.String()), "\n") {
		if strings.Contains(line, `"msg":"mcp tool panic"`) {
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatal(err)
			}
			panics = append(panics, m)
		}
	}
	if len(panics) != 1 {
		t.Fatalf("%d panic lines in %s", len(panics), r.logs.String())
	}
	p := panics[0]
	stack, _ := p[semconv.LogStack].(string)
	if p["level"] != "ERROR" || p[semconv.LogTool] != semconv.MCPToolOverview || !strings.Contains(stack, "goroutine") ||
		!strings.Contains(stack, "mcpserver") {
		t.Errorf("panic line = %v", p)
	}
	r.onlyCall(semconv.MCPToolOverview, semconv.MCPOutcomeError, semconv.MCPClientUnknown)
}

// Review minor 2: mcp.protocol.version is set only from a value shaped like a date. Anything else
// a caller sends in Mcp-Protocol-Version is left off the span, so it cannot put text on a span.
func TestProtocolVersionAttributeOnlyTakesDates(t *testing.T) {
	for _, tt := range []struct {
		header string
		want   string
	}{
		{"2026-07-28", "2026-07-28"},
		{"2025-06-18", "2025-06-18"},
		{"", ""},
		{"CANARY-VERSION", ""},
		{"2026-07-28 ", ""},
		{"20260728", ""},
		{"2026-7-28", ""},
		{"2026-07-28\nX", ""},
		{"2026-07-28-extra", ""},
		{strings.Repeat("9", 5000), ""},
	} {
		t.Run(fmt.Sprintf("%.20q", tt.header), func(t *testing.T) {
			mem := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(mem))
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
			s := &server{tracer: tp.Tracer("test"), log: slog.New(slog.DiscardHandler), deadline: time.Second}
			next := func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
				callFrom(ctx).outcome = semconv.MCPOutcomeOK
				return &mcp.CallToolResult{}, nil
			}
			req := &mcp.CallToolRequest{
				Params: &mcp.CallToolParamsRaw{Name: semconv.MCPToolOverview},
				Extra:  &mcp.RequestExtra{Header: http.Header{"Mcp-Protocol-Version": {tt.header}}},
			}
			if _, err := s.observe(next)(context.Background(), "tools/call", req); err != nil {
				t.Fatal(err)
			}
			spans := mem.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("%d spans", len(spans))
			}
			got := attrMap(spans[0])[semconv.SpanMCPProtocolVersion]
			if got != tt.want {
				t.Errorf("mcp.protocol.version = %q, want %q", got, tt.want)
			}
			if strings.Contains(fmt.Sprint(spans[0].Attributes), "CANARY") {
				t.Error("the header's text reached the span")
			}
		})
	}
}

// Through the SDK's HTTP handler a bad Mcp-Protocol-Version either stops at the SDK or reaches the
// span without the attribute: in neither case does its text appear on a span.
func TestBadProtocolVersionHeaderOverHTTP(t *testing.T) {
	r := newRig(t, populated())
	ts := httptest.NewServer(Handler(r.srv))
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"overview","arguments":{}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "CANARY-VERSION")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	for _, s := range r.spans() {
		if strings.Contains(fmt.Sprint(s.Name, s.Attributes), "CANARY") {
			t.Errorf("the header's text reached span %q: %v", s.Name, s.Attributes)
		}
	}
	for _, s := range r.toolSpans() {
		if v, ok := attrMap(s)[semconv.SpanMCPProtocolVersion]; ok {
			t.Errorf("span carries mcp.protocol.version %q for a bad header", v)
		}
	}
}

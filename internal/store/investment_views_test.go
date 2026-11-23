package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// All values here are made up. The fake normalisers read a file that is the JSON of the result it
// should yield, so a test states the rows and the views are what is tested.
const (
	viewFidelityFormat = "fake-fidelity"
	viewAvivaFormat    = "fake-aviva"
)

type viewNormaliser struct{ provider string }

func (n viewNormaliser) Provider() string { return n.provider }

func (viewNormaliser) NormaliseInvestmentFile(f ledger.File, _ *time.Location) (ledger.InvestmentResult, error) {
	var r ledger.InvestmentResult
	if err := json.Unmarshal(f.Body, &r); err != nil {
		return r, errors.New("fake: unreadable")
	}
	return r, nil
}

type viewEnv struct {
	t      *testing.T
	app    *store.Store
	l      *ledger.Ledger
	clock  time.Time
	reader *store.Reader
}

func newViewEnv(t *testing.T) *viewEnv {
	t.Helper()
	ctx := context.Background()
	super := testpg.Start(t)
	if err := store.Bootstrap(ctx, super, "owner-secret", "app-secret"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := store.EnableMCPLogin(ctx, super, "mcp-secret"); err != nil {
		t.Fatalf("EnableMCPLogin: %v", err)
	}
	if err := store.Migrate(ctx, testpg.UserURL(t, super, store.RoleOwner), ledger.NormaliserRev); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	reader, err := store.OpenReader(ctx, testpg.UserURL(t, super, store.RoleMCP))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	t.Cleanup(reader.Close)
	return &viewEnv{t: t, app: openStore(t, testpg.UserURL(t, super, store.RoleApp)), reader: reader,
		clock: time.Date(2032, time.January, 1, 9, 0, 0, 0, time.UTC),
		l: ledger.New(nil, nil, time.UTC, nil, nil, ledger.WithInvestmentNormalisers(map[string]ledger.InvestmentNormaliser{
			viewFidelityFormat: viewNormaliser{ledger.ProviderFidelity},
			viewAvivaFormat:    viewNormaliser{ledger.ProviderAviva},
		}))}
}

func (e *viewEnv) tick() time.Time {
	e.clock = e.clock.Add(time.Minute)
	return e.clock
}

func (e *viewEnv) catchUp() ledger.Stats {
	e.t.Helper()
	var stats ledger.Stats
	err := e.app.WithWriterLock(context.Background(), store.HolderTest, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stats, err = e.l.CatchUp(ctx, tx, store.SchemaCore)
		return err
	})
	if err != nil {
		e.t.Fatalf("CatchUp: %v", err)
	}
	if stats.FilesRejected != 0 {
		e.t.Fatalf("a file was rejected: %v", stats.Rejections)
	}
	return stats
}

func (e *viewEnv) platform(slug, provider string) {
	e.t.Helper()
	p, err := json.Marshal(ledger.AccountAdd{Slug: slug, Type: "platform", Currency: "GBP", Name: "Made-up",
		ExpectsImports: true, Provider: provider})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := store.InsertOperatorEntry(context.Background(), e.app.DB(),
		store.OperatorEntry{Kind: ledger.EntryAccountAdd, Payload: p, EnteredAt: e.tick()}); err != nil {
		e.t.Fatal(err)
	}
	e.catchUp()
}

func (e *viewEnv) invest(slug, format string, r ledger.InvestmentResult) {
	e.t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, _, err := store.InsertImportFile(context.Background(), e.app.DB(), store.ImportFile{AccountSlug: slug, Format: format,
		FileName: "made-up", ImportedAt: e.tick(), Body: b}); err != nil {
		e.t.Fatal(err)
	}
	e.catchUp()
}

func viewDay(s string) time.Time {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return d
}

func viewEvent(acct, key, kind, order, settle string, flow, cash int64) ledger.InvestmentEventRec {
	b, _ := json.Marshal([]any{key, kind, order, settle, flow, cash})
	r := ledger.InvestmentEventRec{AccountKey: acct, SourceKey: key, EventType: kind, ProviderType: "Made-up", OrderDate: viewDay(order),
		GrossMinor: flow + cash, CashMinor: cash, FlowMinor: flow, Content: b, HashInput: b}
	if settle != "" {
		r.SettleDate = viewDay(settle)
	}
	return r
}

func viewStatement(acct, start, end string, opening, paidIn, paidOut, closing int64, charges *int64) ledger.PensionStatementRec {
	return ledger.PensionStatementRec{AccountKey: acct, PeriodStart: viewDay(start), PeriodEnd: viewDay(end), IssuedOn: viewDay(end).AddDate(0, 1, 0),
		OpeningMinor: opening, PaidInMinor: paidIn, PaidOutMinor: paidOut, GrowthMinor: closing - opening - paidIn + paidOut,
		ChargesMinor: charges, ClosingMinor: closing, Charges: json.RawMessage(`[]`), Projections: json.RawMessage(`{}`)}
}

func viewAccounts(product string, keys ...string) []ledger.InvestmentAccountRec {
	out := make([]ledger.InvestmentAccountRec, len(keys))
	for i, k := range keys {
		out[i] = ledger.InvestmentAccountRec{AccountKey: k, Product: product}
	}
	return out
}

// returnRow is one row of agent.investment_returns.
type returnRow struct {
	kind                     string
	opening, closing         *string
	openingMinor, closeMinor *int64
	change, netFlow, ret     *int64
	reason, taxYear          *string
}

func (e *viewEnv) returns(slug string) map[string]returnRow {
	e.t.Helper()
	rows, err := e.app.DB().Query(context.Background(), `SELECT interval_kind, opening_date::text, closing_date::text,
		opening_minor, closing_minor, value_change_minor, net_flow_minor, return_minor, null_reason, tax_year
		FROM agent.investment_returns WHERE account_slug = $1`, slug)
	if err != nil {
		e.t.Fatal(err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (returnRow, error) {
		var x returnRow
		err := r.Scan(&x.kind, &x.opening, &x.closing, &x.openingMinor, &x.closeMinor, &x.change, &x.netFlow, &x.ret, &x.reason, &x.taxYear)
		return x, err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	out := make(map[string]returnRow, len(list))
	for _, x := range list {
		if _, dup := out[x.kind]; dup {
			e.t.Fatalf("%s has two %s intervals", slug, x.kind)
		}
		out[x.kind] = x
	}
	return out
}

func wantNull(t *testing.T, name string, x returnRow, reason string) {
	t.Helper()
	if x.ret != nil || x.netFlow != nil || x.reason == nil || *x.reason != reason {
		t.Errorf("%s: return %v net flow %v reason %v, want a null return with %s", name, deref(x.ret), deref(x.netFlow), deref(x.reason), reason)
	}
}

func wantReturn(t *testing.T, name string, x returnRow, netFlow, ret int64) {
	t.Helper()
	if x.reason != nil || x.ret == nil || *x.ret != ret || x.netFlow == nil || *x.netFlow != netFlow {
		t.Errorf("%s: return %v net flow %v reason %v, want return %d with net flow %d", name, deref(x.ret), deref(x.netFlow), deref(x.reason), ret, netFlow)
	}
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// AC2: a proven interval has the right value, and each null reason appears where the design says.
// Growth is before charges on this Aviva statement, and the return is value change less flows, so
// the charge is counted once.
func TestInvestmentReturnsProvenAndEachNullReason(t *testing.T) {
	e := newViewEnv(t)
	e.platform("avi", ledger.ProviderAviva)
	e.platform("fid", ledger.ProviderFidelity)

	const planGood, planBad, planEmpty = "PLAN00001111", "PLAN00003333", "PLAN00002222"
	charge := int64(20000)
	e.invest("avi", viewAvivaFormat, ledger.InvestmentResult{Kind: ledger.InvestmentKindStatement,
		Accounts: viewAccounts(ledger.ProductPensionPlan, planGood, planBad), RowCount: 3,
		Statements: []ledger.PensionStatementRec{
			viewStatement(planGood, "2030-04-06", "2031-04-05", 1000000, 100000, 50000, 1200000, &charge),
			// Prints a paid-in figure no stored contribution supports.
			viewStatement(planBad, "2030-04-06", "2031-04-05", 400000, 5000, 0, 410000, &charge),
		},
		Events: []ledger.InvestmentEventRec{viewEvent(planGood, "OUT1", ledger.EventTransferOut, "2030-09-01", "", -50000, 0)}})
	captured := e.clock
	e.invest("avi", viewAvivaFormat, ledger.InvestmentResult{Kind: ledger.InvestmentKindCapture, CapturedAt: &captured,
		Accounts: viewAccounts(ledger.ProductPensionPlan, planGood, planEmpty), RowCount: 3,
		Valuations: []ledger.ValuationRec{
			{AccountKey: planGood, AsOf: viewDay("2031-06-01"), TotalMinor: 1260000},
			{AccountKey: planEmpty, AsOf: viewDay("2031-06-01"), TotalMinor: 0},
		},
		Events: []ledger.InvestmentEventRec{viewEvent(planGood, "IN1", ledger.EventContribution, "2030-06-01", "", 100000, 0)}})

	good := e.returns("avi-pension-1111")
	wantReturn(t, "aviva statement", good["statement"], 50000, 150000)
	if x := good["statement"]; deref(x.opening) != "2030-04-05" || deref(x.closing) != "2031-04-05" || deref(x.change) != int64(200000) || deref(x.taxYear) != "2030-31" {
		t.Errorf("aviva statement interval = %+v", x)
	}
	// The interval after the newest statement shows its value change and proves no return.
	since := good["since_statement"]
	wantNull(t, "after the statement", since, "flows_unverified")
	if deref(since.opening) != "2031-04-05" || deref(since.closing) != "2031-06-01" || deref(since.change) != int64(60000) || since.taxYear != nil {
		t.Errorf("since statement = %+v", since)
	}
	wantNull(t, "failed statement", e.returns("avi-pension-3333")["statement"], "flows_unverified")
	empty := e.returns("avi-pension-2222")["no_statement"]
	wantNull(t, "no statement", empty, "statement_missing")
	if empty.opening != nil || empty.openingMinor != nil || empty.change != nil || deref(empty.closeMinor) != int64(0) {
		t.Errorf("no statement = %+v, want no opening assumed and a closing value of 0", empty)
	}

	// A ledger provider: proven when reconciled through the closing date, flow_unsettled when a flow
	// straddles a boundary, flows_unverified when never checked.
	const sippGood, sippUnsettled, sippUnchecked = "SIPP00004444", "SIPP00005555", "SIPP00006666"
	e.invest("fid", viewFidelityFormat, ledger.InvestmentResult{Kind: ledger.InvestmentKindStatement,
		Accounts: viewAccounts(ledger.ProductSIPP, sippGood, sippUnsettled, sippUnchecked), RowCount: 3,
		Statements: []ledger.PensionStatementRec{
			viewStatement(sippGood, "2030-04-06", "2031-04-05", 500000, 10000, 0, 540000, nil),
			viewStatement(sippUnsettled, "2030-04-06", "2031-04-05", 500000, 1000, 0, 540000, nil),
			viewStatement(sippUnchecked, "2030-04-06", "2031-04-05", 500000, 0, 0, 540000, nil),
		}})
	summaryAt := e.clock
	e.invest("fid", viewFidelityFormat, ledger.InvestmentResult{Kind: ledger.InvestmentKindHoldings, CapturedAt: &summaryAt,
		Accounts: viewAccounts(ledger.ProductSIPP, sippGood, sippUnsettled), RowCount: 2,
		Valuations: []ledger.ValuationRec{
			{AccountKey: sippGood, AsOf: viewDay("2031-06-01"), TotalMinor: 550000},
			{AccountKey: sippUnsettled, AsOf: viewDay("2031-06-01"), TotalMinor: 550000},
		}})
	e.invest("fid", viewFidelityFormat, ledger.InvestmentResult{Kind: ledger.InvestmentKindLedger,
		Accounts: viewAccounts(ledger.ProductSIPP, sippGood, sippUnsettled), RowCount: 2,
		Events: []ledger.InvestmentEventRec{
			viewEvent(sippGood, "T1", ledger.EventTransferIn, "2030-08-01", "2030-08-03", 10000, 0),
			viewEvent(sippUnsettled, "T2", ledger.EventTransferIn, "2031-04-04", "2031-04-07", 1000, 0),
		}})

	proven := e.returns("fid-sipp-4444")
	wantReturn(t, "reconciled statement", proven["statement"], 10000, 30000)
	wantReturn(t, "reconciled since statement", proven["since_statement"], 0, 10000)
	wantNull(t, "unsettled at the closing date", e.returns("fid-sipp-5555")["statement"], "flow_unsettled")
	wantNull(t, "never checked", e.returns("fid-sipp-6666")["statement"], "flows_unverified")

	// A Fidelity statement's null charge stays null; an Aviva charge is the printed figure.
	var fidCharge, aviCharge *int64
	if err := e.app.DB().QueryRow(context.Background(), `SELECT
		(SELECT charges_minor FROM agent.pension_statements WHERE account_slug = 'fid-sipp-4444'),
		(SELECT charges_minor FROM agent.pension_statements WHERE account_slug = 'avi-pension-1111')`).Scan(&fidCharge, &aviCharge); err != nil {
		t.Fatal(err)
	}
	if fidCharge != nil || aviCharge == nil || *aviCharge != charge {
		t.Errorf("charges: fidelity %v aviva %v", deref(fidCharge), deref(aviCharge))
	}

	// pension_summary reads the same rules: the unknown charge is null, never 0, and the statement
	// interval's return reaches it.
	s, truncated, err := e.reader.PensionSummary(context.Background(), store.PensionSummaryParams{Accounts: []string{"fid-sipp-4444"}})
	if err != nil || truncated {
		t.Fatalf("PensionSummary: %v truncated %v", err, truncated)
	}
	if len(s.StatementCharges) != 1 || s.StatementCharges[0].ChargesMinor != nil {
		t.Errorf("statement charges = %+v", s.StatementCharges)
	}
	if len(s.Returns) != 2 || deref(s.Returns[0].ReturnMinor) != int64(30000) || deref(s.Returns[0].TaxYear) != "2030-31" {
		t.Errorf("returns = %+v", s.Returns)
	}
}

// AC3: sibling transfers cancel across an account's children, and a tax-year label appears only on
// an interval of exactly one tax year.
func TestSiblingTransfersCancelAndTaxYearLabels(t *testing.T) {
	e := newViewEnv(t)
	e.platform("fid", ledger.ProviderFidelity)
	const sipp, cash = "SIPP00007777", "CASH00008888"
	accounts := append(viewAccounts(ledger.ProductSIPP, sipp), viewAccounts(ledger.ProductCashManagement, cash)...)
	e.invest("fid", viewFidelityFormat, ledger.InvestmentResult{Kind: ledger.InvestmentKindStatement, Accounts: accounts, RowCount: 2,
		Statements: []ledger.PensionStatementRec{
			viewStatement(sipp, "2030-04-06", "2031-04-05", 10000, 0, 500, 12000, nil),
			// A calendar-year period is not a tax year, so it carries no label.
			viewStatement(cash, "2031-01-01", "2031-12-31", 1000, 500, 0, 1500, nil),
		}})
	capturedAt := e.clock
	e.invest("fid", viewFidelityFormat, ledger.InvestmentResult{Kind: ledger.InvestmentKindHoldings, CapturedAt: &capturedAt,
		Accounts: accounts, RowCount: 2,
		Valuations: []ledger.ValuationRec{
			{AccountKey: sipp, AsOf: viewDay("2032-01-15"), TotalMinor: 12500, CashMinor: 500},
			{AccountKey: cash, AsOf: viewDay("2032-01-15"), TotalMinor: 1500, CashMinor: 500},
		}})
	e.invest("fid", viewFidelityFormat, ledger.InvestmentResult{Kind: ledger.InvestmentKindLedger, Accounts: accounts, RowCount: 3,
		Events: []ledger.InvestmentEventRec{
			viewEvent(sipp, "I1", ledger.EventInterest, "2030-05-01", "", 0, 1000),
			viewEvent(sipp, "S1", ledger.EventSiblingTransfer, "2031-03-01", "", -500, -500),
			viewEvent(cash, "S2", ledger.EventSiblingTransfer, "2031-03-01", "", 500, 500),
		}})

	a, b := e.returns("fid-sipp-7777")["statement"], e.returns("fid-cash-8888")["statement"]
	wantReturn(t, "sipp", a, -500, 2500)
	wantReturn(t, "cash", b, 500, 0)
	if *a.netFlow+*b.netFlow != 0 {
		t.Errorf("sibling flows sum to %d across the children, want 0", *a.netFlow+*b.netFlow)
	}
	if deref(a.taxYear) != "2030-31" {
		t.Errorf("the tax-year interval is labelled %v", deref(a.taxYear))
	}
	if b.taxYear != nil {
		t.Errorf("a calendar-year interval is labelled %v", *b.taxYear)
	}

	s, _, err := e.reader.PensionSummary(context.Background(), store.PensionSummaryParams{Accounts: []string{"fid"}, FromTaxYear: "2030-31", ToTaxYear: "2030-31"})
	if err != nil {
		t.Fatal(err)
	}
	var sibling int64
	for _, y := range s.TaxYears {
		if y.TaxYear != "2030-31" {
			t.Errorf("a row outside the range: %+v", y)
		}
		sibling += y.SiblingTransfersMinor
	}
	if len(s.TaxYears) != 2 || sibling != 0 {
		t.Errorf("tax years = %+v, sibling total %d", s.TaxYears, sibling)
	}

	// limit caps each of the four arrays and says so; a limit that fits them cuts nothing.
	cut, truncated, err := e.reader.PensionSummary(context.Background(), store.PensionSummaryParams{Accounts: []string{"fid"}, Limit: 1})
	if err != nil || !truncated || len(cut.TaxYears) != 1 || len(cut.Returns) != 1 || len(cut.StatementCharges) != 1 || len(cut.Missing) > 1 {
		t.Errorf("limit 1: %+v truncated %v err %v", cut, truncated, err)
	}
	if _, truncated, err := e.reader.PensionSummary(context.Background(), store.PensionSummaryParams{Accounts: []string{"fid"}, Limit: 100}); err != nil || truncated {
		t.Errorf("limit 100: truncated %v err %v", truncated, err)
	}
}

// AC1: each view reads its rows and states its rules. A note joins its event through the child, a
// zero-value child appears, and a position missing from the capture is shown from the last check.
func TestInvestmentViewsAndPortfolio(t *testing.T) {
	e := newViewEnv(t)
	ctx := context.Background()
	e.platform("fid", ledger.ProviderFidelity)
	const held, zero = "SIPP00001234", "SIPP00009999"
	accounts := viewAccounts(ledger.ProductSIPP, held, zero)
	buy := viewEvent(held, "B1", ledger.EventBuy, "2031-01-10", "2031-01-12", 0, 0)
	buy.InstrumentKey, buy.InstrumentName, buy.Units, buy.GrossMinor = "EXMP", "Example Fund", "10.5000", 1000
	gone := viewEvent(held, "B2", ledger.EventBuy, "2031-01-11", "2031-01-13", 0, 0)
	gone.InstrumentKey, gone.InstrumentName, gone.Units = "GONE", "Gone Fund", "2.0000"
	e.invest("fid", viewFidelityFormat, ledger.InvestmentResult{Kind: ledger.InvestmentKindLedger, Accounts: accounts, RowCount: 2,
		Events: []ledger.InvestmentEventRec{buy, gone}})
	e.invest("fid", viewFidelityFormat, ledger.InvestmentResult{Kind: ledger.InvestmentKindTradeNote, RowCount: 2,
		TradeNotes: []ledger.TradeNoteRec{
			{AccountKey: held, Reference: "B1", ISIN: "GB00EXAMPLE1", ExecutedAt: e.clock, Units: "10.5000", ExactPrice: "0.95238",
				GrossMinor: 1000, NetMinor: 1000, SettleDate: viewDay("2031-01-12")},
			// A note whose number matches no child stays stored and joins nothing.
			{AccountKey: "NOCHILD7777", Reference: "B1", ISIN: "GB00EXAMPLE2", ExecutedAt: e.clock, Units: "1", ExactPrice: "1",
				SettleDate: viewDay("2031-01-12")},
		}})
	first := e.clock
	e.invest("fid", viewFidelityFormat, ledger.InvestmentResult{Kind: ledger.InvestmentKindHoldings, CapturedAt: &first,
		Accounts: accounts, RowCount: 3,
		Valuations: []ledger.ValuationRec{
			{AccountKey: held, AsOf: viewDay("2031-02-01"), TotalMinor: 90000},
			{AccountKey: zero, AsOf: viewDay("2031-02-01"), TotalMinor: 0},
		},
		Holdings: []ledger.HoldingRec{{AccountKey: held, InstrumentKey: "EXMP", InstrumentName: "Example Fund", AsOf: viewDay("2031-02-01"),
			Units: "10.5000", Price: "1.25", PriceUnit: ledger.PriceUnitGBP, ValueMinor: 90000}}})
	// A later ledger file makes that capture checkable; GONE is expected and not in the capture.
	e.invest("fid", viewFidelityFormat, ledger.InvestmentResult{Kind: ledger.InvestmentKindLedger, RowCount: 0})

	var n int
	var isin *string
	if err := e.app.DB().QueryRow(ctx, `SELECT count(*)::int, max(isin) FROM agent.investment_events
		WHERE account_slug = 'fid-sipp-1234' AND isin IS NOT NULL`).Scan(&n, &isin); err != nil {
		t.Fatal(err)
	}
	if n != 1 || deref(isin) != "GB00EXAMPLE1" {
		t.Errorf("trade note join: %d rows, isin %v", n, deref(isin))
	}
	if err := e.app.DB().QueryRow(ctx, `SELECT count(*)::int FROM agent.valuations WHERE account_slug = 'fid-sipp-9999' AND total_minor = 0`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the zero-value child has %d valuations", n)
	}

	// A platform slug expands to its children, and a child with a value of 0 and no holdings appears.
	got, truncated, err := e.reader.Portfolio(ctx, store.PortfolioParams{Accounts: []string{"fid"}})
	if err != nil || truncated {
		t.Fatalf("Portfolio: %v truncated %v", err, truncated)
	}
	if len(got) != 2 || got[0].AccountSlug != "fid-sipp-1234" || got[1].AccountSlug != "fid-sipp-9999" {
		t.Fatalf("accounts = %+v", got)
	}
	if got[1].TotalMinor != 0 || got[1].Holdings == nil || len(got[1].Holdings) != 0 || got[0].Provider != "fidelity" {
		t.Errorf("accounts = %+v", got)
	}
	if len(got[0].Holdings) != 2 {
		t.Fatalf("holdings = %+v, want the held position and the one only the check knows", got[0].Holdings)
	}
	h, missing := got[0].Holdings[0], got[0].Holdings[1]
	if h.Instrument != "Example Fund" || deref(h.Units) != "10.5000" || deref(h.ISIN) != "GB00EXAMPLE1" || deref(h.ValueMinor) != int64(90000) {
		t.Errorf("holding = %+v", h)
	}
	if missing.Units != nil || missing.ValueMinor != nil || deref(missing.ExpectedUnits) != "2.0000" ||
		deref(missing.UnitDifference) != "-2.0000" || deref(missing.CheckedAsOf) != "2031-02-01" {
		t.Errorf("position missing from the capture = %+v", missing)
	}

	// limit caps each array and says so.
	if cut, truncated, err := e.reader.Portfolio(ctx, store.PortfolioParams{Accounts: []string{"fid"}, Limit: 1}); err != nil || !truncated ||
		len(cut) != 1 || len(cut[0].Holdings) != 1 {
		t.Errorf("limit 1: %+v truncated %v err %v", cut, truncated, err)
	}
	// An exact limit cuts nothing.
	if _, truncated, err := e.reader.Portfolio(ctx, store.PortfolioParams{Accounts: []string{"fid-sipp-1234"}, Limit: 2}); err != nil || truncated {
		t.Errorf("limit 2 of one account and two holdings: truncated %v err %v", truncated, err)
	}
	// A slug with no data is an empty list, not an error.
	if none, _, err := e.reader.Portfolio(ctx, store.PortfolioParams{Accounts: []string{"no-such-account"}}); err != nil || none == nil || len(none) != 0 {
		t.Errorf("no data: %+v %v", none, err)
	}
	s, _, err := e.reader.PensionSummary(ctx, store.PensionSummaryParams{Accounts: []string{"no-such-account"}})
	if err != nil || s.TaxYears == nil || len(s.TaxYears)+len(s.Returns)+len(s.StatementCharges)+len(s.Missing) != 0 {
		t.Errorf("no data: %+v %v", s, err)
	}
	if _, _, err := e.reader.PensionSummary(ctx, store.PensionSummaryParams{FromTaxYear: "2030-32"}); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("a bad tax year: %v", err)
	}

	for _, view := range []string{"holdings", "investment_events", "valuations", "pension_statements", "investment_returns"} {
		var comment *string
		if err := e.app.DB().QueryRow(ctx, `SELECT obj_description(('agent.' || $1)::regclass, 'pg_class')`, view).Scan(&comment); err != nil {
			t.Fatal(err)
		}
		if comment == nil || len(*comment) < 100 {
			t.Errorf("agent.%s has no rule comment", view)
		}
	}
}

// The MCP role reads the new views, and the shared byte budget cuts a result that outgrows it.
func TestPortfolioSharesTheByteBudget(t *testing.T) {
	e := newViewEnv(t)
	e.platform("fid", ledger.ProviderFidelity)
	const key = "SIPP00001234"
	at := e.clock
	r := ledger.InvestmentResult{Kind: ledger.InvestmentKindHoldings, CapturedAt: &at, Accounts: viewAccounts(ledger.ProductSIPP, key),
		Valuations: []ledger.ValuationRec{{AccountKey: key, AsOf: viewDay("2031-02-01"), TotalMinor: 1000}}}
	name := strings.Repeat("n", 2000)
	for i := range 400 {
		r.Holdings = append(r.Holdings, ledger.HoldingRec{AccountKey: key, InstrumentKey: fmt.Sprintf("T%04d", i),
			InstrumentName: name, AsOf: viewDay("2031-02-01"), Units: "1", Price: "1", PriceUnit: ledger.PriceUnitGBP, ValueMinor: 1})
	}
	r.RowCount = len(r.Holdings) + 1
	e.invest("fid", viewFidelityFormat, r)

	got, truncated, err := e.reader.Portfolio(context.Background(), store.PortfolioParams{Limit: store.CuratedMaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(got) != 1 || len(got[0].Holdings) == 0 || len(got[0].Holdings) >= 400 {
		t.Fatalf("truncated %v, %d accounts", truncated, len(got))
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > store.CuratedMaxBytes {
		t.Errorf("result is %d bytes, over the %d budget", len(b), store.CuratedMaxBytes)
	}
}

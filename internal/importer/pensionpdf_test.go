package importer_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/importer"
	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/pdfdoc/pdfdoctest"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// Every value here is made up, and every fixture is built by hand from the documented shape of the
// format. The name, address and employer strings exist so a test can show they are never read.

const (
	ppdfPlan = "PX00001234"
	ppdfRef  = "AP00005678"
)

// ppdfPersonal are the printed personal strings no record may ever hold.
var ppdfPersonal = []string{"MR A EXAMPLE", "1 EXAMPLE ROAD", "EXAMPLE TOWN", "EX1 1AA", "EXAMPLE EMPLOYER"}

// ppdfGBP prints pence as a statement does, in the single-byte encoding of the fixture font.
func ppdfGBP(n int64) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	whole := strconv.FormatInt(n/100, 10)
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	return fmt.Sprintf("%s\xa3%s.%02d", sign, whole, n%100)
}

func ppdfBuild(pages ...[]string) []byte {
	contents := make([][]byte, len(pages))
	for i, p := range pages {
		contents[i] = []byte(strings.Join(p, ""))
	}
	return pdfdoctest.Build(contents, pdfdoctest.Options{})
}

func ppdfDate(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d
}

// ppdfWantRefused checks that a file was refused in fixed words, with nothing from the file in them.
func ppdfWantRefused(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, importer.ErrFormat) {
		t.Fatalf("got %v, want ErrFormat", err)
	}
	for _, token := range append([]string{ppdfPlan, ppdfRef, "EXAMPLE", "\xa3", "£"}, ppdfPersonal...) {
		if strings.Contains(err.Error(), token) {
			t.Fatalf("the error carries file content: %q", err.Error())
		}
	}
}

// ---- aviva-annual-pdf fixture ----

type ppdfAvivaPay struct {
	date, kind string

	pence int64
}

type ppdfAvivaDoc struct {
	plan, start, end, issued, growthWord, assumptions string

	opening, growth, charges, paidIn, paidOut, closing, outTotalDelta int64

	employer, out []ppdfAvivaPay
}

func ppdfAvivaBase(plan string) *ppdfAvivaDoc {
	d := &ppdfAvivaDoc{plan: plan, start: "6 April 2029", end: "5 April 2030", issued: "5 May 2030", growthWord: "up",
		opening: 1000000, growth: 50000, charges: 12345,
		employer: []ppdfAvivaPay{{"05/05/29", "Regular", 25000}, {"05/06/29", "Regular", 25000}},
		out:      []ppdfAvivaPay{{"13/09/29", "Transfer payment out", 10000}}}
	d.fill()
	return d
}

// fill makes the summary figures agree with the rows.
func (d *ppdfAvivaDoc) fill() {
	d.paidIn, d.paidOut = 0, 0
	for _, p := range d.employer {
		d.paidIn += p.pence
	}
	for _, p := range d.out {
		d.paidOut += p.pence
	}
	d.closing = d.opening + d.paidIn + d.growth - d.paidOut - d.charges
	d.assumptions = "To work these values out on " + d.issued + " we\x92ve assumed:"
}

func (d *ppdfAvivaDoc) pdf() []byte {
	t := pdfdoctest.Text
	header := func() []string {
		return []string{t(440, 770, 9, "Statement for"), t(430, 758, 9, d.start+" to "+d.end), t(440, 744, 9, "Plan number"),
			t(470, 732, 9, d.plan), t(440, 718, 9, "Your scheme is"), t(440, 706, 9, "EXAMPLE EMPLOYER LTD"), t(470, 694, 9, "SC123456"),
			t(440, 680, 9, "Plan holder"), t(440, 668, 9, "MR A EXAMPLE"), t(440, 654, 9, "Plan start date"), t(470, 642, 9, d.start)}
	}
	cover := append(header(), t(71, 620, 10, "MR A EXAMPLE"), t(71, 608, 10, "1 EXAMPLE ROAD"), t(71, 596, 10, "EXAMPLE TOWN"),
		t(71, 584, 10, "EX1 1AA"), t(71, 572, 6, "000000000"), t(71, 50, 6, "Made-up footer code"))
	letter := []string{t(71, 700, 11, "Thank you for saving with us.")}

	const heading = "How much money you already have in your pension plan"
	growth := d.growth
	if growth < 0 {
		growth = -growth
	}
	summary := append(header(), t(71, 625, 11, heading), t(71, 605, 14, heading),
		t(118, 580, 14, ppdfGBP(d.opening)), t(232, 580, 14, ppdfGBP(d.paidIn)), t(360, 580, 14, ppdfGBP(d.paidOut)), t(488, 580, 14, ppdfGBP(d.closing)),
		t(110, 560, 9, "Value of your pension"), t(225, 560, 9, "Money paid in"), t(350, 560, 9, "Money paid out"), t(480, 560, 9, "Value of your pension"),
		t(110, 548, 9, "plan on"), t(225, 548, 9, "between"), t(350, 548, 9, "between"), t(480, 548, 9, "plan on"),
		t(71, 512, 11, "Between "+d.start+" and "+d.end),
		t(71, 492, 11, "You have not paid any money into your pension plan"),
		t(71, 472, 11, "Your employer has added"), t(488, 469, 14, ppdfGBP(d.paidIn)),
		t(71, 452, 11, "Your investments have gone "+d.growthWord+" in value by (before charges)"), t(488, 449, 14, ppdfGBP(growth)),
		t(71, 432, 11, "Payments out of your pension plan"), t(488, 429, 14, ppdfGBP(d.paidOut)),
		t(71, 412, 11, "Costs and charges have been deducted from your pension plan"), t(488, 409, 14, ppdfGBP(d.charges)),
		t(71, 392, 11, "If you asked to transfer your money to another pension scheme"), t(488, 389, 14, ppdfGBP(d.closing)),
		t(71, 372, 11, "on "+d.end+", we would have transferred"))

	forecast := []string{t(71, 720, 20, "Your pension statement continued"), t(71, 690, 11, "How much money you could have at 65")}
	if d.assumptions != "" {
		forecast = append(forecast, t(71, 660, 11, d.assumptions))
	}
	funds := []string{t(74, 770, 9, "Plan number      "+d.plan), t(74, 758, 9, "Plan holder      MR A EXAMPLE"), t(74, 746, 9, "As at      "+d.end),
		t(74, 720, 14, "Your investment choices"), t(74, 700, 10, "Fund name"), t(268, 700, 10, "Fund value"),
		t(74, 685, 10, "Example Growth Fund"), t(279, 685, 10, ppdfGBP(d.closing)), t(74, 670, 10, "Total fund value"), t(279, 670, 10, ppdfGBP(d.closing))}

	payments := []string{t(71, 770, 14, "More info about your payments in the last year")}
	y := 740.0
	table := func(title string, typeX, amountX, totalX float64, rows []ppdfAvivaPay, delta int64) {
		payments = append(payments, t(74, y, 11, title))
		y -= 15
		payments = append(payments, t(74, y, 10, "When"), t(typeX, y, 10, "Payment"), t(amountX, y, 10, "Amount"))
		y -= 15
		var sum int64
		for _, p := range rows {
			payments = append(payments, t(74, y, 10, p.date), t(typeX, y, 10, p.kind), t(amountX, y, 10, ppdfGBP(p.pence)))
			sum += p.pence
			y -= 15
		}
		payments = append(payments, t(74, y, 11, "Total"), t(totalX, y, 11, ppdfGBP(sum+delta)))
		y -= 30
	}
	table("Employer Payments", 130, 232, 224, d.employer, 0)
	table("Payments Out", 136, 269, 261, d.out, d.outTotalDelta)

	charges := []string{t(71, 770, 14, "Charges"), t(71, 750, 11, "Your plan costs and charges were "+ppdfGBP(d.charges)+".")}
	return ppdfBuild(cover, letter, summary, forecast, funds, payments, charges)
}

func ppdfAvivaNormalise(body []byte) (ledger.InvestmentResult, error) {
	return importer.AvivaAnnualNormaliser{}.NormaliseInvestmentFile(
		ledger.File{Format: string(importer.FormatAvivaAnnualPDF), Body: body}, time.UTC)
}

// AC1: a statement parses into its period, figures, charges and dated payments out.
func TestPensionPDFAvivaParsesAStatement(t *testing.T) {
	res, err := ppdfAvivaNormalise(ppdfAvivaBase(ppdfPlan).pdf())
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != ledger.InvestmentKindStatement || res.RowCount != 2 || len(res.Accounts) != 1 ||
		res.Accounts[0] != (ledger.InvestmentAccountRec{AccountKey: ppdfPlan, Product: ledger.ProductPensionPlan}) {
		t.Fatalf("result %+v", res)
	}
	if len(res.Statements) != 1 {
		t.Fatalf("%d statements", len(res.Statements))
	}
	s := res.Statements[0]
	if s.AccountKey != ppdfPlan || !s.PeriodStart.Equal(ppdfDate("2029-04-06")) || !s.PeriodEnd.Equal(ppdfDate("2030-04-05")) ||
		!s.IssuedOn.Equal(ppdfDate("2030-05-05")) {
		t.Fatalf("statement dates %+v", s)
	}
	if s.OpeningMinor != 1000000 || s.PaidInMinor != 50000 || s.PaidOutMinor != 10000 || s.GrowthMinor != 50000 ||
		s.ChargesMinor == nil || *s.ChargesMinor != 12345 || s.ClosingMinor != 1077655 {
		t.Fatalf("statement figures %+v", s)
	}
	if string(s.Charges) != `{"total_minor":12345}` || string(s.Projections) != `{}` {
		t.Fatalf("charges %s, projections %s", s.Charges, s.Projections)
	}
	if len(res.Events) != 1 {
		t.Fatalf("%d events", len(res.Events))
	}
	e := res.Events[0]
	if e.AccountKey != ppdfPlan || e.EventType != ledger.EventTransferOut || e.ProviderType != "Transfer payment out" ||
		!e.OrderDate.Equal(ppdfDate("2029-09-13")) || e.GrossMinor != -10000 || e.FlowMinor != -10000 || e.CashMinor != 0 ||
		e.Units != "" || e.SourceKey == "" || !json.Valid(e.Content) || len(e.HashInput) == 0 {
		t.Fatalf("event %+v", e)
	}
}

// A fall in value, an apostrophe that decodes another way and a second identical payment all read.
func TestPensionPDFAvivaReadsItsVariations(t *testing.T) {
	d := ppdfAvivaBase(ppdfPlan)
	d.growthWord, d.growth = "down", -2500
	d.out = append(d.out, d.out[0])
	d.fill()
	d.assumptions = "To work these values out on " + d.issued + " we've assumed:"
	res, err := ppdfAvivaNormalise(d.pdf())
	if err != nil {
		t.Fatal(err)
	}
	if s := res.Statements[0]; s.GrowthMinor != -2500 || s.PaidOutMinor != 20000 {
		t.Fatalf("statement %+v", s)
	}
	if len(res.Events) != 2 || res.Events[0].SourceKey == res.Events[1].SourceKey ||
		strings.TrimSuffix(res.Events[0].SourceKey, "#1")+"#2" != res.Events[1].SourceKey {
		t.Fatalf("identical rows keyed %q, %q", res.Events[0].SourceKey, res.Events[1].SourceKey)
	}
}

// AC1: a file whose arithmetic or payments out do not add up is refused.
func TestPensionPDFAvivaRejects(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(d *ppdfAvivaDoc)
	}{
		{"closing off by a penny", func(d *ppdfAvivaDoc) { d.closing++ }},
		{"growth off by a penny", func(d *ppdfAvivaDoc) { d.growth++ }},
		{"charges off by a penny", func(d *ppdfAvivaDoc) { d.charges++ }},
		{"payments out total differs from its rows", func(d *ppdfAvivaDoc) { d.outTotalDelta = 1 }},
		{"payments out differ from the figure paid out", func(d *ppdfAvivaDoc) { d.out[0].pence -= 100 }},
		{"payments in differ from the figure paid in", func(d *ppdfAvivaDoc) { d.employer[0].pence -= 100 }},
		{"no issue date", func(d *ppdfAvivaDoc) { d.assumptions = "" }},
		{"growth wording unknown", func(d *ppdfAvivaDoc) { d.growthWord = "sideways" }},
		{"period backwards", func(d *ppdfAvivaDoc) { d.start, d.end = d.end, d.start }},
		{"plan number of another shape", func(d *ppdfAvivaDoc) { d.plan = "PX1" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := ppdfAvivaBase(ppdfPlan)
			c.edit(d)
			_, err := ppdfAvivaNormalise(d.pdf())
			ppdfWantRefused(t, err)
		})
	}
	if _, err := ppdfAvivaNormalise([]byte("not a pdf")); !errors.Is(err, importer.ErrFormat) {
		t.Fatalf("not a pdf: %v", err)
	}
	if _, err := (importer.AvivaAnnualNormaliser{}).NormaliseInvestmentFile(ledger.File{Format: "halifax-pdf",
		Body: ppdfAvivaBase(ppdfPlan).pdf()}, time.UTC); !errors.Is(err, importer.ErrFormat) {
		t.Fatalf("another format: %v", err)
	}
}

// ---- fidelity-annual-pdf fixture ----

type ppdfFidelityDoc struct {
	ref, footerRef, end, issued, extraLabel string

	opening, moneyIn, moneyOut, perf, closing, tablePerfDelta int64

	service, dealing, entry, exit, ongoing, transaction, perfFees, paidDelta int64
}

func ppdfFidelityBase() *ppdfFidelityDoc {
	d := &ppdfFidelityDoc{ref: ppdfRef, footerRef: ppdfRef, end: "5 April 2030", issued: "5 May 2030", moneyIn: 1000000, perf: -12345,
		service: 1200, dealing: 750, entry: 0, exit: 0, ongoing: 2100, transaction: 300, perfFees: 50}
	d.fill()
	return d
}

func (d *ppdfFidelityDoc) fill() { d.closing = d.opening + d.moneyIn - d.moneyOut + d.perf }

func (d *ppdfFidelityDoc) pdf() []byte {
	t := pdfdoctest.Text
	footer := func(n int) []string {
		return []string{t(54, 65, 8, "If you want to speak to us please quote:"), t(54, 55, 8, d.footerRef), t(54, 25, 8, "Page | "+strconv.Itoa(n))}
	}
	letter := []string{t(391, 770, 8, "How to get in touch"), t(391, 758, 8, "Monday to Friday"),
		t(391, 746, 8, "You\x92ll need this reference if you call:"), t(391, 734, 8, d.ref),
		t(72, 720, 7, "ZZ MAIL 1"), t(72, 700, 10, "MR A EXAMPLE"), t(72, 688, 10, "1 EXAMPLE ROAD"), t(72, 676, 10, "EXAMPLE TOWN"), t(72, 664, 10, "EX1 1AA"),
		t(72, 640, 10, d.issued), t(72, 620, 14, "Your annual pension benefit statement"),
		t(72, 600, 9, "Please disregard the previous statement"), t(72, 580, 10, "Dear Mr Example")}

	magnitude := d.perf
	if magnitude < 0 {
		magnitude = -magnitude
	}
	operator := "+"
	if d.perf < 0 {
		operator = "-"
	}
	statement := []string{t(54, 770, 10, "Pension savings annual statement for MR A EXAMPLE"),
		t(50, 755, 10, "Your Address"), t(236, 755, 10, "1 EXAMPLE ROAD"), t(236, 742, 10, "EXAMPLE TOWN"), t(236, 729, 10, "EX1 1AA"),
		t(54, 705, 14, "Your pension savings annual statement for the year to "+d.end),
		t(54, 685, 14, "How much money there is in your pension savings account(s)")}
	if d.moneyOut > 0 {
		statement = append(statement, t(90, 650, 9.5, ppdfGBP(d.opening)), t(180, 650, 9.5, ppdfGBP(d.moneyIn)), t(270, 650, 9.5, ppdfGBP(d.moneyOut)),
			t(360, 650, 9.5, ppdfGBP(magnitude+d.tablePerfDelta)), t(452, 650, 9.5, ppdfGBP(d.closing)))
	} else {
		// The first operator sits close enough to the opening amount for the reader to join them.
		statement = append(statement, t(108, 650, 9.5, ppdfGBP(d.opening)), t(135, 650, 32, "+"), t(214, 650, 9.5, ppdfGBP(d.moneyIn)),
			t(300, 650, 32, operator), t(338, 650, 9.5, ppdfGBP(magnitude+d.tablePerfDelta)), t(415, 650, 32, "="), t(452, 650, 9.5, ppdfGBP(d.closing)))
	}
	statement = append(statement, t(96, 620, 8, "money in your"), t(214, 620, 8, "money transferred"), t(338, 620, 8, "investment"), t(452, 620, 8, "money in your"),
		t(54, 590, 14, "What happened during the year"),
		t(132, 570, 8, "Amount you transferred to Fidelity"), t(425, 570, 8, ppdfGBP(d.moneyIn)),
		t(132, 555, 8, "Investment performance after charges"), t(425, 555, 8, ppdfGBP(d.perf)),
		t(132, 540, 8, "Money in your savings account(s) on "+d.end), t(425, 540, 8, ppdfGBP(d.closing)))
	if d.moneyOut > 0 {
		statement = append(statement, t(132, 525, 8, "Money out during the year"), t(425, 525, 8, ppdfGBP(d.moneyOut)))
	}
	if d.extraLabel != "" {
		statement = append(statement, t(132, 510, 8, d.extraLabel), t(425, 510, 8, ppdfGBP(100)))
	}
	// Body text below the table can start with an amount, and is not a row of the table.
	statement = append(statement, t(50, 480, 10, ppdfGBP(d.closing)+" plus any outstanding tax relief is the amount"))
	statement = append(statement, footer(1)...)

	holdings := append([]string{t(54, 770, 14, "Summary of investments for the year"), t(54, 740, 8, "EXAMPLE FUND ETF"),
		t(250, 734, 8, ppdfGBP(d.closing)), t(54, 728, 8, "GBP ACC"), t(54, 700, 8, "Total"), t(250, 700, 8, ppdfGBP(d.closing))}, footer(2)...)

	service := d.service + d.dealing
	investment := d.entry + d.exit + d.ongoing + d.transaction + d.perfFees
	row := func(y float64, label string, n int64) []string {
		return []string{t(54, y, 8, label), t(265, y, 8, ppdfGBP(n)), t(310, y, 8, "A made-up explanation of this row.")}
	}
	charges := []string{t(54, 770, 14, "Summary of charges for the year to 31 March 2030"), t(54, 750, 9, "Service charges")}
	for _, r := range []struct {
		y     float64
		label string
		n     int64
	}{
		{735, "Service Fee", d.service}, {720, "Dealing Fees", d.dealing}, {705, "Total service charges", service},
		{680, "Entry charges", d.entry}, {665, "Exit charges", d.exit}, {650, "Ongoing charge to figures", d.ongoing},
		{635, "Ongoing transaction costs", d.transaction}, {620, "Performance fees and other incidental costs", d.perfFees},
		{605, "Total investment charges", investment},
	} {
		charges = append(charges, row(r.y, r.label, r.n)...)
	}
	charges = append(charges, t(54, 580, 8, "Charges already paid for Apr"), t(265, 575, 8, ppdfGBP(service+investment+d.paidDelta)), t(54, 570, 8, "2029 to Mar 2030"))
	charges = append(charges, footer(3)...)
	return ppdfBuild(letter, statement, holdings, charges)
}

func ppdfFidelityNormalise(body []byte) (ledger.InvestmentResult, error) {
	return importer.FidelityAnnualNormaliser{}.NormaliseInvestmentFile(
		ledger.File{Format: string(importer.FormatFidelityAnnualPDF), Body: body}, time.UTC)
}

// AC2: a statement parses, with its period derived and its charges kept apart from the figures.
func TestPensionPDFFidelityParsesAStatement(t *testing.T) {
	res, err := ppdfFidelityNormalise(ppdfFidelityBase().pdf())
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != ledger.InvestmentKindStatement || res.RowCount != 1 || len(res.Events) != 0 || len(res.Accounts) != 1 ||
		res.Accounts[0] != (ledger.InvestmentAccountRec{AccountKey: ppdfRef, Product: ledger.ProductSIPP}) || len(res.Statements) != 1 {
		t.Fatalf("result %+v", res)
	}
	s := res.Statements[0]
	if s.AccountKey != ppdfRef || !s.PeriodStart.Equal(ppdfDate("2029-04-06")) || !s.PeriodEnd.Equal(ppdfDate("2030-04-05")) ||
		!s.IssuedOn.Equal(ppdfDate("2030-05-05")) {
		t.Fatalf("statement dates %+v", s)
	}
	if s.OpeningMinor != 0 || s.PaidInMinor != 1000000 || s.PaidOutMinor != 0 || s.GrowthMinor != -12345 ||
		s.ChargesMinor != nil || s.ClosingMinor != 987655 {
		t.Fatalf("statement figures %+v", s)
	}
	var charges struct {
		PeriodEnd string `json:"period_end"`
		Scope     string `json:"scope"`
		Items     []struct {
			Group       string `json:"group"`
			Label       string `json:"label"`
			AmountMinor int64  `json:"amount_minor"`
		} `json:"items"`
		Paid int64 `json:"charges_paid_minor"`
	}
	if err := json.Unmarshal(s.Charges, &charges); err != nil {
		t.Fatal(err)
	}
	if charges.PeriodEnd != "2030-03-31" || charges.Scope != "all_pension_accounts" || len(charges.Items) != 7 || charges.Paid != 4400 ||
		charges.Items[0].Label != "Service Fee" || charges.Items[0].AmountMinor != 1200 || charges.Items[4].AmountMinor != 2100 {
		t.Fatalf("charges %s", s.Charges)
	}
}

// A gain on a non-zero opening with a money-out row also reads.
func TestPensionPDFFidelityReadsAGainAndMoneyOut(t *testing.T) {
	d := ppdfFidelityBase()
	d.opening, d.perf, d.moneyOut = 200000, 4321, 5000
	d.fill()
	res, err := ppdfFidelityNormalise(d.pdf())
	if err != nil {
		t.Fatal(err)
	}
	if s := res.Statements[0]; s.OpeningMinor != 200000 || s.PaidOutMinor != 5000 || s.GrowthMinor != 4321 || s.ClosingMinor != 1199321 {
		t.Fatalf("statement %+v", s)
	}
}

// AC2: a file whose arithmetic does not add up is refused.
func TestPensionPDFFidelityRejects(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(d *ppdfFidelityDoc)
	}{
		{"closing off by a penny", func(d *ppdfFidelityDoc) { d.closing++ }},
		{"performance in the table disagrees", func(d *ppdfFidelityDoc) { d.tablePerfDelta = 1 }},
		{"an unrecognised row in the block", func(d *ppdfFidelityDoc) { d.extraLabel = "Bonus added" }},
		{"footer reference differs", func(d *ppdfFidelityDoc) { d.footerRef = "AP00009999" }},
		{"charges do not add up", func(d *ppdfFidelityDoc) { d.paidDelta = 1 }},
		{"no letter date", func(d *ppdfFidelityDoc) { d.issued = "soon" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := ppdfFidelityBase()
			c.edit(d)
			_, err := ppdfFidelityNormalise(d.pdf())
			ppdfWantRefused(t, err)
		})
	}
}

// ---- fidelity-trade-note-pdf fixture ----

type ppdfNoteCharge struct {
	label string

	pence int64
}

type ppdfNoteTrade struct {
	name, isin, venue, exec, settle, units, price, ref string

	gross, netDelta int64

	charges, taxes []ppdfNoteCharge
}

func (n ppdfNoteTrade) fee() int64 {
	var sum int64
	for _, c := range n.charges {
		sum += c.pence
	}
	return sum
}

func (n ppdfNoteTrade) tax() int64 {
	var sum int64
	for _, c := range n.taxes {
		sum += c.pence
	}
	return sum
}

func (n ppdfNoteTrade) net(side string) int64 {
	if side == "sold" {
		return n.gross - n.fee() - n.tax() + n.netDelta
	}
	return n.gross + n.fee() + n.tax() + n.netDelta
}

func ppdfTradeBase(ref string) ppdfNoteTrade {
	return ppdfNoteTrade{name: "EXAMPLE FUND ETF GBP ACC", isin: "GB00EXAMPLE1", venue: "EXAMPLE EXCHANGE (XLON)",
		exec: "5 Jul 2030 9:05:01 am", settle: "9 Jul 2030", units: "1,000.50", price: "1.23456", ref: ref, gross: 123518,
		charges: []ppdfNoteCharge{{"Dealing fee", 750}}}
}

// ppdfNotePDF builds a note with a section for each side that has trades, sold first.
func ppdfNotePDF(account string, sold, bought []ppdfNoteTrade) []byte {
	t := pdfdoctest.Text
	page := []string{t(391, 770, 8, "How to get in touch"), t(391, 746, 8, "You\x92ll need this reference if you call:"), t(391, 734, 8, account),
		t(72, 720, 7, "ZZ MAIL 1"), t(72, 700, 10, "MR A EXAMPLE"), t(72, 688, 10, "1 EXAMPLE ROAD"), t(72, 676, 10, "EXAMPLE TOWN"), t(72, 664, 10, "EX1 1AA"),
		t(72, 640, 10, "10 Jul 2030"), t(72, 620, 14, "Confirmation of changes to your investments"),
		t(72, 600, 10, "The table below has full details of recent changes to your investments.")}
	y := 575.0
	next := func() float64 {
		y -= 12
		return y + 12
	}
	section := func(side string, trades []ppdfNoteTrade) {
		if len(trades) == 0 {
			return
		}
		page = append(page, t(72, next(), 10, "Your Example Pension Account Number: "+account), t(76, next(), 10, "Investments "+side))
		var total int64
		for _, n := range trades {
			page = append(page, t(75, next(), 8, n.name+" ("+n.isin+")"),
				t(75, next(), 8, "Your Instruction received: 5 Jul 2030 8:00 am Market Order"),
				t(75, next(), 8, "Investment "+side+" on "+n.venue+": "+n.exec+" Settlement date: "+n.settle))
			deal := next()
			page = append(page, t(75, deal, 8, "You "+side+" "+n.units+" units at \xa3"+n.price+" each (transaction reference: "+n.ref+")"),
				t(484, deal, 8, ppdfGBP(n.gross)))
			for _, c := range n.charges {
				page = append(page, t(94, next(), 8, c.label+" "+ppdfGBP(c.pence)))
			}
			word, netLabel := "added", "Cost of this investment plus charges"
			if side == "sold" {
				word, netLabel = "deducted", "Amount received from this sale after charges"
			}
			charged := next()
			page = append(page, t(93, charged, 8, "Total charges "+word), t(421, charged, 8, ppdfGBP(n.fee())))
			for _, c := range n.taxes {
				page = append(page, t(94, next(), 8, c.label+" "+ppdfGBP(c.pence)))
			}
			netRow := next()
			page = append(page, t(75, netRow, 8, netLabel), t(484, netRow, 8, ppdfGBP(n.net(side))))
			total += n.net(side)
		}
		label := "Total paid, including charges"
		if side == "sold" {
			label = "Total received, after charges"
		}
		row := next()
		page = append(page, t(75, row, 10, label), t(474, row, 10, ppdfGBP(total)))
	}
	section("sold", sold)
	section("bought", bought)
	page = append(page, t(72, 40, 7, "Made-up regulatory line"))
	boilerplate := []string{t(72, 770, 10, "Some points to bear in mind"), t(72, 750, 8, "Stamp Duty or Financial Transaction Tax may apply."),
		t(72, 12, 7, "Page 2 of 2")}
	return ppdfBuild(page, boilerplate)
}

func ppdfNoteNormalise(body []byte, loc *time.Location) (ledger.InvestmentResult, error) {
	return importer.FidelityTradeNoteNormaliser{}.NormaliseInvestmentFile(
		ledger.File{Format: string(importer.FormatFidelityTradeNotePDF), Body: body}, loc)
}

func ppdfLondon(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// AC3: a note with several trades parses, each with its own figures, and lists no account.
func TestPensionPDFTradeNoteParsesSeveralTrades(t *testing.T) {
	first := ppdfTradeBase("1000000001")
	second := ppdfTradeBase("1000000002")
	second.exec, second.settle, second.units, second.price, second.gross = "5 Jan 2030 2:10:00 pm", "9 Jan 2030", "10.25", "100.00000", 102500
	second.charges = []ppdfNoteCharge{{"Dealing fee", 750}, {"Stamp Duty", 513}}
	third := ppdfTradeBase("1000000003")
	third.taxes = []ppdfNoteCharge{{"Tax on this trade", 125}}

	res, err := ppdfNoteNormalise(ppdfNotePDF(ppdfRef, []ppdfNoteTrade{first, second}, []ppdfNoteTrade{third}), ppdfLondon(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != ledger.InvestmentKindTradeNote || res.RowCount != 3 || len(res.Accounts) != 0 || len(res.TradeNotes) != 3 {
		t.Fatalf("result %+v", res)
	}
	want := []ledger.TradeNoteRec{
		{AccountKey: ppdfRef, Reference: "1000000001", ISIN: "GB00EXAMPLE1", ExecutedAt: time.Date(2030, 7, 5, 8, 5, 1, 0, time.UTC),
			Venue: "EXAMPLE EXCHANGE (XLON)", Units: "1000.50", ExactPrice: "1.23456", GrossMinor: 123518, FeeMinor: 750, TaxMinor: 0,
			NetMinor: 122768, SettleDate: ppdfDate("2030-07-09")},
		{AccountKey: ppdfRef, Reference: "1000000002", ISIN: "GB00EXAMPLE1", ExecutedAt: time.Date(2030, 1, 5, 14, 10, 0, 0, time.UTC),
			Venue: "EXAMPLE EXCHANGE (XLON)", Units: "10.25", ExactPrice: "100.00000", GrossMinor: 102500, FeeMinor: 1263, TaxMinor: 0,
			NetMinor: 101237, SettleDate: ppdfDate("2030-01-09")},
		{AccountKey: ppdfRef, Reference: "1000000003", ISIN: "GB00EXAMPLE1", ExecutedAt: time.Date(2030, 7, 5, 8, 5, 1, 0, time.UTC),
			Venue: "EXAMPLE EXCHANGE (XLON)", Units: "1000.50", ExactPrice: "1.23456", GrossMinor: 123518, FeeMinor: 750, TaxMinor: 125,
			NetMinor: 124393, SettleDate: ppdfDate("2030-07-09")},
	}
	for i, w := range want {
		got := res.TradeNotes[i]
		if !got.ExecutedAt.Equal(w.ExecutedAt) || !got.SettleDate.Equal(w.SettleDate) {
			t.Fatalf("note %d times: %v, %v", i, got.ExecutedAt, got.SettleDate)
		}
		got.ExecutedAt, got.SettleDate = w.ExecutedAt, w.SettleDate
		if got != w {
			t.Fatalf("note %d: got %+v, want %+v", i, got, w)
		}
	}
}

// A printed time is read in the zone given: an ambiguous one takes the later instant.
func TestPensionPDFTradeNoteReadsAmbiguousTimeAsTheLater(t *testing.T) {
	n := ppdfTradeBase("1000000001")
	n.exec = "27 Oct 2030 1:30:00 am"
	res, err := ppdfNoteNormalise(ppdfNotePDF(ppdfRef, []ppdfNoteTrade{n}, nil), ppdfLondon(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.TradeNotes[0].ExecutedAt; !got.Equal(time.Date(2030, 10, 27, 1, 30, 0, 0, time.UTC)) {
		t.Fatalf("executed at %v", got)
	}
}

// AC3: a trade whose gross, fee, tax and net do not agree is refused, as is anything unclassified.
func TestPensionPDFTradeNoteRejects(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(n *ppdfNoteTrade)
	}{
		{"net off by a penny", func(n *ppdfNoteTrade) { n.netDelta = 1 }},
		{"an unclassified charge row", func(n *ppdfNoteTrade) { n.charges = append(n.charges, ppdfNoteCharge{"Mystery levy", 10}) }},
		{"an unclassified row after the total", func(n *ppdfNoteTrade) { n.taxes = []ppdfNoteCharge{{"Mystery levy", 10}} }},
		{"a time that does not exist", func(n *ppdfNoteTrade) { n.exec = "31 Mar 2030 1:30:00 am" }},
		{"a bad settlement date", func(n *ppdfNoteTrade) { n.settle = "31 Foo 2030" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := ppdfTradeBase("1000000001")
			c.edit(&n)
			_, err := ppdfNoteNormalise(ppdfNotePDF(ppdfRef, []ppdfNoteTrade{n}, nil), ppdfLondon(t))
			ppdfWantRefused(t, err)
		})
	}
	t.Run("a repeated reference", func(t *testing.T) {
		n := ppdfTradeBase("1000000001")
		_, err := ppdfNoteNormalise(ppdfNotePDF(ppdfRef, []ppdfNoteTrade{n}, []ppdfNoteTrade{n}), ppdfLondon(t))
		ppdfWantRefused(t, err)
	})
	t.Run("no trades", func(t *testing.T) {
		_, err := ppdfNoteNormalise(ppdfNotePDF(ppdfRef, nil, nil), ppdfLondon(t))
		ppdfWantRefused(t, err)
	})
}

// ---- through the real ledger ----

// ppdfFakeLedgerFormat stands in for a ledger export: its file is the JSON of the result it yields.
const ppdfFakeLedgerFormat = "ppdf-fake-ledger"

type ppdfFakeLedger struct{}

func (ppdfFakeLedger) Provider() string { return ledger.ProviderFidelity }

func (ppdfFakeLedger) NormaliseInvestmentFile(f ledger.File, _ *time.Location) (ledger.InvestmentResult, error) {
	var r ledger.InvestmentResult
	if err := json.Unmarshal(f.Body, &r); err != nil {
		return r, errors.New("fake: unreadable")
	}
	return r, nil
}

type ppdfEnv struct {
	t *testing.T

	app *store.Store

	l *ledger.Ledger

	clock time.Time
}

func newPPDFEnv(t *testing.T) *ppdfEnv {
	t.Helper()
	ctx := context.Background()
	super := testpg.Start(t)
	if err := store.Bootstrap(ctx, super, "owner-secret", "app-secret"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := store.Migrate(ctx, testpg.UserURL(t, super, store.RoleOwner), ledger.NormaliserRev); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	app, err := store.Open(ctx, testpg.UserURL(t, super, store.RoleApp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	e := &ppdfEnv{t: t, app: app, clock: time.Date(2030, time.August, 1, 9, 0, 0, 0, time.UTC)}
	e.l = ledger.New(nil, map[string]ledger.FileNormaliser{}, time.UTC, slog.New(slog.NewJSONHandler(io.Discard, nil)), nil,
		ledger.WithInvestmentNormalisers(map[string]ledger.InvestmentNormaliser{
			string(importer.FormatAvivaAnnualPDF):       importer.AvivaAnnualNormaliser{},
			string(importer.FormatFidelityAnnualPDF):    importer.FidelityAnnualNormaliser{},
			string(importer.FormatFidelityTradeNotePDF): importer.FidelityTradeNoteNormaliser{},
			ppdfFakeLedgerFormat:                        ppdfFakeLedger{},
		}))
	return e
}

func (e *ppdfEnv) catchUp() ledger.Stats {
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
	return stats
}

func (e *ppdfEnv) platform(slug, provider string) {
	e.t.Helper()
	p, err := json.Marshal(ledger.AccountAdd{Slug: slug, Type: "platform", Currency: "GBP", Name: "Made-up", ExpectsImports: true, Provider: provider})
	if err != nil {
		e.t.Fatal(err)
	}
	e.clock = e.clock.Add(time.Minute)
	if _, err := store.InsertOperatorEntry(context.Background(), e.app.DB(),
		store.OperatorEntry{Kind: ledger.EntryAccountAdd, Payload: p, EnteredAt: e.clock}); err != nil {
		e.t.Fatal(err)
	}
	e.catchUp()
}

// importFile stores body for the platform and normalises it, and fails the test if it is refused.
func (e *ppdfEnv) importFile(slug string, format importer.Format, body []byte) {
	e.t.Helper()
	e.clock = e.clock.Add(time.Minute)
	if _, _, err := store.InsertImportFile(context.Background(), e.app.DB(), store.ImportFile{AccountSlug: slug, Format: string(format),
		FileName: "made-up", ImportedAt: e.clock, Body: body}); err != nil {
		e.t.Fatal(err)
	}
	if st := e.catchUp(); st.FilesRejected != 0 {
		e.t.Fatalf("file refused: %v", st.Rejections)
	}
}

func (e *ppdfEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.app.DB().QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// AC4: a note imported before its event is stored, creates no account, and joins once the event
// arrives.
func TestPensionPDFTradeNoteJoinsItsEventLater(t *testing.T) {
	e := newPPDFEnv(t)
	e.platform("fid", ledger.ProviderFidelity)
	const reference = "1000000001"
	e.importFile("fid", importer.FormatFidelityTradeNotePDF, ppdfNotePDF(ppdfRef, []ppdfNoteTrade{ppdfTradeBase(reference)}, nil))

	const joined = `SELECT count(*) FROM core.trade_note n
		JOIN core.account c ON c.parent_id = n.platform_id AND c.source_key = n.account_key
		JOIN core.investment_event v ON v.account_id = c.id AND v.source_key = n.reference
		WHERE n.reference = $1 AND n.isin = 'GB00EXAMPLE1'`
	if n := e.count(`SELECT count(*) FROM core.trade_note WHERE account_key = $1 AND reference = $2`, ppdfRef, reference); n != 1 {
		t.Fatalf("%d notes stored", n)
	}
	if n := e.count(`SELECT count(*) FROM core.account WHERE parent_id IS NOT NULL`); n != 0 {
		t.Fatalf("a note created %d child accounts", n)
	}
	if n := e.count(joined, reference); n != 0 {
		t.Fatalf("the note joined %d events before any arrived", n)
	}

	content := json.RawMessage(`{"made":"up"}`)
	body, err := json.Marshal(ledger.InvestmentResult{Kind: ledger.InvestmentKindLedger, RowCount: 1,
		Accounts: []ledger.InvestmentAccountRec{{AccountKey: ppdfRef, Product: ledger.ProductSIPP}},
		Events: []ledger.InvestmentEventRec{{AccountKey: ppdfRef, SourceKey: reference, EventType: ledger.EventSell, ProviderType: "Sell",
			InstrumentKey: "EXMP", InstrumentName: "Example Fund", OrderDate: ppdfDate("2030-07-05"), SettleDate: ppdfDate("2030-07-09"),
			Units: "1000.50", GrossMinor: -123518, Content: content, HashInput: content}}})
	if err != nil {
		t.Fatal(err)
	}
	e.importFile("fid", ppdfFakeLedgerFormat, body)
	if n := e.count(joined, reference); n != 1 {
		t.Fatalf("the note joins %d events", n)
	}
}

// AC5: of two statements for one period the later issue wins, in either import order, and a
// transfer out that only the losing statement contains is marked disappeared.
func TestPensionPDFReissuedStatementWinsInEitherOrder(t *testing.T) {
	e := newPPDFEnv(t)
	common := ppdfAvivaPay{"13/09/29", "Transfer payment out", 10000}
	onlyInLoser := ppdfAvivaPay{"20/10/29", "Transfer payment out", 7777}
	file := func(plan, issued string, out ...ppdfAvivaPay) []byte {
		d := ppdfAvivaBase(plan)
		d.issued, d.out = issued, out
		d.fill()
		return d.pdf()
	}
	for _, c := range []struct {
		name, slug, plan string

		loserFirst bool
	}{
		{name: "original first", slug: "avi-one", plan: "PX00001111", loserFirst: true},
		{name: "reissue first", slug: "avi-two", plan: "PX00002222", loserFirst: false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e.t = t
			e.platform(c.slug, ledger.ProviderAviva)
			loser := file(c.plan, "5 May 2030", common, onlyInLoser)
			winner := file(c.plan, "12 June 2030", common)
			first, second := winner, loser
			if c.loserFirst {
				first, second = loser, winner
			}
			e.importFile(c.slug, importer.FormatAvivaAnnualPDF, first)
			e.importFile(c.slug, importer.FormatAvivaAnnualPDF, second)

			if n := e.count(`SELECT count(*) FROM core.pension_statement s JOIN core.account a ON a.id = s.account_id
				WHERE a.source_key = $1`, c.plan); n != 2 {
				t.Fatalf("%d statements stored", n)
			}
			if n := e.count(`SELECT count(*) FROM core.pension_statement s JOIN core.account a ON a.id = s.account_id
				WHERE a.source_key = $1 AND s.is_winner`, c.plan); n != 1 {
				t.Fatalf("%d winners", n)
			}
			if n := e.count(`SELECT count(*) FROM core.pension_statement s JOIN core.account a ON a.id = s.account_id
				WHERE a.source_key = $1 AND s.is_winner AND s.issued_on = DATE '2030-06-12' AND s.paid_out_minor = 10000`, c.plan); n != 1 {
				t.Fatal("the reissue is not the winner")
			}
			const events = `SELECT count(*) FROM core.investment_event v JOIN core.account a ON a.id = v.account_id
				WHERE a.source_key = $1 AND v.gross_minor = $2 AND v.event_type = 'transfer_out' AND (v.disappeared_at IS NOT NULL) = $3`
			if n := e.count(events, c.plan, int64(-10000), false); n != 1 {
				t.Fatal("the transfer both statements contain is not current")
			}
			if n := e.count(events, c.plan, int64(-7777), true); n != 1 {
				t.Fatal("the transfer only the loser contains is not marked disappeared")
			}
		})
	}
}

// AC6: after importing all three formats, no table in core and no view in agent holds a printed
// name or address.
func TestPensionPDFNoAddressInAnyTableOrView(t *testing.T) {
	e := newPPDFEnv(t)
	ctx := context.Background()
	e.platform("avi", ledger.ProviderAviva)
	e.platform("fid", ledger.ProviderFidelity)
	e.importFile("avi", importer.FormatAvivaAnnualPDF, ppdfAvivaBase(ppdfPlan).pdf())
	e.importFile("fid", importer.FormatFidelityAnnualPDF, ppdfFidelityBase().pdf())
	e.importFile("fid", importer.FormatFidelityTradeNotePDF, ppdfNotePDF(ppdfRef, []ppdfNoteTrade{ppdfTradeBase("1000000001")}, nil))
	if n := e.count(`SELECT count(*) FROM core.pension_statement`); n != 2 {
		t.Fatalf("%d statements", n)
	}
	if n := e.count(`SELECT count(*) FROM core.trade_note`); n != 1 {
		t.Fatalf("%d trade notes", n)
	}

	rows, err := e.app.DB().Query(ctx, `SELECT table_schema, table_name FROM information_schema.tables
		WHERE table_schema IN ('core', 'agent') ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	var relations []pgx.Identifier
	for rows.Next() {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			t.Fatal(err)
		}
		relations = append(relations, pgx.Identifier{schema, name})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if len(relations) == 0 {
		t.Fatal("no relations to check")
	}

	patterns := make([]string, 0, len(ppdfPersonal))
	for _, token := range ppdfPersonal {
		patterns = append(patterns, "%"+token+"%")
	}
	for _, r := range relations {
		if n := e.count(`SELECT count(*) FROM `+r.Sanitize()+` x WHERE x::text ILIKE ANY ($1::text[])`, patterns); n != 0 {
			t.Errorf("%s holds a printed name or address in %d rows", r.Sanitize(), n)
		}
	}
}

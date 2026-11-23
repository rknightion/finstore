package importer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/pdfdoc"
	"github.com/rknightion/finstore/internal/pdftext"
)

// The three pension PDF formats, as `finstore import --format` spells them. Each is a
// ledger.InvestmentNormaliser: a pure function of the file's bytes and the time zone it is given.
//
// The parsers read only what a fixed label or a fixed shape identifies. The name, postal address and
// employer printed on a statement are never read into a record. Every error is a fixed category
// wrapping ErrFormat and carries nothing from the file.
const (
	FormatAvivaAnnualPDF       Format = "aviva-annual-pdf"
	FormatFidelityAnnualPDF    Format = "fidelity-annual-pdf"
	FormatFidelityTradeNotePDF Format = "fidelity-trade-note-pdf"
)

// AvivaAnnualNormaliser reads an Aviva annual pension statement.
type AvivaAnnualNormaliser struct{}

// FidelityAnnualNormaliser reads a Fidelity annual pension statement.
type FidelityAnnualNormaliser struct{}

// FidelityTradeNoteNormaliser reads a Fidelity confirmation of transactions.
type FidelityTradeNoteNormaliser struct{}

var (
	_ ledger.InvestmentNormaliser = AvivaAnnualNormaliser{}
	_ ledger.InvestmentNormaliser = FidelityAnnualNormaliser{}
	_ ledger.InvestmentNormaliser = FidelityTradeNoteNormaliser{}
)

// Provider implements ledger.InvestmentNormaliser.
func (AvivaAnnualNormaliser) Provider() string { return ledger.ProviderAviva }

// Provider implements ledger.InvestmentNormaliser.
func (FidelityAnnualNormaliser) Provider() string { return ledger.ProviderFidelity }

// Provider implements ledger.InvestmentNormaliser.
func (FidelityTradeNoteNormaliser) Provider() string { return ledger.ProviderFidelity }

// NormaliseInvestmentFile implements ledger.InvestmentNormaliser.
func (AvivaAnnualNormaliser) NormaliseInvestmentFile(f ledger.File, _ *time.Location) (ledger.InvestmentResult, error) {
	if f.Format != string(FormatAvivaAnnualPDF) {
		return ledger.InvestmentResult{}, ppdfErr("unknown format")
	}
	ts, err := ppdfRead(f.Body)
	if err != nil {
		return ledger.InvestmentResult{}, err
	}
	return ppdfAviva(ts)
}

// NormaliseInvestmentFile implements ledger.InvestmentNormaliser.
func (FidelityAnnualNormaliser) NormaliseInvestmentFile(f ledger.File, _ *time.Location) (ledger.InvestmentResult, error) {
	if f.Format != string(FormatFidelityAnnualPDF) {
		return ledger.InvestmentResult{}, ppdfErr("unknown format")
	}
	ts, err := ppdfRead(f.Body)
	if err != nil {
		return ledger.InvestmentResult{}, err
	}
	return ppdfFidelityAnnual(ts)
}

// NormaliseInvestmentFile implements ledger.InvestmentNormaliser. A printed execution time is wall
// clock time in loc.
func (FidelityTradeNoteNormaliser) NormaliseInvestmentFile(f ledger.File, loc *time.Location) (ledger.InvestmentResult, error) {
	if f.Format != string(FormatFidelityTradeNotePDF) {
		return ledger.InvestmentResult{}, ppdfErr("unknown format")
	}
	if loc == nil {
		loc = time.UTC
	}
	ts, err := ppdfRead(f.Body)
	if err != nil {
		return ledger.InvestmentResult{}, err
	}
	return ppdfTradeNote(ts, loc)
}

// Layout tolerances, in points or as a fraction of a string's font size. Text under ppdfMinSize is
// footers, mail codes and superscripts. ppdfRowEm is the reader's own row tolerance. ppdfNearEm is
// how far below a label its amount may sit, and ppdfWrapDrop the same for a label that wraps around
// its amount. ppdfChargeX bounds the amount column on the Fidelity charges page, whose explanatory
// paragraph shares its baselines.
const (
	ppdfMinSize  = 7.0
	ppdfRowEm    = 0.25
	ppdfNearEm   = 0.6
	ppdfWrapDrop = 14.0
	ppdfChargeX  = 300.0
	ppdfAnyX     = 1e9
	ppdfScanRows = 4
)

const ppdfDatePat = `\d{1,2} [A-Z][a-z]+ \d{4}`

const ppdfMoneyPat = `£[\d,]+\.\d\d`

var ppdfAmountRE = regexp.MustCompile(`^-?` + ppdfMoneyPat + `$`)

var ppdfTrailingAmountRE = regexp.MustCompile(`(-?` + ppdfMoneyPat + `)\.?$`)

var ppdfMoneyTokenRE = regexp.MustCompile(ppdfMoneyPat)

var ppdfBareDateRE = regexp.MustCompile(`^` + ppdfDatePat + `$`)

// ppdfErr is a refusal in fixed words. reason is always a literal, never text from the file.
func ppdfErr(reason string) error { return fmt.Errorf("%w: pension pdf: %s", ErrFormat, reason) }

// ppdfRead returns the file's strings in reading order, without the smallest print.
func ppdfRead(body []byte) ([]pdftext.Text, error) {
	all, err := pdfdoc.Read(body)
	if err != nil {
		return nil, ppdfErr("unreadable pdf")
	}
	out := make([]pdftext.Text, 0, len(all))
	for _, t := range all {
		if t.Size >= ppdfMinSize {
			out = append(out, t)
		}
	}
	return out, nil
}

// ppdfIndex is the first index at or after from whose string satisfies match, or -1.
func ppdfIndex(ts []pdftext.Text, from int, match func(pdftext.Text) bool) int {
	for i := from; i < len(ts); i++ {
		if match(ts[i]) {
			return i
		}
	}
	return -1
}

// ppdfPrefix is the first index at or after from whose string starts with prefix, or -1.
func ppdfPrefix(ts []pdftext.Text, from int, prefix string) int {
	return ppdfIndex(ts, from, func(t pdftext.Text) bool { return strings.HasPrefix(t.S, prefix) })
}

// ppdfHasLabels reports whether every label is printed somewhere in the file.
func ppdfHasLabels(ts []pdftext.Text, labels []string) bool {
	for _, label := range labels {
		if ppdfIndex(ts, 0, func(t pdftext.Text) bool { return strings.Contains(t.S, label) }) < 0 {
			return false
		}
	}
	return true
}

// ppdfNextMatch returns the submatches of the first of the next few strings on the page that match
// re. Only a string of the expected shape can ever be returned, so a name printed between a label
// and its value is passed over unread.
func ppdfNextMatch(ts []pdftext.Text, i int, re *regexp.Regexp) []string {
	for j := i + 1; j < len(ts) && j <= i+ppdfScanRows && ts[j].Page == ts[i].Page; j++ {
		if m := re.FindStringSubmatch(ts[j].S); m != nil {
			return m
		}
	}
	return nil
}

// ppdfAmountAfter reads the amount a label introduces: at the end of the label's own string, or else
// in the next string to its right, up to maxX, whose baseline is at most below points lower.
func ppdfAmountAfter(ts []pdftext.Text, i int, below, maxX float64) (int64, bool) {
	if m := ppdfTrailingAmountRE.FindStringSubmatch(ts[i].S); m != nil {
		return ppdfPounds(m[1])
	}
	for j := i + 1; j < len(ts) && ts[j].Page == ts[i].Page && ts[i].Y-ts[j].Y <= below; j++ {
		if ts[j].X > ts[i].X && ts[j].X <= maxX && ppdfAmountRE.MatchString(ts[j].S) {
			return ppdfPounds(ts[j].S)
		}
	}
	return 0, false
}

func ppdfPounds(s string) (int64, bool) {
	n, err := parsePounds(s, true)
	return n, err == nil
}

// ppdfDay parses a date in words, with the month written in full or in short.
func ppdfDay(s string) (time.Time, bool) {
	for _, layout := range []string{"2 January 2006", "2 Jan 2006"} {
		if d, err := time.Parse(layout, s); err == nil {
			return d, true
		}
	}
	return time.Time{}, false
}

// ppdfRow joins the strings of the visual row starting at i and returns the index after it.
func ppdfRow(ts []pdftext.Text, i int) (string, int) {
	parts := []string{ts[i].S}
	j := i + 1
	for j < len(ts) && ts[j].Page == ts[i].Page && ts[i].Y-ts[j].Y <= ppdfRowEm*ts[i].Size {
		parts = append(parts, ts[j].S)
		j++
	}
	return strings.Join(parts, " "), j
}

// ---- aviva-annual-pdf ----

var ppdfAvivaLabels = []string{"Statement for", "Plan number", "Value of your pension", "Money paid in", "Money paid out",
	"Employer Payments", "Payments Out", "Your plan costs and charges were", "Your investments have gone"}

var ppdfPeriodRE = regexp.MustCompile(`^(` + ppdfDatePat + `) to (` + ppdfDatePat + `)$`)

var ppdfPlanRE = regexp.MustCompile(`^([A-Z]{2}\d{8})$`)

var ppdfPlanRowRE = regexp.MustCompile(`^Plan number\s+(\S+)$`)

// The apostrophe in "we've" decodes differently between fonts, so anything short that is not a
// letter is accepted in its place.
var ppdfAvivaIssuedRE = regexp.MustCompile(`To work these values out on (` + ppdfDatePat + `) we[^a-z]{0,3}ve assumed:`)

var ppdfGrowthRE = regexp.MustCompile(`^Your investments have gone (up|down)\b`)

var ppdfPaymentRE = regexp.MustCompile(`^(\d\d/\d\d/\d\d) (.+) (` + ppdfMoneyPat + `)$`)

var ppdfPaymentHeaderRE = regexp.MustCompile(`^When\s+Payment\s+Amount$`)

var ppdfTotalRE = regexp.MustCompile(`^Total (` + ppdfMoneyPat + `)$`)

const ppdfAvivaSummaryHeading = "How much money you already have in your pension plan"

// ppdfPayment is one dated row of a payments table.
type ppdfPayment struct {
	date  time.Time
	kind  string
	pence int64
}

// ppdfAvivaFigures are a statement's summary figures. Growth is before charges.
type ppdfAvivaFigures struct {
	opening, paidIn, paidOut, growth, charges, closing int64
}

func ppdfAviva(ts []pdftext.Text) (ledger.InvestmentResult, error) {
	var none ledger.InvestmentResult
	if !ppdfHasLabels(ts, ppdfAvivaLabels) {
		return none, ppdfErr("missing label")
	}
	start, end, err := ppdfAvivaPeriod(ts)
	if err != nil {
		return none, err
	}
	plan, err := ppdfAvivaPlan(ts)
	if err != nil {
		return none, err
	}
	issued, err := ppdfAvivaIssued(ts)
	if err != nil {
		return none, err
	}
	fig, err := ppdfAvivaSummary(ts)
	if err != nil {
		return none, err
	}
	if fig.opening+fig.paidIn+fig.growth-fig.paidOut-fig.charges != fig.closing {
		return none, ppdfErr("statement arithmetic")
	}
	from := max(ppdfPrefix(ts, 0, "More info about your payments in the last year"), 0)
	employer, err := ppdfAvivaSection(ts, from, "Employer Payments")
	if err != nil {
		return none, err
	}
	if ppdfPaymentSum(employer) != fig.paidIn {
		return none, ppdfErr("payments in do not add up")
	}
	out, err := ppdfAvivaSection(ts, from, "Payments Out")
	if err != nil {
		return none, err
	}
	if ppdfPaymentSum(out) != fig.paidOut {
		return none, ppdfErr("payments out do not add up")
	}
	events, err := ppdfTransferEvents(plan, out)
	if err != nil {
		return none, err
	}
	charges := fig.charges
	return ledger.InvestmentResult{
		Kind:     ledger.InvestmentKindStatement,
		Accounts: []ledger.InvestmentAccountRec{{AccountKey: plan, Product: ledger.ProductPensionPlan}},
		Events:   events,
		Statements: []ledger.PensionStatementRec{{AccountKey: plan, PeriodStart: start, PeriodEnd: end, IssuedOn: issued,
			OpeningMinor: fig.opening, PaidInMinor: fig.paidIn, PaidOutMinor: fig.paidOut, GrowthMinor: fig.growth,
			ChargesMinor: &charges, ClosingMinor: fig.closing,
			Charges:     json.RawMessage(fmt.Sprintf(`{"total_minor":%d}`, fig.charges)),
			Projections: json.RawMessage(`{}`)}},
		RowCount: 1 + len(events),
	}, nil
}

// ppdfAvivaPeriod reads the period under each "Statement for" label. The header block repeats, and
// every copy must agree.
func ppdfAvivaPeriod(ts []pdftext.Text) (start, end time.Time, err error) {
	found := false
	for i := range ts {
		if ts[i].S != "Statement for" {
			continue
		}
		m := ppdfNextMatch(ts, i, ppdfPeriodRE)
		if m == nil {
			return start, end, ppdfErr("bad period")
		}
		s, ok1 := ppdfDay(m[1])
		e, ok2 := ppdfDay(m[2])
		if !ok1 || !ok2 || e.Before(s) {
			return start, end, ppdfErr("bad period")
		}
		if found && (!s.Equal(start) || !e.Equal(end)) {
			return start, end, ppdfErr("periods disagree")
		}
		start, end, found = s, e, true
	}
	if !found {
		return start, end, ppdfErr("bad period")
	}
	return start, end, nil
}

// ppdfAvivaPlan reads the plan number from each place it is printed, and every copy must agree.
func ppdfAvivaPlan(ts []pdftext.Text) (string, error) {
	plan := ""
	for i := range ts {
		value := ""
		if ts[i].S == "Plan number" {
			m := ppdfNextMatch(ts, i, ppdfPlanRE)
			if m == nil {
				return "", ppdfErr("bad plan number")
			}
			value = m[1]
		} else if m := ppdfPlanRowRE.FindStringSubmatch(ts[i].S); m != nil {
			value = m[1]
		}
		if value == "" {
			continue
		}
		if plan != "" && value != plan {
			return "", ppdfErr("plan numbers disagree")
		}
		plan = value
	}
	if !ppdfPlanRE.MatchString(plan) {
		return "", ppdfErr("bad plan number")
	}
	return plan, nil
}

// ppdfAvivaIssued reads the issue date: the date the statement's forecast assumptions are made on.
func ppdfAvivaIssued(ts []pdftext.Text) (time.Time, error) {
	for i := range ts {
		if m := ppdfAvivaIssuedRE.FindStringSubmatch(ts[i].S); m != nil {
			if d, ok := ppdfDay(m[1]); ok {
				return d, nil
			}
			break
		}
	}
	return time.Time{}, ppdfErr("no issue date")
}

// ppdfAvivaSummary reads the table of four amounts under the large heading, and the body lines below
// it. A body line that is printed must agree with the table.
func ppdfAvivaSummary(ts []pdftext.Text) (ppdfAvivaFigures, error) {
	var f ppdfAvivaFigures
	h := ppdfIndex(ts, 0, func(t pdftext.Text) bool {
		return t.Size >= 13 && strings.Contains(t.S, ppdfAvivaSummaryHeading)
	})
	if h < 0 {
		return f, ppdfErr("no summary table")
	}
	var amounts []int64
	for j := h + 1; j < len(ts) && j <= h+2*ppdfScanRows && len(amounts) < 4; j++ {
		for _, token := range ppdfMoneyTokenRE.FindAllString(ts[j].S, -1) {
			n, ok := ppdfPounds(token)
			if !ok {
				return f, ppdfErr("bad amount")
			}
			amounts = append(amounts, n)
		}
	}
	if len(amounts) != 4 {
		return f, ppdfErr("no summary table")
	}
	f.opening, f.paidIn, f.paidOut, f.closing = amounts[0], amounts[1], amounts[2], amounts[3]

	g := ppdfPrefix(ts, h, "Your investments have gone")
	if g < 0 {
		return f, ppdfErr("no growth line")
	}
	word := ppdfGrowthRE.FindStringSubmatch(ts[g].S)
	if word == nil {
		return f, ppdfErr("no growth line")
	}
	growth, ok := ppdfAmountAfter(ts, g, ppdfNearEm*ts[g].Size, ppdfAnyX)
	if !ok || growth < 0 {
		return f, ppdfErr("no growth line")
	}
	if word[1] == "down" {
		growth = -growth
	}
	f.growth = growth

	c := ppdfPrefix(ts, h, "Your plan costs and charges were")
	if c < 0 {
		return f, ppdfErr("no charges")
	}
	charges, ok := ppdfAmountAfter(ts, c, ppdfNearEm*ts[c].Size, ppdfAnyX)
	if !ok || charges < 0 {
		return f, ppdfErr("no charges")
	}
	f.charges = charges

	for _, line := range []struct {
		label string
		want  int64
	}{
		{"Your employer has added", f.paidIn},
		{"Payments out of your pension plan", f.paidOut},
		{"Costs and charges have been deducted from your pension plan", f.charges},
		{"If you asked to transfer your money to another pension scheme", f.closing},
		{"Total fund value", f.closing},
	} {
		i := ppdfPrefix(ts, h, line.label)
		if i < 0 {
			continue
		}
		if got, ok := ppdfAmountAfter(ts, i, ppdfNearEm*ts[i].Size, ppdfAnyX); !ok || got != line.want {
			return f, ppdfErr("summary lines disagree")
		}
	}
	return f, nil
}

// ppdfAvivaSection reads one payments table: its heading, its header row, dated rows and a Total
// row, which must equal their sum. Any other row rejects the file.
func ppdfAvivaSection(ts []pdftext.Text, from int, heading string) ([]ppdfPayment, error) {
	i := ppdfIndex(ts, from, func(t pdftext.Text) bool { return t.S == heading })
	if i < 0 {
		return nil, ppdfErr("no payments table")
	}
	var rows []ppdfPayment
	for j := i + 1; j < len(ts); {
		row, next := ppdfRow(ts, j)
		j = next
		if ppdfPaymentHeaderRE.MatchString(row) {
			continue
		}
		if m := ppdfTotalRE.FindStringSubmatch(row); m != nil {
			total, ok := ppdfPounds(m[1])
			if !ok || total != ppdfPaymentSum(rows) {
				return nil, ppdfErr("payments total")
			}
			return rows, nil
		}
		m := ppdfPaymentRE.FindStringSubmatch(row)
		if m == nil {
			return nil, ppdfErr("unexpected payment row")
		}
		date, err := time.Parse("02/01/2006", m[1][:6]+"20"+m[1][6:])
		if err != nil {
			return nil, ppdfErr("bad payment date")
		}
		pence, ok := ppdfPounds(m[3])
		if !ok {
			return nil, ppdfErr("bad amount")
		}
		rows = append(rows, ppdfPayment{date: date, kind: strings.Join(strings.Fields(m[2]), " "), pence: pence})
	}
	return nil, ppdfErr("payments total")
}

func ppdfPaymentSum(rows []ppdfPayment) int64 {
	var sum int64
	for _, r := range rows {
		sum += r.pence
	}
	return sum
}

// ppdfTransferEvents makes one transfer_out event of each payment out. A row has no reference, so
// its key is the SHA-256 of its date, type, payer (none is printed) and amount, plus its position
// among identical rows, as Keys does for statement rows. The same payment in a reissued statement
// therefore has the same key.
func ppdfTransferEvents(plan string, rows []ppdfPayment) ([]ledger.InvestmentEventRec, error) {
	seen := make(map[[sha256.Size]byte]int, len(rows))
	out := make([]ledger.InvestmentEventRec, 0, len(rows))
	for _, r := range rows {
		date := r.date.Format("2006-01-02")
		sum := sha256.Sum256([]byte(strings.Join([]string{date, r.kind, "", pence(r.pence)}, keySep)))
		seen[sum]++
		content, err := json.Marshal(struct {
			Date        string `json:"date"`
			Type        string `json:"type"`
			AmountMinor int64  `json:"amount_minor"`
		}{date, r.kind, r.pence})
		if err != nil {
			return nil, ppdfErr("bad payment row")
		}
		out = append(out, ledger.InvestmentEventRec{
			AccountKey:   plan,
			SourceKey:    hex.EncodeToString(sum[:]) + "#" + strconv.Itoa(seen[sum]),
			EventType:    ledger.EventTransferOut,
			ProviderType: r.kind,
			OrderDate:    r.date,
			GrossMinor:   -r.pence,
			FlowMinor:    -r.pence,
			Content:      content,
			HashInput:    content,
		})
	}
	return out, nil
}

// ---- fidelity-annual-pdf ----

var ppdfFidelityLabels = []string{"Your pension savings annual statement for the year to", "Amount you transferred to Fidelity",
	"Investment performance after charges", "Money in your savings account(s) on", "Summary of charges for the year to"}

var ppdfFidelityRefLabelRE = regexp.MustCompile(`^You.{0,3}ll need this reference if you call:$`)

var ppdfFidelityRefRE = regexp.MustCompile(`^([A-Z]{2}\d{8})$`)

var ppdfFidelityEndRE = regexp.MustCompile(`Your pension savings annual statement for the year to (` + ppdfDatePat + `)`)

var ppdfFidelityInRE = regexp.MustCompile(`^Amount you transferred to Fidelity (` + ppdfMoneyPat + `)$`)

var ppdfFidelityPerfRE = regexp.MustCompile(`^Investment performance after charges (-?` + ppdfMoneyPat + `)$`)

var ppdfFidelityClosingRE = regexp.MustCompile(`^Money in your savings account\(s\) on (` + ppdfDatePat + `) (` + ppdfMoneyPat + `)$`)

var ppdfFidelityOutRE = regexp.MustCompile(`^Money out\b.* (-?` + ppdfMoneyPat + `)$`)

var ppdfFidelityChargesRE = regexp.MustCompile(`Summary of charges for the year to (` + ppdfDatePat + `)`)

const (
	ppdfFidelityFooter    = "If you want to speak to us please quote:"
	ppdfFidelityTitle     = "Your annual pension benefit statement"
	ppdfFidelityTable     = "How much money there is in your pension savings account(s)"
	ppdfFidelityHappened  = "What happened during the year"
	ppdfFidelityChargeAll = "all_pension_accounts"
)

// ppdfFidelityFigures are the figures of the "What happened during the year" block. performance is
// signed and after charges.
type ppdfFidelityFigures struct {
	moneyIn, moneyOut, performance, closing int64
	printedOut                              bool
}

func ppdfFidelityAnnual(ts []pdftext.Text) (ledger.InvestmentResult, error) {
	var none ledger.InvestmentResult
	if !ppdfHasLabels(ts, ppdfFidelityLabels) {
		return none, ppdfErr("missing label")
	}
	account, err := ppdfFidelityAccount(ts)
	if err != nil {
		return none, err
	}
	issued, err := ppdfFidelityIssued(ts)
	if err != nil {
		return none, err
	}
	i := ppdfIndex(ts, 0, func(t pdftext.Text) bool { return ppdfFidelityEndRE.MatchString(t.S) })
	if i < 0 {
		return none, ppdfErr("bad period")
	}
	end, ok := ppdfDay(ppdfFidelityEndRE.FindStringSubmatch(ts[i].S)[1])
	if !ok {
		return none, ppdfErr("bad period")
	}
	// The period's start is not printed: it is the day after the same date a year before the end.
	start := end.AddDate(-1, 0, 0).AddDate(0, 0, 1)

	h := ppdfIndex(ts, 0, func(t pdftext.Text) bool { return strings.Contains(t.S, ppdfFidelityHappened) })
	if h < 0 {
		return none, ppdfErr("no summary block")
	}
	fig, err := ppdfFidelityBlock(ts, h, end)
	if err != nil {
		return none, err
	}
	opening, err := ppdfFidelityTableCheck(ts, h, fig)
	if err != nil {
		return none, err
	}
	if opening+fig.moneyIn-fig.moneyOut+fig.performance != fig.closing {
		return none, ppdfErr("statement arithmetic")
	}
	charges, err := ppdfFidelityCharges(ts)
	if err != nil {
		return none, err
	}
	return ledger.InvestmentResult{
		Kind:     ledger.InvestmentKindStatement,
		Accounts: []ledger.InvestmentAccountRec{{AccountKey: account, Product: ledger.ProductSIPP}},
		Statements: []ledger.PensionStatementRec{{AccountKey: account, PeriodStart: start, PeriodEnd: end, IssuedOn: issued,
			OpeningMinor: opening, PaidInMinor: fig.moneyIn, PaidOutMinor: fig.moneyOut, GrowthMinor: fig.performance,
			ClosingMinor: fig.closing, Charges: charges, Projections: json.RawMessage(`{}`)}},
		RowCount: 1,
	}, nil
}

// ppdfFidelityAccount reads the reference under the contact label. The footer of the later pages
// prints it again, and every copy must agree.
func ppdfFidelityAccount(ts []pdftext.Text) (string, error) {
	i := ppdfIndex(ts, 0, func(t pdftext.Text) bool { return ppdfFidelityRefLabelRE.MatchString(t.S) })
	if i < 0 {
		return "", ppdfErr("no account reference")
	}
	m := ppdfNextMatch(ts, i, ppdfFidelityRefRE)
	if m == nil {
		return "", ppdfErr("no account reference")
	}
	footers := 0
	for j := range ts {
		if ts[j].S != ppdfFidelityFooter {
			continue
		}
		f := ppdfNextMatch(ts, j, ppdfFidelityRefRE)
		if f == nil || f[1] != m[1] {
			return "", ppdfErr("account references disagree")
		}
		footers++
	}
	if footers == 0 {
		return "", ppdfErr("no account reference")
	}
	return m[1], nil
}

// ppdfFidelityIssued reads the letter date: the last bare date before the title on its page.
func ppdfFidelityIssued(ts []pdftext.Text) (time.Time, error) {
	title := ppdfIndex(ts, 0, func(t pdftext.Text) bool { return t.S == ppdfFidelityTitle })
	if title < 0 {
		return time.Time{}, ppdfErr("no issue date")
	}
	for j := title - 1; j >= 0 && ts[j].Page == ts[title].Page; j-- {
		if ppdfBareDateRE.MatchString(ts[j].S) {
			if d, ok := ppdfDay(ts[j].S); ok {
				return d, nil
			}
			break
		}
	}
	return time.Time{}, ppdfErr("no issue date")
}

// ppdfFidelityBlock reads the rows of the "What happened during the year" block, which starts after
// index h and runs to the next large heading or the end of the page. A row holding an amount that is
// not one of the known rows rejects the file.
func ppdfFidelityBlock(ts []pdftext.Text, h int, end time.Time) (ppdfFidelityFigures, error) {
	var f ppdfFidelityFigures
	var haveIn, havePerf, haveClosing bool
	for j := h + 1; j < len(ts) && ts[j].Page == ts[h].Page && ts[j].Size < 12; {
		row, next := ppdfRow(ts, j)
		j = next
		if !ppdfMoneyTokenRE.MatchString(row) {
			continue
		}
		var ok bool
		if m := ppdfFidelityInRE.FindStringSubmatch(row); m != nil && !haveIn {
			f.moneyIn, ok = ppdfPounds(m[1])
			haveIn = true
		} else if m := ppdfFidelityPerfRE.FindStringSubmatch(row); m != nil && !havePerf {
			f.performance, ok = ppdfPounds(m[1])
			havePerf = true
		} else if m := ppdfFidelityClosingRE.FindStringSubmatch(row); m != nil && !haveClosing {
			d, dated := ppdfDay(m[1])
			if !dated || !d.Equal(end) {
				return f, ppdfErr("closing date")
			}
			f.closing, ok = ppdfPounds(m[2])
			haveClosing = true
		} else if m := ppdfFidelityOutRE.FindStringSubmatch(row); m != nil && !f.printedOut {
			f.moneyOut, ok = ppdfPounds(m[1])
			if f.moneyOut < 0 {
				f.moneyOut = -f.moneyOut
			}
			f.printedOut = true
		}
		if !ok {
			return f, ppdfErr("unrecognised summary row")
		}
	}
	if !haveIn || !havePerf || !haveClosing {
		return f, ppdfErr("no summary block")
	}
	return f, nil
}

// ppdfFidelityTableCheck reads the table above the block, whose columns are the opening, the money
// in, the money out when one is printed, the unsigned performance and the closing. It returns the
// opening, which only the table prints, after checking the rest against the block. Amounts are taken
// as unsigned tokens because a large operator glyph can be joined to the string next to it.
func ppdfFidelityTableCheck(ts []pdftext.Text, h int, f ppdfFidelityFigures) (int64, error) {
	t := ppdfIndex(ts, 0, func(x pdftext.Text) bool { return strings.Contains(x.S, ppdfFidelityTable) })
	if t < 0 || t >= h {
		return 0, ppdfErr("no statement table")
	}
	var amounts []int64
	for j := t + 1; j < h; j++ {
		for _, token := range ppdfMoneyTokenRE.FindAllString(ts[j].S, -1) {
			n, ok := ppdfPounds(token)
			if !ok {
				return 0, ppdfErr("bad amount")
			}
			amounts = append(amounts, n)
		}
	}
	magnitude := f.performance
	if magnitude < 0 {
		magnitude = -magnitude
	}
	switch {
	case len(amounts) == 4 && !f.printedOut:
		if amounts[2] != magnitude {
			return 0, ppdfErr("statement table disagrees")
		}
	case len(amounts) == 5 && f.printedOut:
		if amounts[2] != f.moneyOut || amounts[3] != magnitude {
			return 0, ppdfErr("statement table disagrees")
		}
	default:
		return 0, ppdfErr("unrecognised statement table")
	}
	if amounts[1] != f.moneyIn || amounts[len(amounts)-1] != f.closing {
		return 0, ppdfErr("statement table disagrees")
	}
	return amounts[0], nil
}

// ppdfFidelityCharge is one printed charge. The labels are this parser's own, never file text.
type ppdfFidelityCharge struct {
	Group       string `json:"group"`
	Label       string `json:"label"`
	AmountMinor int64  `json:"amount_minor"`
}

// ppdfFidelityChargeLabels are the printed charge rows, in order. "Ongoing charge to figures" is
// spelled as the statement prints it.
var ppdfFidelityChargeLabels = []struct{ group, label string }{
	{"service", "Service Fee"},
	{"service", "Dealing Fees"},
	{"investment", "Entry charges"},
	{"investment", "Exit charges"},
	{"investment", "Ongoing charge to figures"},
	{"investment", "Ongoing transaction costs"},
	{"investment", "Performance fees and other incidental costs"},
}

// ppdfFidelityCharges reads the charges page into the statement's charges JSON. The page has its own
// end date and covers all of the holder's pension accounts, so it never sets ChargesMinor.
func ppdfFidelityCharges(ts []pdftext.Text) (json.RawMessage, error) {
	h := ppdfIndex(ts, 0, func(t pdftext.Text) bool { return ppdfFidelityChargesRE.MatchString(t.S) })
	if h < 0 {
		return nil, ppdfErr("no charges page")
	}
	end, ok := ppdfDay(ppdfFidelityChargesRE.FindStringSubmatch(ts[h].S)[1])
	if !ok {
		return nil, ppdfErr("no charges page")
	}
	amount := func(label string, below float64) (int64, bool) {
		i := ppdfIndex(ts, h+1, func(t pdftext.Text) bool { return t.X < ppdfChargeX && strings.HasPrefix(t.S, label) })
		if i < 0 {
			return 0, false
		}
		if below == 0 {
			below = ppdfNearEm * ts[i].Size
		}
		return ppdfAmountAfter(ts, i, below, ppdfChargeX)
	}
	items := make([]ppdfFidelityCharge, 0, len(ppdfFidelityChargeLabels))
	sums := map[string]int64{}
	for _, row := range ppdfFidelityChargeLabels {
		n, ok := amount(row.label, 0)
		if !ok {
			return nil, ppdfErr("missing charge row")
		}
		sums[row.group] += n
		items = append(items, ppdfFidelityCharge{Group: row.group, Label: row.label, AmountMinor: n})
	}
	service, ok1 := amount("Total service charges", 0)
	investment, ok2 := amount("Total investment charges", 0)
	paid, ok3 := amount("Charges already paid for", ppdfWrapDrop)
	if !ok1 || !ok2 || !ok3 {
		return nil, ppdfErr("missing charge row")
	}
	if service != sums["service"] || investment != sums["investment"] || service+investment != paid {
		return nil, ppdfErr("charges do not add up")
	}
	b, err := json.Marshal(struct {
		PeriodEnd        string               `json:"period_end"`
		Scope            string               `json:"scope"`
		Items            []ppdfFidelityCharge `json:"items"`
		ChargesPaidMinor int64                `json:"charges_paid_minor"`
	}{end.Format("2006-01-02"), ppdfFidelityChargeAll, items, paid})
	if err != nil {
		return nil, ppdfErr("bad charges")
	}
	return b, nil
}

// ---- fidelity-trade-note-pdf ----

var ppdfAccountRowRE = regexp.MustCompile(`Account Number:\s*(\S+)$`)

var ppdfISINRE = regexp.MustCompile(`^(.*) \(([A-Z]{2}[A-Z0-9]{9}[0-9])\)$`)

var ppdfExecutionRE = regexp.MustCompile(`^Investment (sold|bought) on (.+?) \(([A-Z0-9]{4})\): (\d{1,2} [A-Za-z]+ \d{4} \d{1,2}:\d\d:\d\d [ap]m) Settlement date: (\d{1,2} [A-Za-z]+ \d{4})$`)

var ppdfDealRE = regexp.MustCompile(`^You (sold|bought) ([\d,]+\.\d+) units at £([\d,]+\.\d+) each \(transaction reference: (\d+)\) (` + ppdfMoneyPat + `)$`)

var ppdfChargeTotalRE = regexp.MustCompile(`^Total charges (deducted|added) (` + ppdfMoneyPat + `)$`)

var ppdfChargeRowRE = regexp.MustCompile(`^(.+?) (` + ppdfMoneyPat + `)$`)

// ppdfChargeLabelRE is the closed list of rows a trade's total charges can be made of.
var ppdfChargeLabelRE = regexp.MustCompile(`(?i)^(dealing fee|stamp duty( or financial transaction tax)?|financial transaction tax|takeover panel levy)$`)

var ppdfTaxLabelRE = regexp.MustCompile(`(?i)\btax\b`)

var ppdfNetRE = regexp.MustCompile(`^(Amount received from this sale after charges|Cost of this investment plus charges) (` + ppdfMoneyPat + `)$`)

var ppdfSectionTotalRE = regexp.MustCompile(`^(Total received, after charges|Total paid, including charges) (` + ppdfMoneyPat + `)$`)

const (
	ppdfSold   = "sold"
	ppdfBought = "bought"
)

func ppdfTradeNote(ts []pdftext.Text, loc *time.Location) (ledger.InvestmentResult, error) {
	var none ledger.InvestmentResult
	var rows []string
	for i := 0; i < len(ts); {
		row, next := ppdfRow(ts, i)
		rows = append(rows, row)
		i = next
	}
	account := ""
	var notes []ledger.TradeNoteRec
	seen := map[string]bool{}
	for i := 0; i < len(rows); {
		row := rows[i]
		if row == "Some points to bear in mind" {
			break
		}
		if m := ppdfAccountRowRE.FindStringSubmatch(row); m != nil {
			if account != "" && account != m[1] {
				return none, ppdfErr("account numbers disagree")
			}
			account = m[1]
			i++
			continue
		}
		side := ""
		switch row {
		case "Investments sold":
			side = ppdfSold
		case "Investments bought":
			side = ppdfBought
		}
		if side == "" {
			i++
			continue
		}
		if account == "" {
			return none, ppdfErr("no account number")
		}
		section, next, err := ppdfTradeSection(rows, i+1, side, account, loc)
		if err != nil {
			return none, err
		}
		for _, n := range section {
			if seen[n.Reference] {
				return none, ppdfErr("repeated reference")
			}
			seen[n.Reference] = true
		}
		notes = append(notes, section...)
		i = next
	}
	if len(notes) == 0 {
		return none, ppdfErr("no trades")
	}
	return ledger.InvestmentResult{Kind: ledger.InvestmentKindTradeNote, TradeNotes: notes, RowCount: len(notes)}, nil
}

// ppdfTradeSection reads the trades of one section up to its total row, which must equal the sum of
// their net amounts. It returns the index after the total row.
func ppdfTradeSection(rows []string, i int, side, account string, loc *time.Location) ([]ledger.TradeNoteRec, int, error) {
	var out []ledger.TradeNoteRec
	var sum int64
	for i < len(rows) {
		if m := ppdfSectionTotalRE.FindStringSubmatch(rows[i]); m != nil {
			total, ok := ppdfPounds(m[2])
			if !ok || len(out) == 0 || total != sum || (m[1] == "Total received, after charges") != (side == ppdfSold) {
				return nil, 0, ppdfErr("section total")
			}
			return out, i + 1, nil
		}
		n, next, err := ppdfTrade(rows, i, side, account, loc)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, n)
		sum += n.NetMinor
		i = next
	}
	return nil, 0, ppdfErr("section total")
}

// ppdfTrade reads one trade block, whose rows come in a fixed order. The fee is the printed total of
// the trade's charges, stamp duty included. Tax is what a row labelled as tax prints after that
// total, and is 0 when there is none. A row of any other kind rejects the file.
func ppdfTrade(rows []string, i int, side, account string, loc *time.Location) (ledger.TradeNoteRec, int, error) {
	var none ledger.TradeNoteRec
	at := func(k int) string {
		if k < len(rows) {
			return rows[k]
		}
		return ""
	}
	name := ppdfISINRE.FindStringSubmatch(at(i))
	if name == nil {
		return none, 0, ppdfErr("unexpected trade row")
	}
	if !strings.HasPrefix(at(i+1), "Your Instruction received:") {
		return none, 0, ppdfErr("unexpected trade row")
	}
	exec := ppdfExecutionRE.FindStringSubmatch(at(i + 2))
	deal := ppdfDealRE.FindStringSubmatch(at(i + 3))
	if exec == nil || deal == nil || exec[1] != side || deal[1] != side {
		return none, 0, ppdfErr("unexpected trade row")
	}
	wall, err := time.Parse("2 Jan 2006 3:04:05 pm", exec[4])
	if err != nil {
		return none, 0, ppdfErr("bad execution time")
	}
	executed, ok := ppdfLocal(wall, loc)
	if !ok {
		return none, 0, ppdfErr("bad execution time")
	}
	settle, ok := ppdfDay(exec[5])
	if !ok {
		return none, 0, ppdfErr("bad settlement date")
	}
	gross, ok := ppdfPounds(deal[5])
	if !ok {
		return none, 0, ppdfErr("bad amount")
	}

	j := i + 4
	var itemised, fee int64
	for {
		row := at(j)
		if m := ppdfChargeTotalRE.FindStringSubmatch(row); m != nil {
			if (m[1] == "deducted") != (side == ppdfSold) {
				return none, 0, ppdfErr("unexpected trade row")
			}
			if fee, ok = ppdfPounds(m[2]); !ok {
				return none, 0, ppdfErr("bad amount")
			}
			j++
			break
		}
		m := ppdfChargeRowRE.FindStringSubmatch(row)
		if m == nil || !ppdfChargeLabelRE.MatchString(m[1]) {
			return none, 0, ppdfErr("unrecognised charge row")
		}
		n, ok := ppdfPounds(m[2])
		if !ok {
			return none, 0, ppdfErr("bad amount")
		}
		itemised += n
		j++
	}
	if itemised != fee {
		return none, 0, ppdfErr("charges do not add up")
	}

	var tax, net int64
	for {
		row := at(j)
		if m := ppdfNetRE.FindStringSubmatch(row); m != nil {
			if (m[1] == "Amount received from this sale after charges") != (side == ppdfSold) {
				return none, 0, ppdfErr("unexpected trade row")
			}
			if net, ok = ppdfPounds(m[2]); !ok {
				return none, 0, ppdfErr("bad amount")
			}
			j++
			break
		}
		m := ppdfChargeRowRE.FindStringSubmatch(row)
		if m == nil || !ppdfTaxLabelRE.MatchString(m[1]) {
			return none, 0, ppdfErr("unrecognised charge row")
		}
		n, ok := ppdfPounds(m[2])
		if !ok {
			return none, 0, ppdfErr("bad amount")
		}
		tax += n
		j++
	}

	want := gross + fee + tax
	if side == ppdfSold {
		want = gross - fee - tax
	}
	if want != net {
		return none, 0, ppdfErr("trade arithmetic")
	}
	return ledger.TradeNoteRec{
		AccountKey: account,
		Reference:  deal[4],
		ISIN:       name[2],
		ExecutedAt: executed,
		Venue:      exec[2] + " (" + exec[3] + ")",
		Units:      strings.ReplaceAll(deal[2], ",", ""),
		ExactPrice: strings.ReplaceAll(deal[3], ",", ""),
		GrossMinor: gross,
		FeeMinor:   fee,
		TaxMinor:   tax,
		NetMinor:   net,
		SettleDate: settle,
	}, j, nil
}

// ppdfLocal reads wall, whose fields are a printed local time, as an instant in loc. Of the two
// instants an ambiguous time could be it takes the later, and it reports false for a time that does
// not exist in loc.
func ppdfLocal(wall time.Time, loc *time.Location) (time.Time, bool) {
	var best time.Time
	found := false
	for _, probe := range []time.Duration{-24 * time.Hour, 24 * time.Hour} {
		_, offset := wall.Add(probe).In(loc).Zone()
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		l := candidate.In(loc)
		if time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), l.Second(), 0, time.UTC).Equal(wall) &&
			(!found || candidate.After(best)) {
			best, found = candidate, true
		}
	}
	return best.UTC(), found
}

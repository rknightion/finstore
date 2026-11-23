package importer

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rknightion/finstore/internal/ledger"
)

// The two Fidelity CSV formats, as `finstore import --format` spells them. Each is a
// ledger.InvestmentNormaliser of provider fidelity: FidelityTransactionsNormaliser for the first,
// FidelitySummaryNormaliser for the second.
const (
	FormatFidelityTransactionsCSV Format = "fidelity-transactions-csv"
	FormatFidelitySummaryCSV      Format = "fidelity-summary-csv"
)

// FidelityTransactionsNormaliser reads a Fidelity transaction history export into ledger events.
type FidelityTransactionsNormaliser struct{}

// FidelitySummaryNormaliser reads a Fidelity account summary export into holdings and valuations.
type FidelitySummaryNormaliser struct{}

var (
	_ ledger.InvestmentNormaliser = FidelityTransactionsNormaliser{}
	_ ledger.InvestmentNormaliser = FidelitySummaryNormaliser{}
)

// FidelityCSVError is why a Fidelity file was refused. Reason is one of a closed list of words and
// Row the 1-based data row it concerns, or 0 for the file as a whole. It never carries anything from
// the file: no amount, name, reference or account number.
type FidelityCSVError struct {
	Reason string
	Row    int
}

// The reasons a Fidelity file is refused.
const (
	fidReasonEncoding   = "not UTF-8"
	fidReasonPreamble   = "preamble"
	fidReasonHeader     = "header"
	fidReasonRowShape   = "row shape"
	fidReasonDate       = "date"
	fidReasonTime       = "export time"
	fidReasonAmount     = "amount"
	fidReasonQuantity   = "quantity"
	fidReasonPrice      = "price"
	fidReasonType       = "unknown type"
	fidReasonProduct    = "unknown product"
	fidReasonSign       = "contradictory sign"
	fidReasonStatus     = "status"
	fidReasonRefBlank   = "blank reference"
	fidReasonRefRepeat  = "repeated reference"
	fidReasonTicker     = "no ticker"
	fidReasonAccount    = "account number"
	fidReasonConflict   = "product conflict"
	fidReasonRowType    = "unknown row type"
	fidReasonOrphan     = "holding without an account row"
	fidReasonRepeatRow  = "repeated row"
	fidReasonCurrency   = "currency"
	fidReasonValueCheck = "value check failed"
	fidReasonTotalCheck = "total check failed"
)

func (e *FidelityCSVError) Error() string {
	if e.Row == 0 {
		return "importer: fidelity csv: " + e.Reason
	}
	return "importer: fidelity csv: " + e.Reason + " at row " + strconv.Itoa(e.Row)
}

// Unwrap makes every refusal an ErrFormat.
func (e *FidelityCSVError) Unwrap() error { return ErrFormat }

func fidErr(reason string, row int) error { return &FidelityCSVError{Reason: reason, Row: row} }

// Provider implements ledger.InvestmentNormaliser.
func (FidelityTransactionsNormaliser) Provider() string { return ledger.ProviderFidelity }

// Provider implements ledger.InvestmentNormaliser.
func (FidelitySummaryNormaliser) Provider() string { return ledger.ProviderFidelity }

const fidelityBOM = "\xef\xbb\xbf"

// fidelityProducts maps the printed product to the ledger's product constant. The list is closed.
var fidelityProducts = map[string]string{
	"SIPP - Pension Savings Account": ledger.ProductSIPP,
	"Cash Management Account":        ledger.ProductCashManagement,
	"Investment ISA":                 ledger.ProductISA,
}

var (
	fidDecimalRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	fidTickerRE  = regexp.MustCompile(`\(([^()]*)\)$`)
)

// fidNextLine returns the first line of data without its line end, and the rest.
func fidNextLine(data []byte) (line string, rest []byte, ok bool) {
	if len(data) == 0 {
		return "", nil, false
	}
	i := bytes.IndexByte(data, '\n')
	if i < 0 {
		return strings.TrimSuffix(string(data), "\r"), nil, true
	}
	return strings.TrimSuffix(string(data[:i]), "\r"), data[i+1:], true
}

// fidBody strips the byte order mark and refuses bytes that are not UTF-8.
func fidBody(body []byte) ([]byte, error) {
	body = bytes.TrimPrefix(body, []byte(fidelityBOM))
	if !utf8.Valid(body) {
		return nil, fidErr(fidReasonEncoding, 0)
	}
	return body, nil
}

// fidTicker is the final parenthesised token of a name such as "EXAMPLE FUND ETF (EXMP)".
func fidTicker(name string) (string, bool) {
	m := fidTickerRE.FindStringSubmatch(strings.TrimSpace(name))
	if m == nil || strings.TrimSpace(m[1]) == "" {
		return "", false
	}
	return strings.TrimSpace(m[1]), true
}

// fidAccountKey is a printed account number, refused unless it can end a slug.
func fidAccountKey(s string) (string, bool) {
	s = strings.TrimSpace(s)
	n := 0
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			n++
		}
	}
	return s, n >= 4
}

// fidelityMapping is how one provider type is read. A type has a mapping on a cash row or on a
// security row, not on both.
type fidelityMapping struct {
	event string
	// cash and flow say whether the row's Amount is its effect on cash and its value as an external flow.
	cash, flow bool
	// sign is +1 when a non-zero Amount must be positive, -1 when it must be negative, 0 when either.
	sign int
}

var fidelitySecurityTypes = map[string]fidelityMapping{
	"Buy":                {event: ledger.EventBuy, sign: 1},
	"Sell":               {event: ledger.EventSell, sign: -1},
	"Auto-sell For Fees": {event: ledger.EventSell, sign: -1},
	"Transfer In":        {event: ledger.EventTransferIn, flow: true, sign: 1},
}

var fidelityCashTypes = map[string]fidelityMapping{
	"Cash In For Transfer": {event: ledger.EventTransferIn, cash: true, flow: true, sign: 1},
	"Dealing Fee":          {event: ledger.EventFee, cash: true, sign: -1},
	"Service Fee":          {event: ledger.EventFee, cash: true, sign: -1},
	"Stamp Duty Or Financial Transaction Tax": {event: ledger.EventFee, cash: true, sign: -1},
	"Cash Interest":   {event: ledger.EventInterest, cash: true, sign: 1},
	"Tax On Interest": {event: ledger.EventTax, sign: -1},
	"Withdrawal":      {event: ledger.EventWithdrawal, cash: true, flow: true, sign: -1},
	"Adjustment":      {event: ledger.EventAdjustment, cash: true, flow: true},
	"Transfer To Cash Management Account For Fees": {event: ledger.EventSiblingTransfer, cash: true, flow: true, sign: -1},
	"Cash In Ring-fenced For Fees":                 {event: ledger.EventSiblingTransfer, cash: true, flow: true, sign: 1},
	"Buy":                                          {event: ledger.EventInternal, cash: true},
	"Sell":                                         {event: ledger.EventInternal, cash: true},
	"Auto-sell For Fees":                           {event: ledger.EventInternal, cash: true},
	"Cash Out For Buy":                             {event: ledger.EventInternal, cash: true, sign: -1},
	"Cash Out For Buy From Transfer":               {event: ledger.EventInternal, cash: true, sign: -1},
	"Cash In From Sell":                            {event: ledger.EventInternal, cash: true, sign: 1},
	"Cash In Fees From Auto-sell":                  {event: ledger.EventInternal, cash: true, sign: 1},
}

// The transaction history's preamble labels, in order, and its exact header.
var fidelityTransactionsPreamble = []string{"Account", "Timeframe", "Transaction type", "Investment name", "Valuations"}

var fidelityTransactionsHeader = []string{"Order date", "Completion date", "Transaction type", "Investments", "Product Wrapper",
	"Account Number", "Source investment", "Amount", "Quantity", "Price per unit", "Reference Number", "Status", ""}

const fidelityDayLayout = "02 Jan 2006"

// fidelityRowContent is the stored content of one transaction row and the input of its version
// hash: the row as printed, in key order, so its JSON encoding is canonical and the same row gives
// the same bytes in every export.
type fidelityRowContent struct {
	Account        string `json:"account"`
	Amount         string `json:"amount"`
	CompletionDate string `json:"completion_date"`
	Investments    string `json:"investments"`
	OrderDate      string `json:"order_date"`
	Price          string `json:"price"`
	Product        string `json:"product"`
	Quantity       string `json:"quantity"`
	Reference      string `json:"reference"`
	Source         string `json:"source_investment"`
	Status         string `json:"status"`
	Type           string `json:"type"`
}

// NormaliseInvestmentFile reads a transaction history. The file is the preamble, a blank line, the
// exact header and one row per provider ledger row. Every row becomes one event keyed by its
// Reference Number; a file that breaks any rule, however late in it, is refused whole.
func (FidelityTransactionsNormaliser) NormaliseInvestmentFile(f ledger.File, _ *time.Location) (ledger.InvestmentResult, error) {
	data, err := fidBody(f.Body)
	if err != nil {
		return ledger.InvestmentResult{}, err
	}
	for _, label := range fidelityTransactionsPreamble {
		var line string
		var ok bool
		if line, data, ok = fidNextLine(data); !ok || !strings.HasPrefix(line, label) {
			return ledger.InvestmentResult{}, fidErr(fidReasonPreamble, 0)
		}
	}
	line, data, ok := fidNextLine(data)
	if !ok || strings.TrimSpace(line) != "" {
		return ledger.InvestmentResult{}, fidErr(fidReasonPreamble, 0)
	}
	line, data, ok = fidNextLine(data)
	if !ok || line != strings.Join(fidelityTransactionsHeader, ",") {
		return ledger.InvestmentResult{}, fidErr(fidReasonHeader, 0)
	}

	res := ledger.InvestmentResult{Kind: ledger.InvestmentKindLedger}
	products := map[string]string{}
	refs := map[string]bool{}
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = len(fidelityTransactionsHeader)
	for n := 1; ; n++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ledger.InvestmentResult{}, fidErr(fidReasonRowShape, n)
		}
		ev, product, err := fidelityEvent(rec, n)
		if err != nil {
			return ledger.InvestmentResult{}, err
		}
		if refs[ev.SourceKey] {
			return ledger.InvestmentResult{}, fidErr(fidReasonRefRepeat, n)
		}
		refs[ev.SourceKey] = true
		if known, seen := products[ev.AccountKey]; !seen {
			products[ev.AccountKey] = product
			res.Accounts = append(res.Accounts, ledger.InvestmentAccountRec{AccountKey: ev.AccountKey, Product: product})
		} else if known != product {
			return ledger.InvestmentResult{}, fidErr(fidReasonConflict, n)
		}
		res.Events = append(res.Events, ev)
	}
	res.RowCount = len(res.Events)
	return res, nil
}

// fidelityEvent reads one data row.
func fidelityEvent(rec []string, row int) (ledger.InvestmentEventRec, string, error) {
	var ev ledger.InvestmentEventRec
	for i := range rec {
		rec[i] = strings.TrimSpace(rec[i])
	}
	if rec[12] != "" {
		return ev, "", fidErr(fidReasonRowShape, row)
	}
	content := fidelityRowContent{OrderDate: rec[0], CompletionDate: rec[1], Type: rec[2], Investments: rec[3], Product: rec[4],
		Account: rec[5], Source: rec[6], Amount: rec[7], Quantity: rec[8], Price: rec[9], Reference: rec[10], Status: rec[11]}

	product, ok := fidelityProducts[content.Product]
	if !ok {
		return ev, "", fidErr(fidReasonProduct, row)
	}
	account, ok := fidAccountKey(content.Account)
	if !ok {
		return ev, "", fidErr(fidReasonAccount, row)
	}
	if content.Status != "Completed" {
		return ev, "", fidErr(fidReasonStatus, row)
	}
	if content.Reference == "" {
		return ev, "", fidErr(fidReasonRefBlank, row)
	}
	order, err := time.Parse(fidelityDayLayout, content.OrderDate)
	if err != nil {
		return ev, "", fidErr(fidReasonDate, row)
	}
	var settle time.Time
	if content.CompletionDate != "" {
		if settle, err = time.Parse(fidelityDayLayout, content.CompletionDate); err != nil {
			return ev, "", fidErr(fidReasonDate, row)
		}
	}
	amount, err := parsePounds(content.Amount, true)
	if err != nil {
		return ev, "", fidErr(fidReasonAmount, row)
	}

	security := content.Investments != "Cash"
	types := fidelityCashTypes
	if security {
		types = fidelitySecurityTypes
	}
	m, ok := types[content.Type]
	if !ok {
		return ev, "", fidErr(fidReasonType, row)
	}
	if (m.sign > 0 && amount < 0) || (m.sign < 0 && amount > 0) {
		return ev, "", fidErr(fidReasonSign, row)
	}

	ev = ledger.InvestmentEventRec{AccountKey: account, SourceKey: content.Reference, EventType: m.event, ProviderType: content.Type,
		OrderDate: order, SettleDate: settle, GrossMinor: amount}
	if m.cash {
		ev.CashMinor = amount
	}
	if m.flow {
		ev.FlowMinor = amount
	}
	if security {
		ticker, ok := fidTicker(content.Investments)
		if !ok {
			return ev, "", fidErr(fidReasonTicker, row)
		}
		if !fidDecimalRE.MatchString(content.Quantity) {
			return ev, "", fidErr(fidReasonQuantity, row)
		}
		ev.InstrumentKey, ev.InstrumentName, ev.Units = ticker, content.Investments, content.Quantity
		if content.Price != "" {
			if !fidDecimalRE.MatchString(content.Price) {
				return ev, "", fidErr(fidReasonPrice, row)
			}
			ev.Price, ev.PriceUnit = content.Price, ledger.PriceUnitGBP
		}
	}
	b, err := json.Marshal(content)
	if err != nil {
		return ev, "", fidErr(fidReasonRowShape, row)
	}
	ev.Content, ev.HashInput = b, b
	return ev, product, nil
}

// The account summary's marker line, preamble labels and block header.
const fidelitySummaryMarker = "View all account details"

var fidelitySummaryHeader = []string{"Type", "Holdings", "Account number", "Product", "Account holder", "Designation", "Provider",
	"Latest price (pence/cents/yen)", "Export date", "Quantity", "Value (£)", "Currency", "Cash available", "Book cost (£)",
	"Gain/loss (£)", "Gain/loss (%)", "Annualised return (%)", "Total return (all time)", "Total return (%)"}

// Column positions in the summary block that are read. The holder, designation, provider and
// per-row export date columns are never read: the holder is a name, and the file's own export date
// and time give the capture.
const (
	fidSumType     = 0
	fidSumHoldings = 1
	fidSumAccount  = 2
	fidSumProduct  = 3
	fidSumPrice    = 7
	fidSumQuantity = 9
	fidSumValue    = 10
	fidSumCurrency = 11
	fidSumCash     = 12
	fidSumBookCost = 13
	fidSumGain     = 14
)

// fidelitySummaryPreamble are the preamble labels that are read, by label.
var fidelitySummaryPreamble = []string{"Client name", "Export date", "Export time"}

// NormaliseInvestmentFile reads an account summary. Only the block after the line "View all account
// details" is read, apart from the export date and time before it, which give the capture instant in
// loc. An Account row gives an account's total and cash, an Asset row one of its holdings. The file
// is refused unless each holding's units times price equals its value, and each account's holdings
// plus cash equal its total, within what the printed rounding allows.
func (FidelitySummaryNormaliser) NormaliseInvestmentFile(f ledger.File, loc *time.Location) (ledger.InvestmentResult, error) {
	data, err := fidBody(f.Body)
	if err != nil {
		return ledger.InvestmentResult{}, err
	}
	if loc == nil {
		loc = time.UTC
	}
	preamble := map[string]string{}
	for {
		var line string
		var ok bool
		if line, data, ok = fidNextLine(data); !ok {
			return ledger.InvestmentResult{}, fidErr(fidReasonPreamble, 0)
		}
		if strings.TrimRight(line, ", \t") == fidelitySummaryMarker {
			break
		}
		fields, err := csv.NewReader(strings.NewReader(line)).Read()
		if err != nil || len(fields) == 0 {
			continue
		}
		label := strings.TrimSpace(fields[0])
		if slices.Contains(fidelitySummaryPreamble, label) {
			if _, seen := preamble[label]; seen {
				return ledger.InvestmentResult{}, fidErr(fidReasonPreamble, 0)
			}
			preamble[label] = ""
			if len(fields) > 1 {
				preamble[label] = strings.TrimSpace(fields[1])
			}
		}
	}
	for _, label := range fidelitySummaryPreamble {
		if _, ok := preamble[label]; !ok {
			return ledger.InvestmentResult{}, fidErr(fidReasonPreamble, 0)
		}
	}
	day, err := time.Parse("02/01/2006", preamble["Export date"])
	if err != nil {
		return ledger.InvestmentResult{}, fidErr(fidReasonDate, 0)
	}
	clock, err := time.Parse("15:04", preamble["Export time"])
	if err != nil {
		return ledger.InvestmentResult{}, fidErr(fidReasonDate, 0)
	}
	captured, ok := fidelityWallTime(day, clock, loc)
	if !ok {
		return ledger.InvestmentResult{}, fidErr(fidReasonTime, 0)
	}

	// The block's header is the next line that is not blank.
	var header string
	for {
		if header, data, ok = fidNextLine(data); !ok {
			return ledger.InvestmentResult{}, fidErr(fidReasonHeader, 0)
		}
		if strings.TrimSpace(header) != "" {
			break
		}
	}
	if header != strings.Join(fidelitySummaryHeader, ",") {
		return ledger.InvestmentResult{}, fidErr(fidReasonHeader, 0)
	}

	res := ledger.InvestmentResult{Kind: ledger.InvestmentKindHoldings, CapturedAt: &captured}
	accounts := map[string]int{} // account key to its index in res.Valuations
	holdingCount := map[string]int{}
	holdingSum := map[string]int64{}
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	type heldKey struct{ account, ticker string }
	held := map[heldKey]bool{}
	for n := 1; ; n++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ledger.InvestmentResult{}, fidErr(fidReasonRowShape, n)
		}
		// An Asset row prints one empty field after the last column of the header.
		if len(rec) == len(fidelitySummaryHeader)+1 && strings.TrimSpace(rec[len(rec)-1]) == "" {
			rec = rec[:len(fidelitySummaryHeader)]
		}
		if len(rec) != len(fidelitySummaryHeader) {
			return ledger.InvestmentResult{}, fidErr(fidReasonRowShape, n)
		}
		for i := range rec {
			rec[i] = strings.TrimSpace(rec[i])
		}
		account, ok := fidAccountKey(rec[fidSumAccount])
		if !ok {
			return ledger.InvestmentResult{}, fidErr(fidReasonAccount, n)
		}
		product, ok := fidelityProducts[rec[fidSumProduct]]
		if !ok {
			return ledger.InvestmentResult{}, fidErr(fidReasonProduct, n)
		}
		switch rec[fidSumType] {
		case "Account":
			if _, seen := accounts[account]; seen {
				return ledger.InvestmentResult{}, fidErr(fidReasonRepeatRow, n)
			}
			v, reason := fidelityValuation(rec, account, day)
			if reason != "" {
				return ledger.InvestmentResult{}, fidErr(reason, n)
			}
			accounts[account] = len(res.Valuations)
			res.Accounts = append(res.Accounts, ledger.InvestmentAccountRec{AccountKey: account, Product: product})
			res.Valuations = append(res.Valuations, v)
		case "Asset":
			i, seen := accounts[account]
			if !seen {
				return ledger.InvestmentResult{}, fidErr(fidReasonOrphan, n)
			}
			if res.Accounts[i].Product != product {
				return ledger.InvestmentResult{}, fidErr(fidReasonConflict, n)
			}
			h, reason := fidelityHolding(rec, account, day)
			if reason != "" {
				return ledger.InvestmentResult{}, fidErr(reason, n)
			}
			k := heldKey{account, h.InstrumentKey}
			if held[k] {
				return ledger.InvestmentResult{}, fidErr(fidReasonRepeatRow, n)
			}
			held[k] = true
			holdingCount[account]++
			holdingSum[account] += h.ValueMinor
			res.Holdings = append(res.Holdings, h)
		default:
			return ledger.InvestmentResult{}, fidErr(fidReasonRowType, n)
		}
		res.RowCount++
	}
	// Total: holdings plus cash equal the printed total, to within one penny a holding.
	for _, v := range res.Valuations {
		diff := v.TotalMinor - v.CashMinor - holdingSum[v.AccountKey]
		if diff < 0 {
			diff = -diff
		}
		if diff > int64(holdingCount[v.AccountKey]) {
			return ledger.InvestmentResult{}, fidErr(fidReasonTotalCheck, 0)
		}
	}
	return res, nil
}

// fidSummaryMoney reads a summary amount in pounds to pence. The summary drops trailing zeros, so 12,
// 12.5 and 12.50 are all valid; more than two decimal places is not.
func fidSummaryMoney(s string) (int64, error) {
	whole, frac, _ := strings.Cut(strings.TrimSpace(s), ".")
	if len(frac) > 2 {
		return 0, ErrFormat
	}
	return parsePounds(whole+"."+frac+strings.Repeat("0", 2-len(frac)), true)
}

// fidelityValuation reads an Account row. The second result is a reason, empty when the row is good.
func fidelityValuation(rec []string, account string, day time.Time) (ledger.ValuationRec, string) {
	total, err := fidSummaryMoney(rec[fidSumValue])
	if err != nil {
		return ledger.ValuationRec{}, fidReasonAmount
	}
	// An account with nothing in it prints a zero value and a blank cash field.
	var cash int64
	if rec[fidSumCash] != "" {
		if cash, err = fidSummaryMoney(rec[fidSumCash]); err != nil {
			return ledger.ValuationRec{}, fidReasonAmount
		}
	} else if total != 0 {
		return ledger.ValuationRec{}, fidReasonAmount
	}
	v := ledger.ValuationRec{AccountKey: account, AsOf: day, TotalMinor: total, CashMinor: cash}
	if rec[fidSumGain] != "" {
		gain, err := fidSummaryMoney(rec[fidSumGain])
		if err != nil {
			return ledger.ValuationRec{}, fidReasonAmount
		}
		v.ReportedGainMinor = &gain
	}
	return v, ""
}

// fidelityHolding reads an Asset row and runs the Value check on it. The second result is a reason,
// empty when the row is good.
func fidelityHolding(rec []string, account string, day time.Time) (ledger.HoldingRec, string) {
	ticker, ok := fidTicker(rec[fidSumHoldings])
	if !ok {
		return ledger.HoldingRec{}, fidReasonTicker
	}
	if !fidDecimalRE.MatchString(rec[fidSumQuantity]) {
		return ledger.HoldingRec{}, fidReasonQuantity
	}
	if !fidDecimalRE.MatchString(rec[fidSumPrice]) {
		return ledger.HoldingRec{}, fidReasonPrice
	}
	value, err := fidSummaryMoney(rec[fidSumValue])
	if err != nil {
		return ledger.HoldingRec{}, fidReasonAmount
	}
	h := ledger.HoldingRec{AccountKey: account, InstrumentKey: ticker, InstrumentName: rec[fidSumHoldings], AsOf: day,
		Units: rec[fidSumQuantity], Price: rec[fidSumPrice], PriceUnit: ledger.PriceUnitGBX, ValueMinor: value}
	if rec[fidSumBookCost] != "" {
		cost, err := fidSummaryMoney(rec[fidSumBookCost])
		if err != nil {
			return ledger.HoldingRec{}, fidReasonAmount
		}
		h.BookCostMinor = &cost
	}
	// The value columns are sterling whatever the quote currency, and a holding record has one
	// currency, so a holding quoted in another currency is refused until it has a mapping.
	if rec[fidSumCurrency] != "GBP" {
		return ledger.HoldingRec{}, fidReasonCurrency
	}
	if !fidelityValueWithinRounding(h.Units, h.Price, value) {
		return ledger.HoldingRec{}, fidReasonValueCheck
	}
	return h, ""
}

// fidelityValueWithinRounding is the Value check. With p the price in pence, v the value in pence,
// u the units, du the printed decimal places of u and dp the printed decimal places of p, it holds
// when |u*p - v| <= u*10^-dp/2 + p*10^-du/2 + 10^-dp*10^-du/4 + 1/2. Both decimals are valid
// unsigned decimal strings; the arithmetic is exact.
func fidelityValueWithinRounding(units, price string, valuePence int64) bool {
	u, okU := new(big.Rat).SetString(units)
	p, okP := new(big.Rat).SetString(price)
	if !okU || !okP {
		return false
	}
	step := func(s string) *big.Rat { // 10^-places
		_, frac, _ := strings.Cut(s, ".")
		return new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(frac))), nil))
	}
	du, dp := step(units), step(price)
	two, four := big.NewRat(2, 1), big.NewRat(4, 1)
	allowed := new(big.Rat).Quo(new(big.Rat).Mul(u, dp), two)
	allowed.Add(allowed, new(big.Rat).Quo(new(big.Rat).Mul(p, du), two))
	allowed.Add(allowed, new(big.Rat).Quo(new(big.Rat).Mul(dp, du), four))
	allowed.Add(allowed, big.NewRat(1, 2))
	diff := new(big.Rat).Sub(new(big.Rat).Mul(u, p), new(big.Rat).SetInt64(valuePence))
	return diff.Abs(diff).Cmp(allowed) <= 0
}

// fidelityWallTime is the instant of a printed date and time of day in loc. A time that happens
// twice takes the later instant, and a time that does not happen is refused.
func fidelityWallTime(day, clock time.Time, loc *time.Location) (time.Time, bool) {
	wall := time.Date(day.Year(), day.Month(), day.Day(), clock.Hour(), clock.Minute(), 0, 0, time.UTC)
	const layout = "2006-01-02 15:04"
	var best time.Time
	for _, probe := range []time.Time{wall.Add(-36 * time.Hour), wall, wall.Add(36 * time.Hour)} {
		_, offset := probe.In(loc).Zone()
		instant := wall.Add(-time.Duration(offset) * time.Second)
		if instant.In(loc).Format(layout) == wall.Format(layout) && instant.After(best) {
			best = instant
		}
	}
	return best, !best.IsZero()
}

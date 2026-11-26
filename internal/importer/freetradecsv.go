package importer

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
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

// FormatFreetradeActivityCSV is Freetrade's activity statement, as `finstore import --format` spells
// it. FreetradeActivityNormaliser reads it as a ledger.InvestmentNormaliser of provider freetrade.
const FormatFreetradeActivityCSV Format = "freetrade-activity-csv"

// FreetradeActivityNormaliser reads a Freetrade activity statement into ledger events.
type FreetradeActivityNormaliser struct{}

var _ ledger.InvestmentNormaliser = FreetradeActivityNormaliser{}

// Provider implements ledger.InvestmentNormaliser.
func (FreetradeActivityNormaliser) Provider() string { return ledger.ProviderFreetrade }

// FreetradeCSVError is why a Freetrade file was refused. Reason is one of a closed list of words and
// Row the 1-based data row it concerns, or 0 for the file as a whole. It never carries anything from
// the file: no amount, name, ISIN or text of Details.
type FreetradeCSVError struct {
	Reason string
	Row    int
}

// The reasons a Freetrade file is refused.
const (
	ftReasonEncoding    = "not UTF-8"
	ftReasonHeader      = "header"
	ftReasonRowShape    = "row shape"
	ftReasonAccountType = "unknown account type"
	ftReasonDate        = "date"
	ftReasonOrder       = "order"
	ftReasonType        = "unknown type"
	ftReasonDirection   = "direction"
	ftReasonAmount      = "amount"
	ftReasonSign        = "contradictory sign"
	ftReasonCurrency    = "currency"
	ftReasonDetails     = "details"
	ftReasonDetailsAmt  = "details amount"
	ftReasonFXRate      = "fx rate"
)

func (e *FreetradeCSVError) Error() string {
	if e.Row == 0 {
		return "importer: freetrade csv: " + e.Reason
	}
	return "importer: freetrade csv: " + e.Reason + " at row " + strconv.Itoa(e.Row)
}

// Unwrap makes every refusal an ErrFormat.
func (e *FreetradeCSVError) Unwrap() error { return ErrFormat }

func ftErr(reason string, row int) error { return &FreetradeCSVError{Reason: reason, Row: row} }

var freetradeHeader = []string{"Value Date", "Type", "Details", "Currency", "Direction", "Value", "Account Type"}

// freetradeAccountTypes maps the printed Account Type to the ledger's product. The file names no
// account number, so the product is also the account's key. The list is closed: an ISA or general
// account is refused until a real example of its export has been seen.
var freetradeAccountTypes = map[string]string{
	"Self-Invested Personal Pension (SIPP)": ledger.ProductSIPP,
}

// freetradeOtherCurrencies are the currencies other than sterling a row may be printed in. Only buys
// may be, and only with a rate that converts them; each has two decimal places, as sterling does.
var freetradeOtherCurrencies = map[string]bool{"EUR": true}

const (
	ftDirIn  = "CASH_IN"
	ftDirOut = "CASH_OUT"
	ftGBP    = "GBP"
)

// freetradeMapping is how one Type is read.
type freetradeMapping struct {
	event string
	// sign is +1 when Value must be money in, -1 when it must be money out, 0 when either.
	sign int
	// flow says Value is an external flow as well as cash.
	flow bool
	// trade says Details is a trade line with units, an instrument, a price and an execution time.
	trade bool
	// details matches the whole of Details. Its first two groups are the amount and its currency,
	// and a third, when present, is the instrument as printed.
	details *regexp.Regexp
}

const (
	ftAmountRE = `([0-9]{1,13}(?:\.[0-9]{1,2})?)`
	ftTimeRE   = `([0-9]{2}-[A-Z][a-z]{2}-[0-9]{4} [0-9]{2}:[0-9]{2}:[0-9]{2})`
	// An instrument is its name and its ISIN in parentheses. Names hold parentheses of their own, so
	// the ISIN is the last parenthesised 12-character token.
	ftInstrumentRE = `(.+ \([A-Z]{2}[A-Z0-9]{9}[0-9]\))`
)

var freetradeTypes = map[string]freetradeMapping{
	"BUY":  {event: ledger.EventBuy, sign: -1, trade: true},
	"SELL": {event: ledger.EventSell, sign: 1, trade: true},
	"FX_FEE": {event: ledger.EventFee, sign: -1,
		details: regexp.MustCompile("^" + ftAmountRE + ` ([A-Z]{3}) fx fee taken for [0-9]{1,13}(?:\.[0-9]{1,8})? ` + ftInstrumentRE + " on " + ftTimeRE + "$")},
	"STAMP_DUTY": {event: ledger.EventFee, sign: -1,
		details: regexp.MustCompile("^" + ftAmountRE + ` ([A-Z]{3}) stamp duty paid$`)},
	"INTEREST": {event: ledger.EventInterest, sign: 1,
		details: regexp.MustCompile("^Interest income of " + ftAmountRE + ` ([A-Z]{3})$`)},
	"INVESTMENT_INCOME": {event: ledger.EventIncome, sign: 1,
		details: regexp.MustCompile("^Investment income of " + ftAmountRE + ` ([A-Z]{3})$`)},
	"SECURITIES_LENDING_INCOME": {event: ledger.EventIncome, sign: 1,
		details: regexp.MustCompile("^Securities lending income of " + ftAmountRE + ` ([A-Z]{3}) for [0-9]{4}-[0-9]{2}$`)},
	"CORPORATE_ACTION": {event: ledger.EventCorporateAction, sign: 1,
		details: regexp.MustCompile("^Corporate action of " + ftAmountRE + ` ([A-Z]{3})(?: from ` + ftInstrumentRE + ")?$")},
	"CASH_TRANSFER": {event: ledger.EventTransferIn, flow: true,
		details: regexp.MustCompile("^" + ftAmountRE + ` ([A-Z]{3}) cash transfer$`)},
}

// ftTradeRE is a BUY or SELL line: units, name, ISIN, price currency and price, an optional rate,
// the execution time and an optional requested amount.
var ftTradeRE = regexp.MustCompile(`^([0-9]{1,13}(?:\.[0-9]{1,8})?) (.+) \(([A-Z]{2}[A-Z0-9]{9}[0-9])\) @ ([A-Z]{3}) ([0-9]{1,13}(?:\.[0-9]{1,10})?)` +
	`(?: \(@ 1 ([A-Z]{3}) = ([0-9]{1,6}(?:\.[0-9]{1,10})?) ([A-Z]{3})\))? on ` + ftTimeRE +
	`(?: \(Requested [0-9]{1,13}(?:\.[0-9]{1,2})? [A-Z]{3}\))?$`)

var (
	ftValueRE    = regexp.MustCompile(`^-?[0-9]{1,13}(?:\.[0-9]{1,2})?$`)
	ftCurrencyRE = regexp.MustCompile(`^[A-Z]{3}$`)
)

const (
	ftDayLayout  = "02/01/2006"
	ftTimeLayout = "02-Jan-2006 15:04:05"
)

// freetradeRowContent is the stored content of one row and the input of its version hash: the row
// as printed, in key order, so its JSON encoding is canonical and the same row gives the same bytes
// in every export.
type freetradeRowContent struct {
	AccountType string `json:"account_type"`
	Currency    string `json:"currency"`
	Details     string `json:"details"`
	Direction   string `json:"direction"`
	Type        string `json:"type"`
	Value       string `json:"value"`
	ValueDate   string `json:"value_date"`
}

// NormaliseInvestmentFile reads an activity statement: the exact header and one row per cash
// movement, newest first. Every row becomes one event of the one account its Account Type names; a
// file that breaks any rule, however late in it, is refused whole. A row has no reference, so its
// key is the hex SHA-256 of its seven fields as printed, joined with U+001F, then "#" and its
// 1-based position among the identical rows of the file.
func (FreetradeActivityNormaliser) NormaliseInvestmentFile(f ledger.File, _ *time.Location) (ledger.InvestmentResult, error) {
	data := bytes.TrimPrefix(f.Body, []byte(fidelityBOM))
	if !utf8.Valid(data) {
		return ledger.InvestmentResult{}, ftErr(ftReasonEncoding, 0)
	}
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil || !slices.Equal(header, freetradeHeader) {
		return ledger.InvestmentResult{}, ftErr(ftReasonHeader, 0)
	}
	res := ledger.InvestmentResult{Kind: ledger.InvestmentKindLedger}
	accounts := map[string]bool{}
	seen := map[[sha256.Size]byte]int{}
	var previous time.Time
	for n := 1; ; n++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || len(rec) != len(freetradeHeader) {
			return ledger.InvestmentResult{}, ftErr(ftReasonRowShape, n)
		}
		ev, day, err := freetradeEvent(rec, n)
		if err != nil {
			return ledger.InvestmentResult{}, err
		}
		// Rows are newest first: a value date later than the row above is another file's shape.
		if n > 1 && day.After(previous) {
			return ledger.InvestmentResult{}, ftErr(ftReasonOrder, n)
		}
		previous = day
		sum := sha256.Sum256([]byte(strings.Join(rec, keySep)))
		seen[sum]++
		ev.SourceKey = hex.EncodeToString(sum[:]) + "#" + strconv.Itoa(seen[sum])
		if !accounts[ev.AccountKey] {
			accounts[ev.AccountKey] = true
			res.Accounts = append(res.Accounts, ledger.InvestmentAccountRec{AccountKey: ev.AccountKey, Product: ev.AccountKey})
		}
		res.Events = append(res.Events, ev)
	}
	res.RowCount = len(res.Events)
	return res, nil
}

// freetradeEvent reads one data row, all but its key, and returns its value date.
func freetradeEvent(rec []string, row int) (ledger.InvestmentEventRec, time.Time, error) {
	var ev ledger.InvestmentEventRec
	c := freetradeRowContent{ValueDate: rec[0], Type: rec[1], Details: rec[2], Currency: rec[3], Direction: rec[4], Value: rec[5], AccountType: rec[6]}
	product, ok := freetradeAccountTypes[c.AccountType]
	if !ok {
		return ev, time.Time{}, ftErr(ftReasonAccountType, row)
	}
	day, err := time.Parse(ftDayLayout, c.ValueDate)
	if err != nil {
		return ev, time.Time{}, ftErr(ftReasonDate, row)
	}
	m, ok := freetradeTypes[c.Type]
	if !ok {
		return ev, time.Time{}, ftErr(ftReasonType, row)
	}
	if c.Direction != ftDirIn && c.Direction != ftDirOut {
		return ev, time.Time{}, ftErr(ftReasonDirection, row)
	}
	if !ftValueRE.MatchString(c.Value) {
		return ev, time.Time{}, ftErr(ftReasonAmount, row)
	}
	value, err := ftMinor(c.Value)
	if err != nil {
		return ev, time.Time{}, ftErr(ftReasonAmount, row)
	}
	in := c.Direction == ftDirIn
	if value == 0 || (value > 0) != in || (m.sign > 0 && !in) || (m.sign < 0 && in) {
		return ev, time.Time{}, ftErr(ftReasonSign, row)
	}
	if !ftCurrencyRE.MatchString(c.Currency) || (c.Currency != ftGBP && (c.Type != "BUY" || !freetradeOtherCurrencies[c.Currency])) {
		return ev, time.Time{}, ftErr(ftReasonCurrency, row)
	}

	ev = ledger.InvestmentEventRec{AccountKey: product, EventType: m.event, ProviderType: c.Type, OrderDate: day,
		GrossMinor: value, CashMinor: value, Currency: c.Currency}
	if c.Type == "CASH_TRANSFER" && !in {
		ev.EventType = ledger.EventTransferOut
	}
	if m.flow {
		ev.FlowMinor = value
	}
	if m.trade {
		if err := freetradeTrade(&ev, c, day, value, row); err != nil {
			return ev, time.Time{}, err
		}
	} else {
		g := m.details.FindStringSubmatch(c.Details)
		if g == nil {
			return ev, time.Time{}, ftErr(ftReasonDetails, row)
		}
		amount, err := ftMinor(g[1])
		if err != nil || amount != abs64(value) || g[2] != c.Currency {
			return ev, time.Time{}, ftErr(ftReasonDetailsAmt, row)
		}
		if len(g) > 3 {
			ev.InstrumentName = g[3]
		}
	}
	b, err := json.Marshal(c)
	if err != nil {
		return ev, time.Time{}, ftErr(ftReasonRowShape, row)
	}
	ev.Content, ev.HashInput = b, b
	return ev, day, nil
}

// freetradeTrade reads a BUY or SELL line into ev: its units and instrument, its execution date as
// the order date and the value date as the settle date, and the sterling cash of a row printed in
// another currency. The price is kept only when it is in pounds and units times price agrees with
// the printed value: the export prints some pence prices as pounds, and those stay in the content.
func freetradeTrade(ev *ledger.InvestmentEventRec, c freetradeRowContent, valueDay time.Time, value int64, row int) error {
	g := ftTradeRE.FindStringSubmatch(c.Details)
	if g == nil {
		return ftErr(ftReasonDetails, row)
	}
	units, name, isin, priceCurrency, price := g[1], g[2], g[3], g[4], g[5]
	rateFrom, rate, rateTo, executed := g[6], g[7], g[8], g[9]
	at, err := time.Parse(ftTimeLayout, executed)
	if err != nil {
		return ftErr(ftReasonDate, row)
	}
	order := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
	if order.After(valueDay) {
		return ftErr(ftReasonDate, row)
	}
	ev.Units, ev.InstrumentKey, ev.InstrumentName = units, isin, name+" ("+isin+")"
	ev.OrderDate, ev.SettleDate = order, valueDay

	if c.Currency != ftGBP {
		// Value is in the row's currency, and the line must give that currency per pound as
		// "1 GBP = r CCY". The other form is printed with the rate the wrong way round on some
		// sterling lines, so it is not trusted to convert cash.
		if priceCurrency != c.Currency || rateFrom != ftGBP || rateTo != c.Currency {
			return ftErr(ftReasonFXRate, row)
		}
		cash, ok := ftConvert(value, rate)
		if !ok {
			return ftErr(ftReasonFXRate, row)
		}
		ev.CashMinor = cash
		return nil
	}
	// A sterling row: the rate only describes the price. It is present exactly when the price is in
	// another currency, and names that currency against sterling.
	namesPrice := (rateFrom == ftGBP && rateTo == priceCurrency) || (rateFrom == priceCurrency && rateTo == ftGBP)
	if (priceCurrency == ftGBP && rateFrom != "") || (priceCurrency != ftGBP && !namesPrice) {
		return ftErr(ftReasonFXRate, row)
	}
	if priceCurrency == ftGBP && ftPriceAgrees(units, price, abs64(value)) {
		ev.Price, ev.PriceUnit = price, ledger.PriceUnitGBP
	}
	return nil
}

// ftConvert is value, in minor units of another currency, divided by rate (that currency per
// pound) and rounded to the penny, half away from zero. ok is false for a zero rate.
func ftConvert(value int64, rate string) (int64, bool) {
	r, ok := new(big.Rat).SetString(rate)
	if !ok || r.Sign() <= 0 {
		return 0, false
	}
	q := new(big.Rat).Quo(new(big.Rat).SetInt64(abs64(value)), r)
	whole, rem := new(big.Int).QuoRem(q.Num(), q.Denom(), new(big.Int))
	if new(big.Int).Mul(rem, big.NewInt(2)).Cmp(q.Denom()) >= 0 {
		whole.Add(whole, big.NewInt(1))
	}
	if !whole.IsInt64() {
		return 0, false
	}
	if value < 0 {
		return -whole.Int64(), true
	}
	return whole.Int64(), true
}

// ftPriceAgrees reports whether units times a price in pounds equals valuePence. The units are an
// exact fill and the value is rounded to the penny; the price is rounded to its printed decimals.
// So |u*p - v| may be u times half the price's last printed place, in pence, plus half a penny.
func ftPriceAgrees(units, price string, valuePence int64) bool {
	u, okU := new(big.Rat).SetString(units)
	p, okP := new(big.Rat).SetString(price)
	if !okU || !okP {
		return false
	}
	_, frac, _ := strings.Cut(price, ".")
	p.Mul(p, big.NewRat(100, 1))
	tol := new(big.Rat).Mul(u, avivaPow10(2-len(frac)))
	tol.Quo(tol, big.NewRat(2, 1))
	tol.Add(tol, big.NewRat(1, 2))
	diff := new(big.Rat).Sub(new(big.Rat).Mul(u, p), new(big.Rat).SetInt64(valuePence))
	return diff.Abs(diff).Cmp(tol) <= 0
}

// ftMinor reads a decimal with at most two places, signed or not, into minor units.
func ftMinor(s string) (int64, error) {
	neg := strings.HasPrefix(s, "-")
	whole, frac, _ := strings.Cut(strings.TrimPrefix(s, "-"), ".")
	if !allDigits(whole) || len(whole) > maxIntegerDigits || len(frac) > 2 || (frac != "" && !allDigits(frac)) {
		return 0, ErrFormat
	}
	n, err := strconv.ParseInt(whole+frac+strings.Repeat("0", 2-len(frac)), 10, 64)
	if err != nil {
		return 0, ErrFormat
	}
	if neg {
		n = -n
	}
	return n, nil
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

package importer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rknightion/finstore/internal/ledger"
)

// FormatAvivaCapture is the format of the JSON an Aviva capture session reads from the provider's
// pages, as `finstore import --format` spells it.
const FormatAvivaCapture Format = "aviva-capture-json"

// The checks ParseAvivaCapture runs on a capture's figures. A failure is a *CheckError: Row is the
// 1-based fund the check broke on, or 0 when it concerns the capture as a whole.
const (
	// CheckAvivaValue: a fund's units times its price equal its value, within what rounding of the
	// printed units and price allows, plus half a penny.
	CheckAvivaValue CheckName = "aviva-value"
	// CheckAvivaTotal: the funds' values equal the printed total, to within one penny per fund.
	CheckAvivaTotal CheckName = "aviva-total"
	// CheckAvivaContributions: the contributions add up to the printed contributions total.
	CheckAvivaContributions CheckName = "aviva-contributions-total"
)

// AvivaSchemaVersion is the one schema_version a capture may carry.
const AvivaSchemaVersion = 1

// Payers a contribution may name.
const (
	AvivaPayerEmployer = "employer"
	AvivaPayerMember   = "member"
)

// AvivaCapture is one parsed capture. Money is in pence. Units and prices stay the decimal strings
// the file printed, because the ledger parses them exactly.
type AvivaCapture struct {
	// CapturedAt is the printed capture time, in UTC.
	CapturedAt time.Time
	// AsOf is the day the figures are for, midnight UTC.
	AsOf    time.Time
	PlanRef string

	TotalMinor int64
	Funds      []AvivaFund

	// ContributionsComplete says the list is the plan's whole contribution history.
	ContributionsComplete bool
	ContributionsMinor    int64
	Contributions         []AvivaContribution

	// ValueHistory is the provider's past-value table. It is checked for shape and kept in raw; it
	// produces no core rows.
	ValueHistory []AvivaValuePoint
}

// AvivaFund is one holding of the plan.
type AvivaFund struct {
	// Name is the fund name as printed, with surrounding space trimmed and inner space collapsed.
	Name              string
	Units             string
	Price             string // pounds
	ValueMinor        int64
	AllocationPercent string
}

// AvivaContribution is one dated payment into the plan.
type AvivaContribution struct {
	Date       time.Time
	Type       string
	Payer      string // AvivaPayerEmployer or AvivaPayerMember
	GrossMinor int64
	// Key is the row's import key, set by the parser: see avivaKeys.
	Key string
}

// AvivaValuePoint is one row of the provider's past-value table.
type AvivaValuePoint struct {
	Date                time.Time
	ContributionsToDate int64
	ValueMinor          int64
}

// avivaObjects lists, for each place the schema has an object, the keys it may carry, all required.
// The path is "" for the file itself and the array's key for one of its elements.
var avivaObjects = map[string][]string{
	"": {"schema_version", "captured_at", "as_of", "plan_ref", "total_value", "funds", "contributions_complete",
		"contributions_total", "contributions", "value_history"},
	"funds":         {"name", "units", "price", "value", "allocation_percent"},
	"contributions": {"date", "type", "payer", "gross"},
	"value_history": {"date", "contributions_to_date", "value"},
}

type avivaRaw struct {
	SchemaVersion         *int                    `json:"schema_version"`
	CapturedAt            *string                 `json:"captured_at"`
	AsOf                  *string                 `json:"as_of"`
	PlanRef               *string                 `json:"plan_ref"`
	TotalValue            *string                 `json:"total_value"`
	Funds                 *[]avivaRawFund         `json:"funds"`
	ContributionsComplete *bool                   `json:"contributions_complete"`
	ContributionsTotal    *string                 `json:"contributions_total"`
	Contributions         *[]avivaRawContribution `json:"contributions"`
	ValueHistory          *[]avivaRawPoint        `json:"value_history"`
}

type avivaRawFund struct {
	Name              *string `json:"name"`
	Units             *string `json:"units"`
	Price             *string `json:"price"`
	Value             *string `json:"value"`
	AllocationPercent *string `json:"allocation_percent"`
}

type avivaRawContribution struct {
	Date  *string `json:"date"`
	Type  *string `json:"type"`
	Payer *string `json:"payer"`
	Gross *string `json:"gross"`
}

type avivaRawPoint struct {
	Date                *string `json:"date"`
	ContributionsToDate *string `json:"contributions_to_date"`
	Value               *string `json:"value"`
}

// avivaErr is an error that says only which kind of fault a file has. It never carries anything
// from the file: no name, no amount, no plan reference and no text of the JSON decoder.
func avivaErr(what string) error {
	return fmt.Errorf("%w: aviva capture: %s", ErrFormat, what)
}

// ParseAvivaCapture parses and checks an aviva-capture-json file, schema version 1: one JSON
// object whose keys are exactly those of the schema, each present once, with every amount, unit and
// price a decimal string. An unknown or repeated key rejects the file, at any depth. Money has
// exactly two decimals; units and prices at most ten. Dates are YYYY-MM-DD and the capture time is
// RFC 3339.
//
// The figures must pass three checks, each reported as a *CheckError: the Value of each fund, the
// Total of the funds against total_value, and the contributions against contributions_total. The
// last applies whether or not the list is declared complete: the total printed with a partial view
// is the total of that view. A file that is not valid in every other respect is rejected with an
// error wrapping ErrFormat. No error carries file content.
func ParseAvivaCapture(data []byte) (*AvivaCapture, error) {
	if err := avivaCheckShape(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var raw avivaRaw
	if err := dec.Decode(&raw); err != nil {
		return nil, avivaErr("a field has the wrong type")
	}
	if raw.SchemaVersion == nil || raw.CapturedAt == nil || raw.AsOf == nil || raw.PlanRef == nil || raw.TotalValue == nil ||
		raw.Funds == nil || raw.ContributionsComplete == nil || raw.ContributionsTotal == nil || raw.Contributions == nil ||
		raw.ValueHistory == nil {
		return nil, avivaErr("a field is missing")
	}
	if *raw.SchemaVersion != AvivaSchemaVersion {
		return nil, avivaErr("unsupported schema version")
	}
	c := &AvivaCapture{ContributionsComplete: *raw.ContributionsComplete}
	var err error
	if c.CapturedAt, err = time.Parse(time.RFC3339, *raw.CapturedAt); err != nil {
		return nil, avivaErr("bad capture time")
	}
	c.CapturedAt = c.CapturedAt.UTC()
	if c.AsOf, err = avivaDay(*raw.AsOf); err != nil {
		return nil, err
	}
	if c.PlanRef, err = avivaPlanRef(*raw.PlanRef); err != nil {
		return nil, err
	}
	if c.TotalMinor, err = avivaMoney(*raw.TotalValue); err != nil {
		return nil, err
	}
	if c.ContributionsMinor, err = avivaMoney(*raw.ContributionsTotal); err != nil {
		return nil, err
	}

	names := map[string]bool{}
	for _, f := range *raw.Funds {
		if f.Name == nil || f.Units == nil || f.Price == nil || f.Value == nil || f.AllocationPercent == nil {
			return nil, avivaErr("a fund field is missing")
		}
		name := ledger.NormaliseFundName(*f.Name)
		if name == "" || len(name) > 200 || !avivaPlain(name) || names[name] {
			return nil, avivaErr("bad fund name")
		}
		names[name] = true
		if _, _, err := avivaDecimal(*f.Units); err != nil {
			return nil, err
		}
		if _, _, err := avivaDecimal(*f.Price); err != nil {
			return nil, err
		}
		if _, _, err := avivaDecimal(*f.AllocationPercent); err != nil {
			return nil, err
		}
		value, err := avivaMoney(*f.Value)
		if err != nil {
			return nil, err
		}
		c.Funds = append(c.Funds, AvivaFund{Name: name, Units: *f.Units, Price: *f.Price, ValueMinor: value,
			AllocationPercent: *f.AllocationPercent})
	}

	for _, r := range *raw.Contributions {
		if r.Date == nil || r.Type == nil || r.Payer == nil || r.Gross == nil {
			return nil, avivaErr("a contribution field is missing")
		}
		row := AvivaContribution{Type: strings.TrimSpace(*r.Type), Payer: *r.Payer}
		if row.Date, err = avivaDay(*r.Date); err != nil {
			return nil, err
		}
		if row.Type == "" || len(row.Type) > 100 || !avivaPlain(row.Type) {
			return nil, avivaErr("bad contribution type")
		}
		if row.Payer != AvivaPayerEmployer && row.Payer != AvivaPayerMember {
			return nil, avivaErr("bad contribution payer")
		}
		if row.GrossMinor, err = avivaMoney(*r.Gross); err != nil {
			return nil, err
		}
		if row.GrossMinor <= 0 {
			return nil, avivaErr("a contribution is not positive")
		}
		c.Contributions = append(c.Contributions, row)
	}
	avivaKeys(c.Contributions)

	for _, p := range *raw.ValueHistory {
		if p.Date == nil || p.ContributionsToDate == nil || p.Value == nil {
			return nil, avivaErr("a value history field is missing")
		}
		var point AvivaValuePoint
		if point.Date, err = avivaDay(*p.Date); err != nil {
			return nil, err
		}
		if point.ContributionsToDate, err = avivaMoney(*p.ContributionsToDate); err != nil {
			return nil, err
		}
		if point.ValueMinor, err = avivaMoney(*p.Value); err != nil {
			return nil, err
		}
		c.ValueHistory = append(c.ValueHistory, point)
	}

	if err := avivaCheck(c); err != nil {
		return nil, err
	}
	return c, nil
}

// avivaCheckShape walks the JSON once, token by token, and rejects what the typed decode cannot
// see: a key of another case, a repeated key, an unknown key and trailing data after the object.
func avivaCheckShape(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return avivaErr("not JSON")
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return avivaErr("not a JSON object")
	}
	if err := avivaWalkObject(dec, ""); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return avivaErr("data after the object")
	}
	return nil
}

// avivaWalkObject reads an object whose opening brace is consumed, through its closing brace.
func avivaWalkObject(dec *json.Decoder, path string) error {
	allowed, known := avivaObjects[path]
	if !known {
		return avivaErr("an unexpected object")
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return avivaErr("not JSON")
		}
		key, _ := tok.(string)
		if !slices.Contains(allowed, key) {
			return avivaErr("an unknown field")
		}
		if seen[key] {
			return avivaErr("a repeated field")
		}
		seen[key] = true
		child := key
		if path != "" {
			child = path + "." + key
		}
		if err := avivaWalkValue(dec, child); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return avivaErr("not JSON")
	}
	return nil
}

// avivaWalkValue reads one value. An array's elements are at the array's own path.
func avivaWalkValue(dec *json.Decoder, path string) error {
	tok, err := dec.Token()
	if err != nil {
		return avivaErr("not JSON")
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if d == '{' {
		return avivaWalkObject(dec, path)
	}
	for dec.More() {
		if err := avivaWalkValue(dec, path); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return avivaErr("not JSON")
	}
	return nil
}

// avivaDay parses YYYY-MM-DD as midnight UTC.
func avivaDay(s string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, avivaErr("bad date")
	}
	return t, nil
}

// avivaPlain reports whether s is valid UTF-8 with no control character.
func avivaPlain(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// avivaPlanRef checks the plan reference. The ledger takes the last four letters and digits of it
// for the child account's slug, so it must have four.
func avivaPlanRef(s string) (string, error) {
	n := 0
	for _, r := range s {
		if r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			n++
		}
	}
	if s == "" || len(s) > 64 || s != strings.TrimSpace(s) || !avivaPlain(s) || n < 4 {
		return "", avivaErr("bad plan reference")
	}
	return s, nil
}

const (
	avivaMaxWhole = 13 // digits before the point of an amount
	avivaMaxFrac  = 10 // digits after the point of a unit or a price
)

// avivaMoney parses an amount in pounds into pence: ASCII digits, a point and exactly two decimals.
// There is no sign, no space and no thousands separator.
func avivaMoney(s string) (int64, error) {
	whole, frac, ok := strings.Cut(s, ".")
	if !ok || len(frac) != 2 || len(whole) == 0 || len(whole) > avivaMaxWhole || !allDigits(whole) || !allDigits(frac) {
		return 0, avivaErr("bad amount")
	}
	n, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return 0, avivaErr("bad amount")
	}
	return n, nil
}

// avivaDecimal parses an unsigned decimal: ASCII digits and, optionally, a point and at least one
// decimal. It returns the value exactly and the number of decimals printed.
func avivaDecimal(s string) (*big.Rat, int, error) {
	whole, frac, hasPoint := strings.Cut(s, ".")
	if len(whole) == 0 || len(whole) > avivaMaxWhole || !allDigits(whole) ||
		(hasPoint && (len(frac) == 0 || len(frac) > avivaMaxFrac || !allDigits(frac))) {
		return nil, 0, avivaErr("bad decimal")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, 0, avivaErr("bad decimal")
	}
	return r, len(frac), nil
}

// avivaPow10 is 10^n for a positive or negative n.
func avivaPow10(n int) *big.Rat {
	p := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(max(n, -n))), nil)
	if n >= 0 {
		return new(big.Rat).SetInt(p)
	}
	return new(big.Rat).SetFrac(big.NewInt(1), p)
}

// avivaCheck runs the Value, Total and contributions checks.
func avivaCheck(c *AvivaCapture) error {
	// Value. With p the price in pence, v the value in pence, u the units, du the printed decimals
	// of the units and dp those of the price in pence (the price's printed decimals less 2, so it
	// may be negative), the units and price are each known to half a unit of their last place, so
	// |u*p - v| may be as much as u*10^-dp/2 + p*10^-du/2 + 10^-dp*10^-du/4, and v itself is a
	// rounded value, half a penny.
	for i, f := range c.Funds {
		u, du, err := avivaDecimal(f.Units)
		if err != nil {
			return err
		}
		price, pp, err := avivaDecimal(f.Price)
		if err != nil {
			return err
		}
		p := new(big.Rat).Mul(price, big.NewRat(100, 1))
		dp := pp - 2
		diff := new(big.Rat).Sub(new(big.Rat).Mul(u, p), new(big.Rat).SetInt64(f.ValueMinor))
		diff.Abs(diff)
		tol := new(big.Rat).Mul(u, avivaPow10(-dp))
		tol.Add(tol, new(big.Rat).Mul(p, avivaPow10(-du)))
		tol.Quo(tol, big.NewRat(2, 1))
		tol.Add(tol, new(big.Rat).Mul(avivaPow10(-dp), new(big.Rat).Mul(avivaPow10(-du), big.NewRat(1, 4))))
		tol.Add(tol, big.NewRat(1, 2))
		if diff.Cmp(tol) > 0 {
			return &CheckError{Check: CheckAvivaValue, Row: i + 1}
		}
	}

	// Total: the plan holds no cash, so its funds make up the total.
	sum := new(big.Int)
	for _, f := range c.Funds {
		sum.Add(sum, big.NewInt(f.ValueMinor))
	}
	sum.Sub(sum, big.NewInt(c.TotalMinor))
	if sum.CmpAbs(big.NewInt(int64(len(c.Funds)))) > 0 {
		return &CheckError{Check: CheckAvivaTotal}
	}

	// Contributions: exact.
	paid := new(big.Int)
	for _, r := range c.Contributions {
		paid.Add(paid, big.NewInt(r.GrossMinor))
	}
	if paid.Cmp(big.NewInt(c.ContributionsMinor)) != 0 {
		return &CheckError{Check: CheckAvivaContributions}
	}
	return nil
}

// avivaKeys sets every contribution's import key, in the pattern of Keys: the hex SHA-256 of the
// row's date as YYYY-MM-DD, type, payer and gross in decimal pence, joined with U+001F, then "#" and
// the row's 1-based position among the rows identical in all four. The same payment in two captures
// gets the same key, wherever it sits in the list. The hash is unkeyed, so a key holds no
// plaintext amount or payer, but anyone holding a key can test a guessed date, type, payer and
// amount against it.
func avivaKeys(rows []AvivaContribution) {
	seen := make(map[[sha256.Size]byte]int, len(rows))
	for i := range rows {
		r := &rows[i]
		fields := []string{r.Date.Format("2006-01-02"), r.Type, r.Payer, pence(r.GrossMinor)}
		sum := sha256.Sum256([]byte(strings.Join(fields, keySep)))
		seen[sum]++
		r.Key = hex.EncodeToString(sum[:]) + "#" + strconv.Itoa(seen[sum])
	}
}

// AvivaCaptureNormaliser adapts ParseAvivaCapture to ledger.InvestmentNormaliser. Register it under
// FormatAvivaCapture. It is a pure function of the file's bytes: it reads no clock and no zone, as
// the capture time carries its own offset.
type AvivaCaptureNormaliser struct{}

var _ ledger.InvestmentNormaliser = AvivaCaptureNormaliser{}

// Provider is ledger.ProviderAviva.
func (AvivaCaptureNormaliser) Provider() string { return ledger.ProviderAviva }

// avivaEventContent is the stored content of one contribution and the input of its version hash.
// Its fields are in key order, so its JSON encoding is canonical.
type avivaEventContent struct {
	Date       string `json:"date"`
	GrossMinor int64  `json:"gross_minor"`
	Payer      string `json:"payer"`
	Type       string `json:"type"`
}

// NormaliseInvestmentFile parses and checks a capture and returns it as an InvestmentKindCapture
// result: one plan account, a holding for each fund, one valuation, a contribution event for each
// contribution, and the plan named in CompleteContributionsFor when the list is declared complete.
// A contribution has no cash effect and its flow is its gross amount. value_history gives no
// record: it stays in raw. RowCount counts every fund, contribution and value history row. The
// error never carries file content.
func (AvivaCaptureNormaliser) NormaliseInvestmentFile(f ledger.File, _ *time.Location) (ledger.InvestmentResult, error) {
	if Format(f.Format) != FormatAvivaCapture {
		return ledger.InvestmentResult{}, fmt.Errorf("%w: unknown format", ErrFormat)
	}
	c, err := ParseAvivaCapture(f.Body)
	if err != nil {
		return ledger.InvestmentResult{}, err
	}
	captured := c.CapturedAt
	res := ledger.InvestmentResult{
		Kind:       ledger.InvestmentKindCapture,
		CapturedAt: &captured,
		Accounts:   []ledger.InvestmentAccountRec{{AccountKey: c.PlanRef, Product: ledger.ProductPensionPlan}},
		Valuations: []ledger.ValuationRec{{AccountKey: c.PlanRef, AsOf: c.AsOf, TotalMinor: c.TotalMinor}},
		RowCount:   len(c.Funds) + len(c.Contributions) + len(c.ValueHistory),
	}
	for _, fund := range c.Funds {
		res.Holdings = append(res.Holdings, ledger.HoldingRec{
			AccountKey:     c.PlanRef,
			InstrumentKey:  fund.Name,
			InstrumentName: fund.Name,
			AsOf:           c.AsOf,
			Units:          fund.Units,
			Price:          fund.Price,
			PriceUnit:      ledger.PriceUnitGBP,
			ValueMinor:     fund.ValueMinor,
		})
	}
	for _, r := range c.Contributions {
		content, err := json.Marshal(avivaEventContent{Date: r.Date.Format("2006-01-02"), GrossMinor: r.GrossMinor,
			Payer: r.Payer, Type: r.Type})
		if err != nil {
			return ledger.InvestmentResult{}, fmt.Errorf("importer: encode contribution: %w", err)
		}
		res.Events = append(res.Events, ledger.InvestmentEventRec{
			AccountKey:   c.PlanRef,
			SourceKey:    r.Key,
			EventType:    ledger.EventContribution,
			ProviderType: r.Type,
			OrderDate:    r.Date,
			GrossMinor:   r.GrossMinor,
			FlowMinor:    r.GrossMinor,
			Content:      content,
			HashInput:    content,
		})
	}
	if c.ContributionsComplete {
		res.CompleteContributionsFor = []string{c.PlanRef}
	}
	return res, nil
}

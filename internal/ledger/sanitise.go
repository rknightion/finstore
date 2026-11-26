package ledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// PostgreSQL text and jsonb hold neither U+0000 nor invalid UTF-8, and refuse them with an error
// that would stop the projection at that row for good. Every record is therefore sanitised as it
// leaves its normaliser, before any id is derived from it or anything is written: U+0000 is
// removed from text fields, invalid UTF-8 becomes U+FFFD, and in stored JSON content each U+0000
// and each lone surrogate escape (which jsonb refuses) becomes U+FFFD. These are pure functions,
// so replay gives the same rows as the live path. The exact bytes stay in raw. Version hashes are
// unaffected: they are computed over HashInput, which is never stored as text.

// cleanText returns s as valid UTF-8 (each invalid sequence becomes U+FFFD) with no U+0000.
func cleanText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "")
}

// needsClean matches the escapes cleanJSON has to look at: U+0000, and any surrogate (D800 to
// DFFF). A surrogate pair is valid and survives the re-encode as its character; the decoder turns a
// lone one into U+FFFD.
var needsClean = regexp.MustCompile(`\\u(0000|[dD][89a-fA-F][0-9a-fA-F]{2})`)

// cleanJSON returns content with every U+0000 in its strings and keys replaced by U+FFFD, every lone
// surrogate escape replaced by U+FFFD and any invalid UTF-8 replaced by U+FFFD. Content that needs
// none of these is returned unchanged; otherwise it is decoded and re-encoded (numbers kept as
// written). Content that does not decode as exactly one JSON value is returned unchanged, for the
// caller's own check to refuse.
func cleanJSON(content json.RawMessage) json.RawMessage {
	// A false positive (an escaped backslash before "u0000") only costs a re-encode that changes
	// nothing in meaning; the decoder, not this test, decides what a string holds.
	if utf8.Valid(content) && !needsClean.Match(content) {
		return content
	}
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return content
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return content
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(cleanValue(v)); err != nil {
		return content
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func cleanValue(v any) any {
	switch x := v.(type) {
	case string:
		return strings.ReplaceAll(x, "\x00", "�")
	case []any:
		for i := range x {
			x[i] = cleanValue(x[i])
		}
		return x
	case map[string]any:
		// Keys in sorted order, so that two keys that clean to the same one resolve the same way
		// every time.
		out := make(map[string]any, len(x))
		for _, k := range slices.Sorted(maps.Keys(x)) {
			out[strings.ReplaceAll(k, "\x00", "�")] = cleanValue(x[k])
		}
		return out
	}
	return v
}

func sanitiseResult(r Result) Result {
	out := Result{
		Accounts:       make([]AccountRec, len(r.Accounts)),
		Txns:           sanitiseTxns(r.Txns),
		Balances:       sanitiseBalances(r.Balances),
		StandingOrders: make([]StandingOrderRec, len(r.StandingOrders)),
		DirectDebits:   make([]DirectDebitRec, len(r.DirectDebits)),
		MandateLists:   make([]MandateList, len(r.MandateLists)),
		Pending:        make([]PendingSnapshot, len(r.Pending)),
		CardLists:      make([]CardList, len(r.CardLists)),
	}
	for i, a := range r.Accounts {
		a.SourceKey = cleanText(a.SourceKey)
		a.Type = cleanText(a.Type)
		a.Currency = cleanText(a.Currency)
		a.Name = cleanText(a.Name)
		a.ParentSourceKey = cleanText(a.ParentSourceKey)
		a.BankRef = cleanText(a.BankRef)
		a.Content = cleanJSON(a.Content)
		out.Accounts[i] = a
	}
	for i, m := range r.StandingOrders {
		for _, f := range []*string{&m.BankRef, &m.MandateKey, &m.Payee, &m.Reference, &m.Frequency, &m.Status, &m.Currency} {
			*f = cleanText(*f)
		}
		m.Content = cleanJSON(m.Content)
		out.StandingOrders[i] = m
	}
	for i, m := range r.DirectDebits {
		for _, f := range []*string{&m.BankRef, &m.MandateKey, &m.Name, &m.Status, &m.Currency} {
			*f = cleanText(*f)
		}
		m.Content = cleanJSON(m.Content)
		out.DirectDebits[i] = m
	}
	for i, l := range r.MandateLists {
		out.MandateLists[i] = MandateList{Kind: cleanText(l.Kind), BankRef: cleanText(l.BankRef)}
	}
	for i, p := range r.Pending {
		p.AccountSourceKey = cleanText(p.AccountSourceKey)
		rows := make([]PendingRec, len(p.Rows))
		for j, row := range p.Rows {
			for _, f := range []*string{&row.ID, &row.Currency, &row.Description, &row.MerchantName} {
				*f = cleanText(*f)
			}
			row.Content = cleanJSON(row.Content)
			rows[j] = row
		}
		p.Rows = rows
		out.Pending[i] = p
	}
	for i, l := range r.CardLists {
		keys := make([]string, len(l.Keys))
		for j, k := range l.Keys {
			keys[j] = cleanText(k)
		}
		out.CardLists[i] = CardList{Source: cleanText(l.Source), Keys: keys}
	}
	return out
}

func sanitiseFileResult(r FileResult) FileResult {
	r.Txns = sanitiseTxns(r.Txns)
	r.Balances = sanitiseBalances(r.Balances)
	r.AccountRef = cleanText(r.AccountRef)
	return r
}

func sanitiseTxns(ts []TxnRec) []TxnRec {
	out := make([]TxnRec, len(ts))
	for i, t := range ts {
		for _, f := range []*string{&t.AccountSourceKey, &t.SourceKey, &t.Currency, &t.LocalCurrency, &t.Description,
			&t.Counterparty, &t.MerchantID, &t.MerchantName, &t.Category, &t.Scheme, &t.Notes} {
			*f = cleanText(*f)
		}
		t.Status = Status(cleanText(string(t.Status)))
		t.Content = cleanJSON(t.Content)
		out[i] = t
	}
	return out
}

func sanitiseBalances(bs []BalanceRec) []BalanceRec {
	out := make([]BalanceRec, len(bs))
	for i, b := range bs {
		b.AccountSourceKey = cleanText(b.AccountSourceKey)
		b.Currency = cleanText(b.Currency)
		out[i] = b
	}
	return out
}

// decimalRE is an unsigned decimal as units and prices are carried.
var decimalRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

func validDecimal(s string) bool { return decimalRE.MatchString(s) }

// isDay reports whether t is a calendar day at midnight UTC.
func isDay(t time.Time) bool {
	return !t.IsZero() && t.Location() == time.UTC && t.Equal(t.Truncate(24*time.Hour))
}

// NormaliseFundName is a fund name as an instrument key: surrounding space trimmed and inner space
// collapsed. Aviva's provider_key is this.
func NormaliseFundName(name string) string { return strings.Join(strings.Fields(name), " ") }

// sanitiseInvestmentResult cleans every text and JSON field of r and checks the rules on
// InvestmentResult, for a normaliser of provider. ok is false when r breaks one: the file is
// rejected as bad_result before anything is written.
func sanitiseInvestmentResult(provider string, r InvestmentResult) (out InvestmentResult, ok bool) {
	p, known := investmentProviders[provider]
	if !known || r.RowCount < 0 {
		return out, false
	}
	switch r.Kind {
	case InvestmentKindLedger, InvestmentKindStatement, InvestmentKindTradeNote:
	case InvestmentKindHoldings, InvestmentKindCapture:
		if r.CapturedAt == nil || r.CapturedAt.IsZero() {
			return out, false
		}
	default:
		return out, false
	}
	allowed := func(kinds ...string) bool { return slices.Contains(kinds, r.Kind) }
	if (len(r.Events) > 0 && !allowed(InvestmentKindLedger, InvestmentKindCapture, InvestmentKindStatement)) ||
		((len(r.Holdings) > 0 || len(r.Valuations) > 0) && !allowed(InvestmentKindHoldings, InvestmentKindCapture)) ||
		(len(r.Statements) > 0 && !allowed(InvestmentKindStatement)) ||
		(len(r.TradeNotes) > 0 && !allowed(InvestmentKindTradeNote)) ||
		(len(r.Accounts) > 0 && allowed(InvestmentKindTradeNote)) ||
		(len(r.CompleteContributionsFor) > 0 && !allowed(InvestmentKindCapture)) {
		return out, false
	}

	out = InvestmentResult{Kind: r.Kind, CapturedAt: r.CapturedAt, RowCount: r.RowCount,
		Accounts:   slices.Clone(r.Accounts),
		Events:     slices.Clone(r.Events),
		Holdings:   slices.Clone(r.Holdings),
		Valuations: slices.Clone(r.Valuations),
		Statements: slices.Clone(r.Statements),
		TradeNotes: slices.Clone(r.TradeNotes),
	}
	accounts := make(map[string]bool, len(out.Accounts))
	for i := range out.Accounts {
		a := &out.Accounts[i]
		a.AccountKey, a.Product = cleanText(a.AccountKey), cleanText(a.Product)
		if a.AccountKey == "" || accounts[a.AccountKey] || !slices.Contains(p.products, a.Product) {
			return out, false
		}
		if p.productKeys {
			if a.AccountKey != a.Product {
				return out, false
			}
		} else if _, ok := lastFour(a.AccountKey); !ok {
			return out, false
		}
		accounts[a.AccountKey] = true
	}
	instrument := func(key string) bool {
		if key == "" {
			return false
		}
		return provider != ProviderAviva || key == NormaliseFundName(key)
	}
	type rowKey struct{ account, key string }
	seen := map[rowKey]bool{}
	first := func(account, key string) bool {
		k := rowKey{account, key}
		if seen[k] {
			return false
		}
		seen[k] = true
		return true
	}
	for i := range out.Events {
		e := &out.Events[i]
		for _, f := range []*string{&e.AccountKey, &e.SourceKey, &e.EventType, &e.ProviderType, &e.InstrumentKey,
			&e.InstrumentName, &e.Units, &e.Price, &e.PriceUnit, &e.Currency} {
			*f = cleanText(*f)
		}
		e.Content = cleanJSON(e.Content)
		if !accounts[e.AccountKey] || e.SourceKey == "" || !first(e.AccountKey, e.SourceKey) || e.ProviderType == "" ||
			!validEventType(e.EventType) || !isDay(e.OrderDate) || (!e.SettleDate.IsZero() && !isDay(e.SettleDate)) ||
			len(e.HashInput) == 0 || !json.Valid(e.Content) {
			return out, false
		}
		if (r.Kind == InvestmentKindCapture && e.EventType != EventContribution) ||
			(r.Kind == InvestmentKindStatement && e.EventType != EventTransferOut) {
			return out, false
		}
		hasUnits := e.Units != ""
		switch e.EventType {
		case EventBuy, EventSell:
			if !hasUnits {
				return out, false
			}
		case EventTransferIn, EventTransferOut:
		default:
			if hasUnits {
				return out, false
			}
		}
		if hasUnits != (e.InstrumentKey != "") || (hasUnits && (!validDecimal(e.Units) || !instrument(e.InstrumentKey))) {
			return out, false
		}
		if (e.Price != "") != (e.PriceUnit != "") || (e.Price != "" && (!hasUnits || !validDecimal(e.Price) || !validPriceUnit(e.PriceUnit))) {
			return out, false
		}
	}
	seen = map[rowKey]bool{}
	valuedOn := make(map[string]time.Time, len(out.Valuations))
	for i := range out.Valuations {
		v := &out.Valuations[i]
		v.AccountKey, v.Currency = cleanText(v.AccountKey), cleanText(v.Currency)
		if !accounts[v.AccountKey] || !isDay(v.AsOf) || !first(v.AccountKey, "") {
			return out, false
		}
		valuedOn[v.AccountKey] = v.AsOf
	}
	seen = map[rowKey]bool{}
	for i := range out.Holdings {
		h := &out.Holdings[i]
		for _, f := range []*string{&h.AccountKey, &h.InstrumentKey, &h.InstrumentName, &h.Units, &h.Price, &h.PriceUnit, &h.Currency} {
			*f = cleanText(*f)
		}
		on, valued := valuedOn[h.AccountKey]
		if !accounts[h.AccountKey] || !valued || !isDay(h.AsOf) || !h.AsOf.Equal(on) || !instrument(h.InstrumentKey) ||
			h.InstrumentName == "" || !first(h.AccountKey, h.InstrumentKey) || !validDecimal(h.Units) ||
			!validDecimal(h.Price) || !validPriceUnit(h.PriceUnit) {
			return out, false
		}
	}
	seen = map[rowKey]bool{}
	for i := range out.Statements {
		s := &out.Statements[i]
		s.AccountKey = cleanText(s.AccountKey)
		s.Charges, s.Projections = cleanJSON(s.Charges), cleanJSON(s.Projections)
		if !accounts[s.AccountKey] || !first(s.AccountKey, "") || !isDay(s.PeriodStart) || !isDay(s.PeriodEnd) ||
			!isDay(s.IssuedOn) || s.PeriodEnd.Before(s.PeriodStart) || !json.Valid(s.Charges) || !json.Valid(s.Projections) {
			return out, false
		}
	}
	seen = map[rowKey]bool{}
	for i := range out.TradeNotes {
		n := &out.TradeNotes[i]
		for _, f := range []*string{&n.AccountKey, &n.Reference, &n.ISIN, &n.Venue, &n.Units, &n.ExactPrice} {
			*f = cleanText(*f)
		}
		if _, ok := lastFour(n.AccountKey); !ok {
			return out, false
		}
		if n.Reference == "" || !first(n.AccountKey, n.Reference) || n.ISIN == "" ||
			n.ExecutedAt.IsZero() || !validDecimal(n.Units) || !validDecimal(n.ExactPrice) || !isDay(n.SettleDate) {
			return out, false
		}
	}
	complete := make(map[string]bool, len(r.CompleteContributionsFor))
	for _, k := range r.CompleteContributionsFor {
		k = cleanText(k)
		if _, valued := valuedOn[k]; !valued || complete[k] {
			return out, false
		}
		complete[k] = true
		out.CompleteContributionsFor = append(out.CompleteContributionsFor, k)
	}
	return out, true
}

func validEventType(t string) bool {
	switch t {
	case EventContribution, EventBuy, EventSell, EventTransferIn, EventTransferOut, EventFee, EventTax, EventInterest,
		EventWithdrawal, EventAdjustment, EventSiblingTransfer, EventInternal, EventIncome, EventCorporateAction:
		return true
	}
	return false
}

func validPriceUnit(u string) bool { return u == PriceUnitGBP || u == PriceUnitGBX }

package ledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"
)

// PostgreSQL text and jsonb hold neither U+0000 nor invalid UTF-8, and refuse them with an error
// that would stop the projection at that row for good. Every record is therefore sanitised as it
// leaves its normaliser, before any id is derived from it or anything is written: U+0000 is
// removed from text fields, invalid UTF-8 becomes U+FFFD, and in stored JSON content each U+0000
// becomes U+FFFD. These are pure functions, so replay gives the same rows as the live path. The
// exact bytes stay in raw. Version hashes are unaffected: they are computed over HashInput, which
// is never stored as text.

// cleanText returns s as valid UTF-8 (each invalid sequence becomes U+FFFD) with no U+0000.
func cleanText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "")
}

// cleanJSON returns content with every U+0000 in its strings and keys replaced by U+FFFD and any
// invalid UTF-8 replaced by U+FFFD. Content that needs neither is returned unchanged; otherwise it
// is decoded and re-encoded (numbers kept as written). Content that does not decode as exactly
// one JSON value is returned unchanged, for the caller's own check to refuse.
func cleanJSON(content json.RawMessage) json.RawMessage {
	// A false positive (an escaped backslash before "u0000") only costs a re-encode that changes
	// nothing in meaning; the decoder, not this test, decides what a string holds.
	if utf8.Valid(content) && !bytes.Contains(content, []byte(`\u0000`)) {
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

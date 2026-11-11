package monzo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rknightion/finstore/internal/ledger"
)

// Endpoints this package fetches.
const (
	pathAccounts     = "/accounts"
	pathBalance      = "/balance"
	pathPots         = "/pots"
	pathTransactions = "/transactions"
	pathWebhooks     = "/webhooks"
)

// capabilityKeys are the app's capability flags. They say nothing about the transaction, so they
// are left out of the version hash (but kept in the stored content).
var capabilityKeys = []string{
	"can_add_to_tab",
	"can_be_excluded_from_breakdown",
	"can_be_made_subscription",
	"can_split_the_bill",
	"can_match_transactions_in_categorization",
	"merchant_feedback_uri",
}

// accountTypes maps Monzo's account types to the ledger's.
var accountTypes = map[string]string{
	"uk_retail":       "current",
	"uk_retail_joint": "joint",
	"uk_monzo_flex":   "flex",
	"uk_loan":         "loan",
	"uk_rewards":      "rewards",
	"uk_business":     "business",
	"uk_prepaid":      "prepaid",
}

// defaultCurrency is used for an account object that names no currency; Monzo's accounts are
// sterling.
const defaultCurrency = "GBP"

// Normaliser turns stored Monzo responses into ledger records. It implements
// ledger.SourceNormaliser. Its errors name the endpoint and what was wrong, never a value.
type Normaliser struct{}

var _ ledger.SourceNormaliser = Normaliser{}

// errUnexpectedBody is wrapped by every error about a body that cannot be understood.
var errUnexpectedBody = errors.New("unexpected body")

// errBody is the only error a body that cannot be understood produces.
func errBody(endpoint, what string) error {
	return fmt.Errorf("monzo: %s response: %s: %w", endpoint, what, errUnexpectedBody)
}

// NormaliseResponse implements ledger.SourceNormaliser.
func (Normaliser) NormaliseResponse(r ledger.Response, loc *time.Location) (ledger.Result, error) {
	if r.Status < 200 || r.Status > 299 {
		return ledger.Result{}, nil
	}
	if loc == nil {
		loc = time.UTC
	}
	switch r.Endpoint {
	case pathAccounts:
		return normaliseAccounts(r.Body, loc)
	case pathPots:
		return normalisePots(r, loc)
	case pathBalance:
		return normaliseBalance(r)
	case pathTransactions:
		return normaliseTransactions(r, loc)
	}
	return ledger.Result{}, nil
}

// decode reads body as one JSON value, keeping numbers as written.
func decode(body []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing data")
	}
	return nil
}

// param returns a string query parameter from a stored params object. A parameter is stored as a
// string, or as an array of strings when it had several values; the first is used.
func param(params map[string]any, key string) string {
	switch v := params[key].(type) {
	case string:
		return v
	case []any:
		if len(v) > 0 {
			s, _ := v[0].(string)
			return s
		}
	case []string:
		if len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func integer(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}

// dayOf is the calendar day of t in loc, at midnight UTC.
func dayOf(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

func normaliseAccounts(body []byte, loc *time.Location) (ledger.Result, error) {
	var doc struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := decode(body, &doc); err != nil {
		return ledger.Result{}, errBody(pathAccounts, "not the expected JSON")
	}
	var out ledger.Result
	for _, a := range doc.Accounts {
		id := str(a, "id")
		if id == "" {
			return ledger.Result{}, errBody(pathAccounts, "an account has no id")
		}
		typ := str(a, "type")
		if mapped, ok := accountTypes[typ]; ok {
			typ = mapped
		}
		cur := str(a, "currency")
		if cur == "" {
			cur = defaultCurrency
		}
		rec := ledger.AccountRec{SourceKey: id, Type: typ, Currency: cur, Name: str(a, "description"),
			BankRef: ledger.BankRef(str(a, "sort_code"), str(a, "account_number"))}
		if t, ok := parseTime(str(a, "created")); ok {
			rec.OpenedOn = dayOf(t, loc)
		}
		rec.Closed, _ = a["closed"].(bool)
		out.Accounts = append(out.Accounts, rec)
	}
	return out, nil
}

func normalisePots(r ledger.Response, _ *time.Location) (ledger.Result, error) {
	parent := param(r.Params, "current_account_id")
	if parent == "" {
		return ledger.Result{}, errBody(pathPots, "request has no current_account_id")
	}
	var doc struct {
		Pots []map[string]any `json:"pots"`
	}
	if err := decode(r.Body, &doc); err != nil {
		return ledger.Result{}, errBody(pathPots, "not the expected JSON")
	}
	var out ledger.Result
	for _, p := range doc.Pots {
		id := str(p, "id")
		bal, ok := integer(p["balance"])
		if id == "" || !ok {
			return ledger.Result{}, errBody(pathPots, "a pot has no id or no integer balance")
		}
		cur := str(p, "currency")
		if cur == "" {
			cur = defaultCurrency
		}
		deleted, _ := p["deleted"].(bool)
		out.Accounts = append(out.Accounts, ledger.AccountRec{
			SourceKey: id, Type: "pot", Currency: cur, Name: str(p, "name"),
			ParentSourceKey: parent, Closed: deleted,
		})
		out.Balances = append(out.Balances, ledger.BalanceRec{
			AccountSourceKey: id, At: r.FetchedAt, BalanceMinor: bal, Currency: cur,
		})
	}
	return out, nil
}

func normaliseBalance(r ledger.Response) (ledger.Result, error) {
	acct := param(r.Params, "account_id")
	if acct == "" {
		return ledger.Result{}, errBody(pathBalance, "request has no account_id")
	}
	var doc map[string]any
	if err := decode(r.Body, &doc); err != nil {
		return ledger.Result{}, errBody(pathBalance, "not the expected JSON")
	}
	bal, ok := integer(doc["balance"])
	if !ok {
		return ledger.Result{}, errBody(pathBalance, "no integer balance")
	}
	rec := ledger.BalanceRec{AccountSourceKey: acct, At: r.FetchedAt, BalanceMinor: bal, Currency: str(doc, "currency")}
	if rec.Currency == "" {
		rec.Currency = defaultCurrency
	}
	if total, ok := integer(doc["total_balance"]); ok {
		rec.TotalBalanceMinor = &total
	}
	return ledger.Result{Balances: []ledger.BalanceRec{rec}}, nil
}

func normaliseTransactions(r ledger.Response, loc *time.Location) (ledger.Result, error) {
	var doc struct {
		Transactions []json.RawMessage `json:"transactions"`
	}
	if err := decode(r.Body, &doc); err != nil {
		return ledger.Result{}, errBody(pathTransactions, "not the expected JSON")
	}
	fallbackAccount := param(r.Params, "account_id")
	var out ledger.Result
	for i, raw := range doc.Transactions {
		rec, err := transaction(raw, fallbackAccount, loc)
		if err != nil {
			return ledger.Result{}, fmt.Errorf("monzo: %s response: transaction %d: %w", pathTransactions, i, err)
		}
		out.Txns = append(out.Txns, rec)
	}
	return out, nil
}

// transaction normalises one transaction object. raw is kept as Content byte for byte.
func transaction(raw json.RawMessage, fallbackAccount string, loc *time.Location) (ledger.TxnRec, error) {
	var t map[string]any
	if err := decode(raw, &t); err != nil || t == nil {
		return ledger.TxnRec{}, errors.New("not a JSON object")
	}
	id := str(t, "id")
	if id == "" {
		return ledger.TxnRec{}, errors.New("no id")
	}
	acct := str(t, "account_id")
	if acct == "" {
		acct = fallbackAccount
	}
	if acct == "" {
		return ledger.TxnRec{}, errors.New("no account")
	}
	amount, ok := integer(t["amount"])
	if !ok {
		return ledger.TxnRec{}, errors.New("no integer amount")
	}
	created, ok := parseTime(str(t, "created"))
	if !ok {
		return ledger.TxnRec{}, errors.New("no valid created time")
	}
	hash, err := HashInput(raw)
	if err != nil {
		return ledger.TxnRec{}, err
	}
	rec := ledger.TxnRec{
		AccountSourceKey: acct,
		SourceKey:        id,
		AmountMinor:      amount,
		Currency:         str(t, "currency"),
		CreatedAt:        &created,
		TxnDate:          dayOf(created, loc),
		Description:      str(t, "description"),
		Category:         str(t, "category"),
		Scheme:           str(t, "scheme"),
		Notes:            str(t, "notes"),
		Content:          append(json.RawMessage(nil), raw...),
		HashInput:        hash,
	}
	if la, ok := integer(t["local_amount"]); ok {
		rec.LocalAmountMinor = &la
		rec.LocalCurrency = str(t, "local_currency")
	}
	settled, isSettled := parseTime(str(t, "settled"))
	_, declined := t["decline_reason"]
	switch {
	case declined:
		rec.Status = ledger.StatusDeclined
	case isSettled:
		rec.Status = ledger.StatusSettled
	default:
		rec.Status = ledger.StatusPending
	}
	if isSettled {
		rec.SettledAt = &settled
	}
	switch m := t["merchant"].(type) {
	case map[string]any:
		rec.MerchantID, rec.MerchantName = str(m, "id"), str(m, "name")
	case string:
		rec.MerchantID = m
	}
	if cp, ok := t["counterparty"].(map[string]any); ok {
		rec.Counterparty = str(cp, "name")
	}
	return rec, nil
}

// HashInput is the canonical encoding a Monzo transaction's version hash is computed over: the
// object as received, without the capability keys, encoded with sorted keys at every level and no
// insignificant whitespace. Numbers keep their exact text.
func HashInput(raw json.RawMessage) ([]byte, error) {
	var t map[string]any
	if err := decode(raw, &t); err != nil || t == nil {
		return nil, errors.New("not a JSON object")
	}
	for _, k := range capabilityKeys {
		delete(t, k)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(t); err != nil {
		return nil, errors.New("cannot be re-encoded")
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// statusClass is the status_class metric attribute for an HTTP status.
func statusClass(status int) string {
	if status < 100 || status > 599 {
		return "none"
	}
	return fmt.Sprintf("%dxx", status/100)
}

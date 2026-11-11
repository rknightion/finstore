package truelayer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rknightion/finstore/internal/ledger"
)

// The two connections, each its own source (Source).
const (
	ConnLloyds = "ob-lloyds"
	ConnMonzo  = "ob-monzo"
)

// Endpoint templates, as raw.api_response.endpoint stores them.
const (
	pathMe             = "/data/v1/me"
	pathAccounts       = "/data/v1/accounts"
	pathBalance        = "/data/v1/accounts/{id}/balance"
	pathTransactions   = "/data/v1/accounts/{id}/transactions"
	pathStandingOrders = "/data/v1/accounts/{id}/standing_orders"
	pathDirectDebits   = "/data/v1/accounts/{id}/direct_debits"
)

// Request parameters the worker records with every per-account response.
const (
	paramBankRef   = "bank_ref"
	paramAccountID = "account_id"
	paramFrom      = "from"
	paramTo        = "to"
)

// TrueLayer account types.
const (
	typeTransaction = "TRANSACTION"
	typeSavings     = "SAVINGS"
)

const defaultCurrency = "GBP"

// Normaliser turns stored TrueLayer responses of one connection into ledger records. It implements
// ledger.SourceNormaliser. For ob-lloyds it reads accounts, transactions, balances and mandates;
// for ob-monzo only mandates, which attach to the Monzo accounts Monzo's own API feeds. Its errors
// name the endpoint and what was wrong, never a value.
type Normaliser struct {
	// Connection is ConnLloyds or ConnMonzo.
	Connection string
}

var _ ledger.SourceNormaliser = Normaliser{}

// errUnexpectedBody is wrapped by every error about a body that cannot be understood.
var errUnexpectedBody = errors.New("unexpected body")

func errBody(endpoint, what string) error {
	return fmt.Errorf("truelayer: %s response: %s: %w", endpoint, what, errUnexpectedBody)
}

// NormaliseResponse implements ledger.SourceNormaliser.
func (n Normaliser) NormaliseResponse(r ledger.Response, loc *time.Location) (ledger.Result, error) {
	if r.Status < 200 || r.Status > 299 {
		return ledger.Result{}, nil
	}
	if loc == nil {
		loc = time.UTC
	}
	lloyds := n.Connection == ConnLloyds
	switch {
	case r.Endpoint == pathAccounts && lloyds:
		return normaliseAccounts(r.Body)
	case r.Endpoint == pathTransactions && lloyds:
		return normaliseTransactions(r, loc)
	case r.Endpoint == pathBalance && lloyds:
		return normaliseBalance(r)
	case r.Endpoint == pathStandingOrders:
		return normaliseStandingOrders(r)
	case r.Endpoint == pathDirectDebits:
		return normaliseDirectDebits(r)
	case r.Endpoint == pathAccounts:
		// Check the body even though ob-monzo keeps nothing from it: the worker acts on it.
		_, err := decodeResults(r.Body, pathAccounts)
		return ledger.Result{}, err
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

// decodeResults returns the items of a {"results": [...]} body, byte for byte.
func decodeResults(body []byte, endpoint string) ([]json.RawMessage, error) {
	var doc struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := decode(body, &doc); err != nil {
		return nil, errBody(endpoint, "not the expected JSON")
	}
	return doc.Results, nil
}

func param(params map[string]any, key string) string {
	s, _ := params[key].(string)
	return s
}

// minor converts an amount to minor units exactly; an absent amount is nil.
func minor(n json.Number, endpoint string) (*int64, error) {
	if n == "" {
		return nil, nil
	}
	v, err := ledger.ParseMinor(string(n))
	if err != nil {
		return nil, errBody(endpoint, "an amount is not a decimal with at most two places")
	}
	return &v, nil
}

// day is the calendar day a TrueLayer timestamp or date names, at midnight UTC: the date as
// written, in the timestamp's own offset. An empty value is the zero time.
func day(s, endpoint string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse(time.DateOnly, s)
	}
	if err != nil {
		return time.Time{}, errBody(endpoint, "a date is not RFC 3339")
	}
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), nil
}

// accountType maps a Lloyds account to the ledger's type: "Joint" by name is joint, otherwise its
// TrueLayer type decides.
func accountType(displayName, typ string) string {
	switch {
	case strings.EqualFold(strings.TrimSpace(displayName), "joint"):
		return "joint"
	case typ == typeTransaction:
		return "current"
	case typ == typeSavings:
		return "savings"
	}
	return "other"
}

// accountItem is the part of an account object this package acts on.
type accountItem struct {
	AccountID     string `json:"account_id"`
	AccountType   string `json:"account_type"`
	DisplayName   string `json:"display_name"`
	Currency      string `json:"currency"`
	AccountNumber struct {
		SortCode string `json:"sort_code"`
		Number   string `json:"number"`
	} `json:"account_number"`
}

func (a accountItem) bankRef() string {
	return ledger.BankRef(a.AccountNumber.SortCode, a.AccountNumber.Number)
}

func normaliseAccounts(body []byte) (ledger.Result, error) {
	items, err := decodeResults(body, pathAccounts)
	if err != nil {
		return ledger.Result{}, err
	}
	var out ledger.Result
	for _, raw := range items {
		var a accountItem
		if err := decode(raw, &a); err != nil {
			return ledger.Result{}, errBody(pathAccounts, "an account is not the expected JSON")
		}
		ref := a.bankRef()
		if ref == "" {
			// An account with no sort code and number cannot be keyed; the worker skips it too.
			continue
		}
		cur := a.Currency
		if cur == "" {
			cur = defaultCurrency
		}
		out.Accounts = append(out.Accounts, ledger.AccountRec{SourceKey: ref, Type: accountType(a.DisplayName, a.AccountType),
			Currency: cur, Name: a.DisplayName, BankRef: ref})
	}
	return out, nil
}

// bankRefParam returns the bank reference the worker resolved for a per-account response.
func bankRefParam(r ledger.Response) (string, error) {
	ref := param(r.Params, paramBankRef)
	if ref == "" {
		return "", errBody(r.Endpoint, "request has no bank reference")
	}
	return ref, nil
}

type txItem struct {
	Timestamp      string          `json:"timestamp"`
	Description    string          `json:"description"`
	Amount         json.Number     `json:"amount"`
	Currency       string          `json:"currency"`
	Type           string          `json:"transaction_type"`
	Category       string          `json:"transaction_category"`
	Classification json.RawMessage `json:"transaction_classification"`
	MerchantName   string          `json:"merchant_name"`
	RunningBalance *struct {
		Amount   json.Number `json:"amount"`
		Currency string      `json:"currency"`
	} `json:"running_balance"`
	NormalisedID string `json:"normalised_provider_transaction_id"`
	ProviderID   string `json:"provider_transaction_id"`
}

// txHash is what a transaction's version hash is computed over: its business fields, never
// TrueLayer's transaction_id, account_id or a fetch time.
type txHash struct {
	Date           string          `json:"date"`
	AmountMinor    int64           `json:"amount_minor"`
	Currency       string          `json:"currency"`
	Description    string          `json:"description"`
	Type           string          `json:"type"`
	Category       string          `json:"category"`
	Classification json.RawMessage `json:"classification,omitempty"`
	MerchantName   string          `json:"merchant_name"`
	RunningBalance *int64          `json:"running_balance_minor"`
}

func normaliseTransactions(r ledger.Response, loc *time.Location) (ledger.Result, error) {
	ref, err := bankRefParam(r)
	if err != nil {
		return ledger.Result{}, err
	}
	items, err := decodeResults(r.Body, pathTransactions)
	if err != nil {
		return ledger.Result{}, err
	}
	var out ledger.Result
	for _, raw := range items {
		var x txItem
		if err := decode(raw, &x); err != nil {
			return ledger.Result{}, errBody(pathTransactions, "a transaction is not the expected JSON")
		}
		key := x.NormalisedID
		if key == "" {
			key = x.ProviderID
		}
		if key == "" {
			return ledger.Result{}, errBody(pathTransactions, "a transaction has no provider id")
		}
		amount, err := minor(x.Amount, pathTransactions)
		if err != nil {
			return ledger.Result{}, err
		}
		if amount == nil {
			return ledger.Result{}, errBody(pathTransactions, "a transaction has no amount")
		}
		date, err := day(x.Timestamp, pathTransactions)
		if err != nil {
			return ledger.Result{}, err
		}
		if date.IsZero() {
			return ledger.Result{}, errBody(pathTransactions, "a transaction has no date")
		}
		var running *int64
		if x.RunningBalance != nil {
			if running, err = minor(x.RunningBalance.Amount, pathTransactions); err != nil {
				return ledger.Result{}, err
			}
		}
		cur := x.Currency
		if cur == "" {
			cur = defaultCurrency
		}
		// The row's day at local midnight: Lloyds gives no time, and a completed period, which
		// starts at local midnight, then contains its rows.
		created := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, loc)
		hash, err := json.Marshal(txHash{Date: date.Format(time.DateOnly), AmountMinor: *amount, Currency: cur,
			Description: x.Description, Type: x.Type, Category: x.Category, Classification: compact(x.Classification),
			MerchantName: x.MerchantName, RunningBalance: running})
		if err != nil {
			return ledger.Result{}, errBody(pathTransactions, "a transaction cannot be encoded")
		}
		out.Txns = append(out.Txns, ledger.TxnRec{
			AccountSourceKey: ref, SourceKey: key, AmountMinor: *amount, Currency: cur,
			CreatedAt: &created, TxnDate: date, Status: ledger.StatusSettled, Description: x.Description,
			MerchantName: x.MerchantName, Category: x.Category, Content: raw, HashInput: hash,
		})
	}
	return out, nil
}

// compact returns raw JSON without insignificant space, so the hash does not depend on layout.
func compact(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return nil
	}
	return b.Bytes()
}

func normaliseBalance(r ledger.Response) (ledger.Result, error) {
	ref, err := bankRefParam(r)
	if err != nil {
		return ledger.Result{}, err
	}
	items, err := decodeResults(r.Body, pathBalance)
	if err != nil {
		return ledger.Result{}, err
	}
	var out ledger.Result
	for _, raw := range items {
		var b struct {
			Current         json.Number `json:"current"`
			Currency        string      `json:"currency"`
			UpdateTimestamp string      `json:"update_timestamp"`
		}
		if err := decode(raw, &b); err != nil {
			return ledger.Result{}, errBody(pathBalance, "a balance is not the expected JSON")
		}
		cur, err := minor(b.Current, pathBalance)
		if err != nil {
			return ledger.Result{}, err
		}
		if cur == nil {
			return ledger.Result{}, errBody(pathBalance, "a balance has no current amount")
		}
		at := r.FetchedAt
		if b.UpdateTimestamp != "" {
			t, err := time.Parse(time.RFC3339Nano, b.UpdateTimestamp)
			if err != nil {
				return ledger.Result{}, errBody(pathBalance, "update_timestamp is not RFC 3339")
			}
			at = t
		}
		currency := b.Currency
		if currency == "" {
			currency = defaultCurrency
		}
		out.Balances = append(out.Balances, ledger.BalanceRec{AccountSourceKey: ref, At: at, BalanceMinor: *cur, Currency: currency})
	}
	return out, nil
}

type soItem struct {
	Frequency          string      `json:"frequency"`
	Status             string      `json:"status"`
	Currency           string      `json:"currency"`
	Payee              string      `json:"payee"`
	Reference          string      `json:"reference"`
	FirstPaymentDate   string      `json:"first_payment_date"`
	FirstPaymentAmount json.Number `json:"first_payment_amount"`
	NextPaymentDate    string      `json:"next_payment_date"`
	NextPaymentAmount  json.Number `json:"next_payment_amount"`
	FinalPaymentDate   string      `json:"final_payment_date"`
	FinalPaymentAmount json.Number `json:"final_payment_amount"`
}

// soHash is what a standing order's version hash is computed over: its business fields, never
// meta.provider_account_id or the response timestamp.
type soHash struct {
	Payee, Reference, Frequency, Status, Currency string
	FirstDate, NextDate, FinalDate                string
	FirstAmount, NextAmount, FinalAmount          *int64
}

func dateText(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.DateOnly)
}

func normaliseStandingOrders(r ledger.Response) (ledger.Result, error) {
	ref, err := bankRefParam(r)
	if err != nil {
		return ledger.Result{}, err
	}
	items, err := decodeResults(r.Body, pathStandingOrders)
	if err != nil {
		return ledger.Result{}, err
	}
	out := ledger.Result{MandateLists: []ledger.MandateList{{Kind: ledger.MandateStandingOrder, BankRef: ref}}}
	for _, raw := range items {
		var x soItem
		if err := decode(raw, &x); err != nil {
			return ledger.Result{}, errBody(pathStandingOrders, "a standing order is not the expected JSON")
		}
		rec := ledger.StandingOrderRec{BankRef: ref, Payee: x.Payee, Reference: x.Reference, Frequency: x.Frequency,
			Status: x.Status, Currency: x.Currency, Content: raw}
		for _, f := range []struct {
			date   string
			amount json.Number
			d      *time.Time
			a      **int64
		}{
			{x.FirstPaymentDate, x.FirstPaymentAmount, &rec.FirstDate, &rec.FirstAmountMinor},
			{x.NextPaymentDate, x.NextPaymentAmount, &rec.NextDate, &rec.NextAmountMinor},
			{x.FinalPaymentDate, x.FinalPaymentAmount, &rec.FinalDate, &rec.FinalAmountMinor},
		} {
			if *f.d, err = day(f.date, pathStandingOrders); err != nil {
				return ledger.Result{}, err
			}
			if *f.a, err = minor(f.amount, pathStandingOrders); err != nil {
				return ledger.Result{}, err
			}
		}
		// TrueLayer gives no per-mandate id: the payee, reference and frequency identify it, written
		// as a JSON array so no two triples give one key. The ledger numbers identical keys.
		key, err := json.Marshal([]string{x.Payee, x.Reference, x.Frequency})
		if err != nil {
			return ledger.Result{}, errBody(pathStandingOrders, "a standing order cannot be encoded")
		}
		rec.MandateKey = string(key)
		if rec.HashInput, err = json.Marshal(soHash{Payee: x.Payee, Reference: x.Reference, Frequency: x.Frequency,
			Status: x.Status, Currency: x.Currency, FirstDate: dateText(rec.FirstDate), NextDate: dateText(rec.NextDate),
			FinalDate: dateText(rec.FinalDate), FirstAmount: rec.FirstAmountMinor, NextAmount: rec.NextAmountMinor,
			FinalAmount: rec.FinalAmountMinor}); err != nil {
			return ledger.Result{}, errBody(pathStandingOrders, "a standing order cannot be encoded")
		}
		out.StandingOrders = append(out.StandingOrders, rec)
	}
	return out, nil
}

type ddItem struct {
	Name                     string      `json:"name"`
	Status                   string      `json:"status"`
	Currency                 string      `json:"currency"`
	PreviousPaymentTimestamp string      `json:"previous_payment_timestamp"`
	PreviousPaymentAmount    json.Number `json:"previous_payment_amount"`
	Meta                     struct {
		MandateID string `json:"provider_mandate_identification"`
	} `json:"meta"`
}

// ddHash is what a direct debit's version hash is computed over: never direct_debit_id,
// meta.provider_account_id or the response timestamp.
type ddHash struct {
	Name, Status, Currency, MandateID, PreviousDate string
	PreviousAmount                                  *int64
}

func normaliseDirectDebits(r ledger.Response) (ledger.Result, error) {
	ref, err := bankRefParam(r)
	if err != nil {
		return ledger.Result{}, err
	}
	items, err := decodeResults(r.Body, pathDirectDebits)
	if err != nil {
		return ledger.Result{}, err
	}
	out := ledger.Result{MandateLists: []ledger.MandateList{{Kind: ledger.MandateDirectDebit, BankRef: ref}}}
	for _, raw := range items {
		var x ddItem
		if err := decode(raw, &x); err != nil {
			return ledger.Result{}, errBody(pathDirectDebits, "a direct debit is not the expected JSON")
		}
		prev, err := day(x.PreviousPaymentTimestamp, pathDirectDebits)
		if err != nil {
			return ledger.Result{}, err
		}
		amount, err := minor(x.PreviousPaymentAmount, pathDirectDebits)
		if err != nil {
			return ledger.Result{}, err
		}
		key := x.Meta.MandateID
		if key == "" {
			key = x.Name
		}
		hash, err := json.Marshal(ddHash{Name: x.Name, Status: x.Status, Currency: x.Currency, MandateID: x.Meta.MandateID,
			PreviousDate: dateText(prev), PreviousAmount: amount})
		if err != nil {
			return ledger.Result{}, errBody(pathDirectDebits, "a direct debit cannot be encoded")
		}
		out.DirectDebits = append(out.DirectDebits, ledger.DirectDebitRec{BankRef: ref, MandateKey: key, Name: x.Name,
			Status: x.Status, PreviousDate: prev, PreviousAmountMinor: amount, Currency: x.Currency, Content: raw, HashInput: hash})
	}
	return out, nil
}

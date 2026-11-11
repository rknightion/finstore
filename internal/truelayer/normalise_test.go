package truelayer

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/ledger"
)

const testRef = "112233 12345678"

func london(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func resp(endpoint, body string, params map[string]any) ledger.Response {
	if params == nil {
		params = map[string]any{}
	}
	return ledger.Response{Endpoint: endpoint, Params: params, Status: 200, Body: []byte(body),
		FetchedAt: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
}

func account(params ...string) map[string]any {
	p := map[string]any{"bank_ref": testRef, "account_id": "acc-0001"}
	for i := 0; i+1 < len(params); i += 2 {
		p[params[i]] = params[i+1]
	}
	return p
}

const accountsBody = `{"results":[
 {"account_id":"a1","account_type":"TRANSACTION","display_name":"Joint","currency":"GBP",
  "account_number":{"iban":"GB00","number":"12345678","sort_code":"11-22-33"},"provider":{"provider_id":"ob-lloyds"}},
 {"account_id":"a2","account_type":"TRANSACTION","display_name":"Classic","currency":"GBP",
  "account_number":{"number":"87654321","sort_code":"112233"}},
 {"account_id":"a3","account_type":"SAVINGS","display_name":"Saver",
  "account_number":{"number":"11112222","sort_code":"112233"}},
 {"account_id":"a4","account_type":"BUSINESS_TRANSACTION","display_name":"Other",
  "account_number":{"number":"33334444","sort_code":"112233"}},
 {"account_id":"a5","account_type":"TRANSACTION","display_name":"No number"}
],"status":"Succeeded"}`

// Lloyds accounts are keyed and referenced by their bank reference; "Joint" by name gives joint,
// and the account type gives the rest. An account with no number cannot be keyed and is left out.
func TestLloydsAccounts(t *testing.T) {
	res, err := Normaliser{Connection: ConnLloyds}.NormaliseResponse(resp(pathAccounts, accountsBody, nil), london(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []ledger.AccountRec{
		{SourceKey: testRef, Type: "joint", Currency: "GBP", Name: "Joint", BankRef: testRef},
		{SourceKey: "112233 87654321", Type: "current", Currency: "GBP", Name: "Classic", BankRef: "112233 87654321"},
		{SourceKey: "112233 11112222", Type: "savings", Currency: "GBP", Name: "Saver", BankRef: "112233 11112222"},
		{SourceKey: "112233 33334444", Type: "other", Currency: "GBP", Name: "Other", BankRef: "112233 33334444"},
	}
	if len(res.Accounts) != len(want) {
		t.Fatalf("accounts = %+v", res.Accounts)
	}
	for i := range want {
		if res.Accounts[i] != want[i] {
			t.Errorf("account %d = %+v, want %+v", i, res.Accounts[i], want[i])
		}
	}
	// "JOINT" in any case is the joint account.
	upper := strings.Replace(accountsBody, `"Joint"`, `" jOiNt "`, 1)
	res, _ = Normaliser{Connection: ConnLloyds}.NormaliseResponse(resp(pathAccounts, upper, nil), london(t))
	if res.Accounts[0].Type != "joint" {
		t.Errorf("display name in another case gives %q", res.Accounts[0].Type)
	}
	// Monzo through TrueLayer creates no accounts: mandates attach to Monzo's own.
	res, err = Normaliser{Connection: ConnMonzo}.NormaliseResponse(resp(pathAccounts, accountsBody, nil), london(t))
	if err != nil || len(res.Accounts) != 0 {
		t.Errorf("ob-monzo accounts = %+v, %v", res.Accounts, err)
	}
}

const txBody = `{"results":[
 {"transaction_id":"t-1","timestamp":"2024-08-22T00:00:00+00:00","description":"MADE UP SHOP","amount":-12.3,
  "currency":"GBP","transaction_type":"DEBIT","transaction_category":"PURCHASE","transaction_classification":[],
  "merchant_name":"Made Up","running_balance":{"amount":1919.59,"currency":"GBP"},
  "normalised_provider_transaction_id":"norm-1","provider_transaction_id":"prov-1","meta":{"provider_category":"DEB"}},
 {"transaction_id":"t-2","timestamp":"2024-12-01T00:00:00+00:00","description":"MADE UP PAY","amount":0.1,
  "currency":"GBP","transaction_type":"CREDIT","running_balance":{"amount":1919.69,"currency":"GBP"},
  "provider_transaction_id":"prov-2"}
]}`

// Lloyds rows: exact amounts, the row's day at local midnight as the created time (so a completed
// period finds it), keyed on the normalised id, else the provider id, with the whole object kept.
func TestLloydsTransactions(t *testing.T) {
	loc := london(t)
	res, err := Normaliser{Connection: ConnLloyds}.NormaliseResponse(resp(pathTransactions, txBody, account()), loc)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Txns) != 2 {
		t.Fatalf("txns = %+v", res.Txns)
	}
	a, b := res.Txns[0], res.Txns[1]
	if a.AccountSourceKey != testRef || a.SourceKey != "norm-1" || a.AmountMinor != -1230 || a.Currency != "GBP" ||
		a.Status != ledger.StatusSettled || a.Description != "MADE UP SHOP" || a.MerchantName != "Made Up" ||
		a.Category != "PURCHASE" {
		t.Errorf("first row = %+v", a)
	}
	// 2024-08-22 is in summer time: local midnight is 23:00 UTC the day before.
	if a.CreatedAt == nil || !a.CreatedAt.Equal(time.Date(2024, 8, 21, 23, 0, 0, 0, time.UTC)) ||
		!a.TxnDate.Equal(time.Date(2024, 8, 22, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("first row times: created %v date %v", a.CreatedAt, a.TxnDate)
	}
	if b.SourceKey != "prov-2" || b.AmountMinor != 10 || !b.CreatedAt.Equal(time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("second row = %+v", b)
	}
	var content map[string]any
	if err := json.Unmarshal(a.Content, &content); err != nil || content["running_balance"] == nil || content["meta"] == nil {
		t.Errorf("content = %s", a.Content)
	}
	// More than two decimal places is not an amount.
	bad := strings.Replace(txBody, "-12.3,", "-12.345,", 1)
	if _, err := (Normaliser{Connection: ConnLloyds}).NormaliseResponse(resp(pathTransactions, bad, account()), loc); err == nil {
		t.Error("an amount with three decimal places was accepted")
	}
	noKey := strings.Replace(txBody, `"normalised_provider_transaction_id":"norm-1","provider_transaction_id":"prov-1",`, "", 1)
	if _, err := (Normaliser{Connection: ConnLloyds}).NormaliseResponse(resp(pathTransactions, noKey, account()), loc); err == nil {
		t.Error("a row with no stable id was accepted")
	}
	if _, err := (Normaliser{Connection: ConnLloyds}).NormaliseResponse(resp(pathTransactions, txBody, nil), loc); err == nil {
		t.Error("a response with no bank reference was accepted")
	}
}

// The version hash is built from business fields only: a call that changes only TrueLayer's ids
// gives the same hash; a changed amount or description does not.
func TestHashesIgnoreTrueLayerIDs(t *testing.T) {
	loc := london(t)
	n := Normaliser{Connection: ConnLloyds}
	hash := func(body string, params map[string]any) []byte {
		t.Helper()
		res, err := n.NormaliseResponse(resp(pathTransactions, body, params), loc)
		if err != nil || len(res.Txns) == 0 {
			t.Fatalf("%v %+v", err, res)
		}
		return res.Txns[0].HashInput
	}
	base := hash(txBody, account())
	if !bytes.Equal(base, hash(strings.Replace(txBody, `"t-1"`, `"t-99"`, 1), account("account_id", "acc-0002"))) {
		t.Error("a new transaction_id or account id changed the hash")
	}
	for _, changed := range []string{
		strings.Replace(txBody, "-12.3,", "-12.4,", 1),
		strings.Replace(txBody, `"MADE UP SHOP"`, `"MADE UP SHOP 2"`, 1),
		strings.Replace(txBody, `"2024-08-22T00:00:00+00:00"`, `"2024-08-23T00:00:00+00:00"`, 1),
	} {
		if bytes.Equal(base, hash(changed, account())) {
			t.Errorf("a business change kept the hash: %s", changed[:80])
		}
	}

	ddBody := `{"results":[{"direct_debit_id":"dd-1","name":"MADE UP CO","status":"Active","currency":"GBP",
	  "previous_payment_timestamp":"2026-10-01T00:00:00+00:00","previous_payment_amount":25,
	  "timestamp":"2026-11-11T12:00:00Z","meta":{"provider_mandate_identification":"MANDATE-1","provider_account_id":"acc-0001"}}]}`
	ddHash := func(body string) []byte {
		t.Helper()
		res, err := Normaliser{Connection: ConnMonzo}.NormaliseResponse(resp(pathDirectDebits, body, account()), loc)
		if err != nil || len(res.DirectDebits) != 1 {
			t.Fatalf("%v %+v", err, res)
		}
		return res.DirectDebits[0].HashInput
	}
	d0 := ddHash(ddBody)
	idsOnly := strings.NewReplacer(`"dd-1"`, `"dd-2"`, `"acc-0001"`, `"acc-0009"`, `"2026-11-11T12:00:00Z"`, `"2026-11-12T06:20:00Z"`).Replace(ddBody)
	if !bytes.Equal(d0, ddHash(idsOnly)) {
		t.Error("a new direct_debit_id, account id or fetch time changed the hash")
	}
	if bytes.Equal(d0, ddHash(strings.Replace(ddBody, `"Active"`, `"Inactive"`, 1))) {
		t.Error("a status change kept the hash")
	}
}

func TestLloydsBalance(t *testing.T) {
	n := Normaliser{Connection: ConnLloyds}
	body := `{"results":[{"currency":"GBP","available":1919.59,"current":1919.59,"overdraft":0,"update_timestamp":"2026-11-11T09:30:00Z"}]}`
	res, err := n.NormaliseResponse(resp(pathBalance, body, account()), london(t))
	if err != nil || len(res.Balances) != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	b := res.Balances[0]
	if b.AccountSourceKey != testRef || b.BalanceMinor != 191959 || b.Currency != "GBP" ||
		!b.At.Equal(time.Date(2026, 11, 11, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("balance = %+v", b)
	}
	res, _ = n.NormaliseResponse(resp(pathBalance, strings.Replace(body, `,"update_timestamp":"2026-11-11T09:30:00Z"`, "", 1), account()), london(t))
	if !res.Balances[0].At.Equal(time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("with no update time the reading is at %v, want the fetch time", res.Balances[0].At)
	}
	// Monzo balances and transactions come from Monzo's own API.
	for _, ep := range []string{pathBalance, pathTransactions} {
		res, err := Normaliser{Connection: ConnMonzo}.NormaliseResponse(resp(ep, body, account()), london(t))
		if err != nil || len(res.Balances)+len(res.Txns) != 0 {
			t.Errorf("ob-monzo %s = %+v, %v", ep, res, err)
		}
	}
}

// Both connections' mandates become records on the bank reference the worker resolved, and every
// successful list, empty or not, says the list is complete for that account and kind.
func TestMandates(t *testing.T) {
	loc := london(t)
	soBody := `{"results":[{"frequency":"MONTHLY","status":"Active","currency":"GBP","payee":"MADE UP PAYEE","reference":"MADE UP REF",
	  "first_payment_date":"2025-01-05T00:00:00+00:00","first_payment_amount":100,
	  "next_payment_date":"2026-12-05T00:00:00+00:00","next_payment_amount":100.5,
	  "timestamp":"2026-11-11T12:00:00Z","meta":{"provider_account_id":"acc-0001"}}]}`
	ddBody := `{"results":[
	  {"direct_debit_id":"dd-1","name":"MADE UP CO","status":"Active","previous_payment_timestamp":"2026-10-01T00:00:00+00:00",
	   "previous_payment_amount":-25,"meta":{"provider_mandate_identification":"MANDATE-1"}},
	  {"direct_debit_id":"dd-2","name":"NO MANDATE CO","status":"Inactive"}]}`
	for _, conn := range []string{ConnLloyds, ConnMonzo} {
		n := Normaliser{Connection: conn}
		res, err := n.NormaliseResponse(resp(pathStandingOrders, soBody, account()), loc)
		if err != nil || len(res.StandingOrders) != 1 {
			t.Fatalf("%s: %v %+v", conn, err, res)
		}
		so := res.StandingOrders[0]
		if so.BankRef != testRef || so.MandateKey == "" || so.Payee != "MADE UP PAYEE" || so.Reference != "MADE UP REF" ||
			so.Frequency != "MONTHLY" || so.Status != "Active" || so.Currency != "GBP" ||
			!so.FirstDate.Equal(time.Date(2025, 1, 5, 0, 0, 0, 0, time.UTC)) || *so.FirstAmountMinor != 10000 ||
			!so.NextDate.Equal(time.Date(2026, 12, 5, 0, 0, 0, 0, time.UTC)) || *so.NextAmountMinor != 10050 ||
			!so.FinalDate.IsZero() || so.FinalAmountMinor != nil {
			t.Errorf("%s: standing order = %+v", conn, so)
		}
		if len(res.MandateLists) != 1 || res.MandateLists[0] != (ledger.MandateList{Kind: ledger.MandateStandingOrder, BankRef: testRef}) {
			t.Errorf("%s: lists = %+v", conn, res.MandateLists)
		}
		res, err = n.NormaliseResponse(resp(pathDirectDebits, ddBody, account()), loc)
		if err != nil || len(res.DirectDebits) != 2 {
			t.Fatalf("%s: %v %+v", conn, err, res)
		}
		d1, d2 := res.DirectDebits[0], res.DirectDebits[1]
		if d1.MandateKey != "MANDATE-1" || d1.Name != "MADE UP CO" || *d1.PreviousAmountMinor != -2500 ||
			!d1.PreviousDate.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) || d2.MandateKey != "NO MANDATE CO" ||
			d2.PreviousAmountMinor != nil {
			t.Errorf("%s: direct debits = %+v / %+v", conn, d1, d2)
		}
		empty, err := n.NormaliseResponse(resp(pathDirectDebits, `{"results":[]}`, account()), loc)
		if err != nil || len(empty.DirectDebits) != 0 ||
			len(empty.MandateLists) != 1 || empty.MandateLists[0] != (ledger.MandateList{Kind: ledger.MandateDirectDebit, BankRef: testRef}) {
			t.Errorf("%s: empty list = %+v, %v", conn, empty, err)
		}
		failed := resp(pathDirectDebits, `{"error":"internal_server_error"}`, account())
		failed.Status = 500
		if res, err := n.NormaliseResponse(failed, loc); err != nil || len(res.MandateLists) != 0 {
			t.Errorf("%s: a failed list proved something: %+v %v", conn, res, err)
		}
		if _, err := n.NormaliseResponse(resp(pathDirectDebits, ddBody, nil), loc); err == nil {
			t.Errorf("%s: a list with no bank reference was accepted", conn)
		}
	}
	// Two standing orders with the same payee, reference and frequency share a natural key; the
	// ledger numbers them.
	two := strings.Replace(soBody, `}]}`, `},{"frequency":"MONTHLY","status":"Active","payee":"MADE UP PAYEE","reference":"MADE UP REF"}]}`, 1)
	res, err := Normaliser{Connection: ConnLloyds}.NormaliseResponse(resp(pathStandingOrders, two, account()), loc)
	if err != nil || len(res.StandingOrders) != 2 || res.StandingOrders[0].MandateKey != res.StandingOrders[1].MandateKey {
		t.Errorf("identical standing orders: %+v %v", res.StandingOrders, err)
	}
}

func TestErrorStatusAndOtherEndpointsYieldNothing(t *testing.T) {
	r := resp(pathAccounts, accountsBody, nil)
	r.Status = 401
	for _, x := range []ledger.Response{r, resp(pathMe, `{"results":[{"consent_expires_at":"2027-02-09T00:00:00Z"}]}`, nil)} {
		res, err := Normaliser{Connection: ConnLloyds}.NormaliseResponse(x, london(t))
		if err != nil || len(res.Accounts)+len(res.Txns)+len(res.Balances)+len(res.MandateLists) != 0 {
			t.Errorf("%s %d: %+v %v", x.Endpoint, x.Status, res, err)
		}
	}
	if _, err := (Normaliser{Connection: ConnLloyds}).NormaliseResponse(resp(pathAccounts, `not json`, nil), london(t)); err == nil {
		t.Error("a body that is not JSON was accepted")
	}
}

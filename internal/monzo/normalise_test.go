package monzo

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/ledger"
)

func london(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// A made-up transaction in the documented shape, with every field the normaliser reads.
const txFixture = `{"id":"tx_fixture_0001","account_id":"acc_fixture_0001","created":"2026-06-15T23:30:00.123Z",
 "amount":-1250,"currency":"GBP","local_amount":-1450,"local_currency":"EUR","description":"FIXTURE CAFE",
 "settled":"2026-06-17T04:00:00Z","category":"eating_out","scheme":"mastercard","notes":"lunch",
 "merchant":{"id":"merch_fixture_0001","name":"Fixture Cafe","category":"eating_out"},
 "counterparty":{},"metadata":{"z":"1","a":"2"},
 "can_add_to_tab":false,"can_be_excluded_from_breakdown":true,"can_be_made_subscription":false,
 "can_split_the_bill":true,"can_match_transactions_in_categorization":false,
 "merchant_feedback_uri":"https://example.test/feedback"}`

func txPage(objs ...string) []byte {
	return []byte(`{"transactions":[` + strings.Join(objs, ",") + `]}`)
}

// edit returns the fixture with fn applied to its decoded fields.
func edit(t *testing.T, fn func(map[string]any)) string {
	t.Helper()
	d := json.NewDecoder(strings.NewReader(txFixture))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		t.Fatal(err)
	}
	fn(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func normaliseTx(t *testing.T, obj string) ledger.TxnRec {
	t.Helper()
	res, err := Normaliser{}.NormaliseResponse(ledger.Response{Endpoint: pathTransactions, Status: 200,
		Params: map[string]any{"account_id": "acc_fixture_0001"}, Body: txPage(obj)}, london(t))
	if err != nil {
		t.Fatalf("NormaliseResponse: %v", err)
	}
	if len(res.Txns) != 1 {
		t.Fatalf("%d transactions, want 1", len(res.Txns))
	}
	return res.Txns[0]
}

func TestTransactionFieldsAndContentAsReceived(t *testing.T) {
	tx := normaliseTx(t, txFixture)
	if tx.SourceKey != "tx_fixture_0001" || tx.AccountSourceKey != "acc_fixture_0001" || tx.AmountMinor != -1250 ||
		tx.Currency != "GBP" || tx.LocalAmountMinor == nil || *tx.LocalAmountMinor != -1450 || tx.LocalCurrency != "EUR" ||
		tx.Description != "FIXTURE CAFE" || tx.MerchantID != "merch_fixture_0001" || tx.MerchantName != "Fixture Cafe" ||
		tx.Category != "eating_out" || tx.Scheme != "mastercard" || tx.Notes != "lunch" {
		t.Fatalf("normalised transaction = %+v", tx)
	}
	if tx.SettledAt == nil || !tx.SettledAt.Equal(time.Date(2026, 6, 17, 4, 0, 0, 0, time.UTC)) {
		t.Fatalf("settled at %v", tx.SettledAt)
	}
	if !bytes.Equal(tx.Content, []byte(txFixture)) {
		t.Fatal("Content is not the object byte for byte as received")
	}
}

func TestStatusIsDeclinedSettledOrPendingAsTheContractSays(t *testing.T) {
	cases := []struct {
		name string
		fn   func(map[string]any)
		want ledger.Status
	}{
		{"declined wins over settled", func(m map[string]any) { m["decline_reason"] = "INSUFFICIENT_FUNDS" }, ledger.StatusDeclined},
		{"declined with empty reason", func(m map[string]any) { m["decline_reason"] = ""; m["settled"] = "" }, ledger.StatusDeclined},
		{"settled timestamp", func(map[string]any) {}, ledger.StatusSettled},
		{"empty settled", func(m map[string]any) { m["settled"] = "" }, ledger.StatusPending},
		{"no settled key", func(m map[string]any) { delete(m, "settled") }, ledger.StatusPending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normaliseTx(t, edit(t, tc.fn)).Status; got != tc.want {
				t.Fatalf("status %s, want %s", got, tc.want)
			}
		})
	}
}

func TestTxnDateIsTheCreatedDayInTheConfiguredZone(t *testing.T) {
	// 23:30 UTC on a June day is 00:30 the next day in London (BST).
	tx := normaliseTx(t, txFixture)
	if want := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC); !tx.TxnDate.Equal(want) {
		t.Fatalf("TxnDate %v, want %v", tx.TxnDate, want)
	}
	if tx.CreatedAt == nil || !tx.CreatedAt.Equal(time.Date(2026, 6, 15, 23, 30, 0, 123e6, time.UTC)) {
		t.Fatalf("CreatedAt %v", tx.CreatedAt)
	}
	// In winter London is UTC.
	winter := normaliseTx(t, edit(t, func(m map[string]any) { m["created"] = "2026-01-15T23:30:00Z" }))
	if want := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC); !winter.TxnDate.Equal(want) {
		t.Fatalf("winter TxnDate %v, want %v", winter.TxnDate, want)
	}
}

func TestCapabilityFlagChangeLeavesTheHashInputUnchanged(t *testing.T) {
	base := normaliseTx(t, txFixture).HashInput
	for _, k := range capabilityKeys {
		changed := normaliseTx(t, edit(t, func(m map[string]any) {
			switch v := m[k].(type) {
			case bool:
				m[k] = !v
			default:
				m[k] = "https://example.test/other"
			}
		})).HashInput
		if !bytes.Equal(base, changed) {
			t.Fatalf("changing %s changed the hash input", k)
		}
		removed := normaliseTx(t, edit(t, func(m map[string]any) { delete(m, k) })).HashInput
		if !bytes.Equal(base, removed) {
			t.Fatalf("removing %s changed the hash input", k)
		}
	}
	for _, k := range capabilityKeys {
		if bytes.Contains(base, []byte(`"`+k+`"`)) {
			t.Fatalf("hash input still holds %s", k)
		}
	}
}

func TestChangeInAmountSettledOrNotesChangesTheHashInput(t *testing.T) {
	base := normaliseTx(t, txFixture).HashInput
	for name, fn := range map[string]func(map[string]any){
		"amount":  func(m map[string]any) { m["amount"] = json.Number("-1251") },
		"settled": func(m map[string]any) { m["settled"] = "" },
		"notes":   func(m map[string]any) { m["notes"] = "dinner" },
		"nested":  func(m map[string]any) { m["merchant"].(map[string]any)["name"] = "Other Cafe" },
	} {
		if bytes.Equal(base, normaliseTx(t, edit(t, fn)).HashInput) {
			t.Fatalf("a change in %s left the hash input unchanged", name)
		}
	}
}

func TestHashInputIsCanonical(t *testing.T) {
	a, err := HashInput(json.RawMessage(`{ "b": 1, "a": {"d": [1, {"y":2,"x":1}], "c": "<&>"}, "can_split_the_bill": true }`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a":{"c":"<&>","d":[1,{"x":1,"y":2}]},"b":1}`; string(a) != want {
		t.Fatalf("HashInput = %s, want %s", a, want)
	}
	b, err := HashInput(json.RawMessage(`{"a":{"c":"<&>","d":[1,{"x":1,"y":2}]},"b":1,"can_split_the_bill":false}`))
	if err != nil || !bytes.Equal(a, b) {
		t.Fatalf("key order or whitespace changed the hash input: %s vs %s", a, b)
	}
}

func TestLargeIntegerAmountsSurviveExactly(t *testing.T) {
	// 2^53 + 1 cannot be held by a float64.
	obj := edit(t, func(m map[string]any) {
		m["amount"] = json.Number("9007199254740993")
		m["local_amount"] = json.Number("-9223372036854775807")
	})
	tx := normaliseTx(t, obj)
	if tx.AmountMinor != 9007199254740993 || *tx.LocalAmountMinor != -9223372036854775807 {
		t.Fatalf("amounts %d %d", tx.AmountMinor, *tx.LocalAmountMinor)
	}
	if !bytes.Contains(tx.HashInput, []byte(`"amount":9007199254740993`)) {
		t.Fatalf("hash input lost the amount's digits: %s", tx.HashInput)
	}
	if !bytes.Equal(tx.Content, []byte(obj)) {
		t.Fatal("Content changed")
	}
	frac := edit(t, func(m map[string]any) { m["amount"] = json.Number("12.5") })
	if _, err := (Normaliser{}).NormaliseResponse(ledger.Response{Endpoint: pathTransactions, Status: 200,
		Body: txPage(frac)}, time.UTC); err == nil {
		t.Fatal("a fractional amount was accepted")
	}
}

func TestMerchantAsBareIDAndCounterpartyName(t *testing.T) {
	tx := normaliseTx(t, edit(t, func(m map[string]any) {
		m["merchant"] = "merch_fixture_0002"
		m["counterparty"] = map[string]any{"name": "A Person", "account_number": "00000000"}
	}))
	if tx.MerchantID != "merch_fixture_0002" || tx.MerchantName != "" || tx.Counterparty != "A Person" {
		t.Fatalf("merchant %q %q counterparty %q", tx.MerchantID, tx.MerchantName, tx.Counterparty)
	}
	none := normaliseTx(t, edit(t, func(m map[string]any) { m["merchant"] = nil }))
	if none.MerchantID != "" {
		t.Fatalf("null merchant gave id %q", none.MerchantID)
	}
}

func TestNon2xxOrNonJSONResponseYieldsNothingOrAnErrorWithoutPanicking(t *testing.T) {
	html := []byte("<html><body><h1>502 Bad Gateway</h1>SECRET-BODY-TEXT</body></html>")
	for _, ep := range []string{pathAccounts, pathPots, pathBalance, pathTransactions, pathWebhooks, "/ping/whoami"} {
		params := map[string]any{"account_id": "acc_fixture_0001", "current_account_id": "acc_fixture_0001"}
		res, err := Normaliser{}.NormaliseResponse(ledger.Response{Endpoint: ep, Status: 502, Body: html, Params: params}, time.UTC)
		if err != nil || len(res.Accounts)+len(res.Txns)+len(res.Balances) != 0 {
			t.Fatalf("%s 502: %+v %v, want an empty result", ep, res, err)
		}
		res, err = Normaliser{}.NormaliseResponse(ledger.Response{Endpoint: ep, Status: 403, Body: []byte(`{"code":"forbidden.insufficient_permissions"}`), Params: params}, time.UTC)
		if err != nil || len(res.Accounts)+len(res.Txns)+len(res.Balances) != 0 {
			t.Fatalf("%s 403: %+v %v", ep, res, err)
		}
		for _, body := range [][]byte{html, nil, []byte(`{"transactions":`), []byte(`[]`), []byte(`{"transactions":[1]}`),
			[]byte(`{"accounts":[{}]}`), []byte(`{"pots":[{"id":"p"}]}`), []byte(`{"balance":"x"}`), []byte(`{} {}`)} {
			_, err := Normaliser{}.NormaliseResponse(ledger.Response{Endpoint: ep, Status: 200, Body: body, Params: params}, time.UTC)
			if err != nil && strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("%s: error carries body text: %v", ep, err)
			}
		}
		if ep == pathWebhooks || ep == "/ping/whoami" {
			continue
		}
		if _, err := (Normaliser{}).NormaliseResponse(ledger.Response{Endpoint: ep, Status: 200, Body: html, Params: params}, time.UTC); err == nil {
			t.Fatalf("%s: a 200 HTML body was accepted", ep)
		}
	}
}

func TestAccountTypesMapAndUnknownTypesPassThrough(t *testing.T) {
	body := `{"accounts":[
	 {"id":"acc_f_1","type":"uk_retail","description":"one","created":"2017-02-01T23:30:00Z","closed":false},
	 {"id":"acc_f_2","type":"uk_retail_joint","created":"2018-01-01T00:00:00Z"},
	 {"id":"acc_f_3","type":"uk_monzo_flex","created":"2021-01-01T00:00:00Z"},
	 {"id":"acc_f_4","type":"uk_loan","created":"2021-01-01T00:00:00Z","closed":true},
	 {"id":"acc_f_5","type":"uk_rewards","created":"2021-01-01T00:00:00Z"},
	 {"id":"acc_f_6","type":"uk_business","created":"2021-01-01T00:00:00Z"},
	 {"id":"acc_f_7","type":"uk_prepaid","created":"2016-07-01T00:00:00Z","closed":true},
	 {"id":"acc_f_8","type":"uk_something_new","currency":"EUR","created":"2025-07-01T23:30:00Z"}]}`
	res, err := Normaliser{}.NormaliseResponse(ledger.Response{Endpoint: pathAccounts, Status: 200, Body: []byte(body)}, london(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"current", "joint", "flex", "loan", "rewards", "business", "prepaid", "uk_something_new"}
	if len(res.Accounts) != len(want) {
		t.Fatalf("%d accounts", len(res.Accounts))
	}
	for i, a := range res.Accounts {
		if a.Type != want[i] {
			t.Fatalf("account %d type %q, want %q", i, a.Type, want[i])
		}
	}
	a0, a3, a7 := res.Accounts[0], res.Accounts[3], res.Accounts[7]
	if a0.Name != "one" || a0.Currency != "GBP" || a0.Closed || !a0.OpenedOn.Equal(time.Date(2017, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("first account %+v", a0)
	}
	if !a3.Closed || a7.Currency != "EUR" || !a7.OpenedOn.Equal(time.Date(2025, 7, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("closed %v currency %q opened %v", a3.Closed, a7.Currency, a7.OpenedOn)
	}
}

func TestPotsBecomeChildAccountsWithABalanceEach(t *testing.T) {
	at := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	body := `{"pots":[{"id":"pot_f_1","name":"Holiday","balance":12345,"currency":"GBP","deleted":false},
	 {"id":"pot_f_2","name":"Old","balance":0,"currency":"GBP","deleted":true}]}`
	for _, params := range []map[string]any{{"current_account_id": "acc_f_1"}, {"current_account_id": []any{"acc_f_1"}}} {
		res, err := Normaliser{}.NormaliseResponse(ledger.Response{Endpoint: pathPots, Status: 200, Body: []byte(body),
			Params: params, FetchedAt: at}, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Accounts) != 2 || len(res.Balances) != 2 {
			t.Fatalf("%d accounts %d balances", len(res.Accounts), len(res.Balances))
		}
		p1, p2 := res.Accounts[0], res.Accounts[1]
		if p1.Type != "pot" || p1.ParentSourceKey != "acc_f_1" || p1.Name != "Holiday" || p1.Closed || !p2.Closed {
			t.Fatalf("pots %+v %+v", p1, p2)
		}
		if b := res.Balances[0]; b.AccountSourceKey != "pot_f_1" || b.BalanceMinor != 12345 || !b.At.Equal(at) {
			t.Fatalf("pot balance %+v", b)
		}
	}
}

func TestBalanceGivesOneReadingForTheRequestedAccount(t *testing.T) {
	at := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	res, err := Normaliser{}.NormaliseResponse(ledger.Response{Endpoint: pathBalance, Status: 200,
		Body:   []byte(`{"balance":-500,"total_balance":12000,"currency":"GBP","spend_today":0}`),
		Params: map[string]any{"account_id": "acc_f_1"}, FetchedAt: at}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Balances) != 1 {
		t.Fatalf("%d balances", len(res.Balances))
	}
	b := res.Balances[0]
	if b.AccountSourceKey != "acc_f_1" || b.BalanceMinor != -500 || b.TotalBalanceMinor == nil || *b.TotalBalanceMinor != 12000 ||
		b.Currency != "GBP" || !b.At.Equal(at) {
		t.Fatalf("balance %+v", b)
	}
}

func TestParamsAreStoredAsStringsOrArrays(t *testing.T) {
	b, err := encodeParams(map[string][]string{"account_id": {"acc_f_1"}, "expand[]": {"merchant", "other"}, "empty": {}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"account_id":"acc_f_1","expand[]":["merchant","other"]}`; string(b) != want {
		t.Fatalf("params %s, want %s", b, want)
	}
}

package truelayer

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/ledger"
)

const testCard = "card:5678"

// cardParams are the request parameters of a per-card response: the card key as bank_ref.
func cardParams() map[string]any {
	return map[string]any{"bank_ref": testCard, "account_id": "card-0001"}
}

const cardsBody = `{"results":[
 {"account_id":"c1","card_network":"VISA","card_type":"CREDIT","currency":"GBP","display_name":"MADE UP CARD",
  "partial_card_number":"5678","name_on_card":"A MADE UP","valid_from":"2023-01-01","valid_to":"2028-12-31",
  "update_timestamp":"2026-11-11T12:00:00+00:00"},
 {"account_id":"c2","card_type":"CREDIT","display_name":"OTHER MADE UP CARD","partial_card_number":"XXXX XXXX XXXX 9012"},
 {"account_id":"c3","display_name":"NO DIGITS","partial_card_number":"XXXX"},
 {"account_id":"c4","display_name":"NO NUMBER"}
]}`

// usableCards keys each card by the last four digits of its partial number and leaves out a card
// with no four trailing digits. A card whose last four another card in the same list shares is left
// out together with that card. Every key seen is listed, the skipped ones included.
func TestUsableCards(t *testing.T) {
	got, err := usableCards([]byte(cardsBody))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cards) != 2 || got.Cards[0].Key != "card:5678" || got.Cards[1].Key != "card:9012" ||
		got.Cards[0].ID != "c1" || got.Cards[0].DisplayName != "MADE UP CARD" || got.NoDigits != 2 || got.Shared != 0 {
		t.Fatalf("usable cards = %+v", got)
	}
	if !slices.Equal(got.Keys, []string{"card:5678", "card:9012"}) {
		t.Errorf("keys listed = %v", got.Keys)
	}

	shared, err := usableCards([]byte(`{"results":[
	  {"account_id":"a","partial_card_number":"1111 5678"},{"account_id":"b","partial_card_number":"2222 5678"},
	  {"account_id":"c","partial_card_number":"9012"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(shared.Cards) != 1 || shared.Cards[0].Key != "card:9012" || shared.Shared != 2 || shared.NoDigits != 0 {
		t.Fatalf("shared last four: %+v", shared)
	}
	if !slices.Equal(shared.Keys, []string{"card:5678", "card:9012"}) {
		t.Errorf("a card skipped for a shared last four is not listed: %v", shared.Keys)
	}

	if _, err := usableCards([]byte(`not json`)); !errors.Is(err, errUnexpectedBody) {
		t.Errorf("an unreadable body: %v", err)
	}
	if _, err := usableCards([]byte(`{"results":[{"partial_card_number":"5678"}]}`)); !errors.Is(err, errUnexpectedBody) {
		t.Errorf("a card with no id: %v", err)
	}
}

// A Lloyds card becomes a credit_card account keyed card:<last4>, with no bank reference, carrying
// the card object as its content; a card list marker names every key the list held.
func TestLloydsCardsBecomeCreditCardAccounts(t *testing.T) {
	res, err := Normaliser{Connection: ConnLloyds}.NormaliseResponse(resp(pathCards, cardsBody, nil), london(t))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal([]byte(cardsBody), &doc); err != nil {
		t.Fatal(err)
	}
	if len(res.Accounts) != 2 {
		t.Fatalf("accounts = %+v", res.Accounts)
	}
	for i, want := range []struct{ key, name string }{{"card:5678", "MADE UP CARD"}, {"card:9012", "OTHER MADE UP CARD"}} {
		a := res.Accounts[i]
		if a.SourceKey != want.key || a.Type != "credit_card" || a.Currency != "GBP" || a.Name != want.name || a.BankRef != "" ||
			!bytes.Equal(a.Content, doc.Results[i]) {
			t.Errorf("account %d = %+v", i, a)
		}
	}
	wantList := []ledger.CardList{{Source: "truelayer:ob-lloyds", Keys: []string{"card:5678", "card:9012"}}}
	if !reflect.DeepEqual(res.CardLists, wantList) {
		t.Errorf("card lists = %+v, want %+v", res.CardLists, wantList)
	}
	// An empty list is still a list: it says no card is held.
	res, err = Normaliser{Connection: ConnLloyds}.NormaliseResponse(resp(pathCards, `{"results":[]}`, nil), london(t))
	if err != nil || len(res.Accounts) != 0 || len(res.CardLists) != 1 || len(res.CardLists[0].Keys) != 0 {
		t.Errorf("empty list: %+v, %v", res, err)
	}
	// The Monzo connection keeps the list raw and projects nothing from it.
	res, err = Normaliser{Connection: ConnMonzo}.NormaliseResponse(resp(pathCards, cardsBody, nil), london(t))
	if err != nil || !reflect.DeepEqual(res, ledger.Result{}) {
		t.Errorf("ob-monzo cards = %+v, %v", res, err)
	}
}

func cardTx(typ, amount string) string {
	t := ""
	if typ != "" {
		t = `"transaction_type":"` + typ + `",`
	}
	return `{"results":[{"transaction_id":"t-1","timestamp":"2026-11-02T00:00:00+00:00","description":"MADE UP SHOP",` + t +
		`"amount":` + amount + `,"currency":"GBP","normalised_provider_transaction_id":"cn-1","meta":{"cardNumber":"XXXXXXXXXXXX5678"}}]}`
}

// A card is a liability: a purchase is negative and a repayment positive, whichever sign the bank
// wrote the amount with, so the sign comes from transaction_type.
func TestCardTransactionsAreSignedFromTheirType(t *testing.T) {
	for _, c := range []struct {
		typ, amount string
		want        int64
	}{
		{"DEBIT", "24.25", -2425},
		{"DEBIT", "-24.25", -2425},
		{"CREDIT", "50.00", 5000},
		{"CREDIT", "-50.00", 5000},
		{"DEBIT", "0", 0},
	} {
		res, err := Normaliser{Connection: ConnLloyds}.NormaliseResponse(resp(pathCardTransactions, cardTx(c.typ, c.amount), cardParams()), london(t))
		if err != nil {
			t.Fatalf("%s %s: %v", c.typ, c.amount, err)
		}
		if len(res.Txns) != 1 || res.Txns[0].AmountMinor != c.want || res.Txns[0].AccountSourceKey != testCard ||
			res.Txns[0].SourceKey != "cn-1" || res.Txns[0].Status != ledger.StatusSettled {
			t.Errorf("%s %s: txns = %+v, want amount %d on %s", c.typ, c.amount, res.Txns, c.want, testCard)
		}
	}
	// The row's version hash covers its type, so a type change is a new version.
	a, _ := Normaliser{Connection: ConnLloyds}.NormaliseResponse(resp(pathCardTransactions, cardTx("DEBIT", "5.00"), cardParams()), london(t))
	b, _ := Normaliser{Connection: ConnLloyds}.NormaliseResponse(resp(pathCardTransactions, cardTx("CREDIT", "5.00"), cardParams()), london(t))
	if len(a.Txns) != 1 || len(b.Txns) != 1 || bytes.Equal(a.Txns[0].HashInput, b.Txns[0].HashInput) {
		t.Error("a debit and a credit of the same amount hash alike")
	}
}

// A card transaction with no transaction_type, or one that is neither DEBIT nor CREDIT, rejects the
// whole response as an unexpected body, as other malformed responses are.
func TestCardTransactionWithoutAUsableTypeRejectsTheResponse(t *testing.T) {
	for _, typ := range []string{"", "OTHER", "debit"} {
		_, err := Normaliser{Connection: ConnLloyds}.NormaliseResponse(resp(pathCardTransactions, cardTx(typ, "1.00"), cardParams()), london(t))
		if !errors.Is(err, errUnexpectedBody) {
			t.Errorf("type %q: err = %v, want an unexpected body", typ, err)
		}
	}
	// Settled rows of an account are not signed from their type.
	res, err := Normaliser{Connection: ConnLloyds}.NormaliseResponse(resp(pathTransactions, txBody, account()), london(t))
	if err != nil || res.Txns[0].AmountMinor != -1230 || res.Txns[1].AmountMinor != 10 {
		t.Errorf("account transactions: %+v, %v", res.Txns, err)
	}
}

// The balance of a card is the negative of the amount owed (current).
func TestCardBalanceIsMinusWhatIsOwed(t *testing.T) {
	body := func(current string) string {
		return `{"results":[{"currency":"GBP","available":900.00,"current":` + current + `,"credit_limit":1500.00,` +
			`"last_statement_balance":80.00,"update_timestamp":"2026-11-11T11:00:00+00:00"}]}`
	}
	for current, want := range map[string]int64{"100.00": -10000, "0": 0, "-5.50": 550} {
		res, err := Normaliser{Connection: ConnLloyds}.NormaliseResponse(resp(pathCardBalance, body(current), cardParams()), london(t))
		if err != nil {
			t.Fatalf("current %s: %v", current, err)
		}
		if len(res.Balances) != 1 || res.Balances[0].BalanceMinor != want || res.Balances[0].AccountSourceKey != testCard ||
			res.Balances[0].Currency != "GBP" || !res.Balances[0].At.Equal(time.Date(2026, 11, 11, 11, 0, 0, 0, time.UTC)) {
			t.Errorf("current %s: balances = %+v, want %d on %s", current, res.Balances, want, testCard)
		}
	}
	if _, err := (Normaliser{Connection: ConnLloyds}).NormaliseResponse(resp(pathCardBalance, `{"results":[{"currency":"GBP"}]}`, cardParams()), london(t)); !errors.Is(err, errUnexpectedBody) {
		t.Errorf("a balance with no current amount: %v", err)
	}
	// The balance of an account keeps its sign.
	res, err := Normaliser{Connection: ConnLloyds}.NormaliseResponse(resp(pathBalance,
		`{"results":[{"currency":"GBP","current":100.00}]}`, account()), london(t))
	if err != nil || res.Balances[0].BalanceMinor != 10000 {
		t.Errorf("account balance: %+v, %v", res.Balances, err)
	}
}

const pendingBody = `{"results":[
 {"transaction_id":"p-1","timestamp":"2026-11-10T00:00:00+00:00","description":"MADE UP PENDING","amount":-7.50,"currency":"GBP",
  "transaction_type":"DEBIT","merchant_name":"Made Up","normalised_provider_transaction_id":"pn-1","provider_transaction_id":"pp-1"},
 {"transaction_id":"p-2","timestamp":"2026-11-10T09:30:00+01:00","description":"SECOND","amount":2.00,"currency":"GBP",
  "provider_transaction_id":"pp-2"},
 {"transaction_id":"p-3","timestamp":"2026-11-09T00:00:00+00:00","description":"THIRD","amount":1.00},
 {"timestamp":"2026-11-09T00:00:00+00:00","description":"NO ID","amount":9.99},
 {"transaction_id":"p-3","timestamp":"2026-11-09T00:00:00+00:00","description":"REPEATED ID","amount":8.88}
]}`

// An account's pending response becomes one snapshot: amounts as the bank gave them, ids by the
// settled-row order with transaction_id as the last resort, a row with no id or a repeated id left
// out and counted.
func TestPendingSnapshotOfAnAccount(t *testing.T) {
	r := resp(pathPending, pendingBody, account())
	r.Seq = 77
	res, err := Normaliser{Connection: ConnLloyds}.NormaliseResponse(r, london(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pending) != 1 || len(res.Txns) != 0 {
		t.Fatalf("result = %+v", res)
	}
	s := res.Pending[0]
	if s.AccountSourceKey != testRef || s.RawSeq != 77 || !s.AsOf.Equal(r.FetchedAt) || s.Skipped != 2 || len(s.Rows) != 3 {
		t.Fatalf("snapshot = %+v", s)
	}
	for i, want := range []struct {
		id     string
		amount int64
		day    time.Time
		merch  string
	}{
		{"pn-1", -750, time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC), "Made Up"},
		{"pp-2", 200, time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC), ""},
		{"p-3", 100, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), ""},
	} {
		row := s.Rows[i]
		if row.ID != want.id || row.AmountMinor != want.amount || !row.Date.Equal(want.day) || row.MerchantName != want.merch ||
			row.Currency != "GBP" || len(row.Content) == 0 {
			t.Errorf("row %d = %+v, want %+v", i, row, want)
		}
	}
	// An empty list is a snapshot with no rows: nothing was pending.
	res, err = Normaliser{Connection: ConnLloyds}.NormaliseResponse(resp(pathPending, `{"results":[]}`, account()), london(t))
	if err != nil || len(res.Pending) != 1 || len(res.Pending[0].Rows) != 0 || res.Pending[0].AccountSourceKey != testRef {
		t.Errorf("empty pending: %+v, %v", res, err)
	}
	// The Monzo connection keeps pending raw and projects nothing from it.
	res, err = Normaliser{Connection: ConnMonzo}.NormaliseResponse(resp(pathPending, pendingBody, account()), london(t))
	if err != nil || !reflect.DeepEqual(res, ledger.Result{}) {
		t.Errorf("ob-monzo pending = %+v, %v", res, err)
	}
	// A pending response with no bank reference cannot be attached to an account.
	if _, err := (Normaliser{Connection: ConnLloyds}).NormaliseResponse(resp(pathPending, pendingBody, nil), london(t)); !errors.Is(err, errUnexpectedBody) {
		t.Errorf("no bank reference: %v", err)
	}
}

// A card's pending rows are signed as its settled rows are, from transaction_type.
func TestPendingSnapshotOfACardIsSignedFromTheType(t *testing.T) {
	body := `{"results":[
	 {"transaction_id":"c-1","timestamp":"2026-11-10T00:00:00+00:00","description":"A","amount":5.00,"transaction_type":"DEBIT"},
	 {"transaction_id":"c-2","timestamp":"2026-11-10T00:00:00+00:00","description":"B","amount":-6.00,"transaction_type":"DEBIT"},
	 {"transaction_id":"c-3","timestamp":"2026-11-10T00:00:00+00:00","description":"C","amount":7.00,"transaction_type":"CREDIT"}]}`
	res, err := Normaliser{Connection: ConnLloyds}.NormaliseResponse(resp(pathCardPending, body, cardParams()), london(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pending) != 1 || res.Pending[0].AccountSourceKey != testCard || len(res.Pending[0].Rows) != 3 {
		t.Fatalf("result = %+v", res)
	}
	var got []int64
	for _, r := range res.Pending[0].Rows {
		got = append(got, r.AmountMinor)
	}
	if !slices.Equal(got, []int64{-500, -600, 700}) {
		t.Errorf("amounts = %v, want [-500 -600 700]", got)
	}
	// A card row with no usable type rejects the response.
	bad := `{"results":[{"transaction_id":"c-1","timestamp":"2026-11-10T00:00:00+00:00","description":"A","amount":5.00}]}`
	if _, err := (Normaliser{Connection: ConnLloyds}).NormaliseResponse(resp(pathCardPending, bad, cardParams()), london(t)); !errors.Is(err, errUnexpectedBody) {
		t.Errorf("a card pending row with no type: %v", err)
	}
}

// Nothing is read from /info.
func TestInfoYieldsNothing(t *testing.T) {
	for _, conn := range []string{ConnLloyds, ConnMonzo} {
		for _, body := range []string{`{"results":[{"full_name":"Made Up Person"}]}`, `not json`} {
			res, err := Normaliser{Connection: conn}.NormaliseResponse(resp(pathInfo, body, nil), london(t))
			if err != nil || !reflect.DeepEqual(res, ledger.Result{}) {
				t.Errorf("%s /info %q = %+v, %v", conn, body, res, err)
			}
		}
	}
}

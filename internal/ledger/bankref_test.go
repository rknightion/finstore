package ledger_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/importer"
	"github.com/rknightion/finstore/internal/importer/importertest"
	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/monzo"
)

func TestBankRefIsDigitsOnlyJoinedByASpace(t *testing.T) {
	for _, tc := range []struct{ sortCode, number, want string }{
		{"11-22-33", "12345678", "112233 12345678"},
		{"112233", "1234 5678", "112233 12345678"},
		{" 11 22 33 ", "12-34-56-78", "112233 12345678"},
		{"", "", ""},
		{"-", " ", ""},
	} {
		if got := ledger.BankRef(tc.sortCode, tc.number); got != tc.want {
			t.Errorf("BankRef(%q, %q) = %q, want %q", tc.sortCode, tc.number, got, tc.want)
		}
	}
}

// A stored Monzo /accounts response sets bank_ref on the Monzo account, live and on a rebuild from
// raw; an account Monzo gives no numbers for (a loan) has none.
func TestMonzoAccountsRecordTheirBankRefLiveAndOnReplay(t *testing.T) {
	e := newEnv(t)
	accounts := `{"accounts":[
	 {"id":"acc_test_1","type":"uk_retail","description":"made up","created":"2020-01-01T00:00:00Z",
	  "closed":false,"sort_code":"990001","account_number":"12345678"},
	 {"id":"acc_test_2","type":"uk_loan","description":"made up","created":"2021-01-01T00:00:00Z","closed":false}]}`
	e.respondAs(e.fetchAs("monzo", 1, "", nil), "monzo", "/accounts", 200, accounts)
	e.catchUp()
	check := func(when string) {
		t.Helper()
		if a := e.account("monzo-current"); a.BankRef == nil || *a.BankRef != "990001 12345678" {
			t.Fatalf("%s: monzo-current bank_ref %v", when, a.BankRef)
		}
		if a := e.account("monzo-loan"); a.BankRef != nil {
			t.Fatalf("%s: monzo-loan bank_ref %v, want none", when, *a.BankRef)
		}
	}
	check("live")
	if diffs := e.renormalise(true); len(diffs) != 0 {
		t.Fatalf("rebuild differs: %+v", diffs)
	}
	e.renormalise(false)
	check("after renormalise")
}

// One made-up bank account as each source shapes it: a Halifax PDF statement, a Monzo /accounts
// object and a TrueLayer /data/v1/accounts object. All three give the same bank reference.
func TestBankRefIsTheSameForAStatementAMonzoAccountAndATrueLayerAccount(t *testing.T) {
	const want = "112233 12345678"

	g := importertest.New(10000, halifaxRows(time.January))
	g.Start = time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
	g.End = time.Date(2030, time.January, 31, 0, 0, 0, 0, time.UTC)
	g.SortCode, g.Account = "11-22-33", "12345678"
	fr, err := importer.Normaliser{}.NormaliseFile(ledger.File{AccountSlug: "bank-joint", Format: "halifax-pdf", Body: g.PDF()})
	if err != nil {
		t.Fatalf("statement: %v", err)
	}
	if fr.AccountRef != want {
		t.Errorf("statement gives %q, want %q", fr.AccountRef, want)
	}

	monzoBody := `{"accounts":[{"id":"acc_test_1","type":"uk_retail_joint","description":"made up",
		"created":"2020-01-01T00:00:00Z","closed":false,"sort_code":"112233","account_number":"12345678"}]}`
	res, err := monzo.Normaliser{}.NormaliseResponse(ledger.Response{Endpoint: "/accounts", Status: 200, Body: []byte(monzoBody)}, time.UTC)
	if err != nil {
		t.Fatalf("monzo: %v", err)
	}
	if len(res.Accounts) != 1 || res.Accounts[0].BankRef != want {
		t.Errorf("monzo account gives %+v, want bank ref %q", res.Accounts, want)
	}

	// TrueLayer's account object, as its Data API v1 documents it, with made-up values. No
	// TrueLayer package exists yet; its normaliser will read these two fields and call BankRef.
	trueLayerBody := `{"results":[{"account_id":"made-up-account-id","account_type":"TRANSACTION",
		"display_name":"Joint","currency":"GBP","account_number":{"iban":"GB00MADE11223312345678",
		"number":"12345678","sort_code":"11-22-33","swift_bic":"MADEGB2L"},
		"update_timestamp":"2030-01-31T00:00:00Z"}]}`
	var tl struct {
		Results []struct {
			AccountNumber struct {
				Number   string `json:"number"`
				SortCode string `json:"sort_code"`
			} `json:"account_number"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(trueLayerBody), &tl); err != nil || len(tl.Results) != 1 {
		t.Fatalf("truelayer body: %v", err)
	}
	if got := ledger.BankRef(tl.Results[0].AccountNumber.SortCode, tl.Results[0].AccountNumber.Number); got != want {
		t.Errorf("truelayer account gives %q, want %q", got, want)
	}
}

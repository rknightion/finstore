package truelayer

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

// Each Lloyds account carries the account object as the bank listed it, byte for byte, as its
// detail. An account with no sort code and number is left out, as before.
func TestLloydsAccountDetail(t *testing.T) {
	res, err := Normaliser{Connection: ConnLloyds}.NormaliseResponse(resp(pathAccounts, accountsBody, nil), london(t))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal([]byte(accountsBody), &doc); err != nil {
		t.Fatal(err)
	}
	if len(res.Accounts) != 4 {
		t.Fatalf("%d accounts, want 4", len(res.Accounts))
	}
	for i, a := range res.Accounts {
		if !bytes.Equal(a.Content, doc.Results[i]) {
			t.Errorf("account %d content = %s, want the listed object %s", i, a.Content, doc.Results[i])
		}
	}
}

// After a connect, agent.accounts.detail is the account object for each Lloyds account.
func TestAgentAccountsDetailForLloydsAccounts(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	for slug, want := range map[string]struct{ name, sortCode, number string }{
		"lloyds-joint":   {"Joint", "11-22-33", "12345678"},
		"lloyds-current": {"Classic", "112233", "87654321"},
	} {
		var detail map[string]any
		if err := h.st.DB().QueryRow(context.Background(), `SELECT detail FROM agent.accounts WHERE slug = $1`, slug).Scan(&detail); err != nil {
			t.Fatalf("%s: %v", slug, err)
		}
		num, _ := detail["account_number"].(map[string]any)
		if detail["display_name"] != want.name || num["sort_code"] != want.sortCode || num["number"] != want.number {
			t.Errorf("%s detail = %v, want the account object for %s", slug, detail, want.name)
		}
	}
}

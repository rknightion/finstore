package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rknightion/finstore/internal/store"
)

// agent.accounts.detail is core.account.detail for a TrueLayer account or card, and for a Monzo
// account it is still the raw Monzo object: the raw lookup wins even where the core column is set,
// and a file account has none.
func TestAccountsDetailIsTheCoreColumnForTrueLayerAccountsAndRawForMonzo(t *testing.T) {
	f := newPotFixture(t)
	ctx := context.Background()
	core := mustCore(t, f.q, store.SchemaCore)

	joint := testAccount("lloyds-joint", "joint", "api")
	joint.Source, joint.SourceKey = "truelayer:ob-lloyds", "112233 12345678"
	joint.Detail = json.RawMessage(`{"account_id":"a-1","display_name":"Joint","account_number":{"sort_code":"112233"}}`)
	card := testAccount("lloyds-card-5678", "credit_card", "api")
	card.Source, card.SourceKey = "truelayer:ob-lloyds", "card:5678"
	card.Detail = json.RawMessage(`{"account_id":"c-1","partial_card_number":"5678","card_type":"CREDIT"}`)
	for _, a := range []store.Account{joint, card} {
		if err := core.UpsertAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	// A Monzo account that also has a core detail still reads its raw Monzo object.
	monzo, err := core.GetAccountBySlug(ctx, "monzo-current")
	if err != nil {
		t.Fatal(err)
	}
	monzo.Detail = json.RawMessage(`{"description":"from core"}`)
	if err := core.UpsertAccount(ctx, monzo); err != nil {
		t.Fatal(err)
	}

	agent := connAs(t, f.super, store.RoleAgent)
	rows, err := agent.Query(ctx, `SELECT slug, detail FROM agent.accounts`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]any{}
	for rows.Next() {
		var slug string
		var d map[string]any
		if err := rows.Scan(&slug, &d); err != nil {
			t.Fatal(err)
		}
		got[slug] = d
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if d := got["lloyds-joint"]; d == nil || d["display_name"] != "Joint" || d["account_id"] != "a-1" {
		t.Errorf("lloyds-joint detail = %v, want the account object", d)
	}
	if d := got["lloyds-card-5678"]; d == nil || d["partial_card_number"] != "5678" {
		t.Errorf("lloyds-card-5678 detail = %v, want the card object", d)
	}
	if d := got["monzo-current"]; d == nil || d["description"] != "new name" {
		t.Errorf("monzo-current detail = %v, want the raw Monzo object", d)
	}
	if d, ok := got["bank-file"]; !ok || d != nil {
		t.Errorf("file account detail = %v (present %v), want null", d, ok)
	}
}

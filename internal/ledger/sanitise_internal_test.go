package ledger

import (
	"encoding/json"
	"testing"
)

func TestCleanTextRemovesNULAndRepairsUTF8(t *testing.T) {
	if got := cleanText("a\x00b\xffc"); got != "ab�c" {
		t.Fatalf("cleanText = %q", got)
	}
	if got := cleanText("plain £"); got != "plain £" {
		t.Fatalf("clean text changed: %q", got)
	}
}

func TestCleanJSONReplacesOnlyRealNULs(t *testing.T) {
	cases := map[string]string{
		`{"a":"x"}`:                      `{"a":"x"}`, // untouched, byte for byte
		`{"b":{"c":["x\u0000y"]},"a":1}`: `{"a":1,"b":{"c":["x` + "�" + `y"]}}`,
		`{"a":"\\u0000"}`:                `{"a":"\\u0000"}`, // an escaped backslash, not a NUL
		`{"k\u0000":"v"}`:                `{"k` + "�" + `":"v"}`,
		"{\"a\":\"\xff\"}":               `{"a":"` + "�" + `"}`,
		`{"n":1.50}`:                     `{"n":1.50}`,
	}
	for in, want := range cases {
		got := cleanJSON(json.RawMessage(in))
		if string(got) != want {
			t.Errorf("cleanJSON(%q) = %q, want %q", in, got, want)
		}
	}
}

// The account object kept as detail is cleaned like every other stored JSON: a U+0000 in a bank body
// cannot fail the jsonb insert.
func TestSanitiseCleansAccountContent(t *testing.T) {
	r := sanitiseResult(Result{Accounts: []AccountRec{{SourceKey: "k", Type: "current",
		Content: json.RawMessage(`{"display_name":"a\u0000b"}`)}}})
	if got, want := string(r.Accounts[0].Content), `{"display_name":"a`+"�"+`b"}`; got != want {
		t.Fatalf("Content = %s, want %s", got, want)
	}
}

// The pending snapshots and card lists a normaliser returns are cleaned like every other field: NUL
// removed from text, invalid UTF-8 replaced, U+0000 in the row's content replaced, so neither can
// fail the insert and stop the projection.
func TestSanitiseCleansPendingAndCardLists(t *testing.T) {
	r := sanitiseResult(Result{
		Pending: []PendingSnapshot{{AccountSourceKey: "a\x00b\xff", Skipped: 2, Rows: []PendingRec{{
			ID: "i\x00d\xff", Currency: "G\x00BP", Description: "d\x00e\xffsc", MerchantName: "m\x00er\xffch",
			Content: json.RawMessage(`{"description":"x\u0000y"}`)}}}},
		CardLists: []CardList{{Source: "s\x00rc\xff", Keys: []string{"card:5\x00678", "card:9\xff012"}}},
	})
	p := r.Pending[0]
	row := p.Rows[0]
	if p.AccountSourceKey != "ab�" || p.Skipped != 2 || row.ID != "id�" || row.Currency != "GBP" ||
		row.Description != "de�sc" || row.MerchantName != "mer�ch" {
		t.Errorf("pending snapshot = %+v", p)
	}
	if got, want := string(row.Content), `{"description":"x`+"�"+`y"}`; got != want {
		t.Errorf("pending content = %s, want %s", got, want)
	}
	l := r.CardLists[0]
	if l.Source != "src�" || len(l.Keys) != 2 || l.Keys[0] != "card:5678" || l.Keys[1] != "card:9�012" {
		t.Errorf("card list = %+v", l)
	}
}

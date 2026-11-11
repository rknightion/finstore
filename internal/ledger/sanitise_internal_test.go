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

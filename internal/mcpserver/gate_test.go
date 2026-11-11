package mcpserver

import (
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testCap = "example.com/cap/finstore"
	capHdr  = "Tailscale-App-Capabilities"
)

// qEncode writes s as one RFC 2047 Q-encoded word, escaping every byte that is not a letter or a
// digit, the way a proxy that must carry non-ASCII text does.
func qEncode(s string) string {
	var b strings.Builder
	b.WriteString("=?utf-8?q?")
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "=%02X", c)
		}
	}
	b.WriteString("?=")
	return b.String()
}

// gated runs one request through Gate with a next handler that records what it saw.
type gated struct {
	called       int
	client, peer string
}

func (g *gated) serve(t *testing.T, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.called++
		g.client, g.peer = Caller(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	Gate(testCap, next).ServeHTTP(rec, req)
	return rec
}

func withCaps(values ...string) func(*http.Request) {
	return func(r *http.Request) {
		for _, v := range values {
			r.Header.Add(capHdr, v)
		}
	}
}

// notFoundBody is what the status page's allow-list answers, so a caller cannot tell the two gates
// apart.
func notFoundBody(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	http.NotFound(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	return rec.Body.String()
}

// Testing 7 / AC1: everything that is not exactly one well-formed header value naming the
// capability with at least one grant value is 404, the same answer the email check gives, and the
// wrapped handler never runs.
func TestGateRefusesWithTheSameNotFound(t *testing.T) {
	good := `{"` + testCap + `":[{"client":"agents"}]}`
	tests := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"no header", nil},
		{"two headers", withCaps(good, good)},
		{"an empty value", withCaps("")},
		{"a blank value", withCaps("   ")},
		{"malformed JSON", withCaps(`{"` + testCap + `":[{"client":`)},
		{"not an object", withCaps(`["` + testCap + `"]`)},
		{"JSON null", withCaps(`null`)},
		{"a missing capability", withCaps(`{"example.com/cap/other":[{"client":"agents"}]}`)},
		{"a capability of another domain", withCaps(`{"example.org/cap/finstore":[{"client":"agents"}]}`)},
		{"a capability that only starts the same", withCaps(`{"` + testCap + `x":[{"client":"agents"}]}`)},
		{"an empty value list", withCaps(`{"` + testCap + `":[]}`)},
		{"a null value list", withCaps(`{"` + testCap + `":null}`)},
		{"a value list that is not a list", withCaps(`{"` + testCap + `":{"client":"agents"}}`)},
		{"a malformed Q-encoded word", withCaps(`=?utf-8?q?=ZZ?=`)},
		{"an unsupported charset", withCaps(`=?x-unknown?q?abc?=`)},
		{"a Q-encoded word that is not the capability", withCaps(qEncode(`{"example.com/cap/other":[{}]}`))},
		{"the capability in a different header", func(r *http.Request) { r.Header.Set("X-Capabilities", good) }},
		{"the email header of the status page", func(r *http.Request) { r.Header.Set("X-Auth-Request-Email", "owner@example.com") }},
	}
	want := notFoundBody(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var g gated
			rec := g.serve(t, tt.mutate)
			if rec.Code != http.StatusNotFound || rec.Body.String() != want {
				t.Errorf("status %d body %q, want 404 %q", rec.Code, rec.Body.String(), want)
			}
			if g.called != 0 {
				t.Error("the wrapped handler ran")
			}
		})
	}
}

// AC1: a cross-origin browser request is 404 even with the capability, so a page on another site
// cannot make a permitted device's browser post to /mcp (a CSRF guard). A request with neither
// Sec-Fetch-Site nor Origin, which is what an MCP client sends, and a same-origin one pass.
func TestGateRefusesCrossOriginBrowserRequests(t *testing.T) {
	good := `{"` + testCap + `":[{"client":"agents"}]}`
	tests := []struct {
		name    string
		header  map[string]string
		wantOK  bool
		comment string
	}{
		{"an MCP client", nil, true, "sends neither header"},
		{"same origin", map[string]string{"Sec-Fetch-Site": "same-origin"}, true, ""},
		{"a direct navigation", map[string]string{"Sec-Fetch-Site": "none"}, true, ""},
		{"cross-site fetch", map[string]string{"Sec-Fetch-Site": "cross-site"}, false, ""},
		{"same-site fetch", map[string]string{"Sec-Fetch-Site": "same-site"}, false, ""},
		{"another origin", map[string]string{"Origin": "https://evil.example.org"}, false, "no Sec-Fetch-Site, an Origin that is not the host"},
		{"a page on another site", map[string]string{"Origin": "http://finance.example.com", "Sec-Fetch-Site": "cross-site"}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var g gated
			rec := g.serve(t, func(r *http.Request) {
				withCaps(good)(r)
				r.Host = "finance.example.com"
				for k, v := range tt.header {
					r.Header.Set(k, v)
				}
			})
			if tt.wantOK {
				if rec.Code != http.StatusNoContent || g.called != 1 {
					t.Errorf("status %d, called %d, want it to pass", rec.Code, g.called)
				}
				return
			}
			if rec.Code != http.StatusNotFound || rec.Body.String() != notFoundBody(t) || g.called != 0 {
				t.Errorf("status %d body %q called %d, want 404 with the standard body", rec.Code, rec.Body.String(), g.called)
			}
		})
	}
}

// AC1: the client label is every distinct client string in the grant values, sorted and joined by
// a comma, or "unknown" when none has one.
func TestGateReportsTheClientGroups(t *testing.T) {
	val := func(values string) string { return `{"` + testCap + `":` + values + `}` }
	tests := []struct {
		name, header, want string
	}{
		{"one value", val(`[{"client":"agents"}]`), "agents"},
		{"two grant values", val(`[{"client":"b"},{"client":"a"}]`), "a,b"},
		{"a repeated value", val(`[{"client":"a"},{"client":"a"},{"client":"b"}]`), "a,b"},
		{"no client key", val(`[{}]`), "unknown"},
		{"an empty client", val(`[{"client":""}]`), "unknown"},
		{"a client that is not a string", val(`[{"client":7}]`), "unknown"},
		{"values that are not objects", val(`[1,"x",null]`), "unknown"},
		{"one value with a client, one without", val(`[{"role":"x"},{"client":"agents"}]`), "agents"},
		{"another capability beside ours", `{"example.com/cap/other":[{"client":"zzz"}],"` + testCap + `":[{"client":"agents"}]}`, "agents"},
		{"another capability that is not a list", `{"example.com/cap/other":"x","` + testCap + `":[{"client":"agents"}]}`, "agents"},
		{"Q-encoded", qEncode(val(`[{"client":"b"},{"client":"a"}]`)), "a,b"},
		{"Q-encoded non-ASCII", qEncode(val(`[{"client":"agentà"}]`)), "agentà"},
		{"JSON with a space after the colon", `{ "` + testCap + `" : [ { "client" : "agents" } ] }`, "agents"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var g gated
			rec := g.serve(t, withCaps(tt.header))
			if rec.Code != http.StatusNoContent || g.called != 1 {
				t.Fatalf("status %d called %d, want the request to pass", rec.Code, g.called)
			}
			if g.client != tt.want {
				t.Errorf("client = %q, want %q", g.client, tt.want)
			}
		})
	}
}

// The encoder the proxy uses (mime.QEncoding) splits a long value into several encoded words, and
// those decode too.
func TestGateDecodesWordsFromTheStandardLibraryEncoder(t *testing.T) {
	json := `{"` + testCap + `":[{"client":"` + strings.Repeat("à", 40) + `"},{"client":"zz"}]}`
	enc := mime.QEncoding.Encode("utf-8", json)
	if !strings.HasPrefix(enc, "=?utf-8?q?") {
		t.Fatalf("encoder did not encode: %q", enc)
	}
	// The encoder separates the words of a long value with a plain space, so what it returns is a
	// valid header value as it is.
	if strings.Count(enc, "=?utf-8?q?") < 2 {
		t.Fatalf("a long value was not split into several words: %q", enc)
	}
	var g gated
	g.serve(t, withCaps(enc))
	if g.called != 1 || g.client != "zz,"+strings.Repeat("à", 40) {
		t.Errorf("called %d, client %q", g.called, g.client)
	}
}

// The peer is the first X-Forwarded-For address when it parses as one. It is for the log only.
func TestGateReportsThePeerFromTheFirstForwardedAddress(t *testing.T) {
	good := `{"` + testCap + `":[{"client":"agents"}]}`
	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{"none", nil, ""},
		{"one address", []string{"100.64.0.7"}, "100.64.0.7"},
		{"a list", []string{"100.64.0.7, 10.0.0.1"}, "100.64.0.7"},
		{"two headers", []string{"100.64.0.7", "10.0.0.1"}, "100.64.0.7"},
		{"IPv6", []string{"fd7a:115c:a1e0::1"}, "fd7a:115c:a1e0::1"},
		{"not an address", []string{"not-an-ip"}, ""},
		{"text a caller could forge into a log line", []string{"192.0.2.4\"}, \"outcome\":\"ok"}, ""},
		{"an empty value", []string{""}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var g gated
			rec := g.serve(t, func(r *http.Request) {
				withCaps(good)(r)
				for _, v := range tt.values {
					r.Header.Add("X-Forwarded-For", v)
				}
			})
			if rec.Code != http.StatusNoContent || g.client != "agents" {
				t.Fatalf("status %d client %q", rec.Code, g.client)
			}
			if g.peer != tt.want {
				t.Errorf("peer = %q, want %q", g.peer, tt.want)
			}
		})
	}
}

// A call that did not come through the gate has no caller to name.
func TestCallerWithoutTheGate(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if c, p := Caller(req.Context()); c != "" || p != "" {
		t.Errorf("Caller = %q, %q, want empty", c, p)
	}
}

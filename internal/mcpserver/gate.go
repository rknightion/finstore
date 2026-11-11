// Package mcpserver serves finstore's MCP endpoint: the gate that admits only callers a tailnet
// grant names, and the server with its five read-only tools.
//
// Every route here answers 404, never 403, to a caller it refuses, as the status page's allow-list
// does: the proxy in front may rewrite a 403 into a login redirect.
package mcpserver

import (
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// capsHeader is the header the Tailscale proxy sets from the app capabilities the caller's grant
// gives it. The proxy removes any copy a client sends, so nothing but a grant can set it.
const capsHeader = "Tailscale-App-Capabilities"

type callerKey struct{}

type caller struct{ client, peer string }

// Caller returns the client label and the peer address that Gate put in ctx: the grant group of
// the caller (every distinct client name in the grant's values, sorted and joined by a comma, or
// "unknown") and its tailnet address when the proxy sent one, else "". Both are empty for a
// context that did not come through Gate.
func Caller(ctx context.Context) (client, peer string) {
	c, _ := ctx.Value(callerKey{}).(caller)
	return c.client, c.peer
}

// Gate admits a request only when exactly one Tailscale-App-Capabilities header value, a JSON
// object of capability name to a list of grant values, holds capName with at least one value, and
// answers 404 with the status page's body to everything else. The value may be RFC 2047
// Q-encoded, as the proxy sends text that is not ASCII.
//
// An admitted request carries its Caller in the context, and passes next only when it is not a
// cross-origin browser request. That is a CSRF guard: a page a browser loaded from another site
// cannot make the browser of a permitted device post to /mcp. It is not what stops DNS rebinding,
// which needs the node's TLS certificate (a rebound name has none) and the proxy's host-keyed
// configuration. An MCP client sends neither Sec-Fetch-Site nor Origin and passes.
func Gate(capName string, next http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(http.NotFound))
	guarded := cop.Handler(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client, ok := clientGroup(r.Header.Values(capsHeader), capName)
		if !ok {
			http.NotFound(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), callerKey{}, caller{client: client, peer: forwardedPeer(r)})
		guarded.ServeHTTP(w, r.WithContext(ctx))
	})
}

// clientGroup reads the one header value and returns the client label, and whether the value
// grants capName.
func clientGroup(values []string, capName string) (string, bool) {
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		return "", false
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(values[0])
	if err != nil {
		return "", false
	}
	var caps map[string]json.RawMessage
	if err := json.Unmarshal([]byte(decoded), &caps); err != nil {
		return "", false
	}
	// Only the entry for capName is read, so another capability whose value has some other shape
	// cannot lock a caller out.
	var grants []json.RawMessage
	if err := json.Unmarshal(caps[capName], &grants); err != nil || len(grants) == 0 {
		return "", false
	}
	var clients []string
	for _, g := range grants {
		var v struct {
			Client string `json:"client"`
		}
		// A value that is not an object, or whose client is not a string, names no client.
		if json.Unmarshal(g, &v) == nil && v.Client != "" && !slices.Contains(clients, v.Client) {
			clients = append(clients, v.Client)
		}
	}
	if len(clients) == 0 {
		return "unknown", true
	}
	slices.Sort(clients)
	return strings.Join(clients, ","), true
}

// forwardedPeer returns the first X-Forwarded-For address when it parses as one, else "". It is
// for the log only, never a metric attribute, and a value that is not an address is dropped so a
// caller cannot write text into a log line.
func forwardedPeer(r *http.Request) string {
	values := r.Header.Values("X-Forwarded-For")
	if len(values) == 0 {
		return ""
	}
	first, _, _ := strings.Cut(values[0], ",")
	addr, err := netip.ParseAddr(strings.TrimSpace(first))
	if err != nil {
		return ""
	}
	return addr.String()
}

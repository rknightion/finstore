package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rknightion/finstore/internal/app"
	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/mcpserver"
	"github.com/rknightion/finstore/internal/monzo"
	"github.com/rknightion/finstore/internal/monzotest"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry/telemetrytest"
)

const mcpCap = "example.com/cap/finstore"

// mcpGrant is the Tailscale-App-Capabilities value the proxy sets for a caller of the agents group.
const mcpGrant = `{"` + mcpCap + `":[{"client":"agents"}]}`

// served is a running `serve`.
type served struct {
	addr   string
	stderr *syncBuffer
	worker *monzo.Worker
	stop   func() int
}

// startServe runs serve with cfg and waits until it listens. It fails the test when serve exits
// first; see tryServe for a start that is expected to fail.
func startServe(t *testing.T, cfg config.Config, w *world, noSchedule bool) *served {
	t.Helper()
	s, code := tryServe(t, cfg, w, noSchedule)
	if s == nil {
		t.Fatalf("serve exited %d before listening", code)
	}
	return s
}

// tryServe runs serve with cfg. It returns the running server once it listens, or nil and the exit
// code when serve exits first.
func tryServe(t *testing.T, cfg config.Config, w *world, noSchedule bool) (*served, int) {
	t.Helper()
	s := &served{stderr: &syncBuffer{}}
	var approve sync.Once
	listening := make(chan net.Addr, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- app.Serve(ctx, app.Env{Cfg: cfg, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: s.stderr, Version: testVersion, Now: w.fake.Now},
			&app.Hooks{
				NoSchedule: noSchedule,
				Listening:  func(a net.Addr) { listening <- a },
				Worker:     func(mw *monzo.Worker) { s.worker = mw },
				MonzoOptions: func(o *monzo.Options) {
					o.Sleep = func(ctx context.Context, _ time.Duration) error {
						approve.Do(w.fake.Approve)
						return ctx.Err()
					}
				},
			})
	}()
	stopped := false
	s.stop = func() int {
		if stopped {
			return -1
		}
		stopped = true
		cancel()
		select {
		case code := <-done:
			return code
		case <-time.After(60 * time.Second):
			t.Error("serve did not stop")
			return -1
		}
	}
	t.Cleanup(func() { s.stop() })
	select {
	case a := <-listening:
		s.addr = a.String()
		return s, 0
	case code := <-done:
		stopped = true
		cancel()
		return nil, code
	case <-time.After(30 * time.Second):
		t.Fatal("serve neither listened nor exited in 30 s")
		return nil, 0
	}
}

// capClient sends every request with the headers the proxy would add.
type capClient map[string]string

func (c capClient) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range c {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func mcpSession(t *testing.T, addr string, headers map[string]string) (*mcp.ClientSession, error) {
	t.Helper()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e-client", Version: "1"}, nil).Connect(context.Background(),
		&mcp.StreamableClientTransport{
			Endpoint: "http://" + addr + "/mcp", HTTPClient: &http.Client{Transport: capClient(headers), Timeout: 60 * time.Second},
			DisableStandaloneSSE: true,
		}, nil)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

func structured(t *testing.T, res *mcp.CallToolResult, out any) {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool error: %v", res.Content)
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
}

// mcpConfig is a world whose serve has the MCP endpoint on: bootstrap gave finstore_mcp its login,
// and the config names its URL and the capability.
func mcpConfig(t *testing.T, sink *telemetrytest.Sink) (*world, config.Config) {
	t.Helper()
	w, cfg := newTelemetryWorld(t, sink, map[string]string{config.EnvMCPPassword: "mcp-password"})
	cfg.MCPDatabaseURL = urlAs(t, w.super, store.RoleMCP)
	cfg.MCPAppCap = mcpCap
	return w, cfg
}

func urlAs(t *testing.T, super, role string) string {
	t.Helper()
	u, err := url.Parse(super)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.User(role)
	return u.String()
}

// connectMonzo runs the connect and callback flow against the fake and waits for the full-history
// fetch, so the database holds synced data.
func connectMonzo(t *testing.T, s *served, w *world) {
	t.Helper()
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(method, path string, header map[string]string) *http.Response {
		req, err := http.NewRequest(method, "http://"+s.addr+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := do(http.MethodPost, "/connect/monzo", map[string]string{"X-Auth-Request-Email": ownerEmail, "Sec-Fetch-Site": "same-origin"})
	bank, err := url.Parse(continueLink(t, resp))
	if err != nil {
		t.Fatal(err)
	}
	state := bank.Query().Get("state")
	w.fake.RegisterCode("code_mcp_1")
	if resp := do(http.MethodGet, "/callback/monzo?code=code_mcp_1&state="+url.QueryEscape(state), nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback: %d: %s", resp.StatusCode, s.stderr.String())
	}
	s.worker.Wait()
}

const (
	mcpDescMarker  = "MCP-DESCRIPTION-MARKER-4417"
	mcpMerchMarker = "MCP-MERCHANT-MARKER-9902"
	sqlCanary      = "CANARY_SQL_TEXT_6631"
)

// Testing 9 / AC3: with the MCP variables set, bootstrap, migrate and serve answer overview and
// search_transactions over HTTP with the synced data when the capability header is present, and 404
// without it. The same run proves the telemetry rule on the real exporters: a query_sql call
// exports an mcp.query span with no db.query.text, and its SQL is in no span or metric.
func TestMCPEndToEnd(t *testing.T) {
	sink := telemetrytest.New(t)
	w, cfg := mcpConfig(t, sink)
	srv := w.fake.Start()
	t.Cleanup(srv.Close)
	cfg.MonzoAPIURL = srv.URL
	now := w.fake.Now()
	w.fake.AddAccount(monzotest.Account{ID: accountID, Type: "uk_retail", Description: "made-up", Created: now.AddDate(0, 0, -60)})
	generated := w.fake.GenerateTransactions(accountID, 40, now.AddDate(0, 0, -59), now.Add(-time.Hour), 3)
	w.fake.UpdateTransaction(generated[10].ID, func(tx *monzotest.Transaction) {
		tx.Description = mcpDescMarker
		tx.Merchant = &monzotest.Merchant{ID: "merch_mcp", Name: mcpMerchMarker, Category: "general"}
	})

	s := startServe(t, cfg, w, true)
	connectMonzo(t, s, w)

	// Without the capability header /mcp is 404, whatever else the request carries, and the pages
	// still need their own sign-in.
	for name, header := range map[string]map[string]string{
		"no header":          nil,
		"the pages' sign-in": {"X-Auth-Request-Email": ownerEmail},
		"another capability": {"Tailscale-App-Capabilities": `{"example.com/cap/other":[{"client":"agents"}]}`},
		"an empty list":      {"Tailscale-App-Capabilities": `{"` + mcpCap + `":[]}`},
		"a browser page":     {"Tailscale-App-Capabilities": mcpGrant, "Sec-Fetch-Site": "cross-site"},
	} {
		req, _ := http.NewRequest(http.MethodPost, "http://"+s.addr+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(b), "404 page not found") {
			t.Errorf("%s: /mcp answered %d %q, want 404", name, resp.StatusCode, b)
		}
	}
	if _, err := mcpSession(t, s.addr, nil); err == nil {
		t.Error("an MCP client without the capability connected")
	}
	// A caller with the capability and no identity gets 404 on the status page.
	req, _ := http.NewRequest(http.MethodGet, "http://"+s.addr+"/", nil)
	req.Header.Set("Tailscale-App-Capabilities", mcpGrant)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("the status page for a capability-only caller: %v %v", resp, err)
	}

	// With it, the five tools are there and the synced data is readable.
	cs, err := mcpSession(t, s.addr, map[string]string{"Tailscale-App-Capabilities": mcpGrant, "X-Forwarded-For": "100.64.0.9"})
	if err != nil {
		t.Fatalf("connect with the capability: %v\n%s", err, s.stderr.String())
	}
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 5 {
		t.Fatalf("tools/list: %v %v", tools, err)
	}
	call := func(tool string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return res
	}
	var ov mcpserver.Overview
	structured(t, call("overview", map[string]any{}), &ov)
	var current *mcpserver.Account
	for i := range ov.Accounts {
		if ov.Accounts[i].AccountSlug == "monzo-current" {
			current = &ov.Accounts[i]
		}
	}
	if current == nil || current.TransactionCount != 40 || current.Source != "monzo" || current.Feed != "api" || current.Currency != "GBP" ||
		current.ReconciliationState != store.StateReconciled || len(current.Gaps) != 0 {
		t.Fatalf("overview accounts = %+v", ov.Accounts)
	}
	if len(ov.Sources) == 0 || ov.Sources[0].Source != "monzo" || ov.Sources[0].ConsentState == nil || *ov.Sources[0].ConsentState != "active" {
		t.Errorf("overview sources = %+v", ov.Sources)
	}
	var found mcpserver.SearchOutput
	structured(t, call("search_transactions", map[string]any{"text": strings.ToLower(mcpDescMarker)}), &found)
	if len(found.Transactions) != 1 || found.Transactions[0].Description != mcpDescMarker || found.Transactions[0].AccountSlug != "monzo-current" ||
		found.Transactions[0].MerchantName == nil || *found.Transactions[0].MerchantName != mcpMerchMarker {
		t.Fatalf("search = %+v", found)
	}
	var all mcpserver.SearchOutput
	structured(t, call("search_transactions", map[string]any{"accounts": []string{"monzo-current"}, "limit": 1000}), &all)
	if len(all.Transactions) != 40 || all.Truncated {
		t.Errorf("search of the account returned %d rows (truncated %v), want 40", len(all.Transactions), all.Truncated)
	}
	var sum mcpserver.SummaryOutput
	structured(t, call("spending_summary", map[string]any{"from": now.AddDate(0, -3, 0).Format(time.DateOnly), "to": now.Format(time.DateOnly), "group_by": "month"}), &sum)
	if len(sum.Groups) == 0 || sum.Groups[0].Currency != "GBP" {
		t.Errorf("summary = %+v", sum)
	}
	var desc mcpserver.DescribeOutput
	structured(t, call("describe_schema", map[string]any{}), &desc)
	if len(desc.Relations) < 10 {
		t.Errorf("describe_schema listed %d relations", len(desc.Relations))
	}
	var q mcpserver.QueryOutput
	structured(t, call("query_sql", map[string]any{"sql": "SELECT/**/'" + sqlCanary + "'::text AS c"}), &q)
	if len(q.Rows) != 1 || fmt.Sprint(q.Rows[0]) != "["+sqlCanary+"]" {
		t.Errorf("query_sql = %+v", q)
	}
	// Postgres privileges, not Go, keep secret out and writes out.
	if res := call("query_sql", map[string]any{"sql": "SELECT * FROM secret.oauth_token"}); !res.IsError || !strings.Contains(fmt.Sprint(res.Content[0].(*mcp.TextContent).Text), "permission denied") {
		t.Errorf("secret read: %+v", res)
	}
	if res := call("query_sql", map[string]any{"sql": "DELETE FROM raw.fetch"}); !res.IsError {
		t.Errorf("a write was not refused: %+v", res)
	}
	if res := call("search_transactions", map[string]any{"accounts": []string{"x y"}}); !res.IsError {
		t.Errorf("invalid input was not an error result")
	}

	if code := s.stop(); code != app.ExitOK {
		t.Fatalf("serve exit %d: %s", code, s.stderr.String())
	}

	// What serve exported. The span allow-list keeps db.query.text for the app's own statements, so
	// its absence from every mcp.query span is ForAgentSQL at work.
	spans := sink.Spans()
	var queries, toolSpans int
	for _, sp := range spans {
		for k, v := range sp.Attrs {
			if strings.Contains(strings.ToUpper(k+v), sqlCanary) {
				t.Errorf("span %q carries the agent's SQL in %s", sp.Name, k)
			}
		}
		if strings.Contains(strings.ToUpper(sp.Name+sp.StatusMessage), sqlCanary) {
			t.Errorf("span %q carries the agent's SQL", sp.Name)
		}
		if sp.Name == "mcp.query" {
			queries++
			if _, ok := sp.Attrs[semconv.SpanDBQueryText]; ok {
				t.Errorf("an mcp.query span carries db.query.text: %v", sp.Attrs)
			}
			if sp.Attrs[semconv.SpanUserName] != store.RoleMCP {
				t.Errorf("mcp.query span user = %q, want %s: %v", sp.Attrs[semconv.SpanUserName], store.RoleMCP, sp.Attrs)
			}
		}
		if strings.HasPrefix(sp.Name, semconv.SpanMCPToolsCallPrefix) {
			toolSpans++
			if sp.Attrs[semconv.SpanMCPClient] != "agents" || sp.Attrs[semconv.SpanMCPMethodName] != "tools/call" || sp.ParentSpanID == "" {
				t.Errorf("tool span = %+v", sp)
			}
		}
	}
	if queries == 0 {
		t.Error("no mcp.query span was exported")
	}
	if toolSpans != 9 {
		t.Errorf("%d tool call spans exported, want 9", toolSpans)
	}
	calls := sink.Metrics()[semconv.MetricMCPToolCalls]
	var okQuery, refused float64
	for _, p := range calls.Points {
		if p.Attrs[semconv.AttrClient] != "agents" || len(p.Attrs) < 3 {
			t.Errorf("tool call point = %v", p.Attrs)
		}
		if p.Attrs[semconv.AttrTool] == "query_sql" && p.Attrs[semconv.AttrOutcome] == "ok" {
			okQuery = p.Value
		}
		if p.Attrs[semconv.AttrTool] == "query_sql" && p.Attrs[semconv.AttrOutcome] == "refused" {
			refused = p.Value
		}
		for k, v := range p.Attrs {
			if strings.Contains(strings.ToUpper(k+v), sqlCanary) {
				t.Errorf("metric attribute carries SQL: %v", p.Attrs)
			}
		}
	}
	if okQuery != 1 || refused != 2 {
		t.Errorf("query_sql ok = %v refused = %v, want 1 and 2 (%+v)", okQuery, refused, calls.Points)
	}
	// The log is the audit: the SQL and the caller, never a row.
	var auditLines int
	for _, l := range sink.Logs() {
		if l.Body != "mcp tool call" {
			continue
		}
		auditLines++
		if l.Attrs[semconv.LogClient] != "agents" || l.Attrs[semconv.LogPeer] != "100.64.0.9" {
			t.Errorf("audit line = %v", l.Attrs)
		}
		if strings.Contains(fmt.Sprint(l.Attrs), mcpDescMarker) || strings.Contains(fmt.Sprint(l.Attrs), mcpMerchMarker) {
			t.Errorf("audit line carries a row: %v", l.Attrs)
		}
		if l.Attrs[semconv.LogTool] == "query_sql" && l.Attrs[semconv.LogOutcome] == "ok" && !strings.Contains(l.Attrs[semconv.LogSQL], sqlCanary) {
			t.Errorf("the query_sql audit line lacks the SQL: %v", l.Attrs)
		}
	}
	if auditLines != 9 {
		t.Errorf("%d audit lines, want 9", auditLines)
	}
}

// Testing 10 / AC4: with either MCP variable unset serve logs the off line and /mcp answers 404,
// even for a caller that holds the capability, and the rest of the service is unaffected.
func TestServeMCPOffWhenEitherVariableIsUnset(t *testing.T) {
	for _, tt := range []struct {
		name         string
		url, capName string
		variable     string
	}{
		{"neither", "", "", config.EnvMCPDatabaseURL},
		{"only the URL", "set", "", config.EnvMCPAppCap},
		{"only the capability", "", mcpCap, config.EnvMCPDatabaseURL},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, cfg := mcpConfig(t, telemetrytest.New(t))
			cfg.OTLPEndpoint = ""
			cfg.MCPDatabaseURL, cfg.MCPAppCap = "", tt.capName
			if tt.url != "" {
				cfg.MCPDatabaseURL = urlAs(t, w.super, store.RoleMCP)
			}
			s := startServe(t, cfg, w, true)
			if !strings.Contains(s.stderr.String(), "mcp endpoint off: "+tt.variable+" unset") {
				t.Errorf("serve did not log the off line for %s:\n%s", tt.variable, s.stderr.String())
			}
			req, _ := http.NewRequest(http.MethodPost, "http://"+s.addr+"/mcp", strings.NewReader("{}"))
			req.Header.Set("Tailscale-App-Capabilities", mcpGrant)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("/mcp with the MCP endpoint off: %d, want 404", resp.StatusCode)
			}
			if resp, err := http.Get("http://" + s.addr + "/healthz"); err != nil || resp.StatusCode != http.StatusOK {
				t.Errorf("healthz: %v %v", resp, err)
			}
			if code := s.stop(); code != app.ExitOK {
				t.Errorf("serve exit %d", code)
			}
		})
	}
}

// Testing 10 / AC4: serve refuses an MCP URL whose login is not finstore_mcp, before it listens:
// the app role would give agents write access, the owner and the superuser more.
func TestServeRefusesAnMCPURLWhoseRoleIsNotFinstoreMCP(t *testing.T) {
	w, cfg := mcpConfig(t, telemetrytest.New(t))
	cfg.OTLPEndpoint = ""
	for name, url := range map[string]string{
		"the app role":   urlAs(t, w.super, store.RoleApp),
		"the owner role": urlAs(t, w.super, store.RoleOwner),
		"the superuser":  w.super,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := cfg
			cfg.MCPDatabaseURL = url
			s, code := tryServe(t, cfg, w, true)
			if s != nil {
				t.Fatal("serve started with an MCP URL that is not finstore_mcp")
			}
			if code != app.ExitError {
				t.Errorf("exit %d, want %d", code, app.ExitError)
			}
		})
	}
	// The message names the variable and the role it needs, and never the connection string.
	cfg.MCPDatabaseURL = urlAs(t, w.super, store.RoleApp)
	s := &syncBuffer{}
	// Bounded, so a regression that lets serve start fails here instead of hanging the package.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	code := app.Serve(ctx, app.Env{Cfg: cfg, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: s, Version: testVersion, Now: w.fake.Now},
		&app.Hooks{NoSchedule: true})
	if code != app.ExitError || !strings.Contains(s.String(), config.EnvMCPDatabaseURL) || !strings.Contains(s.String(), store.RoleMCP) ||
		strings.Contains(s.String(), cfg.MCPDatabaseURL) {
		t.Errorf("exit %d, stderr %q", code, s.String())
	}
}

// An MCP URL that cannot connect stops serve too, as the app's own URL does.
func TestServeRefusesAnUnreachableMCPDatabase(t *testing.T) {
	w, cfg := mcpConfig(t, telemetrytest.New(t))
	cfg.OTLPEndpoint = ""
	cfg.MCPDatabaseURL = strings.Replace(cfg.MCPDatabaseURL, "/finstore?", "/no_such_database?", 1)
	if s, code := tryServe(t, cfg, w, true); s != nil || code != app.ExitError {
		t.Errorf("serve started (%v) or exited %d, want exit %d", s != nil, code, app.ExitError)
	}
}

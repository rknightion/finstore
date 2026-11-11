package web

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rknightion/finstore/internal/mcpserver"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/telemetry"
)

// mcpHits is an MCP handler that records what reached it.
type mcpHits struct {
	methods []string
	emails  []string
}

func (m *mcpHits) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.methods = append(m.methods, r.Method)
	m.emails = append(m.emails, r.Header.Get(authHeader))
	w.WriteHeader(http.StatusTeapot)
}

func serverWithMCP(t *testing.T, mcp http.Handler, logTo io.Writer) http.Handler {
	t.Helper()
	if logTo == nil {
		logTo = io.Discard
	}
	opts := options(logTo, monzoSource(&fakeConn{}))
	opts.MCP = mcp
	s, err := New(opts, &fakeReader{})
	if err != nil {
		t.Fatal(err)
	}
	return s.Handler()
}

// Testing 10 / AC4: /mcp is mounted only when Options.MCP is set.
func TestMCPIsMountedOnlyWhenConfigured(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		h := newServer(t, &fakeConn{}, &fakeReader{}, nil)
		if rec := do(h, method, "/mcp", signedIn(nil)); rec.Code != http.StatusNotFound {
			t.Errorf("%s /mcp with no MCP handler: status %d, want 404", method, rec.Code)
		}
	}
	hits := &mcpHits{}
	h := serverWithMCP(t, hits, nil)
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete, http.MethodPut, http.MethodOptions} {
		if rec := do(h, method, "/mcp", nil); rec.Code != http.StatusTeapot {
			t.Errorf("%s /mcp: status %d, want the handler's answer", method, rec.Code)
		}
	}
	if len(hits.methods) != 5 {
		t.Errorf("handler saw %v", hits.methods)
	}
}

// Only the exact path is mounted.
func TestMCPMountsOnlyTheExactPath(t *testing.T) {
	hits := &mcpHits{}
	h := serverWithMCP(t, hits, nil)
	for _, path := range []string{"/mcp/", "/mcp/x", "/mcpx", "/MCP", "/api/mcp"} {
		if rec := do(h, http.MethodPost, path, nil); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s: status %d, want 404", path, rec.Code)
		}
	}
	if len(hits.methods) != 0 {
		t.Errorf("handler saw %v", hits.methods)
	}
}

// AC4: /mcp is not behind the browser allow-list, whose signed-in address a tagged device never
// has, and whatever address a request carries is left for the handler to see; the pages stay
// behind it.
func TestMCPIsNotBehindTheAllowList(t *testing.T) {
	hits := &mcpHits{}
	h := serverWithMCP(t, hits, nil)
	for name, header := range map[string]map[string]string{
		"no identity header": nil, "an address on the list": signedIn(nil),
		"an address not on the list": {authHeader: "stranger@example.com"},
	} {
		if rec := do(h, http.MethodPost, "/mcp", header); rec.Code != http.StatusTeapot {
			t.Errorf("%s: status %d, want it to reach the handler", name, rec.Code)
		}
	}
	if rec := do(h, http.MethodGet, "/", nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET / without an identity: status %d, want 404", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/", signedIn(nil)); rec.Code != http.StatusOK {
		t.Errorf("GET / signed in: status %d, want 200", rec.Code)
	}
}

// The design's seam: a caller with the capability and no identity header reaches /mcp through the
// gate and gets 404 on /; a caller with an identity and no capability gets 404 on /mcp.
func TestMCPGateAndAllowListAreIndependent(t *testing.T) {
	const capName = "example.com/cap/finstore"
	hits := &mcpHits{}
	h := serverWithMCP(t, mcpserver.Gate(capName, hits), nil)
	caps := map[string]string{"Tailscale-App-Capabilities": `{"` + capName + `":[{"client":"agents"}]}`}

	if rec := do(h, http.MethodPost, "/mcp", caps); rec.Code != http.StatusTeapot {
		t.Errorf("capability, no identity: /mcp status %d, want the handler's answer", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/", caps); rec.Code != http.StatusNotFound {
		t.Errorf("capability, no identity: / status %d, want 404", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/mcp", signedIn(nil)); rec.Code != http.StatusNotFound {
		t.Errorf("identity, no capability: /mcp status %d, want 404", rec.Code)
	}
	if len(hits.methods) != 1 {
		t.Errorf("handler saw %v, want one request", hits.methods)
	}
}

// /mcp goes through the same response headers and request log as every route: the log names the
// route, never the URL.
func TestMCPRequestsAreLoggedAndGetTheSecurityHeaders(t *testing.T) {
	var logs bytes.Buffer
	h := serverWithMCP(t, &mcpHits{}, &logs)
	rec := do(h, http.MethodPost, "/mcp?token=CANARY-QUERY", nil)
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "no-store" ||
		rec.Header().Get("Content-Security-Policy") == "" {
		t.Errorf("headers = %v", rec.Header())
	}
	out := logs.String()
	if !strings.Contains(out, `"endpoint":"/mcp"`) || !strings.Contains(out, `"status":418`) || strings.Contains(out, "CANARY") {
		t.Errorf("log = %s", out)
	}
}

// Behind the OpenTelemetry HTTP middleware /mcp is a server span with the route /mcp, so the
// request duration histogram has a series for it.
func TestMCPRouteIsTracedAndMeasured(t *testing.T) {
	mem := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(mem))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()); _ = mp.Shutdown(context.Background()) })
	p := &telemetry.Providers{Meter: mp.Meter("test"), MeterProvider: mp, TracerProvider: tp}
	h := telemetry.HTTPHandler(serverWithMCP(t, &mcpHits{}, nil), p)

	if rec := do(h, http.MethodPost, "/mcp", nil); rec.Code != http.StatusTeapot {
		t.Fatalf("status %d", rec.Code)
	}
	spans := mem.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("%d spans", len(spans))
	}
	var route string
	for _, kv := range spans[0].Attributes {
		if kv.Key == attribute.Key(semconv.SpanHTTPRoute) {
			route = kv.Value.AsString()
		}
	}
	if route != "/mcp" {
		t.Errorf("http.route = %q, want /mcp (span %q)", route, spans[0].Name)
	}
}

// Adding the MCP handler changes nothing about the options the pages need: a server without
// sources or an allow-list is still refused, with or without it.
func TestMCPDoesNotRelaxTheOtherChecks(t *testing.T) {
	opts := options(io.Discard, monzoSource(&fakeConn{}))
	opts.MCP = &mcpHits{}
	opts.AllowedEmails = nil
	if _, err := New(opts, &fakeReader{}); err == nil {
		t.Error("an empty allow-list was accepted because an MCP handler is set")
	}
}

// stubOverview is an MCP reader that answers only overview.
type stubOverview struct{ mcpserver.Reader }

func (stubOverview) Overview(context.Context) (mcpserver.Overview, error) {
	return mcpserver.Overview{Accounts: []mcpserver.Account{{AccountSlug: "current", Currency: "GBP"}}}, nil
}

// The real MCP handler works behind the page server's request log wrapper, whose response writer
// does not flush: the transport answers in plain JSON, so nothing needs to.
func TestMCPHandlerWorksBehindTheRequestLogWrapper(t *testing.T) {
	const capName = "example.com/cap/finstore"
	var logs bytes.Buffer
	srv := mcpserver.NewServer(mcpserver.Options{Reader: stubOverview{}, Version: "test"})
	ts := httptest.NewServer(serverWithMCP(t, mcpserver.Gate(capName, mcpserver.Handler(srv)), &logs))
	t.Cleanup(ts.Close)

	hc := &http.Client{Transport: headerAdder{"Tailscale-App-Capabilities": `{"` + capName + `":[{"client":"agents"}]}`}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: hc, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "overview", Arguments: map[string]any{}})
	if err != nil || res.IsError || !strings.Contains(fmt.Sprint(res.StructuredContent), "current") {
		t.Fatalf("overview: %v %+v", err, res)
	}
	if !strings.Contains(logs.String(), `"endpoint":"/mcp"`) || !strings.Contains(logs.String(), `"status":200`) {
		t.Errorf("log = %s", logs.String())
	}
}

type headerAdder map[string]string

func (h headerAdder) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

package semconv

import (
	"strings"
	"testing"
)

func TestIsSpanAttributeAllowsTheDeclaredKeys(t *testing.T) {
	for _, k := range SpanAttributes() {
		if !IsSpanAttribute(k) {
			t.Errorf("%q is declared but not allowed", k)
		}
	}
	for _, k := range []string{
		SpanHTTPRoute, SpanURLTemplate, SpanDBQueryText, SpanProcessExitCode, SpanSource,
		SpanConnectOutcome, SpanJobOutcome, SpanRunPages, SpanLockWaitMS, SpanCount, SpanCommand,
		"finstore.command", SpanPgxRowsAffected, SpanPgxPrepareStmtName,
	} {
		if !IsSpanAttribute(k) {
			t.Errorf("%q is not allowed", k)
		}
	}
}

// Every key that can carry a secret or a value from a request, a query or an error stays off the
// list.
func TestIsSpanAttributeRefusesSecretBearingKeys(t *testing.T) {
	for _, k := range []string{
		"pgx.query.parameters", "url.query", "http.url", "http.target", "url.fragment",
		"http.request.header.authorization", "http.request.header.x-auth-request-email",
		"http.response.header.set-cookie", "exception.message", "exception.stacktrace",
		"exception.type", "db.query.parameter.1", "db.statement", "enduser.id", "password", "token",
		"authorization", "", "FINSTORE.SOURCE", "finstore.",
	} {
		if IsSpanAttribute(k) {
			t.Errorf("%q must not be allowed", k)
		}
	}
}

func TestSpanAttributesIsACopy(t *testing.T) {
	a := SpanAttributes()
	a[0] = "tampered"
	if SpanAttributes()[0] == "tampered" {
		t.Fatal("SpanAttributes returns the live slice")
	}
}

func TestIsStatusCategory(t *testing.T) {
	for _, w := range []string{
		"timeout", "network", "rate_limited", "server_error", "unauthorised", "bad_request",
		"forbidden_insufficient_permissions", "forbidden_verification_required", "unexpected_status",
		"history_failed", "consent_lost", "superseded", "interrupted", "card_key_shared",
		"cards_refused", "window_closed", "timestamp_group_over_page", "check_failed",
		"account_mismatch", "unknown_format", "rejected_before", "database_error", "attempt_read",
		"exchange", "cancelled", "internal", "unexpected_body", "error",
		"invalid", "busy", "refused",
	} {
		if !IsStatusCategory(w) {
			t.Errorf("%q should be a category", w)
		}
	}
	for _, w := range []string{
		"", "ERROR: invalid input syntax for type integer: \"CANARY\"", "Get https://x", "timeout ",
		"Timeout", strings.Repeat("a", 200),
	} {
		if IsStatusCategory(w) {
			t.Errorf("%q must not be a category", w)
		}
	}
}

// The MCP attributes survive the exporter allow-list, with the names the OpenTelemetry conventions
// give them (semantic-conventions v1.45.0 registry, MCP and GenAI sections), and finstore's own
// keep the finstore. prefix. None of them can carry SQL or a row.
func TestMCPSpanAttributesAreAllowed(t *testing.T) {
	for want, key := range map[string]string{
		"mcp.method.name": SpanMCPMethodName, "mcp.protocol.version": SpanMCPProtocolVersion,
		"gen_ai.tool.name": SpanGenAIToolName, "gen_ai.operation.name": SpanGenAIOperationName,
		"finstore.mcp.client": SpanMCPClient, "finstore.mcp.rows": SpanMCPRows, "finstore.mcp.truncated": SpanMCPTruncated,
	} {
		if key != want {
			t.Errorf("constant = %q, want %q", key, want)
		}
		if !IsSpanAttribute(want) {
			t.Errorf("%q is not allowed", want)
		}
	}
	for _, k := range []string{
		"gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "gen_ai.prompt.variable.x",
		"mcp.resource.uri", "mcp.session.id", "jsonrpc.request.id", "finstore.mcp.sql", "finstore.mcp.args",
	} {
		if IsSpanAttribute(k) {
			t.Errorf("%q must not be allowed: it can carry what the caller sent or read", k)
		}
	}
}

func TestMCPLogFieldsAreAllowed(t *testing.T) {
	for _, f := range []string{"tool", "client", "peer", "outcome", "rows", "truncated", "duration_ms", "args", "sql", "stack"} {
		if !IsLogField(f) {
			t.Errorf("log field %q is not allowed", f)
		}
	}
	for _, f := range []string{"result", "query", "row", "rows_data"} {
		if IsLogField(f) {
			t.Errorf("log field %q must not be allowed", f)
		}
	}
}

// Every outcome but ok is a status description the exporter keeps, so a failed tool call span does
// not read "error" whatever its cause.
func TestEveryMCPFailureOutcomeMayBeAStatusDescription(t *testing.T) {
	for _, o := range []string{MCPOutcomeInvalid, MCPOutcomeBusy, MCPOutcomeTimeout, MCPOutcomeRefused, MCPOutcomeError} {
		if !IsStatusCategory(o) {
			t.Errorf("outcome %q is not a status category", o)
		}
	}
}

func TestMCPToolsAreTheFiveAndACopy(t *testing.T) {
	got := MCPTools()
	want := []string{"overview", "search_transactions", "spending_summary", "describe_schema", "query_sql"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("MCPTools = %v, want %v", got, want)
	}
	got[0] = "tampered"
	if MCPTools()[0] != "overview" {
		t.Error("MCPTools returns the live slice")
	}
	for _, n := range want {
		if !IsMCPTool(n) {
			t.Errorf("%q is not a tool", n)
		}
	}
	for _, n := range []string{"", MCPToolUnknown, "OVERVIEW", "overview ", "x'; DROP TABLE y"} {
		if IsMCPTool(n) {
			t.Errorf("%q must not be a tool", n)
		}
	}
	if MetricMCPToolCalls != "finstore.mcp.tool.calls" || AttrTool != "tool" || AttrClient != "client" || AttrOutcome != "outcome" {
		t.Error("the MCP metric name or attribute keys changed")
	}
}

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

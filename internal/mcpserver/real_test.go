package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/codes"

	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// Testing 8: the tools against the store's real reader on a private cluster, bootstrapped and
// migrated as production is, with a few made-up rows in core.

var testNS = uuid.MustParse("1b7a6d3e-5a4f-4c52-9a0e-6f1f0d9b7c11")

// realCluster is a bootstrapped and migrated cluster with a small ledger.
type realCluster struct {
	super, mcp string
}

func newRealCluster(t *testing.T) realCluster {
	t.Helper()
	ctx := context.Background()
	super := testpg.Start(t)
	if err := store.Bootstrap(ctx, super, "owner-secret", "app-secret"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := store.EnableMCPLogin(ctx, super, "mcp-secret"); err != nil {
		t.Fatalf("EnableMCPLogin: %v", err)
	}
	if err := store.Migrate(ctx, testpg.UserURL(t, super, store.RoleOwner), 1); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	app, err := store.Open(ctx, testpg.UserURL(t, super, store.RoleApp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)

	acc := store.Account{ID: uuid.NewSHA1(testNS, []byte("uk_retail")), Source: "test", SourceKey: "uk_retail", Slug: "uk_retail",
		Type: "current", Feed: "api", Currency: "GBP", Name: "made-up current"}
	str := func(s string) *string { return &s }
	day := func(d int) time.Time { return time.Date(2030, 3, d, 0, 0, 0, 0, time.UTC) }
	txns := []store.Transaction{
		{SourceKey: "t1", AmountMinor: -450, Currency: "GBP", TxnDate: day(1), Status: "settled", Description: "Coffee 50%_off", MerchantName: str("Beans"), Category: str("eating_out")},
		{SourceKey: "t2", AmountMinor: -1250, Currency: "GBP", TxnDate: day(2), Status: "settled", Description: "Groceries", MerchantName: str("Shop"), Category: str("groceries")},
		{SourceKey: "t3", AmountMinor: 100000, Currency: "GBP", TxnDate: day(3), Status: "settled", Description: "Pay", Counterparty: str("Employer")},
		{SourceKey: "t4", AmountMinor: 2000, Currency: "EUR", TxnDate: day(4), Status: "settled", Description: "Refund", MerchantName: str("Beans")},
		{SourceKey: "t5", AmountMinor: -999, Currency: "GBP", TxnDate: day(5), Status: "declined", Description: "Declined", MerchantName: str("Shop")},
	}
	err = app.WithWriterLock(ctx, store.HolderTest, func(ctx context.Context, tx pgx.Tx) error {
		core, err := store.NewCore(tx, store.SchemaCore)
		if err != nil {
			return err
		}
		if err := core.UpsertAccount(ctx, acc); err != nil {
			return err
		}
		for i, tt := range txns {
			tt.ID, tt.AccountID, tt.Source = uuid.NewSHA1(testNS, []byte(tt.SourceKey)), acc.ID, "test"
			at := tt.TxnDate.Add(12 * time.Hour)
			tt.CreatedAt = &at
			if _, err := core.ApplyTransaction(ctx, tt, json.RawMessage(fmt.Sprintf(`{"id":%q}`, tt.SourceKey)),
				[]byte{byte(i + 1)}, at, int64(i+1)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return realCluster{super: super, mcp: testpg.UserURL(t, super, store.RoleMCP)}
}

// realRig is a server over the real reader, opened as serve opens it: with telemetry and
// ForAgentSQL.
func newRealRig(t *testing.T, c realCluster) *rig {
	t.Helper()
	var reader *store.Reader
	r := newRigWith(t, func(r *rig) Reader {
		var err error
		reader, err = store.OpenReader(context.Background(), c.mcp,
			store.WithTelemetry(r.tp, r.mp), store.ForAgentSQL())
		if err != nil {
			t.Fatalf("OpenReader: %v", err)
		}
		t.Cleanup(reader.Close)
		if err := reader.CheckRole(context.Background()); err != nil {
			t.Fatal(err)
		}
		return FromStore(reader)
	}, nil)
	return r
}

func TestRealReaderAnswersEveryTool(t *testing.T) {
	r := newRealRig(t, newRealCluster(t))

	t.Run("overview", func(t *testing.T) {
		var out Overview
		decode(t, r.call(semconv.MCPToolOverview, map[string]any{}), &out)
		if len(out.Accounts) != 1 {
			t.Fatalf("accounts = %+v", out.Accounts)
		}
		a := out.Accounts[0]
		if a.AccountSlug != "uk_retail" || a.Source != "test" || a.Feed != "api" || a.Type != "current" || a.Currency != "GBP" ||
			a.TransactionCount != 5 || a.EarliestDate == nil || *a.EarliestDate != "2030-03-01" || a.LatestDate == nil || *a.LatestDate != "2030-03-05" ||
			a.Gaps == nil {
			t.Errorf("account = %+v", a)
		}
		if len(out.Sources) == 0 {
			t.Errorf("no sources: %+v", out)
		}
	})
	t.Run("search", func(t *testing.T) {
		var out SearchOutput
		decode(t, r.call(semconv.MCPToolSearchTransactions, map[string]any{"accounts": []string{"uk_retail"}, "text": "50%_OFF"}), &out)
		if len(out.Transactions) != 1 || out.Transactions[0].Description != "Coffee 50%_off" || out.Transactions[0].AmountMinor != -450 ||
			out.Transactions[0].TxnDate != "2030-03-01" || out.Transactions[0].CreatedAt == nil || out.Transactions[0].Detail != nil {
			t.Errorf("search = %+v", out)
		}
		// % and _ match themselves: neither is a wildcard.
		decode(t, r.call(semconv.MCPToolSearchTransactions, map[string]any{"text": "50_%off"}), &out)
		if len(out.Transactions) != 0 {
			t.Errorf("a wildcard matched: %+v", out.Transactions)
		}
		decode(t, r.call(semconv.MCPToolSearchTransactions, map[string]any{"direction": "in", "from": "2030-03-03", "to": "2030-03-04", "include_detail": true}), &out)
		if len(out.Transactions) != 2 || out.Transactions[0].Detail == nil {
			t.Errorf("search in = %+v", out.Transactions)
		}
		decode(t, r.call(semconv.MCPToolSearchTransactions, map[string]any{"limit": 2}), &out)
		if len(out.Transactions) != 2 || !out.Truncated {
			t.Errorf("limit 2: %d rows truncated %v", len(out.Transactions), out.Truncated)
		}
	})
	t.Run("summary", func(t *testing.T) {
		var out SummaryOutput
		decode(t, r.call(semconv.MCPToolSpendingSummary, map[string]any{"from": "2030-03-01", "to": "2030-03-31", "group_by": "merchant"}), &out)
		// Out, declined left out: Shop 1250, Beans 450.
		if len(out.Groups) != 2 || out.Groups[0] != (SummaryGroup{Key: "Shop", Currency: "GBP", TotalMinor: 1250, Count: 1}) ||
			out.Groups[1] != (SummaryGroup{Key: "Beans", Currency: "GBP", TotalMinor: 450, Count: 1}) {
			t.Errorf("summary = %+v", out.Groups)
		}
		decode(t, r.call(semconv.MCPToolSpendingSummary, map[string]any{"from": "2030-03-01", "to": "2030-03-31", "group_by": "month", "direction": "in"}), &out)
		if len(out.Groups) != 2 || out.Groups[0].Key != "2030-03" || out.Groups[0].Currency != "EUR" && out.Groups[1].Currency != "EUR" {
			t.Errorf("summary in by month = %+v (one row per currency, never summed)", out.Groups)
		}
	})
	t.Run("describe", func(t *testing.T) {
		var out DescribeOutput
		decode(t, r.call(semconv.MCPToolDescribeSchema, map[string]any{"schema": "agent"}), &out)
		var found bool
		for _, rel := range out.Relations {
			if rel.Schema != "agent" {
				t.Errorf("relation of schema %q in an agent listing", rel.Schema)
			}
			if rel.Name == "transactions" {
				found = rel.Kind == "view" && rel.Comment != nil && len(rel.Columns) > 5
			}
		}
		if !found {
			t.Errorf("agent.transactions not described as a commented view: %+v", out.Relations)
		}
		decode(t, r.call(semconv.MCPToolDescribeSchema, map[string]any{}), &out)
		for _, rel := range out.Relations {
			if rel.Schema == "secret" {
				t.Errorf("the reader describes %s.%s", rel.Schema, rel.Name)
			}
		}
	})
	t.Run("query", func(t *testing.T) {
		var out QueryOutput
		decode(t, r.call(semconv.MCPToolQuerySQL, map[string]any{"sql": "SELECT count(*)::int AS n, 'x'::text AS s FROM agent.transactions"}), &out)
		if strings.Join(out.Columns, ",") != "n,s" || len(out.Rows) != 1 || fmt.Sprint(out.Rows[0]) != "[5 x]" {
			t.Errorf("query = %+v", out)
		}
		decode(t, r.call(semconv.MCPToolQuerySQL, map[string]any{"sql": "SELECT generate_series(1, 10)", "max_rows": 3}), &out)
		if len(out.Rows) != 3 || !out.Truncated {
			t.Errorf("max_rows 3: %d rows truncated %v", len(out.Rows), out.Truncated)
		}
	})

	// Every call above was observed, none as anything but ok.
	for k := range r.counts() {
		if !strings.Contains(k, "/ok/") {
			t.Errorf("series %s", k)
		}
	}
}

// An account slug as the store takes it, an underscore and capitals included, passes the tools'
// own check and reaches the store.
func TestRealReaderAcceptsTheStoresSlugs(t *testing.T) {
	r := newRealRig(t, newRealCluster(t))
	var out SearchOutput
	for _, slug := range []string{"uk_retail", "UK_Retail", "a.b-c_1", "no-such-account"} {
		res := r.call(semconv.MCPToolSearchTransactions, map[string]any{"accounts": []string{slug}})
		if res.IsError {
			t.Errorf("slug %q refused: %s", slug, resultText(res))
		}
	}
	decode(t, r.call(semconv.MCPToolSearchTransactions, map[string]any{"accounts": []string{"uk_retail"}}), &out)
	if len(out.Transactions) != 5 {
		t.Errorf("uk_retail has %d transactions, want 5", len(out.Transactions))
	}
}

// Input the tools' own checks let through but the store refuses (a 101 character slug) is invalid,
// through the store's classification: the tool says why in one line, the outcome is invalid, and
// the span, the counter and the log agree.
func TestRealReaderInputRefusedByTheStoreIsInvalid(t *testing.T) {
	r := newRealRig(t, newRealCluster(t))
	res := r.call(semconv.MCPToolSearchTransactions, map[string]any{"accounts": []string{strings.Repeat("a", 101)}})
	if !res.IsError || strings.Contains(resultText(res), "store:") || strings.Contains(resultText(res), "\n") || resultText(res) == "" {
		t.Fatalf("result = %v %q, want a one-line error without the store's prefix", res.IsError, resultText(res))
	}
	span, _ := r.onlyCall(semconv.MCPToolSearchTransactions, semconv.MCPOutcomeInvalid, semconv.MCPClientUnknown)
	if span.Status.Code != codes.Error || span.Status.Description != semconv.MCPOutcomeInvalid {
		t.Errorf("span status %+v", span.Status)
	}
}

// Postgres, not Go, refuses what the role may not do, and the outcome is refused: a read of
// secret, a write, a second statement. The text for query_sql is Postgres's own, on one line.
func TestRealReaderRefusals(t *testing.T) {
	r := newRealRig(t, newRealCluster(t))
	for name, sql := range map[string]string{
		"secret":           "SELECT * FROM secret.oauth_token",
		"a write":          "INSERT INTO raw.fetch DEFAULT VALUES",
		"a second command": "SELECT 1; SELECT 2",
		"syntax":           "SELEC 1",
	} {
		res := r.call(semconv.MCPToolQuerySQL, map[string]any{"sql": sql})
		if !res.IsError || !strings.Contains(resultText(res), "SQLSTATE") || strings.Contains(resultText(res), "\n") {
			t.Errorf("%s: result = %v %q", name, res.IsError, resultText(res))
		}
	}
	counts := r.counts()
	if counts["query_sql/refused/"+semconv.MCPClientUnknown] != 4 || len(counts) != 1 {
		t.Errorf("counter = %v, want four refused", counts)
	}
}

// AC5 on the real path: a query_sql call's database span is named mcp.query and carries no
// statement, and the statement is nowhere in any span of the call.
func TestRealReaderQuerySpanHasNoSQL(t *testing.T) {
	r := newRealRig(t, newRealCluster(t))
	const canary = "CANARY_SQL_TEXT"
	r.call(semconv.MCPToolQuerySQL, map[string]any{"sql": "SELECT/**/'" + canary + "'::text"})
	r.call(semconv.MCPToolQuerySQL, map[string]any{"sql": "SELECT/**/'" + canary + "'::int"})
	var queries int
	for _, s := range r.spans() {
		if s.Name == "mcp.query" {
			queries++
		}
		// Name and attributes: what the exporter keeps. A failed statement's status text and events
		// quote the error here, before the exporter's allow-list replaces them (the end to end test
		// in package app reads the exported spans).
		if strings.Contains(strings.ToUpper(fmt.Sprint(s.Name, s.Attributes)), canary) {
			t.Errorf("span %q carries the statement: %v", s.Name, s.Attributes)
		}
		for _, kv := range s.Attributes {
			if kv.Key == semconv.SpanDBQueryText {
				t.Errorf("span %q carries db.query.text", s.Name)
			}
		}
	}
	if queries < 2 {
		t.Errorf("%d mcp.query spans, want one per statement at least", queries)
	}
}

// A statement past the role's 15 second statement_timeout is a timeout, not an error. This test
// waits the full 15 seconds: the timeout is the role's own, and the reader has no hook outside its
// package to shorten it.
func TestRealReaderStatementTimeoutIsATimeout(t *testing.T) {
	r := newRealRig(t, newRealCluster(t))
	start := time.Now()
	res := r.call(semconv.MCPToolQuerySQL, map[string]any{"sql": "SELECT pg_sleep(60)"})
	if took := time.Since(start); !res.IsError || took < 10*time.Second || took > 40*time.Second {
		t.Fatalf("result %v after %v, want an error near the 15 s statement timeout", res.IsError, took)
	}
	r.onlyCall(semconv.MCPToolQuerySQL, semconv.MCPOutcomeTimeout, semconv.MCPClientUnknown)
}

package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rknightion/finstore/internal/semconv"
)

// MCPToolCall is a counter named finstore.mcp.tool.calls whose only attributes are tool, outcome
// and client.
func TestMCPToolCallCountsByToolOutcomeAndClient(t *testing.T) {
	m, read := collect(t)
	ctx := context.Background()
	m.MCPToolCall(ctx, semconv.MCPToolOverview, semconv.MCPOutcomeOK, "agents")
	m.MCPToolCall(ctx, semconv.MCPToolOverview, semconv.MCPOutcomeOK, "agents")
	m.MCPToolCall(ctx, semconv.MCPToolQuerySQL, semconv.MCPOutcomeRefused, "a,b")

	mt, ok := read()[semconv.MetricMCPToolCalls]
	if !ok {
		t.Fatalf("%s was not recorded", semconv.MetricMCPToolCalls)
	}
	sum, ok := mt.Data.(metricdata.Sum[int64])
	if !ok || !sum.IsMonotonic {
		t.Fatalf("%s is not a monotonic int counter: %#v", semconv.MetricMCPToolCalls, mt.Data)
	}
	got := map[string]int64{}
	for _, dp := range sum.DataPoints {
		if dp.Attributes.Len() != 3 {
			t.Errorf("a point has %d attributes, want exactly tool, outcome and client: %v", dp.Attributes.Len(), dp.Attributes)
		}
		var key string
		for _, k := range []string{semconv.AttrTool, semconv.AttrOutcome, semconv.AttrClient} {
			v, ok := dp.Attributes.Value(attribute.Key(k))
			if !ok {
				t.Fatalf("point lacks %q: %v", k, dp.Attributes)
			}
			key += v.AsString() + "/"
		}
		got[key] = dp.Value
	}
	if got["overview/ok/agents/"] != 2 || got["query_sql/refused/a,b/"] != 1 || len(got) != 2 {
		t.Errorf("series = %v", got)
	}
}

// A tool call span's MCP attributes pass the real exporter allow-list untouched, and the keys that
// could carry SQL or a value from a row are dropped and counted, so a library or a later change
// that sets one cannot leak it.
func TestMCPToolCallSpanPassesTheAllowListAndDropsCallerData(t *testing.T) {
	drops := &countingDrops{}
	tr, mem := filtered(t, drops)
	_, span := tr.Start(context.Background(), semconv.SpanMCPToolsCallPrefix+semconv.MCPToolQuerySQL)
	span.SetAttributes(
		attribute.String(semconv.SpanMCPMethodName, "tools/call"),
		attribute.String(semconv.SpanGenAIToolName, semconv.MCPToolQuerySQL),
		attribute.String(semconv.SpanGenAIOperationName, semconv.GenAIOperationExecuteTool),
		attribute.String(semconv.SpanMCPProtocolVersion, "2026-07-28"),
		attribute.String(semconv.SpanMCPClient, "agents"),
		attribute.Int(semconv.SpanMCPRows, 3),
		attribute.Bool(semconv.SpanMCPTruncated, true),
		attribute.String("gen_ai.tool.call.arguments", "CANARY-SQL"),
		attribute.String("gen_ai.tool.call.result", "CANARY-ROW"),
	)
	Fail(span, semconv.MCPOutcomeRefused)
	span.End()

	spans := mem.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("%d spans", len(spans))
	}
	got := spans[0]
	attrs := attrsOf(got)
	for k, want := range map[string]string{
		semconv.SpanMCPMethodName: "tools/call", semconv.SpanGenAIToolName: "query_sql",
		semconv.SpanGenAIOperationName: "execute_tool", semconv.SpanMCPProtocolVersion: "2026-07-28",
		semconv.SpanMCPClient: "agents", semconv.SpanMCPRows: "3", semconv.SpanMCPTruncated: "true",
	} {
		if attrs[k] != want {
			t.Errorf("%s = %q, want %q", k, attrs[k], want)
		}
	}
	for _, k := range []string{"gen_ai.tool.call.arguments", "gen_ai.tool.call.result"} {
		if _, ok := attrs[k]; ok {
			t.Errorf("%q survived the allow-list", k)
		}
		if drops.count(k) != 1 {
			t.Errorf("dropped key %q counted %d times, want 1", k, drops.count(k))
		}
	}
	if got.Status.Description != semconv.MCPOutcomeRefused {
		t.Errorf("status description = %q, want the outcome %q", got.Status.Description, semconv.MCPOutcomeRefused)
	}
}

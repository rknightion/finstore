package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry/telemetrytest"
)

// leaks lists the spans whose name, status message or any attribute value contains text, in any case
// (otelpgx upper-cases the first word of a statement for its span name).
func leaks(spans []telemetrytest.Span, text string) []telemetrytest.Span {
	text = strings.ToUpper(text)
	var out []telemetrytest.Span
	for _, s := range spans {
		found := strings.Contains(strings.ToUpper(s.Name), text) || strings.Contains(strings.ToUpper(s.StatusMessage), text)
		for k, v := range s.Attrs {
			found = found || strings.Contains(strings.ToUpper(k), text) || strings.Contains(strings.ToUpper(v), text)
		}
		if found {
			out = append(out, s)
		}
	}
	return out
}

// An agent's SQL never reaches telemetry. The statement has no space in it, so otelpgx's default
// span name, the statement's first word, is the whole statement, which is the leak ForAgentSQL closes.
func TestAgentSQLNeverReachesTelemetry(t *testing.T) {
	c := newCluster(t)
	url := c.mcpURL(t)
	ctx := context.Background()
	statements := []string{
		"SELECT/**/'" + canary + "'::text",         // succeeds
		"SELECT/**/'" + canary + "'::int",          // fails: the error text quotes the value
		"SELECT/**/'" + canary + "'::text;SELECT1", // refused as two statements
	}

	run := func(t *testing.T, opts ...store.OpenOption) (traced, []telemetrytest.Span, telemetrytest.Span) {
		t.Helper()
		tr := newTraced(t)
		r, err := store.OpenReader(ctx, url, append([]store.OpenOption{store.WithTelemetry(tr.p.TracerProvider, tr.p.MeterProvider)}, opts...)...)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		rctx, root := tr.p.Tracer("test").Start(ctx, "root")
		for _, sql := range statements {
			_, _ = r.Query(rctx, sql, smallLimits)
		}
		// A curated call runs under the same options.
		if _, err := r.Overview(rctx); err != nil {
			t.Fatal(err)
		}
		root.End()
		spans := tr.spans(t)
		for _, s := range spans {
			if s.Name == "root" {
				return tr, spans, s
			}
		}
		t.Fatal("no root span exported")
		return tr, nil, telemetrytest.Span{}
	}

	t.Run("without ForAgentSQL the statement is in a span", func(t *testing.T) {
		// The control: the check below can fail, and does, when the option is not given.
		_, spans, _ := run(t)
		if got := leaks(spans, canary); len(got) == 0 {
			t.Fatalf("no span carries the statement without ForAgentSQL: the test could not detect a leak (%d spans)", len(spans))
		}
	})

	t.Run("with ForAgentSQL nothing carries it", func(t *testing.T) {
		tr, spans, root := run(t, store.ForAgentSQL())
		var queries int
		for _, s := range spans {
			if s.Name != semconv.SpanMCPQuery {
				continue
			}
			queries++
			if _, ok := s.Attrs[semconv.SpanDBQueryText]; ok {
				t.Errorf("an %s span carries db.query.text: %+v", semconv.SpanMCPQuery, s.Attrs)
			}
			// otelpgx sets db.operation.name from the span name function: the same constant.
			if op, ok := s.Attrs[semconv.SpanDBOperationName]; ok && op != semconv.SpanMCPQuery {
				t.Errorf("an %s span carries db.operation.name %q", semconv.SpanMCPQuery, op)
			}
			if s.Kind != 3 || s.Attrs[semconv.SpanDBSystemName] != "postgresql" || !descendsFrom(spans, s, root.SpanID) {
				t.Errorf("%s span is not a Postgres client span under the call: %+v", semconv.SpanMCPQuery, s)
			}
		}
		// Each reader call and each of its statements (begin, describe, run, rollback) is one.
		if queries < len(statements) {
			t.Errorf("%d %s spans for %d statements and a tool query", queries, semconv.SpanMCPQuery, len(statements))
		}
		for _, s := range spans {
			if _, ok := s.Attrs[semconv.SpanDBQueryText]; ok {
				t.Errorf("span %q carries db.query.text", s.Name)
			}
		}
		if got := leaks(spans, canary); len(got) != 0 {
			t.Errorf("spans carry the agent's statement: %+v", got)
		}
		// Nor does any other signal: metrics and logs are in the same export stream.
		if strings.Contains(strings.ToUpper(tr.everything()), strings.ToUpper(canary)) {
			t.Error("the statement reached an exported metric, log or span byte")
		}
	})
}

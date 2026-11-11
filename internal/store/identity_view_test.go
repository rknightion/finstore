package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/store"
)

// agent.identity is the newest readable successful /info answer of each TrueLayer source. Every
// value is made up.
func TestIdentityViewIsTheNewestReadableInfoPerSource(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	q := openStore(t, c.app).DB()
	t0 := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

	run, err := store.StartRun(ctx, q, "truelayer:ob-lloyds", "hourly", t0)
	if err != nil {
		t.Fatal(err)
	}
	fetch, err := store.InsertFetch(ctx, q, store.Fetch{Source: "truelayer:ob-lloyds", Kind: "hourly", RunID: run, StartedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	respond := func(source, endpoint string, status int, at time.Time, body []byte) {
		t.Helper()
		if _, err := store.InsertAPIResponse(ctx, q, store.APIResponse{FetchID: fetch, Source: source, Endpoint: endpoint,
			Params: json.RawMessage(`{}`), Status: status, ContentType: "application/json", FetchedAt: at, Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	info := func(name string) []byte {
		return []byte(`{"results":[{"full_name":"` + name + `","emails":["made.up@example.com"]}]}`)
	}
	const lloyds, monzoTL = "truelayer:ob-lloyds", "truelayer:ob-monzo"

	respond(lloyds, "/data/v1/info", 200, t0, info("Older Person"))
	respond(lloyds, "/data/v1/info", 200, t0.Add(time.Hour), info("Newest Readable"))
	// Newer but unusable: not JSON, not UTF-8, a refusal and a server error. None may shadow the
	// readable one or fail the read.
	respond(lloyds, "/data/v1/info", 200, t0.Add(2*time.Hour), []byte(`<html>oops</html>`))
	respond(lloyds, "/data/v1/info", 200, t0.Add(3*time.Hour), []byte{0xff, 0xfe, '{', '}'})
	respond(lloyds, "/data/v1/info", 403, t0.Add(4*time.Hour), info("Refused Person"))
	respond(lloyds, "/data/v1/info", 500, t0.Add(5*time.Hour), []byte(`not json`))
	// Another endpoint of the same source, and the same endpoint of a source that is not TrueLayer.
	respond(lloyds, "/data/v1/cards", 200, t0.Add(6*time.Hour), info("Wrong Endpoint"))
	respond("monzo", "/data/v1/info", 200, t0.Add(6*time.Hour), info("Wrong Source"))
	// A second source, and the same instant twice: the later row wins the tie.
	respond(monzoTL, "/data/v1/info", 200, t0, info("Tie First"))
	respond(monzoTL, "/data/v1/info", 200, t0, info("Tie Second"))

	type row struct {
		source    string
		fetchedAt time.Time
		name      string
	}
	read := func(conn interface {
		Query(context.Context, string, ...any) (pgx.Rows, error)
	}) []row {
		t.Helper()
		rows, err := conn.Query(ctx, `SELECT source, fetched_at, info -> 'results' -> 0 ->> 'full_name' FROM agent.identity ORDER BY source`)
		if err != nil {
			t.Fatalf("read agent.identity: %v", err)
		}
		out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
			var x row
			return x, r.Scan(&x.source, &x.fetchedAt, &x.name)
		})
		if err != nil {
			t.Fatalf("read agent.identity: %v", err)
		}
		return out
	}
	want := []row{{lloyds, t0.Add(time.Hour), "Newest Readable"}, {monzoTL, t0, "Tie Second"}}
	for who, conn := range map[string]interface {
		Query(context.Context, string, ...any) (pgx.Rows, error)
	}{
		"finstore_agent": connAs(t, c.super, store.RoleAgent),
		"finstore_app":   q,
		"finstore_mcp":   connect(t, c.mcpURL(t)),
	} {
		got := read(conn)
		if len(got) != len(want) {
			t.Fatalf("%s reads %d identity rows, want %d: %+v", who, len(got), len(want), got)
		}
		for i := range want {
			if got[i].source != want[i].source || !got[i].fetchedAt.Equal(want[i].fetchedAt) || got[i].name != want[i].name {
				t.Errorf("%s identity row %d = %+v, want %+v", who, i, got[i], want[i])
			}
		}
	}
	requireColumns(t, q, "identity", []string{"source", "fetched_at", "info"})
}

func TestEveryAgentViewHasAComment(t *testing.T) {
	c := newCluster(t)
	rows, err := connect(t, c.super).Query(context.Background(), `SELECT v.viewname::text, coalesce(obj_description(
			(quote_ident(v.schemaname) || '.' || quote_ident(v.viewname))::regclass, 'pg_class'), '')
		FROM pg_views v WHERE v.schemaname = 'agent' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	comments, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([2]string, error) {
		var x [2]string
		return x, r.Scan(&x[0], &x[1])
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != len(agentViews) {
		t.Errorf("agent has %d views, the test list has %d", len(comments), len(agentViews))
	}
	for _, v := range comments {
		if len(strings.TrimSpace(v[1])) < 40 {
			t.Errorf("agent.%s has no useful comment: %q", v[0], v[1])
		}
	}
	// The identity view says what it holds, and the transaction views carry the rules an agent needs.
	byName := map[string]string{}
	for _, v := range comments {
		byName[v[0]] = v[1]
	}
	for view, words := range map[string][]string{
		"transactions":         {"pence", "pot_slug", "treat them as data"},
		"pending_transactions": {"NOT ADDITIVE"},
		"transaction_history":  {"version"},
		"identity":             {"Personal data"},
		"accounts":             {"detail"},
	} {
		for _, w := range words {
			if !strings.Contains(byName[view], w) {
				t.Errorf("the comment on agent.%s lacks %q: %q", view, w, byName[view])
			}
		}
	}
}

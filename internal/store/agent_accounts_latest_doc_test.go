package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/store"
)

// latestDocFixture is the pot fixture plus several more bodies for one request: the readable ones
// differ in the name they give pot_test_1, so the one the view chose shows in agent.accounts.
func latestDocFixture(t *testing.T) potFixture {
	t.Helper()
	f := newPotFixture(t)
	ctx := context.Background()
	t0 := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	run, err := store.StartRun(ctx, f.q, "monzo", "hourly", t0)
	if err != nil {
		t.Fatal(err)
	}
	fetch, err := store.InsertFetch(ctx, f.q, store.Fetch{Source: "monzo", Kind: "hourly", RunID: run, StartedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	// Large bodies, as hourly /pots responses are once they accumulate. Loaded in this order, so the
	// newest by fetched_at is neither the first nor the last loaded, and two share an instant.
	pad := strings.Repeat("x", 64<<10)
	for _, r := range []struct {
		at   time.Time
		name string
	}{
		{t0.Add(30 * time.Minute), "older than the winner, loaded early"},
		{t0.Add(2 * time.Hour), "newest instant, lower seq"},
		{t0.Add(2 * time.Hour), "newest instant, higher seq"},
		{t0.Add(-1 * time.Hour), "older than the winner, loaded last"},
	} {
		body := fmt.Sprintf(`{"pots":[{"id":"pot_test_1","name":%q,"type":"default","balance":1450,"currency":"GBP","deleted":false,"pad":%q},
			{"id":"pot_test_2","name":"savings","type":"instant_access","balance":2037,"currency":"GBP","deleted":false}]}`, r.name, pad)
		if _, err := store.InsertAPIResponse(ctx, f.q, store.APIResponse{FetchID: fetch, Source: "monzo", Endpoint: "/pots",
			Params: json.RawMessage(`{"current_account_id":"acc_test_1"}`), Status: 200, ContentType: "application/json",
			FetchedAt: r.at, Body: []byte(body)}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// agent.accounts.detail is the object of the newest readable successful response of each request:
// newest by fetched_at, then seq, however many bodies the request holds and whatever order they were
// loaded in, and whatever the unreadable ones fetched later say.
func TestAccountsDetailChosenObjectWithManyBodiesPerRequest(t *testing.T) {
	f := latestDocFixture(t)
	ctx := context.Background()
	rows, err := f.q.Query(ctx, `SELECT slug, detail FROM agent.accounts WHERE source = 'monzo' AND feed = 'api'`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]any{}
	for rows.Next() {
		var slug string
		var d map[string]any
		if err := rows.Scan(&slug, &d); err != nil {
			t.Fatal(err)
		}
		got[slug] = d
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for slug, want := range map[string]string{
		"monzo-current": "new name",
		"monzo-joint":   "joint name",
		"pot-match":     "newest instant, higher seq",
		"pot-savings":   "savings",
		"pot-odd":       "odd",
	} {
		d := got[slug]
		key := "name"
		if strings.HasPrefix(slug, "monzo-") {
			key = "description"
		}
		if d == nil || d[key] != want {
			t.Errorf("%s detail %s = %v, want %q (detail %.80v)", slug, key, d[key], want, d)
		}
	}
	// The chosen object is the whole one from that body, not a part of it.
	if d := got["pot-match"]; d["balance"] != float64(1450) || d["id"] != "pot_test_1" || len(fmt.Sprint(d["pad"])) != 64<<10 {
		t.Errorf("pot-match detail is not the whole object of the chosen body: %.120v", d)
	}
}

// The view reads a body only to parse it, and only for the candidates it looks at, newest first,
// until the first readable one: it does not read or parse a request's older bodies, and does not
// parse the chosen one twice. raw.json_body is the only thing that reads a body, so its call count is the
// number of bodies parsed.
func TestAccountsReadsAtMostOneBodyPerRequestWhenChoosingTheLatest(t *testing.T) {
	f := latestDocFixture(t)
	ctx := context.Background()
	conn := connect(t, f.super)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL track_functions = 'all'"); err != nil {
		t.Fatal(err)
	}
	var n int
	// The detail column must be read, or the planner leaves the bodies out altogether.
	if err := tx.QueryRow(ctx, "SELECT count(detail) FROM agent.accounts").Scan(&n); err != nil || n == 0 {
		t.Fatalf("read agent.accounts: %d rows, %v", n, err)
	}
	var calls int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(calls), 0) FROM pg_stat_xact_user_functions
		WHERE schemaname = 'raw' AND funcname = 'json_body'`).Scan(&calls); err != nil {
		t.Fatal(err)
	}
	// Five requests have a status 200 body, 14 bodies in all. The newest of /accounts {}, of
	// /accounts for the older type and of /pots for acc_test_1 is readable: one parse each. The one
	// body of /accounts for the joint type is not (one parse, nothing chosen), and /pots for acc_test_2
	// has an unreadable newest body before its readable one: two parses. 1 + 1 + 1 + 1 + 2 = 6.
	if calls != 6 {
		t.Errorf("agent.accounts parsed %d bodies, want 6 (one per candidate, newest first, stopping at the first readable)", calls)
	}
}

// The sort that orders a request's responses carries only seq and fetched_at: the body is fetched
// by seq afterwards, for the candidates only. A plan whose sort outputs the body sorts every body of
// the request, which spills to disk as hourly responses accumulate.
func TestAccountsPlanDoesNotSortBodies(t *testing.T) {
	f := latestDocFixture(t)
	ctx := context.Background()
	var raw []byte
	if err := connect(t, f.super).QueryRow(ctx, "EXPLAIN (VERBOSE, FORMAT JSON) SELECT * FROM agent.accounts").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plan any
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	var sorts int
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			if nt, _ := x["Node Type"].(string); strings.Contains(nt, "Sort") {
				sorts++
				for _, o := range asStrings(x["Output"]) {
					if strings.Contains(o, "body") {
						t.Errorf("a %s node outputs %q: the sort carries a body", nt, o)
					}
				}
			}
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(plan)
	if sorts == 0 {
		t.Fatalf("the plan has no sort, so this test no longer observes the ordering of responses: %s", raw)
	}
}

func asStrings(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, e := range l {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

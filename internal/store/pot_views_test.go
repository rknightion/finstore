package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/store"
)

// potFixture is a made-up Monzo estate: a current account and a joint account, six pots, a
// file-fed account, pot transfers in every state that matters, and raw /accounts and /pots
// responses of which only the newest readable successful one per request may be read.
type potFixture struct {
	q     store.DBTX
	super string
	ids   map[string]uuid.UUID // transaction key to id
}

func monzoAccount(slug, key, typ string, parent *store.Account) store.Account {
	a := testAccount(slug, typ, "api")
	a.Source, a.SourceKey = "monzo", key
	if parent != nil {
		a.ParentID = &parent.ID
	}
	return a
}

func newPotFixture(t *testing.T) potFixture {
	t.Helper()
	c := newCluster(t)
	ctx := context.Background()
	q := openStore(t, c.app).DB()
	core := mustCore(t, q, store.SchemaCore)
	f := potFixture{q: q, super: c.super, ids: map[string]uuid.UUID{}}

	current := monzoAccount("monzo-current", "acc_test_1", "current", nil)
	joint := monzoAccount("monzo-joint", "acc_test_2", "joint", nil)
	match := monzoAccount("pot-match", "pot_test_1", "pot", &current)
	savings := monzoAccount("pot-savings", "pot_test_2", "pot", &current)
	odd := monzoAccount("pot-odd", "pot_test_3", "pot", &joint)
	nobal := monzoAccount("pot-nobalance", "pot_test_4", "pot", &current)
	nobal.Closed = true
	closedPot := monzoAccount("pot-closed", "pot_test_5", "pot", &joint)
	closedPot.Closed = true
	drained := monzoAccount("pot-drained", "pot_test_6", "pot", &joint)
	file := testAccount("bank-file", "current", "file")
	file.Source = "import:halifax-pdf"
	for _, a := range []store.Account{current, joint, match, savings, odd, nobal, closedPot, drained, file} {
		if err := core.UpsertAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}

	t0 := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	seq := int64(1000)
	txn := func(acc store.Account, key, status string, amount int64, at time.Time, content map[string]any) store.Transaction {
		t.Helper()
		tx := testTxn(acc, key, amount, at)
		tx.Source = acc.Source
		tx.Status = status
		tx.CreatedAt = &at
		tx.Description = "made up " + key
		body, err := json.Marshal(content)
		if err != nil {
			t.Fatal(err)
		}
		seq++
		if _, err := core.ApplyTransaction(ctx, tx, body, body, at, seq); err != nil {
			t.Fatal(err)
		}
		f.ids[key] = tx.ID
		return tx
	}
	potTxn := func(acc store.Account, key, status string, amount int64, at time.Time, potID, trigger string) map[string]any {
		meta := map[string]any{"pot_id": potID}
		if trigger != "" {
			meta["trigger"] = trigger
		}
		content := map[string]any{"id": key, "amount": amount, "currency": "GBP", "scheme": "uk_retail_pot",
			"description": "made up " + key, "metadata": meta}
		if status == "declined" {
			content["decline_reason"] = "INSUFFICIENT_FUNDS"
		}
		txn(acc, key, status, amount, at, content)
		return content
	}

	// Into pot-match from the current account, then changed by a later version (a note added).
	v1 := potTxn(current, "tx_test_into", "settled", -1000, t0, "pot_test_1", "coin_jar")
	v1["notes"] = "second version"
	txn(current, "tx_test_into", "settled", -1000, t0.Add(time.Minute), v1)
	potTxn(current, "tx_test_out", "settled", 300, t0.Add(time.Hour), "pot_test_1", "user")
	potTxn(current, "tx_test_pending", "pending", -500, t0.Add(2*time.Hour), "pot_test_1", "user")
	potTxn(current, "tx_test_declined", "declined", -700, t0.Add(3*time.Hour), "pot_test_1", "user")
	// The joint account pays into a pot whose parent is the current account.
	potTxn(joint, "tx_test_joint", "settled", -250, t0.Add(4*time.Hour), "pot_test_1", "scheduled_payment")
	potTxn(current, "tx_test_savings", "settled", -2000, t0.Add(5*time.Hour), "pot_test_2", "")
	potTxn(joint, "tx_test_odd", "settled", -600, t0.Add(6*time.Hour), "pot_test_3", "user")
	potTxn(current, "tx_test_unknown", "settled", -400, t0.Add(7*time.Hour), "pot_test_unheld", "user")
	potTxn(current, "tx_test_gone", "settled", -900, t0.Add(8*time.Hour), "pot_test_1", "user")
	if _, err := q.Exec(ctx, "UPDATE core.transaction SET disappeared_at = $2 WHERE id = $1", f.ids["tx_test_gone"], t0.Add(9*time.Hour)); err != nil {
		t.Fatal(err)
	}
	txn(current, "tx_test_card", "settled", -1234, t0.Add(10*time.Hour),
		map[string]any{"id": "tx_test_card", "amount": -1234, "metadata": map[string]any{}})
	// A closed plain pot whose transfers do not net to its zero balance, and a savings pot that paid
	// out more than went in (its interest) and is now empty.
	potTxn(joint, "tx_test_closed_in", "settled", -500, t0.Add(11*time.Hour), "pot_test_5", "user")
	potTxn(joint, "tx_test_drained_in", "settled", -1000, t0.Add(12*time.Hour), "pot_test_6", "user")
	potTxn(joint, "tx_test_drained_out", "settled", 1010, t0.Add(13*time.Hour), "pot_test_6", "user")
	// A transfer made after the pot's latest balance reading: listed, but not in the reconciliation.
	potTxn(current, "tx_test_late", "settled", -999, t0.Add(48*time.Hour), "pot_test_1", "user")
	txn(file, "row-1", "settled", -50, t0, map[string]any{"date": "2026-03-01", "type": "DEB",
		"description": "made up row", "money_in": nil, "money_out": "0.50", "balance": "10.00"})

	balance := func(acc store.Account, rawSeq int64, at time.Time, minor int64) {
		t.Helper()
		if err := core.InsertBalanceSnapshot(ctx, store.BalanceSnapshot{AccountID: acc.ID, RawSeq: rawSeq, At: &at,
			OnDate: ptr(day(at, 0)), BalanceMinor: minor, Currency: "GBP"}); err != nil {
			t.Fatal(err)
		}
	}
	balance(match, 2000, t0, 100)
	balance(match, 2001, t0.Add(24*time.Hour), 1450) // 1000 - 300 + 500 + 250
	balance(savings, 2002, t0.Add(24*time.Hour), 2037)
	balance(odd, 2003, t0.Add(24*time.Hour), 900)
	balance(closedPot, 2004, t0.Add(24*time.Hour), 0)
	balance(drained, 2005, t0.Add(24*time.Hour), 0)

	run, err := store.StartRun(ctx, q, "monzo", "hourly", t0)
	if err != nil {
		t.Fatal(err)
	}
	fetch, err := store.InsertFetch(ctx, q, store.Fetch{Source: "monzo", Kind: "hourly", RunID: run, StartedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	respondAt := func(at time.Time, endpoint, params string, status int, body []byte) {
		t.Helper()
		if _, err := store.InsertAPIResponse(ctx, q, store.APIResponse{FetchID: fetch, Source: "monzo", Endpoint: endpoint,
			Params: json.RawMessage(params), Status: status, ContentType: "application/json", FetchedAt: at,
			Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	respond := func(endpoint, params string, status int, body string) {
		t.Helper()
		respondAt(t0, endpoint, params, status, []byte(body))
	}
	respond("/accounts", `{}`, 200, `{"accounts":[{"id":"acc_test_1","description":"old name","type":"uk_retail","closed":false}]}`)
	respond("/pots", `{"current_account_id":"acc_test_1"}`, 200,
		`{"pots":[{"id":"pot_test_1","name":"old pot name","type":"default","balance":100,"currency":"GBP","deleted":false}]}`)
	respond("/accounts", `{}`, 200, `{"accounts":[
		{"id":"acc_test_1","description":"new name","type":"uk_retail","closed":false},
		{"id":"acc_test_2","description":"joint name","type":"uk_retail_joint","closed":false}]}`)
	respond("/pots", `{"current_account_id":"acc_test_1"}`, 200, `{"pots":[
		{"id":"pot_test_1","name":"new pot name","type":"default","balance":1450,"currency":"GBP","deleted":false},
		{"id":"pot_test_2","name":"savings","type":"instant_access","balance":2037,"currency":"GBP","deleted":false},
		{"id":"pot_test_4","name":"closed","type":"default","balance":0,"currency":"GBP","deleted":true}]}`)
	respond("/pots", `{"current_account_id":"acc_test_2"}`, 200, `{"pots":[
		{"id":"pot_test_3","name":"odd","type":"default","balance":900,"currency":"GBP","deleted":false},
		{"id":"pot_test_5","name":"closed plain","type":"default","balance":0,"currency":"GBP","deleted":true},
		{"id":"pot_test_6","name":"drained","type":"flexible_savings","balance":0,"currency":"GBP","deleted":false}]}`)
	respond("/balance", `{"account_id":"acc_test_1"}`, 200, `{"balance":1,"currency":"GBP"}`)
	// A refused request after every successful one: never the latest object.
	respond("/accounts", `{}`, 401, `{"code":"unauthorized"}`)
	respond("/pots", `{"current_account_id":"acc_test_1"}`, 500, `not json`)
	// Successful responses fetched later whose bodies cannot be read (load-capture stores bodies
	// unchecked): one not JSON under parameters seen nowhere else, one not UTF-8 and one without
	// the list, each under the parameters of a good response. None may shadow a good one or fail
	// a read.
	later := t0.Add(time.Hour)
	respondAt(later, "/accounts", `{"account_type":"uk_retail_joint"}`, 200, []byte(`<html>oops</html>`))
	respondAt(later, "/pots", `{"current_account_id":"acc_test_1"}`, 200, []byte{0xff, 0xfe, '{', '}'})
	respondAt(later, "/pots", `{"current_account_id":"acc_test_2"}`, 200, []byte(`{"unexpected":true}`))
	// Captures loaded after the live fetches (higher seq) but fetched two days earlier: the live
	// fetch is newer, under the same parameters and under others.
	earlier := t0.Add(-48 * time.Hour)
	respondAt(earlier, "/accounts", `{}`, 200,
		[]byte(`{"accounts":[{"id":"acc_test_1","description":"stale capture","type":"uk_retail","closed":false}]}`))
	respondAt(earlier, "/accounts", `{"account_type":"uk_retail"}`, 200,
		[]byte(`{"accounts":[{"id":"acc_test_1","description":"stale other params","type":"uk_retail","closed":false}]}`))
	return f
}

// viewColumns lists a view's columns in order.
func viewColumns(t *testing.T, q store.DBTX, view string) []string {
	t.Helper()
	rows, err := q.Query(context.Background(), `SELECT column_name::text FROM information_schema.columns
		WHERE table_schema = 'agent' AND table_name = $1 ORDER BY ordinal_position`, view)
	if err != nil {
		t.Fatal(err)
	}
	cols, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return cols
}

func requireColumns(t *testing.T, q store.DBTX, view string, want []string) {
	t.Helper()
	got := viewColumns(t, q, view)
	if len(got) != len(want) {
		t.Fatalf("agent.%s columns = %v, want %v", view, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("agent.%s columns = %v, want %v", view, got, want)
		}
	}
}

func TestTransactionsDetailAndPotSlug(t *testing.T) {
	f := newPotFixture(t)
	ctx := context.Background()
	requireColumns(t, f.q, "transactions", []string{"id", "account_slug", "txn_date", "created_at", "settled_at",
		"amount_minor", "currency", "local_amount_minor", "local_currency", "status", "description", "counterparty",
		"merchant_name", "category", "scheme", "notes", "version_count", "first_seen_at", "last_seen_at",
		"detail", "pot_slug"})

	type row struct {
		detail  map[string]any
		potSlug *string
	}
	rows, err := f.q.Query(ctx, `SELECT id, detail, pot_slug FROM agent.transactions`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]row{}
	for rows.Next() {
		var id uuid.UUID
		var r row
		if err := rows.Scan(&id, &r.detail, &r.potSlug); err != nil {
			t.Fatal(err)
		}
		got[id] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if _, ok := got[f.ids["tx_test_gone"]]; ok {
		t.Error("a disappeared transaction is listed")
	}

	wantPot := map[string]string{
		"tx_test_into": "pot-match", "tx_test_out": "pot-match", "tx_test_pending": "pot-match",
		"tx_test_declined": "pot-match", "tx_test_joint": "pot-match", "tx_test_savings": "pot-savings",
		"tx_test_odd": "pot-odd", "tx_test_unknown": "", "tx_test_card": "", "row-1": "",
		"tx_test_closed_in": "pot-closed", "tx_test_drained_in": "pot-drained", "tx_test_drained_out": "pot-drained",
		"tx_test_late": "pot-match",
	}
	if len(got) != len(wantPot) {
		t.Errorf("agent.transactions has %d rows, want %d", len(got), len(wantPot))
	}
	for key, want := range wantPot {
		r, ok := got[f.ids[key]]
		if !ok {
			t.Errorf("%s missing", key)
			continue
		}
		switch {
		case want == "" && r.potSlug != nil:
			t.Errorf("%s pot_slug = %q, want null", key, *r.potSlug)
		case want != "" && (r.potSlug == nil || *r.potSlug != want):
			t.Errorf("%s pot_slug = %v, want %q", key, r.potSlug, want)
		}
		if r.detail == nil {
			t.Errorf("%s detail is null", key)
		}
	}
	if d := got[f.ids["tx_test_into"]].detail; d["notes"] != "second version" || d["id"] != "tx_test_into" {
		t.Errorf("detail is not the latest version: %v", d)
	}
	if d := got[f.ids["row-1"]].detail; d["money_out"] != "0.50" || d["type"] != "DEB" {
		t.Errorf("file row detail = %v", d)
	}
}

func TestPotTransactions(t *testing.T) {
	f := newPotFixture(t)
	ctx := context.Background()
	requireColumns(t, f.q, "pot_transactions", []string{"transaction_id", "pot_slug", "account_slug", "txn_date",
		"created_at", "amount_minor", "currency", "description", "trigger", "status", "detail"})

	type row struct {
		pot, account, currency, description, status string
		trigger                                     *string
		amount                                      int64
		date                                        time.Time
		created                                     *time.Time
		detail                                      map[string]any
	}
	rows, err := f.q.Query(ctx, `SELECT transaction_id, pot_slug, account_slug, txn_date, created_at, amount_minor,
		currency, description, trigger, status, detail FROM agent.pot_transactions`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]row{}
	for rows.Next() {
		var id uuid.UUID
		var r row
		if err := rows.Scan(&id, &r.pot, &r.account, &r.date, &r.created, &r.amount, &r.currency, &r.description,
			&r.trigger, &r.status, &r.detail); err != nil {
			t.Fatal(err)
		}
		got[id] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	type want struct {
		pot, account, trigger, status string
		amount                        int64
	}
	wants := map[string]want{
		"tx_test_into":        {"pot-match", "monzo-current", "coin_jar", "settled", 1000},
		"tx_test_out":         {"pot-match", "monzo-current", "user", "settled", -300},
		"tx_test_pending":     {"pot-match", "monzo-current", "user", "pending", 500},
		"tx_test_joint":       {"pot-match", "monzo-joint", "scheduled_payment", "settled", 250},
		"tx_test_savings":     {"pot-savings", "monzo-current", "", "settled", 2000},
		"tx_test_odd":         {"pot-odd", "monzo-joint", "user", "settled", 600},
		"tx_test_closed_in":   {"pot-closed", "monzo-joint", "user", "settled", 500},
		"tx_test_drained_in":  {"pot-drained", "monzo-joint", "user", "settled", 1000},
		"tx_test_drained_out": {"pot-drained", "monzo-joint", "user", "settled", -1010},
		"tx_test_late":        {"pot-match", "monzo-current", "user", "settled", 999},
	}
	if len(got) != len(wants) {
		t.Errorf("agent.pot_transactions has %d rows, want %d (no declined, unheld-pot, non-pot or disappeared rows)",
			len(got), len(wants))
	}
	for key, w := range wants {
		r, ok := got[f.ids[key]]
		if !ok {
			t.Errorf("%s missing", key)
			continue
		}
		trig := ""
		if r.trigger != nil {
			trig = *r.trigger
		}
		if r.pot != w.pot || r.account != w.account || trig != w.trigger || r.status != w.status || r.amount != w.amount ||
			r.currency != "GBP" || r.description != "made up "+key || r.created == nil || r.date.IsZero() ||
			r.detail["id"] != key {
			t.Errorf("%s = %+v, want %+v", key, r, w)
		}
	}
	if got[f.ids["tx_test_into"]].detail["notes"] != "second version" {
		t.Error("pot_transactions detail is not the latest version")
	}
}

func TestPotReconciliation(t *testing.T) {
	f := newPotFixture(t)
	ctx := context.Background()
	requireColumns(t, f.q, "pot_reconciliation", []string{"pot_slug", "parent_slug", "pot_type", "closed",
		"balance_minor", "balance_at", "transfers_net_minor", "transfer_count", "difference_minor", "reason"})

	type row struct {
		parent     string
		potType    *string
		closed     bool
		balance    *int64
		at         *time.Time
		net, count int64
		difference *int64
		reason     string
	}
	rows, err := f.q.Query(ctx, `SELECT pot_slug, parent_slug, pot_type, closed, balance_minor, balance_at,
		transfers_net_minor, transfer_count, difference_minor, reason FROM agent.pot_reconciliation`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]row{}
	for rows.Next() {
		var slug string
		var r row
		if err := rows.Scan(&slug, &r.parent, &r.potType, &r.closed, &r.balance, &r.at, &r.net, &r.count,
			&r.difference, &r.reason); err != nil {
			t.Fatal(err)
		}
		got[slug] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("agent.pot_reconciliation has %d rows, want one per pot: %+v", len(got), got)
	}

	type want struct {
		parent, potType string
		balance         *int64
		net, count      int64
		difference      *int64
		reason          string
	}
	wants := map[string]want{
		"pot-match":     {"monzo-current", "default", ptr[int64](1450), 1450, 4, ptr[int64](0), "matches"},
		"pot-savings":   {"monzo-current", "instant_access", ptr[int64](2037), 2000, 1, ptr[int64](37), "interest_not_listed"},
		"pot-odd":       {"monzo-joint", "default", ptr[int64](900), 600, 1, ptr[int64](300), "unexplained"},
		"pot-nobalance": {"monzo-current", "default", nil, 0, 0, nil, "unexplained"},
		"pot-closed":    {"monzo-joint", "default", ptr[int64](0), 500, 1, ptr[int64](-500), "unexplained"},
		"pot-drained":   {"monzo-joint", "flexible_savings", ptr[int64](0), -10, 2, ptr[int64](10), "interest_not_listed"},
	}
	eq := func(a, b *int64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	for slug, w := range wants {
		r := got[slug]
		if r.parent != w.parent || r.potType == nil || *r.potType != w.potType || !eq(r.balance, w.balance) ||
			r.net != w.net || r.count != w.count || !eq(r.difference, w.difference) || r.reason != w.reason {
			t.Errorf("%s = %+v, want %+v", slug, r, w)
		}
		if (w.balance == nil) != (r.at == nil) {
			t.Errorf("%s balance_at = %v", slug, r.at)
		}
	}
	if !got["pot-nobalance"].closed || got["pot-match"].closed {
		t.Error("closed is not the account's closed flag")
	}
}

func TestAccountsDetail(t *testing.T) {
	f := newPotFixture(t)
	ctx := context.Background()
	requireColumns(t, f.q, "accounts", []string{"slug", "source", "type", "feed", "currency", "name",
		"parent_slug", "opened_on", "closed", "detail", "bank_ref", "ended_on"})

	rows, err := f.q.Query(ctx, `SELECT slug, detail FROM agent.accounts`)
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
	check := func(slug, key, want string) {
		t.Helper()
		d, ok := got[slug]
		if !ok || d == nil {
			t.Errorf("%s detail is null", slug)
			return
		}
		if d[key] != want {
			t.Errorf("%s detail %s = %v, want %q", slug, key, d[key], want)
		}
	}
	check("monzo-current", "description", "new name")
	check("monzo-joint", "type", "uk_retail_joint")
	check("pot-match", "name", "new pot name")
	check("pot-savings", "type", "instant_access")
	check("pot-odd", "name", "odd")
	if d := got["pot-nobalance"]; d == nil || d["deleted"] != true {
		t.Errorf("pot-nobalance detail = %v", d)
	}
	if d, ok := got["bank-file"]; !ok || d != nil {
		t.Errorf("file-fed account detail = %v (present %v), want null", d, ok)
	}

	// The reader role reads the same, through views that parse raw bodies with raw.json_body, but
	// cannot call that function itself: it has no USAGE on raw.
	agent := connAs(t, f.super, store.RoleAgent)
	var desc, potType string
	if err := agent.QueryRow(ctx, `SELECT a.detail ->> 'description', r.pot_type FROM agent.accounts a,
		agent.pot_reconciliation r WHERE a.slug = 'monzo-current' AND r.pot_slug = 'pot-drained'`).Scan(&desc, &potType); err != nil {
		t.Fatalf("finstore_agent reading the views: %v", err)
	}
	if desc != "new name" || potType != "flexible_savings" {
		t.Errorf("finstore_agent reads %q, %q", desc, potType)
	}
	_, err = agent.Exec(ctx, `SELECT raw.json_body('\x7b7d'::bytea)`)
	requireDenied(t, err, "finstore_agent calling raw.json_body")
}

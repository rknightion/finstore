package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rknightion/finstore/internal/store"
)

// mandateFixture is a made-up estate for the views this change adds or alters: a statement
// account that ended 40 days ago, the API account that continues it under the same bank reference, an
// unrelated pair that shares a bank reference with neither ended, and mandates attached,
// unattached and disappeared.
type mandateFixture struct {
	q     store.DBTX
	super string
	today time.Time
	ended store.Account // bank-joint, ended 40 days ago
	cont  store.Account // lloyds-joint, from 39 days ago
}

const (
	refJoint  = "112233 12345678"
	refShared = "445566 87654321"
)

func newMandateFixture(t *testing.T) mandateFixture {
	t.Helper()
	c := newCluster(t)
	ctx := context.Background()
	q := openStore(t, c.app).DB()
	core := mustCore(t, q, store.SchemaCore)
	today := londonToday(t, q)
	f := mandateFixture{q: q, super: c.super, today: today}

	f.ended = testAccount("bank-joint", "joint", "file")
	f.ended.Source = "manual"
	f.ended.ExpectsImports = true
	f.ended.BankRef = ptr(refJoint)
	f.ended.EndedOn = ptr(day(today, -40))
	f.cont = testAccount("lloyds-joint", "joint", "api")
	f.cont.Source, f.cont.SourceKey = "truelayer:ob-lloyds", refJoint
	f.cont.BankRef = ptr(refJoint)
	f.cont.OpenedOn = ptr(day(today, -39))
	sharedA := testAccount("shared-a", "current", "file")
	sharedA.Source, sharedA.BankRef = "manual", ptr(refShared)
	sharedB := testAccount("shared-b", "current", "api")
	sharedB.Source, sharedB.BankRef = "monzo", ptr(refShared)
	for _, a := range []store.Account{f.ended, f.cont, sharedA, sharedB} {
		if err := core.UpsertAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []store.Account{sharedA, sharedB} {
		if err := core.PutReconciliation(ctx, store.Reconciliation{AccountID: a.ID, Method: "monzo_absolute",
			LastCheckOK: ptr(false), KnownException: ptr("shared_bank_ref")}); err != nil {
			t.Fatal(err)
		}
	}
	if err := core.PutReconciliation(ctx, store.Reconciliation{AccountID: f.cont.ID, Method: "running_balance",
		LastCheckOK: ptr(false), BreakCount: 1, FailedOn: ptr(day(today, -3)), ReconciledThrough: ptr(day(today, -4))}); err != nil {
		t.Fatal(err)
	}

	// Rows either side of the end date on the ended account; one on the account that continues it.
	t0 := time.Now()
	// Its newest row up to the end is over 35 days old, so only the end keeps it from import due.
	for i, d := range []int{-41, -40, -39, -2} {
		tx := testTxn(f.ended, "row-"+string(rune('a'+i)), -100, day(today, d))
		if _, err := core.ApplyTransaction(ctx, tx, []byte(`{}`), []byte(tx.SourceKey), t0, int64(i+1)); err != nil {
			t.Fatal(err)
		}
		if err := core.InsertBalanceSnapshot(ctx, store.BalanceSnapshot{AccountID: f.ended.ID, RawSeq: int64(100 + i),
			OnDate: ptr(day(today, d)), BalanceMinor: int64(1000 - 100*i), Currency: "GBP"}); err != nil {
			t.Fatal(err)
		}
	}
	at := londonMidnight(t, day(today, -1)).Add(time.Hour)
	if err := core.InsertBalanceSnapshot(ctx, store.BalanceSnapshot{AccountID: f.ended.ID, RawSeq: 200, At: &at,
		BalanceMinor: 1, Currency: "GBP"}); err != nil {
		t.Fatal(err)
	}
	tx := testTxn(f.cont, "lloyds-row", -100, day(today, -39))
	if _, err := core.ApplyTransaction(ctx, tx, []byte(`{}`), []byte("h"), t0, 10); err != nil {
		t.Fatal(err)
	}
	// The continuing account's coverage comes from core.fetch_period: one period whose start is
	// part-way through its first day, which covers that day.
	if err := core.PutFetchPeriod(ctx, store.FetchPeriod{FetchID: 77, AccountID: f.cont.ID,
		PeriodStart: londonMidnight(t, day(today, -39)).Add(13 * time.Hour), PeriodEnd: londonMidnight(t, today).Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	amount := func(n int64) *int64 { return &n }
	so := func(key string, acc *store.Account, gone bool) {
		t.Helper()
		s := store.StandingOrder{ID: uuid.NewSHA1(testNS, []byte("so/"+key)), BankRef: refJoint, Source: "truelayer:ob-lloyds",
			MandateKey: key, Payee: ptr("MADE UP PAYEE"), Reference: ptr("MADE UP REF"), Frequency: ptr("MONTHLY"),
			Status: ptr("ACTIVE"), FirstDate: ptr(day(today, -100)), FirstAmountMinor: amount(-1000),
			NextDate: ptr(day(today, 5)), NextAmountMinor: amount(-1000), Currency: ptr("GBP")}
		if acc != nil {
			s.AccountID = &acc.ID
		}
		content, _ := json.Marshal(map[string]any{"payee": "MADE UP PAYEE", "key": key})
		if _, err := core.ApplyStandingOrder(ctx, s, content, []byte(key), t0, 20); err != nil {
			t.Fatal(err)
		}
		if gone {
			if _, _, err := core.SetMandatesDisappeared(ctx, store.MandateStandingOrder, s.Source, s.BankRef,
				[]uuid.UUID{uuid.NewSHA1(testNS, []byte("so/kept"))}, t0.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
	}
	so("kept", &f.cont, false)
	so("cancelled", &f.cont, true)
	dd := store.DirectDebit{ID: uuid.NewSHA1(testNS, []byte("dd/1")), BankRef: "999999 00000001", Source: "truelayer:ob-monzo",
		MandateKey: "MADE-UP-MANDATE", Name: ptr("MADE UP COMPANY"), Status: ptr("Active"),
		PreviousDate: ptr(day(today, -20)), PreviousAmountMinor: amount(-2500), Currency: ptr("GBP")}
	if _, err := core.ApplyDirectDebit(ctx, dd, []byte(`{"name":"MADE UP COMPANY"}`), []byte("dd"), t0, 21); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestMandateViews(t *testing.T) {
	f := newMandateFixture(t)
	ctx := context.Background()
	requireColumns(t, f.q, "standing_orders", []string{"id", "account_slug", "source", "bank_ref", "mandate_key",
		"payee", "reference", "frequency", "status", "first_date", "first_amount_minor", "next_date",
		"next_amount_minor", "final_date", "final_amount_minor", "currency", "version_count", "first_seen_at",
		"last_seen_at", "disappeared_at", "detail"})
	requireColumns(t, f.q, "direct_debits", []string{"id", "account_slug", "source", "bank_ref", "mandate_key",
		"name", "status", "previous_date", "previous_amount_minor", "currency", "version_count", "first_seen_at",
		"last_seen_at", "disappeared_at", "detail"})

	type soRow struct {
		slug     *string
		payee    string
		gone     bool
		detail   map[string]any
		next     time.Time
		nextMins int64
	}
	rows, err := f.q.Query(ctx, `SELECT mandate_key, account_slug, payee, disappeared_at IS NOT NULL, detail, next_date,
		next_amount_minor FROM agent.standing_orders`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]soRow{}
	for rows.Next() {
		var key string
		var r soRow
		if err := rows.Scan(&key, &r.slug, &r.payee, &r.gone, &r.detail, &r.next, &r.nextMins); err != nil {
			t.Fatal(err)
		}
		got[key] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if k := got["kept"]; len(got) != 2 || k.slug == nil || *k.slug != "lloyds-joint" || k.gone || k.detail["key"] != "kept" ||
		k.payee != "MADE UP PAYEE" || !k.next.Equal(day(f.today, 5)) || k.nextMins != -1000 {
		t.Fatalf("standing orders %+v", got)
	}
	if c := got["cancelled"]; !c.gone {
		t.Fatalf("a mandate missing from a complete list is not shown as disappeared: %+v", c)
	}

	var slug *string
	var name string
	var detail map[string]any
	if err := f.q.QueryRow(ctx, `SELECT account_slug, name, detail FROM agent.direct_debits`).Scan(&slug, &name, &detail); err != nil {
		t.Fatal(err)
	}
	if slug != nil || name != "MADE UP COMPANY" || detail["name"] != "MADE UP COMPANY" {
		t.Fatalf("unresolved direct debit: slug %v name %q detail %v", slug, name, detail)
	}

	// The reader role reads both views.
	agent := connAs(t, f.super, store.RoleAgent)
	var n int
	if err := agent.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent.standing_orders) + (SELECT count(*) FROM agent.direct_debits)`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("finstore_agent reads %d mandates, %v", n, err)
	}
}

func TestAccountsShowBankRefAndEndDate(t *testing.T) {
	f := newMandateFixture(t)
	ctx := context.Background()
	requireColumns(t, f.q, "accounts", []string{"slug", "source", "type", "feed", "currency", "name",
		"parent_slug", "opened_on", "closed", "detail", "bank_ref", "ended_on"})
	var ref *string
	var ended *time.Time
	if err := f.q.QueryRow(ctx, `SELECT bank_ref, ended_on FROM agent.accounts WHERE slug = 'bank-joint'`).Scan(&ref, &ended); err != nil {
		t.Fatal(err)
	}
	if ref == nil || *ref != refJoint || ended == nil || !ended.Equal(day(f.today, -40)) {
		t.Fatalf("bank-joint bank_ref %v ended_on %v", ref, ended)
	}
	if err := f.q.QueryRow(ctx, `SELECT bank_ref, ended_on FROM agent.accounts WHERE slug = 'lloyds-joint'`).Scan(&ref, &ended); err != nil {
		t.Fatal(err)
	}
	if ref == nil || *ref != refJoint || ended != nil {
		t.Fatalf("lloyds-joint bank_ref %v ended_on %v", ref, ended)
	}
}

func TestEndedAccountRowsAfterTheEndAreLeftOutOfTheViews(t *testing.T) {
	f := newMandateFixture(t)
	ctx := context.Background()

	var dates []time.Time
	rows, err := f.q.Query(ctx, `SELECT txn_date FROM agent.transactions WHERE account_slug = 'bank-joint' ORDER BY txn_date`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		dates = append(dates, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(dates) != 2 || !dates[1].Equal(day(f.today, -40)) {
		t.Fatalf("bank-joint transactions on %v, want the two up to its end date", dates)
	}
	// Balances: the two dated on or before the end; the one after it and the timed one after it are
	// left out.
	var n int
	if err := f.q.QueryRow(ctx, `SELECT count(*) FROM agent.balances WHERE account_slug = 'bank-joint'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("bank-joint balances %d, %v; want 2", n, err)
	}

	cov, err := store.Coverage(ctx, f.q)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]store.CoverageRow{}
	for _, r := range cov {
		by[r.AccountSlug] = r
	}
	e := by["bank-joint"]
	if e.TransactionCount != 2 || e.LatestDate == nil || !e.LatestDate.Equal(day(f.today, -40)) ||
		e.LatestStatementDate == nil || !e.LatestStatementDate.Equal(day(f.today, -40)) {
		t.Fatalf("ended account coverage %+v", e)
	}
	for _, g := range e.Gaps {
		if g.Reason == store.GapImportDue {
			t.Fatalf("an ended account is reported as import due: %+v", e.Gaps)
		}
	}

	// The continuing account is covered from the first day of its fetch period, which started
	// part-way through that day, and its running_balance failure names its day.
	l := by["lloyds-joint"]
	if len(l.Gaps) != 0 || l.ReconciliationState != store.StateMismatch || l.FailedOn == nil || !l.FailedOn.Equal(day(f.today, -3)) {
		t.Fatalf("lloyds-joint coverage %+v", l)
	}

	// Two accounts of different sources share a bank reference, neither ended: both mismatch,
	// each naming the other.
	for slug, other := range map[string]string{"shared-a": "shared-b", "shared-b": "shared-a"} {
		r := by[slug]
		found := false
		for _, g := range r.Gaps {
			if g.Reason == store.GapSharedBankRef+other {
				found = true
			}
		}
		if r.ReconciliationState != store.StateMismatch || !found {
			t.Fatalf("%s coverage %+v, want a mismatch naming %s", slug, r, other)
		}
	}
	// The ended account and its continuation share a reference but are not reported.
	for _, slug := range []string{"bank-joint", "lloyds-joint"} {
		for _, g := range by[slug].Gaps {
			if g.Reason == store.GapSharedBankRef+"bank-joint" || g.Reason == store.GapSharedBankRef+"lloyds-joint" {
				t.Fatalf("%s reports its ended twin: %+v", slug, by[slug].Gaps)
			}
		}
	}
}

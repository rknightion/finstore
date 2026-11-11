package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rknightion/finstore/internal/store"
)

type wantGap struct {
	from, to time.Time
	reason   string
}

func TestCoverageGaps(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	q := openStore(t, c.app).DB()
	core := mustCore(t, q, store.SchemaCore)
	today := londonToday(t, q)
	mid := func(offset int) time.Time { return londonMidnight(t, day(today, offset)) }

	// An API account opened 30 days ago. Completed fetches cover days -30..-21 and -10..today; a
	// fetch of -20..-11 was cut short; an unfinished backfill window spans -15..-13.
	api := testAccount("monzo-current", "current", "api")
	api.Source, api.SourceKey = "monzo", "acc_1"
	api.OpenedOn = ptr(day(today, -30))
	if err := core.UpsertAccount(ctx, api); err != nil {
		t.Fatal(err)
	}
	runID, err := store.StartRun(ctx, q, "monzo", "backfill", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	fetch := func(from, to time.Time, complete bool) {
		t.Helper()
		id, err := store.InsertFetch(ctx, q, store.Fetch{Source: "monzo", Kind: "backfill", AccountKey: ptr("acc_1"),
			PeriodStart: &from, PeriodEnd: &to, RunID: runID, StartedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		if complete {
			if _, err := store.InsertFetchComplete(ctx, q, store.FetchComplete{FetchID: id, CompletedAt: time.Now(), Pages: 1}); err != nil {
				t.Fatal(err)
			}
			// The ledger records each completed fetch's period in core.fetch_period, which coverage reads.
			if err := core.PutFetchPeriod(ctx, store.FetchPeriod{FetchID: id, AccountID: api.ID, PeriodStart: from, PeriodEnd: to}); err != nil {
				t.Fatal(err)
			}
		}
	}
	fetch(mid(-30), mid(-20), true)
	fetch(mid(-20), mid(-10), false)
	fetch(mid(-10), time.Now(), true)
	if _, err := store.UpsertBackfillWindow(ctx, q, "acc_1", mid(-15), mid(-12)); err != nil {
		t.Fatal(err)
	}
	if err := core.EnsureReconciliation(ctx, api.ID, "monzo_absolute"); err != nil {
		t.Fatal(err)
	}
	if err := core.PutReconciliation(ctx, store.Reconciliation{AccountID: api.ID, Method: "monzo_absolute",
		LastCheckOK: ptr(true), LastPassAt: ptr(time.Now())}); err != nil {
		t.Fatal(err)
	}

	// A second API account whose first range took two fetches: the first was cut short and has no
	// completion, the second resumed part-way and completed only what it listed, and the range's
	// window is complete. Its last range ends in the future and was completed 8 days ago, so it
	// covers no later than that; a completed fetch covers the last 3 days.
	resumed := testAccount("monzo-resumed", "joint", "api")
	resumed.Source, resumed.SourceKey = "monzo", "acc_2"
	resumed.OpenedOn = ptr(day(today, -30))
	if err := core.UpsertAccount(ctx, resumed); err != nil {
		t.Fatal(err)
	}
	fetch2 := func(from, to time.Time, complete bool) {
		t.Helper()
		id, err := store.InsertFetch(ctx, q, store.Fetch{Source: "monzo", Kind: "backfill", AccountKey: ptr("acc_2"),
			PeriodStart: &from, PeriodEnd: &to, RunID: runID, StartedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		if complete {
			if _, err := store.InsertFetchComplete(ctx, q, store.FetchComplete{FetchID: id, CompletedAt: time.Now(), Pages: 1}); err != nil {
				t.Fatal(err)
			}
			if err := core.PutFetchPeriod(ctx, store.FetchPeriod{FetchID: id, AccountID: resumed.ID, PeriodStart: from, PeriodEnd: to}); err != nil {
				t.Fatal(err)
			}
		}
	}
	window := func(from, to, completedAt time.Time) {
		t.Helper()
		w, err := store.UpsertBackfillWindow(ctx, q, "acc_2", from, to)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CompleteBackfillWindow(ctx, q, w.ID, completedAt); err != nil {
			t.Fatal(err)
		}
	}
	fetch2(mid(-30), mid(-20), false)
	fetch2(mid(-25), mid(-20), true)
	window(mid(-30), mid(-20), time.Now())
	window(mid(-20), mid(100), mid(-8))
	fetch2(mid(-3), time.Now(), true)

	// A pot, which only ever has balances.
	pot := testAccount("monzo-pot", "pot", "api")
	pot.Source, pot.SourceKey = "monzo", "pot_1"
	pot.OpenedOn = ptr(day(today, -30))
	if err := core.UpsertAccount(ctx, pot); err != nil {
		t.Fatal(err)
	}

	// File accounts: one expecting imports and last imported 36 days ago (with a break between
	// two earlier files), one last imported 35 days ago, a joint account 40 days ago (the account
	// type does not matter, only the flag), and a mortgage that expects none, imported long ago.
	fileAcc := func(slug, typ string, lastDay int, expects bool) store.Account {
		t.Helper()
		a := testAccount(slug, typ, "file")
		a.ExpectsImports = expects
		if err := core.UpsertAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
		tx := testTxn(a, "row-1", -100, day(today, lastDay))
		if _, err := core.ApplyTransaction(ctx, tx, []byte(`{}`), []byte("h"), time.Now(), 1); err != nil {
			t.Fatal(err)
		}
		return a
	}
	joint := fileAcc("joint", "current", -36, true)
	fileAcc("joint-recent", "current", -35, true)
	fileAcc("shared", "joint", -40, true)
	fileAcc("mortgage", "mortgage", -100, false)
	if err := core.InsertFileGap(ctx, store.FileGap{AccountID: joint.ID, AfterDate: day(today, -60),
		BeforeDate: day(today, -50), DiffMinor: 1234, RawSeq: 99}); err != nil {
		t.Fatal(err)
	}

	rows, err := store.Coverage(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	bySlug := make(map[string]store.CoverageRow)
	for _, r := range rows {
		bySlug[r.AccountSlug] = r
	}
	if len(bySlug) != 7 {
		t.Fatalf("coverage has %d rows, want one per account: %+v", len(bySlug), rows)
	}

	checkGaps := func(slug string, want []wantGap) {
		t.Helper()
		got := bySlug[slug].Gaps
		if len(got) != len(want) {
			t.Fatalf("%s gaps = %+v, want %+v", slug, got, want)
		}
		for i, w := range want {
			g := got[i]
			if g.From == nil || !g.From.Equal(w.from) || !g.To.Equal(w.to) || g.Reason != w.reason {
				t.Errorf("%s gap %d = %v..%v %q, want %v..%v %q", slug, i, g.From, g.To, g.Reason, w.from, w.to, w.reason)
			}
		}
	}
	checkGaps("monzo-current", []wantGap{
		{day(today, -20), day(today, -16), store.GapNoCompletedFetch},
		{day(today, -15), day(today, -13), store.GapBackfillUnfinished},
		{day(today, -12), day(today, -11), store.GapNoCompletedFetch},
	})
	checkGaps("monzo-resumed", []wantGap{
		{day(today, -8), day(today, -4), store.GapNoCompletedFetch},
	})
	checkGaps("joint", []wantGap{
		{day(today, -60), day(today, -50), store.GapNoFile},
		{day(today, -35), today, store.GapImportDue},
	})
	if d := bySlug["joint"].Gaps[0].DiffMinor; d == nil || *d != 1234 {
		t.Errorf("file gap difference = %v, want 1234", d)
	}
	checkGaps("joint-recent", nil)
	checkGaps("shared", []wantGap{{day(today, -39), today, store.GapImportDue}})
	checkGaps("mortgage", nil)
	checkGaps("monzo-pot", nil)

	if s := bySlug["monzo-current"].ReconciliationState; s != store.StateReconciled {
		t.Errorf("monzo-current state %q, want reconciled", s)
	}
	if s := bySlug["monzo-pot"].ReconciliationState; s != store.StateBalancesOnly {
		t.Errorf("pot state %q, want balances_only", s)
	}
	if s := bySlug["mortgage"].ReconciliationState; s != store.StateNotChecked {
		t.Errorf("mortgage state %q, want not_checked", s)
	}
	j := bySlug["joint"]
	if j.TransactionCount != 1 || j.LatestDate == nil || !j.LatestDate.Equal(day(today, -36)) || j.Feed != "file" {
		t.Errorf("joint row %+v", j)
	}

	// latest_statement_date: every file account's newest statement date, whatever its type or
	// flag, taking a balance snapshot newer than its newest transaction; null for API accounts.
	if err := core.InsertBalanceSnapshot(ctx, store.BalanceSnapshot{AccountID: joint.ID, RawSeq: 98, Ordinal: 0,
		OnDate: ptr(day(today, -30)), BalanceMinor: 0, Currency: "GBP"}); err != nil {
		t.Fatal(err)
	}
	rows, err = store.Coverage(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	wantLatest := map[string]*time.Time{
		"joint": ptr(day(today, -30)), "joint-recent": ptr(day(today, -35)), "shared": ptr(day(today, -40)),
		"mortgage": ptr(day(today, -100)), "monzo-current": nil, "monzo-resumed": nil, "monzo-pot": nil,
	}
	for _, r := range rows {
		w, ok := wantLatest[r.AccountSlug]
		if !ok {
			continue
		}
		if (w == nil) != (r.LatestStatementDate == nil) || (w != nil && !r.LatestStatementDate.Equal(*w)) {
			t.Errorf("%s latest_statement_date = %v, want %v", r.AccountSlug, r.LatestStatementDate, w)
		}
	}
}

func TestSyncStatus(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	q := openStore(t, c.app).DB()
	core := mustCore(t, q, store.SchemaCore)
	now := time.Now().UTC().Truncate(time.Microsecond)

	id, err := store.CreateConsent(ctx, q, "monzo", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentConsent(ctx, q, id); err != nil {
		t.Fatal(err)
	}
	expiry := now.Add(10*24*time.Hour + time.Hour)
	if _, err := store.TransitionConsent(ctx, q, id, store.ConsentTransition{To: store.ConsentActive, At: now, ExpectedExpiryAt: &expiry}); err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, q, "monzo", "hourly", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, q, run, store.RunResult{FinishedAt: now, Outcome: "partial"}); err != nil {
		t.Fatal(err)
	}
	acc := store.Account{ID: uuid.New(), Source: "file", SourceKey: "joint", Slug: "joint", Type: "current",
		Feed: "file", Currency: "GBP", Name: "joint"}
	if err := core.UpsertAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.InsertImportFile(ctx, q, store.ImportFile{AccountSlug: "joint", Format: "manual-csv",
		FileName: "x.csv", ImportedAt: now, Body: []byte("date,type,amount,balance_after,description\n")}); err != nil {
		t.Fatal(err)
	}

	rows, err := store.SyncStatus(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Source != "file" || rows[1].Source != "monzo" {
		t.Fatalf("sync status rows %+v", rows)
	}
	m := rows[1]
	if m.ConsentState == nil || *m.ConsentState != store.ConsentActive || m.DaysToExpiry == nil || *m.DaysToExpiry != 10 ||
		m.LastRunOutcome == nil || *m.LastRunOutcome != "partial" || m.LastImportAt != nil {
		t.Errorf("monzo row %+v", m)
	}
	f := rows[0]
	if f.ConsentState != nil || f.LastImportAt["joint"] == nil || !f.LastImportAt["joint"].Equal(now) {
		t.Errorf("file row %+v", f)
	}
}

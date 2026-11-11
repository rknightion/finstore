package truelayer

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rknightion/finstore/internal/consent"
	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/truelayertest"
)

// templateOf returns a logged request's endpoint template.
func templateOf(path string) string {
	for _, family := range []string{"accounts", "cards"} {
		rest, ok := strings.CutPrefix(path, "/data/v1/"+family+"/")
		if !ok {
			continue
		}
		_, kind, _ := strings.Cut(rest, "/")
		return "/data/v1/" + family + "/{id}/" + kind
	}
	return path
}

// The auth link names the bank, the shared callback and the scopes, and carries the state.
func TestAuthoriseURL(t *testing.T) {
	h := newHarness(t)
	w := h.worker(ConnLloyds, nil)
	link, err := w.AuthoriseURL(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	q := u.Query()
	if q.Get("provider_id") != ConnLloyds || q.Get("redirect_uri") != "https://finstore.example.test/callback/truelayer" ||
		q.Get("scope") != Scopes || q.Get("response_type") != "code" || q.Get("client_id") != truelayertest.DefaultClientID ||
		len(q.Get("state")) < 43 {
		t.Fatalf("auth link %s", link)
	}
}

// AC3: a connect exchanges the code, stores the pair and the consent together, approves the
// consent until the end /me reports, and runs the connect-time fetch on the exchanged token, never
// refreshed even with less than the margin left, with transactions from 2024-08-01 before each
// balance.
func TestConnectFetchesFullHistoryWithTheExchangedTokenAndTransactionsBeforeBalance(t *testing.T) {
	// A 60-second token is inside the five-minute margin from the start: a fetch that refreshed
	// first would show a second token call.
	h := newHarness(t, truelayertest.WithTokenLifetime(time.Minute), truelayertest.WithConsentLifetime(89*24*time.Hour+7*time.Hour))
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)

	c := h.consent(Source(ConnLloyds))
	if c.State != store.ConsentActive || c.ExpectedExpiryAt == nil || !c.ExpectedExpiryAt.Equal(h.fake.ConsentEnd(ConnLloyds)) {
		t.Fatalf("consent %s expiry %v, want active until %v", c.State, c.ExpectedExpiryAt, h.fake.ConsentEnd(ConnLloyds))
	}
	tok, err := store.GetOAuthToken(context.Background(), h.st.DB(), Source(ConnLloyds))
	if err != nil || tok.RefreshToken == "" {
		t.Fatalf("stored token: %v", err)
	}
	if n := h.tokenCalls(); n != 1 {
		t.Fatalf("%d token calls, want the one exchange", n)
	}
	reqs := h.fake.Requests()
	var paths []string
	exchanged := reqs[1].Bearer
	for _, r := range reqs[1:] {
		paths = append(paths, templateOf(r.Path))
		if r.Bearer != exchanged {
			t.Errorf("%s used another token than the exchanged one", r.Path)
		}
		if templateOf(r.Path) == pathTransactions && (r.Query.Get("from") != "2024-08-01" || r.Query.Get("to") != "2026-11-11") {
			t.Errorf("history asked %v, want 2024-08-01 to 2026-11-11", r.Query)
		}
	}
	perAccount := []string{pathStandingOrders, pathDirectDebits, pathTransactions, pathBalance}
	want := append(append(append([]string{pathMe, pathAccounts}, perAccount...), perAccount...), pathCards, pathPending, pathPending, pathInfo)
	if !slices.Equal(paths, want) {
		t.Fatalf("calls %v\nwant  %v", paths, want)
	}
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindConnect, outcomeOK, "")

	// /me is under a meta fetch; the transactions fetch covers 2024-08-01 to the end of today in
	// London, and is complete.
	var sawMeta, sawPeriod bool
	for r, err := range store.RawRowsAfter(context.Background(), h.st.DB(), 0) {
		if err != nil {
			t.Fatal(err)
		}
		if r.Kind == store.RawAPIResponse && r.APIResponse.Endpoint == pathMe {
			f, _ := store.GetFetch(context.Background(), h.st.DB(), r.APIResponse.FetchID)
			sawMeta = f.Kind == kindMeta && f.RunID == 0
		}
		if r.Kind == store.RawFetch && r.Fetch.PeriodStart != nil {
			sawPeriod = r.Fetch.Kind == KindConnect &&
				r.Fetch.PeriodStart.Equal(time.Date(2024, 7, 31, 23, 0, 0, 0, time.UTC)) &&
				r.Fetch.PeriodEnd.Equal(time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC))
		}
		if r.Kind == store.RawAPIResponse && r.APIResponse.Endpoint == pathTransactions {
			p := params(t, r.APIResponse.Params)
			if p["bank_ref"] == nil || p["account_id"] == nil || p["from"] != "2024-08-01" || p["to"] != "2026-11-11" {
				t.Errorf("transactions params %v", p)
			}
		}
	}
	if !sawMeta || !sawPeriod {
		t.Fatalf("meta fetch for /me %v; transactions period %v", sawMeta, sawPeriod)
	}

	// The ledger: two Lloyds accounts slugged by type, every row, a reconciled joint account and its
	// mandates.
	for slug, ref := range map[string]string{"lloyds-joint": refJoint, "lloyds-current": refCurrent} {
		acc, err := h.core().GetAccountBySlug(context.Background(), slug)
		if err != nil || acc.BankRef == nil || *acc.BankRef != ref || acc.SourceKey != ref {
			t.Fatalf("%s: %+v %v", slug, acc, err)
		}
	}
	for i := 1; i <= 6; i++ {
		if _, ok := h.txn(fmt.Sprintf("norm-%06d", i)); !ok {
			t.Fatalf("transaction %d not in core", i)
		}
	}
	rec, err := h.core().GetReconciliation(context.Background(), ledger.AccountID(Source(ConnLloyds), refJoint))
	if err != nil || rec.LastCheckOK == nil || !*rec.LastCheckOK || rec.BreakCount != 0 {
		t.Fatalf("joint reconciliation %+v %v", rec, err)
	}
	var sos, dds int
	if err := h.st.DB().QueryRow(context.Background(), `SELECT (SELECT count(*) FROM core.standing_order WHERE account_id IS NOT NULL),
		(SELECT count(*) FROM core.direct_debit WHERE account_id IS NOT NULL)`).Scan(&sos, &dds); err != nil || sos != 1 || dds != 1 {
		t.Fatalf("attached mandates %d standing orders, %d direct debits, %v", sos, dds, err)
	}
}

// AC3: /me failing leaves the consent approved for 90 days.
func TestApprovalWithoutMeIsNinetyDays(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	h.fake.FailMe(ConnLloyds, 1)
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	c := h.consent(Source(ConnLloyds))
	if c.State != store.ConsentActive || !c.ExpectedExpiryAt.Equal(h.fake.Now().Add(consent.Lifetime)) {
		t.Fatalf("consent %s expiry %v, want 90 days from now", c.State, c.ExpectedExpiryAt)
	}
}

// The callback acts only on a live state issued for its own source: an unknown, reused or
// other-source state is refused before any code exchange, and nothing changes.
func TestCallbackRefusesUnknownReusedOrOtherSourceState(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	h.monzoFixture()
	lloyds := h.worker(ConnLloyds, nil)
	monzo := h.worker(ConnMonzo, nil)
	ctx := context.Background()

	if err := lloyds.HandleCallback(ctx, "made-up-state", "code"); !errors.Is(err, ErrCallbackRefused) {
		t.Fatalf("unknown state: %v", err)
	}
	link, err := monzo.AuthoriseURL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	code, state, err := h.fake.Authorise(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := lloyds.HandleCallback(ctx, state, code); !errors.Is(err, ErrCallbackRefused) {
		t.Fatalf("other source's state: %v", err)
	}
	if n := len(h.fake.Requests()); n != 0 {
		t.Fatalf("refused callbacks made %d requests", n)
	}
	h.connect(lloyds)
	link, _ = lloyds.AuthoriseURL(ctx)
	code, state, _ = h.fake.Authorise(link)
	if err := lloyds.HandleCallback(ctx, state, code); err != nil {
		t.Fatal(err)
	}
	lloyds.Wait()
	before := len(h.fake.Requests())
	if err := lloyds.HandleCallback(ctx, state, code); !errors.Is(err, ErrCallbackRefused) {
		t.Fatalf("reused state: %v", err)
	}
	if n := len(h.fake.Requests()) - before; n != 0 {
		t.Fatalf("a refused callback made %d requests", n)
	}
	if _, err := store.CurrentConsent(ctx, h.st.DB(), Source(ConnMonzo)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the other source has a consent: %v", err)
	}
}

// AC3: a failed history call ends the connect run partial with history_failed, counted by
// category.
func TestFailedHistoryEndsTheConnectRunPartial(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	h.fake.FailTransactions(ConnLloyds, 1)
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindConnect, outcomePartial, categoryHistoryFailed)
	if n := h.syncRuns(outcomePartial, categoryHistoryFailed); n != 1 {
		t.Fatalf("finstore.sync.runs{partial, history_failed} = %d, want 1", n)
	}
	if c := h.consent(Source(ConnLloyds)); c.State != store.ConsentActive {
		t.Fatalf("consent %s", c.State)
	}
}

// A reconnect whose bank returns a shorter history leaves the older stored rows alone.
func TestShorterHistoryOnReconnectLeavesOlderRowsLive(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	h.fake.Advance(24 * time.Hour)
	h.fake.SetHistoryStart(ConnLloyds, date(2025, 1, 1))
	h.connect(w)
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindConnect, outcomeOK, "")
	for _, key := range []string{"norm-000001", "norm-000002", "norm-000003"} {
		tx, ok := h.txn(key)
		if !ok || tx.DisappearedAt != nil {
			t.Fatalf("%s before the shorter history: present %v disappeared %v", key, ok, tx.DisappearedAt)
		}
	}
}

// AC4: a 429 is one attempt, ends the run partial with rate_limited and leaves the consent active.
func TestRateLimitedCallIsNotRetriedAndKeepsTheConsent(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	h.fake.Fail429(ConnLloyds, pathTransactions, 1)
	before := len(h.fake.Requests())
	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatalf("RunScheduled: %v", err)
	}
	var paths []string
	for _, r := range h.requests(before) {
		paths = append(paths, templateOf(r.Path))
	}
	if want := []string{pathMe, pathAccounts, pathStandingOrders, pathDirectDebits, pathTransactions}; !slices.Equal(paths, want) {
		t.Fatalf("calls %v, want %v: one transactions attempt and then nothing", paths, want)
	}
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindSync, outcomePartial, categoryRateLimited)
	if c := h.consent(Source(ConnLloyds)); c.State != store.ConsentActive {
		t.Fatalf("consent %s after a 429", c.State)
	}
	if n := h.syncRuns(outcomePartial, categoryRateLimited); n != 1 {
		t.Fatalf("finstore.sync.runs{partial, rate_limited} = %d, want 1", n)
	}
}

// AC4: before every call the worker counts the day's calls to that endpoint and bank reference;
// the fifth is never made and the run ends partial with rate_limited.
func TestFifthCallOfADayIsSkipped(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w) // /me once (the approval), everything else once
	for i := range 3 {
		if err := w.RunScheduled(context.Background()); err != nil {
			t.Fatalf("sync %d: %v", i+1, err)
		}
		requireRun(t, h.lastRun(Source(ConnLloyds)), KindSync, outcomeOK, "")
	}
	before := len(h.fake.Requests())
	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatalf("fifth: %v", err)
	}
	if n := len(h.fake.Requests()) - before; n != 0 {
		t.Fatalf("the day's fifth run made %d requests", n)
	}
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindSync, outcomePartial, categoryRateLimited)
	// The next day the count starts again.
	h.fake.Advance(24 * time.Hour)
	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindSync, outcomeOK, "")
}

// AC4: refreshes are serialised per connection with a five-minute margin, and each stores the
// refresh token returned.
func TestConcurrentCallsMakeOneRefreshAndStoreTheReturnedRefreshToken(t *testing.T) {
	h := newHarness(t, truelayertest.WithRotatingRefreshTokens())
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	ctx := context.Background()
	before, _ := store.GetOAuthToken(ctx, h.st.DB(), Source(ConnLloyds))

	h.fake.Advance(54 * time.Minute) // six minutes left: no refresh yet
	if _, err := w.tokens.access(ctx); err != nil || h.tokenCalls() != 1 {
		t.Fatalf("access with six minutes left: %v, %d token calls", err, h.tokenCalls())
	}
	h.fake.Advance(2 * time.Minute) // four minutes left: refresh
	// Every caller starts at once and the token call is slow, so unserialised refreshes would
	// overlap: the fake rotates the refresh token, so a second one would be refused.
	h.tokenDelay.Store(int64(100 * time.Millisecond))
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 8)
	var runErr error
	for i := range errs {
		wg.Go(func() { <-start; _, errs[i] = w.tokens.access(ctx) })
	}
	wg.Go(func() { <-start; runErr = w.RunScheduled(ctx) })
	close(start)
	wg.Wait()
	h.tokenDelay.Store(0)
	errs = append(errs, runErr)
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := h.tokenCalls(); n != 2 {
		t.Fatalf("%d token calls, want the exchange and one refresh", n)
	}
	after, _ := store.GetOAuthToken(ctx, h.st.DB(), Source(ConnLloyds))
	if after.RefreshToken == before.RefreshToken || after.AccessToken == before.AccessToken {
		t.Fatal("the refresh token TrueLayer returned was not stored")
	}
	// The stored token works for the next refresh, which the fake only accepts with the new one.
	h.fake.Advance(58 * time.Minute)
	if err := w.RunScheduled(ctx); err != nil {
		t.Fatalf("run after a second refresh: %v", err)
	}
	if c := h.consent(Source(ConnLloyds)); c.State != store.ConsentActive {
		t.Fatalf("consent %s", c.State)
	}
}

// AC4: a refused refresh ends the consent as refresh_rejected.
func TestRefusedRefreshEndsTheConsent(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	h.fake.RevokeConsent(ConnLloyds)
	h.fake.Advance(58 * time.Minute)
	if err := w.RunScheduled(context.Background()); !errors.Is(err, ErrConsentLost) {
		t.Fatalf("RunScheduled = %v, want ErrConsentLost", err)
	}
	c := h.consent(Source(ConnLloyds))
	if c.State != store.ConsentLost || *c.EndReason != string(consent.ReasonRefreshRejected) {
		t.Fatalf("consent %s %v, want lost refresh_rejected", c.State, c.EndReason)
	}
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindSync, outcomeFailed, categoryConsentLost)
}

// AC4: a data call refused with access_denied or 401 ends the consent as access_denied.
func TestRefusedDataCallEndsTheConsent(t *testing.T) {
	for name, refuse := range map[string]func(f *truelayertest.Fake){
		"access_denied": func(f *truelayertest.Fake) { f.ExpireConsent(ConnLloyds) },
		"401":           func(f *truelayertest.Fake) { f.RevokeConsent(ConnLloyds) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.lloydsFixture()
			w := h.worker(ConnLloyds, nil)
			h.connect(w)
			refuse(h.fake)
			before := len(h.fake.Requests())
			if err := w.RunScheduled(context.Background()); !errors.Is(err, ErrConsentLost) {
				t.Fatalf("RunScheduled = %v, want ErrConsentLost", err)
			}
			c := h.consent(Source(ConnLloyds))
			if c.State != store.ConsentLost || *c.EndReason != string(consent.ReasonAccessDenied) {
				t.Fatalf("consent %s %v, want lost access_denied", c.State, c.EndReason)
			}
			if n := len(h.fake.Requests()) - before; n != 1 {
				t.Fatalf("%d calls after the refusal, want the refused one only", n)
			}
			requireRun(t, h.lastRun(Source(ConnLloyds)), KindSync, outcomeFailed, categoryConsentLost)
		})
	}
}

// AC3 and AC5: Monzo through TrueLayer fetches only accounts and mandates, never for a pot, and its
// mandates attach to the Monzo accounts finstore already holds.
func TestMonzoMandatesSkipPotsAndAttachToMonzoAccounts(t *testing.T) {
	h := newHarness(t)
	h.monzoFixture()
	ids := h.addMonzoAccounts()
	w := h.worker(ConnMonzo, nil)
	h.connect(w)
	var paths []string
	for _, r := range h.fake.Requests()[1:] {
		paths = append(paths, templateOf(r.Path))
	}
	want := []string{pathMe, pathAccounts, pathStandingOrders, pathDirectDebits, pathStandingOrders, pathDirectDebits, pathCards, pathInfo}
	if !slices.Equal(paths, want) {
		t.Fatalf("calls %v, want %v", paths, want)
	}
	requireRun(t, h.lastRun(Source(ConnMonzo)), KindConnect, outcomeOK, "")
	rows, err := h.st.DB().Query(context.Background(), `SELECT bank_ref, account_id FROM core.standing_order
		UNION ALL SELECT bank_ref, account_id FROM core.direct_debit`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var ref string
		var acc *uuid.UUID
		if err := rows.Scan(&ref, &acc); err != nil {
			t.Fatal(err)
		}
		if acc == nil || ids[ref] != *acc || ref == refPot {
			t.Errorf("mandate on %s attached to %v", ref, acc)
		}
		n++
	}
	if n != 3 {
		t.Fatalf("%d mandates, want 3", n)
	}
	var created int
	if err := h.st.DB().QueryRow(context.Background(), `SELECT count(*) FROM core.account WHERE source = $1`, Source(ConnMonzo)).Scan(&created); err != nil || created != 0 {
		t.Fatalf("ob-monzo created %d accounts, %v", created, err)
	}
}

// AC5: a sync that changes only TrueLayer's transaction, direct debit and account ids creates no
// new version of anything.
func TestChangingTrueLayerIDsCreateNoNewVersions(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, func(o *Options) { o.SyncDays = 900 })
	h.connect(w)
	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindSync, outcomeOK, "")
	var txMax, soMax, ddMax, accounts int
	if err := h.st.DB().QueryRow(context.Background(), `SELECT
		(SELECT max(version_count) FROM core.transaction), (SELECT max(version_count) FROM core.standing_order),
		(SELECT max(version_count) FROM core.direct_debit), (SELECT count(*) FROM core.account)`).Scan(&txMax, &soMax, &ddMax, &accounts); err != nil {
		t.Fatal(err)
	}
	if txMax != 1 || soMax != 1 || ddMax != 1 || accounts != 2 {
		t.Fatalf("versions: transactions %d, standing orders %d, direct debits %d; accounts %d", txMax, soMax, ddMax, accounts)
	}
	// The ids did change between the two runs.
	var ids []string
	for r, err := range store.RawRowsAfter(context.Background(), h.st.DB(), 0) {
		if err != nil {
			t.Fatal(err)
		}
		if r.Kind == store.RawAPIResponse && r.APIResponse.Endpoint == pathDirectDebits && r.APIResponse.Status == 200 &&
			params(t, r.APIResponse.Params)["bank_ref"] == refJoint {
			ids = append(ids, params(t, r.APIResponse.Params)["account_id"].(string))
		}
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("account ids across the runs: %v", ids)
	}
}

// AC6: after a restart an unfinished connect run ends partial with history_failed, and a consent
// left awaiting approval is approved with /me.
func TestResumeEndsAnUnfinishedConnectAndApprovesAWaitingConsent(t *testing.T) {
	h := newHarness(t, truelayertest.WithConsentLifetime(80*24*time.Hour))
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	ctx := context.Background()
	// The state a process killed right after HandleCallback returned leaves: the exchange and the
	// connect run it records, and nothing after (the background work died with the process).
	link, _ := w.AuthoriseURL(ctx)
	code, state, _ := h.fake.Authorise(link)
	att, err := w.consents.ConsumeAttempt(ctx, h.st.DB(), state)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.tokens.exchange(ctx, code, att.ID); err != nil {
		t.Fatal(err)
	}
	runID, err := store.StartRun(ctx, h.st.DB(), Source(ConnLloyds), KindConnect, h.fake.Now())
	if err != nil {
		t.Fatal(err)
	}

	restarted := h.worker(ConnLloyds, nil)
	if err := restarted.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	restarted.Wait()
	r, err := store.GetRun(ctx, h.st.DB(), runID)
	if err != nil {
		t.Fatal(err)
	}
	requireRun(t, r, KindConnect, outcomePartial, categoryHistoryFailed)
	c := h.consent(Source(ConnLloyds))
	if c.State != store.ConsentActive || !c.ExpectedExpiryAt.Equal(h.fake.ConsentEnd(ConnLloyds)) {
		t.Fatalf("consent %s expiry %v, want active until /me's end", c.State, c.ExpectedExpiryAt)
	}
}

// AC6: a consent with no stored token (a database restore) ends lost with token_missing.
func TestResumeEndsAConsentWithNoStoredToken(t *testing.T) {
	h := newHarness(t)
	w := h.worker(ConnLloyds, nil)
	ctx := context.Background()
	state, err := w.consents.StartAttempt(ctx, h.st.DB())
	if err != nil {
		t.Fatal(err)
	}
	att, err := w.consents.ConsumeAttempt(ctx, h.st.DB(), state)
	if err != nil {
		t.Fatal(err)
	}
	cons, err := w.consents.Exchanged(ctx, h.st.DB(), att.ID, h.fake.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.consents.Approved(ctx, h.st.DB(), cons.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.Resume(ctx); !errors.Is(err, ErrConsentLost) {
		t.Fatalf("Resume = %v, want ErrConsentLost", err)
	}
	w.Wait()
	c := h.consent(Source(ConnLloyds))
	if c.State != store.ConsentLost || *c.EndReason != string(consent.ReasonTokenMissing) {
		t.Fatalf("consent %s %v, want lost token_missing", c.State, c.EndReason)
	}
	if n := len(h.fake.Requests()); n != 0 {
		t.Fatalf("%d requests with no token", n)
	}
}

// A run has a deadline: one that passes ends the run failed and records it.
func TestRunDeadline(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	short := h.worker(ConnLloyds, func(o *Options) { o.RunTimeout = time.Nanosecond })
	if err := short.RunScheduled(context.Background()); err == nil {
		t.Fatal("a run past its deadline reported no error")
	}
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindSync, outcomeFailed, "cancelled")
}

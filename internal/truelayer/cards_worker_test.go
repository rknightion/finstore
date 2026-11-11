package truelayer

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"

	"github.com/rknightion/finstore/internal/consent"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/truelayertest"
)

// cardFixture is the Lloyds fixture plus two credit cards: one ending 5678 with a purchase and a
// repayment, one ending 9012 with a purchase the bank writes with a negative amount.
func (h *harness) cardFixture() (card, other *truelayertest.FakeCard) {
	h.lloydsFixture()
	f := h.fake
	card = f.AddCard(ConnLloyds, truelayertest.Card{PartialNumber: "5678", DisplayName: "MADE UP CARD"})
	f.SetCardBalance(card, 12345, 137655)
	f.AddCardTransaction(card, truelayertest.CardTxn{Type: "DEBIT", Amount: "24.25", Date: date(2026, 11, 2), Description: "MADE UP SHOP"})
	f.AddCardTransaction(card, truelayertest.CardTxn{Type: "CREDIT", Amount: "50.00", Date: date(2026, 11, 5), Description: "MADE UP REPAYMENT"})
	other = f.AddCard(ConnLloyds, truelayertest.Card{PartialNumber: "9012", DisplayName: "OTHER MADE UP CARD"})
	f.SetCardBalance(other, 0, 150000)
	f.AddCardTransaction(other, truelayertest.CardTxn{Type: "DEBIT", Amount: "-3.00", Date: date(2026, 11, 3), Description: "MADE UP CAFE"})
	return card, other
}

func (h *harness) amounts(account store.Account) []int64 {
	h.t.Helper()
	rows, err := h.st.DB().Query(context.Background(), `SELECT amount_minor FROM core.transaction WHERE account_id = $1 ORDER BY txn_date, amount_minor`, account.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// A Lloyds card is a credit_card ledger account keyed card:<last4>, slugged lloyds-card-<last4>,
// with no bank reference, balances_only reconciliation and the card object as its detail. Its
// purchases are negative, its repayments positive, and its balance minus what is owed.
func TestLloydsCardsAreLedgerAccounts(t *testing.T) {
	h := newHarness(t)
	h.cardFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindConnect, outcomeOK, "")

	ctx := context.Background()
	acc, err := h.core().GetAccountBySlug(ctx, "lloyds-card-5678")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Type != "credit_card" || acc.SourceKey != "card:5678" || acc.Source != Source(ConnLloyds) || acc.BankRef != nil ||
		acc.Name != "MADE UP CARD" || acc.Currency != "GBP" || acc.Feed != "api" {
		t.Fatalf("card account = %+v", acc)
	}
	var detail map[string]any
	if err := json.Unmarshal(acc.Detail, &detail); err != nil || detail["partial_card_number"] != "5678" || detail["display_name"] != "MADE UP CARD" ||
		detail["card_type"] != "CREDIT" {
		t.Fatalf("detail = %s, %v; want the card object", acc.Detail, err)
	}
	if rec, err := h.core().GetReconciliation(ctx, acc.ID); err != nil || rec.Method != "balances_only" {
		t.Fatalf("reconciliation %+v, %v", rec, err)
	}
	if got := h.amounts(acc); !slices.Equal(got, []int64{-2425, 5000}) {
		t.Errorf("card amounts = %v, want [-2425 5000]", got)
	}
	if bal, err := h.core().LatestBalanceSnapshot(ctx, acc.ID); err != nil || bal.BalanceMinor != -12345 {
		t.Errorf("card balance = %+v, %v; want -12345", bal, err)
	}
	other, err := h.core().GetAccountBySlug(ctx, "lloyds-card-9012")
	if err != nil {
		t.Fatal(err)
	}
	if got := h.amounts(other); !slices.Equal(got, []int64{-300}) {
		t.Errorf("a debit written with a negative amount: %v, want [-300]", got)
	}
	// The Lloyds current accounts are unaffected and the cards never share a bank reference.
	if a, err := h.core().GetAccountBySlug(ctx, "lloyds-joint"); err != nil || a.BankRef == nil || *a.BankRef != refJoint {
		t.Errorf("joint account %+v, %v", a, err)
	}
	// The agent view shows the card as an account.
	var typ string
	var n int
	if err := h.st.DB().QueryRow(ctx, `SELECT type, (SELECT count(*) FROM agent.accounts) FROM agent.accounts WHERE slug = 'lloyds-card-5678'`).Scan(&typ, &n); err != nil || typ != "credit_card" || n != 4 {
		t.Errorf("agent.accounts: type %q, %d rows, %v", typ, n, err)
	}
}

// A card's history is one fetch from CardHistoryFrom on a connect and SyncDays back on a scheduled
// run, and completing it records the period in core.
func TestCardHistoryFetchCompletesItsPeriod(t *testing.T) {
	h := newHarness(t)
	h.cardFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	for _, r := range h.fake.Requests() {
		if templateOf(r.Path) == pathCardTransactions && (r.Query.Get("from") != "2021-01-01" || r.Query.Get("to") != "2026-11-11") {
			t.Errorf("card history on connect asked %v, want 2021-01-01 to 2026-11-11", r.Query)
		}
	}
	acc, err := h.core().GetAccountBySlug(context.Background(), "lloyds-card-5678")
	if err != nil {
		t.Fatal(err)
	}
	var start, end time.Time
	var n int
	if err := h.st.DB().QueryRow(context.Background(), `SELECT count(*), min(period_start), min(period_end) FROM core.fetch_period WHERE account_id = $1`, acc.ID).Scan(&n, &start, &end); err != nil || n != 1 {
		t.Fatalf("fetch periods of the card: %d, %v", n, err)
	}
	// TrueLayer may hold a shorter history than was asked for, so a completed fetch proves its period
	// only from the earliest row it returned (2026-11-02), up to the end of today in London.
	if !start.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("period %v to %v, want the earliest row (2026-11-02) to the end of 2026-11-11 in London", start, end)
	}
	// A purchase the bank no longer lists inside a completed period disappears, as for an account.
	before := len(h.fake.Requests())
	h.fake.Advance(24 * time.Hour)
	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, r := range h.requests(before) {
		if templateOf(r.Path) == pathCardTransactions && r.Query.Get("from") != "2026-10-08" {
			t.Errorf("scheduled card history asked %v, want from 35 days back (2026-10-08)", r.Query)
		}
	}
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindSync, outcomeOK, "")
}

// Three runs in one day across two cards stay within each card's daily limit: each card is counted
// alone, by its card key, not under one shared counter.
func TestThreeRunsAcrossTwoCardsStayWithinEachCardsLimit(t *testing.T) {
	h := newHarness(t)
	h.cardFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	for range 2 {
		if err := w.RunScheduled(context.Background()); err != nil {
			t.Fatal(err)
		}
		requireRun(t, h.lastRun(Source(ConnLloyds)), KindSync, outcomeOK, "")
	}
	if n := h.fake.Count(pathCardBalance); n != 6 {
		t.Fatalf("%d card balance calls, want 3 runs x 2 cards", n)
	}
	var refused int
	if err := h.st.DB().QueryRow(context.Background(), `SELECT count(*) FROM raw.api_response WHERE status = 429`).Scan(&refused); err != nil || refused != 0 {
		t.Fatalf("%d calls were answered 429, %v", refused, err)
	}
	for _, key := range []string{"card:5678", "card:9012"} {
		n, err := store.CountCallsToday(context.Background(), h.st.DB(), Source(ConnLloyds), pathCardBalance, key, h.fake.Now().In(h.loc))
		if err != nil || n != 3 {
			t.Errorf("%s: %d balance calls today, want 3 (%v)", key, n, err)
		}
	}
}

// On a Lloyds connect the card list counts towards the history check: a /cards that fails ends the
// run partial with history_failed, because the card history is lost until the next reconnect.
func TestFailedCardListOnConnectEndsTheRunHistoryFailed(t *testing.T) {
	h := newHarness(t)
	h.cardFixture()
	h.fake.Fail(ConnLloyds, pathCards, http.StatusInternalServerError, 1)
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindConnect, outcomePartial, categoryHistoryFailed)
	if c := h.consent(Source(ConnLloyds)); c.State != store.ConsentActive {
		t.Fatalf("consent %s", c.State)
	}
	if n := h.fake.Count(pathCardTransactions); n != 0 {
		t.Errorf("%d card history calls after a failed card list", n)
	}
	// The account history was unaffected.
	if _, err := h.core().GetAccountBySlug(context.Background(), "lloyds-joint"); err != nil {
		t.Error(err)
	}
	// On a scheduled run a failed /cards is only a partial run.
	h.fake.Advance(24 * time.Hour)
	h.fake.Fail(ConnLloyds, pathCards, http.StatusInternalServerError, 1)
	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindSync, outcomePartial, "server_error")
}

// cardCalls counts the card calls among the requests made since index from.
func (h *harness) cardCalls(from int) map[string]int {
	out := map[string]int{}
	for _, r := range h.requests(from) {
		switch t := templateOf(r.Path); t {
		case pathCards, pathCardTransactions, pathCardBalance, pathCardPending:
			out[t]++
		}
	}
	return out
}

// A refusal (401, 403 or access_denied) on any card path never ends the consent: the run ends
// partial with cards_refused, makes no further card call (not even a pending read), and every
// account read of the run is unaffected. On a connect the missing card history does not make the
// run history_failed either.
func TestRefusedCardReadsKeepTheConsentAndStopTheCardCalls(t *testing.T) {
	type refusal struct {
		status int
		code   string
	}
	forbidden := refusal{http.StatusForbidden, "access_denied"}
	for name, c := range map[string]struct {
		tmpl    string
		refusal refusal
		want    map[string]int // card calls made in the run
	}{
		"list access_denied":    {pathCards, forbidden, map[string]int{pathCards: 1}},
		"list 401":              {pathCards, refusal{http.StatusUnauthorized, ""}, map[string]int{pathCards: 1}},
		"list plain 403":        {pathCards, refusal{http.StatusForbidden, ""}, map[string]int{pathCards: 1}},
		"card transactions":     {pathCardTransactions, forbidden, map[string]int{pathCards: 1, pathCardTransactions: 1}},
		"card balance 401":      {pathCardBalance, refusal{http.StatusUnauthorized, ""}, map[string]int{pathCards: 1, pathCardTransactions: 1, pathCardBalance: 1}},
		"card balance 403":      {pathCardBalance, refusal{http.StatusForbidden, ""}, map[string]int{pathCards: 1, pathCardTransactions: 1, pathCardBalance: 1}},
		"card transactions 401": {pathCardTransactions, refusal{http.StatusUnauthorized, ""}, map[string]int{pathCards: 1, pathCardTransactions: 1}},
	} {
		for _, kind := range []string{KindConnect, KindSync} {
			t.Run(name+" "+kind, func(t *testing.T) {
				h := newHarness(t)
				h.cardFixture()
				w := h.worker(ConnLloyds, nil)
				var before int
				if kind == KindConnect {
					h.fake.FailWith(ConnLloyds, c.tmpl, c.refusal.status, c.refusal.code, 1)
					h.connect(w)
				} else {
					h.connect(w)
					h.fake.Advance(24 * time.Hour)
					h.fake.FailWith(ConnLloyds, c.tmpl, c.refusal.status, c.refusal.code, 1)
					before = len(h.fake.Requests())
					if err := w.RunScheduled(context.Background()); err != nil {
						t.Fatalf("RunScheduled: %v", err)
					}
				}
				requireRun(t, h.lastRun(Source(ConnLloyds)), kind, outcomePartial, categoryCardsRefused)
				if cs := h.consent(Source(ConnLloyds)); cs.State != store.ConsentActive {
					t.Fatalf("consent %s %v after a refused card read", cs.State, cs.EndReason)
				}
				if got := h.cardCalls(before); !maps.Equal(got, c.want) {
					t.Errorf("card calls %v, want %v", got, c.want)
				}
				// The account reads of the run are all there: two accounts, their history, balance and
				// pending reads.
				reqs := h.requests(before)
				for tmpl, want := range map[string]int{pathTransactions: 2, pathBalance: 2, pathPending: 2} {
					n := 0
					for _, r := range reqs {
						if templateOf(r.Path) == tmpl {
							n++
						}
					}
					if n != want {
						t.Errorf("%s called %d times in the run, want %d", tmpl, n, want)
					}
				}
				if got := h.logs.messages(slog.LevelWarn); !slices.Contains(got, "truelayer card read refused") {
					t.Errorf("warnings %v lack the refused line", got)
				}
			})
		}
	}
}

// A refused token refresh on the way to a card call is no bank answer to that call: it still ends
// the consent.
func TestRefusedRefreshDuringACardCallStillEndsTheConsent(t *testing.T) {
	h := newHarness(t)
	h.cardFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	h.fake.Advance(24 * time.Hour)
	// Right after /cards the bank revokes the consent and the token runs out: the card's
	// transactions call needs a refresh, which is refused.
	hook := func(rw http.ResponseWriter, r *http.Request) bool {
		h.fake.ServeHTTP(rw, r)
		if templateOf(r.URL.Path) == pathCards {
			h.fake.RevokeConsent(ConnLloyds)
			h.fake.Advance(58 * time.Minute)
		}
		return true
	}
	h.onRequest.Store(&hook)
	if err := w.RunScheduled(context.Background()); !errors.Is(err, ErrConsentLost) {
		t.Fatalf("RunScheduled = %v, want ErrConsentLost", err)
	}
	if c := h.consent(Source(ConnLloyds)); c.State != store.ConsentLost || *c.EndReason != string(consent.ReasonRefreshRejected) {
		t.Fatalf("consent %s %v, want lost refresh_rejected", c.State, c.EndReason)
	}
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindSync, outcomeFailed, categoryConsentLost)
}

// On a connect a card's own transactions call that fails makes the run history_failed, because that
// card's history is lost until the next reconnect (each listed card counts until its history
// arrives, not only the card list).
func TestFailedCardHistoryOnConnectEndsTheRunHistoryFailed(t *testing.T) {
	h := newHarness(t)
	h.cardFixture()
	h.fake.Fail(ConnLloyds, pathCardTransactions, http.StatusInternalServerError, 1)
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindConnect, outcomePartial, categoryHistoryFailed)
	if c := h.consent(Source(ConnLloyds)); c.State != store.ConsentActive {
		t.Fatalf("consent %s", c.State)
	}
	// The other card's history arrived.
	if n := h.fake.Count(pathCardTransactions); n != 2 {
		t.Errorf("%d card history calls, want one per card", n)
	}
}

// A card whose partial number does not end in four digits is skipped with a warning and the run is
// still ok. Two cards that share their last four are both skipped, the run is partial, and neither
// becomes an account; a card in the same list that is fine is kept.
func TestCardsWithoutUsableDigitsOrWithASharedLastFourAreSkipped(t *testing.T) {
	h := newHarness(t)
	h.lloydsFixture()
	good := h.fake.AddCard(ConnLloyds, truelayertest.Card{PartialNumber: "9012", DisplayName: "GOOD"})
	h.fake.AddCardTransaction(good, truelayertest.CardTxn{Type: "DEBIT", Amount: "1.00", Date: date(2026, 11, 2), Description: "X"})
	h.fake.AddCard(ConnLloyds, truelayertest.Card{PartialNumber: "XXXX", DisplayName: "NO DIGITS"})
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindConnect, outcomeOK, "")
	if got := h.logs.messages(slog.LevelWarn); !slices.Contains(got, "truelayer cards skipped: no usable last four digits") {
		t.Errorf("warnings %v lack the no-digits line", got)
	}
	var n int
	if err := h.st.DB().QueryRow(context.Background(), `SELECT count(*) FROM core.account WHERE type = 'credit_card'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d card accounts, want only the good card (%v)", n, err)
	}
	if got := h.fake.Count(pathCardBalance); got != 1 {
		t.Errorf("%d card balance calls, want one for the good card", got)
	}

	h2 := newHarness(t)
	h2.lloydsFixture()
	a := h2.fake.AddCard(ConnLloyds, truelayertest.Card{PartialNumber: "1111 5678", DisplayName: "FIRST"})
	b := h2.fake.AddCard(ConnLloyds, truelayertest.Card{PartialNumber: "2222 5678", DisplayName: "SECOND"})
	for _, c := range []*truelayertest.FakeCard{a, b} {
		h2.fake.AddCardTransaction(c, truelayertest.CardTxn{Type: "DEBIT", Amount: "1.00", Date: date(2026, 11, 2), Description: "X"})
	}
	ok := h2.fake.AddCard(ConnLloyds, truelayertest.Card{PartialNumber: "9012", DisplayName: "FINE"})
	h2.fake.AddCardTransaction(ok, truelayertest.CardTxn{Type: "DEBIT", Amount: "1.00", Date: date(2026, 11, 2), Description: "X"})
	w2 := h2.worker(ConnLloyds, nil)
	h2.connect(w2)
	requireRun(t, h2.lastRun(Source(ConnLloyds)), KindConnect, outcomePartial, categoryCardKeyShared)
	if got := h2.logs.messages(slog.LevelWarn); !slices.Contains(got, "truelayer cards skipped: last four digits shared") {
		t.Errorf("warnings %v lack the shared line", got)
	}
	rows, err := h2.st.DB().Query(context.Background(), `SELECT source_key FROM core.account WHERE type = 'credit_card'`)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := pgxStrings(rows)
	if err != nil || !slices.Equal(keys, []string{"card:9012"}) {
		t.Fatalf("card accounts %v (%v), want only card:9012", keys, err)
	}
	if got := h2.fake.Count(pathCardTransactions); got != 1 {
		t.Errorf("%d card history calls, want one for the fine card", got)
	}
}

// A card account the bank stops listing is warned of and counted by finstore.truelayer.cards_unlisted;
// a card skipped by the skip rules counts as listed, so neither a shared last four nor a card with
// no digits triggers it.
func TestACardMissingFromASuccessfulListingIsWarnedOf(t *testing.T) {
	h := newHarness(t)
	card, _ := h.cardFixture()
	h.fake.AddCard(ConnLloyds, truelayertest.Card{PartialNumber: "XXXX", DisplayName: "NO DIGITS"})
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	src := attribute.String(semconv.AttrSource, Source(ConnLloyds))
	if v, ok := h.gauge(semconv.MetricTrueLayerCardsUnlisted, src); !ok || v != 0 {
		t.Fatalf("cards_unlisted after the connect = %d (set %v), want 0", v, ok)
	}
	const warn = "card accounts not in the newest card list"
	if slices.Contains(h.logs.messages(slog.LevelWarn), warn) {
		t.Fatal("a warning before any card went missing")
	}

	h.fake.RemoveCard(ConnLloyds, card)
	h.fake.Advance(24 * time.Hour)
	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindSync, outcomeOK, "")
	n := 0
	for _, m := range h.logs.messages(slog.LevelWarn) {
		if m == warn {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d warnings for the card that went missing, want 1", n)
	}
	if v, ok := h.gauge(semconv.MetricTrueLayerCardsUnlisted, src); !ok || v != 1 {
		t.Errorf("cards_unlisted = %d (set %v), want 1", v, ok)
	}
	// The held account stays open with its last balance until an operator ends it.
	if a, err := h.core().GetAccountBySlug(context.Background(), "lloyds-card-5678"); err != nil || a.EndedOn != nil {
		t.Errorf("the missing card's account: %+v, %v", a, err)
	}

	// A held card whose last four are now shared by two listed cards is skipped, not missing: the
	// only warning of this run is for the card that is still gone.
	h.fake.AddCard(ConnLloyds, truelayertest.Card{PartialNumber: "1111 9012", DisplayName: "CLASH"})
	h.fake.Advance(time.Hour)
	before := h.counterWarn(warn)
	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindSync, outcomePartial, categoryCardKeyShared)
	if got := h.counterWarn(warn) - before; got != 1 {
		t.Errorf("%d warnings in the run with the shared last four, want 1 (the card still gone)", got)
	}
	for _, line := range h.logs.attrs(warn) {
		if strings.Contains(line, "lloyds-card") || strings.Contains(line, "9012") || strings.Contains(line, "5678") {
			t.Errorf("the warning carries card identity: %s", line)
		}
	}
	if v, _ := h.gauge(semconv.MetricTrueLayerCardsUnlisted, src); v != 1 {
		t.Errorf("cards_unlisted = %d, want the 1 missing card only", v)
	}
}

func (h *harness) counterWarn(msg string) int {
	n := 0
	for _, m := range h.logs.messages(slog.LevelWarn) {
		if m == msg {
			n++
		}
	}
	return n
}

// The calls of a Lloyds run with one account and one card: ledger reads first (the account, then
// the card list, the card's transactions and balance), optional reads after.
func TestLloydsRunCallSequenceWithOneAccountAndOneCard(t *testing.T) {
	h := newHarness(t)
	f := h.fake
	f.AddAccount(ConnLloyds, truelayertest.Account{ID: "acc-joint", Type: "TRANSACTION", DisplayName: "Joint", SortCode: "11-22-33", Number: "12345678", Opening: 50000})
	f.AddTransaction(ConnLloyds, "acc-joint", truelayertest.Transaction{Date: date(2026, 11, 2), Amount: -999, Description: "MADE UP CAFE"})
	card := f.AddCard(ConnLloyds, truelayertest.Card{PartialNumber: "5678", DisplayName: "MADE UP CARD"})
	f.AddCardTransaction(card, truelayertest.CardTxn{Type: "DEBIT", Amount: "1.00", Date: date(2026, 11, 2), Description: "X"})
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	h.fake.Advance(24 * time.Hour)
	before := len(h.fake.Requests())
	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range h.requests(before) {
		if r.Method == http.MethodGet {
			paths = append(paths, templateOf(r.Path))
		}
	}
	want := []string{pathMe, pathAccounts, pathStandingOrders, pathDirectDebits, pathTransactions, pathBalance,
		pathCards, pathCardTransactions, pathCardBalance, pathPending, pathCardPending, pathInfo}
	if !slices.Equal(paths, want) {
		t.Fatalf("calls %v\nwant  %v", paths, want)
	}
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindSync, outcomeOK, "")
}

// The Monzo connection calls /cards once, as an optional read stored raw under the run's list fetch,
// and makes no per-card call and no account.
func TestMonzoConnectionListsCardsOnly(t *testing.T) {
	h := newHarness(t)
	h.monzoFixture()
	flex := h.fake.AddCard(ConnMonzo, truelayertest.Card{PartialNumber: "3456", DisplayName: "MADE UP FLEX"})
	h.fake.SetCardBalance(flex, 5000, 100000)
	h.fake.AddCardTransaction(flex, truelayertest.CardTxn{Type: "DEBIT", Amount: "1.00", Date: date(2026, 11, 2), Description: "X"})
	w := h.worker(ConnMonzo, nil)
	h.connect(w)
	requireRun(t, h.lastRun(Source(ConnMonzo)), KindConnect, outcomeOK, "")
	if n := h.fake.Count(pathCards); n != 1 {
		t.Fatalf("/cards called %d times, want once", n)
	}
	for _, tmpl := range []string{pathPending, pathCardBalance, pathCardTransactions, pathCardPending} {
		if n := h.fake.Count(tmpl); n != 0 {
			t.Errorf("%s called %d times on the Monzo connection", tmpl, n)
		}
	}
	if n := h.optional(pathCards, "ok"); n != 1 {
		t.Errorf("optional_reads{cards, ok} = %d, want 1", n)
	}
	var accounts, stored int
	if err := h.st.DB().QueryRow(context.Background(), `SELECT (SELECT count(*) FROM core.account WHERE source = $1),
		(SELECT count(*) FROM raw.api_response r JOIN raw.fetch f ON f.id = r.fetch_id
		  WHERE r.endpoint = $2 AND r.status = 200 AND f.account_key IS NULL AND position('MADE UP FLEX' in convert_from(r.body, 'UTF8')) > 0)`,
		Source(ConnMonzo), pathCards).Scan(&accounts, &stored); err != nil || accounts != 0 || stored != 1 {
		t.Fatalf("ob-monzo accounts %d, raw /cards bodies under a list fetch %d, %v", accounts, stored, err)
	}
	// The call is last but for /info.
	reqs := h.fake.Requests()
	if got := templateOf(reqs[len(reqs)-2].Path); got != pathCards {
		t.Errorf("second to last call %s, want /cards", got)
	}

	// A refusal of the optional /cards does not end the consent or touch the run's outcome.
	h.fake.Advance(24 * time.Hour)
	h.fake.FailWith(ConnMonzo, pathCards, http.StatusForbidden, "access_denied", 1)
	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireRun(t, h.lastRun(Source(ConnMonzo)), KindSync, outcomeOK, "")
	if c := h.consent(Source(ConnMonzo)); c.State != store.ConsentActive {
		t.Fatalf("consent %s after a refused optional /cards", c.State)
	}
	if n := h.optional(pathCards, "refused"); n != 1 {
		t.Errorf("optional_reads{cards, refused} = %d, want 1", n)
	}
}

// A consent whose /me lacks cards makes no card call, on either connection.
func TestNoCardCallWhenMeLacksTheCardsScope(t *testing.T) {
	h := newHarness(t)
	h.cardFixture()
	h.monzoFixture()
	h.fake.AddCard(ConnMonzo, truelayertest.Card{PartialNumber: "3456", DisplayName: "MADE UP FLEX"})
	h.fake.SetScopes(ConnLloyds, scopesWithout("cards")...)
	h.fake.SetScopes(ConnMonzo, scopesWithout("cards")...)
	for _, conn := range []string{ConnLloyds, ConnMonzo} {
		w := h.worker(conn, nil)
		h.connect(w)
		requireRun(t, h.lastRun(Source(conn)), KindConnect, outcomeOK, "")
		h.fake.Advance(24 * time.Hour)
		if err := w.RunScheduled(context.Background()); err != nil {
			t.Fatal(err)
		}
		requireRun(t, h.lastRun(Source(conn)), KindSync, outcomeOK, "")
	}
	for _, tmpl := range []string{pathCards, pathCardBalance, pathCardTransactions, pathCardPending} {
		if n := h.fake.Count(tmpl); n != 0 {
			t.Errorf("%s called %d times without the cards scope", tmpl, n)
		}
	}
	var n int
	if err := h.st.DB().QueryRow(context.Background(), `SELECT count(*) FROM core.account WHERE type = 'credit_card'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d card accounts without the scope (%v)", n, err)
	}
	// An unreadable /me grants nothing either.
	h.fake.SetScopes(ConnLloyds, scopesWithout()...)
	h.fake.Advance(24 * time.Hour)
	h.fake.FailMe(ConnLloyds, 1)
	w := h.worker(ConnLloyds, nil)
	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := h.fake.Count(pathCards); n != 0 {
		t.Errorf("/cards called %d times after a failed /me", n)
	}
}

func pgxStrings(rows pgx.Rows) ([]string, error) {
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	slices.Sort(out)
	return out, err
}

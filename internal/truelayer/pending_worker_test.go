package truelayer

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/truelayertest"
)

// pendingFixture is the card fixture with one pending row on the joint account and one on the first
// card.
func (h *harness) pendingFixture() *truelayertest.FakeCard {
	card, _ := h.cardFixture()
	h.fake.AddPending(ConnLloyds, "acc-joint", truelayertest.PendingTxn{Date: date(2026, 11, 10), Amount: "-7.50", Description: "MADE UP PENDING", MerchantName: "Made Up"})
	h.fake.AddPending(ConnLloyds, card.ID, truelayertest.PendingTxn{Date: date(2026, 11, 10), Type: "DEBIT", Amount: "5.00", Description: "CARD PENDING"})
	return card
}

type pendingRowGot struct {
	slug string
	id   string
	amt  int64
}

func (h *harness) pendingRows() []pendingRowGot {
	h.t.Helper()
	rows, err := h.st.DB().Query(context.Background(), `SELECT account_slug, transaction_id, amount_minor FROM agent.pending_transactions ORDER BY account_slug, transaction_id`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []pendingRowGot
	for rows.Next() {
		var r pendingRowGot
		if err := rows.Scan(&r.slug, &r.id, &r.amt); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (h *harness) pendingAsOf() map[string]*time.Time {
	h.t.Helper()
	rows, err := h.st.DB().Query(context.Background(), `SELECT slug, pending_as_of FROM agent.accounts`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]*time.Time{}
	for rows.Next() {
		var slug string
		var at *time.Time
		if err := rows.Scan(&slug, &at); err != nil {
			h.t.Fatal(err)
		}
		out[slug] = at
	}
	return out
}

// After a connect every Lloyds account and card has a pending snapshot: the rows as the bank gave
// them for an account, signed from transaction_type for a card, and an empty snapshot (nothing
// pending) still sets pending_as_of. The Monzo accounts have none.
func TestPendingTransactionsReachTheAgentViews(t *testing.T) {
	h := newHarness(t)
	h.pendingFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindConnect, outcomeOK, "")

	rows := h.pendingRows()
	want := []pendingRowGot{{"lloyds-card-5678", rows[0].id, -500}, {"lloyds-joint", rows[1].id, -750}}
	if len(rows) != 2 || rows[0] != want[0] || rows[1] != want[1] {
		t.Fatalf("agent.pending_transactions = %+v, want %+v", rows, want)
	}
	asOf := h.pendingAsOf()
	for _, slug := range []string{"lloyds-joint", "lloyds-current", "lloyds-card-5678", "lloyds-card-9012"} {
		if asOf[slug] == nil || !asOf[slug].Equal(h.fake.Now()) {
			t.Errorf("pending_as_of of %s = %v, want %v (an empty snapshot counts)", slug, asOf[slug], h.fake.Now())
		}
	}
	// The bank's own ids change on every call; the pending rows replace each other as a whole, so
	// the next day's run leaves the same rows, and a row that settled is gone.
	h.fake.Advance(24 * time.Hour)
	if err := w.RunScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.pendingRows(); len(got) != 2 {
		t.Fatalf("after the next run: %+v", got)
	}
	// Pending rows are never ledger rows: no core.transaction row carries their description.
	var n int
	if err := h.st.DB().QueryRow(context.Background(), `SELECT count(*) FROM core.transaction WHERE description IN ('MADE UP PENDING', 'CARD PENDING')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d pending rows in core.transaction (%v)", n, err)
	}
	// Both pending reads are stored raw under the account's or card's own fetch, keyed like its
	// other calls.
	var stored int
	if err := h.st.DB().QueryRow(context.Background(), `SELECT count(*) FROM raw.api_response r JOIN raw.fetch f ON f.id = r.fetch_id
		WHERE r.endpoint IN ($1, $2) AND r.status = 200 AND f.account_key = r.params->>'bank_ref'`, pathPending, pathCardPending).Scan(&stored); err != nil || stored != 8 {
		t.Fatalf("%d pending answers stored under their account's fetch, want 8 (%v)", stored, err)
	}
	if n := h.optional(pathPending, "ok"); n != 4 {
		t.Errorf("optional_reads{pending, ok} = %d, want 4 (two accounts, two runs)", n)
	}
	if n := h.optional(pathCardPending, "ok"); n != 4 {
		t.Errorf("optional_reads{card pending, ok} = %d, want 4 (two cards, two runs)", n)
	}
}

// A failing or refused pending read never changes a run's outcome or the consent, and on a connect it
// is not part of the history check.
func TestFailingPendingReadsLeaveTheRunAndTheHistoryCheckAlone(t *testing.T) {
	h := newHarness(t)
	h.pendingFixture()
	h.fake.FailWith(ConnLloyds, pathPending, http.StatusForbidden, "access_denied", 2)
	h.fake.Fail(ConnLloyds, pathCardPending, http.StatusInternalServerError, 2)
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindConnect, outcomeOK, "")
	if c := h.consent(Source(ConnLloyds)); c.State != store.ConsentActive {
		t.Fatalf("consent %s", c.State)
	}
	if n := h.optional(pathPending, "refused"); n != 2 {
		t.Errorf("refused account pending reads = %d, want 2", n)
	}
	if n := h.optional(pathCardPending, "failed"); n != 2 {
		t.Errorf("failed card pending reads = %d, want 2", n)
	}
	if got := h.pendingRows(); len(got) != 0 {
		t.Errorf("pending rows after failed reads: %+v", got)
	}
	for slug, at := range h.pendingAsOf() {
		if at != nil {
			t.Errorf("pending_as_of of %s is set after a failed read", slug)
		}
	}
}

// A 429 on the first pending read ends the run's optional reads there: no other pending read and no
// /info is made, the run stays ok and every ledger read of the run is intact.
func TestRateLimitOnThePendingReadStopsTheRemainingOptionalReads(t *testing.T) {
	h := newHarness(t)
	h.pendingFixture()
	w := h.worker(ConnLloyds, nil)
	h.connect(w)
	h.fake.Advance(24 * time.Hour)
	h.fake.Fail429(ConnLloyds, pathPending, 1)
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
	want := []string{pathMe, pathAccounts,
		pathStandingOrders, pathDirectDebits, pathTransactions, pathBalance,
		pathStandingOrders, pathDirectDebits, pathTransactions, pathBalance,
		pathCards, pathCardTransactions, pathCardBalance, pathCardTransactions, pathCardBalance,
		pathPending}
	if !slices.Equal(paths, want) {
		t.Fatalf("calls %v\nwant  %v", paths, want)
	}
	requireRun(t, h.lastRun(Source(ConnLloyds)), KindSync, outcomeOK, "")
	if n := h.optional(pathPending, "rate_limited"); n != 1 {
		t.Errorf("optional_reads{pending, rate_limited} = %d, want 1", n)
	}
	// The ledger is whole: both cards and the joint account have the rows and balances they had.
	for slug, n := range map[string]int{"lloyds-joint": 6, "lloyds-card-5678": 2, "lloyds-card-9012": 1} {
		acc, err := h.core().GetAccountBySlug(context.Background(), slug)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(h.amounts(acc)); got != n {
			t.Errorf("%s has %d transactions, want %d", slug, got, n)
		}
	}
	// The pending rows of the earlier run are still there: a failed read replaces nothing.
	if got := h.pendingRows(); len(got) != 2 {
		t.Errorf("pending rows after a rate-limited read: %+v", got)
	}
}

package ledger_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/ledger"
)

// orderFile is one made-up file for one platform.
type orderFile struct {
	platform, format string
	result           ledger.InvestmentResult
}

// businessState is every investment table by business key, leaving out what depends on the order
// files were stored in: raw_seq, imported_at, first_seen_at, last_seen_at, seen_at, version_no,
// last_check_at and last_pass_at. disappeared_at is compared by its null state only.
func businessState(t *testing.T, e *env) string {
	t.Helper()
	ctx := context.Background()
	var b strings.Builder
	for _, q := range []struct{ name, sql string }{
		{"account", `SELECT slug, type, source, source_key, coalesce((SELECT slug FROM core.account p WHERE p.id = a.parent_id), '') FROM core.account a ORDER BY slug`},
		{"instrument", `SELECT provider, provider_key, coalesce(ticker, '') FROM core.instrument ORDER BY 1, 2`},
		{"event", `SELECT a.slug, e.source_key, e.event_type, e.provider_type, coalesce(e.instrument_name, ''), e.order_date::text,
			coalesce(e.settle_date::text, ''), coalesce(e.units::text, ''), e.cash_minor::text, e.flow_minor::text, e.gross_minor::text,
			(e.disappeared_at IS NULL)::text FROM core.investment_event e JOIN core.account a ON a.id = e.account_id ORDER BY 1, 2`},
		{"holding", `SELECT a.slug, i.provider_key, h.as_of::text, h.captured_at::text, h.units::text, h.value_minor::text
			FROM core.holding_snapshot h JOIN core.account a ON a.id = h.account_id JOIN core.instrument i ON i.id = h.instrument_id ORDER BY 1, 2, 3, 4`},
		{"valuation", `SELECT a.slug, v.as_of::text, v.captured_at::text, v.total_minor::text, v.cash_minor::text
			FROM core.valuation v JOIN core.account a ON a.id = v.account_id ORDER BY 1, 2, 3`},
		{"statement", `SELECT a.slug, s.period_start::text, s.period_end::text, s.issued_on::text, s.closing_minor::text, s.is_winner::text
			FROM core.pension_statement s JOIN core.account a ON a.id = s.account_id ORDER BY 1, 2, 4`},
		{"note", `SELECT a.slug, n.reference, n.isin, n.net_minor::text FROM core.trade_note n JOIN core.account a ON a.id = n.account_id ORDER BY 1, 2`},
		{"check", `SELECT a.slug, i.provider_key, c.checked_as_of::text, c.expected_units::text, c.reported_units::text, c.difference::text
			FROM core.holding_check c JOIN core.account a ON a.id = c.account_id JOIN core.instrument i ON i.id = c.instrument_id ORDER BY 1, 2`},
		{"reconciliation", `SELECT a.slug, r.method, coalesce(r.last_check_ok::text, 'null'), coalesce(r.last_diff_minor::text, 'null'),
			coalesce(r.reconciled_through::text, 'null'), r.break_count::text
			FROM core.reconciliation r JOIN core.account a ON a.id = r.account_id ORDER BY 1`},
	} {
		rows, err := e.app.DB().Query(ctx, q.sql)
		if err != nil {
			t.Fatalf("%s: %v", q.name, err)
		}
		lines, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
			vals, err := r.Values()
			if err != nil {
				return "", err
			}
			parts := make([]string, len(vals))
			for i, v := range vals {
				parts[i] = fmt.Sprint(v)
			}
			return q.name + " " + strings.Join(parts, "|"), nil
		})
		if err != nil {
			t.Fatalf("%s: %v", q.name, err)
		}
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

// The same files in opposite orders give the same business state: capture ordering, statement
// winners and disappearance depend on the set of files held. The clock is fixed, so every file has
// the same import time, after every capture; no two captures or statements tie.
func TestOppositeImportOrdersGiveTheSameState(t *testing.T) {
	const fid, avi = "ACC00001234", "PLAN00005678"
	at := func(day, hour int) time.Time { return time.Date(2030, time.June, day, hour, 0, 0, 0, time.UTC) }
	buy := trade(fid, "R1", ledger.EventBuy, "EXMP", "2030-01-05", "2030-01-07", "10.5")
	in := cashRow(fid, "R2", ledger.EventTransferIn, "2030-01-02", 5000)
	out := cashRow(fid, "R3", ledger.EventInternal, "2030-01-07", -4000)
	interest := cashRow(fid, "R4", ledger.EventInterest, "2030-03-05", 25)
	c1, c2, c3 := contribution(avi, "K1", "2029-06-01", 600), contribution(avi, "K2", "2029-09-01", 400), contribution(avi, "K3", "2030-05-01", 500)
	keptOut, droppedOut := transferOut(avi, "T-KEPT", "2029-12-01", 100), transferOut(avi, "T-LOST", "2029-11-01", 200)

	files := []orderFile{
		{"fid", fakeFidelityFormat, ledgerFile(fid, buy, in, out)},
		{"fid", fakeFidelityFormat, ledgerFile(fid, buy, in, out, interest)},
		{"fid", fakeFidelityFormat, summary(fid, "2030-02-01", at(1, 9), 1000, map[string]string{"EXMP": "10.5"})},
		// A mistaken capture for a date, then its correction: the correction is later whichever is
		// imported first.
		{"fid", fakeFidelityFormat, summary(fid, "2030-04-01", at(2, 9), 1, map[string]string{"EXMP": "10.5"})},
		{"fid", fakeFidelityFormat, summary(fid, "2030-04-01", at(2, 10), 1025, map[string]string{"EXMP": "10.5"})},
		{"fid", fakeFidelityFormat, ledger.InvestmentResult{Kind: ledger.InvestmentKindTradeNote, Accounts: sipp(fid), RowCount: 1,
			TradeNotes: []ledger.TradeNoteRec{{AccountKey: fid, Reference: "R1", ISIN: "GB00EXAMPLE1", ExecutedAt: at(1, 8),
				Units: "10.5", ExactPrice: "1.2345", GrossMinor: 1296, NetMinor: 1296, SettleDate: date("2030-01-07")}}}},

		// The older capture lists K2; the newer complete one does not, so K2 disappears. K3 is dated
		// after the newer capture's date and is left alone by it.
		{"avi", fakeAvivaFormat, capture(avi, "2030-01-31", at(3, 9), true, c1, c2)},
		{"avi", fakeAvivaFormat, capture(avi, "2030-03-31", at(4, 9), true, c1)},
		{"avi", fakeAvivaFormat, capture(avi, "2030-01-15", at(1, 7), false, c3)},
		// A statement and its reissue for the same period: the reissue, issued later, wins, and the
		// payment only the loser holds disappears.
		{"avi", fakeAvivaFormat, statementFile(avi, "2029-04-06", "2030-04-05", "2030-05-01", 10000, 600, 11000, keptOut, droppedOut)},
		{"avi", fakeAvivaFormat, statementFile(avi, "2029-04-06", "2030-04-05", "2030-05-20", 10000, 600, 11000, keptOut)},
	}
	fixed := time.Date(2030, time.July, 1, 12, 0, 0, 0, time.UTC)
	run := func(t *testing.T, order []orderFile) (string, *env) {
		e := newInvestmentEnv(t)
		e.platform("fid", ledger.ProviderFidelity)
		e.platform("avi", ledger.ProviderAviva)
		for i, f := range order {
			if st := e.investAt(f.platform, f.format, f.result, fixed); st.FilesRejected != 0 {
				t.Fatalf("file %d rejected: %v", i, st.Rejections)
			}
		}
		return businessState(t, e), e
	}
	forward, e := run(t, files)
	reversed := slices.Clone(files)
	slices.Reverse(reversed)
	backward, _ := run(t, reversed)
	if forward != backward {
		t.Fatalf("the two orders differ:\n%s", diffLines(forward, backward))
	}

	// The state both orders reached is the one the rules say, not merely the same.
	for _, want := range []string{
		"K1|contribution|Regular||2029-06-01|||0|600|600|true",
		"K2|contribution|Regular||2029-09-01|||0|400|400|false",
		"K3|contribution|Regular||2030-05-01|||0|500|500|true",
		"T-KEPT|transfer_out|Payment out||2029-12-01|||0|-100|-100|true",
		"T-LOST|transfer_out|Payment out||2029-11-01|||0|-200|-200|false",
		"statement avi-pension-5678|2029-04-06|2030-04-05|2030-05-01|11000|false",
		"statement avi-pension-5678|2029-04-06|2030-04-05|2030-05-20|11000|true",
		"reconciliation avi-pension-5678|statement_flows|true|0|2030-04-05|0",
		"reconciliation fid-sipp-1234|holdings_replay|true|0|2030-04-01|0",
	} {
		if !strings.Contains(forward, want) {
			t.Errorf("state lacks %q:\n%s", want, forward)
		}
	}

	// AC9: replay gives the same rows, at this revision, over a database holding investment data.
	if diffs := e.renormalise(true); len(diffs) != 0 {
		t.Fatalf("verify reported differences: %+v", diffs)
	}
	before := businessState(t, e)
	e.renormalise(false)
	if after := businessState(t, e); after != before {
		t.Fatalf("a rebuild changed the state:\n%s", diffLines(before, after))
	}
	if diffs := e.renormalise(true); len(diffs) != 0 {
		t.Fatalf("verify after a rebuild: %+v", diffs)
	}
}

func diffLines(a, b string) string {
	as, bs := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out strings.Builder
	for _, l := range as {
		if !slices.Contains(bs, l) {
			out.WriteString("- " + l + "\n")
		}
	}
	for _, l := range bs {
		if !slices.Contains(as, l) {
			out.WriteString("+ " + l + "\n")
		}
	}
	return out.String()
}

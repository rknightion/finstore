package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/store"
)

// txnClass inserts a core.txn_class row for the fixture transaction under key.
func (f *insightFixture) txnClass(key, class string, employer bool, seq int64) {
	f.t.Helper()
	err := f.core.PutTxnClass(context.Background(), store.TxnClass{TxnID: f.ids[key], FlowClass: class, Employer: employer,
		EntrySeq: seq, EnteredAt: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		f.t.Fatal(err)
	}
}

// A txn_class row sets the class of its own transaction, reason txn_rule, whatever the direction and
// the payee's own setting, and no other row's. Rows that a pot, Flex, held account or pairing rule
// classifies keep their class.
func TestTxnClassOverridesOneTransaction(t *testing.T) {
	f := newInsightFixture(t)
	ctx := context.Background()

	// A made-up company that pays salary and takes loan repayments under one name, with no setting.
	f.tx(itx{acct: "cur", key: "co_salary", date: "2026-03-05", amt: 250000, desc: "OWNED CO"})
	f.tx(itx{acct: "cur", key: "co_salary2", date: "2026-03-06", amt: 250000, desc: "OWNED CO"})
	f.tx(itx{acct: "cur", key: "co_loan_out", date: "2026-03-07", amt: -100000, desc: "OWNED CO"})
	f.tx(itx{acct: "cur", key: "co_loan_in", date: "2026-03-08", amt: 40000, desc: "OWNED CO"})
	f.tx(itx{acct: "cur", key: "co_plain", date: "2026-03-09", amt: -300, desc: "OWNED CO"})
	f.tx(itx{acct: "cur", key: "other", date: "2026-03-09", amt: -700, desc: "OTHER CO"})
	// A payee class setting loses to an override on one of its rows, in the direction it would not
	// have applied to.
	f.setting("n:spend co", "", "spending", "", "", nil, 4)
	f.tx(itx{acct: "cur", key: "sp_in", date: "2026-03-05", amt: 1200, desc: "SPEND CO"})
	f.tx(itx{acct: "cur", key: "sp_out", date: "2026-03-05", amt: -1300, desc: "SPEND CO"})
	// Rules 1 to 4 keep their rows: a pot move, a Flex repayment, a held counterparty in the same
	// scope and the Open Banking side of a pair in the same scope.
	f.tx(itx{acct: "cur", key: "r1", date: "2026-03-02", amt: -1000, desc: "OWNED CO", detail: md("pot_id", "pot_cur")})
	f.tx(itx{acct: "cur", key: "r2", date: "2026-03-03", amt: -2000, desc: "OWNED CO", detail: md("flex_transaction_type", "repayment")})
	f.tx(itx{acct: "cur", key: "r3", date: "2026-03-04", amt: -5000, cp: "My Other Bank", detail: cpDetail("sort_code", "30-99-99", "account_number", "33333333")})
	f.tx(itx{acct: "cur", key: "r4_cur", date: "2026-03-10", amt: -5100, cp: "My Other Bank", detail: cpDetail("sort_code", "30-99-99", "account_number", "33333333")})
	f.tx(itx{acct: "ob", key: "r4_ob", date: "2026-03-11", amt: 5100, desc: "FASTER PAYMENT FROM SELF"})
	// A transfer across scopes (rule 6) is a row the operator may reclassify.
	f.tx(itx{acct: "cur", key: "r6", date: "2026-03-05", amt: -222, cp: "Joint", detail: cpDetail("account_id", "acc_joint")})

	before := f.flows()
	for k, want := range map[string][2]string{
		"co_salary": {"income", "direction"}, "co_loan_out": {"spending", "direction"}, "r1": {"internal", "pot"},
		"r2": {"internal", "flex"}, "r3": {"internal", "own_account"}, "r4_cur": {"internal", "own_account"},
		"r4_ob": {"internal", "paired"}, "r6": {"scope_transfer", "own_account"},
	} {
		if g := before[k]; g.class != want[0] || g.reason != want[1] {
			t.Fatalf("before the override %s is %s/%s, want %s/%s", k, g.class, g.reason, want[0], want[1])
		}
	}

	f.txnClass("co_salary", "income", true, 1)
	f.txnClass("co_loan_out", "own_external", false, 2)
	f.txnClass("co_loan_in", "own_external", false, 3)
	f.txnClass("sp_in", "income", false, 4)
	f.txnClass("sp_out", "refund", false, 5) // a class the row's sign would never give it: the operator chose it
	f.txnClass("r1", "income", false, 6)
	f.txnClass("r2", "income", false, 7)
	f.txnClass("r3", "income", false, 8)
	f.txnClass("r4_ob", "income", true, 9)
	f.txnClass("r6", "own_external", false, 10)

	after := f.flows()
	if len(after) != len(before) {
		t.Fatalf("%d rows after the overrides, %d before: every transaction must stay once", len(after), len(before))
	}
	type want struct {
		class, reason string
		employer      *bool
	}
	for k, w := range map[string]want{
		"co_salary":   {"income", "txn_rule", ptr(true)},
		"co_loan_out": {"own_external", "txn_rule", ptr(false)},
		"co_loan_in":  {"own_external", "txn_rule", ptr(false)},
		"sp_in":       {"income", "txn_rule", ptr(false)},
		"sp_out":      {"refund", "txn_rule", ptr(false)},
		"r6":          {"own_external", "txn_rule", ptr(false)},
		// Rules 1 to 4: not changed, employer as the payee gives it.
		"r1": {"internal", "pot", nil}, "r2": {"internal", "flex", nil}, "r3": {"internal", "own_account", nil},
		"r4_ob": {"internal", "paired", nil},
	} {
		g := after[k]
		if g.class != w.class || g.reason != w.reason {
			t.Errorf("%s is %s/%s, want %s/%s", k, g.class, g.reason, w.class, w.reason)
		}
		if (g.employer == nil) != (w.employer == nil) || (g.employer != nil && *g.employer != *w.employer) {
			t.Errorf("%s employer %v, want %v", k, g.employer, w.employer)
		}
	}
	// Every other row is exactly as it was: the same payee's other rows included.
	overridden := map[string]bool{"co_salary": true, "co_loan_out": true, "co_loan_in": true, "sp_in": true, "sp_out": true, "r6": true}
	for k, b := range before {
		if overridden[k] {
			continue
		}
		if a := after[k]; a.class != b.class || a.reason != b.reason || a.amount != b.amount || a.payeeID != b.payeeID {
			t.Errorf("%s changed from %+v to %+v", k, b, a)
		}
	}
	for _, k := range []string{"co_salary2", "co_plain", "other"} {
		if g := after[k]; g.reason != "direction" {
			t.Errorf("%s, a row of the same payee with no entry, is %s/%s", k, g.class, g.reason)
		}
	}

	// monthly_flows: the overrides reach the totals, salary only from the employer-flagged income,
	// and the class columns still add up to the transactions.
	var income, salary, ownIn, ownOut, bad int64
	if err := f.q.QueryRow(ctx, `SELECT income_minor, salary_minor, own_external_in_minor, own_external_out_minor
		FROM agent.monthly_flows WHERE month = '2026-03' AND scope = 'personal' AND currency = 'GBP' AND status = 'settled'`).
		Scan(&income, &salary, &ownIn, &ownOut); err != nil {
		t.Fatal(err)
	}
	// income: the two salaries, the sp_in override, and the plain row of nothing else: 250000 + 250000 + 1200.
	// Rows in other classes are not income: the salary row of a fixture here is the only one in the month.
	if income != 500000+1200 || salary != 250000 || ownIn != 40000 || ownOut != 100000+222 {
		t.Errorf("income %d salary %d own_external in %d out %d", income, salary, ownIn, ownOut)
	}
	if err := f.q.QueryRow(ctx, `
		WITH m AS (SELECT month, scope, currency, status,
		             income_minor + refund_minor + borrowing_minor - spending_minor - debt_repayment_minor
		             + investment_in_minor - investment_out_minor + own_external_in_minor - own_external_out_minor
		             + scope_transfer_in_minor - scope_transfer_out_minor + internal_in_minor - internal_out_minor AS s
		           FROM agent.monthly_flows),
		     c AS (SELECT to_char(txn_date, 'YYYY-MM') AS month, account_scope AS scope, currency, status, sum(amount_minor) AS s
		           FROM agent.cash_flows GROUP BY 1, 2, 3, 4)
		SELECT count(*) FILTER (WHERE m.s IS DISTINCT FROM c.s) FROM m FULL JOIN c USING (month, scope, currency, status)`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Errorf("%d monthly_flows rows do not add up to their transactions", bad)
	}

	// agent.payees: the company has no setting, yet is an employer through its overridden salary row.
	var employer *bool
	var class string
	if err := f.q.QueryRow(ctx, `SELECT employer, flow_class FROM agent.payees WHERE payee_id = 'n:owned co' AND currency = 'GBP'`).
		Scan(&employer, &class); err != nil {
		t.Fatal(err)
	}
	if employer == nil || !*employer {
		t.Errorf("payees.employer for the company = %v, want true", employer)
	}
	if err := f.q.QueryRow(ctx, `SELECT employer FROM agent.payees WHERE payee_id = 'n:other co'`).Scan(&employer); err != nil || employer != nil {
		t.Errorf("an unrelated payee's employer = %v, %v", employer, err)
	}
	// The payee's own class setting is not replaced by an override on a row of it.
	if err := f.q.QueryRow(ctx, `SELECT flow_class FROM agent.payees WHERE payee_id = 'n:spend co'`).Scan(&class); err != nil || class != "spending" {
		t.Errorf("payee class setting after a row override = %q, %v", class, err)
	}

	// A clear (the row deleted) puts the transaction back where the other rules had it.
	if err := f.core.DeleteTxnClass(ctx, f.ids["co_loan_out"]); err != nil {
		t.Fatal(err)
	}
	if g := f.flows()["co_loan_out"]; g.class != "spending" || g.reason != "direction" {
		t.Errorf("co_loan_out after the clear is %s/%s", g.class, g.reason)
	}
}

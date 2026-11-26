package store_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// timingBudgets are the budgets of the design's Non-Functional Requirements: a full SELECT of each
// insight view over 30,000 transactions in under two seconds, agent.recurring_payments in under three.
var timingBudgets = []struct {
	view   string
	budget time.Duration
}{
	{"account_scopes", 2 * time.Second},
	{"payee_keys", 2 * time.Second},
	{"cash_flows", 2 * time.Second},
	{"payees", 2 * time.Second},
	{"payee_years", 2 * time.Second},
	{"payee_rule_status", 2 * time.Second},
	{"recurring_payments", 3 * time.Second},
	{"upcoming_payments", 2 * time.Second},
	{"monthly_flows", 2 * time.Second},
	{"category_months", 2 * time.Second},
	{"anomalies", 2 * time.Second},
}

// timingLoad writes 30,000 made-up transactions over 60 accounts and nine years: monthly
// subscriptions (a third with a price step), direct debit series, salaries, paired transfers
// between the accounts, card checks, foreign-currency card payments with a fee, and a long tail of
// variable spending at 120 shops. Everything is generated; nothing is real.
const timingLoad = `
SELECT setseed(0.42);
INSERT INTO core.account (id, source, source_key, slug, type, feed, currency, name, bank_ref)
SELECT gen_random_uuid(), 'timing', 'acct_' || n, 'acct-' || lpad(n::text, 2, '0'), 'current', 'file', 'GBP', 'Account ' || n,
       '04' || lpad((n % 90 + 10)::text, 4, '0') || ' ' || lpad((10000000 + n)::text, 8, '0')
FROM generate_series(1, 60) n;

CREATE TEMP TABLE gen (id uuid DEFAULT gen_random_uuid(), acct int, d date, amt bigint, descr text, cp text,
                       scheme text, loc text, detail jsonb DEFAULT '{}');

-- 250 subscriptions of 40 monthly charges; a third rise by 40 percent after the 20th charge.
INSERT INTO gen (acct, d, amt, descr, scheme)
SELECT 1 + (s % 60), (current_date - 1230 - (s * 7 % 2000) + 30 * m)::date,
       -(500 + (s * 37 % 4000))::bigint * CASE WHEN m >= 20 AND s % 3 = 0 THEN 14 ELSE 10 END / 10,
       'Subscription ' || translate(s::text, '0123456789', 'abcdefghij'), 'mastercard'
FROM generate_series(1, 250) s CROSS JOIN generate_series(0, 39) m;

-- 25 direct debit series of 40 monthly charges.
INSERT INTO gen (acct, d, amt, descr, scheme, detail)
SELECT 1 + (s % 60), (current_date - 1220 - (s * 11 % 1900) + 30 * m)::date, -(2000 + s * 91)::bigint,
       'Utility ' || translate(s::text, '0123456789', 'abcdefghij'), 'faster_payments',
       jsonb_build_object('metadata', jsonb_build_object('bacs_direct_debit_instruction_id', 'mandate_' || translate(s::text, '0123456789', 'abcdefghij')))
FROM generate_series(1, 25) s CROSS JOIN generate_series(0, 39) m;

-- Two employers paying monthly for nine years.
INSERT INTO gen (acct, d, amt, descr, scheme)
SELECT 1 + (e % 60), (current_date - 3280 + 30 * m)::date, 250000 + e * 5000, 'Employer ' || translate(e::text, '0123456789', 'abcdefghij'), 'faster_payments'
FROM generate_series(1, 2) e CROSS JOIN generate_series(0, 107) m;

-- 750 transfers between held accounts, both legs on the same day, naming each other's account.
INSERT INTO gen (acct, d, amt, descr, cp, scheme, detail)
SELECT leg.acct, t.d, leg.amt, 'Transfer', 'Own Account', 'faster_payments',
       jsonb_build_object('counterparty', jsonb_build_object('account_id', 'acct_' || leg.other))
FROM (SELECT 1 + (g % 60) AS a, 1 + ((g * 7 + 3) % 60) AS b, (current_date - 3200 + (g * 4) % 3100)::date AS d,
             (1000 + g * 13)::bigint AS amt FROM generate_series(1, 750) g) t
CROSS JOIN LATERAL (VALUES (t.a, -t.amt, t.b), (t.b, t.amt, t.a)) AS leg(acct, amt, other);

-- 300 card checks, 200 foreign card payments with a fee.
INSERT INTO gen (acct, d, amt, descr, scheme)
SELECT 1 + (g % 60), (current_date - 3000 + g * 9)::date, -50, 'Card Check ' || translate((g % 7)::text, '0123456789', 'abcdefghij'), 'mastercard'
FROM generate_series(1, 300) g;
INSERT INTO gen (acct, d, amt, descr, scheme, loc, detail)
SELECT 1 + (g % 60), (current_date - 2900 + g * 13)::date, -(1000 + g * 7)::bigint, 'Abroad ' || translate((g % 9)::text, '0123456789', 'abcdefghij'),
       'mastercard', 'EUR', jsonb_build_object('fees', jsonb_build_object('currency_conversion', 20))
FROM generate_series(1, 200) g;

-- Habitual shops of a busy account: three takeaways charging a few fixed amounts (often twice in a
-- day) and two supermarkets with varied baskets, 6,000 charges over the last three years.
INSERT INTO gen (acct, d, amt, descr, scheme)
SELECT 1 + (g % 3), (current_date - 1100 + floor(random() * 1100)::int)::date,
       -(CASE floor(random() * 3)::int WHEN 0 THEN 1850 WHEN 1 THEN 2350 ELSE 2999 END)::bigint,
       'Takeaway ' || translate((g % 3)::text, '0123456789', 'abcdefghij'), 'mastercard'
FROM generate_series(1, 3000) g;
INSERT INTO gen (acct, d, amt, descr, scheme)
SELECT 1 + (g % 3), (current_date - 1100 + floor(random() * 1100)::int)::date,
       -(400 + floor(random() * random() * 14000) * CASE WHEN random() < 0.03 THEN 4 ELSE 1 END)::bigint,
       'Supermarket ' || translate((g % 2)::text, '0123456789', 'abcdefghij'), 'mastercard'
FROM generate_series(1, 3000) g;

-- The remainder: variable spending at 120 shops, skewed to a few, with the odd large purchase.
INSERT INTO gen (acct, d, amt, descr, scheme)
SELECT 1 + floor(random() * 60)::int, (current_date - 3280 + floor(random() * 3280)::int)::date,
       -(300 + floor(random() * random() * 15000) * CASE WHEN random() < 0.01 THEN 8 ELSE 1 END)::bigint,
       'Shop ' || translate(floor(120 * random() * random())::int::text, '0123456789', 'abcdefghij'), 'mastercard'
FROM generate_series(1, 30000 - (SELECT count(*) FROM gen)::int) g;

INSERT INTO core.transaction (id, account_id, source, source_key, amount_minor, currency, local_amount_minor, local_currency,
                              created_at, settled_at, txn_date, status, description, counterparty, scheme,
                              version_count, current_hash, first_seen_at, last_seen_at)
SELECT g.id, a.id, 'timing', g.id::text, g.amt, 'GBP', CASE WHEN g.loc IS NOT NULL THEN g.amt END, g.loc,
       g.d + interval '10 hours', g.d + interval '11 hours', g.d, CASE WHEN g.d >= current_date - 2 THEN 'pending' ELSE 'settled' END,
       g.descr, g.cp, g.scheme, 1, '\x01', now(), now()
FROM gen g JOIN core.account a ON a.source_key = 'acct_' || g.acct;
INSERT INTO core.transaction_version (transaction_id, version_no, content, content_hash, seen_at, raw_seq)
SELECT id, 1, detail, '\x01', now(), 1 FROM gen;
ANALYZE core.account;
ANALYZE core.transaction;
ANALYZE core.transaction_version;
`

// TestInsightViewTiming runs only with FINSTORE_TIMING=1 (it takes a while and its budgets depend on
// the machine): it loads 30,000 generated transactions and times a full SELECT of every insight view.
func TestInsightViewTiming(t *testing.T) {
	if os.Getenv("FINSTORE_TIMING") != "1" {
		t.Skip("set FINSTORE_TIMING=1 to time the insight views over 30,000 generated transactions")
	}
	c := newCluster(t)
	ctx := context.Background()
	load, err := pgx.Connect(ctx, c.super)
	if err != nil {
		t.Fatal(err)
	}
	defer load.Close(ctx)
	if _, err := load.Exec(ctx, timingLoad, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("load: %v", err)
	}
	var n, accts int
	if err := load.QueryRow(ctx, `SELECT (SELECT count(*) FROM core.transaction), (SELECT count(*) FROM core.account)`).Scan(&n, &accts); err != nil {
		t.Fatal(err)
	}
	if n != 30000 || accts != 60 {
		t.Fatalf("generated %d transactions over %d accounts, want 30000 over 60", n, accts)
	}

	agent := connAs(t, c.super, "finstore_agent")
	// A busy account should see a handful of flags in 90 days, not dozens (issue 177). The generated
	// spending is more regular than a person's, so the ceiling is generous.
	var flags90 int
	rows90, err := agent.Query(ctx, `SELECT kind, count(*) FROM agent.anomalies WHERE txn_date >= current_date - 90 GROUP BY kind ORDER BY kind`)
	if err != nil {
		t.Fatal(err)
	}
	for rows90.Next() {
		var kind string
		var k int
		if err := rows90.Scan(&kind, &k); err != nil {
			t.Fatal(err)
		}
		t.Logf("anomalies in the last 90 days: %-16s %d", kind, k)
		flags90 += k
	}
	rows90.Close()
	if flags90 > 20 {
		t.Errorf("%d anomalies in the last 90 days over 30000 transactions, want at most 20", flags90)
	}
	// Each timed read fetches every row of the view, as a reader would. A view is read three times
	// and judged on its fastest read. Where /proc is readable the read is measured by the CPU time of
	// its backend, which a busy machine does not stretch, and the wall time is logged beside it.
	var pid int
	if err := agent.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	for _, b := range timingBudgets {
		var reads, wall []time.Duration
		count := 0
		for i := 0; i < 3; i++ {
			cpu0, haveCPU := backendCPU(pid)
			start := time.Now()
			rows, err := agent.Query(ctx, `SELECT * FROM agent.`+b.view)
			if err != nil {
				t.Fatalf("agent.%s: %v", b.view, err)
			}
			count = 0
			for rows.Next() {
				if _, err := rows.Values(); err != nil {
					t.Fatal(err)
				}
				count++
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("agent.%s: %v", b.view, err)
			}
			rows.Close()
			took := time.Since(start)
			wall = append(wall, took)
			if cpu1, ok := backendCPU(pid); haveCPU && ok {
				took = cpu1 - cpu0
			}
			reads = append(reads, took)
		}
		best := slices.Min(reads)
		t.Logf("agent.%-20s %6d rows  best %6d ms (reads %v, wall %v)  budget %v", b.view, count,
			best.Milliseconds(), msList(reads), msList(wall), b.budget)
		if best > b.budget {
			t.Errorf("agent.%s took %v at best over 30000 transactions, over its %v budget", b.view, best, b.budget)
		}
	}
}

func msList(ds []time.Duration) []int64 {
	out := make([]int64, len(ds))
	for i, d := range ds {
		out[i] = d.Milliseconds()
	}
	return out
}

// backendCPU is the CPU time a PostgreSQL backend has used (user plus system), read from
// /proc/<pid>/stat; ok is false where that is unavailable (not Linux).
func backendCPU(pid int) (time.Duration, bool) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	// The command name is in parentheses and may contain spaces; the fields after it start at "state".
	i := strings.LastIndex(string(raw), ")")
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(string(raw)[i+1:])
	if len(f) < 13 {
		return 0, false
	}
	utime, err1 := strconv.ParseInt(f[11], 10, 64)
	stime, err2 := strconv.ParseInt(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	// Clock ticks are 100 a second on every Linux platform finstore runs on.
	return time.Duration(utime+stime) * 10 * time.Millisecond, true
}

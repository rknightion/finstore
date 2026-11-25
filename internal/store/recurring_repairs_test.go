package store_test

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A standing order evidence series is left out of agent.upcoming_payments only where a live row of
// agent.standing_orders covers its account; without one it is projected like any other series.
func TestUpcomingStandingOrderSeriesCoverage(t *testing.T) {
	r := newRecFix(t)
	cur, sav := r.acct["cur"], r.acct["sav"]
	r.standingOrder("covered", &cur, 5, 1000, false)
	r.standingOrder("withdrawn", &sav, 6, 6000, true)
	soSeries := func(name, acct, id string) {
		r.put(rs{name: name, acct: acct, amts: flat(2500, 4), offs: steps(4, 30, -20), detail: md("series_id", id)})
	}
	soSeries("Covered Co", "cur", "so_covered")
	soSeries("Orphan Co", "sav", "so_orphan")

	rows, err := r.q.Query(context.Background(), `SELECT payee_name, source FROM agent.upcoming_payments
		WHERE payee_name IN ('Covered Co', 'Orphan Co') ORDER BY payee_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var name, source string
		if err := rows.Scan(&name, &source); err != nil {
			t.Fatal(err)
		}
		if source != "standing_order" {
			t.Errorf("%s source = %s, want standing_order", name, source)
		}
		got[name]++
	}
	if got["Covered Co"] != 0 {
		t.Errorf("Covered Co has %d upcoming rows, want none (a live standing order covers its account)", got["Covered Co"])
	}
	if got["Orphan Co"] != 2 {
		t.Errorf("Orphan Co has %d upcoming rows, want 2 (no live standing order on its account)", got["Orphan Co"])
	}
}

// Overdue series do not list past projected steps: only the first row may be before today.
func TestUpcomingOverdueSeriesSkipsMissedSteps(t *testing.T) {
	r := newRecFix(t)
	// Gaps of 8 days are weekly, but the projection steps 7: next expected today - 8 (overdue), and
	// the step after it, today - 1, is already past.
	r.put(rs{name: "Late Weekly Co", amts: flat(500, 5), offs: steps(5, 8, -16)})
	rows, err := r.q.Query(context.Background(), `SELECT date FROM agent.upcoming_payments WHERE payee_name = 'Late Weekly Co' ORDER BY date`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		got = append(got, d.Format("2006-01-02"))
	}
	want := []string{r.at(-8), r.at(6), r.at(13), r.at(20), r.at(27), r.at(34), r.at(41)}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Late Weekly Co dates = %v, want %v", got, want)
	}
}

// The twelve month window of amount_kind includes the day exactly a year before the last payment.
func TestRecurringAmountKindWindowIsInclusive(t *testing.T) {
	r := newRecFix(t)
	r.put(rs{name: "Annual Co", amts: []int64{1000, 1200}, offs: []int{-370, -5}})
	x := one(t, r.recurring(), "Annual Co")
	if x.cadence != "yearly" || x.kind != "variable" {
		t.Errorf("Annual Co = cadence %s kind %s, want yearly variable (the price changed on the boundary day)", x.cadence, x.kind)
	}
}

package store_test

import (
	"fmt"
	"testing"
)

// price_changes lists every step of a series whose amounts form steps (each amount holding for at
// least two consecutive charges), whatever its amount_kind, and none for a bill that varies every
// charge. Every value is invented.
func TestRecurringPriceSteps(t *testing.T) {
	r := newRecFix(t)
	// A within-tolerance rise inside the last year: 999 for six months, then 1099 for four.
	r.put(rs{name: "Step Co", amts: repeat(flat(999, 6), flat(1099, 4)), offs: steps(10, 30, -10)})
	// Two steps, the first older than twelve months: 999, 1099, then 1150 (all inside one band).
	r.put(rs{name: "Old Step Co", amts: repeat(flat(999, 4), flat(1099, 12), flat(1150, 8)), offs: steps(24, 30, -10)})
	// A bill that varies every charge lists nothing.
	r.put(rs{name: "Vary Co", amts: []int64{999, 1050, 999, 1050, 1000, 1049, 999, 1050}, offs: steps(8, 30, -10)})

	all := r.recurring()
	step := one(t, all, "Step Co")
	wantStep := []priceChange{{Date: r.date(steps(10, 30, -10)[6]).Format("2006-01-02"), From: 999, To: 1099}}
	if step.kind != "variable" || fmt.Sprint(step.changes) != fmt.Sprint(wantStep) {
		t.Errorf("Step Co = %s %+v, want variable with %+v", step.kind, step.changes, wantStep)
	}

	o := steps(24, 30, -10)
	old := one(t, all, "Old Step Co")
	wantOld := []priceChange{
		{Date: r.date(o[4]).Format("2006-01-02"), From: 999, To: 1099},
		{Date: r.date(o[16]).Format("2006-01-02"), From: 1099, To: 1150},
	}
	if old.kind != "variable" || fmt.Sprint(old.changes) != fmt.Sprint(wantOld) {
		t.Errorf("Old Step Co = %s %+v, want variable with %+v", old.kind, old.changes, wantOld)
	}

	vary := one(t, all, "Vary Co")
	if vary.kind != "variable" || len(vary.changes) != 0 {
		t.Errorf("Vary Co = %s %+v, want variable with no price changes", vary.kind, vary.changes)
	}
}

package importer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// CheckName names a statement check.
type CheckName string

// The checks Check runs.
const (
	// CheckTotals: the rows add up to the header's Money In and Money Out.
	CheckTotals CheckName = "totals"
	// CheckRunningBalance: each row's balance follows from the row before.
	CheckRunningBalance CheckName = "running-balance"
	// CheckClosingBalance: the last row's balance is the header's closing balance.
	CheckClosingBalance CheckName = "closing-balance"
	// CheckDateOrder: each row's date is on or after the previous row's.
	CheckDateOrder CheckName = "date-order"
	// CheckPeriod: each row's date lies within the printed period, both days
	// included. Halifax PDF only: the manual CSV has no printed period.
	CheckPeriod CheckName = "period"
)

// CheckError reports a failed check. It names the check and the 1-based row
// where it broke, and nothing else: never an amount or a description. Row is 0
// when the check concerns the statement as a whole (totals).
type CheckError struct {
	Check CheckName
	Row   int
}

func (e *CheckError) Error() string {
	if e.Row == 0 {
		return fmt.Sprintf("importer: %s check failed", e.Check)
	}
	return fmt.Sprintf("importer: %s check failed at row %d", e.Check, e.Row)
}

// Check runs the statement's own checks. A halifax-pdf statement must pass
// all five, in the order totals, running balance, closing balance, date
// order, period; a manual-csv statement has the running-balance and
// date-order checks, in that order. The first failure is returned as a
// *CheckError.
func Check(st *Statement) error {
	switch st.Format {
	case FormatHalifaxPDF:
		if err := checkTotals(st); err != nil {
			return err
		}
		if err := checkRunning(st); err != nil {
			return err
		}
		if n := len(st.Rows); n > 0 && st.Rows[n-1].Balance != st.ClosingBalance {
			return &CheckError{Check: CheckClosingBalance, Row: n}
		}
		if err := checkDateOrder(st); err != nil {
			return err
		}
		return checkPeriod(st)
	case FormatManualCSV:
		if err := checkRunning(st); err != nil {
			return err
		}
		return checkDateOrder(st)
	}
	return fmt.Errorf("%w: unknown format", ErrFormat)
}

func checkTotals(st *Statement) error {
	var in, out int64
	for _, r := range st.Rows {
		in += r.MoneyIn
		out += r.MoneyOut
	}
	if in != st.TotalIn || out != st.TotalOut {
		return &CheckError{Check: CheckTotals}
	}
	return nil
}

func checkRunning(st *Statement) error {
	for i := 1; i < len(st.Rows); i++ {
		prev, r := st.Rows[i-1], st.Rows[i]
		if r.Balance != prev.Balance+r.MoneyIn-r.MoneyOut {
			return &CheckError{Check: CheckRunningBalance, Row: i + 1}
		}
	}
	return nil
}

// checkDateOrder fails at the first row dated before the row above it: both
// formats list rows oldest first, and same-day rows are allowed.
func checkDateOrder(st *Statement) error {
	for i := 1; i < len(st.Rows); i++ {
		if st.Rows[i].Date.Before(st.Rows[i-1].Date) {
			return &CheckError{Check: CheckDateOrder, Row: i + 1}
		}
	}
	return nil
}

// checkPeriod fails at the first row dated before PeriodStart or after
// PeriodEnd.
func checkPeriod(st *Statement) error {
	for i, r := range st.Rows {
		if r.Date.Before(st.PeriodStart) || r.Date.After(st.PeriodEnd) {
			return &CheckError{Check: CheckPeriod, Row: i + 1}
		}
	}
	return nil
}

// OpeningBalance is the balance before the first row: its balance, less what
// it paid in, plus what it paid out. The caller compares it with the last
// balance already stored for the account. A statement with no rows has no
// opening balance and yields 0; check len(st.Rows) first.
func OpeningBalance(st *Statement) int64 {
	if len(st.Rows) == 0 {
		return 0
	}
	r := st.Rows[0]
	return r.Balance - r.MoneyIn + r.MoneyOut
}

const keySep = "\x1f" // U+001F, a single byte in UTF-8

// Keys sets every row's import key: the hex SHA-256 of the row's fields joined
// with U+001F, then "#" and the row's 1-based position among the statement's
// rows that are identical in all those fields. The same row in two overlapping
// statements therefore gets the same key.
//
// The fields are the date as YYYY-MM-DD, type, description, money in, money
// out and balance, with amounts in decimal pence. A manual-csv row uses date,
// type, amount (money in less money out), balance and description, in that
// order.
func Keys(st *Statement) {
	seen := make(map[[sha256.Size]byte]int, len(st.Rows))
	for i := range st.Rows {
		r := &st.Rows[i]
		date := r.Date.Format("2006-01-02")
		var fields []string
		if st.Format == FormatManualCSV {
			fields = []string{date, r.Type, pence(r.MoneyIn - r.MoneyOut), pence(r.Balance), r.Description}
		} else {
			fields = []string{date, r.Type, r.Description, pence(r.MoneyIn), pence(r.MoneyOut), pence(r.Balance)}
		}
		sum := sha256.Sum256([]byte(strings.Join(fields, keySep)))
		seen[sum]++
		r.Key = hex.EncodeToString(sum[:]) + "#" + strconv.Itoa(seen[sum])
	}
}

func pence(n int64) string { return strconv.FormatInt(n, 10) }

// Package importer turns statement files into rows: the Halifax PDF statement
// and the project's own manual CSV layout. The parsers are pure functions of
// their input. Errors never carry statement content: no amounts, descriptions
// or identifiers.
package importer

import (
	"errors"
	"time"
)

// Format names a statement format, as `finstore import --format` spells it.
type Format string

// Supported formats.
const (
	FormatHalifaxPDF Format = "halifax-pdf"
	FormatManualCSV  Format = "manual-csv"
)

// ErrFormat is wrapped by every error that says a file is not a statement this
// package accepts.
var ErrFormat = errors.New("importer: unreadable statement")

// Statement is one parsed statement file. All amounts are minor units (pence).
//
// For FormatManualCSV only Format and Rows are set.
type Statement struct {
	Format Format

	// SortCode and AccountNumber are the identifiers printed in the header,
	// exactly as printed. The caller matches them to its account.
	SortCode      string
	AccountNumber string

	// PeriodStart and PeriodEnd are midnight UTC on the first and last day.
	PeriodStart time.Time
	PeriodEnd   time.Time

	// TotalIn and TotalOut are the header's Money In and Money Out totals.
	TotalIn  int64
	TotalOut int64

	// ClosingBalance is the header's "Balance on <end date>".
	ClosingBalance int64
	// StartBalance is the header's "Balance on <start date>". It is stored and
	// never used: in some statements it is the balance after the first row,
	// in others before it.
	StartBalance int64

	// Rows are in statement order, oldest first.
	Rows []Row
}

// Row is one transaction line. MoneyIn and MoneyOut are never negative; a
// row has at most one of them non-zero. Balance is the balance after the row.
type Row struct {
	// Date is midnight UTC.
	Date        time.Time
	Description string
	// Type is the bank's transaction type code.
	Type     string
	MoneyIn  int64
	MoneyOut int64
	Balance  int64
	// Key is the import row key, set by Keys.
	Key string
}

package importer

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rknightion/finstore/internal/ledger"
)

// Normaliser adapts the statement parsers to ledger.FileNormaliser. Register it under both
// FormatHalifaxPDF and FormatManualCSV.
type Normaliser struct{}

var _ ledger.FileNormaliser = Normaliser{}

// rowContent is the stored content of one imported row and the input of its version hash. Its
// fields are in key order, so its JSON encoding is canonical.
type rowContent struct {
	Balance     int64  `json:"balance"`
	Date        string `json:"date"`
	Description string `json:"description"`
	MoneyIn     int64  `json:"money_in"`
	MoneyOut    int64  `json:"money_out"`
	Type        string `json:"type"`
}

// NormaliseFile parses f by its format, runs the statement's own checks and returns its rows as
// settled transactions keyed by import row key, one balance per row, and the statement's opening
// and closing balances and the dates it covers. A halifax-pdf statement covers its printed period,
// so consecutive monthly statements abut, and names its account by sort code and account number
// (digits only, joined by a space); its period is exact. A manual-csv statement covers its first
// to its last row date, which is not exact, and names no account. A statement with no rows (only
// a PDF can have none) has its closing
// balance as both opening and closing. The error never carries statement content.
func (Normaliser) NormaliseFile(f ledger.File) (ledger.FileResult, error) {
	var st *Statement
	var err error
	switch Format(f.Format) {
	case FormatHalifaxPDF:
		st, err = ParseHalifaxPDF(f.Body)
	case FormatManualCSV:
		st, err = ParseManualCSV(f.Body)
	default:
		return ledger.FileResult{}, fmt.Errorf("%w: unknown format", ErrFormat)
	}
	if err != nil {
		return ledger.FileResult{}, err
	}
	if err := Check(st); err != nil {
		return ledger.FileResult{}, err
	}
	var res ledger.FileResult
	if len(st.Rows) == 0 {
		res.OpeningMinor, res.ClosingMinor = st.ClosingBalance, st.ClosingBalance
	} else {
		res.OpeningMinor, res.ClosingMinor = OpeningBalance(st), st.Rows[len(st.Rows)-1].Balance
		res.FirstDate, res.LastDate = st.Rows[0].Date, st.Rows[len(st.Rows)-1].Date
	}
	if st.Format == FormatHalifaxPDF {
		res.FirstDate, res.LastDate, res.PeriodExact = st.PeriodStart, st.PeriodEnd, true
		res.AccountRef = accountRef(st.SortCode, st.AccountNumber)
	}
	for _, r := range st.Rows {
		content, err := json.Marshal(rowContent{Balance: r.Balance, Date: r.Date.Format("2006-01-02"),
			Description: r.Description, MoneyIn: r.MoneyIn, MoneyOut: r.MoneyOut, Type: r.Type})
		if err != nil {
			return ledger.FileResult{}, fmt.Errorf("importer: encode row: %w", err)
		}
		res.Txns = append(res.Txns, ledger.TxnRec{
			AccountSourceKey: f.AccountSlug,
			SourceKey:        r.Key,
			AmountMinor:      r.MoneyIn - r.MoneyOut,
			TxnDate:          r.Date,
			Status:           ledger.StatusSettled,
			Description:      r.Description,
			Scheme:           r.Type,
			Content:          content,
			HashInput:        content,
		})
		res.Balances = append(res.Balances, ledger.BalanceRec{AccountSourceKey: f.AccountSlug, OnDate: r.Date,
			BalanceMinor: r.Balance})
	}
	return res, nil
}

// accountRef is the sort code and account number with every non-digit removed, joined by a
// space, or empty when both are.
func accountRef(sortCode, accountNumber string) string {
	digits := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, s)
	}
	sc, an := digits(sortCode), digits(accountNumber)
	if sc == "" && an == "" {
		return ""
	}
	return sc + " " + an
}

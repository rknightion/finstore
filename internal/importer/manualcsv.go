package importer

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

var manualCSVHeader = []string{"date", "type", "amount", "balance_after", "description"}

// ParseManualCSV parses the manual-csv layout: UTF-8, comma-separated, header
// date,type,amount,balance_after,description, oldest row first. Dates are
// YYYY-MM-DD. Amounts are decimal pounds with exactly two decimals and an
// optional pound sign; amount is signed (negative reduces the balance). A
// positive amount is stored as money in, a negative one as money out.
func ParseManualCSV(data []byte) (*Statement, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = len(manualCSVHeader)
	header, err := r.Read()
	if err != nil || !equalFold(header, manualCSVHeader) {
		return nil, fmt.Errorf("%w: bad csv header", ErrFormat)
	}
	st := &Statement{Format: FormatManualCSV}
	for n := 1; ; n++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: bad csv record %d", ErrFormat, n)
		}
		row, err := csvRow(rec)
		if err != nil {
			return nil, fmt.Errorf("%w (row %d)", err, n)
		}
		st.Rows = append(st.Rows, row)
	}
	if len(st.Rows) == 0 {
		return nil, fmt.Errorf("%w: no rows", ErrFormat)
	}
	Keys(st)
	return st, nil
}

func equalFold(got, want []string) bool {
	for i := range want {
		if strings.TrimSpace(got[i]) != want[i] {
			return false
		}
	}
	return true
}

func csvRow(rec []string) (Row, error) {
	date, err := time.Parse("2006-01-02", strings.TrimSpace(rec[0]))
	if err != nil {
		return Row{}, fmt.Errorf("%w: bad date", ErrFormat)
	}
	amount, err := parsePounds(rec[2], true)
	if err != nil {
		return Row{}, err
	}
	balance, err := parsePounds(rec[3], true)
	if err != nil {
		return Row{}, err
	}
	row := Row{
		Date:        date,
		Type:        strings.TrimSpace(rec[1]),
		Description: strings.TrimSpace(rec[4]),
		Balance:     balance,
	}
	if amount >= 0 {
		row.MoneyIn = amount
	} else {
		row.MoneyOut = -amount
	}
	return row, nil
}

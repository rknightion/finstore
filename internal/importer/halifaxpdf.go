package importer

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rknightion/finstore/internal/pdftext"
)

const (
	// lineTolerance is how far apart, in points, two texts may be in y and
	// still sit on one line.
	lineTolerance = 0.5
	// colTolerance absorbs float noise when comparing x to a header's left edge.
	colTolerance = 0.05
	// maxContinuationGap is how far, in points, a continuation line may sit
	// below the line before it in its row. Rows are 25 points apart and a
	// continuation line is 12 points below its row in the real layout.
	maxContinuationGap = 16

	longDate  = "02 January 2006"
	shortDate = "02 Jan 06"

	endMarker = "Transaction types"
)

// Table columns, in header order.
const (
	colDate = iota
	colDescription
	colType
	colIn
	colOut
	colBalance
	numCols
)

var headerTexts = [numCols]string{"Date", "Description", "Type", "Money In (£)", "Money Out (£)", "Balance (£)"}

// line is the texts of one page that share a y coordinate, left to right. y is
// the coordinate of its topmost text.
type line struct {
	page  int
	y     float64
	items []pdftext.Text
}

// ParseHalifaxPDF parses a Halifax statement PDF as described in the design,
// section 1.8. It does not run the statement's checks; call Check. Rows carry
// their import keys.
func ParseHalifaxPDF(data []byte) (*Statement, error) {
	texts, err := pdftext.Read(data)
	if err != nil {
		return nil, fmt.Errorf("importer: reading pdf: %w", err)
	}
	lines := groupLines(texts)
	first := -1
	for i, l := range lines {
		if _, ok := headerColumns(l); ok {
			first = i
			break
		}
	}
	if first < 0 {
		return nil, fmt.Errorf("%w: no transaction table header", ErrFormat)
	}
	st := &Statement{Format: FormatHalifaxPDF}
	if err := parseHeaderBlock(st, lines[:first]); err != nil {
		return nil, err
	}
	rows, err := parseTable(lines[first:])
	if err != nil {
		return nil, err
	}
	st.Rows = rows
	Keys(st)
	return st, nil
}

// groupLines groups texts by page and y, pages in order, top of the page
// first, each line sorted by x.
func groupLines(texts []pdftext.Text) []line {
	ts := append([]pdftext.Text(nil), texts...)
	sort.SliceStable(ts, func(i, j int) bool {
		if ts[i].Page != ts[j].Page {
			return ts[i].Page < ts[j].Page
		}
		return ts[i].Y > ts[j].Y
	})
	var lines []line
	var top float64
	for _, t := range ts {
		n := len(lines)
		if n == 0 || lines[n-1].page != t.Page || top-t.Y > lineTolerance {
			lines = append(lines, line{page: t.Page, y: t.Y})
			top = t.Y
			n++
		}
		lines[n-1].items = append(lines[n-1].items, t)
	}
	for _, l := range lines {
		sort.SliceStable(l.items, func(i, j int) bool { return l.items[i].X < l.items[j].X })
	}
	return lines
}

// headerColumns returns the left edges of the six columns if l is the table
// header row.
func headerColumns(l line) ([numCols]float64, bool) {
	var cols [numCols]float64
	if len(l.items) != numCols {
		return cols, false
	}
	for i, it := range l.items {
		if strings.TrimSpace(it.S) != headerTexts[i] {
			return cols, false
		}
		cols[i] = it.X
	}
	return cols, true
}

type balanceLine struct {
	date   time.Time
	amount int64
}

// parseHeaderBlock reads the period, identifiers, totals and balances from the
// lines above the table.
func parseHeaderBlock(st *Statement, lines []line) error {
	var (
		haveIn, haveOut, havePeriod bool
		balances                    []balanceLine
	)
	for _, l := range lines {
		for i, it := range l.items {
			s := strings.TrimSpace(it.S)
			value := ""
			if i+1 < len(l.items) {
				value = strings.TrimSpace(l.items[i+1].S)
			}
			var err error
			switch {
			case s == "Sort Code":
				st.SortCode = value
			case s == "Account Number":
				st.AccountNumber = value
			case s == "Money In":
				st.TotalIn, err = parsePounds(value, false)
				haveIn = err == nil
			case s == "Money Out":
				st.TotalOut, err = parsePounds(value, false)
				haveOut = err == nil
			case strings.HasPrefix(s, "Balance on "):
				var b balanceLine
				if b.date, err = time.Parse(longDate, strings.TrimPrefix(s, "Balance on ")); err == nil {
					b.amount, err = parsePounds(value, true)
				}
				if err != nil {
					return fmt.Errorf("%w: bad balance line", ErrFormat)
				}
				balances = append(balances, b)
			case !havePeriod:
				if a, b, ok := parsePeriod(s); ok {
					st.PeriodStart, st.PeriodEnd, havePeriod = a, b, true
				}
			}
		}
	}
	switch {
	case !havePeriod:
		return fmt.Errorf("%w: missing period", ErrFormat)
	case st.SortCode == "" || st.AccountNumber == "":
		return fmt.Errorf("%w: missing account identifiers", ErrFormat)
	case !haveIn || !haveOut:
		return fmt.Errorf("%w: missing totals", ErrFormat)
	}
	var haveStart, haveEnd bool
	for _, b := range balances {
		switch {
		case b.date.Equal(st.PeriodStart) && !haveStart:
			st.StartBalance, haveStart = b.amount, true
		case b.date.Equal(st.PeriodEnd) && !haveEnd:
			st.ClosingBalance, haveEnd = b.amount, true
		}
	}
	if !haveStart || !haveEnd {
		return fmt.Errorf("%w: missing balance lines", ErrFormat)
	}
	return nil
}

func parsePeriod(s string) (start, end time.Time, ok bool) {
	from, to, found := strings.Cut(s, " to ")
	if !found {
		return start, end, false
	}
	start, err1 := time.Parse(longDate, from)
	end, err2 := time.Parse(longDate, to)
	return start, end, err1 == nil && err2 == nil && !end.Before(start)
}

// parseTable reads the rows. The table is open from a header row until the
// page ends, the "Transaction types" line appears, or a line that does not
// belong to the table (such as a page footer) is met; a repeated header on a
// later page opens it again. A row dropped by that rule breaks the checks.
//
// A line with text only in the description column continues the current row
// when it is on the same page as that row's last line and no more than
// maxContinuationGap points below it. Any other description-only line inside
// the table is an error, not furniture to skip: it may be a continuation in a
// changed layout, and dropping or misattaching it would silently change a
// row's description and so its key.
func parseTable(lines []line) ([]Row, error) {
	var (
		rows    []Row
		cols    [numCols]float64
		open    bool
		page    int
		current *Row
		// lastPage and lastY locate the current row's last line.
		lastPage int
		lastY    float64
	)
	for _, l := range lines {
		if l.page != page {
			page, open = l.page, false
		}
		if h, ok := headerColumns(l); ok {
			cols, open = h, true
			continue
		}
		if !open {
			continue
		}
		if hasText(l, endMarker) {
			break
		}
		cells, ok := assignColumns(l, cols)
		if !ok {
			open = false
			continue
		}
		if cells[colDate] == "" {
			if !onlyDescription(cells) {
				open = false
				continue
			}
			if current == nil {
				return nil, fmt.Errorf("%w: text before the first row", ErrFormat)
			}
			if gap := lastY - l.y; l.page != lastPage || gap <= 0 || gap > maxContinuationGap {
				return nil, fmt.Errorf("%w: description line apart from any row (after row %d)", ErrFormat, len(rows))
			}
			current.Description += " " + cells[colDescription]
			lastY = l.y
			continue
		}
		date, err := time.Parse(shortDate, cells[colDate])
		if err != nil {
			open = false
			continue
		}
		row, err := tableRow(date, cells)
		if err != nil {
			return nil, fmt.Errorf("%w (row %d)", err, len(rows)+1)
		}
		rows = append(rows, row)
		current = &rows[len(rows)-1]
		lastPage, lastY = l.page, l.y
	}
	return rows, nil
}

func hasText(l line, s string) bool {
	for _, it := range l.items {
		if strings.TrimSpace(it.S) == s {
			return true
		}
	}
	return false
}

// assignColumns puts each text in the last column whose header starts at or
// left of it. That also places a right-aligned amount: its left edge lies at
// or right of its own header's and left of the next one's. It fails if a text
// lies left of the first column.
func assignColumns(l line, cols [numCols]float64) ([numCols]string, bool) {
	var cells [numCols]string
	for _, it := range l.items {
		c := -1
		for i := range cols {
			if it.X >= cols[i]-colTolerance {
				c = i
			}
		}
		if c < 0 {
			return cells, false
		}
		cells[c] += it.S //nolint:gosec // c is checked to lie in [0, numCols)
	}
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells, true
}

func onlyDescription(cells [numCols]string) bool {
	for i, c := range cells {
		if i != colDescription && c != "" {
			return false
		}
	}
	return cells[colDescription] != ""
}

func tableRow(date time.Time, cells [numCols]string) (Row, error) {
	row := Row{Date: date, Description: cells[colDescription], Type: cells[colType]}
	var err error
	if cells[colIn] != "" {
		if row.MoneyIn, err = parsePounds(cells[colIn], false); err != nil {
			return Row{}, err
		}
	}
	if cells[colOut] != "" {
		if row.MoneyOut, err = parsePounds(cells[colOut], false); err != nil {
			return Row{}, err
		}
	}
	if (cells[colIn] == "") == (cells[colOut] == "") {
		return Row{}, fmt.Errorf("%w: row needs exactly one of money in and money out", ErrFormat)
	}
	if row.Balance, err = parsePounds(cells[colBalance], true); err != nil {
		return Row{}, err
	}
	return row, nil
}

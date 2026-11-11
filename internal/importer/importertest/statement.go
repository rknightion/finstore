// Package importertest generates made-up statement PDFs in the structure the real Halifax
// statements have, for tests of the importer and of the ledger.
package importertest

import (
	"fmt"
	"strings"
	"time"

	"github.com/rknightion/finstore/internal/pdftext/pdftexttest"
)

// Row is one made-up statement row. Bal is the balance printed after it.
type Row struct {
	Date     string // DD Mon YY
	Desc     string
	Cont     string // optional continuation line under the description
	Type     string
	In, Out  int64
	Bal      int64
	PageEnds bool // the next row starts on a new page, under a repeated header
}

// Statement is a made-up statement in the structure the real ones have: a
// header block, a table whose cells share a y coordinate, right-aligned
// amounts, invisible 0.05-point labels and a footer on every page.
type Statement struct {
	SortCode, Account string
	Start, End        time.Time
	TotalIn, TotalOut int64
	StartBal, Closing int64
	Rows              []Row
	PoundInCells      bool
}

var (
	headerX = []float64{57, 122.2, 270.8, 322.6, 393.3, 490}
	headers = []string{"Date", "Description", "Type", "Money In (£)", "Money Out (£)", "Balance (£)"}
	// Right edges of the amount columns, in points.
	rightEdge = map[int]float64{3: 385, 4: 480, 5: 545}
)

// New builds a consistent statement: balances run on from opening, and the
// totals and closing balance follow from the rows.
func New(opening int64, rows []Row) *Statement {
	g := &Statement{
		SortCode: "00-00-00", Account: "00000000",
		Start: time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2030, time.January, 31, 0, 0, 0, 0, time.UTC),
		Rows:  rows, StartBal: opening,
	}
	bal := opening
	for i := range g.Rows {
		bal += g.Rows[i].In - g.Rows[i].Out
		g.Rows[i].Bal = bal
		g.TotalIn += g.Rows[i].In
		g.TotalOut += g.Rows[i].Out
	}
	g.Closing = bal
	return g
}

func pounds(n int64) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	whole := fmt.Sprint(n / 100)
	var groups []string
	for len(whole) > 3 {
		groups = append([]string{whole[len(whole)-3:]}, groups...)
		whole = whole[:len(whole)-3]
	}
	groups = append([]string{whole}, groups...)
	return fmt.Sprintf("%s%s.%02d", sign, strings.Join(groups, ","), n%100)
}

// PDF renders the statement as a PDF file.
func (g *Statement) PDF() []byte {
	const (
		firstRowY = 504.2
		rowGap    = 25.0
		contGap   = 12.0
	)
	var pages [][]string
	var cur []string
	add := func(x, y, size float64, s string) { cur = append(cur, pdftexttest.Text(x, y, size, s)) }
	furniture := func() {
		add(305.5, 809, 9, "01 February 2030")
		add(60, 790, 0.05, "decoy label")
		cur = append(cur, "/Xf1 Do\n")
		add(57, 73, 6.2, "Made-up footer line one")
		add(209.3, 73, 6.2, " 000 000 0000")
		add(57, 45.8, 6.2, "Made-up footer line two")
	}
	header := func(y float64) {
		for i, h := range headers {
			add(headerX[i], y, 9, h)
		}
	}
	amount := func(col int, y float64, s string) {
		if g.PoundInCells {
			s = "£" + s
		}
		add(rightEdge[col]-4.5*float64(len(s)), y, 9, s)
	}

	furniture()
	add(57, 756, 8, "Made-up account holder")
	add(305.5, 733, 9, "Sort Code")
	add(392.5, 733, 9, g.SortCode)
	add(305.5, 721, 9, "Account Number")
	add(392.5, 721, 9, g.Account)
	add(57, 657, 12, "MADE-UP NAME")
	add(317.2, 657, 12, g.Start.Format("02 January 2006")+" to "+g.End.Format("02 January 2006"))
	add(57, 616.8, 9, "Money In")
	add(258.5, 616.8, 9, "£"+pounds(g.TotalIn))
	add(304, 616.8, 9, "Balance on "+g.Start.Format("02 January 2006"))
	add(497.9, 616.8, 9, "£"+pounds(g.StartBal))
	add(57, 599.7, 9, "Money Out")
	add(250.9, 599.7, 9, "£"+pounds(g.TotalOut))
	add(304, 599.7, 9, "Balance on "+g.End.Format("02 January 2006"))
	add(497.9, 599.7, 9, "£"+pounds(g.Closing))
	add(57, 561.8, 12, "Your Transactions")
	header(529.2)

	y := firstRowY
	for _, r := range g.Rows {
		add(headerX[0], y, 9, r.Date)
		add(headerX[1], y, 9, r.Desc)
		add(headerX[2], y, 9, r.Type)
		if r.In != 0 {
			amount(3, y, pounds(r.In))
		}
		if r.Out != 0 {
			amount(4, y, pounds(r.Out))
		}
		amount(5, y, pounds(r.Bal))
		if r.Cont != "" {
			y -= contGap
			add(headerX[1], y, 9, r.Cont)
		}
		y -= rowGap
		if r.PageEnds {
			pages = append(pages, cur)
			cur = nil
			furniture()
			header(700)
			y = 675
		}
	}
	add(57, y-10, 12, "Transaction types")
	add(59, y-40, 9, "BGC")
	add(85.4, y-40, 9, "Bank Giro Credit")
	pages = append(pages, cur)

	contents := make([][]byte, len(pages))
	for i, p := range pages {
		contents[i] = []byte(strings.Join(p, ""))
	}
	return pdftexttest.Build(contents, pdftexttest.Options{})
}

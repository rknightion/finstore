package importer_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/importer"
)

func baseRows() []genRow {
	return []genRow{
		{Date: "03 Jan 30", Desc: "MADE UP PAYROLL", Type: "FPI", In: 123456},
		{Date: "04 Jan 30", Desc: "EXAMPLE SHOP (TOWN) £5 OFF", Type: "DEB", Out: 1250},
		{Date: "05 Jan 30", Desc: "FIXTURE UTILITY", Type: "DD", Out: 9999},
		{Date: "09 Jan 30", Desc: "SAMPLE CO", Type: "BGC", In: 100000},
	}
}

func d(day int) time.Time { return time.Date(2030, time.January, day, 0, 0, 0, 0, time.UTC) }

func parseGen(t *testing.T, g *genStatement) *importer.Statement {
	t.Helper()
	st, err := importer.ParseHalifaxPDF(g.PDF())
	if err != nil {
		t.Fatalf("ParseHalifaxPDF: %v", err)
	}
	return st
}

func assertRows(t *testing.T, st *importer.Statement, g *genStatement) {
	t.Helper()
	if len(st.Rows) != len(g.Rows) {
		t.Fatalf("got %d rows, want %d", len(st.Rows), len(g.Rows))
	}
	for i, w := range g.Rows {
		got := st.Rows[i]
		date, _ := time.Parse("02 Jan 06", w.Date)
		desc := w.Desc
		if w.Cont != "" {
			desc += " " + w.Cont
		}
		if !got.Date.Equal(date) || got.Description != desc || got.Type != w.Type ||
			got.MoneyIn != w.In || got.MoneyOut != w.Out || got.Balance != w.Bal || got.Key == "" {
			t.Errorf("row %d: got %+v, want %+v", i+1, got, w)
		}
	}
}

func TestParseHalifaxPDF(t *testing.T) {
	g := newGen(50000, baseRows())
	st := parseGen(t, g)
	assertRows(t, st, g)
	if st.Format != importer.FormatHalifaxPDF || st.SortCode != g.SortCode || st.AccountNumber != g.Account {
		t.Errorf("identifiers: %+v", st)
	}
	if !st.PeriodStart.Equal(g.Start) || !st.PeriodEnd.Equal(g.End) {
		t.Errorf("period: %v to %v", st.PeriodStart, st.PeriodEnd)
	}
	if st.TotalIn != g.TotalIn || st.TotalOut != g.TotalOut || st.ClosingBalance != g.Closing || st.StartBalance != 50000 {
		t.Errorf("header amounts: %+v", st)
	}
	if err := importer.Check(st); err != nil {
		t.Errorf("Check: %v", err)
	}
	if got, want := importer.OpeningBalance(st), int64(50000); got != want {
		t.Errorf("OpeningBalance = %d, want %d", got, want)
	}
	// The first row's amount is 1,234.56: thousands commas parse to exact pence.
	if st.Rows[0].MoneyIn != 123456 {
		t.Errorf("thousands comma amount: %d", st.Rows[0].MoneyIn)
	}
}

func TestParseHalifaxPDFPoundSignInCells(t *testing.T) {
	g := newGen(50000, baseRows())
	g.PoundInCells = true
	st := parseGen(t, g)
	assertRows(t, st, g)
}

func TestParseHalifaxPDFNegativeBalance(t *testing.T) {
	g := newGen(1000, []genRow{{Date: "03 Jan 30", Desc: "OVER", Type: "DEB", Out: 5000}})
	st := parseGen(t, g)
	assertRows(t, st, g)
	if st.Rows[0].Balance != -4000 {
		t.Errorf("balance = %d", st.Rows[0].Balance)
	}
	if err := importer.Check(st); err != nil {
		t.Error(err)
	}
}

func TestParseHalifaxPDFContinuation(t *testing.T) {
	rows := baseRows()
	rows[1].Cont = "SECOND LINE OF TEXT"
	rows[2].Cont = "ANOTHER"
	g := newGen(50000, rows)
	st := parseGen(t, g)
	assertRows(t, st, g)
	if st.Rows[1].Description != "EXAMPLE SHOP (TOWN) £5 OFF SECOND LINE OF TEXT" {
		t.Errorf("description = %q", st.Rows[1].Description)
	}
	if err := importer.Check(st); err != nil {
		t.Error(err)
	}
}

func TestParseHalifaxPDFTwoPages(t *testing.T) {
	rows := baseRows()
	rows[1].PageEnds = true
	rows[2].Cont = "CONTINUES ON PAGE TWO"
	g := newGen(50000, rows)
	st := parseGen(t, g)
	assertRows(t, st, g)
	if err := importer.Check(st); err != nil {
		t.Error(err)
	}
}

func TestParseHalifaxPDFRejects(t *testing.T) {
	good := newGen(50000, baseRows()).PDF()
	cases := map[string][]byte{
		"empty":     nil,
		"not a pdf": []byte("not a pdf"),
	}
	// Any cut before the last content stream ends loses table rows or the page.
	for n := 0; n < bytes.LastIndex(good, []byte("endstream")); n += 53 {
		cases[fmt.Sprintf("cut at %d", n)] = good[:n]
	}
	for name, in := range cases {
		if st, err := importer.ParseHalifaxPDF(in); err == nil {
			t.Errorf("%s: parsed %+v, want an error", name, st)
		}
	}
}

func TestParseHalifaxPDFMissingFields(t *testing.T) {
	g := newGen(50000, baseRows())
	g.SortCode = ""
	if _, err := importer.ParseHalifaxPDF(g.PDF()); !errors.Is(err, importer.ErrFormat) {
		t.Errorf("got %v, want ErrFormat", err)
	}
}

func TestCheckFailures(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(g *genStatement)
		check  importer.CheckName
		row    int
	}{
		{"totals in", func(g *genStatement) { g.TotalIn += 1 }, importer.CheckTotals, 0},
		{"totals out", func(g *genStatement) { g.TotalOut -= 100 }, importer.CheckTotals, 0},
		{"running balance", func(g *genStatement) { g.Rows[2].Bal += 1; g.Rows[3].Bal += 1; g.Closing += 1 }, importer.CheckRunningBalance, 3},
		{"closing balance", func(g *genStatement) { g.Closing += 1 }, importer.CheckClosingBalance, 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := newGen(50000, baseRows())
			tc.break_(g)
			st := parseGen(t, g)
			err := importer.Check(st)
			var ce *importer.CheckError
			if !errors.As(err, &ce) {
				t.Fatalf("got %v, want a CheckError", err)
			}
			if ce.Check != tc.check || ce.Row != tc.row {
				t.Errorf("got %s row %d, want %s row %d", ce.Check, ce.Row, tc.check, tc.row)
			}
			for _, banned := range []string{"MADE UP", "1234", "123456", "1,234"} {
				if strings.Contains(err.Error(), banned) {
					t.Errorf("error %q carries statement content", err)
				}
			}
		})
	}
}

func TestKeys(t *testing.T) {
	// Two identical rows in one statement.
	rows := []genRow{
		{Date: "03 Jan 30", Desc: "SAME", Type: "DEB", Out: 100},
		{Date: "03 Jan 30", Desc: "SAME", Type: "DEB", Out: 100},
		{Date: "04 Jan 30", Desc: "OTHER", Type: "DEB", Out: 200},
	}
	a := parseGen(t, newGen(10000, rows))
	if a.Rows[0].Key == a.Rows[1].Key {
		t.Fatal("identical rows share a key")
	}
	// Rows 1 and 2 differ in balance, so they are not identical in all six fields.
	if !strings.HasSuffix(a.Rows[0].Key, "#1") {
		t.Errorf("key %q", a.Rows[0].Key)
	}

	// A true duplicate: same balance too (a zero-sum pair is impossible, so build it directly).
	st := &importer.Statement{Format: importer.FormatManualCSV, Rows: []importer.Row{
		{Date: d(3), Type: "X", Description: "SAME", MoneyOut: 100, Balance: 900},
		{Date: d(3), Type: "X", Description: "SAME", MoneyOut: 100, Balance: 900},
	}}
	importer.Keys(st)
	if st.Rows[0].Key == st.Rows[1].Key || !strings.HasSuffix(st.Rows[0].Key, "#1") || !strings.HasSuffix(st.Rows[1].Key, "#2") {
		t.Errorf("duplicate keys: %q %q", st.Rows[0].Key, st.Rows[1].Key)
	}
	if i := strings.IndexByte(st.Rows[0].Key, '#'); i != 64 {
		t.Errorf("hash part is %d characters, want 64 hex", i)
	}
}

func TestKeysStableAcrossOverlappingStatements(t *testing.T) {
	all := []genRow{
		{Date: "03 Jan 30", Desc: "ONE", Type: "DEB", Out: 100},
		{Date: "04 Jan 30", Desc: "TWO", Type: "FPI", In: 500},
		{Date: "05 Jan 30", Desc: "THREE", Type: "DD", Out: 300},
		{Date: "06 Jan 30", Desc: "FOUR", Type: "DD", Out: 50},
	}
	full := parseGen(t, newGen(10000, append([]genRow(nil), all...)))
	// A later part-month statement starting at row 2 with the opening balance after row 1.
	part := parseGen(t, newGen(10000-100, append([]genRow(nil), all[1:]...)))
	for i, r := range part.Rows {
		if r.Key != full.Rows[i+1].Key {
			t.Errorf("row %d: key differs between overlapping statements", i+1)
		}
	}
	if importer.OpeningBalance(part) != full.Rows[0].Balance {
		t.Error("opening balance of the later statement is not the earlier closing balance")
	}
	if full.Rows[0].Key == full.Rows[1].Key {
		t.Error("distinct rows share a key")
	}
}

func TestParseManualCSV(t *testing.T) {
	csv := "date,type,amount,balance_after,description\n" +
		"2030-01-03,PAY,\"1,732.00\",\"-98,268.00\",\"Payment, with comma\"\n" +
		"2030-01-31,INT,-215.75,-98483.75,Interest\n" +
		"2030-02-03,PAY,£1732.00,-96751.75,Payment\n"
	st, err := importer.ParseManualCSV([]byte(csv))
	if err != nil {
		t.Fatal(err)
	}
	if st.Format != importer.FormatManualCSV || len(st.Rows) != 3 {
		t.Fatalf("got %+v", st)
	}
	want := []importer.Row{
		{Date: d(3), Type: "PAY", MoneyIn: 173200, Balance: -9826800, Description: "Payment, with comma"},
		{Date: d(31), Type: "INT", MoneyOut: 21575, Balance: -9848375, Description: "Interest"},
		{Date: time.Date(2030, time.February, 3, 0, 0, 0, 0, time.UTC), Type: "PAY", MoneyIn: 173200, Balance: -9675175, Description: "Payment"},
	}
	for i, w := range want {
		got := st.Rows[i]
		got.Key = ""
		if got != w {
			t.Errorf("row %d: got %+v, want %+v", i+1, got, w)
		}
	}
	if err := importer.Check(st); err != nil {
		t.Error(err)
	}
	if got := importer.OpeningBalance(st); got != -10000000 {
		t.Errorf("OpeningBalance = %d", got)
	}
}

func TestParseManualCSVBrokenBalance(t *testing.T) {
	csv := "date,type,amount,balance_after,description\n" +
		"2030-01-03,PAY,100.00,100.00,A\n" +
		"2030-01-04,PAY,100.00,200.00,B\n" +
		"2030-01-05,PAY,100.00,301.00,C\n"
	st, err := importer.ParseManualCSV([]byte(csv))
	if err != nil {
		t.Fatal(err)
	}
	var ce *importer.CheckError
	if err := importer.Check(st); !errors.As(err, &ce) || ce.Check != importer.CheckRunningBalance || ce.Row != 3 {
		t.Fatalf("got %v", err)
	}
}

func TestParseManualCSVRejects(t *testing.T) {
	const h = "date,type,amount,balance_after,description\n"
	for name, in := range map[string]string{
		"empty":          "",
		"wrong header":   "date,amount\n2030-01-03,1.00\n",
		"header only":    h,
		"bad date":       h + "03/01/2030,PAY,1.00,1.00,A\n",
		"bad amount":     h + "2030-01-03,PAY,1,1.00,A\n",
		"one decimal":    h + "2030-01-03,PAY,1.5,1.50,A\n",
		"bad grouping":   h + "2030-01-03,PAY,\"1,00.00\",1.00,A\n",
		"bad balance":    h + "2030-01-03,PAY,1.00,abc,A\n",
		"short record":   h + "2030-01-03,PAY,1.00\n",
		"unclosed quote": h + "2030-01-03,PAY,1.00,1.00,\"A\n",
		"binary":         "\x00\x01\x02",
	} {
		if _, err := importer.ParseManualCSV([]byte(in)); !errors.Is(err, importer.ErrFormat) {
			t.Errorf("%s: got %v, want ErrFormat", name, err)
		}
	}
}

func TestCheckUnknownFormat(t *testing.T) {
	if err := importer.Check(&importer.Statement{}); !errors.Is(err, importer.ErrFormat) {
		t.Errorf("got %v", err)
	}
}

func FuzzParsers(f *testing.F) {
	f.Add(newGen(100, baseRows()).PDF())
	f.Add([]byte("date,type,amount,balance_after,description\n2030-01-03,PAY,1.00,1.00,A\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if st, err := importer.ParseHalifaxPDF(data); err == nil {
			_ = importer.Check(st)
		}
		if st, err := importer.ParseManualCSV(data); err == nil {
			_ = importer.Check(st)
		}
	})
}

package importer_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/rknightion/finstore/internal/importer"
	"github.com/rknightion/finstore/internal/ledger"
)

func TestNormaliserGivesSignedSettledRowsAndStatementBounds(t *testing.T) {
	g := newGen(50000, baseRows())
	res, err := importer.Normaliser{}.NormaliseFile(ledger.File{AccountSlug: "bank-current", Format: "halifax-pdf", Body: g.PDF()})
	if err != nil {
		t.Fatalf("NormaliseFile: %v", err)
	}
	st := parseGen(t, g)
	if len(res.Txns) != len(g.Rows) || len(res.Balances) != len(g.Rows) {
		t.Fatalf("%d txns, %d balances, want %d", len(res.Txns), len(res.Balances), len(g.Rows))
	}
	for i, w := range g.Rows {
		tx, b := res.Txns[i], res.Balances[i]
		if tx.AmountMinor != w.In-w.Out || tx.Status != ledger.StatusSettled || tx.SourceKey != st.Rows[i].Key ||
			tx.Scheme != w.Type || tx.Description != w.Desc || !tx.TxnDate.Equal(st.Rows[i].Date) {
			t.Errorf("row %d: %+v", i+1, tx)
		}
		if string(tx.Content) != string(tx.HashInput) || !json.Valid(tx.Content) {
			t.Errorf("row %d: content %s, hash input %s", i+1, tx.Content, tx.HashInput)
		}
		if b.BalanceMinor != w.Bal || !b.OnDate.Equal(st.Rows[i].Date) {
			t.Errorf("row %d balance %+v", i+1, b)
		}
	}
	// A PDF covers its printed period, not its first and last row dates.
	if res.OpeningMinor != 50000 || res.ClosingMinor != g.Closing || !res.FirstDate.Equal(d(1)) || !res.LastDate.Equal(d(31)) {
		t.Fatalf("bounds: opening %d closing %d first %s last %s", res.OpeningMinor, res.ClosingMinor, res.FirstDate, res.LastDate)
	}
	if res.AccountRef != "000000 00000000" {
		t.Fatalf("account ref %q", res.AccountRef)
	}
}

func TestNormaliserAccountRefIsDigitsOnly(t *testing.T) {
	g := newGen(50000, baseRows())
	g.SortCode, g.Account = "12-34-56", "8765 4321"
	res, err := importer.Normaliser{}.NormaliseFile(ledger.File{Format: "halifax-pdf", Body: g.PDF()})
	if err != nil {
		t.Fatal(err)
	}
	if res.AccountRef != "123456 87654321" {
		t.Fatalf("account ref %q", res.AccountRef)
	}
}

func TestNormaliserContentIsCanonical(t *testing.T) {
	csv := []byte("date,type,amount,balance_after,description\n2030-01-03,DEB,-5.00,95.00,MADE UP\n")
	a, err := importer.Normaliser{}.NormaliseFile(ledger.File{Format: "manual-csv", Body: csv})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"balance":9500,"date":"2030-01-03","description":"MADE UP","money_in":0,"money_out":500,"type":"DEB"}`
	if string(a.Txns[0].HashInput) != want {
		t.Fatalf("hash input %s, want %s", a.Txns[0].HashInput, want)
	}
	if a.OpeningMinor != 10000 || a.ClosingMinor != 9500 || a.AccountRef != "" ||
		!a.FirstDate.Equal(d(3)) || !a.LastDate.Equal(d(3)) {
		t.Fatalf("opening %d closing %d", a.OpeningMinor, a.ClosingMinor)
	}
}

func TestNormaliserRefusesAStatementFailingItsChecks(t *testing.T) {
	g := newGen(50000, baseRows())
	g.Closing++
	_, err := importer.Normaliser{}.NormaliseFile(ledger.File{Format: "halifax-pdf", Body: g.PDF()})
	var ce *importer.CheckError
	if !errors.As(err, &ce) || ce.Check != importer.CheckClosingBalance {
		t.Fatalf("want closing-balance check error, got %v", err)
	}
	if _, err := (importer.Normaliser{}).NormaliseFile(ledger.File{Format: "other", Body: []byte("x")}); !errors.Is(err, importer.ErrFormat) {
		t.Fatalf("unknown format: %v", err)
	}
}

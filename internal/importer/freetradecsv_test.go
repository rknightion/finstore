package importer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/importer"
	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/store"
)

// Every value in this file is made up. The fixtures are written by hand from the documented shape
// of a Freetrade activity statement and are not derived from any real export.

const (
	ftHeader = "Value Date,Type,Details,Currency,Direction,Value,Account Type"
	ftSIPP   = "Self-Invested Personal Pension (SIPP)"
)

// ftRow is one row as Freetrade prints it: Details quoted, the other columns bare.
func ftRow(date, typ, details, currency, direction, value string) string {
	return ftRowOf(date, typ, details, currency, direction, value, ftSIPP)
}

func ftRowOf(date, typ, details, currency, direction, value, account string) string {
	quoted := "\"" + strings.ReplaceAll(details, "\"", "\"\"") + "\""
	return strings.Join([]string{date, typ, quoted, currency, direction, value, account}, ",")
}

// ftFile is an export as Freetrade writes it: LF line ends, no byte order mark and no line end
// after the last row.
func ftFile(rows ...string) []byte {
	return []byte(strings.Join(append([]string{ftHeader}, rows...), "\n"))
}

func ftResult(t *testing.T, body []byte) ledger.InvestmentResult {
	t.Helper()
	res, err := importer.FreetradeActivityNormaliser{}.NormaliseInvestmentFile(ledger.File{Body: body}, time.UTC)
	if err != nil {
		t.Fatalf("NormaliseInvestmentFile: %v", err)
	}
	return res
}

// Made-up rows, newest first, one of each shape.
var (
	ftBuyUSD   = ftRow("05/03/2030", "BUY", "12.5 Example Corp (US0000000001) @ USD 10.00000 (@ 1 GBP = 1.25000 USD) on 01-Mar-2030 14:30:00 (Requested 100 GBP)", "GBP", "CASH_OUT", "-100.00")
	ftBuyGBP   = ftRow("05/03/2030", "BUY", "4 Example Trust plc (GB0000000005) @ GBP 12.50000 on 03-Mar-2030 09:15:00 (Requested 50 GBP)", "GBP", "CASH_OUT", "-50")
	ftSell     = ftRow("04/03/2030", "SELL", "2.12345678 Example Holding AG (ADR) (US0000000002) @ USD 20.00000 (@ 1 USD = 1.25000 GBP) on 28-Feb-2030 15:00:00", "GBP", "CASH_IN", "33.97")
	ftFXFee    = ftRow("04/03/2030", "FX_FEE", "0.70 GBP fx fee taken for 12.5 Example Corp (US0000000001) on 01-Mar-2030 14:30:00", "GBP", "CASH_OUT", "-0.70")
	ftStamp    = ftRow("04/03/2030", "STAMP_DUTY", "0.35 GBP stamp duty paid", "GBP", "CASH_OUT", "-0.35")
	ftInterest = ftRow("03/03/2030", "INTEREST", "Interest income of 0.37 GBP", "GBP", "CASH_IN", "0.37")
	ftIncome   = ftRow("03/03/2030", "INVESTMENT_INCOME", "Investment income of 2.9 GBP", "GBP", "CASH_IN", "2.90")
	ftLending  = ftRow("02/03/2030", "SECURITIES_LENDING_INCOME", "Securities lending income of 0.17 GBP for 2030-02", "GBP", "CASH_IN", "0.17")
	ftCorpFrom = ftRow("02/03/2030", "CORPORATE_ACTION", "Corporate action of 12.34 GBP from Example plc (GB0000000004)", "GBP", "CASH_IN", "12.34")
	ftCorp     = ftRow("02/03/2030", "CORPORATE_ACTION", "Corporate action of 4.56 GBP", "GBP", "CASH_IN", "4.56")
	ftOut      = ftRow("01/03/2030", "CASH_TRANSFER", "20.00 GBP cash transfer", "GBP", "CASH_OUT", "-20.00")
	ftIn       = ftRow("01/03/2030", "CASH_TRANSFER", "1000 GBP cash transfer", "GBP", "CASH_IN", "1000")
	ftBuyEUR   = ftRow("06/03/2030", "BUY", "3 Example ETF EUR (Dis.) (IE0000000003) @ EUR 39.00000 (@ 1 GBP = 1.17000 EUR) on 04-Mar-2030 10:00:00 (Requested 100 GBP)", "EUR", "CASH_OUT", "-117.00")
)

func ftDay(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d
}

// AC1, AC2: every type maps to its documented event, with its units, cash and flow, and one event
// is stored for each row against one child account named by Account Type.
func TestFreetradeMapsEveryType(t *testing.T) {
	type want struct {
		event, units, key, name, price string
		order, settle                  string
		gross, cash, flow              int64
	}
	cases := []struct {
		name string
		row  string
		want want
	}{
		{"buy priced in dollars", ftBuyUSD, want{event: ledger.EventBuy, units: "12.5", key: "US0000000001", name: "Example Corp (US0000000001)",
			order: "2030-03-01", settle: "2030-03-05", gross: -10000, cash: -10000}},
		{"buy priced in pounds", ftBuyGBP, want{event: ledger.EventBuy, units: "4", key: "GB0000000005", name: "Example Trust plc (GB0000000005)",
			price: "12.50000", order: "2030-03-03", settle: "2030-03-05", gross: -5000, cash: -5000}},
		{"sell of a name with parentheses", ftSell, want{event: ledger.EventSell, units: "2.12345678", key: "US0000000002",
			name: "Example Holding AG (ADR) (US0000000002)", order: "2030-02-28", settle: "2030-03-04", gross: 3397, cash: 3397}},
		{"fx fee", ftFXFee, want{event: ledger.EventFee, name: "Example Corp (US0000000001)", order: "2030-03-04", gross: -70, cash: -70}},
		{"stamp duty", ftStamp, want{event: ledger.EventFee, order: "2030-03-04", gross: -35, cash: -35}},
		{"interest", ftInterest, want{event: ledger.EventInterest, order: "2030-03-03", gross: 37, cash: 37}},
		{"investment income", ftIncome, want{event: ledger.EventIncome, order: "2030-03-03", gross: 290, cash: 290}},
		{"securities lending income", ftLending, want{event: ledger.EventIncome, order: "2030-03-02", gross: 17, cash: 17}},
		{"corporate action from a company", ftCorpFrom, want{event: ledger.EventCorporateAction, name: "Example plc (GB0000000004)", order: "2030-03-02", gross: 1234, cash: 1234}},
		{"corporate action", ftCorp, want{event: ledger.EventCorporateAction, order: "2030-03-02", gross: 456, cash: 456}},
		{"cash transfer out", ftOut, want{event: ledger.EventTransferOut, order: "2030-03-01", gross: -2000, cash: -2000, flow: -2000}},
		{"cash transfer in", ftIn, want{event: ledger.EventTransferIn, order: "2030-03-01", gross: 100000, cash: 100000, flow: 100000}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := ftResult(t, ftFile(c.row))
			if res.Kind != ledger.InvestmentKindLedger || res.RowCount != 1 || len(res.Events) != 1 {
				t.Fatalf("result %+v", res)
			}
			if len(res.Accounts) != 1 || res.Accounts[0] != (ledger.InvestmentAccountRec{AccountKey: ledger.ProductSIPP, Product: ledger.ProductSIPP}) {
				t.Fatalf("accounts %+v", res.Accounts)
			}
			e, w := res.Events[0], c.want
			if e.AccountKey != ledger.ProductSIPP || e.EventType != w.event || e.Units != w.units || e.InstrumentKey != w.key ||
				e.InstrumentName != w.name || e.GrossMinor != w.gross || e.CashMinor != w.cash || e.FlowMinor != w.flow || e.Currency != "GBP" {
				t.Fatalf("event %+v, want %+v", e, w)
			}
			if !e.OrderDate.Equal(ftDay(w.order)) {
				t.Fatalf("order date %v, want %s", e.OrderDate, w.order)
			}
			if (w.settle == "") != e.SettleDate.IsZero() || (w.settle != "" && !e.SettleDate.Equal(ftDay(w.settle))) {
				t.Fatalf("settle date %v, want %q", e.SettleDate, w.settle)
			}
			wantUnit := ""
			if w.price != "" {
				wantUnit = ledger.PriceUnitGBP
			}
			if e.Price != w.price || e.PriceUnit != wantUnit {
				t.Fatalf("price %q %q, want %q", e.Price, e.PriceUnit, w.price)
			}
			if e.ProviderType == "" || !strings.HasSuffix(e.SourceKey, "#1") || len(e.HashInput) == 0 || !json.Valid(e.Content) {
				t.Fatalf("event identity %+v", e)
			}
		})
	}

	// The whole file at once: one event per row, one account.
	rows := []string{ftBuyEUR, ftBuyUSD, ftBuyGBP, ftSell, ftFXFee, ftStamp, ftInterest, ftIncome, ftLending, ftCorpFrom, ftCorp, ftOut, ftIn}
	res := ftResult(t, ftFile(rows...))
	if res.RowCount != len(rows) || len(res.Events) != len(rows) || len(res.Accounts) != 1 {
		t.Fatalf("%d rows, %d events, %d accounts", res.RowCount, len(res.Events), len(res.Accounts))
	}
}

// AC4: a buy printed in another currency keeps its printed amount and currency, and its sterling
// cash is the printed amount divided by the printed rate, rounded to the penny half away from zero.
func TestFreetradeNonSterlingBuyDerivesSterlingCash(t *testing.T) {
	cases := []struct {
		name, details, value string
		gross, cash          int64
	}{
		// 117.00 / 1.17 = 100.00 exactly.
		{"exact", "3 Example ETF EUR (Dis.) (IE0000000003) @ EUR 39.00000 (@ 1 GBP = 1.17000 EUR) on 04-Mar-2030 10:00:00 (Requested 100 GBP)", "-117.00", -11700, -10000},
		// 100.00 / 1.17 = 85.4700..., down to 85.47.
		{"rounded down", "2.5641 Example ETF EUR (Dis.) (IE0000000003) @ EUR 39.00000 (@ 1 GBP = 1.17000 EUR) on 04-Mar-2030 10:00:00", "-100.00", -10000, -8547},
		// 50.00 / 1.16 = 43.1034..., down to 43.10.
		{"another rate", "1 Example ETF EUR (Dis.) (IE0000000003) @ EUR 50.00000 (@ 1 GBP = 1.16000 EUR) on 04-Mar-2030 10:00:00", "-50", -5000, -4310},
		// 0.25 / 2 = 0.125, a tie: away from zero, to 0.13.
		{"half a penny", "0.1 Example ETF EUR (Dis.) (IE0000000003) @ EUR 2.50000 (@ 1 GBP = 2.00000 EUR) on 04-Mar-2030 10:00:00", "-0.25", -25, -13},
		// 0.8 printed with one decimal: 0.80 / 1.5 = 0.5333..., to 0.53.
		{"one decimal", "0.2 Example ETF EUR (Dis.) (IE0000000003) @ EUR 4.00000 (@ 1 GBP = 1.50000 EUR) on 04-Mar-2030 10:00:00", "-0.8", -80, -53},
		// 1.00 / 1.6 = 0.625, a tie: away from zero, to 0.63.
		{"another tie", "0.25 Example ETF EUR (Dis.) (IE0000000003) @ EUR 4.00000 (@ 1 GBP = 1.6 EUR) on 04-Mar-2030 10:00:00", "-1", -100, -63},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := ftResult(t, ftFile(ftRow("06/03/2030", "BUY", c.details, "EUR", "CASH_OUT", c.value)))
			e := res.Events[0]
			if e.EventType != ledger.EventBuy || e.Currency != "EUR" || e.GrossMinor != c.gross || e.CashMinor != c.cash || e.FlowMinor != 0 ||
				e.InstrumentKey != "IE0000000003" || e.Price != "" || e.PriceUnit != "" {
				t.Fatalf("event %+v, want gross %d EUR and cash %d", e, c.gross, c.cash)
			}
		})
	}
}

// AC3: identical rows are separate events with keys that differ only in their position, and the
// same row gets the same key and content in every export that prints it.
func TestFreetradeIdenticalRowsAndOverlappingExports(t *testing.T) {
	res := ftResult(t, ftFile(ftIncome, ftIncome, ftInterest, ftIncome))
	keys := map[string]bool{}
	for _, e := range res.Events {
		keys[e.SourceKey] = true
	}
	if len(res.Events) != 4 || len(keys) != 4 {
		t.Fatalf("%d events with %d keys", len(res.Events), len(keys))
	}
	base, _, _ := strings.Cut(res.Events[0].SourceKey, "#")
	for i, want := range map[int]string{0: "#1", 1: "#2", 3: "#3"} {
		if res.Events[i].SourceKey != base+want {
			t.Fatalf("row %d key %q, want %q", i+1, res.Events[i].SourceKey, base+want)
		}
	}
	if !bytes.Equal(res.Events[0].HashInput, res.Events[1].HashInput) {
		t.Fatal("identical rows have different content")
	}
	if strings.Contains(res.Events[0].SourceKey, "2.9") || strings.Contains(res.Events[0].SourceKey, "INVESTMENT") {
		t.Fatal("a key holds plaintext")
	}

	// Two exports that overlap on the middle rows give those rows identical records.
	all := []string{ftBuyUSD, ftSell, ftFXFee, ftIncome, ftIncome, ftOut, ftIn}
	newer, older := ftResult(t, ftFile(all[:5]...)), ftResult(t, ftFile(all[1:]...))
	byKey := map[string]ledger.InvestmentEventRec{}
	for _, e := range newer.Events {
		byKey[e.SourceKey] = e
	}
	shared := 0
	for _, e := range older.Events {
		if n, ok := byKey[e.SourceKey]; ok {
			shared++
			if !bytes.Equal(n.HashInput, e.HashInput) || n.CashMinor != e.CashMinor {
				t.Fatalf("row %s differs between exports", e.SourceKey)
			}
		}
	}
	if shared != 4 {
		t.Fatalf("%d rows shared, want 4", shared)
	}
}

// A byte order mark, CRLF line ends and a final line end change nothing.
func TestFreetradeLineEndsAndByteOrderMark(t *testing.T) {
	rows := []string{ftBuyUSD, ftIncome, ftIn}
	want := ftResult(t, ftFile(rows...))
	lf := string(ftFile(rows...))
	for name, body := range map[string]string{
		"final LF":      lf + "\n",
		"CRLF":          strings.ReplaceAll(lf, "\n", "\r\n"),
		"CRLF and last": strings.ReplaceAll(lf, "\n", "\r\n") + "\r\n",
		"BOM":           "\xef\xbb\xbf" + lf,
	} {
		got := ftResult(t, []byte(body))
		if len(got.Events) != len(want.Events) {
			t.Fatalf("%s: %d events", name, len(got.Events))
		}
		for i := range got.Events {
			if got.Events[i].SourceKey != want.Events[i].SourceKey || !bytes.Equal(got.Events[i].HashInput, want.Events[i].HashInput) {
				t.Fatalf("%s: row %d differs", name, i+1)
			}
		}
	}
}

// A header with no rows is a valid export of a quiet period.
func TestFreetradeEmptyExportIsValid(t *testing.T) {
	for _, body := range []string{ftHeader, ftHeader + "\n", ftHeader + "\r\n"} {
		res := ftResult(t, []byte(body))
		if res.Kind != ledger.InvestmentKindLedger || res.RowCount != 0 || len(res.Events) != 0 || len(res.Accounts) != 0 {
			t.Fatalf("result %+v", res)
		}
	}
}

// A sterling price is kept only when units times price agrees with the printed value; a price in
// another currency, or one that does not agree, stays in the row's content alone.
func TestFreetradePriceIsKeptOnlyWhenItAgreesWithTheValue(t *testing.T) {
	agree := ftResult(t, ftFile(ftBuyGBP)).Events[0]
	if agree.Price != "12.50000" || agree.PriceUnit != ledger.PriceUnitGBP {
		t.Fatalf("agreeing price %q %q", agree.Price, agree.PriceUnit)
	}
	// 4 units at 1250.00000 is 5000.00, not 50.00: a price in pence printed as pounds.
	off := ftResult(t, ftFile(ftRow("05/03/2030", "BUY", "4 Example Trust plc (GB0000000005) @ GBP 1250.00000 on 03-Mar-2030 09:15:00", "GBP", "CASH_OUT", "-50.00"))).Events[0]
	if off.Price != "" || off.PriceUnit != "" || off.Units != "4" || off.CashMinor != -5000 {
		t.Fatalf("disagreeing price kept: %+v", off)
	}
	if !strings.Contains(string(off.Content), "1250.00000") {
		t.Fatal("the content lost the printed price")
	}
	// One penny off what rounding allows is not kept either: 4 at 12.50000 is 50.00 exactly.
	penny := ftResult(t, ftFile(ftRow("05/03/2030", "BUY", "4 Example Trust plc (GB0000000005) @ GBP 12.50000 on 03-Mar-2030 09:15:00", "GBP", "CASH_OUT", "-50.01"))).Events[0]
	if penny.Price != "" {
		t.Fatalf("a price a penny out was kept: %+v", penny)
	}
}

// AC2: a file that breaks any rule is refused whole, with a fixed reason and no file content.
func TestFreetradeRejectsTheWholeFile(t *testing.T) {
	good := []string{ftBuyUSD, ftIncome, ftIn}
	with := func(rows ...string) []byte { return ftFile(append(append([]string(nil), good...), rows...)...) }
	cases := []struct {
		name   string
		body   []byte
		reason string
	}{
		{"not UTF-8", with("01/03/2030,INTEREST,\"Interest income of 1 GBP\xff\",GBP,CASH_IN,1," + ftSIPP), "not UTF-8"},
		{"header", bytes.Replace(ftFile(good...), []byte("Value Date"), []byte("Date"), 1), "header"},
		{"extra header column", bytes.Replace(ftFile(good...), []byte("Account Type"), []byte("Account Type,Note"), 1), "header"},
		{"empty file", nil, "header"},
		{"short row", with("01/03/2030,INTEREST,\"Interest income of 1 GBP\",GBP,CASH_IN,1"), "row shape"},
		{"unknown type", with(ftRow("01/03/2030", "DIVIDEND", "Investment income of 1.00 GBP", "GBP", "CASH_IN", "1.00")), "unknown type"},
		{"unknown account type", with(ftRowOf("01/03/2030", "INTEREST", "Interest income of 1.00 GBP", "GBP", "CASH_IN", "1.00", "Stocks and Shares ISA")), "unknown account type"},
		{"date", with(ftRow("2030-03-01", "INTEREST", "Interest income of 1.00 GBP", "GBP", "CASH_IN", "1.00")), "date"},
		{"impossible date", with(ftRow("31/02/2030", "INTEREST", "Interest income of 1.00 GBP", "GBP", "CASH_IN", "1.00")), "date"},
		{"dates go up", ftFile(ftIn, ftBuyUSD), "order"},
		{"value", with(ftRow("01/03/2030", "INTEREST", "Interest income of 1.00 GBP", "GBP", "CASH_IN", "1.001")), "amount"},
		{"value with a pound sign", with(ftRow("01/03/2030", "INTEREST", "Interest income of 1.00 GBP", "GBP", "CASH_IN", "£1.00")), "amount"},
		{"zero value", with(ftRow("01/03/2030", "INTEREST", "Interest income of 0 GBP", "GBP", "CASH_IN", "0")), "contradictory sign"},
		{"direction against value", with(ftRow("01/03/2030", "INTEREST", "Interest income of 1.00 GBP", "GBP", "CASH_OUT", "1.00")), "contradictory sign"},
		{"unknown direction", with(ftRow("01/03/2030", "INTEREST", "Interest income of 1.00 GBP", "GBP", "IN", "1.00")), "direction"},
		{"buy as money in", with(ftRow("01/03/2030", "BUY", "1 Example Corp (US0000000001) @ GBP 1.00000 on 01-Mar-2030 10:00:00", "GBP", "CASH_IN", "1.00")), "contradictory sign"},
		{"fee as money in", with(ftRow("01/03/2030", "STAMP_DUTY", "1.00 GBP stamp duty paid", "GBP", "CASH_IN", "1.00")), "contradictory sign"},
		{"details", with(ftRow("01/03/2030", "INTEREST", "Interest of 1.00 GBP", "GBP", "CASH_IN", "1.00")), "details"},
		{"trade without an ISIN", with(ftRow("01/03/2030", "BUY", "1 Example Corp @ GBP 1.00000 on 01-Mar-2030 10:00:00", "GBP", "CASH_OUT", "-1.00")), "details"},
		{"units with nine decimals", with(ftRow("01/03/2030", "BUY", "1.123456789 Example Corp (US0000000001) @ GBP 1.00000 on 01-Mar-2030 10:00:00", "GBP", "CASH_OUT", "-1.00")), "details"},
		{"impossible execution date", with(ftRow("01/03/2030", "BUY", "1 Example Corp (US0000000001) @ GBP 1.00000 on 31-Feb-2030 10:00:00", "GBP", "CASH_OUT", "-1.00")), "date"},
		{"execution after the value date", with(ftRow("01/03/2030", "BUY", "1 Example Corp (US0000000001) @ GBP 1.00000 on 02-Mar-2030 10:00:00", "GBP", "CASH_OUT", "-1.00")), "date"},
		{"details amount", with(ftRow("01/03/2030", "INTEREST", "Interest income of 1.01 GBP", "GBP", "CASH_IN", "1.00")), "details amount"},
		{"fee amount", with(ftRow("01/03/2030", "FX_FEE", "0.51 GBP fx fee taken for 1 Example Corp (US0000000001) on 01-Mar-2030 10:00:00", "GBP", "CASH_OUT", "-0.50")), "details amount"},
		{"details currency", with(ftRow("01/03/2030", "INTEREST", "Interest income of 1.00 EUR", "GBP", "CASH_IN", "1.00")), "details amount"},
		{"non-sterling income", with(ftRow("01/03/2030", "INVESTMENT_INCOME", "Investment income of 1.00 EUR", "EUR", "CASH_IN", "1.00")), "currency"},
		{"non-sterling sell", with(ftRow("01/03/2030", "SELL", "1 Example ETF EUR (Dis.) (IE0000000003) @ EUR 1.00000 (@ 1 GBP = 1.17000 EUR) on 01-Mar-2030 10:00:00", "EUR", "CASH_IN", "1.00")), "currency"},
		{"non-sterling buy without a rate", with(ftRow("01/03/2030", "BUY", "1 Example ETF EUR (Dis.) (IE0000000003) @ EUR 1.00000 on 01-Mar-2030 10:00:00", "EUR", "CASH_OUT", "-1.00")), "fx rate"},
		{"non-sterling buy with the other rate", with(ftRow("01/03/2030", "BUY", "1 Example ETF EUR (Dis.) (IE0000000003) @ EUR 1.00000 (@ 1 EUR = 0.85000 GBP) on 01-Mar-2030 10:00:00", "EUR", "CASH_OUT", "-1.00")), "fx rate"},
		{"non-sterling buy with another currency's rate", with(ftRow("01/03/2030", "BUY", "1 Example ETF EUR (Dis.) (IE0000000003) @ EUR 1.00000 (@ 1 GBP = 1.25000 USD) on 01-Mar-2030 10:00:00", "EUR", "CASH_OUT", "-1.00")), "fx rate"},
		{"non-sterling buy priced in another currency", with(ftRow("01/03/2030", "BUY", "1 Example Corp (US0000000001) @ USD 1.00000 (@ 1 GBP = 1.17000 EUR) on 01-Mar-2030 10:00:00", "EUR", "CASH_OUT", "-1.00")), "fx rate"},
		{"zero rate", with(ftRow("01/03/2030", "BUY", "1 Example ETF EUR (Dis.) (IE0000000003) @ EUR 1.00000 (@ 1 GBP = 0.00000 EUR) on 01-Mar-2030 10:00:00", "EUR", "CASH_OUT", "-1.00")), "fx rate"},
		{"lower-case currency", with(ftRow("01/03/2030", "INTEREST", "Interest income of 1.00 GBP", "gbp", "CASH_IN", "1.00")), "currency"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := importer.FreetradeActivityNormaliser{}.NormaliseInvestmentFile(ledger.File{Body: c.body}, time.UTC)
			var fe *importer.FreetradeCSVError
			if !errors.As(err, &fe) || fe.Reason != c.reason {
				t.Fatalf("error %v, want reason %q", err, c.reason)
			}
			if !errors.Is(err, importer.ErrFormat) {
				t.Fatalf("error %v is not an ErrFormat", err)
			}
			for _, s := range []string{"Example", "US0000000001", "IE0000000003", "1.00", "2030", "income"} {
				if strings.Contains(err.Error(), s) {
					t.Fatalf("error %q carries file content %q", err, s)
				}
			}
		})
	}
}

// A normaliser is a pure function of its bytes: it reads no clock or file and logs nothing.
func TestFreetradeNormaliserSourceReadsNoClockAndLogsNothing(t *testing.T) {
	src, err := os.ReadFile("freetradecsv.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"time.Now", "os.Open", "os.ReadFile", "log/slog", "\"log\"", "\"fmt\"", "Println", "Printf"} {
		if bytes.Contains(src, []byte(banned)) {
			t.Errorf("freetradecsv.go uses %s: a normaliser reads no clock or file and logs nothing", banned)
		}
	}
	body := ftFile(ftBuyEUR, ftBuyUSD, ftSell, ftIncome, ftIncome)
	a, b := ftResult(t, body), ftResult(t, append([]byte(nil), body...))
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if !bytes.Equal(ja, jb) {
		t.Fatal("two reads of the same bytes differ")
	}
}

func FuzzFreetradeCSV(f *testing.F) {
	f.Add(ftFile(ftBuyEUR, ftBuyUSD, ftSell, ftFXFee, ftIncome, ftIncome, ftCorpFrom, ftIn))
	f.Add([]byte(ftHeader))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = importer.FreetradeActivityNormaliser{}.NormaliseInvestmentFile(ledger.File{Body: b}, time.UTC)
	})
}

// newFreetradeEnv is a database with the Freetrade format registered, and a freetrade platform
// for each slug.
func newFreetradeEnv(t *testing.T, slugs ...string) *fidEnv {
	t.Helper()
	e := newFidEnv(t)
	e.l = ledger.New(nil, map[string]ledger.FileNormaliser{}, time.UTC, slog.New(slog.NewJSONHandler(e.logs, nil)), nil,
		ledger.WithInvestmentNormalisers(map[string]ledger.InvestmentNormaliser{
			string(importer.FormatFreetradeActivityCSV): importer.FreetradeActivityNormaliser{},
		}))
	for _, slug := range slugs {
		e.platformOf(slug, ledger.ProviderFreetrade)
	}
	return e
}

func (e *fidEnv) platformOf(slug, provider string) {
	e.t.Helper()
	p, err := json.Marshal(ledger.AccountAdd{Slug: slug, Type: "platform", Currency: "GBP", Name: "Made-up", ExpectsImports: true, Provider: provider})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := store.InsertOperatorEntry(context.Background(), e.app.DB(), store.OperatorEntry{Kind: ledger.EntryAccountAdd, Payload: p, EnteredAt: e.tick()}); err != nil {
		e.t.Fatal(err)
	}
	e.catchUp()
}

func (e *fidEnv) end(slug, last string) {
	e.t.Helper()
	p, err := json.Marshal(ledger.AccountEnd{Slug: slug, LastDate: &last})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := store.InsertOperatorEntry(context.Background(), e.app.DB(), store.OperatorEntry{Kind: ledger.EntryAccountEnd, Payload: p, EnteredAt: e.tick()}); err != nil {
		e.t.Fatal(err)
	}
	e.catchUp()
}

// ftBalanced is a made-up history whose cash comes to nothing: 1000.00 in, a buy, a sale, income
// and fees, and the rest transferred out. Its units do not come to nothing, as in a real export
// that lacks the transfer of the holdings.
var ftBalanced = []string{
	ftRow("10/03/2030", "CASH_TRANSFER", "939.09 GBP cash transfer", "GBP", "CASH_OUT", "-939.09"),
	ftBuyUSD, ftSell, ftFXFee, ftStamp, ftInterest, ftIncome, ftIncome, ftIn,
}

// AC1, AC3, AC5: an export imports into a freetrade platform as one event per row under the child
// <platform>-sipp; a repeat or overlapping export stores nothing new; units are never checked, and
// cash is checked once the account is ended: an ended account must hold no cash, so a history that
// comes to nothing is reconciled and one that does not is a mismatch by the difference.
func TestFreetradeImportsAndChecksCashThroughTheLedger(t *testing.T) {
	e := newFreetradeEnv(t, "ft")
	body := ftFile(ftBalanced...)
	if st := e.do("ft", importer.FormatFreetradeActivityCSV, body); st.FilesRejected != 0 || st.AccountsCreated != 1 {
		t.Fatalf("import: %+v", st)
	}
	child := e.account("ft-sipp")
	if child.Type != "pension" || child.SourceKey != ledger.ProductSIPP || child.ParentID == nil || child.Currency != "GBP" {
		t.Fatalf("child %+v", child)
	}
	if n := e.count("SELECT count(*) FROM core.investment_event WHERE account_id = $1", child.ID); n != len(ftBalanced) {
		t.Fatalf("%d events, want %d", n, len(ftBalanced))
	}
	if n := e.count("SELECT count(*) FROM core.investment_event WHERE event_type = 'income'"); n != 2 {
		t.Fatalf("%d income events, want 2", n)
	}
	var cash int64
	if err := e.app.DB().QueryRow(context.Background(), "SELECT sum(cash_minor) FROM core.investment_event WHERE account_id = $1", child.ID).Scan(&cash); err != nil || cash != 0 {
		t.Fatalf("replayed cash %d, %v", cash, err)
	}
	// Not ended: nothing to check cash against.
	if rec := e.recon("ft-sipp"); rec.Method != "closing_cash" || rec.LastCheckOK != nil || rec.BreakCount != 0 {
		t.Fatalf("before the end: %+v", rec)
	}

	// The same bytes again, and an export that overlaps it, store nothing new.
	e.do("ft", importer.FormatFreetradeActivityCSV, body)
	if st := e.do("ft", importer.FormatFreetradeActivityCSV, ftFile(ftBalanced[3:]...)); st.FilesRejected != 0 {
		t.Fatalf("overlap: %v", st.Rejections)
	}
	if n := e.count("SELECT count(*) FROM core.investment_event"); n != len(ftBalanced) {
		t.Fatalf("%d events after the overlap, want %d", n, len(ftBalanced))
	}
	if n := e.count("SELECT count(*) FROM core.investment_event_version"); n != len(ftBalanced) {
		t.Fatalf("%d versions after the overlap, want %d", n, len(ftBalanced))
	}

	e.end("ft-sipp", "2030-03-10")
	rec := e.recon("ft-sipp")
	if rec.LastCheckOK == nil || !*rec.LastCheckOK || rec.BreakCount != 0 || rec.LastDiffMinor == nil || *rec.LastDiffMinor != 0 ||
		rec.ReconciledThrough == nil || !rec.ReconciledThrough.Equal(ftDay("2030-03-10")) {
		t.Fatalf("balanced and ended: %+v", rec)
	}
	// Units are never checked: the export holds 12.5 bought and 2.12345678 of another sold.
	if n := e.count("SELECT count(*) FROM core.holding_check"); n != 0 {
		t.Fatalf("%d holding checks", n)
	}

	// A history whose cash does not come to nothing is a mismatch by the difference: the account
	// is closed, so it holds 0, and the history leaves it at -100.00.
	short := append([]string{ftRow("10/03/2030", "CASH_TRANSFER", "1039.09 GBP cash transfer", "GBP", "CASH_OUT", "-1039.09")}, ftBalanced[1:]...)
	s := newFreetradeEnv(t, "ft-short")
	s.do("ft-short", importer.FormatFreetradeActivityCSV, ftFile(short...))
	if rec := s.recon("ft-short-sipp"); rec.LastCheckOK != nil {
		t.Fatalf("checked before the end: %+v", rec)
	}
	s.end("ft-short-sipp", "2030-03-10")
	rec = s.recon("ft-short-sipp")
	if rec.LastCheckOK == nil || *rec.LastCheckOK || rec.BreakCount != 1 || rec.LastDiffMinor == nil || *rec.LastDiffMinor != 10000 ||
		rec.ReconciledThrough != nil {
		t.Fatalf("short and ended: %+v", rec)
	}
	var state string
	if err := s.app.DB().QueryRow(context.Background(), "SELECT reconciliation_state FROM agent.coverage WHERE account_slug = 'ft-short-sipp'").Scan(&state); err != nil || state != "mismatch" {
		t.Fatalf("coverage state %q, %v", state, err)
	}
	// Ending it on an earlier day judges the data up to that day only: 1000.00 in by then.
	s.end("ft-short-sipp", "2030-03-01")
	rec = s.recon("ft-short-sipp")
	if rec.LastCheckOK == nil || *rec.LastCheckOK || rec.LastDiffMinor == nil || *rec.LastDiffMinor != -100000 {
		t.Fatalf("ended early: %+v", rec)
	}

	// The SIPP is keyed by its product, so a second freetrade platform cannot take it over.
	e.platformOf("ft-other", ledger.ProviderFreetrade)
	if st := e.do("ft-other", importer.FormatFreetradeActivityCSV, body); len(st.Rejections) != 1 || st.Rejections[0] != "account_mismatch" {
		t.Fatalf("on a second platform: %v", st.Rejections)
	}

	// Nothing from the files reaches a log line.
	logs := e.logs.String() + s.logs.String()
	for _, s := range []string{"Example", "US0000000001", "939.09", "1039.09", "Self-Invested", "cash transfer"} {
		if strings.Contains(logs, s) {
			t.Errorf("a log line carries %q", s)
		}
	}
}

// A Freetrade file against a platform of another provider is refused, and a refused file stores
// nothing.
func TestFreetradeRefusedFilesStoreNothing(t *testing.T) {
	e := newFreetradeEnv(t, "ft")
	e.platformOf("fid", ledger.ProviderFidelity)
	if st := e.do("fid", importer.FormatFreetradeActivityCSV, ftFile(ftBalanced...)); len(st.Rejections) != 1 || st.Rejections[0] != "account_mismatch" {
		t.Fatalf("on a fidelity platform: %v", st.Rejections)
	}
	bad := ftFile(append(append([]string(nil), ftBalanced...), ftRow("01/03/2030", "DIVIDEND", "Investment income of 1.00 GBP", "GBP", "CASH_IN", "1.00"))...)
	if st := e.do("ft", importer.FormatFreetradeActivityCSV, bad); len(st.Rejections) != 1 || st.Rejections[0] != "check_failed" {
		t.Fatalf("unknown type: %v", st.Rejections)
	}
	for _, table := range []string{"core.investment_event", "core.investment_import", "core.instrument"} {
		if n := e.count("SELECT count(*) FROM " + table); n != 0 {
			t.Fatalf("%s holds %d rows after refused files", table, n)
		}
	}
	if n := e.count("SELECT count(*) FROM core.account WHERE parent_id IS NOT NULL"); n != 0 {
		t.Fatalf("a refused file created %d child accounts", n)
	}
}

package importer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/importer"
	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// Every value in this file is made up. The fixtures are written by hand from the documented shape
// of each format and are not derived from any real export.

const (
	fidSIPP    = "SIPP - Pension Savings Account"
	fidCMA     = "Cash Management Account"
	fidISA     = "Investment ISA"
	fidAcct    = "ACC00001234"
	fidCashAct = "ACC00005678"
	fidFund    = "EXAMPLE FUND ETF (EXMP)"
	fidOther   = "SAMPLE TRUST PLC (SMPL)"
)

const fidTxHeader = "Order date,Completion date,Transaction type,Investments,Product Wrapper,Account Number,Source investment,Amount,Quantity,Price per unit,Reference Number,Status,"

const fidTxPreamble = "Account ,All made-up accounts\r\nTimeframe,Made-up range\r\nTransaction type,All\r\nInvestment name,All\r\nValuations,Made-up\r\n\r\n"

// fidRow is one transaction row as printed: its columns in order.
func fidRow(order, complete, typ, investments, product, account, source, amount, qty, price, ref, status string) string {
	return strings.Join([]string{order, complete, typ, investments, product, account, source, amount, qty, price, ref, status, ""}, ",")
}

// fidCash and fidSec are Completed rows with the usual made-up dates.
func fidCash(typ, account, product, amount, ref string) string {
	return fidRow("05 Mar 2030", "07 Mar 2030", typ, "Cash", product, account, "", amount, "0", "", ref, "Completed")
}

func fidSec(typ, name, account, amount, qty, price, ref string) string {
	return fidRow("05 Mar 2030", "07 Mar 2030", typ, name, fidSIPP, account, "", amount, qty, price, ref, "Completed")
}

// fidTx is a transaction history file: a byte order mark, CRLF line ends, the preamble and rows.
func fidTx(rows ...string) []byte {
	return []byte("\xef\xbb\xbf" + fidTxPreamble + fidTxHeader + "\r\n" + strings.Join(rows, "\r\n") + "\r\n")
}

func fidTxResult(t *testing.T, body []byte) ledger.InvestmentResult {
	t.Helper()
	res, err := importer.FidelityTransactionsNormaliser{}.NormaliseInvestmentFile(ledger.File{Body: body}, time.UTC)
	if err != nil {
		t.Fatalf("NormaliseInvestmentFile: %v", err)
	}
	return res
}

const fidSumHeader = "Type,Holdings,Account number,Product,Account holder,Designation,Provider,Latest price (pence/cents/yen),Export date,Quantity,Value (£),Currency,Cash available,Book cost (£),Gain/loss (£),Gain/loss (%),Annualised return (%),Total return (all time),Total return (%)"

// fidSumRow is one block row: the columns that are read, the rest filled with made-up text.
func fidSumRow(typ, holdings, account, product, price, qty, value, currency, cash, book, gain string) string {
	return strings.Join([]string{typ, holdings, account, product, "Made-up Holder", "Made-up", "Made-up", price, "02/04/2030", qty, value, currency,
		cash, book, gain, "0.0", "0.0", "0.00", "0.0"}, ",")
}

func fidSummaryAt(date, clock string, rows ...string) []byte {
	return []byte("\xef\xbb\xbf" + "Client name,Made-up Client\r\nExport date," + date + "\r\nExport time," + clock +
		"\r\n\r\nView all account details\r\n" + fidSumHeader + "\r\n" + strings.Join(rows, "\r\n") + "\r\n")
}

func fidSummary(rows ...string) []byte { return fidSummaryAt("02/04/2030", "10:30", rows...) }

func fidSumResult(t *testing.T, body []byte, loc *time.Location) ledger.InvestmentResult {
	t.Helper()
	res, err := importer.FidelitySummaryNormaliser{}.NormaliseInvestmentFile(ledger.File{Body: body}, loc)
	if err != nil {
		t.Fatalf("NormaliseInvestmentFile: %v", err)
	}
	return res
}

func fidDay(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d
}

// AC1: every provider type in the design's event table maps as the table says.
func TestFidelityTransactionsMapsEveryProviderType(t *testing.T) {
	type want struct {
		event      string
		units      string
		cash, flow int64
		security   bool
		price      string
	}
	cases := []struct {
		row  func(ref string) string
		typ  string
		want want
	}{
		{func(r string) string { return fidSec("Buy", fidFund, fidAcct, "107.50", "10", "10.00", r) }, "Buy",
			want{event: ledger.EventBuy, units: "10", security: true, price: "10.00"}},
		{func(r string) string { return fidSec("Sell", fidFund, fidAcct, "-50.00", "5", "10.00", r) }, "Sell",
			want{event: ledger.EventSell, units: "5", security: true, price: "10.00"}},
		{func(r string) string {
			return fidSec("Auto-sell For Fees", fidOther, fidAcct, "-20.25", "2.5", "8.10", r)
		}, "Auto-sell For Fees",
			want{event: ledger.EventSell, units: "2.5", security: true, price: "8.10"}},
		{func(r string) string { return fidSec("Transfer In", fidFund, fidAcct, "300.00", "30", "10.00", r) }, "Transfer In",
			want{event: ledger.EventTransferIn, units: "30", flow: 30000, security: true, price: "10.00"}},
		{func(r string) string { return fidCash("Cash In For Transfer", fidAcct, fidSIPP, `"1,250.00"`, r) }, "Cash In For Transfer",
			want{event: ledger.EventTransferIn, cash: 125000, flow: 125000}},
		{func(r string) string { return fidCash("Dealing Fee", fidAcct, fidSIPP, "-7.50", r) }, "Dealing Fee",
			want{event: ledger.EventFee, cash: -750}},
		{func(r string) string { return fidCash("Service Fee", fidAcct, fidSIPP, "-3.10", r) }, "Service Fee",
			want{event: ledger.EventFee, cash: -310}},
		{func(r string) string {
			return fidCash("Stamp Duty Or Financial Transaction Tax", fidAcct, fidSIPP, "-0.52", r)
		}, "Stamp Duty Or Financial Transaction Tax", want{event: ledger.EventFee, cash: -52}},
		{func(r string) string { return fidCash("Cash Interest", fidAcct, fidSIPP, "2.00", r) }, "Cash Interest",
			want{event: ledger.EventInterest, cash: 200}},
		{func(r string) string { return fidCash("Tax On Interest", fidAcct, fidSIPP, "-0.40", r) }, "Tax On Interest",
			want{event: ledger.EventTax}},
		{func(r string) string { return fidCash("Withdrawal", fidAcct, fidSIPP, "-80.00", r) }, "Withdrawal",
			want{event: ledger.EventWithdrawal, cash: -8000, flow: -8000}},
		{func(r string) string { return fidCash("Adjustment", fidAcct, fidSIPP, "-1.00", r) }, "Adjustment (negative)",
			want{event: ledger.EventAdjustment, cash: -100, flow: -100}},
		{func(r string) string { return fidCash("Adjustment", fidAcct, fidSIPP, "1.00", r) }, "Adjustment (positive)",
			want{event: ledger.EventAdjustment, cash: 100, flow: 100}},
		{func(r string) string {
			return fidCash("Transfer To Cash Management Account For Fees", fidAcct, fidSIPP, "-5.00", r)
		}, "Transfer To Cash Management Account For Fees", want{event: ledger.EventSiblingTransfer, cash: -500, flow: -500}},
		{func(r string) string { return fidCash("Cash In Ring-fenced For Fees", fidCashAct, fidCMA, "5.00", r) }, "Cash In Ring-fenced For Fees",
			want{event: ledger.EventSiblingTransfer, cash: 500, flow: 500}},
		{func(r string) string { return fidCash("Buy", fidAcct, fidSIPP, "-100.00", r) }, "Buy (cash row)",
			want{event: ledger.EventInternal, cash: -10000}},
		{func(r string) string { return fidCash("Sell", fidAcct, fidSIPP, "50.00", r) }, "Sell (cash row)",
			want{event: ledger.EventInternal, cash: 5000}},
		{func(r string) string { return fidCash("Auto-sell For Fees", fidAcct, fidSIPP, "20.25", r) }, "Auto-sell For Fees (cash row)",
			want{event: ledger.EventInternal, cash: 2025}},
		{func(r string) string { return fidCash("Cash Out For Buy", fidAcct, fidSIPP, "-100.00", r) }, "Cash Out For Buy",
			want{event: ledger.EventInternal, cash: -10000}},
		{func(r string) string { return fidCash("Cash Out For Buy From Transfer", fidAcct, fidSIPP, "-60.00", r) }, "Cash Out For Buy From Transfer",
			want{event: ledger.EventInternal, cash: -6000}},
		{func(r string) string { return fidCash("Cash In From Sell", fidAcct, fidSIPP, "50.00", r) }, "Cash In From Sell",
			want{event: ledger.EventInternal, cash: 5000}},
		{func(r string) string { return fidCash("Cash In Fees From Auto-sell", fidAcct, fidSIPP, "20.25", r) }, "Cash In Fees From Auto-sell",
			want{event: ledger.EventInternal, cash: 2025}},
	}
	rows := make([]string, len(cases))
	for i, c := range cases {
		rows[i] = c.row("REF" + string(rune('A'+i)) + "00001")
	}
	res := fidTxResult(t, fidTx(rows...))
	if res.Kind != ledger.InvestmentKindLedger || res.CapturedAt != nil || res.RowCount != len(cases) || len(res.Events) != len(cases) ||
		len(res.Holdings)+len(res.Valuations)+len(res.Statements)+len(res.TradeNotes) != 0 {
		t.Fatalf("result shape: kind %q, %d rows, %d events", res.Kind, res.RowCount, len(res.Events))
	}
	wantAccounts := []ledger.InvestmentAccountRec{{AccountKey: fidAcct, Product: ledger.ProductSIPP}, {AccountKey: fidCashAct, Product: ledger.ProductCashManagement}}
	if !reflect.DeepEqual(res.Accounts, wantAccounts) {
		t.Fatalf("accounts %+v", res.Accounts)
	}
	for i, c := range cases {
		ev := res.Events[i]
		w := c.want
		if ev.EventType != w.event || ev.CashMinor != w.cash || ev.FlowMinor != w.flow || ev.Units != w.units || ev.Price != w.price ||
			!strings.HasPrefix(ev.ProviderType, strings.Fields(c.typ)[0]) {
			t.Errorf("%s: event %q cash %d flow %d units %q price %q provider type %q", c.typ, ev.EventType, ev.CashMinor, ev.FlowMinor, ev.Units, ev.Price, ev.ProviderType)
		}
		if (ev.Price != "") != (ev.PriceUnit == ledger.PriceUnitGBP) {
			t.Errorf("%s: price %q unit %q", c.typ, ev.Price, ev.PriceUnit)
		}
		if w.security != (ev.InstrumentKey != "") || (w.security && ev.InstrumentName == "") {
			t.Errorf("%s: instrument key %q name %q", c.typ, ev.InstrumentKey, ev.InstrumentName)
		}
		if !ev.OrderDate.Equal(fidDay("2030-03-05")) || !ev.SettleDate.Equal(fidDay("2030-03-07")) {
			t.Errorf("%s: dates %s %s", c.typ, ev.OrderDate, ev.SettleDate)
		}
		if string(ev.Content) != string(ev.HashInput) || !json.Valid(ev.Content) {
			t.Errorf("%s: content %s, hash input %s", c.typ, ev.Content, ev.HashInput)
		}
		if ev.SourceKey != "REF"+string(rune('A'+i))+"00001" || ev.GrossMinor == 0 {
			t.Errorf("%s: key %q gross %d", c.typ, ev.SourceKey, ev.GrossMinor)
		}
	}
	// The ticker is the final parenthesised token, and the instrument is named as printed.
	if ev := res.Events[0]; ev.InstrumentKey != "EXMP" || ev.InstrumentName != fidFund {
		t.Errorf("buy instrument %q %q", ev.InstrumentKey, ev.InstrumentName)
	}
	// Gross is the Amount as printed: a buy's includes the fee, a thousands comma is understood.
	if res.Events[0].GrossMinor != 10750 || res.Events[4].GrossMinor != 125000 {
		t.Errorf("gross %d, %d", res.Events[0].GrossMinor, res.Events[4].GrossMinor)
	}
}

// The design's worked example: a buy for 100.00 plus a 7.50 fee is three rows, and only the two
// cash rows count in cash.
func TestFidelityBuyWithFeeCountsOnlyItsCashRows(t *testing.T) {
	res := fidTxResult(t, fidTx(
		fidSec("Buy", fidFund, fidAcct, "107.50", "10", "10.00", "WK100001"),
		fidCash("Cash Out For Buy", fidAcct, fidSIPP, "-100.00", "WK100002"),
		fidCash("Dealing Fee", fidAcct, fidSIPP, "-7.50", "WK100003")))
	var cash int64
	for _, ev := range res.Events {
		cash += ev.CashMinor
	}
	if len(res.Events) != 3 || cash != -10750 || res.Events[0].CashMinor != 0 || res.Events[0].Units != "10" {
		t.Fatalf("cash %d, events %+v", cash, res.Events)
	}
}

// A file with only the preamble and header is a quiet period: valid, with no accounts.
func TestFidelityTransactionsEmptyExportIsValid(t *testing.T) {
	res := fidTxResult(t, fidTx())
	if res.RowCount != 0 || len(res.Events) != 0 || len(res.Accounts) != 0 || res.Kind != ledger.InvestmentKindLedger {
		t.Fatalf("%+v", res)
	}
	// Line ends of LF alone and no byte order mark read the same.
	lf := bytes.ReplaceAll(bytes.TrimPrefix(fidTx(fidCash("Cash Interest", fidAcct, fidSIPP, "2.00", "LF000001")), []byte("\xef\xbb\xbf")), []byte("\r\n"), []byte("\n"))
	if res := fidTxResult(t, lf); len(res.Events) != 1 {
		t.Fatalf("LF file: %+v", res)
	}
}

// AC2: the file is refused whole, with a reason that names no content.
func TestFidelityTransactionsRejectsTheWholeFile(t *testing.T) {
	good := fidCash("Cash Interest", fidAcct, fidSIPP, "2.00", "GOOD0001")
	secret := []string{"ACC0000", "EXAMPLE FUND", "SAMPLE TRUST", "REFBAD", "Made-up", "107.50"}
	withRows := func(rows ...string) []byte { return fidTx(append([]string{good}, rows...)...) }
	mutate := func(from, to string) []byte {
		b := fidTx(good, fidSec("Buy", fidFund, fidAcct, "107.50", "10", "10.00", "REFBAD01"))
		if !bytes.Contains(b, []byte(from)) {
			t.Fatalf("fixture lacks %q", from)
		}
		return bytes.Replace(b, []byte(from), []byte(to), 1)
	}
	cases := []struct {
		name   string
		body   []byte
		reason string
	}{
		{"changed header", mutate("Order date,", "Order Date,"), "header"},
		{"header without its trailing comma", mutate("Status,\r\n", "Status\r\n"), "header"},
		{"extra header column", mutate("Status,\r\n", "Status,Extra,\r\n"), "header"},
		{"missing preamble label", mutate("Timeframe,", "Period,"), "preamble"},
		{"no blank line before the header", mutate("Valuations,Made-up\r\n\r\n", "Valuations,Made-up\r\n"), "preamble"},
		{"unknown security type", withRows(fidSec("Dividend", fidFund, fidAcct, "1.00", "1", "1.00", "REFBAD01")), "unknown type"},
		{"unknown cash type", withRows(fidCash("Cash Dividend", fidAcct, fidSIPP, "1.00", "REFBAD01")), "unknown type"},
		{"cash type on a security row", withRows(fidSec("Dealing Fee", fidFund, fidAcct, "-1.00", "1", "1.00", "REFBAD01")), "unknown type"},
		{"security type spelt in lower case", withRows(fidSec("buy", fidFund, fidAcct, "1.00", "1", "1.00", "REFBAD01")), "unknown type"},
		{"unknown product", withRows(fidRow("05 Mar 2030", "07 Mar 2030", "Cash Interest", "Cash", "Made-up Wrapper", fidAcct, "", "1.00", "0", "", "REFBAD01", "Completed")), "unknown product"},
		{"buy with a negative amount", withRows(fidSec("Buy", fidFund, fidAcct, "-107.50", "10", "10.00", "REFBAD01")), "contradictory sign"},
		{"sell with a positive amount", withRows(fidSec("Sell", fidFund, fidAcct, "50.00", "5", "10.00", "REFBAD01")), "contradictory sign"},
		{"transfer in with a negative amount", withRows(fidSec("Transfer In", fidFund, fidAcct, "-50.00", "5", "10.00", "REFBAD01")), "contradictory sign"},
		{"cash out for buy with a positive amount", withRows(fidCash("Cash Out For Buy", fidAcct, fidSIPP, "100.00", "REFBAD01")), "contradictory sign"},
		{"fee with a positive amount", withRows(fidCash("Dealing Fee", fidAcct, fidSIPP, "7.50", "REFBAD01")), "contradictory sign"},
		{"withdrawal with a positive amount", withRows(fidCash("Withdrawal", fidAcct, fidSIPP, "7.50", "REFBAD01")), "contradictory sign"},
		{"interest with a negative amount", withRows(fidCash("Cash Interest", fidAcct, fidSIPP, "-7.50", "REFBAD01")), "contradictory sign"},
		{"blank reference", withRows(fidCash("Cash Interest", fidAcct, fidSIPP, "1.00", "")), "blank reference"},
		{"repeated reference", withRows(fidCash("Cash Interest", fidAcct, fidSIPP, "1.00", "GOOD0001")), "repeated reference"},
		{"repeated reference on another account", withRows(fidCash("Cash Interest", fidCashAct, fidCMA, "1.00", "GOOD0001")), "repeated reference"},
		{"status not completed", withRows(fidRow("05 Mar 2030", "07 Mar 2030", "Cash Interest", "Cash", fidSIPP, fidAcct, "", "1.00", "0", "", "REFBAD01", "Pending")), "status"},
		{"bad order date", withRows(fidRow("2030-03-05", "07 Mar 2030", "Cash Interest", "Cash", fidSIPP, fidAcct, "", "1.00", "0", "", "REFBAD01", "Completed")), "date"},
		{"bad completion date", withRows(fidRow("05 Mar 2030", "7 Mar", "Cash Interest", "Cash", fidSIPP, fidAcct, "", "1.00", "0", "", "REFBAD01", "Completed")), "date"},
		{"amount with three decimals", withRows(fidCash("Cash Interest", fidAcct, fidSIPP, "1.005", "REFBAD01")), "amount"},
		{"blank amount", withRows(fidCash("Cash Interest", fidAcct, fidSIPP, "", "REFBAD01")), "amount"},
		{"signed quantity", withRows(fidSec("Buy", fidFund, fidAcct, "1.00", "-1", "1.00", "REFBAD01")), "quantity"},
		{"exponent quantity", withRows(fidSec("Buy", fidFund, fidAcct, "1.00", "1e3", "1.00", "REFBAD01")), "quantity"},
		{"bad price", withRows(fidSec("Buy", fidFund, fidAcct, "1.00", "1", "-1.00", "REFBAD01")), "price"},
		{"security without a ticker", withRows(fidSec("Buy", "EXAMPLE FUND ETF", fidAcct, "1.00", "1", "1.00", "REFBAD01")), "no ticker"},
		{"security with an empty ticker", withRows(fidSec("Buy", "EXAMPLE FUND ETF ()", fidAcct, "1.00", "1", "1.00", "REFBAD01")), "no ticker"},
		{"short account number", withRows(fidCash("Cash Interest", "A-1", fidSIPP, "1.00", "REFBAD01")), "account number"},
		{"one account under two products", withRows(fidCash("Cash Interest", fidAcct, fidISA, "1.00", "REFBAD01")), "product conflict"},
		{"short row", withRows("05 Mar 2030,07 Mar 2030,Cash Interest"), "row shape"},
		{"long row", withRows(strings.Replace(fidCash("Cash Interest", fidAcct, fidSIPP, "1.00", "REFBAD01"), "Completed,", "Completed,x,", 1)), "row shape"},
		{"text in the unnamed last column", withRows(strings.TrimSuffix(fidCash("Cash Interest", fidAcct, fidSIPP, "1.00", "REFBAD01"), ",") + ",x"), "row shape"},
		{"not UTF-8", withRows(fidCash("Cash Interest", fidAcct, fidSIPP, "1.00", "REFBAD01") + "\xff"), "not UTF-8"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := importer.FidelityTransactionsNormaliser{}.NormaliseInvestmentFile(ledger.File{Body: c.body}, time.UTC)
			var fe *importer.FidelityCSVError
			if !errors.As(err, &fe) || !errors.Is(err, importer.ErrFormat) {
				t.Fatalf("want a FidelityCSVError, got %v", err)
			}
			if fe.Reason != c.reason {
				t.Fatalf("reason %q, want %q", fe.Reason, c.reason)
			}
			for _, s := range secret {
				if strings.Contains(err.Error(), s) {
					t.Fatalf("error %q carries file content %q", err, s)
				}
			}
		})
	}
	// The unmutated shape is accepted, so each refusal above is its own mutation's.
	if res := fidTxResult(t, fidTx(good, fidSec("Buy", fidFund, fidAcct, "107.50", "10", "10.00", "REFBAD01"))); len(res.Events) != 2 {
		t.Fatalf("baseline %+v", res)
	}
}

// A row dated or completed oddly is fine when it is on the documented shape: a row with no
// completion date has no settle date, and an unpriced security row has no price.
func TestFidelityTransactionsOptionalColumns(t *testing.T) {
	res := fidTxResult(t, fidTx(
		fidRow("05 Mar 2030", "", "Buy", fidFund, fidSIPP, fidAcct, "", "1.00", "1", "", "OPT00001", "Completed")))
	ev := res.Events[0]
	if !ev.SettleDate.IsZero() || ev.Price != "" || ev.PriceUnit != "" || ev.Units != "1" {
		t.Fatalf("%+v", ev)
	}
}

// AC3, at the parser: the same row printed by two exports is the same record, so the ledger has
// one event and no new version.
func TestFidelityOverlappingExportsGiveIdenticalRecords(t *testing.T) {
	a := fidSec("Buy", fidFund, fidAcct, "107.50", "10", "10.00", "OV000001")
	b := fidCash("Cash Interest", fidAcct, fidSIPP, "2.00", "OV000002")
	c := fidCash("Cash Interest", fidAcct, fidSIPP, "3.00", "OV000003")
	first := fidTxResult(t, fidTx(a, b))
	second := fidTxResult(t, fidTx(b, c))
	if !reflect.DeepEqual(first.Events[1], second.Events[0]) {
		t.Fatalf("the shared row differs:\n%+v\n%+v", first.Events[1], second.Events[0])
	}
}

// The normalisers are pure: a second read of the same bytes is identical, and the printed export
// time follows the location they are given.
func TestFidelityNormalisersArePure(t *testing.T) {
	body := fidTx(fidSec("Buy", fidFund, fidAcct, "107.50", "10", "10.00", "PU000001"), fidCash("Cash Interest", fidAcct, fidSIPP, "2.00", "PU000002"))
	if !reflect.DeepEqual(fidTxResult(t, body), fidTxResult(t, body)) {
		t.Fatal("transactions differ between reads")
	}
	sum := fidSummary(fidSumRow("Account", "", fidAcct, fidSIPP, "", "", "100.00", "", "100.00", "", ""))
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	a, b := fidSumResult(t, sum, london), fidSumResult(t, sum, london)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("summaries differ between reads")
	}
	// 10:30 on 2 April 2030 is British Summer Time.
	if want := time.Date(2030, time.April, 2, 9, 30, 0, 0, time.UTC); !a.CapturedAt.Equal(want) {
		t.Fatalf("captured at %s, want %s", a.CapturedAt, want)
	}
}

func fidSummaryRows() []string {
	return []string{
		fidSumRow("Account", "", fidAcct, fidSIPP, "", "", "994.55", "", "889.50", "", "-2.45"),
		fidSumRow("Asset", fidFund, fidAcct, fidSIPP, "1050.5", "10", "105.05", "GBP", "", "107.50", ""),
		fidSumRow("Account", "", fidCashAct, fidCMA, "", "", "5.00", "", "5.00", "", ""),
	}
}

// AC4: holdings and valuations are stored.
func TestFidelitySummaryGivesHoldingsAndValuations(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	res := fidSumResult(t, fidSummary(fidSummaryRows()...), london)
	if res.Kind != ledger.InvestmentKindHoldings || res.RowCount != 3 || len(res.Events) != 0 {
		t.Fatalf("shape: kind %q rows %d", res.Kind, res.RowCount)
	}
	if res.CapturedAt == nil || !res.CapturedAt.Equal(time.Date(2030, time.April, 2, 9, 30, 0, 0, time.UTC)) {
		t.Fatalf("captured at %v", res.CapturedAt)
	}
	wantAccounts := []ledger.InvestmentAccountRec{{AccountKey: fidAcct, Product: ledger.ProductSIPP}, {AccountKey: fidCashAct, Product: ledger.ProductCashManagement}}
	if !reflect.DeepEqual(res.Accounts, wantAccounts) {
		t.Fatalf("accounts %+v", res.Accounts)
	}
	gain, book := int64(-245), int64(10750)
	wantVal := []ledger.ValuationRec{
		{AccountKey: fidAcct, AsOf: fidDay("2030-04-02"), TotalMinor: 99455, CashMinor: 88950, ReportedGainMinor: &gain},
		{AccountKey: fidCashAct, AsOf: fidDay("2030-04-02"), TotalMinor: 500, CashMinor: 500},
	}
	if !reflect.DeepEqual(res.Valuations, wantVal) {
		t.Fatalf("valuations %+v", res.Valuations)
	}
	wantHold := []ledger.HoldingRec{{AccountKey: fidAcct, InstrumentKey: "EXMP", InstrumentName: fidFund, AsOf: fidDay("2030-04-02"),
		Units: "10", Price: "1050.5", PriceUnit: ledger.PriceUnitGBX, ValueMinor: 10505, BookCostMinor: &book}}
	if !reflect.DeepEqual(res.Holdings, wantHold) {
		t.Fatalf("holdings %+v", res.Holdings)
	}
	// The holder and client names are not carried by any record.
	if b, _ := json.Marshal(res); bytes.Contains(b, []byte("Made-up")) {
		t.Fatalf("a name reached the result: %s", b)
	}
}

// A thousands comma and a pound sign in a value are read.
func TestFidelitySummaryValueFormats(t *testing.T) {
	res := fidSumResult(t, fidSummary(
		fidSumRow("Account", "", fidAcct, fidSIPP, "", "", `"£1,234.50"`, "", "134.50", "", ""),
		fidSumRow("Asset", fidFund, fidAcct, fidSIPP, "110.00", "1000.00", `"£1,100.00"`, "GBP", "", "", "")), time.UTC)
	if res.Holdings[0].ValueMinor != 110000 || res.Valuations[0].TotalMinor != 123450 {
		t.Fatalf("%+v %+v", res.Holdings[0], res.Valuations[0])
	}
	if res.Holdings[0].Currency != "" {
		t.Fatalf("a sterling holding has currency %q", res.Holdings[0].Currency)
	}
}

// The summary drops trailing zeros from amounts, prints an empty field after an Asset row's last
// column, and leaves cash blank on an account with nothing in it.
func TestFidelitySummaryPrintedShapes(t *testing.T) {
	res := fidSumResult(t, fidSummary(
		fidSumRow("Account", "", fidAcct, fidSIPP, "", "", "994.5", "", "889.5", "", "-2.5"),
		fidSumRow("Asset", fidFund, fidAcct, fidSIPP, "1050", "10", "105", "GBP", "", "107.5", "")+",",
		fidSumRow("Account", "", fidCashAct, fidCMA, "", "", "0", "", "", "", "")), time.UTC)
	if v := res.Valuations[0]; v.TotalMinor != 99450 || v.CashMinor != 88950 || v.ReportedGainMinor == nil || *v.ReportedGainMinor != -250 {
		t.Fatalf("account %+v", v)
	}
	if h := res.Holdings[0]; h.ValueMinor != 10500 || h.BookCostMinor == nil || *h.BookCostMinor != 10750 {
		t.Fatalf("holding %+v", h)
	}
	if v := res.Valuations[1]; v.TotalMinor != 0 || v.CashMinor != 0 {
		t.Fatalf("empty account %+v", v)
	}
}

// AC4: a file failing the Value or the Total check is refused, and the checks are the design's.
func TestFidelitySummaryRejectsFailedChecks(t *testing.T) {
	account := func(total, cash string) string {
		return fidSumRow("Account", "", fidAcct, fidSIPP, "", "", total, "", cash, "", "")
	}
	// 10 units at 1050.5p is 105.05. Printed to five places the units allow about a penny either way.
	asset := func(value string) string {
		return fidSumRow("Asset", fidFund, fidAcct, fidSIPP, "1050.5", "10.00000", value, "GBP", "", "", "")
	}
	cases := []struct {
		name   string
		rows   []string
		reason string
	}{
		{"value off by two pence", []string{account("994.57", "889.50"), asset("105.07")}, "value check failed"},
		{"value off by a pound", []string{account("995.55", "889.50"), asset("106.05")}, "value check failed"},
		{"total two pence high", []string{account("994.57", "889.50"), asset("105.05")}, "total check failed"},
		{"total two pence low", []string{account("994.53", "889.50"), asset("105.05")}, "total check failed"},
		{"cash does not add up", []string{account("994.55", "890.50"), asset("105.05")}, "total check failed"},
		{"an account with no holdings must add up exactly", []string{account("100.01", "100.00")}, "total check failed"},
		{"holding on an account with no account row", []string{asset("105.05")}, "holding without an account row"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := importer.FidelitySummaryNormaliser{}.NormaliseInvestmentFile(ledger.File{Body: fidSummary(c.rows...)}, time.UTC)
			var fe *importer.FidelityCSVError
			if !errors.As(err, &fe) || fe.Reason != c.reason {
				t.Fatalf("want %q, got %v", c.reason, err)
			}
			if strings.Contains(err.Error(), "105") || strings.Contains(err.Error(), "ACC0000") {
				t.Fatalf("error carries content: %v", err)
			}
		})
	}
	// Within the tolerances the file is accepted: a penny either way on a value, and a penny a holding on a total.
	for _, rows := range [][]string{
		{account("994.55", "889.50"), asset("105.05")},
		{account("994.56", "889.50"), asset("105.05")},
		{account("994.54", "889.50"), asset("105.05")},
		{account("994.56", "889.50"), asset("105.06")},
		{account("994.54", "889.50"), asset("105.04")},
	} {
		if _, err := (importer.FidelitySummaryNormaliser{}).NormaliseInvestmentFile(ledger.File{Body: fidSummary(rows...)}, time.UTC); err != nil {
			t.Errorf("rows %q: %v", rows, err)
		}
	}
}

// The Value check's bound is the design's formula. With units 3.3 (one place) and price 99.9 pence
// (one place), u*p = 329.67 and the allowed difference is 3.3*0.1/2 + 99.9*0.1/2 + 0.01/4 + 0.5 =
// 0.165 + 4.995 + 0.0025 + 0.5 = 5.6625 pence.
func TestFidelityValueCheckBoundary(t *testing.T) {
	row := func(value string) []byte {
		return fidSummary(
			fidSumRow("Account", "", fidAcct, fidSIPP, "", "", value, "", "0.00", "", ""),
			fidSumRow("Asset", fidFund, fidAcct, fidSIPP, "99.9", "3.3", value, "GBP", "", "", ""))
	}
	norm := importer.FidelitySummaryNormaliser{}
	// The bounds are 329.67 +- 5.6625 pence, 324.0075 to 335.3325: 3.25 to 3.35 pass.
	for _, c := range []struct {
		value string
		ok    bool
	}{{"3.35", true}, {"3.36", false}, {"3.25", true}, {"3.24", false}} {
		_, err := norm.NormaliseInvestmentFile(ledger.File{Body: row(c.value)}, time.UTC)
		if (err == nil) != c.ok {
			t.Errorf("value %s: err %v, want ok %v", c.value, err, c.ok)
		}
	}
}

func TestFidelitySummaryRejects(t *testing.T) {
	account := fidSumRow("Account", "", fidAcct, fidSIPP, "", "", "100.00", "", "100.00", "", "")
	build := func(date, clock, header string, rows ...string) []byte {
		b := fidSummaryAt(date, clock, rows...)
		return bytes.Replace(b, []byte(fidSumHeader), []byte(header), 1)
	}
	cases := []struct {
		name   string
		body   []byte
		reason string
	}{
		{"changed header", build("02/04/2030", "10:30", strings.Replace(fidSumHeader, "Quantity", "Units", 1), account), "header"},
		{"header with a column dropped", build("02/04/2030", "10:30", strings.TrimSuffix(fidSumHeader, ",Total return (%)"), account), "header"},
		{"no marker line", bytes.Replace(fidSummary(account), []byte("View all account details"), []byte("View some account details"), 1), "preamble"},
		{"no export time", bytes.Replace(fidSummary(account), []byte("Export time,10:30\r\n"), nil, 1), "preamble"},
		{"no export date", bytes.Replace(fidSummary(account), []byte("Export date,02/04/2030\r\n"), nil, 1), "preamble"},
		{"bad export date", fidSummaryAt("2030-04-02", "10:30", account), "date"},
		{"bad export time", fidSummaryAt("02/04/2030", "10.30", account), "date"},
		{"unknown product", fidSummary(fidSumRow("Account", "", fidAcct, "Made-up Wrapper", "", "", "100.00", "", "100.00", "", "")), "unknown product"},
		{"unknown row type", fidSummary(account, fidSumRow("Liability", "", fidAcct, fidSIPP, "", "", "1.00", "", "", "", "")), "unknown row type"},
		{"repeated account row", fidSummary(account, account), "repeated row"},
		{"asset without a ticker", fidSummary(account, fidSumRow("Asset", "EXAMPLE FUND ETF", fidAcct, fidSIPP, "100", "1", "1.00", "GBP", "", "", "")), "no ticker"},
		{"repeated holding", fidSummary(account,
			fidSumRow("Asset", fidFund, fidAcct, fidSIPP, "100", "1", "1.00", "GBP", "", "", ""),
			fidSumRow("Asset", fidFund, fidAcct, fidSIPP, "100", "1", "1.00", "GBP", "", "", "")), "repeated row"},
		{"asset product differs from its account", fidSummary(account, fidSumRow("Asset", fidFund, fidAcct, fidISA, "100", "1", "1.00", "GBP", "", "", "")), "product conflict"},
		{"bad quantity", fidSummary(account, fidSumRow("Asset", fidFund, fidAcct, fidSIPP, "100", "-1", "1.00", "GBP", "", "", "")), "quantity"},
		{"bad price", fidSummary(account, fidSumRow("Asset", fidFund, fidAcct, fidSIPP, "n/a", "1", "1.00", "GBP", "", "", "")), "price"},
		{"bad value", fidSummary(account, fidSumRow("Asset", fidFund, fidAcct, fidSIPP, "100", "1", "one pound", "GBP", "", "", "")), "amount"},
		{"bad book cost", fidSummary(account, fidSumRow("Asset", fidFund, fidAcct, fidSIPP, "100", "1", "1.00", "GBP", "", "n/a", "")), "amount"},
		{"blank cash", fidSummary(fidSumRow("Account", "", fidAcct, fidSIPP, "", "", "100.00", "", "", "", "")), "amount"},
		{"bad currency", fidSummary(account, fidSumRow("Asset", fidFund, fidAcct, fidSIPP, "100", "1", "1.00", "pounds", "", "", "")), "currency"},
		{"a holding quoted in another currency", fidSummary(account, fidSumRow("Asset", fidFund, fidAcct, fidSIPP, "25.00", "3", "0.75", "USD", "", "", "")), "currency"},
		{"short account number", fidSummary(fidSumRow("Account", "", "A-1", fidSIPP, "", "", "100.00", "", "100.00", "", "")), "account number"},
		{"short row", fidSummary(account, "Asset,EXAMPLE"), "row shape"},
		{"a filled column after the last", fidSummary(account, fidSumRow("Asset", fidFund, fidAcct, fidSIPP, "100", "1", "1.00", "GBP", "", "", "")+",extra"), "row shape"},
		{"three decimal places", fidSummary(account, fidSumRow("Asset", fidFund, fidAcct, fidSIPP, "100", "1", "1.001", "GBP", "", "", "")), "amount"},
		// 01:30 on 31 March 2030 does not happen in London: the clocks went forward at 01:00.
		{"export time that does not exist", fidSummaryAt("31/03/2030", "01:30", account), "export time"},
	}
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := importer.FidelitySummaryNormaliser{}.NormaliseInvestmentFile(ledger.File{Body: c.body}, london)
			var fe *importer.FidelityCSVError
			if !errors.As(err, &fe) || !errors.Is(err, importer.ErrFormat) || fe.Reason != c.reason {
				t.Fatalf("want %q, got %v", c.reason, err)
			}
			for _, s := range []string{"ACC0000", "EXAMPLE", "Made-up", "100.00"} {
				if strings.Contains(err.Error(), s) {
					t.Fatalf("error %q carries file content %q", err, s)
				}
			}
		})
	}
}

// A time that happens twice takes the later instant.
func TestFidelitySummaryAmbiguousExportTimeTakesTheLaterInstant(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	// 01:30 on 27 October 2030 happens in BST (00:30 UTC) and again in GMT (01:30 UTC).
	res := fidSumResult(t, fidSummaryAt("27/10/2030", "01:30", fidSumRow("Account", "", fidAcct, fidSIPP, "", "", "1.00", "", "1.00", "", "")), london)
	if want := time.Date(2030, time.October, 27, 1, 30, 0, 0, time.UTC); !res.CapturedAt.Equal(want) {
		t.Fatalf("captured at %s, want %s", res.CapturedAt, want)
	}
}

// A normaliser reads no clock and no file and logs nothing, so the ledger may call it again on a
// stored file and no line of it can carry content.
func TestFidelityNormaliserSourceReadsNoClockAndLogsNothing(t *testing.T) {
	src, err := os.ReadFile("fidelitycsv.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"time.Now", "os.Open", "os.ReadFile", "log/slog", `"log"`, `"fmt"`, "Println", "Printf"} {
		if bytes.Contains(src, []byte(banned)) {
			t.Errorf("fidelitycsv.go uses %s: a normaliser reads no clock or file and logs nothing", banned)
		}
	}
}

func FuzzFidelityCSV(f *testing.F) {
	f.Add(fidTx(fidSec("Buy", fidFund, fidAcct, "107.50", "10", "10.00", "FZ000001"), fidCash("Cash Interest", fidAcct, fidSIPP, "2.00", "FZ000002")))
	f.Add(fidSummary(fidSummaryRows()...))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = importer.FidelityTransactionsNormaliser{}.NormaliseInvestmentFile(ledger.File{Body: b}, time.UTC)
		_, _ = importer.FidelitySummaryNormaliser{}.NormaliseInvestmentFile(ledger.File{Body: b}, time.UTC)
	})
}

// fidEnv is a database with the Fidelity formats registered, for the tests that go through the
// ledger. Its clock starts after every printed time in the fixtures.
type fidEnv struct {
	t     *testing.T
	app   *store.Store
	l     *ledger.Ledger
	logs  *bytes.Buffer
	clock time.Time
}

func newFidEnv(t *testing.T) *fidEnv {
	t.Helper()
	ctx := context.Background()
	super := testpg.Start(t)
	if err := store.Bootstrap(ctx, super, "owner-secret", "app-secret"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	ownerURL := testpg.UserURL(t, super, store.RoleOwner)
	if err := store.Migrate(ctx, ownerURL, ledger.NormaliserRev); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	app, err := store.Open(ctx, testpg.UserURL(t, super, store.RoleApp))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(app.Close)
	e := &fidEnv{t: t, app: app, logs: &bytes.Buffer{}, clock: time.Date(2030, time.April, 10, 9, 0, 0, 0, time.UTC)}
	e.l = ledger.New(nil, map[string]ledger.FileNormaliser{}, time.UTC, slog.New(slog.NewJSONHandler(e.logs, nil)), nil,
		ledger.WithInvestmentNormalisers(map[string]ledger.InvestmentNormaliser{
			string(importer.FormatFidelityTransactionsCSV): importer.FidelityTransactionsNormaliser{},
			string(importer.FormatFidelitySummaryCSV):      importer.FidelitySummaryNormaliser{},
		}))
	return e
}

func (e *fidEnv) tick() time.Time {
	e.clock = e.clock.Add(time.Minute)
	return e.clock
}

func (e *fidEnv) core() store.Core {
	c, err := store.NewCore(e.app.DB(), store.SchemaCore)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *fidEnv) catchUp() ledger.Stats {
	e.t.Helper()
	var stats ledger.Stats
	err := e.app.WithWriterLock(context.Background(), store.HolderTest, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stats, err = e.l.CatchUp(ctx, tx, store.SchemaCore)
		return err
	})
	if err != nil {
		e.t.Fatalf("CatchUp: %v", err)
	}
	return stats
}

func (e *fidEnv) platform(slug string) {
	e.t.Helper()
	p, err := json.Marshal(ledger.AccountAdd{Slug: slug, Type: "platform", Currency: "GBP", Name: "Made-up", ExpectsImports: true, Provider: ledger.ProviderFidelity})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := store.InsertOperatorEntry(context.Background(), e.app.DB(), store.OperatorEntry{Kind: ledger.EntryAccountAdd, Payload: p, EnteredAt: e.tick()}); err != nil {
		e.t.Fatal(err)
	}
	e.catchUp()
}

// importAt stores body as a file of format imported at the given instant and normalises it.
func (e *fidEnv) importAt(slug string, format importer.Format, body []byte, at time.Time) ledger.Stats {
	e.t.Helper()
	if _, _, err := store.InsertImportFile(context.Background(), e.app.DB(), store.ImportFile{AccountSlug: slug, Format: string(format),
		FileName: "made-up", ImportedAt: at, Body: body}); err != nil {
		e.t.Fatal(err)
	}
	return e.catchUp()
}

func (e *fidEnv) do(slug string, format importer.Format, body []byte) ledger.Stats {
	e.t.Helper()
	return e.importAt(slug, format, body, e.tick())
}

func (e *fidEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.app.DB().QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *fidEnv) account(slug string) store.Account {
	e.t.Helper()
	a, err := e.core().GetAccountBySlug(context.Background(), slug)
	if err != nil {
		e.t.Fatalf("account %s: %v", slug, err)
	}
	return a
}

func (e *fidEnv) recon(slug string) store.Reconciliation {
	e.t.Helper()
	r, err := e.core().GetReconciliation(context.Background(), e.account(slug).ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// fidHistory is a made-up transaction history for two accounts, named by their numbers: a SIPP
// holding 10 units and 889.50 of cash, and a cash account holding 5.00.
func fidHistory(sipp, cash string) []string {
	return []string{
		fidRow("01 Mar 2030", "01 Mar 2030", "Cash In For Transfer", "Cash", fidSIPP, sipp, "", "1000.00", "1000.00", "", "AC000001", "Completed"),
		fidRow("05 Mar 2030", "07 Mar 2030", "Buy", fidFund, fidSIPP, sipp, "", "107.50", "10", "10.00", "AC000002", "Completed"),
		fidRow("07 Mar 2030", "07 Mar 2030", "Cash Out For Buy", "Cash", fidSIPP, sipp, fidFund, "-100.00", "100.00", "", "AC000003", "Completed"),
		fidRow("07 Mar 2030", "07 Mar 2030", "Dealing Fee", "Cash", fidSIPP, sipp, fidFund, "-7.50", "0", "", "AC000004", "Completed"),
		fidRow("20 Mar 2030", "20 Mar 2030", "Cash Interest", "Cash", fidSIPP, sipp, "", "2.00", "0", "", "AC000005", "Completed"),
		fidRow("20 Mar 2030", "20 Mar 2030", "Tax On Interest", "Cash", fidSIPP, sipp, "", "-0.40", "0", "", "AC000006", "Completed"),
		fidRow("25 Mar 2030", "25 Mar 2030", "Transfer To Cash Management Account For Fees", "Cash", fidSIPP, sipp, "", "-5.00", "5.00", "", "AC000007", "Completed"),
		fidRow("25 Mar 2030", "25 Mar 2030", "Cash In Ring-fenced For Fees", "Cash", fidCMA, cash, "", "5.00", "5.00", "", "AC000008", "Completed"),
	}
}

func fidSnapshot(sipp, cash, sippCash, units, value, total string) []byte {
	return fidSummary(
		fidSumRow("Account", "", sipp, fidSIPP, "", "", total, "", sippCash, "", ""),
		fidSumRow("Asset", fidFund, sipp, fidSIPP, "1050.5", units, value, "GBP", "", "", ""),
		fidSumRow("Account", "", cash, fidCMA, "", "", "5.00", "", "5.00", "", ""))
}

// AC5: a transactions file and a summary file reach reconciled with the expected units and cash,
// and a summary that disagrees with the history reaches mismatch, so the pass is not vacuous.
// AC6: nothing the files hold reaches a log line.
func TestFidelityPairReconcilesThroughTheLedger(t *testing.T) {
	e := newFidEnv(t)
	for _, slug := range []string{"fid-ok", "fid-cash", "fid-units"} {
		e.platform(slug)
	}

	// Reconciled. The summary is printed at 10:30 on 2 April, and the history is imported after it,
	// in either order.
	ok := fidTx(fidHistory(fidAcct, fidCashAct)...)
	if st := e.do("fid-ok", importer.FormatFidelitySummaryCSV, fidSnapshot(fidAcct, fidCashAct, "889.50", "10", "105.05", "994.55")); st.FilesRejected != 0 {
		t.Fatalf("summary rejected: %v", st.Rejections)
	}
	if rec := e.recon("fid-ok-sipp-1234"); rec.LastCheckOK != nil {
		t.Fatalf("a summary with no history was checked: %+v", rec)
	}
	if st := e.do("fid-ok", importer.FormatFidelityTransactionsCSV, ok); st.FilesRejected != 0 {
		t.Fatalf("history rejected: %v", st.Rejections)
	}
	sipp, cash := e.recon("fid-ok-sipp-1234"), e.recon("fid-ok-cash-5678")
	for name, rec := range map[string]store.Reconciliation{"sipp": sipp, "cash": cash} {
		if rec.Method != "holdings_replay" || rec.LastCheckOK == nil || !*rec.LastCheckOK || rec.BreakCount != 0 ||
			rec.LastDiffMinor == nil || *rec.LastDiffMinor != 0 || rec.ReconciledThrough == nil || !rec.ReconciledThrough.Equal(fidDay("2030-04-02")) {
			t.Fatalf("%s reconciliation %+v", name, rec)
		}
	}
	checks, err := e.core().ListHoldingChecks(context.Background(), e.account("fid-ok-sipp-1234").ID)
	if err != nil || len(checks) != 1 || checks[0].ExpectedUnits != "10" || checks[0].ReportedUnits != "10" || checks[0].Difference != "0" {
		t.Fatalf("holding checks %+v, %v", checks, err)
	}
	var cashMinor int64
	if err := e.app.DB().QueryRow(context.Background(), `SELECT sum(cash_minor) FROM core.investment_event WHERE account_id = $1`,
		e.account("fid-ok-sipp-1234").ID).Scan(&cashMinor); err != nil || cashMinor != 88950 {
		t.Fatalf("replayed cash %d, %v", cashMinor, err)
	}
	if e.account("fid-ok-sipp-1234").Type != "pension" || e.account("fid-ok-cash-5678").Type != "investment_cash" {
		t.Fatal("child account types")
	}

	// Cash that differs from the history by 1.00 is a mismatch with that difference.
	e.do("fid-cash", importer.FormatFidelityTransactionsCSV, fidTx(fidHistory("ACC00002222", "ACC00003333")...))
	e.do("fid-cash", importer.FormatFidelitySummaryCSV, fidSnapshot("ACC00002222", "ACC00003333", "890.50", "10", "105.05", "995.55"))
	if rec := e.recon("fid-cash-sipp-2222"); rec.LastCheckOK == nil || *rec.LastCheckOK || rec.BreakCount != 1 || rec.LastDiffMinor == nil || *rec.LastDiffMinor != 100 {
		t.Fatalf("cash mismatch: %+v", rec)
	}
	// Units that differ from the history are a mismatch with no cash difference.
	e.do("fid-units", importer.FormatFidelityTransactionsCSV, fidTx(fidHistory("ACC00004444", "ACC00006666")...))
	e.do("fid-units", importer.FormatFidelitySummaryCSV, fidSnapshot("ACC00004444", "ACC00006666", "889.50", "11", "115.56", "1005.06"))
	if rec := e.recon("fid-units-sipp-4444"); rec.LastCheckOK == nil || *rec.LastCheckOK || rec.BreakCount != 1 || rec.LastDiffMinor == nil || *rec.LastDiffMinor != 0 {
		t.Fatalf("units mismatch: %+v", rec)
	}
	checks, err = e.core().ListHoldingChecks(context.Background(), e.account("fid-units-sipp-4444").ID)
	if err != nil || len(checks) != 1 || checks[0].ExpectedUnits != "10" || checks[0].ReportedUnits != "11" || checks[0].Difference != "1" {
		t.Fatalf("units checks %+v, %v", checks, err)
	}

	// AC6: no log line carries an amount, holding, name or file content.
	logs := e.logs.String()
	for _, s := range []string{"ACC0000", "EXAMPLE", "EXMP", "Made-up", "1050.5", "107.50", "889.50", "994.55", "AC00000"} {
		if strings.Contains(logs, s) {
			t.Errorf("a log line carries %q", s)
		}
	}
}

// AC3: two overlapping exports store each reference once. AC2: a refused file stores no rows.
func TestFidelityOverlapStoresEachReferenceOnceAndARefusalStoresNothing(t *testing.T) {
	e := newFidEnv(t)
	e.platform("fid")
	rows := fidHistory(fidAcct, fidCashAct)

	// Refused files first: each is stored in raw, rejected as check_failed, and writes nothing else.
	bad := map[string][]byte{
		"unknown type":       fidTx(append(fidClone(rows), fidCash("Cash Dividend", fidAcct, fidSIPP, "1.00", "BAD00001"))...),
		"contradictory sign": fidTx(append(fidClone(rows), fidSec("Buy", fidFund, fidAcct, "-1.00", "1", "1.00", "BAD00002"))...),
		"unknown product":    fidTx(append(fidClone(rows), fidRow("05 Mar 2030", "07 Mar 2030", "Cash Interest", "Cash", "Made-up Wrapper", fidAcct, "", "1.00", "0", "", "BAD00003", "Completed"))...),
		"blank reference":    fidTx(append(fidClone(rows), fidCash("Cash Interest", fidAcct, fidSIPP, "1.00", ""))...),
		"repeated reference": fidTx(append(fidClone(rows), rows[0])...),
		"changed header":     bytes.Replace(fidTx(rows...), []byte("Amount,"), []byte("Value,"), 1),
	}
	for name, body := range bad {
		st := e.do("fid", importer.FormatFidelityTransactionsCSV, body)
		if st.FilesRejected != 1 || len(st.Rejections) != 1 || st.Rejections[0] != "check_failed" {
			t.Fatalf("%s: rejections %v (%d)", name, st.Rejections, st.FilesRejected)
		}
	}
	for _, table := range []string{"core.investment_event", "core.investment_import", "core.instrument", "core.valuation", "core.holding_snapshot"} {
		if n := e.count(`SELECT count(*) FROM ` + table); n != 0 {
			t.Fatalf("%s holds %d rows after refused files", table, n)
		}
	}
	if n := e.count(`SELECT count(*) FROM core.account WHERE parent_id IS NOT NULL`); n != 0 {
		t.Fatalf("a refused file created %d child accounts", n)
	}
	if n := e.count(`SELECT count(*) FROM raw.import_file`); n != len(bad) {
		t.Fatalf("raw holds %d files, want %d", n, len(bad))
	}

	// Two exports whose date ranges overlap: references 3 to 6 are in both.
	first := fidTx(rows[:6]...)
	second := fidTx(rows[2:]...)
	if st := e.do("fid", importer.FormatFidelityTransactionsCSV, first); st.FilesRejected != 0 {
		t.Fatalf("first export: %v", st.Rejections)
	}
	if st := e.do("fid", importer.FormatFidelityTransactionsCSV, second); st.FilesRejected != 0 {
		t.Fatalf("second export: %v", st.Rejections)
	}
	if n := e.count(`SELECT count(*) FROM core.investment_event`); n != len(rows) {
		t.Fatalf("%d events, want %d", n, len(rows))
	}
	if n := e.count(`SELECT count(DISTINCT source_key) FROM core.investment_event`); n != len(rows) {
		t.Fatalf("%d distinct references, want %d", n, len(rows))
	}
	if n := e.count(`SELECT count(*) FROM core.investment_event WHERE version_count <> 1`); n != 0 {
		t.Fatalf("%d events have more than one version", n)
	}
	if n := e.count(`SELECT count(*) FROM core.investment_event_version`); n != len(rows) {
		t.Fatalf("%d versions, want %d", n, len(rows))
	}
	if n := e.count(`SELECT count(*) FROM core.investment_event WHERE disappeared_at IS NOT NULL`); n != 0 {
		t.Fatalf("%d events disappeared", n)
	}
	// Each file is counted by its own rows.
	if n := e.count(`SELECT sum(row_count) FROM core.investment_import`); n != len(rows[:6])+len(rows[2:]) {
		t.Fatalf("row counts sum to %d", n)
	}
}

func fidClone(s []string) []string { return append([]string(nil), s...) }

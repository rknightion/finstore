package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/importer"
	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/store"
)

// All values here are made up. The fake normalisers read a file that is the JSON of the result it
// should yield, so a test states the result and the ledger's handling of it is what is tested.
const (
	fakeFidelityFormat = "fake-fidelity"
	fakeAvivaFormat    = "fake-aviva"
)

type fakeInvestment struct{ provider string }

func (f fakeInvestment) Provider() string { return f.provider }

func (f fakeInvestment) NormaliseInvestmentFile(file ledger.File, _ *time.Location) (ledger.InvestmentResult, error) {
	var r ledger.InvestmentResult
	if err := json.Unmarshal(file.Body, &r); err != nil {
		return r, errors.New("fake: unreadable")
	}
	return r, nil
}

func newInvestmentEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.l = ledger.New(nil, map[string]ledger.FileNormaliser{
		string(importer.FormatHalifaxPDF): importer.Normaliser{},
		string(importer.FormatManualCSV):  importer.Normaliser{},
	}, time.UTC, slog.New(slog.NewJSONHandler(e.logs, nil)), nil, ledger.WithInvestmentNormalisers(map[string]ledger.InvestmentNormaliser{
		fakeFidelityFormat: fakeInvestment{ledger.ProviderFidelity},
		fakeAvivaFormat:    fakeInvestment{ledger.ProviderAviva},
	}))
	return e
}

func (e *env) platform(slug, provider string) {
	e.t.Helper()
	e.operator(ledger.EntryAccountAdd, ledger.AccountAdd{Slug: slug, Type: "platform", Currency: "GBP", Name: "Made-up",
		ExpectsImports: true, Provider: provider})
	e.catchUp()
}

// investAt stores r as a file imported at the given instant and normalises it.
func (e *env) investAt(slug, format string, r ledger.InvestmentResult, at time.Time) ledger.Stats {
	e.t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, _, err := store.InsertImportFile(context.Background(), e.app.DB(), store.ImportFile{AccountSlug: slug, Format: format,
		FileName: "made-up", ImportedAt: at, Body: b}); err != nil {
		e.t.Fatal(err)
	}
	return e.catchUp()
}

func (e *env) invest(slug, format string, r ledger.InvestmentResult) ledger.Stats {
	e.t.Helper()
	return e.investAt(slug, format, r, e.tick())
}

func (e *env) wantRejected(st ledger.Stats, category string) {
	e.t.Helper()
	if st.FilesRejected != 1 || len(st.Rejections) != 1 || st.Rejections[0] != category {
		e.t.Fatalf("rejections %v (%d), want %s", st.Rejections, st.FilesRejected, category)
	}
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.app.DB().QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func sipp(key string) []ledger.InvestmentAccountRec {
	return []ledger.InvestmentAccountRec{{AccountKey: key, Product: ledger.ProductSIPP}}
}

func plan(key string) []ledger.InvestmentAccountRec {
	return []ledger.InvestmentAccountRec{{AccountKey: key, Product: ledger.ProductPensionPlan}}
}

func hashed(parts ...any) (json.RawMessage, []byte) {
	b, _ := json.Marshal(parts)
	return b, b
}

func trade(acct, key, kind, ticker, order, settle, units string) ledger.InvestmentEventRec {
	c, h := hashed(key, kind, ticker, order, settle, units)
	r := ledger.InvestmentEventRec{AccountKey: acct, SourceKey: key, EventType: kind, ProviderType: "Made-up", InstrumentKey: ticker,
		InstrumentName: "Made-up Fund", OrderDate: date(order), Units: units, Content: c, HashInput: h}
	if settle != "" {
		r.SettleDate = date(settle)
	}
	return r
}

func cashRow(acct, key, kind, on string, cash int64) ledger.InvestmentEventRec {
	c, h := hashed(key, kind, on, cash)
	return ledger.InvestmentEventRec{AccountKey: acct, SourceKey: key, EventType: kind, ProviderType: "Made-up", OrderDate: date(on),
		GrossMinor: cash, CashMinor: cash, FlowMinor: cash, Content: c, HashInput: h}
}

func contribution(acct, key, on string, amount int64) ledger.InvestmentEventRec {
	c, h := hashed(key, on, amount)
	return ledger.InvestmentEventRec{AccountKey: acct, SourceKey: key, EventType: ledger.EventContribution, ProviderType: "Regular",
		OrderDate: date(on), GrossMinor: amount, FlowMinor: amount, Content: c, HashInput: h}
}

func transferOut(acct, key, on string, amount int64) ledger.InvestmentEventRec {
	c, h := hashed(key, on, amount)
	return ledger.InvestmentEventRec{AccountKey: acct, SourceKey: key, EventType: ledger.EventTransferOut, ProviderType: "Payment out",
		OrderDate: date(on), GrossMinor: -amount, FlowMinor: -amount, Content: c, HashInput: h}
}

func ledgerFile(acct string, events ...ledger.InvestmentEventRec) ledger.InvestmentResult {
	return ledger.InvestmentResult{Kind: ledger.InvestmentKindLedger, Accounts: sipp(acct), Events: events, RowCount: len(events)}
}

// summary is a holdings file on asOf, captured at capturedAt, with cash and tickers to units.
func summary(acct, asOf string, capturedAt time.Time, cash int64, units map[string]string) ledger.InvestmentResult {
	r := ledger.InvestmentResult{Kind: ledger.InvestmentKindHoldings, CapturedAt: &capturedAt, Accounts: sipp(acct),
		Valuations: []ledger.ValuationRec{{AccountKey: acct, AsOf: date(asOf), TotalMinor: 100000, CashMinor: cash}}, RowCount: 1 + len(units)}
	for ticker, u := range units {
		r.Holdings = append(r.Holdings, ledger.HoldingRec{AccountKey: acct, InstrumentKey: ticker, InstrumentName: "Made-up Fund",
			AsOf: date(asOf), Units: u, Price: "1.25", PriceUnit: ledger.PriceUnitGBP, ValueMinor: 1000})
	}
	return r
}

func statementFile(acct, start, end, issued string, opening, paidIn, closing int64, events ...ledger.InvestmentEventRec) ledger.InvestmentResult {
	return ledger.InvestmentResult{Kind: ledger.InvestmentKindStatement, Accounts: plan(acct), Events: events, RowCount: 1 + len(events),
		Statements: []ledger.PensionStatementRec{{AccountKey: acct, PeriodStart: date(start), PeriodEnd: date(end), IssuedOn: date(issued),
			OpeningMinor: opening, PaidInMinor: paidIn, GrowthMinor: closing - opening - paidIn, ClosingMinor: closing,
			Charges: json.RawMessage(`[]`), Projections: json.RawMessage(`{}`)}}}
}

func capture(acct, asOf string, capturedAt time.Time, complete bool, contributions ...ledger.InvestmentEventRec) ledger.InvestmentResult {
	r := ledger.InvestmentResult{Kind: ledger.InvestmentKindCapture, CapturedAt: &capturedAt, Accounts: plan(acct), Events: contributions,
		Valuations: []ledger.ValuationRec{{AccountKey: acct, AsOf: date(asOf), TotalMinor: 100000}},
		Holdings: []ledger.HoldingRec{{AccountKey: acct, InstrumentKey: "Example Growth Fund", InstrumentName: "Example Growth Fund",
			AsOf: date(asOf), Units: "1000.0000", Price: "1.0000", PriceUnit: ledger.PriceUnitGBP, ValueMinor: 100000}},
		RowCount: 1 + len(contributions)}
	if complete {
		r.CompleteContributionsFor = []string{acct}
	}
	return r
}

func TestNormaliserRegistrationRefusals(t *testing.T) {
	e := newInvestmentEnv(t)
	e.platform("fid", ledger.ProviderFidelity)
	e.platform("avi", ledger.ProviderAviva)
	e.operator(ledger.EntryAccountAdd, ledger.AccountAdd{Slug: "bank-joint", Type: "joint", Currency: "GBP", Name: "Made-up"})
	e.catchUp()
	if n := e.count(`SELECT count(*) FROM core.reconciliation r JOIN core.account a ON a.id = r.account_id WHERE a.type = 'platform'`); n != 0 {
		t.Fatalf("a platform has %d reconciliation rows", n)
	}
	good := ledgerFile("ACC00001234", cashRow("ACC00001234", "R1", ledger.EventInterest, "2030-01-05", 100))

	// A format imports only against a platform of its provider.
	e.wantRejected(e.invest("avi", fakeFidelityFormat, good), "account_mismatch")
	e.wantRejected(e.invest("fid", fakeAvivaFormat, ledger.InvestmentResult{Kind: ledger.InvestmentKindLedger}), "account_mismatch")
	e.wantRejected(e.invest("bank-joint", fakeFidelityFormat, good), "account_mismatch")
	if st := e.invest("fid", fakeFidelityFormat, good); st.FilesRejected != 0 {
		t.Fatalf("a good file: %v", st.Rejections)
	}
	child := e.account("fid-sipp-1234")
	if child.ParentID == nil || *child.ParentID != e.account("fid").ID || child.Type != "pension" || child.SourceKey != "ACC00001234" ||
		child.Source != "investment:fidelity" || !child.ExpectsImports || child.Feed != "file" {
		t.Fatalf("child %+v", child)
	}
	// Nor against a child.
	e.wantRejected(e.invest(child.Slug, fakeFidelityFormat, ledgerFile("ACC00001234",
		cashRow("ACC00001234", "R2", ledger.EventInterest, "2030-01-06", 1))), "account_mismatch")

	// A bank format is refused on any investment account.
	e.importFile("fid", string(importer.FormatManualCSV), csvFile(0, csvRow{"2030-01-01", "PAY", 100, "made-up"}))
	e.wantRejected(e.catchUp(), "account_mismatch")
	e.importFile(child.Slug, string(importer.FormatManualCSV), csvFile(0, csvRow{"2030-01-02", "PAY", 100, "made-up"}))
	e.wantRejected(e.catchUp(), "account_mismatch")

	// A provider account stays under its first platform.
	e.platform("fid-two", ledger.ProviderFidelity)
	e.wantRejected(e.invest("fid-two", fakeFidelityFormat, ledgerFile("ACC00001234",
		cashRow("ACC00001234", "R3", ledger.EventInterest, "2030-01-07", 1))), "account_mismatch")

	// A slug another account has.
	e.operator(ledger.EntryAccountAdd, ledger.AccountAdd{Slug: "fid-sipp-9999", Type: "joint", Currency: "GBP", Name: "Made-up"})
	e.catchUp()
	e.wantRejected(e.invest("fid", fakeFidelityFormat, ledgerFile("ACC00009999",
		cashRow("ACC00009999", "R4", ledger.EventInterest, "2030-01-08", 1))), "child_key_shared")

	// The parser's own failure, and a result that breaks the contract: nothing is stored.
	if _, _, err := store.InsertImportFile(context.Background(), e.app.DB(), store.ImportFile{AccountSlug: "fid", Format: fakeFidelityFormat,
		FileName: "made-up", ImportedAt: e.tick(), Body: []byte("not json")}); err != nil {
		t.Fatal(err)
	}
	e.wantRejected(e.catchUp(), "check_failed")
	before := e.count(`SELECT count(*) FROM core.investment_event`)
	for name, r := range map[string]ledger.InvestmentResult{
		"unknown kind":        {Kind: "made-up"},
		"unknown product":     {Kind: ledger.InvestmentKindLedger, Accounts: []ledger.InvestmentAccountRec{{AccountKey: "ACC00005555", Product: "made-up"}}},
		"product of another":  {Kind: ledger.InvestmentKindLedger, Accounts: plan("ACC00005555")},
		"short account":       {Kind: ledger.InvestmentKindLedger, Accounts: sipp("A-1")},
		"account not listed":  {Kind: ledger.InvestmentKindLedger, Accounts: sipp("ACC00005555"), Events: []ledger.InvestmentEventRec{cashRow("ACC00006666", "R5", ledger.EventFee, "2030-01-09", -1)}},
		"repeated key":        ledgerFile("ACC00005555", cashRow("ACC00005555", "R6", ledger.EventFee, "2030-01-09", -1), cashRow("ACC00005555", "R6", ledger.EventFee, "2030-01-10", -1)),
		"buy without units":   ledgerFile("ACC00005555", cashRow("ACC00005555", "R7", ledger.EventBuy, "2030-01-09", -1)),
		"fee with units":      ledgerFile("ACC00005555", trade("ACC00005555", "R8", ledger.EventFee, "EXMP", "2030-01-09", "", "1")),
		"bad decimal":         ledgerFile("ACC00005555", trade("ACC00005555", "R9", ledger.EventBuy, "EXMP", "2030-01-09", "", "1e3")),
		"negative units":      ledgerFile("ACC00005555", trade("ACC00005555", "R10", ledger.EventBuy, "EXMP", "2030-01-09", "", "-1")),
		"holdings no capture": {Kind: ledger.InvestmentKindHoldings, Accounts: sipp("ACC00005555")},
		"holding no valuation": {Kind: ledger.InvestmentKindHoldings, CapturedAt: ptr(e.clock), Accounts: sipp("ACC00005555"),
			Holdings: []ledger.HoldingRec{{AccountKey: "ACC00005555", InstrumentKey: "EXMP", InstrumentName: "x", AsOf: date("2030-01-09"), Units: "1", Price: "1", PriceUnit: "GBP"}}},
		"trade note listing accounts":                              {Kind: ledger.InvestmentKindTradeNote, Accounts: sipp("ACC00005555"), TradeNotes: []ledger.TradeNoteRec{madeUpNote("ACC00005555", "N1", e.clock)}},
		"trade note, no accounts in the result but a short number": {Kind: ledger.InvestmentKindTradeNote, TradeNotes: []ledger.TradeNoteRec{madeUpNote("A-1", "N2", e.clock)}},
		"statement in a ledger":                                    {Kind: ledger.InvestmentKindLedger, Accounts: sipp("ACC00005555"), Statements: []ledger.PensionStatementRec{{AccountKey: "ACC00005555"}}},
		"complete with no valuation": {Kind: ledger.InvestmentKindCapture, CapturedAt: ptr(e.clock), Accounts: sipp("ACC00005555"),
			CompleteContributionsFor: []string{"ACC00005555"}},
		"date not at midnight": ledgerFile("ACC00005555", func() ledger.InvestmentEventRec {
			r := cashRow("ACC00005555", "R11", ledger.EventFee, "2030-01-09", -1)
			r.OrderDate = r.OrderDate.Add(time.Hour)
			return r
		}()),
	} {
		t.Run(name, func(t *testing.T) { e.t = t; e.wantRejected(e.invest("fid", fakeFidelityFormat, r), "bad_result") })
	}
	e.t = t
	if after := e.count(`SELECT count(*) FROM core.investment_event`); after != before {
		t.Fatalf("a rejected result stored rows: %d, was %d", after, before)
	}
	if _, err := e.core().GetAccountBySlug(context.Background(), "fid-sipp-5555"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a rejected file created a child: %v", err)
	}
	// The category reaches the log, the file's content never does.
	if got := e.logs.String(); !strings.Contains(got, "child_key_shared") || !strings.Contains(got, "account_mismatch") ||
		strings.Contains(got, "ACC0000") || strings.Contains(got, "made-up") {
		t.Fatalf("log: %s", got)
	}
}

func madeUpNote(acct, reference string, at time.Time) ledger.TradeNoteRec {
	return ledger.TradeNoteRec{AccountKey: acct, Reference: reference, ISIN: "GB00EXAMPLE1", ExecutedAt: at, Venue: "Made-up", Units: "1.5",
		ExactPrice: "12.3456", GrossMinor: 1800, FeeMinor: 10, NetMinor: 1810, SettleDate: date("2030-02-03")}
}

// A result creates a child account, instruments, events, snapshots, valuations, statements and
// notes, and a byte-identical file adds nothing.
func TestInvestmentResultCreatesItsRows(t *testing.T) {
	e := newInvestmentEnv(t)
	ctx := context.Background()
	e.platform("fid", ledger.ProviderFidelity)
	e.platform("avi", ledger.ProviderAviva)
	const acct, avivaAcct = "ACC00001234", "PLAN00005678"

	// A note imported before any file names its account is stored and creates no child.
	early := ledger.InvestmentResult{Kind: ledger.InvestmentKindTradeNote, RowCount: 1, TradeNotes: []ledger.TradeNoteRec{madeUpNote(acct, "R1", e.clock)}}
	if st := e.invest("fid", fakeFidelityFormat, early); st.FilesRejected != 0 || st.AccountsCreated != 0 {
		t.Fatalf("an early note: %+v", st)
	}
	if n := e.count(`SELECT count(*) FROM core.account WHERE parent_id = $1`, e.account("fid").ID); n != 0 {
		t.Fatalf("a note created %d child accounts", n)
	}
	if n := e.count(`SELECT count(*) FROM core.trade_note WHERE platform_id = $1 AND account_key = $2 AND reference = 'R1'`, e.account("fid").ID, acct); n != 1 {
		t.Fatalf("%d early notes stored", n)
	}

	buy := trade(acct, "R1", ledger.EventBuy, "EXMP", "2030-01-05", "2030-01-07", "10.5")
	buy.Price, buy.PriceUnit, buy.GrossMinor = "1234.56789", ledger.PriceUnitGBX, 13000
	lf := ledgerFile(acct, buy, cashRow(acct, "R2", ledger.EventInternal, "2030-01-07", -12000))
	if st := e.invest("fid", fakeFidelityFormat, lf); st.FilesRejected != 0 || st.AccountsCreated != 1 {
		t.Fatalf("ledger file: %+v", st)
	}
	child := e.account("fid-sipp-1234")
	if n := e.count(`SELECT count(*) FROM core.investment_event WHERE account_id = $1`, child.ID); n != 2 {
		t.Fatalf("%d events", n)
	}
	// Once the child and event exist, the early note joins them.
	if n := e.count(`SELECT count(*) FROM core.trade_note t
		JOIN core.account c ON c.parent_id = t.platform_id AND c.source_key = t.account_key
		JOIN core.investment_event v ON v.account_id = c.id AND v.source_key = t.reference
		WHERE c.id = $1 AND t.reference = 'R1'`, child.ID); n != 1 {
		t.Fatalf("the early note joins %d events", n)
	}
	id := ledger.InvestmentEventID(child.ID, "investment:fidelity", "R1")
	ev, err := e.core().GetInvestmentEvent(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Units == nil || *ev.Units != "10.5" || ev.Price == nil || *ev.Price != "1234.56789" || *ev.PriceUnit != "GBX" || ev.VersionCount != 1 ||
		ev.InstrumentID == nil || *ev.InstrumentID != ledger.InstrumentID("fidelity", "EXMP") {
		t.Fatalf("event %+v", ev)
	}
	var ticker *string
	if err := e.app.DB().QueryRow(ctx, `SELECT ticker FROM core.instrument WHERE provider = 'fidelity' AND provider_key = 'EXMP'`).Scan(&ticker); err != nil || ticker == nil || *ticker != "EXMP" {
		t.Fatalf("ticker %v, %v", ticker, err)
	}

	// An overlapping export repeats a row: no duplicate and no new version.
	lf2 := ledgerFile(acct, buy, cashRow(acct, "R3", ledger.EventInterest, "2030-01-20", 5))
	e.invest("fid", fakeFidelityFormat, lf2)
	if n := e.count(`SELECT count(*) FROM core.investment_event WHERE account_id = $1`, child.ID); n != 3 {
		t.Fatalf("%d events after an overlap", n)
	}
	if n := e.count(`SELECT count(*) FROM core.investment_event_version WHERE event_id = $1`, id); n != 1 {
		t.Fatalf("%d versions after an identical sighting", n)
	}
	// A changed row gets a new version.
	changed := buy
	changed.Content, changed.HashInput = hashed("changed")
	e.invest("fid", fakeFidelityFormat, ledgerFile(acct, changed))
	if ev, _ := e.core().GetInvestmentEvent(ctx, id); ev.VersionCount != 2 {
		t.Fatalf("version count %d after a change", ev.VersionCount)
	}

	at := e.clock
	sf := summary(acct, "2030-02-01", at, 0, map[string]string{"EXMP": "10.5"})
	e.invest("fid", fakeFidelityFormat, sf)
	note := ledger.InvestmentResult{Kind: ledger.InvestmentKindTradeNote, RowCount: 1, TradeNotes: []ledger.TradeNoteRec{{
		AccountKey: acct, Reference: "R-NOT-YET", ISIN: "GB00EXAMPLE1", ExecutedAt: at, Venue: "Made-up", Units: "1.5", ExactPrice: "12.3456",
		GrossMinor: 1800, FeeMinor: 10, TaxMinor: 0, NetMinor: 1810, SettleDate: date("2030-02-03")}}}
	e.invest("fid", fakeFidelityFormat, note)
	if n := e.count(`SELECT count(*) FROM core.trade_note WHERE platform_id = $1 AND account_key = $2 AND reference = 'R-NOT-YET' AND exact_price = 12.3456`, e.account("fid").ID, acct); n != 1 {
		t.Fatalf("%d trade notes", n)
	}
	if n := e.count(`SELECT count(*) FROM core.holding_snapshot WHERE account_id = $1`, child.ID); n != 1 {
		t.Fatalf("%d holdings", n)
	}
	if n := e.count(`SELECT count(*) FROM core.valuation WHERE account_id = $1`, child.ID); n != 1 {
		t.Fatalf("%d valuations", n)
	}

	stmt := statementFile(avivaAcct, "2029-04-06", "2030-04-05", "2030-05-01", 1000, 0, 1100)
	e.invest("avi", fakeAvivaFormat, stmt)
	avChild := e.account("avi-pension-5678")
	if avChild.Type != "pension" {
		t.Fatalf("aviva child %+v", avChild)
	}
	if n := e.count(`SELECT count(*) FROM core.pension_statement WHERE account_id = $1 AND is_winner`, avChild.ID); n != 1 {
		t.Fatalf("%d winning statements", n)
	}
	if n := e.count(`SELECT count(*) FROM core.investment_import`); n != 7 {
		t.Fatalf("%d investment imports", n)
	}

	// The same bytes again store nothing.
	before := e.count(`SELECT count(*) FROM raw.import_file`)
	e.invest("fid", fakeFidelityFormat, sf)
	if after := e.count(`SELECT count(*) FROM raw.import_file`); after != before {
		t.Fatalf("a repeat file added a raw row: %d, was %d", after, before)
	}
}

// reconState is the fields the design's outcome table fixes.
func (e *env) wantRecon(slug string, ok *bool, diff *int64, through string, breaks int) store.Reconciliation {
	e.t.Helper()
	r := e.recon(e.account(slug).ID)
	if (r.LastCheckOK == nil) != (ok == nil) || (ok != nil && *r.LastCheckOK != *ok) {
		e.t.Fatalf("%s last_check_ok %v, want %v", slug, deref(r.LastCheckOK), deref(ok))
	}
	if (r.LastDiffMinor == nil) != (diff == nil) || (diff != nil && *r.LastDiffMinor != *diff) {
		e.t.Fatalf("%s last_diff_minor %v, want %v", slug, deref(r.LastDiffMinor), deref(diff))
	}
	gotThrough := ""
	if r.ReconciledThrough != nil {
		gotThrough = r.ReconciledThrough.Format(time.DateOnly)
	}
	if gotThrough != through || r.BreakCount != breaks {
		e.t.Fatalf("%s through %q breaks %d, want %q %d", slug, gotThrough, r.BreakCount, through, breaks)
	}
	if (ok == nil) != (r.LastCheckAt == nil) {
		e.t.Fatalf("%s last_check_at %v with ok %v", slug, r.LastCheckAt, deref(ok))
	}
	return r
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestHoldingsReplayOutcomes(t *testing.T) {
	e := newInvestmentEnv(t)
	yes, no := ptr(true), ptr(false)
	zero := ptr(int64(0))
	next := 0
	// fresh makes a platform with one account and returns its slugs.
	fresh := func() (platform, acct, child string) {
		next++
		platform = fmt.Sprintf("p%d", next)
		acct = fmt.Sprintf("ACC%08d", 1000+next)
		e.platform(platform, ledger.ProviderFidelity)
		return platform, acct, fmt.Sprintf("%s-sipp-%04d", platform, 1000+next)
	}
	base := func(acct string) []ledger.InvestmentEventRec {
		buy := trade(acct, acct+"-B", ledger.EventBuy, "EXMP", "2030-01-05", "2030-01-07", "10.5")
		return []ledger.InvestmentEventRec{buy, cashRow(acct, acct+"-C", ledger.EventTransferIn, "2030-01-02", 5000),
			cashRow(acct, acct+"-O", ledger.EventInternal, "2030-01-07", -4000)}
	}

	t.Run("pass, then a cash difference keeps the pass fields", func(t *testing.T) {
		e.t = t
		p, acct, child := fresh()
		e.invest(p, fakeFidelityFormat, ledgerFile(acct, base(acct)...))
		// Nothing checkable: no snapshot yet.
		e.wantRecon(child, nil, nil, "", 0)
		exported := e.clock
		e.invest(p, fakeFidelityFormat, summary(acct, "2030-02-01", exported, 1000, map[string]string{"EXMP": "10.50"}))
		pass := e.wantRecon(child, yes, zero, "2030-02-01", 0)
		if pass.LastPassAt == nil || !pass.LastPassAt.Equal(*pass.LastCheckAt) {
			t.Fatalf("pass times %v %v", pass.LastPassAt, pass.LastCheckAt)
		}
		checks, err := e.core().ListHoldingChecks(context.Background(), e.account(child).ID)
		if err != nil || len(checks) != 1 || checks[0].Difference != "0.00" {
			t.Fatalf("holding checks %+v, %v", checks, err)
		}
		e.invest(p, fakeFidelityFormat, summary(acct, "2030-03-01", exported, 900, map[string]string{"EXMP": "10.5"}))
		cash := e.wantRecon(child, no, ptr(int64(-100)), "2030-02-01", 1)
		if !cash.LastPassAt.Equal(*pass.LastPassAt) || !cash.LastCheckAt.After(*pass.LastCheckAt) {
			t.Fatalf("a cash difference moved last_pass_at: %v, last_check_at %v", cash.LastPassAt, cash.LastCheckAt)
		}
	})

	t.Run("a units-only difference", func(t *testing.T) {
		e.t = t
		p, acct, child := fresh()
		e.invest(p, fakeFidelityFormat, ledgerFile(acct, base(acct)...))
		e.invest(p, fakeFidelityFormat, summary(acct, "2030-02-01", e.clock, 1000, map[string]string{"EXMP": "10.25"}))
		r := e.wantRecon(child, no, zero, "", 1)
		if r.LastPassAt != nil {
			t.Fatalf("last_pass_at %v", r.LastPassAt)
		}
		checks, _ := e.core().ListHoldingChecks(context.Background(), e.account(child).ID)
		if len(checks) != 1 || checks[0].ExpectedUnits != "10.50" || checks[0].ReportedUnits != "10.25" || checks[0].Difference != "-0.25" {
			t.Fatalf("checks %+v", checks)
		}
	})

	t.Run("an instrument missing from the snapshot", func(t *testing.T) {
		e.t = t
		p, acct, child := fresh()
		e.invest(p, fakeFidelityFormat, ledgerFile(acct, base(acct)...))
		e.invest(p, fakeFidelityFormat, summary(acct, "2030-02-01", e.clock, 1000, nil))
		e.wantRecon(child, no, zero, "", 1)
		checks, _ := e.core().ListHoldingChecks(context.Background(), e.account(child).ID)
		if len(checks) != 1 || checks[0].ReportedUnits != "0.0" || checks[0].Difference != "-10.5" {
			t.Fatalf("checks %+v", checks)
		}
	})

	t.Run("an instrument missing from the ledger", func(t *testing.T) {
		e.t = t
		p, acct, child := fresh()
		e.invest(p, fakeFidelityFormat, ledgerFile(acct, base(acct)...))
		e.invest(p, fakeFidelityFormat, summary(acct, "2030-02-01", e.clock, 1000, map[string]string{"EXMP": "10.5", "OTHR": "2"}))
		e.wantRecon(child, no, zero, "", 1)
		checks, _ := e.core().ListHoldingChecks(context.Background(), e.account(child).ID)
		if len(checks) != 2 {
			t.Fatalf("checks %+v", checks)
		}
	})

	t.Run("a position sold to zero and absent from the snapshot passes", func(t *testing.T) {
		e.t = t
		p, acct, child := fresh()
		events := append(base(acct), trade(acct, acct+"-S", ledger.EventSell, "EXMP", "2030-01-10", "2030-01-12", "10.5"))
		e.invest(p, fakeFidelityFormat, ledgerFile(acct, events...))
		e.invest(p, fakeFidelityFormat, summary(acct, "2030-02-01", e.clock, 1000, nil))
		e.wantRecon(child, yes, zero, "2030-02-01", 0)
	})

	t.Run("an unsettled event makes the snapshot uncheckable", func(t *testing.T) {
		e.t = t
		p, acct, child := fresh()
		events := append(base(acct), trade(acct, acct+"-U", ledger.EventBuy, "EXMP", "2030-01-30", "2030-02-03", "1"))
		e.invest(p, fakeFidelityFormat, ledgerFile(acct, events...))
		exported := e.clock
		e.invest(p, fakeFidelityFormat, summary(acct, "2030-02-01", exported, 1000, map[string]string{"EXMP": "10.5"}))
		e.wantRecon(child, nil, nil, "", 0)
		// An older snapshot, before the unsettled order, is the newest checkable one.
		e.invest(p, fakeFidelityFormat, summary(acct, "2030-01-20", exported, 1000, map[string]string{"EXMP": "10.5"}))
		e.wantRecon(child, yes, zero, "2030-01-20", 0)
	})

	t.Run("an event on the snapshot's date makes it uncheckable", func(t *testing.T) {
		e.t = t
		p, acct, child := fresh()
		events := append(base(acct), cashRow(acct, acct+"-D", ledger.EventInterest, "2030-02-01", 1))
		e.invest(p, fakeFidelityFormat, ledgerFile(acct, events...))
		e.invest(p, fakeFidelityFormat, summary(acct, "2030-02-01", e.clock, 1000, map[string]string{"EXMP": "10.5"}))
		e.wantRecon(child, nil, nil, "", 0)
	})

	t.Run("a stale export is not checked and a fresh one is", func(t *testing.T) {
		e.t = t
		p, acct, child := fresh()
		e.invest(p, fakeFidelityFormat, ledgerFile(acct, base(acct)...))
		// The summary was exported after the only transactions file was imported.
		exported := e.clock.Add(time.Hour)
		e.investAt(p, fakeFidelityFormat, summary(acct, "2030-02-01", exported, 1100, map[string]string{"EXMP": "10.5"}), exported.Add(time.Minute))
		e.wantRecon(child, nil, nil, "", 0)
		// A transactions export taken after it holds the missing interest, and passes.
		events := append(base(acct), cashRow(acct, acct+"-I", ledger.EventInterest, "2030-01-15", 100))
		e.investAt(p, fakeFidelityFormat, ledgerFile(acct, events...), exported.Add(2*time.Minute))
		e.wantRecon(child, yes, zero, "2030-02-01", 0)
	})

	t.Run("a stale export that raised a mismatch is cleared by a fresh one", func(t *testing.T) {
		e.t = t
		p, acct, child := fresh()
		exported := e.clock.Add(time.Hour)
		// Imported after the summary's export time, but holding only the older rows.
		e.investAt(p, fakeFidelityFormat, ledgerFile(acct, base(acct)...), exported.Add(time.Minute))
		e.investAt(p, fakeFidelityFormat, summary(acct, "2030-02-01", exported, 1100, map[string]string{"EXMP": "10.5"}), exported.Add(2*time.Minute))
		e.wantRecon(child, no, ptr(int64(100)), "", 1)
		events := append(base(acct), cashRow(acct, acct+"-I", ledger.EventInterest, "2030-01-15", 100))
		e.investAt(p, fakeFidelityFormat, ledgerFile(acct, events...), exported.Add(3*time.Minute))
		e.wantRecon(child, yes, zero, "2030-02-01", 0)
	})

	t.Run("the latest capture of a date gives the date's snapshot", func(t *testing.T) {
		e.t = t
		p, acct, child := fresh()
		e.invest(p, fakeFidelityFormat, ledgerFile(acct, base(acct)...))
		at := e.clock
		e.investAt(p, fakeFidelityFormat, summary(acct, "2030-02-01", at.Add(-time.Minute), 1000, map[string]string{"EXMP": "10.5"}), at.Add(time.Minute))
		e.wantRecon(child, yes, zero, "2030-02-01", 0)
		// A mistaken capture on the same date, printed later, is what the date now says.
		e.investAt(p, fakeFidelityFormat, summary(acct, "2030-02-01", at, 1, map[string]string{"EXMP": "10.5"}), at.Add(2*time.Minute))
		e.wantRecon(child, no, ptr(int64(-999)), "2030-02-01", 1)
	})

	t.Run("an ended account is judged on data up to its end", func(t *testing.T) {
		e.t = t
		p, acct, child := fresh()
		e.invest(p, fakeFidelityFormat, ledgerFile(acct, base(acct)...))
		exported := e.clock
		e.invest(p, fakeFidelityFormat, summary(acct, "2030-02-01", exported, 1000, map[string]string{"EXMP": "10.5"}))
		e.invest(p, fakeFidelityFormat, summary(acct, "2030-03-01", exported, 7, map[string]string{"EXMP": "10.5"}))
		e.wantRecon(child, no, ptr(int64(-993)), "2030-02-01", 1)
		last := "2030-02-15"
		e.operator(ledger.EntryAccountEnd, ledger.AccountEnd{Slug: child, LastDate: &last})
		e.catchUp()
		e.wantRecon(child, yes, zero, "2030-02-01", 0)
		e.operator(ledger.EntryAccountEnd, ledger.AccountEnd{Slug: child})
		e.catchUp()
		e.wantRecon(child, no, ptr(int64(-993)), "2030-02-01", 1)
	})
	e.t = t

	t.Run("a platform cannot be ended", func(t *testing.T) {
		e.t = t
		p, _, _ := fresh()
		last := "2030-02-15"
		e.operator(ledger.EntryAccountEnd, ledger.AccountEnd{Slug: p, LastDate: &last})
		if st := e.catchUp(); st.Unparseable != 1 {
			t.Fatalf("stats %+v", st)
		}
		if e.account(p).EndedOn != nil {
			t.Fatal("a platform took an end")
		}
	})
}

func TestStatementFlows(t *testing.T) {
	e := newInvestmentEnv(t)
	yes, no := ptr(true), ptr(false)
	zero := ptr(int64(0))
	next := 0
	fresh := func() (platform, acct, child string) {
		next++
		platform = fmt.Sprintf("a%d", next)
		acct = fmt.Sprintf("PLAN%08d", 2000+next)
		e.platform(platform, ledger.ProviderAviva)
		return platform, acct, fmt.Sprintf("%s-pension-%04d", platform, 2000+next)
	}
	at := time.Date(2031, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("not checked before a statement, then passes", func(t *testing.T) {
		e.t = t
		p, acct, child := fresh()
		e.invest(p, fakeAvivaFormat, capture(acct, "2030-03-01", at, true, contribution(acct, "C1", "2029-06-01", 600), contribution(acct, "C2", "2030-02-01", 400)))
		e.wantRecon(child, nil, nil, "", 0)
		e.invest(p, fakeAvivaFormat, statementFile(acct, "2029-04-06", "2030-04-05", "2030-05-01", 10000, 1000, 12000))
		e.wantRecon(child, yes, zero, "2030-04-05", 0)
	})

	t.Run("contributions that do not sum to paid in fail", func(t *testing.T) {
		e.t = t
		p, acct, child := fresh()
		e.invest(p, fakeAvivaFormat, capture(acct, "2030-03-01", at, true, contribution(acct, "C1", "2029-06-01", 600)))
		e.invest(p, fakeAvivaFormat, statementFile(acct, "2029-04-06", "2030-04-05", "2030-05-01", 10000, 1000, 12000))
		r := e.wantRecon(child, no, ptr(int64(400)), "", 1)
		if r.LastPassAt != nil {
			t.Fatalf("last_pass_at %v", r.LastPassAt)
		}
		// The missing contribution arrives, and the check passes.
		e.invest(p, fakeAvivaFormat, capture(acct, "2030-03-02", at.Add(time.Hour), true,
			contribution(acct, "C1", "2029-06-01", 600), contribution(acct, "C2", "2030-02-01", 400)))
		e.wantRecon(child, yes, zero, "2030-04-05", 0)
	})

	t.Run("a statement that does not open on the last close fails, and only when they abut", func(t *testing.T) {
		e.t = t
		p, acct, child := fresh()
		e.invest(p, fakeAvivaFormat, statementFile(acct, "2028-04-06", "2029-04-05", "2029-05-01", 5000, 0, 6000))
		e.wantRecon(child, yes, zero, "2029-04-05", 0)
		e.invest(p, fakeAvivaFormat, statementFile(acct, "2029-04-06", "2030-04-05", "2030-05-01", 6100, 0, 7000))
		e.wantRecon(child, no, ptr(int64(100)), "2029-04-05", 1)

		p2, acct2, child2 := fresh()
		e.invest(p2, fakeAvivaFormat, statementFile(acct2, "2027-04-06", "2028-04-05", "2028-05-01", 5000, 0, 6000))
		e.invest(p2, fakeAvivaFormat, statementFile(acct2, "2029-04-06", "2030-04-05", "2030-05-01", 6100, 0, 7000))
		e.wantRecon(child2, yes, zero, "2030-04-05", 0)
	})

	t.Run("an ended account counts only statements up to its end", func(t *testing.T) {
		e.t = t
		p, acct, child := fresh()
		e.invest(p, fakeAvivaFormat, statementFile(acct, "2028-04-06", "2029-04-05", "2029-05-01", 5000, 0, 6000))
		e.invest(p, fakeAvivaFormat, statementFile(acct, "2029-04-06", "2030-04-05", "2030-05-01", 6100, 0, 7000))
		e.wantRecon(child, no, ptr(int64(100)), "2029-04-05", 1)
		last := "2029-12-31"
		e.operator(ledger.EntryAccountEnd, ledger.AccountEnd{Slug: child, LastDate: &last})
		e.catchUp()
		e.wantRecon(child, yes, zero, "2029-04-05", 0)
	})
}

func TestAcceptGapIsRefusedOnAnInvestmentAccount(t *testing.T) {
	e := newInvestmentEnv(t)
	e.platform("avi", ledger.ProviderAviva)
	e.invest("avi", fakeAvivaFormat, statementFile("PLAN00000042", "2029-04-06", "2030-04-05", "2030-05-01", 1000, 0, 1100))
	e.operator(ledger.EntryAcceptGap, ledger.AcceptGap{Slug: "avi-pension-0042", AfterDate: "2029-01-01", BeforeDate: "2029-02-01", Reason: "made-up"})
	if st := e.catchUp(); st.Unparseable != 1 {
		t.Fatalf("stats %+v", st)
	}
	r := e.recon(e.account("avi-pension-0042").ID)
	if r.Method != "statement_flows" || r.LastCheckOK == nil || !*r.LastCheckOK {
		t.Fatalf("reconciliation %+v", r)
	}
}

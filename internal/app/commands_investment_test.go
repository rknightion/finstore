package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/importer"
	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry"
	"github.com/rknightion/finstore/internal/testpg"
)

const fakeInvestmentFormat, fakeAvivaFormat = "fake-investment", "fake-aviva"

// fakeInvestmentNormaliser reads a file that is the JSON of the result it should yield. All values
// in these tests are made up.
type fakeInvestmentNormaliser struct{ provider string }

func (f fakeInvestmentNormaliser) Provider() string { return f.provider }

func (f fakeInvestmentNormaliser) NormaliseInvestmentFile(file ledger.File, _ *time.Location) (ledger.InvestmentResult, error) {
	var r ledger.InvestmentResult
	if err := json.Unmarshal(file.Body, &r); err != nil {
		return r, errors.New("fake: unreadable")
	}
	return r, nil
}

// investmentRig is a bootstrapped database and the command runner, with the fake formats
// registered for the test.
type investmentRig struct {
	t   *testing.T
	cfg config.Config
	st  *store.Store
}

func newInvestmentRig(t *testing.T) *investmentRig {
	t.Helper()
	saved := investmentFormats
	investmentFormats = map[string]ledger.InvestmentNormaliser{
		fakeInvestmentFormat: fakeInvestmentNormaliser{ledger.ProviderFidelity},
		fakeAvivaFormat:      fakeInvestmentNormaliser{ledger.ProviderAviva},
	}
	t.Cleanup(func() { investmentFormats = saved })
	ctx := context.Background()
	super := testpg.Start(t)
	env := map[string]string{
		config.EnvSuperuserDatabaseURL: super,
		config.EnvOwnerDatabaseURL:     testpg.UserURL(t, super, store.RoleOwner),
		config.EnvDatabaseURL:          testpg.UserURL(t, super, store.RoleApp),
		config.EnvOwnerPassword:        "owner-password",
		config.EnvAppPassword:          "app-password",
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	r := &investmentRig{t: t, cfg: cfg}
	for _, fn := range []func(context.Context, Env) int{Bootstrap, Migrate} {
		if code, out := r.run("", fn); code != ExitOK {
			t.Fatalf("setup exit %d: %s", code, out)
		}
	}
	r.st, err = store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.st.Close)
	return r
}

// run runs fn and returns its exit code and everything it wrote.
func (r *investmentRig) run(stdin string, fn func(context.Context, Env) int) (int, string) {
	r.t.Helper()
	var out bytes.Buffer
	code := fn(context.Background(), Env{Cfg: r.cfg, Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &out})
	return code, out.String()
}

func (r *investmentRig) add(spec AccountSpec) (int, string) {
	r.t.Helper()
	return r.run("", func(ctx context.Context, e Env) int { return AccountAdd(ctx, e, spec) })
}

func (r *investmentRig) imp(slug, format string, result ledger.InvestmentResult) (int, string) {
	r.t.Helper()
	b, err := json.Marshal(result)
	if err != nil {
		r.t.Fatal(err)
	}
	return r.run(string(b), func(ctx context.Context, e Env) int { return Import(ctx, e, slug, format, "made-up") })
}

func valuationFile(key, product string, asOf time.Time) ledger.InvestmentResult {
	captured := asOf.Add(12 * time.Hour)
	return ledger.InvestmentResult{Kind: ledger.InvestmentKindHoldings, CapturedAt: &captured, RowCount: 1,
		Accounts:   []ledger.InvestmentAccountRec{{AccountKey: key, Product: product}},
		Valuations: []ledger.ValuationRec{{AccountKey: key, AsOf: asOf, TotalMinor: 12345, CashMinor: 12345}}}
}

func TestAccountAddPlatform(t *testing.T) {
	r := newInvestmentRig(t)
	for _, provider := range []string{"fidelity", "aviva", "freetrade"} {
		if code, out := r.add(AccountSpec{Slug: "pf-" + provider, Type: "platform", Provider: provider, Currency: "GBP", Name: "Made-up", ExpectsImports: true}); code != ExitOK {
			t.Fatalf("add %s: exit %d: %s", provider, code, out)
		}
		core, err := store.NewCore(r.st.DB(), store.SchemaCore)
		if err != nil {
			t.Fatal(err)
		}
		a, err := core.GetAccountBySlug(context.Background(), "pf-"+provider)
		if err != nil {
			t.Fatal(err)
		}
		if a.Type != "platform" || a.Feed != "file" || a.Source != "investment:"+provider || a.SourceKey != a.Slug || !a.ExpectsImports {
			t.Fatalf("platform %+v", a)
		}
		if _, err := core.GetReconciliation(context.Background(), a.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a platform has a reconciliation row: %v", err)
		}
	}
	for name, spec := range map[string]AccountSpec{
		"no provider":               {Slug: "x-a", Type: "platform", Currency: "GBP", Name: "Made-up"},
		"unknown provider":          {Slug: "x-b", Type: "platform", Provider: "madeup", Currency: "GBP", Name: "Made-up"},
		"provider on a joint":       {Slug: "x-c", Type: "joint", Provider: "fidelity", Currency: "GBP", Name: "Made-up"},
		"provider on a current":     {Slug: "x-d", Type: "current", Provider: "aviva", Currency: "GBP", Name: "Made-up"},
		"provider on a savings":     {Slug: "x-e", Type: "savings", Provider: "aviva", Currency: "GBP", Name: "Made-up"},
		"provider on a mortgage":    {Slug: "x-f", Type: "mortgage", Provider: "aviva", Currency: "GBP", Name: "Made-up"},
		"a child type is not added": {Slug: "x-g", Type: "pension", Currency: "GBP", Name: "Made-up"},
	} {
		t.Run(name, func(t *testing.T) {
			r.t = t
			if code, out := r.add(spec); code != ExitError {
				t.Fatalf("exit %d: %s", code, out)
			}
		})
	}
	r.t = t

	// A platform is ended only through its children.
	code, out := r.run("", func(ctx context.Context, e Env) int {
		d := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		return AccountEnd(ctx, e, "pf-aviva", &d)
	})
	if code != ExitError || !strings.Contains(out, "is a platform") {
		t.Fatalf("account end on a platform: exit %d: %s", code, out)
	}
}

func TestImportInvestmentFile(t *testing.T) {
	r := newInvestmentRig(t)
	if code, out := r.add(AccountSpec{Slug: "fid", Type: "platform", Provider: "fidelity", Currency: "GBP", Name: "Made-up", ExpectsImports: true}); code != ExitOK {
		t.Fatalf("add: %d %s", code, out)
	}
	if code, out := r.add(AccountSpec{Slug: "bank-joint", Type: "joint", Currency: "GBP", Name: "Made-up"}); code != ExitOK {
		t.Fatalf("add: %d %s", code, out)
	}
	asOf := time.Date(2030, 1, 31, 0, 0, 0, 0, time.UTC)
	file := valuationFile("ACC00001234", ledger.ProductSIPP, asOf)

	code, out := r.imp("fid", fakeInvestmentFormat, file)
	if code != ExitOK || !strings.Contains(out, "imported: 1 rows") {
		t.Fatalf("first import: exit %d: %s", code, out)
	}
	var rows int
	if err := r.st.DB().QueryRow(context.Background(), `SELECT rows_seen FROM sync.run WHERE kind = 'import' ORDER BY id DESC LIMIT 1`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("run rows_seen %d, %v", rows, err)
	}
	// The same bytes again are recognised through investment_import.
	code, out = r.imp("fid", fakeInvestmentFormat, file)
	if code != ExitOK || !strings.Contains(out, "already imported") {
		t.Fatalf("repeat: exit %d: %s", code, out)
	}
	var raw int
	if err := r.st.DB().QueryRow(context.Background(), `SELECT count(*) FROM raw.import_file`).Scan(&raw); err != nil || raw != 1 {
		t.Fatalf("%d raw files, %v", raw, err)
	}

	// Refusals carry words with no sort code in them, and a repeat of a refused file stays refused.
	code, out = r.imp("bank-joint", fakeInvestmentFormat, file)
	if code != ExitRejected || !strings.Contains(out, "imports only into a platform") || strings.Contains(out, "sort code") {
		t.Fatalf("on a bank account: exit %d: %s", code, out)
	}
	code, out = r.imp("bank-joint", fakeInvestmentFormat, file)
	if code != ExitRejected || !strings.Contains(out, "given before and rejected") {
		t.Fatalf("repeat of a refused file: exit %d: %s", code, out)
	}
	code, out = r.run("date,type,amount,balance_after,description\n2030-01-02,PAY,1.00,1.00,made-up\n",
		func(ctx context.Context, e Env) int { return Import(ctx, e, "fid", FormatManualCSV, "made-up") })
	if code != ExitRejected || strings.Contains(out, "sort code") {
		t.Fatalf("a bank format on a platform: exit %d: %s", code, out)
	}

	if code, out := r.add(AccountSpec{Slug: "fid-sipp-9999", Type: "joint", Currency: "GBP", Name: "Made-up"}); code != ExitOK {
		t.Fatalf("add: %d %s", code, out)
	}
	code, out = r.imp("fid", fakeInvestmentFormat, valuationFile("ACC00009999", ledger.ProductSIPP, asOf))
	if code != ExitRejected || !strings.Contains(out, "slug another account already has") {
		t.Fatalf("slug collision: exit %d: %s", code, out)
	}
	var category string
	if err := r.st.DB().QueryRow(context.Background(), `SELECT error_category FROM sync.run WHERE kind = 'import' ORDER BY id DESC LIMIT 1`).Scan(&category); err != nil || category != "child_key_shared" {
		t.Fatalf("category %q, %v", category, err)
	}
	if !semconv.IsStatusCategory("child_key_shared") {
		t.Fatal("child_key_shared is not a status category")
	}

	code, out = r.run("", func(ctx context.Context, e Env) int { return Import(ctx, e, "fid", "not-a-format", "made-up") })
	if code != ExitError || !strings.Contains(out, fakeInvestmentFormat) {
		t.Fatalf("unknown format: exit %d: %s", code, out)
	}
}

// agent.coverage leaves platforms out, applies 70 days to investment children and adds no column,
// and finstore.import.due records 0 and 1 for those children and nothing else.
func TestCoverageAndImportDueForInvestmentAccounts(t *testing.T) {
	r := newInvestmentRig(t)
	ctx := context.Background()
	for _, s := range []AccountSpec{
		{Slug: "fid", Type: "platform", Provider: "fidelity", Currency: "GBP", Name: "Made-up", ExpectsImports: true},
		{Slug: "bank-joint", Type: "joint", Currency: "GBP", Name: "Made-up", ExpectsImports: true},
	} {
		if code, out := r.add(s); code != ExitOK {
			t.Fatalf("add %s: %d %s", s.Slug, code, out)
		}
	}
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	// A day of margin either side of the rule, so the test does not depend on the hour it runs.
	fresh, stale := today.AddDate(0, 0, -68), today.AddDate(0, 0, -72)
	for _, f := range []ledger.InvestmentResult{
		valuationFile("ACC00001111", ledger.ProductSIPP, fresh),
		valuationFile("ACC00002222", ledger.ProductISA, stale),
		// Named by a file with no valuation of its own: it has never been valued.
		{Kind: ledger.InvestmentKindLedger, RowCount: 1, Accounts: []ledger.InvestmentAccountRec{{AccountKey: "ACC00003333", Product: ledger.ProductCashManagement}},
			Events: []ledger.InvestmentEventRec{{AccountKey: "ACC00003333", SourceKey: "R1", EventType: ledger.EventInterest, ProviderType: "Cash Interest",
				OrderDate: today.AddDate(0, 0, -200), SettleDate: today.AddDate(0, 0, -199), GrossMinor: 5, CashMinor: 5,
				Content: json.RawMessage(`{"made":"up"}`), HashInput: []byte("r1")}}},
		valuationFile("ACC00004444", ledger.ProductSIPP, stale),
	} {
		if code, out := r.imp("fid", fakeInvestmentFormat, f); code != ExitOK {
			t.Fatalf("import: %d %s", code, out)
		}
	}
	lastDate := today.AddDate(0, 0, -71)
	if code, out := r.run("", func(ctx context.Context, e Env) int { return AccountEnd(ctx, e, "fid-sipp-4444", &lastDate) }); code != ExitOK {
		t.Fatalf("account end: %d %s", code, out)
	}

	rows, err := store.Coverage(ctx, r.st.DB())
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]store.CoverageRow{}
	for _, c := range rows {
		by[c.AccountSlug] = c
	}
	if _, ok := by["fid"]; ok {
		t.Fatal("agent.coverage lists a platform")
	}
	due := func(slug string) bool {
		c, ok := by[slug]
		if !ok {
			t.Fatalf("%s is not in coverage", slug)
		}
		for _, g := range c.Gaps {
			if g.Reason == store.GapImportDue {
				return true
			}
		}
		return false
	}
	for slug, want := range map[string]bool{"fid-sipp-1111": false, "fid-isa-2222": true, "fid-cash-3333": true, "fid-sipp-4444": false, "bank-joint": true} {
		if got := due(slug); got != want {
			t.Errorf("%s import due = %v, want %v", slug, got, want)
		}
	}
	cash := by["fid-cash-3333"]
	if cash.TransactionCount != 0 || cash.EarliestDate == nil || !cash.EarliestDate.Equal(today.AddDate(0, 0, -199)) ||
		cash.LatestDate == nil || !cash.LatestDate.Equal(today.AddDate(0, 0, -199)) || cash.LatestStatementDate != nil {
		t.Fatalf("an investment child's dates: %+v", cash)
	}
	if by["fid-sipp-1111"].LatestStatementDate != nil {
		t.Fatalf("child %+v", by["fid-sipp-1111"])
	}

	// No column was added: the columns are the ones this version's readers name.
	cols, err := r.st.DB().Query(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'agent' AND table_name = 'coverage' ORDER BY ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	defer cols.Close()
	var names []string
	for cols.Next() {
		var n string
		if err := cols.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	want := "account_slug feed earliest_date latest_date transaction_count reconciliation_state reconciled_through last_diff_minor gaps latest_statement_date failed_on source"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("coverage columns:\n%s\nwant\n%s", got, want)
	}

	// sync_status resolves a child's last import through its platform.
	statuses, err := store.SyncStatus(ctx, r.st.DB())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range statuses {
		if s.Source == "investment:fidelity" {
			found = true
			if at, ok := s.LastImportAt["fid-sipp-1111"]; !ok || at == nil {
				t.Fatalf("sync status: %+v", s.LastImportAt)
			}
		}
	}
	if !found {
		t.Fatal("sync_status has no row for the provider")
	}

	// The gauge: 1 and 0 for investment children, and no series for the platform or a bank account.
	reader := sdkmetric.NewManualReader()
	metrics, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	p := &statusPublisher{st: r.st, m: metrics, log: slog.New(slog.DiscardHandler), now: time.Now}
	if err := p.run(ctx); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != semconv.MetricImportDue {
				continue
			}
			for _, dp := range m.Data.(metricdata.Gauge[int64]).DataPoints {
				v, _ := dp.Attributes.Value(semconv.AttrAccount)
				got[v.AsString()] = dp.Value
			}
		}
	}
	wantDue := map[string]int64{"fid-sipp-1111": 0, "fid-isa-2222": 1, "fid-cash-3333": 1, "fid-sipp-4444": 0}
	if len(got) != len(wantDue) {
		t.Fatalf("import.due series %v, want %v", got, wantDue)
	}
	for slug, v := range wantDue {
		if got[slug] != v {
			t.Errorf("import.due %s = %d, want %d", slug, got[slug], v)
		}
	}
}

// AC1: finstore import --format freetrade-activity-csv against a freetrade platform stores one
// event per row and creates <platform>-sipp from Account Type; the same file again stores nothing,
// and the child is checked on cash once it is ended. Every value is made up.
func TestImportFreetradeActivityStatement(t *testing.T) {
	r := newInvestmentRig(t)
	investmentFormats[string(importer.FormatFreetradeActivityCSV)] = importer.FreetradeActivityNormaliser{}
	if code, out := r.add(AccountSpec{Slug: "ft", Type: "platform", Provider: "freetrade", Currency: "GBP", Name: "Made-up", ExpectsImports: true}); code != ExitOK {
		t.Fatalf("add: exit %d: %s", code, out)
	}
	const sipp = "Self-Invested Personal Pension (SIPP)"
	file := strings.Join([]string{
		"Value Date,Type,Details,Currency,Direction,Value,Account Type",
		"09/01/2030,CASH_TRANSFER,\"100.37 GBP cash transfer\",GBP,CASH_OUT,-100.37," + sipp,
		"08/01/2030,INTEREST,\"Interest income of 0.37 GBP\",GBP,CASH_IN,0.37," + sipp,
		"07/01/2030,SELL,\"10 Example Trust plc (GB0000000005) @ GBP 10.00000 on 03-Jan-2030 09:00:00\",GBP,CASH_IN,100," + sipp,
		"06/01/2030,BUY,\"10 Example Trust plc (GB0000000005) @ GBP 10.00000 on 02-Jan-2030 09:00:00 (Requested 100 GBP)\",GBP,CASH_OUT,-100.00," + sipp,
		"02/01/2030,CASH_TRANSFER,\"100.00 GBP cash transfer\",GBP,CASH_IN,100.00," + sipp,
	}, "\n")
	run := func() (int, string) {
		return r.run(file, func(ctx context.Context, e Env) int {
			return Import(ctx, e, "ft", string(importer.FormatFreetradeActivityCSV), "made-up.csv")
		})
	}
	if code, out := run(); code != ExitOK || !strings.Contains(out, "imported: 5 rows") {
		t.Fatalf("import: exit %d: %s", code, out)
	}
	if code, out := run(); code != ExitOK || !strings.Contains(out, "already imported") {
		t.Fatalf("repeat: exit %d: %s", code, out)
	}
	ctx := context.Background()
	var slug, typ string
	var events int
	if err := r.st.DB().QueryRow(ctx, "SELECT a.slug, a.type, count(e.*) FROM core.account a JOIN core.investment_event e ON e.account_id = a.id GROUP BY a.slug, a.type").Scan(&slug, &typ, &events); err != nil || slug != "ft-sipp" || typ != "pension" || events != 5 {
		t.Fatalf("child %q %q with %d events, %v", slug, typ, events, err)
	}
	state := func() string {
		var s string
		if err := r.st.DB().QueryRow(ctx, "SELECT reconciliation_state FROM agent.coverage WHERE account_slug = 'ft-sipp'").Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := state(); s != "not_checked" {
		t.Fatalf("before the end: %s", s)
	}
	if code, out := r.run("", func(ctx context.Context, e Env) int {
		d := time.Date(2030, 1, 9, 0, 0, 0, 0, time.UTC)
		return AccountEnd(ctx, e, "ft-sipp", &d)
	}); code != ExitOK {
		t.Fatalf("end: exit %d: %s", code, out)
	}
	if s := state(); s != "reconciled" {
		t.Fatalf("after the end: %s", s)
	}
}

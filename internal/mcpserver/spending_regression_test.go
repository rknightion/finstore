package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

const regressionFormat = "fake-regression"

// regressionNormaliser reads a file that is the JSON of the result it should yield. Every value is
// made up.
type regressionNormaliser struct{}

func (regressionNormaliser) Provider() string { return ledger.ProviderFidelity }

func (regressionNormaliser) NormaliseInvestmentFile(f ledger.File, _ *time.Location) (ledger.InvestmentResult, error) {
	var r ledger.InvestmentResult
	if err := json.Unmarshal(f.Body, &r); err != nil {
		return r, errors.New("fake: unreadable")
	}
	return r, nil
}

// spendingSnapshot is every spending_summary grouping, both directions, and all of
// agent.transactions, as JSON.
func spendingSnapshot(t *testing.T, r *rig) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, group := range []string{"month", "category", "merchant", "counterparty", "account"} {
		for _, direction := range []string{"in", "out"} {
			var s SummaryOutput
			decode(t, r.call(semconv.MCPToolSpendingSummary, map[string]any{
				"from": "2020-01-01", "to": "2040-12-31", "group_by": group, "direction": direction, "limit": 1000,
			}), &s)
			b, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			out[group+"/"+direction] = string(b)
		}
	}
	var q QueryOutput
	decode(t, r.call(semconv.MCPToolQuerySQL, map[string]any{
		"sql": "SELECT * FROM agent.transactions ORDER BY id", "max_rows": 2000,
	}), &q)
	b, err := json.Marshal([]any{q.Columns, q.Rows, q.Truncated})
	if err != nil {
		t.Fatal(err)
	}
	out["transactions"] = string(b)
	return out
}

// AC5: importing investment data changes neither spending_summary nor agent.transactions.
func TestInvestmentImportLeavesSpendingUnchanged(t *testing.T) {
	ctx := context.Background()
	c := newRealClusterAt(t, ledger.NormaliserRev)
	r := newRealRig(t, c)
	before := spendingSnapshot(t, r)
	if before["month/out"] == `{"groups":[],"truncated":false}` {
		t.Fatal("the fixture has no spending, so the comparison would prove nothing")
	}

	app, err := store.Open(ctx, testpg.UserURL(t, c.super, store.RoleApp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	l := ledger.New(nil, nil, time.UTC, nil, nil, ledger.WithInvestmentNormalisers(map[string]ledger.InvestmentNormaliser{
		regressionFormat: regressionNormaliser{},
	}))
	at := time.Date(2030, time.April, 1, 9, 0, 0, 0, time.UTC)
	payload, err := json.Marshal(ledger.AccountAdd{Slug: "fid", Type: "platform", Currency: "GBP", Name: "Made-up",
		ExpectsImports: true, Provider: ledger.ProviderFidelity})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertOperatorEntry(ctx, app.DB(), store.OperatorEntry{Kind: ledger.EntryAccountAdd, Payload: payload, EnteredAt: at}); err != nil {
		t.Fatal(err)
	}
	const key = "SIPP00001234"
	on := time.Date(2030, time.March, 3, 0, 0, 0, 0, time.UTC)
	content, err := json.Marshal([]string{"made-up", "row"})
	if err != nil {
		t.Fatal(err)
	}
	// Dated inside the spending window and shaped like spending, so a leak would show.
	body, err := json.Marshal(ledger.InvestmentResult{Kind: ledger.InvestmentKindLedger, RowCount: 2,
		Accounts: []ledger.InvestmentAccountRec{{AccountKey: key, Product: ledger.ProductSIPP}},
		Events: []ledger.InvestmentEventRec{
			{AccountKey: key, SourceKey: "R1", EventType: ledger.EventFee, ProviderType: "Service Fee", OrderDate: on,
				GrossMinor: -750, CashMinor: -750, Content: content, HashInput: []byte("R1")},
			{AccountKey: key, SourceKey: "R2", EventType: ledger.EventWithdrawal, ProviderType: "Withdrawal", OrderDate: on,
				GrossMinor: -5000, CashMinor: -5000, FlowMinor: -5000, Content: content, HashInput: []byte("R2")},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.InsertImportFile(ctx, app.DB(), store.ImportFile{AccountSlug: "fid", Format: regressionFormat,
		FileName: "made-up", ImportedAt: at.Add(time.Minute), Body: body}); err != nil {
		t.Fatal(err)
	}
	var stats ledger.Stats
	if err := app.WithWriterLock(ctx, store.HolderTest, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stats, err = l.CatchUp(ctx, tx, store.SchemaCore)
		return err
	}); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	if stats.FilesRejected != 0 {
		t.Fatalf("the import was rejected: %v", stats.Rejections)
	}
	// The import really landed: otherwise an unchanged total would prove nothing.
	var events QueryOutput
	decode(t, r.call(semconv.MCPToolQuerySQL, map[string]any{"sql": "SELECT count(*)::int FROM agent.investment_events"}), &events)
	if len(events.Rows) != 1 || len(events.Rows[0]) != 1 || events.Rows[0][0] != float64(2) {
		t.Fatalf("investment events = %+v, want 2", events.Rows)
	}

	after := spendingSnapshot(t, r)
	for k, want := range before {
		if after[k] != want {
			t.Errorf("%s changed:\nbefore %s\nafter  %s", k, want, after[k])
		}
	}
}

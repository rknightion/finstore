package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rknightion/finstore/internal/store"
)

// finstore txn class and txn clear write a txn_class entry, refuse an unknown transaction id without
// repeating it, say so when a rule an override never changes classifies the row, and the override
// is cleared by txn clear.
func TestTxnClassAndClearCommands(t *testing.T) {
	ctx := context.Background()
	r := newPayeeRig(t)
	core, err := store.NewCore(r.st.DB(), store.SchemaCore)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := core.GetAccountBySlug(ctx, "bank-main")
	if err != nil {
		t.Fatal(err)
	}
	// A Flex repayment, which rule 2 classifies as internal.
	day := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	flex := store.Transaction{ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("flex")), AccountID: acc.ID, Source: "manual", SourceKey: "tx_flex",
		AmountMinor: -9000, Currency: "GBP", TxnDate: day, Status: "settled", Description: "MADE UP FLEX"}
	if _, err := core.ApplyTransaction(ctx, flex, []byte(`{"metadata":{"flex_transaction_type":"repayment"}}`), []byte("hflex"), day, 100); err != nil {
		t.Fatal(err)
	}
	idOf := func(desc string, amount int64) string {
		t.Helper()
		var id uuid.UUID
		if err := r.st.DB().QueryRow(ctx, `SELECT id FROM agent.transactions WHERE description = $1 AND amount_minor = $2`, desc, amount).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id.String()
	}
	classOf := func(id string) (class, reason string) {
		t.Helper()
		if err := r.st.DB().QueryRow(ctx, `SELECT flow_class, flow_reason FROM agent.cash_flows WHERE id = $1`, id).Scan(&class, &reason); err != nil {
			t.Fatal(err)
		}
		return class, reason
	}
	one, other, flexID := idOf("ALPHA SHOP", -1000), idOf("ALPHA SHOP", -2000), idOf("MADE UP FLEX", -9000)

	// Refusals: each exits non-zero, names its reason, never repeats the id and writes nothing.
	unknown := uuid.NewString()
	before := r.entries()
	for name, c := range map[string]struct {
		fn  func(context.Context, Env) int
		why string
	}{
		"unknown id":          {func(ctx context.Context, e Env) int { return TxnClass(ctx, e, unknown, "income", false) }, "not a transaction"},
		"unknown id clear":    {func(ctx context.Context, e Env) int { return TxnClear(ctx, e, unknown) }, "not a transaction"},
		"not an id":           {func(ctx context.Context, e Env) int { return TxnClass(ctx, e, "Made Up Id 12", "income", false) }, "txn_id"},
		"employer not income": {func(ctx context.Context, e Env) int { return TxnClass(ctx, e, one, "spending", true) }, "employer"},
		"unknown class":       {func(ctx context.Context, e Env) int { return TxnClass(ctx, e, one, "gift", false) }, "flow_class"},
	} {
		code, out, stderr := r.run(c.fn)
		if code == ExitOK || out != "" || !strings.Contains(stderr, c.why) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, want a refusal naming %q", name, code, out, stderr, c.why)
		}
		for _, secret := range []string{unknown, "Made Up Id 12", one} {
			if strings.Contains(stderr, secret) {
				t.Errorf("%s: stderr echoes an id: %q", name, stderr)
			}
		}
	}
	if n := r.entries(); n != before {
		t.Fatalf("the refusals left %d raw operator entries", n-before)
	}

	// An override changes its own transaction and no other of the payee.
	out := r.mustRun(func(ctx context.Context, e Env) int { return TxnClass(ctx, e, one, "own_external", false) })
	if strings.Contains(out, "will not apply") {
		t.Errorf("a plain override carries the will-not-apply note: %q", out)
	}
	if c, why := classOf(one); c != "own_external" || why != "txn_rule" {
		t.Errorf("overridden row is %s/%s", c, why)
	}
	if c, why := classOf(other); c != "spending" || why != "direction" {
		t.Errorf("another row of the payee is %s/%s", c, why)
	}

	// A row that rules 1 to 4 classify: the command says the override will not apply, the entry is
	// written, and the row keeps its class.
	out = r.mustRun(func(ctx context.Context, e Env) int { return TxnClass(ctx, e, flexID, "income", false) })
	if !strings.Contains(out, "will not apply") || strings.Contains(out, flexID) {
		t.Errorf("output for a Flex row: %q", out)
	}
	if c, why := classOf(flexID); c != "internal" || why != "flex" {
		t.Errorf("Flex row is %s/%s", c, why)
	}

	// A clear removes the override. That the entry survives a rebuild is the ledger's replay test: the
	// transactions of this rig are written to core directly and have no raw rows to rebuild from.
	r.mustRun(func(ctx context.Context, e Env) int { return TxnClear(ctx, e, one) })
	if c, why := classOf(one); c != "spending" || why != "direction" {
		t.Errorf("after the clear the row is %s/%s", c, why)
	}
	var n int
	if err := r.st.DB().QueryRow(ctx, `SELECT count(*) FROM core.txn_class WHERE txn_id = $1`, uuid.MustParse(one)).Scan(&n); err != nil || n != 0 {
		t.Errorf("the clear left %d rows (%v)", n, err)
	}
}

package ledger_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/store"
)

func TestRefusalsDoNotEchoTheirInput(t *testing.T) {
	secret := "Made Up Person 12"
	for _, err := range []error{
		ledger.ValidatePayeeRule(ledger.PayeeRule{Action: "separate", PayeeKey: secret}),
		ledger.ValidatePayeeRule(ledger.PayeeRule{Action: "merge", PayeeKey: "n:acme", IntoKey: secret}),
		ledger.ValidatePayeeRule(ledger.PayeeRule{Action: "recurring", SeriesKey: "n:" + secret, Recurring: "confirm"}),
		ledger.ValidatePayeeRule(ledger.PayeeRule{Action: "clear", SeriesKey: secret, Clears: "recurring"}),
	} {
		if err == nil {
			t.Fatal("refusal expected")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("refusal echoes its input: %v", err)
		}
	}
}

func TestValidatePayeeRule(t *testing.T) {
	long := strings.Repeat("a", 3)
	cases := []struct {
		name string
		rule ledger.PayeeRule
		bad  string // a word the refusal must contain; empty when the rule is valid
	}{
		{"merge", ledger.PayeeRule{Action: "merge", PayeeKey: "n:acme", IntoKey: "mg:grp_1"}, ""},
		{"merge into a payee not seen yet", ledger.PayeeRule{Action: "merge", PayeeKey: "mu:user_1", IntoKey: "x:" + long}, ""},
		{"merge without a target", ledger.PayeeRule{Action: "merge", PayeeKey: "n:acme"}, "into_key"},
		{"merge into itself", ledger.PayeeRule{Action: "merge", PayeeKey: "n:acme", IntoKey: "n:acme"}, "itself"},
		{"merge with a bad key", ledger.PayeeRule{Action: "merge", PayeeKey: "acme", IntoKey: "n:acme"}, "payee_key"},
		{"merge with a bad target", ledger.PayeeRule{Action: "merge", PayeeKey: "n:acme", IntoKey: "zz:acme"}, "into_key"},
		{"empty key body", ledger.PayeeRule{Action: "separate", PayeeKey: "n:"}, "payee_key"},
		{"separate", ledger.PayeeRule{Action: "separate", PayeeKey: "n:acme"}, ""},
		{"rename", ledger.PayeeRule{Action: "rename", PayeeKey: "n:acme", Name: ptr("Acme")}, ""},
		{"rename without a name", ledger.PayeeRule{Action: "rename", PayeeKey: "n:acme"}, "name"},
		{"rename to blank", ledger.PayeeRule{Action: "rename", PayeeKey: "n:acme", Name: ptr(" ")}, "name"},
		{"class", ledger.PayeeRule{Action: "class", PayeeKey: "n:acme", FlowClass: "spending"}, ""},
		{"income employer", ledger.PayeeRule{Action: "class", PayeeKey: "n:acme", FlowClass: "income", Employer: true}, ""},
		{"employer without income", ledger.PayeeRule{Action: "class", PayeeKey: "n:acme", FlowClass: "spending", Employer: true}, "employer"},
		{"unknown class", ledger.PayeeRule{Action: "class", PayeeKey: "n:acme", FlowClass: "gift"}, "flow_class"},
		{"borrowing", ledger.PayeeRule{Action: "class", PayeeKey: "n:acme", FlowClass: "borrowing"}, ""},
		{"debt repayment", ledger.PayeeRule{Action: "class", PayeeKey: "n:acme", FlowClass: "debt_repayment"}, ""},
		{"employer with borrowing", ledger.PayeeRule{Action: "class", PayeeKey: "n:acme", FlowClass: "borrowing", Employer: true}, "employer"},
		{"category", ledger.PayeeRule{Action: "category", PayeeKey: "n:acme", Category: "groceries"}, ""},
		{"category blank", ledger.PayeeRule{Action: "category", PayeeKey: "n:acme"}, "category"},
		{"recurring", ledger.PayeeRule{Action: "recurring", SeriesKey: "sub:mg:grp_1", Recurring: "confirm"}, ""},
		{"recurring not_recurring", ledger.PayeeRule{Action: "recurring", SeriesKey: "dd:x", Recurring: "not_recurring"}, ""},
		{"recurring bad decision", ledger.PayeeRule{Action: "recurring", SeriesKey: "so:x", Recurring: "yes"}, "recurring"},
		{"recurring bad series", ledger.PayeeRule{Action: "recurring", SeriesKey: "n:acme", Recurring: "ignore"}, "series_key"},
		{"clear merge", ledger.PayeeRule{Action: "clear", PayeeKey: "n:acme", Clears: "merge"}, ""},
		{"clear series", ledger.PayeeRule{Action: "clear", SeriesKey: "inf:n:acme", Clears: "recurring"}, ""},
		{"clear series with a payee key only", ledger.PayeeRule{Action: "clear", PayeeKey: "n:acme", Clears: "recurring"}, "series_key"},
		{"clear unknown", ledger.PayeeRule{Action: "clear", PayeeKey: "n:acme", Clears: "everything"}, "clears"},
		{"clear needs a key", ledger.PayeeRule{Action: "clear", Clears: "name"}, "payee_key"},
		{"unknown action", ledger.PayeeRule{Action: "delete", PayeeKey: "n:acme"}, "action"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ledger.ValidatePayeeRule(c.rule)
			switch {
			case c.bad == "" && err != nil:
				t.Fatalf("valid rule refused: %v", err)
			case c.bad != "" && err == nil:
				t.Fatalf("invalid rule accepted")
			case c.bad != "" && !strings.Contains(err.Error(), c.bad):
				t.Fatalf("refusal %q does not name %q", err, c.bad)
			}
		})
	}
}

func TestValidateCategoryNameAndAccountScope(t *testing.T) {
	if err := ledger.ValidateCategoryName(ledger.CategoryName{Category: "eating_out", Name: ptr("Eating out")}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.ValidateCategoryName(ledger.CategoryName{Category: "eating_out"}); err != nil {
		t.Fatalf("a null name clears and is valid: %v", err)
	}
	if err := ledger.ValidateCategoryName(ledger.CategoryName{Name: ptr("x")}); err == nil {
		t.Fatal("a category name without a category was accepted")
	}
	if err := ledger.ValidateCategoryName(ledger.CategoryName{Category: "c", Name: ptr("  ")}); err == nil {
		t.Fatal("a blank name was accepted")
	}
	for _, scope := range []string{"personal", "joint", "business"} {
		if err := ledger.ValidateAccountScope(ledger.AccountScope{Slug: "a", Scope: ptr(scope)}); err != nil {
			t.Fatalf("scope %s: %v", scope, err)
		}
	}
	if err := ledger.ValidateAccountScope(ledger.AccountScope{Slug: "a"}); err != nil {
		t.Fatalf("a null scope clears and is valid: %v", err)
	}
	if err := ledger.ValidateAccountScope(ledger.AccountScope{Slug: "a", Scope: ptr("family")}); err == nil {
		t.Fatal("an unknown scope was accepted")
	}
	if err := ledger.ValidateAccountScope(ledger.AccountScope{Scope: ptr("joint")}); err == nil {
		t.Fatal("a scope without a slug was accepted")
	}
	if err := ledger.ValidateScopeTarget(store.Account{Slug: "p", Type: "pot"}); err == nil || !strings.Contains(err.Error(), "pot") {
		t.Fatalf("a pot was accepted: %v", err)
	}
	if err := ledger.ValidateScopeTarget(store.Account{Slug: "c", Type: "current"}); err != nil {
		t.Fatal(err)
	}
}

// rule writes a payee_rule entry and catches up, returning the entry's seq and the counted
// unparseable entries.
func (e *env) rule(r ledger.PayeeRule) (seq int64, unparseable int) {
	e.t.Helper()
	e.operator(ledger.EntryPayeeRule, r)
	return e.lastEntry(), e.catchUp().Unparseable
}

// lastEntry returns the seq of the newest operator entry.
func (e *env) lastEntry() int64 {
	e.t.Helper()
	var seq int64
	if err := e.app.DB().QueryRow(context.Background(), `SELECT max(seq) FROM raw.operator_entry`).Scan(&seq); err != nil {
		e.t.Fatal(err)
	}
	return seq
}

func TestPayeeRuleEntriesNormaliseWithTheEntrysSeqAndTime(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sample := ptr("MADE UP SHOP")

	seq, bad := e.rule(ledger.PayeeRule{Action: "merge", PayeeKey: "n:made up shop", IntoKey: "mg:grp_1", NameSample: sample})
	if bad != 0 {
		t.Fatalf("%d unparseable", bad)
	}
	var enteredAt = e.clock
	a, err := e.core().GetPayeeAlias(ctx, "n:made up shop")
	if err != nil {
		t.Fatal(err)
	}
	if a.Separate || a.IntoKey == nil || *a.IntoKey != "mg:grp_1" || a.NameSample == nil || *a.NameSample != *sample ||
		a.EntrySeq != seq || !a.EnteredAt.Equal(enteredAt) {
		t.Fatalf("alias %+v, want merge into mg:grp_1 from entry %d at %v", a, seq, enteredAt)
	}

	seq, _ = e.rule(ledger.PayeeRule{Action: "rename", PayeeKey: "mg:grp_1", Name: ptr("Made Up Shop")})
	s, err := e.core().GetPayeeSetting(ctx, "mg:grp_1")
	if err != nil || s.Name == nil || *s.Name != "Made Up Shop" || s.FlowClass != nil || s.Category != nil || s.Employer != nil || s.EntrySeq != seq {
		t.Fatalf("setting %+v, %v", s, err)
	}
	// A later class and category on the same row keep the name and take the later entry.
	e.rule(ledger.PayeeRule{Action: "class", PayeeKey: "mg:grp_1", FlowClass: "income", Employer: true})
	seq, _ = e.rule(ledger.PayeeRule{Action: "category", PayeeKey: "mg:grp_1", Category: "groceries"})
	s, _ = e.core().GetPayeeSetting(ctx, "mg:grp_1")
	if s.Name == nil || *s.Name != "Made Up Shop" || s.FlowClass == nil || *s.FlowClass != "income" ||
		s.Employer == nil || !*s.Employer || s.Category == nil || *s.Category != "groceries" || s.EntrySeq != seq {
		t.Fatalf("setting %+v", s)
	}
	// A new class replaces the employer flag: a class other than income has none.
	e.rule(ledger.PayeeRule{Action: "class", PayeeKey: "mg:grp_1", FlowClass: "spending"})
	s, _ = e.core().GetPayeeSetting(ctx, "mg:grp_1")
	if s.FlowClass == nil || *s.FlowClass != "spending" || s.Employer != nil {
		t.Fatalf("setting after a new class %+v", s)
	}
	// Borrowing is a class an operator may set, and the core table takes it.
	if _, bad := e.rule(ledger.PayeeRule{Action: "class", PayeeKey: "mg:grp_1", FlowClass: "borrowing"}); bad != 0 {
		t.Fatalf("%d unparseable after a borrowing class", bad)
	}
	s, _ = e.core().GetPayeeSetting(ctx, "mg:grp_1")
	if s.FlowClass == nil || *s.FlowClass != "borrowing" || s.Employer != nil {
		t.Fatalf("setting after the borrowing class %+v", s)
	}

	seq, _ = e.rule(ledger.PayeeRule{Action: "recurring", SeriesKey: "sub:mg:grp_1", Recurring: "confirm"})
	var rec string
	var recSeq int64
	if err := e.app.DB().QueryRow(ctx, `SELECT recurring, entry_seq FROM core.series_setting WHERE series_key = 'sub:mg:grp_1'`).Scan(&rec, &recSeq); err != nil || rec != "confirm" || recSeq != seq {
		t.Fatalf("series setting %q seq %d, %v", rec, recSeq, err)
	}
	e.rule(ledger.PayeeRule{Action: "recurring", SeriesKey: "sub:mg:grp_1", Recurring: "not_recurring"})
	if err := e.app.DB().QueryRow(ctx, `SELECT recurring FROM core.series_setting WHERE series_key = 'sub:mg:grp_1'`).Scan(&rec); err != nil || rec != "not_recurring" {
		t.Fatalf("a later decision did not replace the earlier: %q %v", rec, err)
	}

	e.operator(ledger.EntryCategoryName, ledger.CategoryName{Category: "groceries", Name: ptr("Food shopping")})
	seq = e.lastEntry()
	e.catchUp()
	var name string
	var nameSeq int64
	if err := e.app.DB().QueryRow(ctx, `SELECT name, entry_seq FROM core.category_name WHERE category = 'groceries'`).Scan(&name, &nameSeq); err != nil || name != "Food shopping" || nameSeq != seq {
		t.Fatalf("category name %q seq %d, %v", name, nameSeq, err)
	}
	e.operator(ledger.EntryCategoryName, ledger.CategoryName{Category: "groceries"})
	if st := e.catchUp(); st.Unparseable != 0 {
		t.Fatalf("%d unparseable", st.Unparseable)
	}
	if err := e.app.DB().QueryRow(ctx, `SELECT name FROM core.category_name WHERE category = 'groceries'`).Scan(&name); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a null name left the row: %q %v", name, err)
	}
}

// separate and merge on one key are both kept; each clear removes only its own setting, and the
// row goes when nothing is left.
func TestSeparateWithMergeOnOneKeyAndEachClear(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	key := "n:made up shop"
	alias := func() (store.PayeeAlias, error) { return e.core().GetPayeeAlias(ctx, key) }

	e.rule(ledger.PayeeRule{Action: "merge", PayeeKey: key, IntoKey: "mg:grp_1"})
	e.rule(ledger.PayeeRule{Action: "separate", PayeeKey: key})
	a, err := alias()
	if err != nil || !a.Separate || a.IntoKey == nil || *a.IntoKey != "mg:grp_1" {
		t.Fatalf("separate with merge: %+v, %v", a, err)
	}
	// Clearing the merge keeps the row while the key is separated.
	e.rule(ledger.PayeeRule{Action: "clear", PayeeKey: key, Clears: "merge"})
	a, err = alias()
	if err != nil || !a.Separate || a.IntoKey != nil {
		t.Fatalf("after clearing the merge: %+v, %v", a, err)
	}
	// Merge again, then clearing the separation keeps the merge.
	e.rule(ledger.PayeeRule{Action: "merge", PayeeKey: key, IntoKey: "mg:grp_2"})
	e.rule(ledger.PayeeRule{Action: "clear", PayeeKey: key, Clears: "separate"})
	a, err = alias()
	if err != nil || a.Separate || a.IntoKey == nil || *a.IntoKey != "mg:grp_2" {
		t.Fatalf("after clearing the separation: %+v, %v", a, err)
	}
	// Clearing the merge now deletes the row.
	e.rule(ledger.PayeeRule{Action: "clear", PayeeKey: key, Clears: "merge"})
	if _, err := alias(); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("row remains: %v", err)
	}
	// Separate alone, then clearing it deletes the row.
	e.rule(ledger.PayeeRule{Action: "separate", PayeeKey: key})
	e.rule(ledger.PayeeRule{Action: "clear", PayeeKey: key, Clears: "separate"})
	if _, err := alias(); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("row remains: %v", err)
	}

	// Settings: each clear nulls its own column; class nulls employer too; the row goes last.
	sk := "mg:grp_9"
	e.rule(ledger.PayeeRule{Action: "rename", PayeeKey: sk, Name: ptr("Made Up")})
	e.rule(ledger.PayeeRule{Action: "class", PayeeKey: sk, FlowClass: "income", Employer: true})
	e.rule(ledger.PayeeRule{Action: "category", PayeeKey: sk, Category: "salary"})
	// A clear of something already unset changes nothing, not even entry_seq.
	e.rule(ledger.PayeeRule{Action: "rename", PayeeKey: "mg:grp_8", Name: ptr("Other")})
	before, _ := e.core().GetPayeeSetting(ctx, "mg:grp_8")
	e.rule(ledger.PayeeRule{Action: "clear", PayeeKey: "mg:grp_8", Clears: "category"})
	e.rule(ledger.PayeeRule{Action: "clear", PayeeKey: "mg:grp_8", Clears: "class"})
	after, _ := e.core().GetPayeeSetting(ctx, "mg:grp_8")
	if after.EntrySeq != before.EntrySeq || !after.EnteredAt.Equal(before.EnteredAt) {
		t.Fatalf("no-op clear changed the row: %+v -> %+v", before, after)
	}
	e.rule(ledger.PayeeRule{Action: "separate", PayeeKey: "n:made up other"})
	beforeA, _ := e.core().GetPayeeAlias(ctx, "n:made up other")
	e.rule(ledger.PayeeRule{Action: "clear", PayeeKey: "n:made up other", Clears: "merge"})
	afterA, _ := e.core().GetPayeeAlias(ctx, "n:made up other")
	if afterA.EntrySeq != beforeA.EntrySeq {
		t.Fatalf("no-op merge clear changed the alias: %+v -> %+v", beforeA, afterA)
	}

	e.rule(ledger.PayeeRule{Action: "clear", PayeeKey: sk, Clears: "class"})
	s, err := e.core().GetPayeeSetting(ctx, sk)
	if err != nil || s.FlowClass != nil || s.Employer != nil || s.Name == nil || s.Category == nil {
		t.Fatalf("after clearing the class: %+v, %v", s, err)
	}
	e.rule(ledger.PayeeRule{Action: "clear", PayeeKey: sk, Clears: "name"})
	s, _ = e.core().GetPayeeSetting(ctx, sk)
	if s.Name != nil || s.Category == nil {
		t.Fatalf("after clearing the name: %+v", s)
	}
	e.rule(ledger.PayeeRule{Action: "clear", PayeeKey: sk, Clears: "category"})
	if _, err := e.core().GetPayeeSetting(ctx, sk); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("setting row remains: %v", err)
	}
	// Clearing what is not set changes nothing and is not an error.
	if _, bad := e.rule(ledger.PayeeRule{Action: "clear", PayeeKey: sk, Clears: "name"}); bad != 0 {
		t.Fatalf("a clear of nothing counted unparseable")
	}

	// A series decision, then its clear.
	e.rule(ledger.PayeeRule{Action: "recurring", SeriesKey: "dd:made-up", Recurring: "ignore"})
	e.rule(ledger.PayeeRule{Action: "clear", SeriesKey: "dd:made-up", Clears: "recurring"})
	var n int
	if err := e.app.DB().QueryRow(ctx, `SELECT count(*) FROM core.series_setting`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("series settings left: %d %v", n, err)
	}
}

func TestInvalidOperatorEntriesAreSkippedAsUnparseable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.accounts(fAcc{ID: "acc_1", Type: "current", Opened: "2029-01-01"}, fAcc{ID: "pot_1", Type: "pot", Parent: "acc_1"})
	e.catchUp()
	pot := e.potSlug()
	e.operator(ledger.EntryPayeeRule, ledger.PayeeRule{Action: "class", PayeeKey: "n:x", FlowClass: "spending", Employer: true})
	e.operator(ledger.EntryPayeeRule, ledger.PayeeRule{Action: "merge", PayeeKey: "bad", IntoKey: "n:x"})
	e.operator(ledger.EntryPayeeRule, ledger.PayeeRule{Action: "recurring", SeriesKey: "n:x", Recurring: "confirm"})
	e.operator(ledger.EntryCategoryName, ledger.CategoryName{Name: ptr("no category")})
	e.operator(ledger.EntryAccountScope, ledger.AccountScope{Slug: pot, Scope: ptr("joint")})
	e.operator(ledger.EntryAccountScope, ledger.AccountScope{Slug: "no-such-account", Scope: ptr("joint")})
	e.operator(ledger.EntryAccountScope, ledger.AccountScope{Slug: e.parentSlug(), Scope: ptr("family")})
	if st := e.catchUp(); st.Unparseable != 7 {
		t.Fatalf("unparseable = %d, want 7", st.Unparseable)
	}
	for _, table := range []string{"payee_alias", "payee_setting", "series_setting", "category_name"} {
		var n int
		if err := e.app.DB().QueryRow(ctx, "SELECT count(*) FROM core."+table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s has %d rows, %v", table, n, err)
		}
	}
	if a := e.account(pot); a.Scope != nil {
		t.Fatalf("a pot took scope %q", *a.Scope)
	}
}

func (e *env) potSlug() string {
	e.t.Helper()
	accs, err := e.core().ListAccounts(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	for _, a := range accs {
		if a.Type == "pot" {
			return a.Slug
		}
	}
	e.t.Fatal("no pot")
	return ""
}

func (e *env) parentSlug() string {
	e.t.Helper()
	accs, err := e.core().ListAccounts(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	for _, a := range accs {
		if a.Type == "current" {
			return a.Slug
		}
	}
	e.t.Fatal("no current account")
	return ""
}

func TestAccountScopeEntrySetsAndClearsTheScopeAndKeepsOtherColumns(t *testing.T) {
	e := newEnv(t)
	e.addFileAccount("bank-current")
	e.operator(ledger.EntryAccountEnd, ledger.AccountEnd{Slug: "bank-current", LastDate: ptr("2030-01-31")})
	e.operator(ledger.EntryAccountScope, ledger.AccountScope{Slug: "bank-current", Scope: ptr("business")})
	if st := e.catchUp(); st.Unparseable != 0 {
		t.Fatalf("%d unparseable", st.Unparseable)
	}
	a := e.account("bank-current")
	if a.Scope == nil || *a.Scope != "business" || a.EndedOn == nil {
		t.Fatalf("account %+v: scope should be set and ended_on kept", a)
	}
	e.operator(ledger.EntryAccountScope, ledger.AccountScope{Slug: "bank-current"})
	e.catchUp()
	a = e.account("bank-current")
	if a.Scope != nil || a.EndedOn == nil {
		t.Fatalf("account %+v: scope should be cleared and ended_on kept", a)
	}
}

// A rebuild from raw gives the same four tables and scope as the live run.
func TestReplayGivesEqualRuleTablesAndScope(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	scenario(e)
	e.rule(ledger.PayeeRule{Action: "merge", PayeeKey: "n:made up shop", IntoKey: "mg:grp_1", NameSample: ptr("MADE UP SHOP")})
	e.rule(ledger.PayeeRule{Action: "separate", PayeeKey: "n:made up shop"})
	e.rule(ledger.PayeeRule{Action: "separate", PayeeKey: "n:gone"})
	e.rule(ledger.PayeeRule{Action: "clear", PayeeKey: "n:gone", Clears: "separate"})
	e.rule(ledger.PayeeRule{Action: "rename", PayeeKey: "mg:grp_1", Name: ptr("Made Up")})
	e.rule(ledger.PayeeRule{Action: "class", PayeeKey: "mg:grp_1", FlowClass: "income", Employer: true})
	e.rule(ledger.PayeeRule{Action: "category", PayeeKey: "mg:grp_1", Category: "salary"})
	e.rule(ledger.PayeeRule{Action: "recurring", SeriesKey: "sub:mg:grp_1", Recurring: "confirm"})
	e.operator(ledger.EntryCategoryName, ledger.CategoryName{Category: "salary", Name: ptr("Pay")})
	e.operator(ledger.EntryAccountScope, ledger.AccountScope{Slug: "bank-current", Scope: ptr("joint")})
	e.catchUp()

	err := e.owner.HoldWriterLock(ctx, store.HolderTest, func(ctx context.Context, conn *pgx.Conn) error {
		if err := store.CreateCoreNext(ctx, conn, ledger.NormaliserRev); err != nil {
			return err
		}
		if _, err := e.l.CatchUp(ctx, conn, store.SchemaCoreNext); err != nil {
			return err
		}
		diffs, err := store.DiffCore(ctx, conn)
		if err != nil {
			return err
		}
		if len(diffs) != 0 {
			t.Errorf("replay differs from the incremental core: %+v", diffs)
		}
		// The comparison saw rows: the tables are not trivially equal because both are empty.
		for table, want := range map[string]int{"payee_alias": 1, "payee_setting": 1, "series_setting": 1, "category_name": 1} {
			var n int
			if err := conn.QueryRow(ctx, "SELECT count(*) FROM core_next."+table).Scan(&n); err != nil || n != want {
				t.Errorf("core_next.%s has %d rows, want %d (%v)", table, n, want, err)
			}
		}
		var scope *string
		if err := conn.QueryRow(ctx, `SELECT scope FROM core_next.account WHERE slug = 'bank-current'`).Scan(&scope); err != nil || scope == nil || *scope != "joint" {
			t.Errorf("core_next scope %v, %v", scope, err)
		}
		return store.DropCoreNext(ctx, conn)
	})
	if err != nil {
		t.Fatal(err)
	}
}

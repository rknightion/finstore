package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rknightion/finstore/internal/store"
)

// insightFixture is a made-up estate for the classification views: Monzo, Open Banking and card
// accounts in three scopes, and helpers that write transactions and operator rules. Every value is
// invented.
type insightFixture struct {
	t     *testing.T
	q     store.DBTX
	super string
	core  store.Core
	acct  map[string]store.Account
	ids   map[string]uuid.UUID
	names map[uuid.UUID]string
	seq   int64
}

// itx describes one made-up transaction. Zero fields take defaults: GBP, settled, and a
// description derived from the key.
type itx struct {
	acct, key, date, status, desc, cp, merchant, scheme, category, currency, local string
	amt                                                                            int64
	blank                                                                          bool
	detail                                                                         map[string]any
}

const (
	insRefCur   = "040004 11111111"
	insRefJoint = "040004 22222222"
	insRefOB    = "309999 33333333"
	insRefOld   = "040004 44444444"
	insRefSav   = "040004 55555555"
	insRefBiz   = "040004 66666666"
)

func newInsightFixture(t *testing.T) *insightFixture {
	t.Helper()
	c := newCluster(t)
	ctx := context.Background()
	q := openStore(t, c.app).DB()
	f := &insightFixture{t: t, q: q, super: c.super, core: mustCore(t, q, store.SchemaCore),
		acct: map[string]store.Account{}, ids: map[string]uuid.UUID{}, names: map[uuid.UUID]string{}, seq: 1000}

	add := func(slug, source, key, typ, feed string, ref *string, parent string, mod func(*store.Account)) {
		a := testAccount(slug, typ, feed)
		a.Source, a.SourceKey, a.BankRef = source, key, ref
		if parent != "" {
			p := f.acct[parent]
			a.ParentID = &p.ID
		}
		if mod != nil {
			mod(&a)
		}
		f.acct[slug] = a
		if err := f.core.UpsertAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	add("cur", "monzo", "acc_cur", "current", "api", ptr(insRefCur), "", nil)
	add("sav", "monzo", "acc_sav", "current", "api", ptr(insRefSav), "", nil)
	add("joint", "monzo", "acc_joint", "joint", "api", ptr(insRefJoint), "", nil)
	add("biz", "monzo", "acc_biz", "business", "api", ptr(insRefBiz), "", nil)
	add("flex", "monzo", "acc_flex", "flex", "api", nil, "", nil)
	add("pot-cur", "monzo", "pot_cur", "pot", "api", nil, "cur", nil)
	add("pot-joint", "monzo", "pot_joint", "pot", "api", nil, "joint", nil)
	add("ob", "truelayer", "tl_cur", "current", "api", ptr(insRefOB), "", nil)
	add("card", "truelayer", "tl_card", "credit_card", "api", nil, "", nil)
	// An account that ended before the fixture's transactions, and an older account that shares the
	// Open Banking account's bank reference (allowed while one of them has ended).
	add("old", "monzo", "acc_old", "current", "api", ptr(insRefOld), "", func(a *store.Account) {
		a.EndedOn = ptr(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	})
	add("ob-old", "manual", "ob_old", "current", "file", ptr(insRefOB), "", func(a *store.Account) {
		a.EndedOn = ptr(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	})
	// An Open Banking account the operator marks as joint, with a pot-less file account beside it.
	add("ob-joint", "truelayer", "tl_joint", "current", "api", nil, "", func(a *store.Account) { a.Scope = ptr("joint") })
	add("monzo-biz-pot", "monzo", "pot_biz", "pot", "api", nil, "biz", nil)
	add("scoped-pot", "monzo", "pot_scoped", "pot", "api", nil, "cur", func(a *store.Account) { a.Scope = ptr("business") })
	add("orphan-pot", "monzo", "pot_orphan", "pot", "api", nil, "", nil)
	return f
}

func d(s string) time.Time {
	v, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return v
}

// md builds a Monzo metadata object.
func md(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return map[string]any{"metadata": m}
}

func merchantDetail(gid, name string) map[string]any {
	return map[string]any{"merchant": map[string]any{"id": "merch_" + gid, "group_id": gid, "name": name}}
}

func cpDetail(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return map[string]any{"counterparty": m}
}

func merge(parts ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, p := range parts {
		for k, v := range p {
			out[k] = v
		}
	}
	return out
}

// tx writes one transaction and records its id under its key.
func (f *insightFixture) tx(x itx) {
	f.t.Helper()
	a, ok := f.acct[x.acct]
	if !ok {
		f.t.Fatalf("no account %q", x.acct)
	}
	at := d(x.date).Add(10 * time.Hour)
	t := testTxn(a, x.key, x.amt, d(x.date))
	t.Source = a.Source
	t.CreatedAt = &at
	if x.status != "" {
		t.Status = x.status
	}
	if x.currency != "" {
		t.Currency = x.currency
	}
	t.Description = x.desc
	if x.desc == "" && x.cp == "" && x.merchant == "" && x.scheme == "" && x.detail == nil && !x.blank {
		t.Description = "made up " + x.key
	}
	if x.cp != "" {
		t.Counterparty = ptr(x.cp)
	}
	if x.merchant != "" {
		t.MerchantName = ptr(x.merchant)
	}
	if x.scheme != "" {
		t.Scheme = ptr(x.scheme)
	}
	if x.category != "" {
		t.Category = ptr(x.category)
	}
	if x.local != "" {
		t.LocalCurrency = ptr(x.local)
		t.LocalAmountMinor = ptr(x.amt)
	}
	content := merge(x.detail, map[string]any{"id": x.key, "amount": x.amt})
	body, err := json.Marshal(content)
	if err != nil {
		f.t.Fatal(err)
	}
	f.seq++
	if _, err := f.core.ApplyTransaction(context.Background(), t, body, body, at, f.seq); err != nil {
		f.t.Fatal(err)
	}
	f.ids[x.key] = t.ID
	f.names[t.ID] = x.key
}

func (f *insightFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.q.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// alias inserts a core.payee_alias row; into and sample are empty for no value.
func (f *insightFixture) alias(key string, separate bool, into, sample string, seq int64) {
	f.t.Helper()
	f.exec(`INSERT INTO core.payee_alias (payee_key, separate, into_key, name_sample, entry_seq, entered_at)
		VALUES ($1, $2, nullif($3, ''), nullif($4, ''), $5, now())`, key, separate, into, sample, seq)
}

// setting inserts a core.payee_setting row; an empty string is no value.
func (f *insightFixture) setting(key, name, class, category, sample string, employer *bool, seq int64) {
	f.t.Helper()
	f.exec(`INSERT INTO core.payee_setting (payee_key, name, flow_class, employer, category, name_sample, entry_seq, entered_at)
		VALUES ($1, nullif($2, ''), nullif($3, ''), $4, nullif($5, ''), nullif($6, ''), $7, now())`,
		key, name, class, employer, category, sample, seq)
}

type flowRow struct {
	status, scheme, currency, scope, payeeKey, payeeID, payeeName string
	class, reason                                                 string
	amount                                                        int64
	paired                                                        *uuid.UUID
	employer                                                      *bool
	category, categoryName, txnCategory                           *string
}

// flows reads agent.cash_flows keyed by the fixture's transaction keys.
func (f *insightFixture) flows() map[string]flowRow {
	f.t.Helper()
	rows, err := f.q.Query(context.Background(), `SELECT id, status, scheme, currency, account_scope, payee_key, payee_id,
		payee_name, flow_class, flow_reason, amount_minor, paired_id, employer, category, category_name, txn_category
		FROM agent.cash_flows`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]flowRow{}
	for rows.Next() {
		var id uuid.UUID
		var r flowRow
		var scheme *string
		if err := rows.Scan(&id, &r.status, &scheme, &r.currency, &r.scope, &r.payeeKey, &r.payeeID, &r.payeeName,
			&r.class, &r.reason, &r.amount, &r.paired, &r.employer, &r.category, &r.categoryName, &r.txnCategory); err != nil {
			f.t.Fatal(err)
		}
		if scheme != nil {
			r.scheme = *scheme
		}
		key, ok := f.names[id]
		if !ok {
			f.t.Fatalf("agent.cash_flows has a row for no fixture transaction: %s", id)
		}
		if _, dup := out[key]; dup {
			f.t.Fatalf("agent.cash_flows has two rows for %s", key)
		}
		out[key] = r
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func TestInsightViewColumns(t *testing.T) {
	f := newInsightFixture(t)
	requireColumns(t, f.q, "account_scopes", []string{"slug", "scope", "scope_source"})
	requireColumns(t, f.q, "cash_flows", []string{"id", "account_slug", "txn_date", "status", "amount_minor", "currency",
		"scheme", "description", "merchant_name", "counterparty", "txn_category", "account_scope", "payee_key", "payee_id",
		"payee_name", "flow_class", "flow_reason", "paired_id", "employer", "category", "category_name"})
	requireColumns(t, f.q, "payees", []string{"payee_id", "payee_name", "currency", "keys", "name_variants", "first_date",
		"last_date", "txn_count", "out_minor", "in_minor", "out_12m_minor", "in_12m_minor", "flow_class", "employer",
		"category", "category_name", "recurring_series"})
	requireColumns(t, f.q, "payee_years", []string{"payee_id", "currency", "year", "out_minor", "in_minor", "txn_count"})
	requireColumns(t, f.q, "payee_rule_status", []string{"rule_table", "key", "detail", "entered_at", "matches",
		"last_match_date", "problem"})

	// The readers the views serve can call them, including the function they use.
	for _, role := range []string{store.RoleAgent, store.RoleApp} {
		conn := connAs(t, f.super, role)
		for _, v := range []string{"account_scopes", "payee_keys", "cash_flows", "payees", "payee_years", "payee_rule_status"} {
			rows, err := conn.Query(context.Background(), "SELECT * FROM agent."+v)
			if err != nil {
				t.Fatalf("%s reading agent.%s: %v", role, v, err)
			}
			rows.Close()
		}
	}
}

func TestAccountScopes(t *testing.T) {
	f := newInsightFixture(t)
	rows, err := f.q.Query(context.Background(), `SELECT slug, scope, scope_source FROM agent.account_scopes`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][2]string{}
	for rows.Next() {
		var slug string
		var v [2]string
		if err := rows.Scan(&slug, &v[0], &v[1]); err != nil {
			t.Fatal(err)
		}
		got[slug] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"cur":           {"personal", "default"},
		"joint":         {"joint", "type"},
		"biz":           {"business", "type"},
		"ob-joint":      {"joint", "setting"},    // the operator's setting on a type the default would call personal
		"pot-cur":       {"personal", "parent"},  // a pot takes its parent's scope
		"pot-joint":     {"joint", "parent"},     // including a scope the parent has from its type
		"monzo-biz-pot": {"business", "parent"},  //
		"scoped-pot":    {"business", "setting"}, // the operator's setting comes before the parent
		"orphan-pot":    {"personal", "default"}, // a pot with no parent falls to the default
		"ob":            {"personal", "default"},
		"old":           {"personal", "default"}, // an ended account has a scope too
	}
	if len(got) != len(f.acct) {
		t.Errorf("agent.account_scopes has %d rows, want one per account (%d)", len(got), len(f.acct))
	}
	for slug, w := range want {
		if got[slug] != w {
			t.Errorf("%s = %v, want %v", slug, got[slug], w)
		}
	}

	// A setting on a parent reaches its pots, and the cross-scope rows it makes follow it.
	f.exec(`UPDATE core.account SET scope = 'business' WHERE slug = 'cur'`)
	var scope, source string
	if err := f.q.QueryRow(context.Background(), `SELECT scope, scope_source FROM agent.account_scopes WHERE slug = 'pot-cur'`).Scan(&scope, &source); err != nil {
		t.Fatal(err)
	}
	if scope != "business" || source != "parent" {
		t.Errorf("pot-cur after its parent is set to business = %s (%s)", scope, source)
	}
}

// payeeFixtureTxns writes one transaction for each payee key order and automatic merge case.
func (f *insightFixture) payeeKeyTxns() {
	f.tx(itx{acct: "cur", key: "mg1", date: "2026-03-05", amt: -300, merchant: "Coffee Co", detail: merchantDetail("grp_1", "Coffee Co")})
	f.tx(itx{acct: "cur", key: "mg1b", date: "2026-03-06", amt: -310, merchant: "Coffee Co", detail: merchantDetail("grp_1", "Coffee Co")})
	// grp_2 is the same company under another name form and has the earlier first transaction.
	f.tx(itx{acct: "cur", key: "mg2", date: "2026-03-01", amt: -320, merchant: "Coffee Co Ltd", detail: merchantDetail("grp_2", "Coffee Co Ltd")})
	f.tx(itx{acct: "cur", key: "mg3", date: "2026-03-07", amt: -400, merchant: "Tea House", detail: merchantDetail("grp_3", "Tea House")})
	// An n: key that normalises to the same name as the merged mg: payee: never merged with it.
	f.tx(itx{acct: "cur", key: "n_coffee", date: "2026-03-08", amt: -500, desc: "COFFEE CO"})
	// Person to person.
	f.tx(itx{acct: "cur", key: "mu1", date: "2026-03-09", amt: -2500, cp: "A Friend", desc: "pizza",
		detail: cpDetail("user_id", "user_1", "name", "A Friend")})
	f.tx(itx{acct: "cur", key: "mu1b", date: "2026-03-10", amt: 2500, cp: "Friend Renamed", desc: "back",
		detail: cpDetail("user_id", "user_1", "name", "Friend Renamed")})
	f.tx(itx{acct: "cur", key: "mu2", date: "2026-03-11", amt: -100, cp: "A Friend", desc: "other person with the same name",
		detail: cpDetail("user_id", "user_2", "name", "A Friend")})
	// A payment collector: bank details shared by names that are different payees, and one payee
	// whose name carries references and dates (and an Open Banking row for it with no details).
	shared := cpDetail("sort_code", "20-00-00", "account_number", "12345678")
	f.tx(itx{acct: "cur", key: "col1", date: "2026-03-12", amt: -2000, cp: "Gym Club REF123456", desc: "gym", detail: shared})
	f.tx(itx{acct: "cur", key: "col2", date: "2026-03-13", amt: -3000, cp: "Landlord Properties Ltd", desc: "rent", detail: shared})
	f.tx(itx{acct: "ob", key: "ob_gym", date: "2026-03-14", amt: -2000, desc: "GYM CLUB 01NOV26 987654", category: "DIRECT_DEBIT"})
	// Nothing to name it by.
	f.tx(itx{acct: "cur", key: "x1", date: "2026-03-15", amt: -50, scheme: "faster_payments", desc: ""})
	f.tx(itx{acct: "cur", key: "x1b", date: "2026-03-16", amt: -60, scheme: "faster_payments", desc: ""})
	f.tx(itx{acct: "cur", key: "x2", date: "2026-03-17", amt: -70, blank: true})
	// A description that normalises to nothing falls through to x:.
	f.tx(itx{acct: "cur", key: "x3", date: "2026-03-18", amt: -80, scheme: "bacs", desc: "ltd 12345678"})
}

func TestPayeeKeys(t *testing.T) {
	f := newInsightFixture(t)
	f.payeeKeyTxns()
	got := f.flows()
	wantKey := map[string]string{
		"mg1": "mg:grp_1", "mg1b": "mg:grp_1", "mg2": "mg:grp_2", "mg3": "mg:grp_3",
		"n_coffee": "n:coffee co",
		"mu1":      "mu:user_1", "mu1b": "mu:user_1", "mu2": "mu:user_2",
		"col1": "n:gym club", "col2": "n:landlord properties", "ob_gym": "n:gym club",
		"x1": "x:faster_payments", "x1b": "x:faster_payments", "x2": "x:unknown", "x3": "x:bacs",
	}
	for k, w := range wantKey {
		if got[k].payeeKey != w {
			t.Errorf("%s payee_key = %q, want %q", k, got[k].payeeKey, w)
		}
	}
	// Automatic merging: grp_1 and grp_2 share their most frequent normalised merchant name, so they
	// are one payee, identified by the member with the earliest first transaction (grp_2). The
	// n: payee of the same name and the other groups never join it.
	wantPayee := map[string]string{
		"mg1": "mg:grp_2", "mg1b": "mg:grp_2", "mg2": "mg:grp_2", "mg3": "mg:grp_3", "n_coffee": "n:coffee co",
		"mu1": "mu:user_1", "mu1b": "mu:user_1", "mu2": "mu:user_2",
		"col1": "n:gym club", "ob_gym": "n:gym club", "col2": "n:landlord properties",
		"x1": "x:faster_payments", "x2": "x:unknown",
	}
	for k, w := range wantPayee {
		if got[k].payeeID != w {
			t.Errorf("%s payee_id = %q, want %q", k, got[k].payeeID, w)
		}
	}
	if got["col1"].payeeID == got["col2"].payeeID {
		t.Error("two names on shared bank details are one payee")
	}
	// The display name is the most frequent raw name.
	if got["mg2"].payeeName != "Coffee Co" {
		t.Errorf("merged payee name = %q, want the most frequent raw name (Coffee Co, twice)", got["mg2"].payeeName)
	}
	if got["mu1"].payeeName != "A Friend" && got["mu1"].payeeName != "Friend Renamed" {
		t.Errorf("person payee name = %q", got["mu1"].payeeName)
	}

	// The most frequent normalised merchant name decides, ties by the lowest name: a group whose
	// names split evenly takes the lower one.
	f.tx(itx{acct: "cur", key: "tie_a", date: "2026-04-01", amt: -1, merchant: "Zed Shop", detail: merchantDetail("grp_t", "Zed Shop")})
	f.tx(itx{acct: "cur", key: "tie_b", date: "2026-04-02", amt: -1, merchant: "Alpha Shop", detail: merchantDetail("grp_t", "Alpha Shop")})
	f.tx(itx{acct: "cur", key: "alpha", date: "2026-04-03", amt: -1, merchant: "Alpha Shop", detail: merchantDetail("grp_u", "Alpha Shop")})
	got = f.flows()
	if got["tie_a"].payeeID != got["alpha"].payeeID || got["tie_a"].payeeID != "mg:grp_t" {
		t.Errorf("tied names: payee ids %q and %q, want both mg:grp_t", got["tie_a"].payeeID, got["alpha"].payeeID)
	}
}

func TestCanonicalKeys(t *testing.T) {
	f := newInsightFixture(t)
	ctx := context.Background()
	mg := func(key, date, gid, name string, amt int64) {
		f.tx(itx{acct: "cur", key: key, date: date, amt: amt, merchant: name, detail: merchantDetail(gid, name)})
	}

	// Group identity: the member with the earliest first transaction, moving only when an earlier
	// transaction is imported for another member.
	mg("id_a", "2026-02-10", "gid_a", "Identity Shop", -100)
	mg("id_b", "2026-02-05", "gid_b", "Identity Shop", -100)
	mg("id_c", "2026-03-01", "gid_c", "Identity Shop", -100) // joins later under the existing identity
	got := f.flows()
	for _, k := range []string{"id_a", "id_b", "id_c"} {
		if got[k].payeeID != "mg:gid_b" {
			t.Errorf("%s payee_id = %q, want mg:gid_b", k, got[k].payeeID)
		}
	}
	mg("id_a_early", "2026-01-01", "gid_a", "Identity Shop", -100)
	got = f.flows()
	for _, k := range []string{"id_a", "id_b", "id_c", "id_a_early"} {
		if got[k].payeeID != "mg:gid_a" {
			t.Errorf("after an earlier import for gid_a, %s payee_id = %q, want mg:gid_a", k, got[k].payeeID)
		}
	}

	// A merge moves the whole automatic group, including members that join later.
	mg("mv_a", "2026-02-01", "mv_a", "Move Shop", -100)
	mg("mv_b", "2026-02-02", "mv_b", "Move Shop", -100)
	f.tx(itx{acct: "cur", key: "target", date: "2026-02-03", amt: -100, desc: "TARGET CO"})
	f.alias("mg:mv_b", false, "n:target co", "", 10)
	mg("mv_c_late", "2026-03-02", "mv_c", "Move Shop", -100)
	got = f.flows()
	for _, k := range []string{"mv_a", "mv_b", "mv_c_late", "target"} {
		if got[k].payeeID != "n:target co" {
			t.Errorf("%s payee_id = %q, want n:target co (a merge on one member moves the group)", k, got[k].payeeID)
		}
	}

	// separate takes a key out of its group; separate with merge moves that key alone.
	mg("sp_a", "2026-02-01", "sp_a", "Split Shop", -100)
	mg("sp_b", "2026-02-02", "sp_b", "Split Shop", -100)
	mg("sp_c", "2026-02-03", "sp_c", "Split Shop", -100)
	f.tx(itx{acct: "cur", key: "sp_target", date: "2026-02-04", amt: -100, desc: "SPLIT TARGET"})
	f.alias("mg:sp_b", true, "", "", 20)
	f.alias("mg:sp_c", true, "n:split target", "", 21)
	got = f.flows()
	if got["sp_a"].payeeID != "mg:sp_a" || got["sp_b"].payeeID != "mg:sp_b" {
		t.Errorf("separate: sp_a = %q, sp_b = %q, want each alone", got["sp_a"].payeeID, got["sp_b"].payeeID)
	}
	if got["sp_c"].payeeID != "n:split target" || got["sp_target"].payeeID != "n:split target" {
		t.Errorf("separate with merge: sp_c = %q, want n:split target", got["sp_c"].payeeID)
	}

	// Two members of one group carry different merges: the higher entry_seq wins.
	mg("tw_a", "2026-02-01", "tw_a", "Twin Shop", -100)
	mg("tw_b", "2026-02-02", "tw_b", "Twin Shop", -100)
	f.tx(itx{acct: "cur", key: "tw_t1", date: "2026-02-04", amt: -100, desc: "TWIN ONE"})
	f.tx(itx{acct: "cur", key: "tw_t2", date: "2026-02-05", amt: -100, desc: "TWIN TWO"})
	f.alias("mg:tw_a", false, "n:twin one", "", 31)
	f.alias("mg:tw_b", false, "n:twin two", "", 30)
	got = f.flows()
	if got["tw_a"].payeeID != "n:twin one" || got["tw_b"].payeeID != "n:twin one" {
		t.Errorf("competing merges: %q, %q, want n:twin one (entry_seq 31 beats 30)", got["tw_a"].payeeID, got["tw_b"].payeeID)
	}

	// Settings on a non-canonical key reach the payee; per setting the higher entry_seq wins among
	// the payee's rows, and a null leaves a lower row's value in place.
	mg("st_a", "2026-02-10", "st_a", "Setting Shop", -100)
	mg("st_b", "2026-02-05", "st_b", "Setting Shop", -100) // canonical
	f.setting("mg:st_a", "First Name", "", "cat_first", "", nil, 40)
	f.setting("mg:st_b", "Second Name", "", "", "", nil, 41)
	got = f.flows()
	for _, k := range []string{"st_a", "st_b"} {
		r := got[k]
		if r.payeeID != "mg:st_b" || r.payeeName != "Second Name" || r.category == nil || *r.category != "cat_first" {
			t.Errorf("%s = %+v, want payee mg:st_b named Second Name with category cat_first", k, r)
		}
	}

	// A merge loop through automatic groups leaves its groups unmerged and is reported.
	mg("lp_a", "2026-02-01", "lp_a", "Loop A", -100)
	mg("lp_b", "2026-02-02", "lp_b", "Loop B", -100)
	f.alias("mg:lp_a", false, "mg:lp_b", "", 50)
	f.alias("mg:lp_b", false, "mg:lp_a", "", 51)
	// A chain of twelve: ten steps reach the end, eleven do not.
	for i := 1; i <= 12; i++ {
		mg(fmt.Sprintf("ch%02d", i), fmt.Sprintf("2026-02-%02d", i), fmt.Sprintf("ch%02d", i), fmt.Sprintf("Chain Shop %c", 'a'+i), -100)
	}
	for i := 1; i < 12; i++ {
		f.alias(fmt.Sprintf("mg:ch%02d", i), false, fmt.Sprintf("mg:ch%02d", i+1), "", int64(60+i))
	}
	got = f.flows()
	if got["lp_a"].payeeID != "mg:lp_a" || got["lp_b"].payeeID != "mg:lp_b" {
		t.Errorf("a merge loop: lp_a = %q, lp_b = %q, want each unmerged", got["lp_a"].payeeID, got["lp_b"].payeeID)
	}
	if got["ch01"].payeeID != "mg:ch01" {
		t.Errorf("eleven merge steps: ch01 = %q, want it unmerged (the walk stops at ten)", got["ch01"].payeeID)
	}
	if got["ch02"].payeeID != "mg:ch12" || got["ch11"].payeeID != "mg:ch12" {
		t.Errorf("ten merge steps: ch02 = %q, ch11 = %q, want mg:ch12", got["ch02"].payeeID, got["ch11"].payeeID)
	}

	// An n: rule follows the normaliser: it is stored against a key and a raw name, and the view
	// resolves the key from the name at read time.
	f.tx(itx{acct: "cur", key: "nr", date: "2026-02-01", amt: -100, desc: "ACME WIDGETS LTD 01NOV26 REF123456"})
	f.setting("n:stale form of the key", "Acme", "", "", "Acme Widgets Ltd 987654", nil, 70)
	f.alias("n:another stale form", false, "n:target co", "ACME WIDGETS LTD", 71)
	got = f.flows()
	if r := got["nr"]; r.payeeKey != "n:acme widgets" || r.payeeName != "Acme" || r.payeeID != "n:target co" {
		t.Errorf("n: rules through name_sample: %+v, want key n:acme widgets, named Acme, merged into n:target co", r)
	}

	// Rule status: one row per rule row, with the problem.
	type status struct {
		key     string
		matches bool
		last    *time.Time
		problem *string
		detail  map[string]any
	}
	rows, err := f.q.Query(ctx, `SELECT rule_table, key, matches, last_match_date, problem, detail FROM agent.payee_rule_status`)
	if err != nil {
		t.Fatal(err)
	}
	st := map[string]status{}
	for rows.Next() {
		var table string
		var s status
		if err := rows.Scan(&table, &s.key, &s.matches, &s.last, &s.problem, &s.detail); err != nil {
			t.Fatal(err)
		}
		st[table+" "+s.key] = s
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	prob := func(k string) string {
		s, ok := st[k]
		if !ok {
			return "MISSING"
		}
		if s.problem == nil {
			return "<nil>"
		}
		return *s.problem
	}
	for k, w := range map[string]string{
		"payee_alias mg:lp_a": "merge_loop", "payee_alias mg:lp_b": "merge_loop", "payee_alias mg:ch01": "merge_loop",
		"payee_alias mg:ch02": "<nil>", "payee_alias mg:mv_b": "<nil>", "payee_alias mg:sp_b": "<nil>",
		"payee_setting mg:st_a": "<nil>", "payee_setting n:acme widgets": "<nil>", "payee_alias n:acme widgets": "<nil>",
	} {
		if got := prob(k); got != w {
			t.Errorf("rule status %s problem = %s, want %s", k, got, w)
		}
	}
	if s := st["payee_setting n:acme widgets"]; !s.matches || s.last == nil || s.last.Format("2006-01-02") != "2026-03-02" || s.detail["name"] != "Acme" {
		t.Errorf("n: setting status = %+v, want a match with last_match_date 2026-03-02 (the newest row of the payee it applies to, after the merge) and its settings", s)
	}

	// A rule that names a key with no transactions, a series nobody has and a category no
	// transaction carries is no_match; a rule on a merge target with no transactions of its own is
	// not, because it applies to the payee merged into it.
	f.alias("mg:never_seen", true, "", "", 80)
	f.setting("n:nobody", "Nobody", "", "", "", nil, 81)
	f.setting("n:target co", "", "", "target_cat", "", nil, 82)
	f.setting("mg:empty_target", "Empty Target", "", "", "", nil, 83)
	mg("et_a", "2026-03-03", "et_a", "Empty Target Feeder", -100)
	f.alias("mg:et_a", false, "mg:empty_target", "", 84)
	// An inferred series is keyed by its first transaction: confirm decisions on that key match, and
	// on the key of a later transaction of the same series (a key that no longer exists) do not.
	var infKey string
	if err := f.q.QueryRow(ctx, `SELECT 'inf:' || id FROM agent.cash_flows WHERE payee_id = 'n:target co' ORDER BY txn_date, id LIMIT 1`).Scan(&infKey); err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO core.series_setting (series_key, recurring, entry_seq, entered_at) VALUES
		('dd:nothing', 'ignore', 90, now()), ($1, 'confirm', 91, now()), ('inf:' || $2, 'confirm', 94, now())`,
		infKey, f.ids["mv_c_late"].String())
	f.exec(`INSERT INTO core.category_name (category, name, entry_seq, entered_at) VALUES
		('category_unused', 'Unused', 92, now()), ('cat_first', 'First Category', 93, now())`)
	rows, err = f.q.Query(ctx, `SELECT rule_table, key, matches, last_match_date, problem, detail FROM agent.payee_rule_status`)
	if err != nil {
		t.Fatal(err)
	}
	st = map[string]status{}
	for rows.Next() {
		var table string
		var s status
		if err := rows.Scan(&table, &s.key, &s.matches, &s.last, &s.problem, &s.detail); err != nil {
			t.Fatal(err)
		}
		st[table+" "+s.key] = s
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for k, w := range map[string]string{
		"payee_alias mg:never_seen": "no_match", "payee_setting n:nobody": "no_match",
		"payee_setting mg:empty_target": "<nil>", "payee_setting n:target co": "<nil>",
		"series_setting dd:nothing": "no_match", "series_setting " + infKey: "<nil>",
		"series_setting inf:" + f.ids["mv_c_late"].String(): "no_match",
		"category_name category_unused":                     "no_match", "category_name cat_first": "<nil>",
	} {
		if got := prob(k); got != w {
			t.Errorf("rule status %s problem = %s, want %s", k, got, w)
		}
	}
	var n, want int
	if err := f.q.QueryRow(ctx, `SELECT count(*) FROM agent.payee_rule_status`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := f.q.QueryRow(ctx, `SELECT (SELECT count(*) FROM core.payee_alias) + (SELECT count(*) FROM core.payee_setting)
		+ (SELECT count(*) FROM core.series_setting) + (SELECT count(*) FROM core.category_name)`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Errorf("agent.payee_rule_status has %d rows, want one per rule row (%d)", n, want)
	}
}

// flowTxns writes the transactions of the flow-class tests: one or more for every rule, pairing
// case and class-setting direction.
func (f *insightFixture) flowTxns() {
	// Rule 1: a pot move, even when its payee has a class setting.
	f.setting("n:broker co", "", "investment", "", "", nil, 2)
	f.tx(itx{acct: "cur", key: "pot", date: "2026-03-02", amt: -1000, detail: md("pot_id", "pot_cur")})
	f.tx(itx{acct: "cur", key: "pot_broker", date: "2026-03-02", amt: -1111, desc: "BROKER CO", detail: md("pot_id", "pot_cur")})
	f.tx(itx{acct: "joint", key: "pot_joint", date: "2026-03-02", amt: 1000, detail: md("pot_id", "pot_joint")})

	// Rule 2: every Flex shape, and an ordinary Flex purchase.
	f.tx(itx{acct: "cur", key: "flex_repay", date: "2026-03-03", amt: -2000, detail: md("flex_transaction_type", "repayment")})
	f.tx(itx{acct: "flex", key: "flex_repay_in", date: "2026-03-03", amt: 2000, detail: md("flex_transaction_type", "repayment")})
	f.tx(itx{acct: "cur", key: "flex_moved_cur", date: "2026-03-03", amt: -3000, detail: md("flex_transaction_type", "transaction_moved_to_flex_current_account_side")})
	f.tx(itx{acct: "flex", key: "flex_moved_flex", date: "2026-03-03", amt: 3000, detail: md("flex_transaction_type", "transaction_moved_to_flex_flex_side")})
	f.tx(itx{acct: "cur", key: "flex_mm_cur", date: "2026-03-03", amt: -400, detail: md("flex_transaction_type", "flex_move_money_transfer_current_account_side")})
	f.tx(itx{acct: "flex", key: "flex_mm_flex", date: "2026-03-03", amt: 400, detail: md("flex_transaction_type", "flex_move_money_transfer_flex_side")})
	f.tx(itx{acct: "flex", key: "flex_buy", date: "2026-03-03", amt: -123, desc: "SOME SHOP", detail: md("flex_transaction_type", "purchase")})

	// Rule 3: a held counterparty in the same scope, by bank reference (with the hyphen a Monzo
	// sort code may carry), by Monzo account id, and for an ended account.
	f.tx(itx{acct: "cur", key: "own_ref", date: "2026-03-04", amt: -5000, cp: "My Other Bank", detail: cpDetail("sort_code", "30-99-99", "account_number", "33333333")})
	f.tx(itx{acct: "cur", key: "own_id", date: "2026-03-04", amt: -600, cp: "Savings", detail: cpDetail("account_id", "acc_sav", "user_id", "user_self")})
	f.tx(itx{acct: "sav", key: "own_id_in", date: "2026-03-04", amt: 600, cp: "Current", detail: cpDetail("account_id", "acc_cur", "user_id", "user_self")})
	f.tx(itx{acct: "cur", key: "own_old", date: "2026-03-04", amt: -700, cp: "Old Account", detail: cpDetail("sort_code", "040004", "account_number", "44444444")})
	// The same bank reference is on three accounts: the row still appears once.
	f.tx(itx{acct: "cur", key: "own_dup", date: "2026-03-04", amt: -800, cp: "My Other Bank", detail: cpDetail("sort_code", "309999", "account_number", "33333333")})
	// Not held: the same sort code, another account number.
	f.tx(itx{acct: "cur", key: "not_held", date: "2026-03-04", amt: -900, cp: "Someone Else", detail: cpDetail("sort_code", "040004", "account_number", "99999999")})

	// Rule 4: the Open Banking side of a transfer, paired in the same scope; across scopes it is
	// rule 6 and a scope_transfer. The Monzo side names the Open Banking account.
	ob := cpDetail("sort_code", "30-99-99", "account_number", "33333333")
	f.tx(itx{acct: "cur", key: "pair_cur", date: "2026-03-10", amt: -5100, cp: "My Other Bank", detail: ob})
	f.tx(itx{acct: "ob", key: "pair_ob", date: "2026-03-11", amt: 5100, desc: "FASTER PAYMENT FROM SELF"})
	f.tx(itx{acct: "joint", key: "xpair_joint", date: "2026-03-12", amt: -7000, cp: "My Other Bank", detail: ob})
	f.tx(itx{acct: "ob", key: "xpair_ob", date: "2026-03-12", amt: 7000, desc: "FASTER PAYMENT FROM JOINT"})
	// Ambiguous: two candidate legs for one Monzo row; two Monzo rows for one leg; four days apart;
	// exactly the window apart; another currency.
	f.tx(itx{acct: "cur", key: "amb1_cur", date: "2026-03-20", amt: -3000, cp: "My Other Bank", detail: ob})
	f.tx(itx{acct: "ob", key: "amb1_ob_a", date: "2026-03-20", amt: 3000, desc: "AMBIGUOUS A"})
	f.tx(itx{acct: "ob", key: "amb1_ob_b", date: "2026-03-20", amt: 3000, desc: "AMBIGUOUS B"})
	f.tx(itx{acct: "cur", key: "amb2_cur", date: "2026-03-21", amt: -4000, cp: "My Other Bank", detail: ob})
	f.tx(itx{acct: "sav", key: "amb2_sav", date: "2026-03-22", amt: -4000, cp: "My Other Bank", detail: ob})
	f.tx(itx{acct: "ob", key: "amb2_ob", date: "2026-03-21", amt: 4000, desc: "AMBIGUOUS C"})
	f.tx(itx{acct: "cur", key: "far_cur", date: "2026-03-21", amt: -9000, cp: "My Other Bank", detail: ob})
	f.tx(itx{acct: "ob", key: "far_ob", date: "2026-03-25", amt: 9000, desc: "FOUR DAYS LATER"})
	f.tx(itx{acct: "cur", key: "edge_cur", date: "2026-03-21", amt: -9100, cp: "My Other Bank", detail: ob})
	f.tx(itx{acct: "ob", key: "edge_ob", date: "2026-03-24", amt: 9100, desc: "THREE DAYS LATER"})
	f.tx(itx{acct: "cur", key: "eur_cur", date: "2026-03-21", amt: -9200, cp: "My Other Bank", detail: ob})
	f.tx(itx{acct: "ob", key: "eur_ob", date: "2026-03-21", amt: 9200, currency: "EUR", desc: "EURO LEG"})

	// Held credit card repayments. Paired: both sides internal. Unpaired: the card side is a card
	// credit and the paying side spending. Class setting on the paying payee: no pairing. Two card
	// credits for one payment: none pair.
	f.tx(itx{acct: "card", key: "cc_in", date: "2026-03-15", amt: 20000, desc: "PAYMENT RECEIVED THANK YOU"})
	f.tx(itx{acct: "cur", key: "cc_out", date: "2026-03-14", amt: -20000, desc: "MADE UP CARD CO"})
	f.tx(itx{acct: "card", key: "cc2_in", date: "2026-03-16", amt: 30001, desc: "PAYMENT RECEIVED THANK YOU"})
	f.tx(itx{acct: "cur", key: "cc2_out", date: "2026-03-16", amt: -30000, desc: "MADE UP CARD CO"})
	f.tx(itx{acct: "card", key: "cc3_in", date: "2026-03-17", amt: 40000, desc: "PAYMENT RECEIVED THANK YOU"})
	f.tx(itx{acct: "cur", key: "cc3_out", date: "2026-03-17", amt: -40000, desc: "DEBT CARD CO"})
	f.setting("n:debt card co", "", "debt_repayment", "", "", nil, 1)
	f.tx(itx{acct: "card", key: "cc4_in_a", date: "2026-03-18", amt: 5500, desc: "PAYMENT RECEIVED THANK YOU"})
	f.tx(itx{acct: "card", key: "cc4_in_b", date: "2026-03-18", amt: 5500, desc: "PAYMENT RECEIVED THANK YOU"})
	f.tx(itx{acct: "cur", key: "cc4_out", date: "2026-03-18", amt: -5500, desc: "MADE UP CARD CO"})

	// One debit, two card credits of its amount, only one of which names the debit's account: the
	// debit has two possible partners, so neither credit pairs with it.
	f.tx(itx{acct: "cur", key: "two_out", date: "2026-03-26", amt: -6600, desc: "MADE UP CARD CO"})
	f.tx(itx{acct: "card", key: "two_in_named", date: "2026-03-26", amt: 6600, desc: "PAYMENT RECEIVED THANK YOU", cp: "Current", detail: cpDetail("account_id", "acc_cur")})
	f.tx(itx{acct: "card", key: "two_in_plain", date: "2026-03-26", amt: 6600, desc: "PAYMENT RECEIVED THANK YOU"})
	// A card credit and a transfer between held accounts of the same amount: the transfer is
	// already classified (rule 3, or rule 6 across scopes) and is not a card payment, so the credit
	// stays a refund.
	f.tx(itx{acct: "cur", key: "held_out", date: "2026-03-27", amt: -7700, cp: "Savings", detail: cpDetail("account_id", "acc_sav")})
	f.tx(itx{acct: "card", key: "held_credit", date: "2026-03-27", amt: 7700, desc: "PAYMENT RECEIVED THANK YOU"})
	f.tx(itx{acct: "cur", key: "xheld_out", date: "2026-03-27", amt: -7800, cp: "Joint", detail: cpDetail("account_id", "acc_joint")})
	f.tx(itx{acct: "card", key: "xheld_credit", date: "2026-03-27", amt: 7800, desc: "PAYMENT RECEIVED THANK YOU"})

	// Rule 5, in each direction.
	f.tx(itx{acct: "cur", key: "inv_out", date: "2026-03-05", amt: -1000, desc: "BROKER CO"})
	f.tx(itx{acct: "cur", key: "inv_in", date: "2026-03-06", amt: 500, desc: "BROKER CO"})
	f.setting("n:shop co", "", "income", "", "", ptr(true), 3)
	f.tx(itx{acct: "cur", key: "inc_neg", date: "2026-03-05", amt: -300, desc: "SHOP CO"})
	f.tx(itx{acct: "cur", key: "inc_pos", date: "2026-03-05", amt: 300, desc: "SHOP CO"})
	f.setting("n:spend co", "", "spending", "", "", nil, 4)
	f.tx(itx{acct: "cur", key: "sp_neg", date: "2026-03-05", amt: -200, desc: "SPEND CO"})
	f.tx(itx{acct: "cur", key: "sp_refund", date: "2026-03-05", amt: 150, desc: "SPEND CO", scheme: "mastercard"})
	f.setting("n:debt co", "", "debt_repayment", "", "", nil, 5)
	f.tx(itx{acct: "cur", key: "debt_neg", date: "2026-03-05", amt: -900, desc: "DEBT CO"})
	// A payee classed for debt: its money in is borrowing, never income (a drawdown, or a cashback
	// the operator can put right row by row).
	f.tx(itx{acct: "cur", key: "debt_cashback", date: "2026-03-05", amt: 50, desc: "DEBT CO"})
	f.tx(itx{acct: "cur", key: "debt_drawdown", date: "2026-03-05", amt: 800000, desc: "DEBT CO"})
	// Borrowing set explicitly: money in is borrowing, and money out is not a class this setting
	// applies to, so it falls through like any other direction mismatch.
	f.setting("n:lender co", "", "borrowing", "", "", nil, 11)
	f.tx(itx{acct: "cur", key: "lend_in", date: "2026-03-05", amt: 90000, desc: "LENDER CO"})
	f.tx(itx{acct: "cur", key: "lend_out", date: "2026-03-05", amt: -70, desc: "LENDER CO"})
	f.setting("n:refund co", "", "refund", "", "", nil, 6)
	f.tx(itx{acct: "cur", key: "ref_pos", date: "2026-03-05", amt: 80, desc: "REFUND CO"})
	f.tx(itx{acct: "cur", key: "ref_neg", date: "2026-03-05", amt: -80, desc: "REFUND CO"})
	f.setting("n:partner", "", "scope_transfer", "", "", nil, 7)
	f.tx(itx{acct: "cur", key: "scope_set", date: "2026-03-05", amt: 15, desc: "PARTNER"})
	f.setting("n:external co", "", "own_external", "", "", nil, 8)
	f.tx(itx{acct: "cur", key: "ext_out", date: "2026-03-05", amt: -16, desc: "EXTERNAL CO"})
	f.setting("n:internal co", "", "internal", "", "", nil, 9)
	f.tx(itx{acct: "cur", key: "int_out", date: "2026-03-05", amt: -17, desc: "INTERNAL CO"})
	f.tx(itx{acct: "cur", key: "int_in", date: "2026-03-05", amt: 17, desc: "INTERNAL CO"})
	// A class setting beats a cross-scope held counterparty (rule 5 before rule 6).
	f.setting("n:joint partner", "", "own_external", "", "", nil, 10)
	f.tx(itx{acct: "cur", key: "rule_before_6", date: "2026-03-05", amt: -123, cp: "Joint Partner", detail: cpDetail("account_id", "acc_joint")})

	// Rule 6: held accounts in another scope, by Monzo account id and by bank reference.
	f.tx(itx{acct: "cur", key: "scope_id", date: "2026-03-05", amt: -222, cp: "Joint", detail: cpDetail("account_id", "acc_joint")})
	f.tx(itx{acct: "cur", key: "scope_biz", date: "2026-03-05", amt: -223, cp: "Business", detail: cpDetail("sort_code", "04-00-04", "account_number", "66666666")})
	f.tx(itx{acct: "biz", key: "scope_biz_in", date: "2026-03-05", amt: 223, cp: "Personal", detail: cpDetail("account_id", "acc_cur")})

	// Rule 7: the self flag, with and without a held match.
	f.tx(itx{acct: "cur", key: "self_ext", date: "2026-03-06", amt: -333, desc: "TO MY OTHER BANK", detail: md("p2p_is_to_self", true)})
	f.tx(itx{acct: "cur", key: "self_held", date: "2026-03-06", amt: -334, cp: "Savings", detail: merge(md("p2p_is_to_self", true), cpDetail("account_id", "acc_sav"))})

	// Rules 8 to 11.
	f.tx(itx{acct: "cur", key: "loan_out", date: "2026-03-07", amt: -100, scheme: "instalment_loan", desc: "LOAN CO"})
	f.tx(itx{acct: "cur", key: "loan_in", date: "2026-03-07", amt: 101, scheme: "instalment_loan", desc: "LOAN CO PAYOUT"})
	f.tx(itx{acct: "cur", key: "mc_in", date: "2026-03-07", amt: 10, scheme: "mastercard", desc: "A SHOP REFUND"})
	f.tx(itx{acct: "cur", key: "gps_in", date: "2026-03-07", amt: 11, scheme: "gps_mastercard", desc: "A SHOP REFUND"})
	f.tx(itx{acct: "card", key: "card_credit", date: "2026-03-07", amt: 12, desc: "CASHBACK"})
	f.tx(itx{acct: "cur", key: "salary", date: "2026-03-07", amt: 100000, scheme: "bacs", desc: "MADE UP EMPLOYER"})
	f.tx(itx{acct: "cur", key: "spend", date: "2026-03-07", amt: -450, scheme: "mastercard", desc: "COFFEE", merchant: "Coffee Co", detail: merchantDetail("flow_g", "Coffee Co")})
	f.tx(itx{acct: "ob", key: "ob_spend", date: "2026-03-07", amt: -451, desc: "A SHOP", category: "PURCHASE"})
	f.tx(itx{acct: "cur", key: "eur_spend", date: "2026-03-07", amt: -1500, currency: "EUR", desc: "ABROAD"})

	// Pending rows are classified; declined rows are not in the view at all.
	f.tx(itx{acct: "cur", key: "pending", date: "2026-04-02", amt: -777, status: "pending", desc: "PENDING SHOP"})
	f.tx(itx{acct: "cur", key: "declined", date: "2026-04-02", amt: -888, status: "declined", desc: "DECLINED SHOP"})
	// An ended account's rows after its end date are not in agent.transactions.
	f.tx(itx{acct: "old", key: "after_end", date: "2026-03-07", amt: -5, desc: "AFTER THE END"})
}

func TestFlowClasses(t *testing.T) {
	f := newInsightFixture(t)
	f.flowTxns()
	got := f.flows()

	if _, ok := got["declined"]; ok {
		t.Error("a declined transaction is in agent.cash_flows")
	}
	if _, ok := got["after_end"]; ok {
		t.Error("a row after its account's end date is in agent.cash_flows")
	}
	if r := got["pending"]; r.status != "pending" || r.class != "spending" {
		t.Errorf("pending = %+v, want a pending spending row", r)
	}

	type want struct{ class, reason, paired string }
	for k, w := range map[string]want{
		"pot": {"internal", "pot", ""}, "pot_broker": {"internal", "pot", ""}, "pot_joint": {"internal", "pot", ""},
		"flex_repay": {"internal", "flex", ""}, "flex_repay_in": {"internal", "flex", ""},
		"flex_moved_cur": {"internal", "flex", ""}, "flex_moved_flex": {"internal", "flex", ""},
		"flex_mm_cur": {"internal", "flex", ""}, "flex_mm_flex": {"internal", "flex", ""},
		"flex_buy": {"spending", "direction", ""},

		"own_ref": {"internal", "own_account", ""}, "own_id": {"internal", "own_account", ""},
		"own_id_in": {"internal", "own_account", ""}, "own_old": {"internal", "own_account", ""},
		"own_dup": {"internal", "own_account", ""}, "not_held": {"spending", "direction", ""},

		"pair_cur": {"internal", "own_account", ""}, "pair_ob": {"internal", "paired", "pair_cur"},
		"xpair_joint": {"scope_transfer", "own_account", ""}, "xpair_ob": {"scope_transfer", "paired", "xpair_joint"},
		"amb1_cur": {"internal", "own_account", ""}, "amb1_ob_a": {"income", "direction", ""}, "amb1_ob_b": {"income", "direction", ""},
		"amb2_cur": {"internal", "own_account", ""}, "amb2_sav": {"internal", "own_account", ""}, "amb2_ob": {"income", "direction", ""},
		"far_cur": {"internal", "own_account", ""}, "far_ob": {"income", "direction", ""},
		"edge_cur": {"internal", "own_account", ""}, "edge_ob": {"internal", "paired", "edge_cur"},
		"eur_cur": {"internal", "own_account", ""}, "eur_ob": {"income", "direction", ""},

		"cc_in": {"internal", "paired", "cc_out"}, "cc_out": {"internal", "paired", "cc_in"},
		"cc2_in": {"refund", "card_credit", ""}, "cc2_out": {"spending", "direction", ""},
		"cc3_in": {"refund", "card_credit", ""}, "cc3_out": {"debt_repayment", "rule", ""},
		"cc4_in_a": {"refund", "card_credit", ""}, "cc4_in_b": {"refund", "card_credit", ""}, "cc4_out": {"spending", "direction", ""},

		"two_out": {"spending", "direction", ""}, "two_in_named": {"internal", "own_account", ""}, "two_in_plain": {"refund", "card_credit", ""},
		"held_out": {"internal", "own_account", ""}, "held_credit": {"refund", "card_credit", ""},
		"xheld_out": {"scope_transfer", "own_account", ""}, "xheld_credit": {"refund", "card_credit", ""},

		"inv_out": {"investment", "rule", ""}, "inv_in": {"investment", "rule", ""},
		"inc_neg": {"spending", "direction", ""}, "inc_pos": {"income", "rule", ""},
		"sp_neg": {"spending", "rule", ""}, "sp_refund": {"refund", "card_credit", ""},
		"debt_neg": {"debt_repayment", "rule", ""}, "debt_cashback": {"borrowing", "rule", ""},
		"debt_drawdown": {"borrowing", "rule", ""}, "lend_in": {"borrowing", "rule", ""}, "lend_out": {"spending", "direction", ""},
		"ref_pos": {"refund", "rule", ""}, "ref_neg": {"spending", "direction", ""},
		"scope_set": {"scope_transfer", "rule", ""}, "ext_out": {"own_external", "rule", ""},
		"int_out": {"internal", "rule", ""}, "int_in": {"internal", "rule", ""},
		"rule_before_6": {"own_external", "rule", ""},

		"scope_id": {"scope_transfer", "own_account", ""}, "scope_biz": {"scope_transfer", "own_account", ""},
		"scope_biz_in": {"scope_transfer", "own_account", ""},

		"self_ext": {"own_external", "self_flag", ""}, "self_held": {"internal", "own_account", ""},

		"loan_out": {"debt_repayment", "scheme", ""}, "loan_in": {"income", "direction", ""},
		"mc_in": {"refund", "card_credit", ""}, "gps_in": {"refund", "card_credit", ""}, "card_credit": {"refund", "card_credit", ""},
		"salary": {"income", "direction", ""}, "spend": {"spending", "direction", ""}, "ob_spend": {"spending", "direction", ""},
		"eur_spend": {"spending", "direction", ""},
	} {
		r, ok := got[k]
		if !ok {
			t.Errorf("%s is missing from agent.cash_flows", k)
			continue
		}
		if r.class != w.class || r.reason != w.reason {
			t.Errorf("%s = %s/%s, want %s/%s", k, r.class, r.reason, w.class, w.reason)
		}
		switch {
		case w.paired == "" && r.paired != nil:
			t.Errorf("%s paired_id = %s, want null", k, *r.paired)
		case w.paired != "" && (r.paired == nil || *r.paired != f.ids[w.paired]):
			t.Errorf("%s paired_id = %v, want %s", k, r.paired, w.paired)
		}
	}
	// The employer setting is on the income rows of its payee, and scopes follow the account.
	if e := got["inc_pos"].employer; e == nil || !*e {
		t.Errorf("inc_pos employer = %v, want true", e)
	}
	if got["xpair_ob"].scope != "personal" || got["xpair_joint"].scope != "joint" || got["pot_joint"].scope != "joint" {
		t.Errorf("scopes: xpair_ob %s, xpair_joint %s, pot_joint %s", got["xpair_ob"].scope, got["xpair_joint"].scope, got["pot_joint"].scope)
	}

	// An account the operator set to joint changes what a transfer to it is.
	f.tx(itx{acct: "cur", key: "to_obj", date: "2026-03-30", amt: -61, cp: "Joint Elsewhere", detail: cpDetail("account_id", "tl_joint")})
	f.tx(itx{acct: "ob-joint", key: "obj_in", date: "2026-03-30", amt: 61, desc: "FROM CURRENT"})
	got = f.flows()
	if got["to_obj"].class != "scope_transfer" || got["obj_in"].class != "scope_transfer" || got["obj_in"].reason != "paired" {
		t.Errorf("transfer to an account set to joint: %+v / %+v, want scope_transfer both, the Open Banking leg paired", got["to_obj"], got["obj_in"])
	}

	// Own category before and after a payee setting, and its operator name.
	f.tx(itx{acct: "cur", key: "cat1", date: "2026-03-31", amt: -10, desc: "CATEGORY CO", category: "category_0001"})
	f.exec(`INSERT INTO core.category_name (category, name, entry_seq, entered_at) VALUES ('category_0001', 'Hobbies', 100, now())`)
	got = f.flows()
	if r := got["cat1"]; r.txnCategory == nil || *r.txnCategory != "category_0001" || r.category == nil || *r.category != "category_0001" ||
		r.categoryName == nil || *r.categoryName != "Hobbies" {
		t.Errorf("category columns = %+v", r)
	}
	f.setting("n:category co", "", "", "groceries", "", nil, 101)
	got = f.flows()
	if r := got["cat1"]; *r.txnCategory != "category_0001" || r.category == nil || *r.category != "groceries" || r.categoryName != nil {
		t.Errorf("category after a payee setting = %+v, want txn_category kept and category groceries", r)
	}
}

// TestCashFlowsConservation: every transaction that is not declined appears exactly once, so per
// month and currency the sums and the row counts of agent.cash_flows equal those of agent.transactions.
func TestCashFlowsConservation(t *testing.T) {
	f := newInsightFixture(t)
	f.payeeKeyTxns()
	f.flowTxns()
	rows, err := f.q.Query(context.Background(), `
		WITH a AS (SELECT to_char(txn_date, 'YYYY-MM') AS m, currency, sum(amount_minor) AS s, count(*) AS n
		           FROM agent.transactions WHERE status <> 'declined' GROUP BY 1, 2),
		     b AS (SELECT to_char(txn_date, 'YYYY-MM') AS m, currency, sum(amount_minor) AS s, count(*) AS n
		           FROM agent.cash_flows GROUP BY 1, 2)
		SELECT coalesce(a.m, b.m), coalesce(a.currency, b.currency), a.s, b.s, a.n, b.n
		FROM a FULL JOIN b ON a.m = b.m AND a.currency = b.currency ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	groups := 0
	for rows.Next() {
		var m, cur string
		var sa, sb, na, nb *int64
		if err := rows.Scan(&m, &cur, &sa, &sb, &na, &nb); err != nil {
			t.Fatal(err)
		}
		groups++
		if sa == nil || sb == nil || *sa != *sb || *na != *nb {
			t.Errorf("%s %s: agent.transactions %v over %v rows, agent.cash_flows %v over %v rows", m, cur, sa, na, sb, nb)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if groups < 3 {
		t.Errorf("only %d month and currency groups compared", groups)
	}
	// agent.monthly_flows loses nothing either: its class columns, signed, add up to the sum of the
	// transactions of each month, scope, currency and status, so borrowing is a column of its own
	// and not a hole.
	var bad, months int
	if err := f.q.QueryRow(context.Background(), `
		WITH m AS (SELECT month, scope, currency, status,
		             income_minor + refund_minor + borrowing_minor - spending_minor - debt_repayment_minor
		             + investment_in_minor - investment_out_minor + own_external_in_minor - own_external_out_minor
		             + scope_transfer_in_minor - scope_transfer_out_minor + internal_in_minor - internal_out_minor AS s
		           FROM agent.monthly_flows),
		     c AS (SELECT to_char(txn_date, 'YYYY-MM') AS month, account_scope AS scope, currency, status, sum(amount_minor) AS s
		           FROM agent.cash_flows GROUP BY 1, 2, 3, 4)
		SELECT count(*) FILTER (WHERE m.s IS DISTINCT FROM c.s), count(*)
		FROM m FULL JOIN c USING (month, scope, currency, status)`).Scan(&bad, &months); err != nil {
		t.Fatal(err)
	}
	if bad != 0 || months < 3 {
		t.Errorf("%d of %d monthly_flows rows do not add up to their transactions", bad, months)
	}
	// Every transaction's key is a payee the resolution lists.
	var orphan int
	if err := f.q.QueryRow(context.Background(), `SELECT count(*) FROM agent.cash_flows c
		WHERE NOT EXISTS (SELECT 1 FROM agent.payee_keys k WHERE k.payee_key = c.payee_key)`).Scan(&orphan); err != nil {
		t.Fatal(err)
	}
	if orphan != 0 {
		t.Errorf("%d cash_flows rows have a payee_key that agent.payee_keys does not list", orphan)
	}
}

func TestPayeesAndYears(t *testing.T) {
	f := newInsightFixture(t)
	ctx := context.Background()
	today := londonToday(t, f.q)
	at := func(offset int) string { return day(today, offset).Format("2006-01-02") }
	// One payee, mixed raw names, money in and out over several years and two currencies, a
	// pending row (not counted) and a declined one.
	f.tx(itx{acct: "cur", key: "p1", date: at(-30), amt: -1000, desc: "WIDGETS LTD", category: "shopping"})
	f.tx(itx{acct: "cur", key: "p2", date: at(-20), amt: -2000, desc: "Widgets Ltd", category: "shopping"})
	f.tx(itx{acct: "cur", key: "p3", date: at(-10), amt: 500, desc: "refund 123456", cp: "Widgets Ltd", category: "general"})
	f.tx(itx{acct: "cur", key: "p4", date: at(-800), amt: -4000, desc: "widgets ltd 987654", category: "shopping"})
	f.tx(itx{acct: "cur", key: "p5", date: at(-5), amt: -900, desc: "widgets", status: "pending"})
	f.tx(itx{acct: "cur", key: "p6", date: at(-4), amt: -800, desc: "widgets", status: "declined"})
	f.tx(itx{acct: "cur", key: "p7", date: at(-3), amt: -300, desc: "widgets ltd", currency: "EUR", category: "shopping"})
	// Older than two years: the display name comes from the last two years when there are any.
	f.tx(itx{acct: "cur", key: "q1", date: at(-1000), amt: -10, desc: "OLD NAME CO"})
	f.tx(itx{acct: "cur", key: "q2", date: at(-1100), amt: -10, desc: "OLD NAME CO"})
	f.tx(itx{acct: "cur", key: "q3", date: at(-1200), amt: -10, desc: "Old Name Co"})
	f.tx(itx{acct: "cur", key: "q4", date: at(-30), amt: -10, desc: "Old Name Co Ltd"})
	f.tx(itx{acct: "cur", key: "r1", date: at(-3000), amt: -10, desc: "ANCIENT THING"})
	f.tx(itx{acct: "cur", key: "r2", date: at(-3001), amt: -10, desc: "Ancient Thing"})
	f.tx(itx{acct: "cur", key: "r3", date: at(-3002), amt: -10, desc: "Ancient Thing"})
	f.setting("n:salary co", "", "income", "", "", ptr(true), 5)
	f.tx(itx{acct: "cur", key: "s1", date: at(-40), amt: 200000, desc: "SALARY CO"})

	rows, err := f.q.Query(ctx, `SELECT payee_id, payee_name, currency, keys, name_variants, first_date, last_date, txn_count,
		out_minor, in_minor, out_12m_minor, in_12m_minor, flow_class, employer, category, category_name, recurring_series
		FROM agent.payees ORDER BY payee_id, currency`)
	if err != nil {
		t.Fatal(err)
	}
	type prow struct {
		id, name, currency string
		keys, variants     []string
		first, last        time.Time
		count              int64
		out, in, out12     int64
		in12               int64
		class              string
		employer           *bool
		category, catName  *string
		series             int64
	}
	got := map[string]prow{}
	for rows.Next() {
		var p prow
		if err := rows.Scan(&p.id, &p.name, &p.currency, &p.keys, &p.variants, &p.first, &p.last, &p.count, &p.out, &p.in,
			&p.out12, &p.in12, &p.class, &p.employer, &p.category, &p.catName, &p.series); err != nil {
			t.Fatal(err)
		}
		got[p.id+"/"+p.currency] = p
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	w := got["n:widgets/GBP"]
	if w.name != "Widgets Ltd" {
		t.Errorf("widgets name = %q, want the most frequent raw name (Widgets Ltd, three times of four)", w.name)
	}
	// Settled rows only: p1, p2, p3 and p4, which is older than a year.
	if w.count != 4 || w.out != 7000 || w.in != 500 || w.out12 != 3000 || w.in12 != 500 {
		t.Errorf("widgets totals = count %d out %d in %d out12 %d in12 %d, want 4, 7000, 500, 3000, 500", w.count, w.out, w.in, w.out12, w.in12)
	}
	if w.first.Format("2006-01-02") != at(-800) || w.last.Format("2006-01-02") != at(-10) {
		t.Errorf("widgets dates %v to %v, want %s to %s", w.first, w.last, at(-800), at(-10))
	}
	if len(w.keys) != 1 || w.keys[0] != "n:widgets" || len(w.variants) != 3 || w.variants[0] != "Widgets Ltd" {
		t.Errorf("widgets keys %v, variants %v, want one key and three variants, the most frequent first", w.keys, w.variants)
	}
	if w.class != "spending" || w.category == nil || *w.category != "shopping" || w.series != 0 {
		t.Errorf("widgets class %s, category %v, series %d", w.class, w.category, w.series)
	}
	if e := got["n:widgets/EUR"]; e.count != 1 || e.out != 300 || e.in != 0 {
		t.Errorf("widgets EUR = %+v, want its own row", e)
	}
	if on := got["n:old name co/GBP"]; on.name != "Old Name Co Ltd" {
		t.Errorf("old name co display = %q, want the only name from the last two years", on.name)
	}
	if an := got["n:ancient thing/GBP"]; an.name != "Ancient Thing" {
		t.Errorf("ancient thing display = %q, want the most frequent name over all time", an.name)
	}
	if s := got["n:salary co/GBP"]; s.class != "income" || s.employer == nil || !*s.employer || s.in != 200000 {
		t.Errorf("salary = %+v, want class income, employer, in 200000", s)
	}

	// A rename and a class setting beat the computed values.
	f.setting("n:widgets", "Widgets Incorporated", "investment", "", "", nil, 6)
	var name, class string
	if err := f.q.QueryRow(ctx, `SELECT payee_name, flow_class FROM agent.payees WHERE payee_id = 'n:widgets' AND currency = 'GBP'`).Scan(&name, &class); err != nil {
		t.Fatal(err)
	}
	if name != "Widgets Incorporated" || class != "investment" {
		t.Errorf("with settings: name %q, class %q", name, class)
	}

	// Per calendar year, settled rows only.
	yrows, err := f.q.Query(ctx, `SELECT year, out_minor, in_minor, txn_count FROM agent.payee_years
		WHERE payee_id = 'n:widgets' AND currency = 'GBP' ORDER BY year`)
	if err != nil {
		t.Fatal(err)
	}
	type yr struct{ year, out, in, n int64 }
	var years []yr
	for yrows.Next() {
		var y yr
		if err := yrows.Scan(&y.year, &y.out, &y.in, &y.n); err != nil {
			t.Fatal(err)
		}
		years = append(years, y)
	}
	if err := yrows.Err(); err != nil {
		t.Fatal(err)
	}
	byYear := map[int64]*yr{}
	for _, x := range []struct {
		off int
		amt int64
	}{{-30, -1000}, {-20, -2000}, {-10, 500}, {-800, -4000}} {
		y := int64(day(today, x.off).Year())
		if byYear[y] == nil {
			byYear[y] = &yr{year: y}
		}
		byYear[y].n++
		if x.amt < 0 {
			byYear[y].out -= x.amt
		} else {
			byYear[y].in += x.amt
		}
	}
	var wantYears []yr
	for y := int64(2000); y < 2100; y++ {
		if byYear[y] != nil {
			wantYears = append(wantYears, *byYear[y])
		}
	}
	if fmt.Sprint(years) != fmt.Sprint(wantYears) {
		t.Errorf("payee_years = %v, want %v", years, wantYears)
	}
}

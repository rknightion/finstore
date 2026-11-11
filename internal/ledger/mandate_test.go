package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/store"
)

// Made-up bank references.
const (
	refA = "112233 00000001"
	refB = "112233 00000002"
	refC = "112233 00000003"
)

func wantMandateID(source, bankRef, kind, key string) uuid.UUID {
	return uuid.NewSHA1(idNamespace, []byte(source+"\x1f"+bankRef+"\x1f"+kind+"\x1f"+key))
}

// mandates stores one 200 response of source carrying b, in its own fetch.
func (e *env) mandates(source string, b fBody) {
	e.t.Helper()
	e.respondAs(e.fetchAs(source, 1, "", nil), source, "/mandates", 200, body(b))
}

func (e *env) standingOrder(source, bankRef, key string) store.StandingOrder {
	e.t.Helper()
	s, err := e.core().GetStandingOrder(context.Background(), wantMandateID(source, bankRef, ledger.MandateStandingOrder, key))
	if err != nil {
		e.t.Fatalf("standing order %s: %v", key, err)
	}
	return s
}

func (e *env) directDebit(source, bankRef, key string) store.DirectDebit {
	e.t.Helper()
	d, err := e.core().GetDirectDebit(context.Background(), wantMandateID(source, bankRef, ledger.MandateDirectDebit, key))
	if err != nil {
		e.t.Fatalf("direct debit %s: %v", key, err)
	}
	return d
}

func TestMandateIDIsUUIDv5OfSourceBankRefKindAndKey(t *testing.T) {
	got := ledger.MandateID("truelayer:ob-monzo", refA, ledger.MandateDirectDebit, "MADE-UP-1")
	if got != wantMandateID("truelayer:ob-monzo", refA, ledger.MandateDirectDebit, "MADE-UP-1") || got.Version() != 5 {
		t.Fatalf("MandateID = %s", got)
	}
}

// A mandate goes to the fetching source's account whose source key is the bank reference; failing
// that, to the one unended account of any source with that bank reference; failing that, to none.
func TestMandateResolvesToTheFetchingSourcesAccountThenToAnyUnendedAccountWithItsBankRef(t *testing.T) {
	e := newEnv(t)
	// refA: the fetching source's own account, and another source's account with the same
	// reference. refB: only another source's account. refC: only an ended account.
	e.accounts(fAcc{ID: refA, Type: "joint", BankRef: refA})
	e.respondAs(e.fetchAs(otherSource, 1, "", nil), otherSource, "/accounts", 200, body(fBody{Accounts: []fAcc{
		{ID: "other_1", Type: "current", BankRef: refA}, {ID: "other_2", Type: "current", BankRef: refB},
		{ID: "other_3", Type: "current", BankRef: refC}}}))
	e.catchUp()
	e.operator(ledger.EntryAccountEnd, ledger.AccountEnd{Slug: "fake-other-current-3", LastDate: ptr("2030-01-31")})
	e.mandates(fakeSource, fBody{
		SOs: []fSO{{BankRef: refA, Key: "rent", Payee: "MADE UP"}, {BankRef: refB, Key: "rent", Payee: "MADE UP"}},
		DDs: []fDD{{BankRef: refC, Key: "MANDATE-1", Name: "MADE UP CO"}},
	})
	st := e.catchUp()
	if st.MandatesCreated != 3 || st.MandatesUnresolved != 1 {
		t.Fatalf("stats %+v", st)
	}
	if s := e.standingOrder(fakeSource, refA, "rent"); s.AccountID == nil || *s.AccountID != wantAccountID(fakeSource, refA) {
		t.Fatalf("refA standing order on %v, want the fetching source's account", s.AccountID)
	}
	if s := e.standingOrder(fakeSource, refB, "rent"); s.AccountID == nil || *s.AccountID != wantAccountID(otherSource, "other_2") {
		t.Fatalf("refB standing order on %v, want the other source's account", s.AccountID)
	}
	if d := e.directDebit(fakeSource, refC, "MANDATE-1"); d.AccountID != nil || d.Source != fakeSource || d.BankRef != refC {
		t.Fatalf("refC direct debit %+v, want no account (its only account has ended)", d)
	}
}

// A mandate whose account is unknown is kept with no account. A renormalise whose normalisers now
// read the account (here: a source the live ledger had no normaliser for) attaches it.
func TestUnresolvedMandateIsKeptAndRenormaliseResolvesIt(t *testing.T) {
	e := newEnv(t)
	e.l = e.ledger(false)
	e.respondAs(e.fetchAs(otherSource, 1, "", nil), otherSource, "/accounts", 200,
		body(fBody{Accounts: []fAcc{{ID: "other_1", Type: "current", BankRef: refA}}}))
	e.mandates(fakeSource, fBody{SOs: []fSO{{BankRef: refA, Key: "rent", Payee: "MADE UP"}},
		Lists: []fList{{ledger.MandateStandingOrder, refA}}})
	st := e.catchUp()
	if st.MandatesUnresolved != 1 || st.Unparseable != 1 {
		t.Fatalf("stats %+v", st)
	}
	s := e.standingOrder(fakeSource, refA, "rent")
	if s.AccountID != nil || s.DisappearedAt != nil {
		t.Fatalf("unresolved standing order %+v", s)
	}
	if n := e.countAgent("SELECT count(*) FROM agent.standing_orders WHERE account_slug IS NULL"); n != 1 {
		t.Fatalf("agent.standing_orders shows %d unattached, want 1", n)
	}

	e.l = e.ledger(true)
	e.renormalise(false)
	s = e.standingOrder(fakeSource, refA, "rent")
	if s.AccountID == nil || *s.AccountID != wantAccountID(otherSource, "other_1") {
		t.Fatalf("after renormalise the standing order is on %v", s.AccountID)
	}
}

func TestMandateListMarksMissingMandatesDisappearedAndClearsThemWhenTheyReturn(t *testing.T) {
	e := newEnv(t)
	e.accounts(fAcc{ID: refA, Type: "joint", BankRef: refA}, fAcc{ID: refB, Type: "current", BankRef: refB})
	rent := fSO{BankRef: refA, Key: "rent", Payee: "MADE UP"}
	gym := fSO{BankRef: refA, Key: "gym", Payee: "MADE UP GYM"}
	other := fSO{BankRef: refB, Key: "rent", Payee: "MADE UP"}
	power := fDD{BankRef: refA, Key: "MANDATE-1", Name: "MADE UP POWER"}
	e.mandates(fakeSource, fBody{SOs: []fSO{rent, gym, other}, DDs: []fDD{power}, Lists: []fList{
		{ledger.MandateStandingOrder, refA}, {ledger.MandateStandingOrder, refB}, {ledger.MandateDirectDebit, refA}}})
	e.catchUp()

	// gym is cancelled: the refA standing order list no longer has it. The refB list and the
	// direct debit list are not given, so they prove nothing.
	e.mandates(fakeSource, fBody{SOs: []fSO{rent}, Lists: []fList{{ledger.MandateStandingOrder, refA}}})
	if st := e.catchUp(); st.MandatesDisappeared != 1 {
		t.Fatalf("stats %+v", st)
	}
	if e.standingOrder(fakeSource, refA, "gym").DisappearedAt == nil || e.standingOrder(fakeSource, refA, "rent").DisappearedAt != nil ||
		e.standingOrder(fakeSource, refB, "rent").DisappearedAt != nil || e.directDebit(fakeSource, refA, "MANDATE-1").DisappearedAt != nil {
		t.Fatal("only gym should have disappeared")
	}

	// A failed list response proves nothing, whatever its body says.
	e.respondAs(e.fetchAs(fakeSource, 1, "", nil), fakeSource, "/mandates", 500,
		body(fBody{Lists: []fList{{ledger.MandateDirectDebit, refA}}}))
	if st := e.catchUp(); st.MandatesDisappeared != 0 {
		t.Fatalf("a failed response marked mandates disappeared: %+v", st)
	}

	// An empty list marks every mandate of that kind and account.
	e.mandates(fakeSource, fBody{Lists: []fList{{ledger.MandateDirectDebit, refA}}})
	e.catchUp()
	if e.directDebit(fakeSource, refA, "MANDATE-1").DisappearedAt == nil {
		t.Fatal("an empty direct debit list left the direct debit in place")
	}

	// gym comes back.
	e.mandates(fakeSource, fBody{SOs: []fSO{rent, gym}, Lists: []fList{{ledger.MandateStandingOrder, refA}}})
	if st := e.catchUp(); st.MandatesReturned != 1 {
		t.Fatalf("stats %+v", st)
	}
	if e.standingOrder(fakeSource, refA, "gym").DisappearedAt != nil {
		t.Fatal("a returning standing order is still marked disappeared")
	}
}

func TestMandateGetsANewVersionOnlyWhenABusinessFieldChanges(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.accounts(fAcc{ID: refA, Type: "joint", BankRef: refA})
	dd := fDD{ID: "volatile-1", BankRef: refA, Key: "MANDATE-1", Name: "MADE UP CO", Status: "Active", Previous: "2030-01-05", Amount: -1500}
	e.mandates(fakeSource, fBody{DDs: []fDD{dd}})
	e.catchUp()
	id := wantMandateID(fakeSource, refA, ledger.MandateDirectDebit, "MANDATE-1")

	// The bank's per-call id changes: no new version.
	dd.ID = "volatile-2"
	e.mandates(fakeSource, fBody{DDs: []fDD{dd}})
	if st := e.catchUp(); st.MandatesChanged != 0 || st.MandatesCreated != 0 {
		t.Fatalf("an id-only change made a version: %+v", st)
	}
	if d := e.directDebit(fakeSource, refA, "MANDATE-1"); d.VersionCount != 1 || !d.LastSeenAt.Equal(e.clock) {
		t.Fatalf("after an id-only change: %+v", d)
	}

	// The amount changes: version 2, and the row follows it.
	dd.Amount, dd.Previous = -1600, "2030-02-05"
	e.mandates(fakeSource, fBody{DDs: []fDD{dd}})
	if st := e.catchUp(); st.MandatesChanged != 1 {
		t.Fatalf("stats %+v", st)
	}
	d := e.directDebit(fakeSource, refA, "MANDATE-1")
	versions, err := e.core().ListMandateVersions(ctx, store.MandateDirectDebit, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.VersionCount != 2 || d.PreviousAmountMinor == nil || *d.PreviousAmountMinor != -1600 || len(versions) != 2 ||
		!sameDay(d.PreviousDate, date("2030-02-05")) || d.Name == nil || *d.Name != "MADE UP CO" {
		t.Fatalf("after a change: %+v, %d versions", d, len(versions))
	}
}

// Two standing orders with the same payee, reference and frequency are kept apart: the one with
// the earlier first payment keeps the key, the other gets "#2", whatever order they are listed in.
func TestIdenticalMandateKeysAreNumberedByFirstDateThenAmount(t *testing.T) {
	e := newEnv(t)
	e.accounts(fAcc{ID: refA, Type: "joint", BankRef: refA})
	key := "MADE UP\x1fREF\x1fMONTHLY"
	later := fSO{BankRef: refA, Key: key, Payee: "MADE UP", First: "2030-02-01", FirstAmount: -100}
	earlier := fSO{BankRef: refA, Key: key, Payee: "MADE UP", First: "2030-01-01", FirstAmount: -200}
	sameDaySmaller := fSO{BankRef: refA, Key: key, Payee: "MADE UP", First: "2030-02-01", FirstAmount: -300}
	e.mandates(fakeSource, fBody{SOs: []fSO{later, earlier, sameDaySmaller}, Lists: []fList{{ledger.MandateStandingOrder, refA}}})
	if st := e.catchUp(); st.MandatesCreated != 3 {
		t.Fatalf("stats %+v", st)
	}
	for k, wantFirst := range map[string]int64{key: -200, key + "#2": -300, key + "#3": -100} {
		s := e.standingOrder(fakeSource, refA, k)
		if s.FirstAmountMinor == nil || *s.FirstAmountMinor != wantFirst || s.MandateKey != k {
			t.Errorf("%q: %+v, want first amount %d", k, s, wantFirst)
		}
	}
	// Listed again in another order: the same three, nothing disappears.
	e.mandates(fakeSource, fBody{SOs: []fSO{sameDaySmaller, earlier, later}, Lists: []fList{{ledger.MandateStandingOrder, refA}}})
	if st := e.catchUp(); st.MandatesCreated != 0 || st.MandatesChanged != 0 || st.MandatesDisappeared != 0 {
		t.Fatalf("relisted: %+v", st)
	}
}

func TestMandatesAreTheSameAfterARebuild(t *testing.T) {
	e := newEnv(t)
	e.accounts(fAcc{ID: refA, Type: "joint", BankRef: refA})
	e.mandates(fakeSource, fBody{SOs: []fSO{{BankRef: refA, Key: "rent", Payee: "MADE UP", NextAmount: -500}},
		DDs: []fDD{{BankRef: refA, Key: "MANDATE-1", Name: "MADE UP CO"}, {BankRef: refB, Key: "MANDATE-2", Name: "MADE UP"}}})
	e.catchUp()
	e.mandates(fakeSource, fBody{SOs: []fSO{{BankRef: refA, Key: "rent", Payee: "MADE UP", NextAmount: -600}},
		Lists: []fList{{ledger.MandateStandingOrder, refA}, {ledger.MandateDirectDebit, refA}}})
	e.catchUp()
	if diffs := e.renormalise(true); len(diffs) != 0 {
		t.Fatalf("rebuild differs: %+v", diffs)
	}
}

// A natural key that looks like a numbered one ("X#2") never collides with the number given to a
// second "X": every record is kept apart.
func TestNumberedKeysCannotCollideWithANaturalKey(t *testing.T) {
	e := newEnv(t)
	e.accounts(fAcc{ID: refA, Type: "joint", BankRef: refA})
	e.mandates(fakeSource, fBody{DDs: []fDD{
		{BankRef: refA, Key: "X", Name: "MADE UP", Previous: "2030-01-01"},
		{BankRef: refA, Key: "X", Name: "MADE UP", Previous: "2030-02-01"},
		{BankRef: refA, Key: "X#2", Name: "MADE UP TOO", Previous: "2030-03-01"},
	}, Lists: []fList{{ledger.MandateDirectDebit, refA}}})
	if st := e.catchUp(); st.MandatesCreated != 3 || st.MandatesChanged != 0 {
		t.Fatalf("stats %+v", st)
	}
	if n := e.countAgent("SELECT count(DISTINCT mandate_key) FROM agent.direct_debits"); n != 3 {
		t.Fatalf("%d distinct keys, want 3", n)
	}
}

// countAgent runs a count query as the application role.
func (e *env) countAgent(query string) int {
	e.t.Helper()
	var n int
	if err := e.app.DB().QueryRow(context.Background(), query).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

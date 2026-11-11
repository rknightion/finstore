package ledger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/importer"
	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/store"
)

// capturedAt is long before the test clock: when an old capture's responses were fetched.
var capturedAt = time.Date(2030, time.January, 10, 12, 0, 0, 0, time.UTC)

func statusTxn(id, account string, amount int64, created time.Time, status string) json.RawMessage {
	b, _ := json.Marshal(fTxn{ID: id, Account: account, Amount: amount, Created: created.Format(time.RFC3339), Status: status})
	return b
}

func TestOlderResponseLoadedLaterDoesNotBecomeTheCurrentState(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.accounts(fAcc{ID: "acc_1", Type: "current"})
	e.respond(e.fetch(2, "acc_1", nil), "/transactions", body(fBody{Transactions: []json.RawMessage{
		statusTxn("tx_1", "acc_1", -1200, jan5, "settled")}}))
	e.catchUp()
	liveSeen := e.clock
	// An old capture, loaded after the live data: the same transaction while it was pending.
	e.respondAt(e.fetch(1, "acc_1", nil), "/transactions", body(fBody{Transactions: []json.RawMessage{
		statusTxn("tx_1", "acc_1", -1000, jan5, "pending")}}), capturedAt)
	e.catchUp()

	accID := wantAccountID(fakeSource, "acc_1")
	txID := wantTxnID(accID, fakeSource, "tx_1")
	got := e.txn(txID)
	if got.Status != "settled" || got.AmountMinor != -1200 || got.VersionCount != 2 ||
		!got.FirstSeenAt.Equal(capturedAt) || !got.LastSeenAt.Equal(liveSeen) {
		t.Fatalf("transaction %+v: want settled -1200, 2 versions, first seen at the capture, last seen live", got)
	}
	if r := e.recon(accID); r.RunningSumMinor != -1200 {
		t.Fatalf("running sum %d, want -1200", r.RunningSumMinor)
	}
	versions, err := e.core().ListTransactionVersions(ctx, txID)
	if err != nil || len(versions) != 2 || !versions[1].SeenAt.Equal(capturedAt) || !bytes.Contains(versions[1].Content, []byte("pending")) {
		t.Fatalf("versions %+v, %v", versions, err)
	}
}

func TestOldCompletedFetchLoadedLateDisappearsNothing(t *testing.T) {
	e := newEnv(t)
	jan := &period{date("2030-01-01"), date("2030-02-01")}
	e.accounts(fAcc{ID: "acc_1", Type: "current"})
	e.respond(e.fetch(2, "acc_1", nil), "/transactions", body(fBody{Transactions: []json.RawMessage{txn("tx_1", "acc_1", -100, jan5)}}))
	e.catchUp()
	// An old capture's completed fetch of January, which did not list tx_1, completed before
	// tx_1 was last seen.
	e.completeAt(e.fetch(1, "acc_1", jan), capturedAt)
	if st := e.catchUp(); st.Disappeared != 0 {
		t.Fatalf("stats %+v", st)
	}
	accID := wantAccountID(fakeSource, "acc_1")
	if got := e.txn(wantTxnID(accID, fakeSource, "tx_1")); got.DisappearedAt != nil {
		t.Fatalf("an old fetch disappeared a transaction seen after it: %+v", got)
	}
	if r := e.recon(accID); r.RunningSumMinor != -100 {
		t.Fatalf("running sum %d", r.RunningSumMinor)
	}
	// Control: the same empty fetch completing after the sighting does disappear it.
	e.complete(e.fetch(3, "acc_1", jan))
	if st := e.catchUp(); st.Disappeared != 1 {
		t.Fatalf("control stats %+v", st)
	}
}

func TestNULAndBadUTF8AreSanitisedAndTheRunGoesOn(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.accounts(fAcc{ID: "acc_1", Type: "current", Name: "made\x00up"})
	nested := json.RawMessage(`{"id":"tx_nested","account":"acc_1","amount":-5,"created":"` + jan5.Format(time.RFC3339) +
		`","extra":{"deep":{"note":"x\u0000y","path":"a\\u0000b"}}}`)
	descNUL, _ := json.Marshal(fTxn{ID: "tx_nul", Account: "acc_1", Amount: -7, Created: jan5.Format(time.RFC3339), Description: "a\x00b"})
	e.respond(e.fetch(1, "acc_1", nil), "/transactions", body(fBody{Transactions: []json.RawMessage{
		descNUL, nested, txn("tx_plain", "acc_1", -11, jan5)}}))
	st := e.catchUp()
	if st.Unparseable != 0 || st.TransactionsCreated != 3 || e.lastSeq() != e.maxRawSeq() {
		t.Fatalf("stats %+v", st)
	}
	accID := wantAccountID(fakeSource, "acc_1")
	if a := e.account("fake-current"); a.Name != "madeup" {
		t.Fatalf("account name %q", a.Name)
	}
	if got := e.txn(wantTxnID(accID, fakeSource, "tx_nul")); got.Description != "ab" {
		t.Fatalf("description %q", got.Description)
	}
	vs, err := e.core().ListTransactionVersions(ctx, wantTxnID(accID, fakeSource, "tx_nested"))
	if err != nil || len(vs) != 1 {
		t.Fatalf("versions %+v, %v", vs, err)
	}
	var content struct {
		Extra struct{ Deep struct{ Note, Path string } }
	}
	if err := json.Unmarshal(vs[0].Content, &content); err != nil {
		t.Fatal(err)
	}
	// The NUL is replaced; the escaped backslash followed by "u0000" is text and is kept.
	if content.Extra.Deep.Note != "x�y" || content.Extra.Deep.Path != `a\u0000b` {
		t.Fatalf("content %s", vs[0].Content)
	}
	if r := e.recon(accID); r.RunningSumMinor != -23 {
		t.Fatalf("running sum %d, want the whole page", r.RunningSumMinor)
	}
}

func TestAcceptDifferenceOnAFileAccountIsRefusedAndChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.addFileAccount("bank-current")
	e.importFile("bank-current", "manual-csv", sepCSV)
	e.catchUp()
	accID := wantAccountID(ledger.SourceManual, "bank-current")
	before := e.recon(accID)
	e.operator(ledger.EntryAcceptDifference, ledger.AcceptDifference{Slug: "bank-current", DiffMinor: ptr(int64(-100)), Reason: "made-up"})
	if st := e.catchUp(); st.Unparseable != 1 {
		t.Fatalf("stats %+v", st)
	}
	if !bytes.Contains(e.logs.Bytes(), []byte(`"error_category":"file_account"`)) {
		t.Fatalf("log: %s", e.logs)
	}
	// A later import recomputes continuity and still leaves no accepted difference behind.
	e.importFile("bank-current", "manual-csv", novCSV)
	e.catchUp()
	after := e.recon(accID)
	if after.KnownException != nil || after.AcceptedDiffMinor != nil || before.KnownException != nil {
		t.Fatalf("an accepted difference reached a file account: %+v", after)
	}
	if s := e.coverageState("bank-current"); s != "mismatch" {
		t.Fatalf("coverage state %q", s)
	}
}

func TestRecomputedContinuityKeepsTheAcceptedDifferenceFields(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addFileAccount("bank-current")
	e.catchUp()
	accID := wantAccountID(ledger.SourceManual, "bank-current")
	// Only a store write can put these on a file account now; continuity must not clear them.
	r := e.recon(accID)
	r.KnownException, r.AcceptedDiffMinor = ptr("made-up"), ptr(int64(5))
	if err := e.core().PutReconciliation(ctx, r); err != nil {
		t.Fatal(err)
	}
	e.importFile("bank-current", "manual-csv", sepCSV)
	e.catchUp()
	if got := e.recon(accID); got.KnownException == nil || got.AcceptedDiffMinor == nil || *got.AcceptedDiffMinor != 5 {
		t.Fatalf("continuity cleared the operator's fields: %+v", got)
	}
}

func TestVerifyCatchesLiveCoreUpBeforeComparing(t *testing.T) {
	e := newEnv(t)
	scenario(e)
	// Rows stored without a catch-up, as the approval poll does.
	e.accounts(fAcc{ID: "acc_3", Type: "savings"})
	e.respond(e.fetch(20, "acc_3", nil), "/transactions", body(fBody{Transactions: []json.RawMessage{txn("tx_s", "acc_3", 10, jan5)}}))
	if diffs := e.renormalise(true); len(diffs) != 0 {
		t.Fatalf("verify reported a lagging live core as differences: %+v", diffs)
	}
	if e.lastSeq() != e.maxRawSeq() {
		t.Fatal("verify did not catch live core up")
	}
}

func TestRejectionsNameEachRejectedFileInOrder(t *testing.T) {
	e := newEnv(t)
	e.accounts(fAcc{ID: "acc_1", Type: "current"})
	e.addFileAccount("bank-current")
	e.catchUp()
	good := csvFile(0, csvRow{"2030-01-03", "BGC", 500, "MADE UP"})
	e.importFile("fake-current", "manual-csv", good)
	e.importFile("no-such-account", "manual-csv", good)
	e.importFile("bank-current", "no-such-format", good)
	e.importFile("bank-current", "manual-csv", []byte("date,type,amount,balance_after,description\n2030-01-03,DEB,-5.00,95.00,X\n2030-01-04,DEB,-5.00,91.00,X\n"))
	st := e.catchUp()
	want := []string{"api_account", "unknown_account", "unknown_format", "check_failed"}
	if st.FilesRejected != 4 || !reflect.DeepEqual(st.Rejections, want) {
		t.Fatalf("rejections %v (%d), want %v", st.Rejections, st.FilesRejected, want)
	}
}

func TestCatchUpWithAnotherZoneIsRefusedUntilRenormalise(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.accounts(fAcc{ID: "acc_1", Type: "current"})
	e.catchUp()
	if ps, _ := e.core().GetProjectionState(ctx); ps.Zone == nil || *ps.Zone != "UTC" {
		t.Fatalf("zone not recorded: %+v", ps)
	}
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	e.l = ledger.New(map[string]ledger.SourceNormaliser{fakeSource: fakeNormaliser{}},
		map[string]ledger.FileNormaliser{"manual-csv": importer.Normaliser{}}, london, slog.New(slog.DiscardHandler), nil)
	e.accounts(fAcc{ID: "acc_2", Type: "savings"})
	err = e.app.WithWriterLock(ctx, store.HolderTest, func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.l.CatchUp(ctx, tx, store.SchemaCore)
		return err
	})
	if !errors.Is(err, store.ErrRenormaliseRequired) || !errors.Is(err, ledger.ErrZoneChanged) {
		t.Fatalf("CatchUp with another zone: %v", err)
	}
	if _, err := e.core().GetAccountBySlug(ctx, "fake-savings"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a refused CatchUp wrote")
	}
	e.renormalise(false)
	e.catchUp()
	if ps, _ := e.core().GetProjectionState(ctx); ps.Zone == nil || *ps.Zone != "Europe/London" {
		t.Fatalf("zone after renormalise: %+v", ps)
	}
	e.account("fake-savings")
}

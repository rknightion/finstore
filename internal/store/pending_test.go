package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/store"
)

func pendingRow(id string, amount int64, desc string) store.PendingTransaction {
	return store.PendingTransaction{TransactionID: id, TxnDate: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC), AmountMinor: amount,
		Currency: "GBP", Description: desc, Content: json.RawMessage(`{"transaction_id":"` + id + `"}`)}
}

// ReplacePendingSnapshot replaces an account's pending rows as a whole and records the snapshot: a
// newer one removes rows it does not carry, an empty one clears them and still records when it was
// taken, and another account's rows are never touched.
func TestReplacePendingSnapshot(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	app := openStore(t, c.app)
	t1 := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	err := app.WithWriterLock(ctx, store.HolderTest, func(ctx context.Context, tx pgx.Tx) error {
		core := mustCore(t, tx, store.SchemaCore)
		a, b := testAccount("lloyds-joint", "joint", "api"), testAccount("lloyds-current", "current", "api")
		for _, acc := range []store.Account{a, b} {
			if err := core.UpsertAccount(ctx, acc); err != nil {
				return err
			}
		}
		merchant := "Made Up"
		first := pendingRow("p-1", -750, "ONE")
		first.MerchantName = &merchant
		if err := core.ReplacePendingSnapshot(ctx, a.ID, store.PendingSnapshot{AsOf: t1, RawSeq: 10,
			Rows: []store.PendingTransaction{first, pendingRow("p-2", 200, "TWO")}}); err != nil {
			return err
		}
		if err := core.ReplacePendingSnapshot(ctx, b.ID, store.PendingSnapshot{AsOf: t1, RawSeq: 11,
			Rows: []store.PendingTransaction{pendingRow("q-1", 5, "OTHER ACCOUNT")}}); err != nil {
			return err
		}
		got, err := core.ListPending(ctx, a.ID)
		if err != nil {
			return err
		}
		if len(got) != 2 || got[0].TransactionID != "p-1" || got[0].AmountMinor != -750 || got[0].MerchantName == nil || *got[0].MerchantName != "Made Up" ||
			got[1].MerchantName != nil || got[1].Description != "TWO" {
			t.Fatalf("first snapshot rows %+v", got)
		}

		// A newer snapshot replaces the rows: p-1 settled, p-3 is new.
		t2 := t1.Add(6 * time.Hour)
		if err := core.ReplacePendingSnapshot(ctx, a.ID, store.PendingSnapshot{AsOf: t2, RawSeq: 20,
			Rows: []store.PendingTransaction{pendingRow("p-2", 200, "TWO"), pendingRow("p-3", -1, "THREE")}}); err != nil {
			return err
		}
		got, _ = core.ListPending(ctx, a.ID)
		var ids []string
		for _, r := range got {
			ids = append(ids, r.TransactionID)
		}
		if !slices.Equal(ids, []string{"p-2", "p-3"}) {
			t.Fatalf("second snapshot rows %v, want p-2 and p-3", ids)
		}
		snap, err := core.GetPendingSnapshot(ctx, a.ID)
		if err != nil {
			return err
		}
		if !snap.AsOf.Equal(t2) || snap.RawSeq != 20 || snap.RowCount != 2 {
			t.Fatalf("snapshot %+v, want as of %v from raw 20 with 2 rows", snap, t2)
		}

		// An empty snapshot clears the rows and still records that nothing was pending.
		t3 := t2.Add(time.Hour)
		if err := core.ReplacePendingSnapshot(ctx, a.ID, store.PendingSnapshot{AsOf: t3, RawSeq: 30}); err != nil {
			return err
		}
		if got, _ = core.ListPending(ctx, a.ID); len(got) != 0 {
			t.Fatalf("an empty snapshot left %d rows", len(got))
		}
		if snap, err = core.GetPendingSnapshot(ctx, a.ID); err != nil || !snap.AsOf.Equal(t3) || snap.RowCount != 0 {
			t.Fatalf("empty snapshot record %+v, %v", snap, err)
		}
		if other, _ := core.ListPending(ctx, b.ID); len(other) != 1 {
			t.Fatalf("the other account has %d pending rows, want 1", len(other))
		}
		// An account with no snapshot says so.
		if _, err := core.GetPendingSnapshot(ctx, testAccount("never", "current", "api").ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("snapshot of an account with none: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// UnendedCardKeys lists the source keys of a source's credit_card accounts that have no end.
func TestUnendedCardKeys(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	app := openStore(t, c.app)
	err := app.WithWriterLock(ctx, store.HolderTest, func(ctx context.Context, tx pgx.Tx) error {
		core := mustCore(t, tx, store.SchemaCore)
		mk := func(slug, key, source, typ string, ended bool) error {
			a := testAccount(slug, typ, "api")
			a.Source, a.SourceKey = source, key
			if ended {
				d := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
				a.EndedOn = &d
			}
			return core.UpsertAccount(ctx, a)
		}
		for _, x := range []struct {
			slug, key, source, typ string
			ended                  bool
		}{
			{"lloyds-card-5678", "card:5678", "truelayer:ob-lloyds", "credit_card", false},
			{"lloyds-card-1111", "card:1111", "truelayer:ob-lloyds", "credit_card", true},
			{"lloyds-card-9012", "card:9012", "truelayer:ob-lloyds", "credit_card", false},
			{"lloyds-joint", "112233 12345678", "truelayer:ob-lloyds", "joint", false},
			{"other-card", "card:7777", "other", "credit_card", false},
		} {
			if err := mk(x.slug, x.key, x.source, x.typ, x.ended); err != nil {
				return err
			}
		}
		keys, err := core.UnendedCardKeys(ctx, "truelayer:ob-lloyds")
		if err != nil {
			return err
		}
		if !slices.Equal(keys, []string{"card:5678", "card:9012"}) {
			t.Fatalf("unended card keys %v", keys)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// agent.pending_transactions has the columns the documentation promises, in this order, and
// finstore_agent can read it (and still no core table).
func TestPendingTransactionsViewColumnsAndGrants(t *testing.T) {
	c := newCluster(t)
	q := openStore(t, c.app).DB()
	requireColumns(t, q, "pending_transactions", []string{"account_slug", "source", "as_of", "transaction_id", "txn_date",
		"amount_minor", "currency", "description", "merchant_name", "detail"})
	agent := connAs(t, c.super, store.RoleAgent)
	if _, err := agent.Exec(context.Background(), `SELECT * FROM agent.pending_transactions`); err != nil {
		t.Fatalf("finstore_agent reading the view: %v", err)
	}
	_, err := agent.Exec(context.Background(), `SELECT 1 FROM core.pending_transaction`)
	requireDenied(t, err, "finstore_agent reading core.pending_transaction")
	_, err = agent.Exec(context.Background(), `SELECT 1 FROM core.pending_snapshot`)
	requireDenied(t, err, "finstore_agent reading core.pending_snapshot")
}

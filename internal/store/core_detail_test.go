package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/store"
)

func jsonEqual(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("detail %q is not JSON: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Fatalf("detail = %s, want %s", gb, wb)
	}
}

// core.account.detail is written with the account, and an upsert that carries none keeps what is
// there (the paths that read an account and write it back, such as account end, must not lose it)
// while one that carries a detail replaces it.
func TestUpsertAccountKeepsDetailWhenNoneIsGiven(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	app := openStore(t, c.app)
	err := app.WithWriterLock(ctx, func(ctx context.Context, tx pgx.Tx) error {
		core := mustCore(t, tx, store.SchemaCore)
		acc := testAccount("lloyds-joint", "joint", "api")
		acc.Detail = json.RawMessage(`{"display_name":"Made Up","account_id":"a-1"}`)
		if err := core.UpsertAccount(ctx, acc); err != nil {
			return err
		}
		got, err := core.GetAccount(ctx, acc.ID)
		if err != nil {
			return err
		}
		jsonEqual(t, got.Detail, `{"display_name":"Made Up","account_id":"a-1"}`)

		got.Name = "renamed"
		got.Detail = nil
		if err := core.UpsertAccount(ctx, got); err != nil {
			return err
		}
		again, err := core.GetAccountBySlug(ctx, "lloyds-joint")
		if err != nil {
			return err
		}
		if again.Name != "renamed" {
			t.Errorf("the upsert without a detail did not write the name: %q", again.Name)
		}
		jsonEqual(t, again.Detail, `{"display_name":"Made Up","account_id":"a-1"}`)

		again.Detail = json.RawMessage(`{"display_name":"Newer"}`)
		if err := core.UpsertAccount(ctx, again); err != nil {
			return err
		}
		list, err := core.ListAccounts(ctx)
		if err != nil {
			return err
		}
		if len(list) != 1 {
			t.Fatalf("%d accounts", len(list))
		}
		jsonEqual(t, list[0].Detail, `{"display_name":"Newer"}`)

		// An account that never had a detail has a null one.
		plain := testAccount("file-account", "current", "file")
		if err := core.UpsertAccount(ctx, plain); err != nil {
			return err
		}
		p, err := core.GetAccount(ctx, plain.ID)
		if err != nil {
			return err
		}
		if p.Detail != nil {
			t.Errorf("an account given no detail has %s", p.Detail)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/store"
)

// The writing commands of `finstore txn`. Each validates its entry with the ledger's own function
// before it writes anything, then refuses a transaction id the database does not hold (the refusal
// never repeats the id), and normalises the entry in the same writer transaction.

// TxnClass sets the flow class of one transaction, with the employer flag for income. It prints a
// note, and still writes the entry, when the transaction is classified by a pot, Flex, held account
// or pairing rule, which an override never changes.
func TxnClass(ctx context.Context, e Env, id, class string, employer bool) int {
	return writeTxnClass(ctx, e, "txn class", ledger.TxnClass{TxnID: id, FlowClass: &class, Employer: employer})
}

// TxnClear removes the class override of one transaction.
func TxnClear(ctx context.Context, e Env, id string) int {
	return writeTxnClass(ctx, e, "txn clear", ledger.TxnClass{TxnID: id})
}

func writeTxnClass(ctx context.Context, e Env, command string, p ledger.TxnClass) int {
	return e.withTelemetry(ctx, command, func(ctx context.Context, t cliTelemetry) int {
		if err := ledger.ValidateTxnClass(p); err != nil {
			return e.fail(err)
		}
		id, _ := ledger.ParseTxnID(p.TxnID)
		body, err := json.Marshal(p)
		if err != nil {
			return e.fail(err)
		}
		var out string
		err = withWriter(ctx, e, t, "txn", func(ctx context.Context, tx pgx.Tx, l *ledger.Ledger) error {
			var status string
			err := tx.QueryRow(ctx, `SELECT status FROM agent.transactions WHERE id = $1`, id).Scan(&status)
			held := err == nil
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if p.FlowClass == nil && !held {
				// A clear may name a transaction that has since gone, but only one with an override.
				core, err := store.NewCore(tx, store.SchemaCore)
				if err != nil {
					return err
				}
				if _, err := core.GetTxnClass(ctx, id); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return errors.New("--id is not a transaction finstore holds")
					}
					return err
				}
			} else if !held {
				return errors.New("--id is not a transaction finstore holds")
			} else if status == "declined" && p.FlowClass != nil {
				return errors.New("--id is a declined transaction, which is not classified")
			}
			if _, err := store.InsertOperatorEntry(ctx, tx, store.OperatorEntry{Kind: ledger.EntryTxnClass, Payload: body, EnteredAt: e.now()}); err != nil {
				return err
			}
			if _, err := l.CatchUp(ctx, tx, store.SchemaCore); err != nil {
				return err
			}
			if p.FlowClass == nil {
				out = "transaction: class override cleared"
				return nil
			}
			var class, reason string
			err = tx.QueryRow(ctx, `SELECT flow_class, flow_reason FROM agent.cash_flows WHERE id = $1`, id).Scan(&class, &reason)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			out = fmt.Sprintf("transaction: class override set to %s", *p.FlowClass)
			// Rules 1 to 4 are the only ones that give internal with one of these reasons.
			if class == "internal" && (reason == "pot" || reason == "flex" || reason == "own_account" || reason == "paired") {
				out += fmt.Sprintf("\nnote: the override will not apply: this transaction is classified %s (%s) by a rule an override never changes", class, reason)
			}
			return nil
		})
		if err != nil {
			return e.fail(err)
		}
		_, _ = fmt.Fprintln(e.Stdout, out)
		return ExitOK
	})
}

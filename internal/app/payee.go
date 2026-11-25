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

// The writing commands of `finstore payee`, `finstore category` and `finstore account scope`. Each
// validates its entry with the ledger's own functions before it writes anything, so a refused
// command leaves no raw row; a valid entry is inserted and normalised in one writer transaction.
// The commands take their keys as given: resolving a key to its canonical payee is a later change.

// PayeeMerge writes a merge of the payee key into another.
func PayeeMerge(ctx context.Context, e Env, key, into string) int {
	return writeEntry(ctx, e, "payee merge", "payee", ledger.EntryPayeeRule,
		ledger.PayeeRule{Action: ledger.ActionMerge, PayeeKey: key, IntoKey: into},
		fmt.Sprintf("payee %s merged into %s", key, into))
}

// PayeeSeparate writes a separation: the key is never merged into another payee.
func PayeeSeparate(ctx context.Context, e Env, key string) int {
	return writeEntry(ctx, e, "payee separate", "payee", ledger.EntryPayeeRule,
		ledger.PayeeRule{Action: ledger.ActionSeparate, PayeeKey: key},
		fmt.Sprintf("payee %s kept separate", key))
}

// PayeeRename writes a display name for the payee key.
func PayeeRename(ctx context.Context, e Env, key, name string) int {
	return writeEntry(ctx, e, "payee rename", "payee", ledger.EntryPayeeRule,
		ledger.PayeeRule{Action: ledger.ActionRename, PayeeKey: key, Name: &name},
		fmt.Sprintf("payee %s renamed", key))
}

// PayeeClass writes a flow class for the payee key, with the employer flag for income.
func PayeeClass(ctx context.Context, e Env, key, class string, employer bool) int {
	return writeEntry(ctx, e, "payee class", "payee", ledger.EntryPayeeRule,
		ledger.PayeeRule{Action: ledger.ActionClass, PayeeKey: key, FlowClass: class, Employer: employer},
		fmt.Sprintf("payee %s set to class %s", key, class))
}

// PayeeCategory writes a category for the payee key.
func PayeeCategory(ctx context.Context, e Env, key, category string) int {
	return writeEntry(ctx, e, "payee category", "payee", ledger.EntryPayeeRule,
		ledger.PayeeRule{Action: ledger.ActionCategory, PayeeKey: key, Category: category},
		fmt.Sprintf("payee %s set to category %s", key, category))
}

// PayeeRecurring writes a decision about a recurring series. The command's word for no is
// not-recurring; the entry stores not_recurring.
func PayeeRecurring(ctx context.Context, e Env, series, decision string) int {
	if decision == "not-recurring" {
		decision = ledger.RecurringNotRecurring
	}
	return writeEntry(ctx, e, "payee recurring", "payee", ledger.EntryPayeeRule,
		ledger.PayeeRule{Action: ledger.ActionRecurring, SeriesKey: series, Recurring: decision},
		fmt.Sprintf("series %s decided: %s", series, decision))
}

// PayeeClear removes a setting of a payee key (what is merge, separate, name, class or category),
// or with a non-empty series the decision about that series. Exactly one of key and series is set.
func PayeeClear(ctx context.Context, e Env, key, what, series string) int {
	rule := ledger.PayeeRule{Action: ledger.ActionClear, PayeeKey: key, Clears: what}
	done := fmt.Sprintf("payee %s: %s cleared", key, what)
	if series != "" {
		rule = ledger.PayeeRule{Action: ledger.ActionClear, SeriesKey: series, Clears: ledger.ClearRecurring}
		done = fmt.Sprintf("series %s: decision cleared", series)
	}
	return writeEntry(ctx, e, "payee clear", "payee", ledger.EntryPayeeRule, rule, done)
}

// CategoryName writes a display name for a category.
func CategoryName(ctx context.Context, e Env, category, name string) int {
	return writeEntry(ctx, e, "category name", "category", ledger.EntryCategoryName,
		ledger.CategoryName{Category: category, Name: &name}, fmt.Sprintf("category %s named", category))
}

// CategoryClear removes the display name of a category.
func CategoryClear(ctx context.Context, e Env, category string) int {
	return writeEntry(ctx, e, "category clear", "category", ledger.EntryCategoryName,
		ledger.CategoryName{Category: category}, fmt.Sprintf("category %s: name cleared", category))
}

// AccountScope sets the scope of an account, or with a nil scope clears the setting. It refuses a
// slug no account has and a pot, which takes its parent's scope.
func AccountScope(ctx context.Context, e Env, slug string, scope *string) int {
	done := fmt.Sprintf("account %s: scope cleared", slug)
	if scope != nil {
		done = fmt.Sprintf("account %s: scope set to %s", slug, *scope)
	}
	return writeEntry(ctx, e, "account scope", "account", ledger.EntryAccountScope,
		ledger.AccountScope{Slug: slug, Scope: scope}, done)
}

// validateEntry checks a payload with the ledger's exported validation.
func validateEntry(payload any) error {
	switch p := payload.(type) {
	case ledger.PayeeRule:
		return ledger.ValidatePayeeRule(p)
	case ledger.CategoryName:
		return ledger.ValidateCategoryName(p)
	case ledger.AccountScope:
		return ledger.ValidateAccountScope(p)
	}
	return errors.New("unknown entry payload")
}

// writeEntry validates payload, then inserts it as an operator entry of kind and normalises it in
// one writer transaction, and prints done. A refusal returns before any row is written, and an
// error inside the transaction rolls it back.
func writeEntry(ctx context.Context, e Env, command, requires, kind string, payload any, done string) int {
	return e.withTelemetry(ctx, command, func(ctx context.Context, t cliTelemetry) int {
		if err := validateEntry(payload); err != nil {
			return e.fail(err)
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return e.fail(err)
		}
		err = withWriter(ctx, e, t, requires, func(ctx context.Context, tx pgx.Tx, l *ledger.Ledger) error {
			if scope, ok := payload.(ledger.AccountScope); ok {
				if err := checkScopeAccount(ctx, tx, scope.Slug); err != nil {
					return err
				}
			}
			if _, err := store.InsertOperatorEntry(ctx, tx, store.OperatorEntry{Kind: kind, Payload: body, EnteredAt: e.now()}); err != nil {
				return err
			}
			_, err := l.CatchUp(ctx, tx, store.SchemaCore)
			return err
		})
		if err != nil {
			return e.fail(err)
		}
		_, _ = fmt.Fprintln(e.Stdout, done)
		return ExitOK
	})
}

// checkScopeAccount refuses a slug no account has and an account that takes no scope.
func checkScopeAccount(ctx context.Context, tx pgx.Tx, slug string) error {
	core, err := store.NewCore(tx, store.SchemaCore)
	if err != nil {
		return err
	}
	acc, err := core.GetAccountBySlug(ctx, slug)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no account has the slug %q", slug)
	}
	if err != nil {
		return err
	}
	return ledger.ValidateScopeTarget(acc)
}

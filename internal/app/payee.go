package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/store"
)

// The writing commands of `finstore payee`, `finstore category` and `finstore account scope`. Each
// validates its entry with the ledger's own functions before it writes anything, so a refused
// command leaves no raw row; a valid entry is inserted and normalised in one writer transaction.
// The payee commands that name a key read the views first (see writePayeeRule); the others take their
// arguments as given.

// PayeeMerge writes a merge of the payee key into another. It refuses a target that already
// resolves to the key's payee and one whose own merge chain returns to the key.
func PayeeMerge(ctx context.Context, e Env, key, into string) int {
	return writePayeeRule(ctx, e, "payee merge", ledger.PayeeRule{Action: ledger.ActionMerge, PayeeKey: key, IntoKey: into}, false)
}

// PayeeSeparate writes a separation: the key is never merged into another payee.
func PayeeSeparate(ctx context.Context, e Env, key string) int {
	return writePayeeRule(ctx, e, "payee separate", ledger.PayeeRule{Action: ledger.ActionSeparate, PayeeKey: key}, false)
}

// PayeeRename writes a display name for the payee the key belongs to.
func PayeeRename(ctx context.Context, e Env, key, name string) int {
	return writePayeeRule(ctx, e, "payee rename", ledger.PayeeRule{Action: ledger.ActionRename, PayeeKey: key, Name: &name}, true)
}

// PayeeClass writes a flow class for the payee the key belongs to, with the employer flag for
// income.
func PayeeClass(ctx context.Context, e Env, key, class string, employer bool) int {
	return writePayeeRule(ctx, e, "payee class",
		ledger.PayeeRule{Action: ledger.ActionClass, PayeeKey: key, FlowClass: class, Employer: employer}, true)
}

// PayeeCategory writes a category for the payee the key belongs to.
func PayeeCategory(ctx context.Context, e Env, key, category string) int {
	return writePayeeRule(ctx, e, "payee category",
		ledger.PayeeRule{Action: ledger.ActionCategory, PayeeKey: key, Category: category}, true)
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
	if series != "" {
		rule := ledger.PayeeRule{Action: ledger.ActionClear, SeriesKey: series, Clears: ledger.ClearRecurring}
		return writeEntry(ctx, e, "payee clear", "payee", ledger.EntryPayeeRule, rule, fmt.Sprintf("series %s: decision cleared", series))
	}
	return writePayeeRule(ctx, e, "payee clear", ledger.PayeeRule{Action: ledger.ActionClear, PayeeKey: key, Clears: what}, false)
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

// writePayeeRule writes a payee rule on a key and prints the payee's new effective state as one
// line. With canonical set the rule is written against the canonical key of the key given (when
// any transaction gives the key a payee), as the settings commands do. When the key written is an
// n: key that has transactions, name_sample is filled from its most frequent raw name. A merge is
// refused when the target already resolves to the key's payee or its merge chain returns to the
// key; the refusal names the flag and never the key, which can carry a name.
func writePayeeRule(ctx context.Context, e Env, command string, rule ledger.PayeeRule, canonical bool) int {
	return e.withTelemetry(ctx, command, func(ctx context.Context, t cliTelemetry) int {
		if err := ledger.ValidatePayeeRule(rule); err != nil {
			return e.fail(err)
		}
		var state store.PayeeState
		err := withWriter(ctx, e, t, "payee", func(ctx context.Context, tx pgx.Tx, l *ledger.Ledger) error {
			if canonical {
				id, _, ok, err := store.PayeeOf(ctx, tx, rule.PayeeKey)
				if err != nil {
					return err
				}
				if ok {
					rule.PayeeKey = id
				}
			}
			if rule.Action == ledger.ActionMerge {
				if err := checkMerge(ctx, tx, rule.PayeeKey, rule.IntoKey); err != nil {
					return err
				}
			}
			if strings.HasPrefix(rule.PayeeKey, "n:") && rule.Action != ledger.ActionClear {
				sample, err := store.MostFrequentName(ctx, tx, rule.PayeeKey)
				if err != nil {
					return err
				}
				if sample != "" {
					rule.NameSample = &sample
				}
			}
			if err := ledger.ValidatePayeeRule(rule); err != nil {
				return err
			}
			body, err := json.Marshal(rule)
			if err != nil {
				return err
			}
			if _, err := store.InsertOperatorEntry(ctx, tx, store.OperatorEntry{Kind: ledger.EntryPayeeRule, Payload: body, EnteredAt: e.now()}); err != nil {
				return err
			}
			if _, err := l.CatchUp(ctx, tx, store.SchemaCore); err != nil {
				return err
			}
			state, err = store.EffectivePayeeState(ctx, tx, rule.PayeeKey)
			return err
		})
		if err != nil {
			return e.fail(err)
		}
		printPayeeState(e, state)
		return ExitOK
	})
}

// checkMerge refuses a merge of key into target when the target already resolves to the same
// payee as the key, or when following the target's merge chain returns to the key.
func checkMerge(ctx context.Context, q store.DBTX, key, target string) error {
	keyID, _, keyOK, err := store.PayeeOf(ctx, q, key)
	if err != nil {
		return err
	}
	targetID, _, targetOK, err := store.PayeeOf(ctx, q, target)
	if err != nil {
		return err
	}
	if keyOK && targetOK && keyID == targetID {
		return errors.New("--into already resolves to the same payee as --key")
	}
	returns, err := store.MergeChainReturns(ctx, q, key, target)
	if err != nil {
		return err
	}
	if returns {
		return errors.New("--into has a merge chain that returns to --key")
	}
	return nil
}

// printPayeeState prints the effective state of a payee on one line.
func printPayeeState(e Env, s store.PayeeState) {
	class, category := s.FlowClass, s.Category
	if class == "" {
		class = "-"
	}
	if category == "" {
		category = "-"
	}
	_, _ = fmt.Fprintf(e.Stdout, "%s %q class=%s category=%s keys=%d\n", s.PayeeID, s.Name, class, category, s.Keys)
}

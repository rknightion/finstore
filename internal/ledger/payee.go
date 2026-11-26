package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rknightion/finstore/internal/store"
)

// Operator entry kinds for the payee, category and account scope settings.
const (
	// EntryPayeeRule merges, separates, renames, classes or categorises a payee, decides a
	// recurring series, or clears one of those. Payload: PayeeRule.
	EntryPayeeRule = "payee_rule"
	// EntryCategoryName names a category, or clears its name. Payload: CategoryName.
	EntryCategoryName = "category_name"
	// EntryAccountScope sets an account's scope, or clears it. Payload: AccountScope.
	EntryAccountScope = "account_scope"
)

// The actions of a payee_rule entry.
const (
	ActionMerge     = "merge"
	ActionSeparate  = "separate"
	ActionRename    = "rename"
	ActionClass     = "class"
	ActionCategory  = "category"
	ActionRecurring = "recurring"
	ActionClear     = "clear"
)

// What a clear removes. The first five name a setting of a payee key; recurring names a series
// decision.
const (
	ClearMerge     = "merge"
	ClearSeparate  = "separate"
	ClearName      = "name"
	ClearClass     = "class"
	ClearCategory  = "category"
	ClearRecurring = "recurring"
)

// The decisions of a recurring action, as stored.
const (
	RecurringConfirm      = "confirm"
	RecurringIgnore       = "ignore"
	RecurringNotRecurring = "not_recurring"
)

// FlowClassIncome is the flow class that takes the employer flag.
const FlowClassIncome = "income"

// FlowClasses are the flow classes an operator may set on a payee. debt_repayment on a lender
// also makes its positive rows borrowing; borrowing sets only those.
var FlowClasses = []string{"internal", "scope_transfer", "own_external", "investment",
	"debt_repayment", "borrowing", FlowClassIncome, "refund", "spending"}

// AccountScopes are the scopes an operator may set on an account.
var AccountScopes = []string{"personal", "joint", "business"}

var (
	payeeKeyPattern  = regexp.MustCompile(`^(mg|mu|n|x):.+$`)
	seriesKeyPattern = regexp.MustCompile(`^(dd|so|sub|inf):.+$`)
)

// PayeeRule is the payload of a payee_rule operator entry. Only the fields its action uses are
// set. A later entry replaces the setting an earlier one made.
type PayeeRule struct {
	Action string `json:"action"`
	// PayeeKey is the payee key every action but recurring (and a clear of recurring) names.
	PayeeKey string `json:"payee_key,omitempty"`
	// NameSample is the raw name seen when the key is n:, kept on the row; null leaves it as it was.
	NameSample *string `json:"name_sample,omitempty"`
	// IntoKey is the merge target.
	IntoKey string `json:"into_key,omitempty"`
	// Name is the new display name of a rename.
	Name *string `json:"name,omitempty"`
	// FlowClass and Employer belong to a class action; Employer only with income.
	FlowClass string `json:"flow_class,omitempty"`
	Employer  bool   `json:"employer,omitempty"`
	// Category belongs to a category action.
	Category string `json:"category,omitempty"`
	// SeriesKey and Recurring belong to a recurring action; SeriesKey also to a clear of recurring.
	SeriesKey string `json:"series_key,omitempty"`
	Recurring string `json:"recurring,omitempty"`
	// Clears says what a clear removes.
	Clears string `json:"clears,omitempty"`
}

// CategoryName is the payload of a category_name operator entry. A null Name clears the name.
type CategoryName struct {
	Category string  `json:"category"`
	Name     *string `json:"name"`
}

// AccountScope is the payload of an account_scope operator entry. A null Scope clears the setting.
type AccountScope struct {
	Slug  string  `json:"slug"`
	Scope *string `json:"scope"`
}

// ValidatePayeeKey reports whether key matches the payee key grammar ^(mg|mu|n|x):.+$. The payee
// need not exist yet.
func ValidatePayeeKey(key string) error {
	if !payeeKeyPattern.MatchString(key) {
		return errors.New("not a payee key: it must be mg:, mu:, n: or x: followed by an identifier")
	}
	return nil
}

// ValidateSeriesKey reports whether key matches the series key grammar ^(dd|so|sub|inf):.+$.
func ValidateSeriesKey(key string) error {
	if !seriesKeyPattern.MatchString(key) {
		return errors.New("not a series key: it must be dd:, so:, sub: or inf: followed by an identifier")
	}
	return nil
}

// ValidatePayeeRule checks a payee_rule payload by the rules of the design: the live run and a
// rebuild apply the same ones, and the CLI refuses an entry that fails them before writing it. The
// error says why, in words that carry no data.
func ValidatePayeeRule(p PayeeRule) error {
	needKey := func() error {
		if err := ValidatePayeeKey(p.PayeeKey); err != nil {
			return fmt.Errorf("payee_key: %w", err)
		}
		return nil
	}
	switch p.Action {
	case ActionMerge:
		if err := needKey(); err != nil {
			return err
		}
		if p.IntoKey == "" {
			return errors.New("into_key is required for a merge")
		}
		if err := ValidatePayeeKey(p.IntoKey); err != nil {
			return fmt.Errorf("into_key: %w", err)
		}
		if p.IntoKey == p.PayeeKey {
			return errors.New("a payee cannot be merged into itself")
		}
	case ActionSeparate:
		return needKey()
	case ActionRename:
		if err := needKey(); err != nil {
			return err
		}
		if p.Name == nil || strings.TrimSpace(*p.Name) == "" {
			return errors.New("name is required for a rename")
		}
	case ActionClass:
		if err := needKey(); err != nil {
			return err
		}
		if !slices.Contains(FlowClasses, p.FlowClass) {
			return fmt.Errorf("flow_class must be one of %s", strings.Join(FlowClasses, ", "))
		}
		if p.Employer && p.FlowClass != FlowClassIncome {
			return errors.New("employer is accepted only with flow_class income")
		}
	case ActionCategory:
		if err := needKey(); err != nil {
			return err
		}
		if strings.TrimSpace(p.Category) == "" {
			return errors.New("category is required for a category")
		}
	case ActionRecurring:
		if err := ValidateSeriesKey(p.SeriesKey); err != nil {
			return fmt.Errorf("series_key: %w", err)
		}
		if !slices.Contains([]string{RecurringConfirm, RecurringIgnore, RecurringNotRecurring}, p.Recurring) {
			return errors.New("recurring must be confirm, ignore or not_recurring")
		}
	case ActionClear:
		switch p.Clears {
		case ClearMerge, ClearSeparate, ClearName, ClearClass, ClearCategory:
			return needKey()
		case ClearRecurring:
			if err := ValidateSeriesKey(p.SeriesKey); err != nil {
				return fmt.Errorf("series_key: %w", err)
			}
		default:
			return errors.New("clears must be merge, separate, name, class, category or recurring")
		}
	default:
		return errors.New("action must be merge, separate, rename, class, category, recurring or clear")
	}
	return nil
}

// ValidateCategoryName checks a category_name payload.
func ValidateCategoryName(p CategoryName) error {
	if strings.TrimSpace(p.Category) == "" {
		return errors.New("category is required")
	}
	if p.Name != nil && strings.TrimSpace(*p.Name) == "" {
		return errors.New("name must not be blank: omit it to clear the name")
	}
	return nil
}

// ValidateAccountScope checks the shape of an account_scope payload. Whether the account exists
// and takes a scope is ValidateScopeTarget's question, which needs the account.
func ValidateAccountScope(p AccountScope) error {
	if p.Slug == "" {
		return errors.New("slug is required")
	}
	if p.Scope != nil && !slices.Contains(AccountScopes, *p.Scope) {
		return fmt.Errorf("scope must be one of %s", strings.Join(AccountScopes, ", "))
	}
	return nil
}

// ValidateScopeTarget refuses an account that takes no scope of its own: a pot takes its parent's.
func ValidateScopeTarget(acc store.Account) error {
	if acc.Type == "pot" {
		return errors.New("a pot takes the scope of its parent account: set the parent's scope")
	}
	return nil
}

// payeeRule applies a payee_rule entry. seq and at are the raw entry's, never the clock's, so a
// rebuild writes the same rows.
func (a *applier) payeeRule(ctx context.Context, payload []byte, seq int64, at time.Time) error {
	var p PayeeRule
	if err := json.Unmarshal(payload, &p); err != nil || ValidatePayeeRule(p) != nil {
		return unparseable("bad_payload")
	}
	switch p.Action {
	case ActionMerge, ActionSeparate:
		return a.aliasRule(ctx, p, seq, at)
	case ActionRename, ActionClass, ActionCategory:
		return a.settingRule(ctx, p, seq, at)
	case ActionRecurring:
		return a.core.PutSeriesSetting(ctx, store.SeriesSetting{SeriesKey: p.SeriesKey, Recurring: p.Recurring,
			EntrySeq: seq, EnteredAt: at})
	}
	return a.clearRule(ctx, p, seq, at)
}

// aliasRule writes the merge or separate setting of a key, keeping the other one.
func (a *applier) aliasRule(ctx context.Context, p PayeeRule, seq int64, at time.Time) error {
	row, err := a.core.GetPayeeAlias(ctx, p.PayeeKey)
	if errors.Is(err, store.ErrNotFound) {
		row, err = store.PayeeAlias{PayeeKey: p.PayeeKey}, nil
	}
	if err != nil {
		return err
	}
	if p.Action == ActionMerge {
		into := p.IntoKey
		row.IntoKey = &into
	} else {
		row.Separate = true
	}
	if p.NameSample != nil {
		row.NameSample = p.NameSample
	}
	row.EntrySeq, row.EnteredAt = seq, at
	return a.core.PutPayeeAlias(ctx, row)
}

// settingRule writes a name, class or category on a key, keeping the other settings of the row.
func (a *applier) settingRule(ctx context.Context, p PayeeRule, seq int64, at time.Time) error {
	row, err := a.core.GetPayeeSetting(ctx, p.PayeeKey)
	if errors.Is(err, store.ErrNotFound) {
		row, err = store.PayeeSetting{PayeeKey: p.PayeeKey}, nil
	}
	if err != nil {
		return err
	}
	switch p.Action {
	case ActionRename:
		name := *p.Name
		row.Name = &name
	case ActionClass:
		class := p.FlowClass
		row.FlowClass, row.Employer = &class, nil
		if class == FlowClassIncome {
			employer := p.Employer
			row.Employer = &employer
		}
	case ActionCategory:
		category := p.Category
		row.Category = &category
	}
	if p.NameSample != nil {
		row.NameSample = p.NameSample
	}
	row.EntrySeq, row.EnteredAt = seq, at
	return a.core.PutPayeeSetting(ctx, row)
}

// clearRule removes what a clear names. Clearing what is not set changes nothing.
func (a *applier) clearRule(ctx context.Context, p PayeeRule, seq int64, at time.Time) error {
	switch p.Clears {
	case ClearRecurring:
		return a.core.DeleteSeriesSetting(ctx, p.SeriesKey)
	case ClearMerge, ClearSeparate:
		row, err := a.core.GetPayeeAlias(ctx, p.PayeeKey)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if p.Clears == ClearMerge {
			if row.IntoKey == nil {
				return nil
			}
			row.IntoKey = nil
		} else {
			if !row.Separate {
				return nil
			}
			row.Separate = false
		}
		if !row.Separate && row.IntoKey == nil {
			return a.core.DeletePayeeAlias(ctx, p.PayeeKey)
		}
		row.EntrySeq, row.EnteredAt = seq, at
		return a.core.PutPayeeAlias(ctx, row)
	}
	row, err := a.core.GetPayeeSetting(ctx, p.PayeeKey)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	switch p.Clears {
	case ClearName:
		if row.Name == nil {
			return nil
		}
		row.Name = nil
	case ClearClass:
		if row.FlowClass == nil && row.Employer == nil {
			return nil
		}
		row.FlowClass, row.Employer = nil, nil
	case ClearCategory:
		if row.Category == nil {
			return nil
		}
		row.Category = nil
	}
	if row.Name == nil && row.FlowClass == nil && row.Employer == nil && row.Category == nil {
		return a.core.DeletePayeeSetting(ctx, p.PayeeKey)
	}
	row.EntrySeq, row.EnteredAt = seq, at
	return a.core.PutPayeeSetting(ctx, row)
}

// categoryName sets or clears the name of a category.
func (a *applier) categoryName(ctx context.Context, payload []byte, seq int64, at time.Time) error {
	var p CategoryName
	if err := json.Unmarshal(payload, &p); err != nil || ValidateCategoryName(p) != nil {
		return unparseable("bad_payload")
	}
	if p.Name == nil {
		return a.core.DeleteCategoryName(ctx, p.Category)
	}
	return a.core.PutCategoryName(ctx, store.CategoryName{Category: p.Category, Name: *p.Name, EntrySeq: seq, EnteredAt: at})
}

// accountScope sets or clears core.account.scope through the read and upsert path account_end uses.
func (a *applier) accountScope(ctx context.Context, payload []byte) error {
	var p AccountScope
	if err := json.Unmarshal(payload, &p); err != nil || ValidateAccountScope(p) != nil {
		return unparseable("bad_payload")
	}
	acc, err := a.core.GetAccountBySlug(ctx, p.Slug)
	if errors.Is(err, store.ErrNotFound) {
		return unparseable("unknown_account")
	}
	if err != nil {
		return err
	}
	if ValidateScopeTarget(acc) != nil {
		return unparseable("pot_account")
	}
	acc.Scope = p.Scope
	return a.core.UpsertAccount(ctx, acc)
}

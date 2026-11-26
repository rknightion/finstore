package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/rknightion/finstore/internal/store"
)

// Operator entry kinds.
const (
	// EntryAccountAdd creates a file-fed account. Payload: AccountAdd.
	EntryAccountAdd = "account_add"
	// EntryAcceptDifference accepts a reconciliation difference. Payload: AcceptDifference.
	EntryAcceptDifference = "accept_difference"
	// EntryAccountEnd ends an account at a date, or clears its end. Payload: AccountEnd.
	EntryAccountEnd = "account_end"
	// EntryAcceptGap accepts a gap between two statements of a file-fed account. Payload: AcceptGap.
	EntryAcceptGap = "accept_gap"
)

// AcceptGap is the payload of an accept_gap operator entry: the file gap of the account from
// AfterDate (the last date of the earlier statement) to BeforeDate (the first date of the later
// one), both YYYY-MM-DD, is accepted with Reason. It applies only while such a gap exists with a
// difference of 0: the statements either side of it join, and the bank issued none in between.
// A later entry for the same gap replaces the reason.
type AcceptGap struct {
	Slug       string `json:"slug"`
	AfterDate  string `json:"after_date"`
	BeforeDate string `json:"before_date"`
	Reason     string `json:"reason"`
}

// AccountEnd is the payload of an account_end operator entry: the account has no transactions
// after LastDate (YYYY-MM-DD). A null LastDate clears the end. A later entry for the same account
// replaces an earlier one.
type AccountEnd struct {
	Slug     string  `json:"slug"`
	LastDate *string `json:"last_date"`
}

// AccountAdd is the payload of an account_add operator entry.
type AccountAdd struct {
	Slug           string `json:"slug"`
	Type           string `json:"type"`
	Currency       string `json:"currency"`
	Name           string `json:"name"`
	ExpectsImports bool   `json:"expects_imports"`
	// Provider is required for type platform (fidelity, aviva or freetrade) and refused for any
	// other type.
	Provider string `json:"provider,omitempty"`
}

// AcceptDifference is the payload of an accept_difference operator entry: the account's checks
// pass while their difference (balance minus running sum) equals DiffMinor.
type AcceptDifference struct {
	Slug      string `json:"slug"`
	DiffMinor *int64 `json:"diff_minor"`
	Reason    string `json:"reason"`
}

func (a *applier) operatorEntry(ctx context.Context, e *store.OperatorEntry) error {
	switch e.Kind {
	case EntryAccountAdd:
		return a.accountAdd(ctx, e.Payload)
	case EntryAcceptDifference:
		return a.acceptDifference(ctx, e.Payload)
	case EntryAccountEnd:
		return a.accountEnd(ctx, e.Payload, e.EnteredAt)
	case EntryAcceptGap:
		return a.acceptGap(ctx, e.Payload, e.EnteredAt)
	case EntryPayeeRule:
		return a.payeeRule(ctx, e.Payload, e.Seq, e.EnteredAt)
	case EntryCategoryName:
		return a.categoryName(ctx, e.Payload, e.Seq, e.EnteredAt)
	case EntryTxnClass:
		return a.txnClass(ctx, e.Payload, e.Seq, e.EnteredAt)
	case EntryAccountScope:
		return a.accountScope(ctx, e.Payload)
	}
	return unparseable("unknown_entry")
}

// accountAdd creates a file-fed account with source "manual" and the slug as its source key, or for
// type platform one with source investment:<provider>, which has no reconciliation row: its
// children are checked, each on its own. An entry whose slug is already taken changes nothing, and a
// platform whose id another account of its provider already has (a child whose account number
// equals the slug) is refused.
func (a *applier) accountAdd(ctx context.Context, payload []byte) error {
	var p AccountAdd
	if err := json.Unmarshal(payload, &p); err != nil || p.Slug == "" || p.Type == "" || p.Currency == "" {
		return unparseable("bad_payload")
	}
	_, err := a.core.GetAccountBySlug(ctx, p.Slug)
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	name := p.Name
	if name == "" {
		name = p.Slug
	}
	source := SourceManual
	if p.Type == typePlatform {
		if _, ok := investmentProviders[p.Provider]; !ok {
			return unparseable("bad_payload")
		}
		source = InvestmentSource(p.Provider)
		if _, err := a.core.GetAccount(ctx, AccountID(source, p.Slug)); err == nil {
			return unparseable("bad_payload")
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
	} else if p.Provider != "" {
		return unparseable("bad_payload")
	}
	id := AccountID(source, p.Slug)
	if err := a.core.UpsertAccount(ctx, store.Account{ID: id, Source: source, SourceKey: p.Slug, Slug: p.Slug,
		Type: p.Type, Feed: feedFile, Currency: p.Currency, Name: name, ExpectsImports: p.ExpectsImports}); err != nil {
		return err
	}
	if p.Type != typePlatform {
		if err := a.core.EnsureReconciliation(ctx, id, methodFileContinuity); err != nil {
			return err
		}
	}
	a.stats.AccountsCreated++
	return nil
}

// acceptDifference records the accepted difference and its reason on the account's
// reconciliation row and re-evaluates the last check, if there has been one.
func (a *applier) acceptDifference(ctx context.Context, payload []byte) error {
	var p AcceptDifference
	if err := json.Unmarshal(payload, &p); err != nil || p.Slug == "" || p.DiffMinor == nil || p.Reason == "" {
		return unparseable("bad_payload")
	}
	acc, err := a.core.GetAccountBySlug(ctx, p.Slug)
	if errors.Is(err, store.ErrNotFound) {
		return unparseable("unknown_account")
	}
	if err != nil {
		return err
	}
	// Accepted differences apply to API-fed accounts only: a file account's state follows its
	// statement gaps, which an accepted difference does not close.
	if acc.Feed != feedAPI {
		return unparseable("file_account")
	}
	rec, err := a.core.GetReconciliation(ctx, acc.ID)
	if err != nil {
		return err
	}
	reason, diff := p.Reason, *p.DiffMinor
	rec.KnownException = &reason
	rec.AcceptedDiffMinor = &diff
	if rec.LastDiffMinor != nil {
		evaluate(&rec)
	}
	if err := a.core.PutReconciliation(ctx, rec); err != nil {
		return err
	}
	// The reason replaced known_exception; a shared bank reference that still holds is reported
	// again in its place.
	return a.checkSharedBankRefs(ctx)
}

// acceptGap recomputes the continuity of a file-fed account, which reads every accept_gap entry
// for it up to this one (see acceptGaps). The entry changes nothing unless the gap it names exists
// with a difference of 0.
func (a *applier) acceptGap(ctx context.Context, payload []byte, at time.Time) error {
	p, ok := parseAcceptGap(payload)
	if !ok {
		return unparseable("bad_payload")
	}
	acc, err := a.core.GetAccountBySlug(ctx, p.Slug)
	if errors.Is(err, store.ErrNotFound) {
		return unparseable("unknown_account")
	}
	if err != nil {
		return err
	}
	if acc.Feed != feedFile {
		return unparseable("api_account")
	}
	if strings.HasPrefix(acc.Source, investmentSourcePrefix) {
		return unparseable("investment_account")
	}
	return a.recomputeContinuity(ctx, acc, at)
}

// parseAcceptGap reads an accept_gap payload; ok is false when a field is missing or a date is
// not YYYY-MM-DD.
func parseAcceptGap(payload []byte) (p AcceptGap, ok bool) {
	if json.Unmarshal(payload, &p) != nil || p.Slug == "" || p.Reason == "" {
		return p, false
	}
	for _, d := range []string{p.AfterDate, p.BeforeDate} {
		if _, err := time.Parse(time.DateOnly, d); err != nil {
			return p, false
		}
	}
	return p, true
}

// accountEnd sets or clears the account's ended_on. A later entry for the same account replaces
// an earlier one. A file-fed account's continuity is recomputed (statements after the end no
// longer count), an investment child's method runs again on the data up to the end, a platform is
// refused, and shared bank references are checked again, since an ended account shares its
// reference with no one.
func (a *applier) accountEnd(ctx context.Context, payload []byte, at time.Time) error {
	var p AccountEnd
	if err := json.Unmarshal(payload, &p); err != nil || p.Slug == "" {
		return unparseable("bad_payload")
	}
	var ended *time.Time
	if p.LastDate != nil {
		d, err := time.Parse(time.DateOnly, *p.LastDate)
		if err != nil {
			return unparseable("bad_payload")
		}
		ended = &d
	}
	acc, err := a.core.GetAccountBySlug(ctx, p.Slug)
	if errors.Is(err, store.ErrNotFound) {
		return unparseable("unknown_account")
	}
	if err != nil {
		return err
	}
	// A platform has no verdict of its own: each child is ended on its own.
	if acc.Type == typePlatform {
		return unparseable("platform_account")
	}
	acc.EndedOn = ended
	if err := a.core.UpsertAccount(ctx, acc); err != nil {
		return err
	}
	switch {
	case strings.HasPrefix(acc.Source, investmentSourcePrefix):
		if err := a.runInvestmentMethod(ctx, acc, at); err != nil {
			return err
		}
	case acc.Feed == feedFile:
		if err := a.recomputeContinuity(ctx, acc, at); err != nil {
			return err
		}
	}
	return a.checkSharedBankRefs(ctx)
}

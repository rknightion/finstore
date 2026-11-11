package ledger

import (
	"context"
	"encoding/json"
	"errors"
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
)

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
	}
	return unparseable("unknown_entry")
}

// accountAdd creates a file-fed account with source "manual" and the slug as its source key. An
// entry whose slug is already taken changes nothing.
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
	id := AccountID(SourceManual, p.Slug)
	if err := a.core.UpsertAccount(ctx, store.Account{ID: id, Source: SourceManual, SourceKey: p.Slug, Slug: p.Slug,
		Type: p.Type, Feed: feedFile, Currency: p.Currency, Name: name, ExpectsImports: p.ExpectsImports}); err != nil {
		return err
	}
	if err := a.core.EnsureReconciliation(ctx, id, methodFileContinuity); err != nil {
		return err
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

// accountEnd sets or clears the account's ended_on. A later entry for the same account replaces
// an earlier one. A file-fed account's continuity is recomputed (statements after the end no
// longer count), and shared bank references are checked again, since an ended account shares its
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
	acc.EndedOn = ended
	if err := a.core.UpsertAccount(ctx, acc); err != nil {
		return err
	}
	if acc.Feed == feedFile {
		if err := a.recomputeContinuity(ctx, acc, at); err != nil {
			return err
		}
	}
	return a.checkSharedBankRefs(ctx)
}

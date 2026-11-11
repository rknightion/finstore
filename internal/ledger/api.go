package ledger

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/rknightion/finstore/internal/store"
)

// Reconciliation methods.
const (
	methodMonzoAbsolute  = "monzo_absolute"
	methodFileContinuity = "file_continuity"
	methodBalancesOnly   = "balances_only"
)

// Feeds.
const (
	feedAPI  = "api"
	feedFile = "file"
)

const typePot = "pot"

// normalise runs a source normaliser over one stored response.
func (l *Ledger) normalise(n SourceNormaliser, r store.APIResponse) (Result, error) {
	params := map[string]any{}
	if len(r.Params) > 0 {
		if err := json.Unmarshal(r.Params, &params); err != nil {
			return Result{}, err
		}
	}
	res, err := n.NormaliseResponse(Response{Seq: r.Seq, Endpoint: r.Endpoint, Params: params, Status: r.Status,
		Body: r.Body, FetchedAt: r.FetchedAt}, l.loc)
	if err != nil {
		return Result{}, err
	}
	return sanitiseResult(res), nil
}

func (a *applier) apiResponse(ctx context.Context, r *store.APIResponse) error {
	fetch, err := store.GetFetch(ctx, a.q, r.FetchID)
	if err != nil {
		return err
	}
	n, ok := a.l.sources[r.Source]
	if !ok {
		return unparseable("no_normaliser")
	}
	res, err := a.l.normalise(n, *r)
	if err != nil {
		return unparseable("normaliser_error")
	}
	if err := a.applyAccounts(ctx, r.Source, res.Accounts); err != nil {
		return err
	}
	for _, t := range res.Txns {
		acc, err := a.knownAccount(ctx, AccountID(r.Source, t.AccountSourceKey))
		if err != nil {
			return err
		}
		if err := a.applyTxn(ctx, acc, r.Source, t, r.FetchedAt); err != nil {
			return err
		}
	}
	for i, b := range res.Balances {
		acc, err := a.knownAccount(ctx, AccountID(r.Source, b.AccountSourceKey))
		if err != nil {
			return err
		}
		if err := a.insertBalance(ctx, acc, i, b); err != nil {
			return err
		}
		if err := a.absoluteCheck(ctx, n, fetch, r, acc, b); err != nil {
			return err
		}
	}
	return nil
}

// knownAccount returns the account with id, or skips the row as unparseable when there is none.
func (a *applier) knownAccount(ctx context.Context, id uuid.UUID) (store.Account, error) {
	acc, err := a.core.GetAccount(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Account{}, unparseable("unknown_account")
	}
	return acc, err
}

// applyAccounts creates or updates the accounts a response lists. New accounts are created in
// order of opening date (an unknown date last), then source key, so that the slug suffixes of a
// repeated type follow opening order. A slug is chosen once, when the account is created, from the
// slugs already taken, and never changes.
func (a *applier) applyAccounts(ctx context.Context, source string, recs []AccountRec) error {
	for _, rec := range recs {
		if rec.SourceKey == "" || rec.Type == "" {
			return unparseable("bad_account")
		}
	}
	recs = slices.Clone(recs)
	slices.SortStableFunc(recs, func(x, y AccountRec) int {
		if c := cmp.Compare(boolInt(x.OpenedOn.IsZero()), boolInt(y.OpenedOn.IsZero())); c != 0 {
			return c
		}
		if c := x.OpenedOn.Compare(y.OpenedOn); c != 0 {
			return c
		}
		return cmp.Compare(x.SourceKey, y.SourceKey)
	})
	for _, rec := range recs {
		id := AccountID(source, rec.SourceKey)
		acc, err := a.core.GetAccount(ctx, id)
		created := false
		switch {
		case errors.Is(err, store.ErrNotFound):
			slug, err := a.freeSlug(ctx, source+"-"+rec.Type)
			if err != nil {
				return err
			}
			acc = store.Account{ID: id, Source: source, SourceKey: rec.SourceKey, Slug: slug, Feed: feedAPI}
			created = true
		case err != nil:
			return err
		}
		acc.Type = rec.Type
		acc.Currency = rec.Currency
		acc.Name = rec.Name
		acc.ParentID = nil
		if rec.ParentSourceKey != "" {
			parent := AccountID(source, rec.ParentSourceKey)
			acc.ParentID = &parent
		}
		acc.OpenedOn = nil
		if !rec.OpenedOn.IsZero() {
			opened := rec.OpenedOn
			acc.OpenedOn = &opened
		}
		acc.Closed = rec.Closed
		if err := a.core.UpsertAccount(ctx, acc); err != nil {
			return err
		}
		if !created {
			continue
		}
		method := methodMonzoAbsolute
		if rec.Type == typePot {
			method = methodBalancesOnly
		}
		if err := a.core.EnsureReconciliation(ctx, id, method); err != nil {
			return err
		}
		a.stats.AccountsCreated++
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// freeSlug returns base, or base-2, base-3, ... whichever is first not taken.
func (a *applier) freeSlug(ctx context.Context, base string) (string, error) {
	for n := 1; ; n++ {
		slug := base
		if n > 1 {
			slug = base + "-" + strconv.Itoa(n)
		}
		_, err := a.core.GetAccountBySlug(ctx, slug)
		if errors.Is(err, store.ErrNotFound) {
			return slug, nil
		}
		if err != nil {
			return "", err
		}
	}
}

// validStatus is the set the core.transaction check constraint allows.
var validStatus = map[Status]bool{StatusPending: true, StatusSettled: true, StatusDeclined: true}

// applyTxn applies one transaction record to acc and moves the account's running sum by the
// change in its effective amount. seenAt and the version's raw row come from the row being
// applied. A record the schema would refuse skips the row instead of failing the projection.
func (a *applier) applyTxn(ctx context.Context, acc store.Account, source string, rec TxnRec, seenAt time.Time) error {
	if rec.SourceKey == "" || !validStatus[rec.Status] || len(rec.HashInput) == 0 || !json.Valid(rec.Content) {
		return unparseable("bad_transaction")
	}
	currency := rec.Currency
	if currency == "" {
		currency = acc.Currency
	}
	hash := sha256.Sum256(rec.HashInput)
	t := store.Transaction{
		ID:               TransactionID(acc.ID, source, rec.SourceKey),
		AccountID:        acc.ID,
		Source:           source,
		SourceKey:        rec.SourceKey,
		AmountMinor:      rec.AmountMinor,
		Currency:         currency,
		LocalAmountMinor: rec.LocalAmountMinor,
		LocalCurrency:    optional(rec.LocalCurrency),
		CreatedAt:        rec.CreatedAt,
		SettledAt:        rec.SettledAt,
		TxnDate:          rec.TxnDate,
		Status:           string(rec.Status),
		Description:      rec.Description,
		Counterparty:     optional(rec.Counterparty),
		MerchantID:       optional(rec.MerchantID),
		MerchantName:     optional(rec.MerchantName),
		Category:         optional(rec.Category),
		Scheme:           optional(rec.Scheme),
		Notes:            optional(rec.Notes),
	}
	res, err := a.core.ApplyTransaction(ctx, t, rec.Content, hash[:], seenAt, a.row.Seq)
	if err != nil {
		return err
	}
	var before int64
	switch res.Outcome {
	case store.TransactionCreated:
		a.stats.TransactionsCreated++
	case store.TransactionChanged, store.TransactionOlderVersion:
		// An older version leaves the row, so Current equals Previous and the delta is 0.
		a.stats.TransactionsChanged++
		before = res.Previous.EffectiveAmount()
	case store.TransactionUnchanged:
		before = res.Previous.EffectiveAmount()
	}
	return a.addToSum(ctx, acc.ID, res.Current.EffectiveAmount()-before)
}

func (a *applier) addToSum(ctx context.Context, accountID uuid.UUID, delta int64) error {
	if delta == 0 {
		return nil
	}
	_, err := a.core.AddToRunningSum(ctx, accountID, delta)
	return err
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// insertBalance stores one balance reading as the snapshot with this ordinal of the row.
func (a *applier) insertBalance(ctx context.Context, acc store.Account, ordinal int, b BalanceRec) error {
	snap := store.BalanceSnapshot{AccountID: acc.ID, RawSeq: a.row.Seq, Ordinal: ordinal,
		BalanceMinor: b.BalanceMinor, TotalBalanceMinor: b.TotalBalanceMinor, Currency: b.Currency}
	if snap.Currency == "" {
		snap.Currency = acc.Currency
	}
	if !b.At.IsZero() {
		at := b.At
		snap.At = &at
	}
	if !b.OnDate.IsZero() {
		on := b.OnDate
		snap.OnDate = &on
	}
	return a.core.InsertBalanceSnapshot(ctx, snap)
}

// absoluteCheck compares a balance reading with the account's running sum, for an account whose
// method is monzo_absolute. It runs when the account is closed (one reading is enough), or when
// the previous response of the same endpoint for the same account in the same run, normalised
// again, holds the same balance. Otherwise the reconciliation row is left as it is.
func (a *applier) absoluteCheck(ctx context.Context, n SourceNormaliser, fetch store.Fetch, r *store.APIResponse, acc store.Account, b BalanceRec) error {
	rec, err := a.core.GetReconciliation(ctx, acc.ID)
	if err != nil {
		return err
	}
	if rec.Method != methodMonzoAbsolute {
		return nil
	}
	if !acc.Closed {
		prev, found, err := store.PreviousResponseInRun(ctx, a.q, fetch.RunID, b.AccountSourceKey, r.Endpoint, r.Seq)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		pres, err := a.l.normalise(n, prev)
		if err != nil {
			return nil // the earlier reading cannot be read, so it cannot confirm this one
		}
		i := slices.IndexFunc(pres.Balances, func(p BalanceRec) bool { return p.AccountSourceKey == b.AccountSourceKey })
		if i < 0 || pres.Balances[i].BalanceMinor != b.BalanceMinor {
			return nil
		}
	}
	at := r.FetchedAt
	diff := b.BalanceMinor - rec.RunningSumMinor
	rec.LastCheckAt = &at
	rec.LastDiffMinor = &diff
	evaluate(&rec)
	return a.core.PutReconciliation(ctx, rec)
}

// evaluate sets LastCheckOK from LastDiffMinor, which must be set: the check passes when the
// difference is zero or equals the accepted difference. A pass moves LastPassAt to LastCheckAt.
// KnownException and AcceptedDiffMinor are left as they are: only an operator entry changes them.
func evaluate(rec *store.Reconciliation) {
	diff := *rec.LastDiffMinor
	ok := diff == 0 || (rec.AcceptedDiffMinor != nil && *rec.AcceptedDiffMinor == diff)
	rec.LastCheckOK = &ok
	if ok {
		rec.LastPassAt = rec.LastCheckAt
	}
}

// fetchComplete applies a completed fetch of one account's transactions over a period: every
// stored transaction of the account created in the period that none of the fetch's responses
// contained disappears, and every one they did contain that had disappeared returns. Only
// responses stored before the completion row are read, so replay sees what the live path saw.
// A fetch with no account or no period, or for an account not in core, changes nothing.
func (a *applier) fetchComplete(ctx context.Context, fc *store.FetchComplete) error {
	fetch, err := store.GetFetch(ctx, a.q, fc.FetchID)
	if err != nil {
		return err
	}
	if fetch.AccountKey == nil || fetch.PeriodStart == nil || fetch.PeriodEnd == nil {
		return nil
	}
	acc, err := a.core.GetAccount(ctx, AccountID(fetch.Source, *fetch.AccountKey))
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	n, ok := a.l.sources[fetch.Source]
	if !ok {
		return unparseable("no_normaliser")
	}
	resps, err := store.APIResponsesForFetch(ctx, a.q, fc.FetchID)
	if err != nil {
		return err
	}
	present := []uuid.UUID{}
	for _, r := range resps {
		if r.Seq >= a.row.Seq {
			continue
		}
		res, err := a.l.normalise(n, r)
		if err != nil {
			// A page that cannot be read proves nothing about absence.
			return unparseable("normaliser_error")
		}
		for _, t := range res.Txns {
			if t.AccountSourceKey == *fetch.AccountKey {
				present = append(present, TransactionID(acc.ID, fetch.Source, t.SourceKey))
			}
		}
	}
	dr, err := a.core.SetDisappeared(ctx, acc.ID, *fetch.PeriodStart, *fetch.PeriodEnd, present, fc.CompletedAt)
	if err != nil {
		return err
	}
	var delta int64
	for _, s := range dr.Disappeared {
		before := s
		before.DisappearedAt = nil
		delta -= before.EffectiveAmount()
		a.stats.Disappeared++
	}
	for _, s := range dr.Returned {
		delta += s.EffectiveAmount()
		a.stats.Returned++
	}
	return a.addToSum(ctx, acc.ID, delta)
}

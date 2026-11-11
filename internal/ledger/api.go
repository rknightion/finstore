package ledger

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rknightion/finstore/internal/store"
)

// Reconciliation methods.
const (
	methodMonzoAbsolute  = "monzo_absolute"
	methodRunningBalance = "running_balance"
	methodFileContinuity = "file_continuity"
	methodBalancesOnly   = "balances_only"
)

// SourceTrueLayerLloyds is the source of the TrueLayer connection to Lloyds. Its accounts are
// slugged lloyds-<type> and reconciled by running_balance.
const SourceTrueLayerLloyds = "truelayer:ob-lloyds"

// sourceTrueLayerPrefix begins the name of every TrueLayer source. A TrueLayer bank may hold a
// shorter history than was asked for, so a completed transactions fetch of one proves its period
// complete only from its earliest row.
const sourceTrueLayerPrefix = "truelayer:"

// slugPrefix maps a source to the prefix of its accounts' slugs; any other source is its own.
var slugPrefix = map[string]string{SourceTrueLayerLloyds: "lloyds"}

// sourceMethod maps a source to the reconciliation method of its accounts (pots excepted); any
// other source is checked as monzo_absolute.
var sourceMethod = map[string]string{SourceTrueLayerLloyds: methodRunningBalance}

// exceptionSharedBankRef is the known_exception of an account that shares its bank reference with
// an account of another source while neither has ended.
const exceptionSharedBankRef = "shared_bank_ref"

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
		if err := a.runningBalanceCheck(ctx, acc, r.FetchedAt, b); err != nil {
			return err
		}
	}
	return a.applyMandates(ctx, r.Source, res, r.FetchedAt)
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
			prefix, ok := slugPrefix[source]
			if !ok {
				prefix = source
			}
			slug, err := a.freeSlug(ctx, prefix+"-"+rec.Type)
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
		acc.BankRef = optional(rec.BankRef)
		if err := a.core.UpsertAccount(ctx, acc); err != nil {
			return err
		}
		if !created {
			continue
		}
		method, ok := sourceMethod[source]
		if !ok {
			method = methodMonzoAbsolute
		}
		if rec.Type == typePot {
			method = methodBalancesOnly
		}
		if err := a.core.EnsureReconciliation(ctx, id, method); err != nil {
			return err
		}
		a.stats.AccountsCreated++
	}
	if len(recs) == 0 {
		return nil
	}
	return a.checkSharedBankRefs(ctx)
}

// checkSharedBankRefs reports every account that shares its bank reference with an account of
// another source while neither has ended: its reconciliation gets known_exception
// shared_bank_ref and last_check_ok false. An account so reported whose condition no longer holds
// has the report cleared and its last check re-evaluated from the state its method left. It runs
// whenever an account's bank reference or end can have changed.
func (a *applier) checkSharedBankRefs(ctx context.Context) error {
	shared, err := a.core.SharedBankRefs(ctx)
	if err != nil {
		return err
	}
	recs, err := a.core.ListReconciliations(ctx)
	if err != nil {
		return err
	}
	for _, rec := range recs {
		_, isShared := shared[rec.AccountID]
		flagged := sharedFlag(rec)
		switch {
		case isShared && (!flagged || rec.LastCheckOK == nil || *rec.LastCheckOK):
			exception, ok := exceptionSharedBankRef, false
			rec.KnownException, rec.LastCheckOK = &exception, &ok
		case !isShared && flagged:
			reason, err := a.acceptedReason(ctx, rec)
			if err != nil {
				return err
			}
			rec.KnownException = reason
			recheck(&rec)
		default:
			continue
		}
		if err := a.core.PutReconciliation(ctx, rec); err != nil {
			return err
		}
	}
	return nil
}

// acceptedReason returns the reason of the accept_difference entry behind the account's accepted
// difference, which the shared bank reference report replaced in known_exception: the newest such
// entry before the row being applied that is readable and names the accepted difference. It is
// nil when the account has no accepted difference.
func (a *applier) acceptedReason(ctx context.Context, rec store.Reconciliation) (*string, error) {
	if rec.AcceptedDiffMinor == nil {
		return nil, nil
	}
	acc, err := a.core.GetAccount(ctx, rec.AccountID)
	if err != nil {
		return nil, err
	}
	entries, err := store.OperatorEntriesBefore(ctx, a.q, EntryAcceptDifference, acc.Slug, a.row.Seq)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		var p AcceptDifference
		if json.Unmarshal(e.Payload, &p) == nil && p.Reason != "" && p.DiffMinor != nil && *p.DiffMinor == *rec.AcceptedDiffMinor {
			return &p.Reason, nil
		}
	}
	return nil, nil
}

func sharedFlag(rec store.Reconciliation) bool {
	return rec.KnownException != nil && *rec.KnownException == exceptionSharedBankRef
}

// recheck re-evaluates LastCheckOK from what the account's method last recorded, after a shared
// bank reference report is cleared: a file account passes with no gap; an API account by its last
// difference (evaluate), or with no difference recorded, by its failing days alone. A
// running_balance difference that was not yet confirmed counts here as a failure until the next
// check.
func recheck(rec *store.Reconciliation) {
	switch {
	case rec.LastCheckAt == nil:
		rec.LastCheckOK = nil
	case rec.Method == methodFileContinuity || rec.LastDiffMinor == nil:
		ok := rec.BreakCount == 0
		rec.LastCheckOK = &ok
		if ok {
			rec.LastPassAt = rec.LastCheckAt
		}
	default:
		evaluate(rec)
	}
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
// difference is zero or equals the accepted difference, no day has failed (running_balance) and
// the account does not share its bank reference. A pass moves LastPassAt to LastCheckAt.
// KnownException and AcceptedDiffMinor are left as they are: only an operator entry changes them.
func evaluate(rec *store.Reconciliation) {
	diff := *rec.LastDiffMinor
	ok := (diff == 0 || (rec.AcceptedDiffMinor != nil && *rec.AcceptedDiffMinor == diff)) &&
		rec.BreakCount == 0 && !sharedFlag(*rec)
	rec.LastCheckOK = &ok
	if ok {
		rec.LastPassAt = rec.LastCheckAt
	}
}

// fetchComplete applies a completed fetch of one account's transactions over a period: it records
// the period the fetch proved complete in core.fetch_period, as instants, and then every stored
// transaction of the account created in that period that none of the fetch's responses contained
// disappears, and every one they did contain that had disappeared returns. Only responses stored
// before the completion row are read, so replay sees what the live path saw. A fetch with no
// account or no period, or for an account not in core, changes nothing.
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
	var earliest *time.Time
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
				if t.CreatedAt != nil && (earliest == nil || t.CreatedAt.Before(*earliest)) {
					earliest = t.CreatedAt
				}
			}
		}
	}
	// The period the fetch proved complete: the one asked for, except that a source whose history
	// may start later than asked proves it only from its earliest row, so a shorter history never
	// marks older rows disappeared. A response with no rows covers what was asked.
	start := *fetch.PeriodStart
	if strings.HasPrefix(fetch.Source, sourceTrueLayerPrefix) && earliest != nil && earliest.After(start) {
		start = *earliest
	}
	if err := a.core.PutFetchPeriod(ctx, store.FetchPeriod{FetchID: fc.FetchID, AccountID: acc.ID,
		PeriodStart: start, PeriodEnd: *fetch.PeriodEnd}); err != nil {
		return err
	}
	dr, err := a.core.SetDisappeared(ctx, fc.FetchID, present, fc.CompletedAt)
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

// runningBalanceCheck reconciles an account whose method is running_balance against a balance
// reading. It is recomputed from scratch over the account's transactions that have not
// disappeared, grouped by day, reading each row's balance after it from running_balance.amount in
// its latest version's content (rows carry a date and no time, so a day's rows have no known
// order):
//
//   - A day passes when its opening balance plus the sum of its amounts equals one of its running
//     balances, which is then its closing balance. Its opening balance is the previous day's
//     closing balance; on the first day of history, or after a day that failed, it is any value
//     that some row's balance before (its running balance minus its amount) gives and that fits.
//   - break_count is the number of failing days, failed_on the earliest, and reconciled_through
//     the day before it, else the newest day.
//   - The newest closing balance (0 with no rows) is compared with the reading. The difference is
//     recorded in last_diff_minor; it fails the check only when the previous check recorded the
//     same non-zero difference and it is not the accepted one, because the reading and the
//     transactions call are moments apart.
//
// last_check_ok is no failing day and a passing comparison (and no shared bank reference).
func (a *applier) runningBalanceCheck(ctx context.Context, acc store.Account, at time.Time, b BalanceRec) error {
	rec, err := a.core.GetReconciliation(ctx, acc.ID)
	if err != nil {
		return err
	}
	if rec.Method != methodRunningBalance {
		return nil
	}
	rows, err := a.core.LiveTransactionRows(ctx, acc.ID)
	if err != nil {
		return err
	}
	failing, newest, closing := checkDays(groupDays(rows))

	var diff *int64
	balanceOK := true
	if closing != nil {
		d := b.BalanceMinor - *closing
		diff = &d
		confirmed := rec.LastDiffMinor != nil && *rec.LastDiffMinor == d
		accepted := rec.AcceptedDiffMinor != nil && *rec.AcceptedDiffMinor == d
		balanceOK = d == 0 || accepted || !confirmed
	}
	rec.LastCheckAt = &at
	rec.LastDiffMinor = diff
	rec.BreakCount = len(failing)
	rec.FailedOn, rec.ReconciledThrough = nil, nil
	switch {
	case len(failing) > 0:
		first := failing[0]
		before := first.AddDate(0, 0, -1)
		rec.FailedOn, rec.ReconciledThrough = &first, &before
	case newest != nil:
		rec.ReconciledThrough = newest
	}
	ok := len(failing) == 0 && balanceOK && !sharedFlag(rec)
	rec.LastCheckOK = &ok
	if ok {
		rec.LastPassAt = &at
	}
	return a.core.PutReconciliation(ctx, rec)
}

// rbDay is one day of running_balance rows: the sum of their amounts, and the balances after and
// before each row that gives a readable running balance.
type rbDay struct {
	date    time.Time
	sum     int64
	afters  []int64
	befores []int64
}

func groupDays(rows []store.DayRow) []rbDay {
	var days []rbDay
	for _, r := range rows {
		if len(days) == 0 || !days[len(days)-1].date.Equal(r.TxnDate) {
			days = append(days, rbDay{date: r.TxnDate})
		}
		d := &days[len(days)-1]
		d.sum += r.AmountMinor
		if after, ok := runningBalance(r.Content); ok {
			d.afters = append(d.afters, after)
			d.befores = append(d.befores, after-r.AmountMinor)
		}
	}
	return days
}

// runningBalance reads running_balance.amount (decimal pounds, as a JSON number or string) from a
// row's content.
func runningBalance(content json.RawMessage) (int64, bool) {
	var doc struct {
		RunningBalance *struct {
			Amount json.RawMessage `json:"amount"`
		} `json:"running_balance"`
	}
	if err := json.Unmarshal(content, &doc); err != nil || doc.RunningBalance == nil {
		return 0, false
	}
	text := string(doc.RunningBalance.Amount)
	if unquoted, err := strconv.Unquote(text); err == nil {
		text = unquoted
	}
	n, err := ParseMinor(text)
	return n, err == nil
}

// checkDays walks days in date order and returns the failing days, the newest day, and the
// newest day's closing balance (nil when that day failed, 0 when there are no days).
func checkDays(days []rbDay) (failing []time.Time, newest *time.Time, closing *int64) {
	var prev *int64
	for _, d := range days {
		openings := []int64{}
		if prev != nil {
			openings = append(openings, *prev)
		} else {
			openings = append(openings, d.befores...)
			slices.Sort(openings)
		}
		prev = nil
		for _, o := range openings {
			if c := o + d.sum; slices.Contains(d.afters, c) {
				prev = &c
				break
			}
		}
		if prev == nil {
			failing = append(failing, d.date)
		}
	}
	if len(days) == 0 {
		zero := int64(0)
		return nil, nil, &zero
	}
	last := days[len(days)-1].date
	return failing, &last, prev
}

// ParseMinor converts a decimal amount in major units, as a bank writes it in JSON ("-12.3",
// "1919.59", "7"), to minor units exactly. It accepts an optional leading minus, digits, and at
// most two decimal places; anything else, or a value outside int64, is an error.
func ParseMinor(s string) (int64, error) {
	bad := errors.New("ledger: not a decimal amount with at most two decimal places")
	digitsOnly := func(x string) bool {
		return x != "" && strings.Trim(x, "0123456789") == ""
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, dotted := strings.Cut(s, ".")
	if !digitsOnly(whole) || (dotted && (!digitsOnly(frac) || len(frac) > 2)) {
		return 0, bad
	}
	for len(frac) < 2 {
		frac += "0"
	}
	n, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return 0, bad
	}
	if neg {
		n = -n
	}
	return n, nil
}

// applyMandates applies a response's standing orders and direct debits, then its mandate lists.
// Records of one kind that share a bank reference and key are told apart first (numberKeys).
// Each is resolved to an account (resolveMandate) and versioned like a transaction under the
// fetching source; for each listed kind and bank reference, that source's mandates missing from
// the response disappear and those present that had disappeared return.
func (a *applier) applyMandates(ctx context.Context, source string, res Result, seenAt time.Time) error {
	if len(res.StandingOrders) == 0 && len(res.DirectDebits) == 0 && len(res.MandateLists) == 0 {
		return nil
	}
	for _, l := range res.MandateLists {
		if (l.Kind != MandateStandingOrder && l.Kind != MandateDirectDebit) || l.BankRef == "" {
			return unparseable("bad_mandate_list")
		}
	}
	sos := numberKeys(res.StandingOrders, func(r *StandingOrderRec) mandateOrder {
		return mandateOrder{r.BankRef, &r.MandateKey, r.FirstDate, r.FirstAmountMinor, r.HashInput}
	})
	dds := numberKeys(res.DirectDebits, func(r *DirectDebitRec) mandateOrder {
		return mandateOrder{r.BankRef, &r.MandateKey, r.PreviousDate, r.PreviousAmountMinor, r.HashInput}
	})
	present := map[MandateList][]uuid.UUID{}
	resolved := map[string]*uuid.UUID{}
	resolve := func(bankRef string) (*uuid.UUID, error) {
		if id, ok := resolved[bankRef]; ok {
			return id, nil
		}
		id, err := a.resolveMandate(ctx, source, bankRef)
		resolved[bankRef] = id
		return id, err
	}
	for _, r := range sos {
		if !validMandate(r.BankRef, r.MandateKey, r.Content, r.HashInput) {
			return unparseable("bad_mandate")
		}
		accountID, err := resolve(r.BankRef)
		if err != nil {
			return err
		}
		id := MandateID(source, r.BankRef, MandateStandingOrder, r.MandateKey)
		hash := sha256.Sum256(r.HashInput)
		out, err := a.core.ApplyStandingOrder(ctx, store.StandingOrder{ID: id, AccountID: accountID, BankRef: r.BankRef,
			Source: source, MandateKey: r.MandateKey, Payee: optional(r.Payee), Reference: optional(r.Reference),
			Frequency: optional(r.Frequency), Status: optional(r.Status), FirstDate: optionalDate(r.FirstDate),
			FirstAmountMinor: r.FirstAmountMinor, NextDate: optionalDate(r.NextDate), NextAmountMinor: r.NextAmountMinor,
			FinalDate: optionalDate(r.FinalDate), FinalAmountMinor: r.FinalAmountMinor, Currency: optional(r.Currency)},
			r.Content, hash[:], seenAt, a.row.Seq)
		if err != nil {
			return err
		}
		a.countMandate(out, accountID)
		k := MandateList{Kind: MandateStandingOrder, BankRef: r.BankRef}
		present[k] = append(present[k], id)
	}
	for _, r := range dds {
		if !validMandate(r.BankRef, r.MandateKey, r.Content, r.HashInput) {
			return unparseable("bad_mandate")
		}
		accountID, err := resolve(r.BankRef)
		if err != nil {
			return err
		}
		id := MandateID(source, r.BankRef, MandateDirectDebit, r.MandateKey)
		hash := sha256.Sum256(r.HashInput)
		out, err := a.core.ApplyDirectDebit(ctx, store.DirectDebit{ID: id, AccountID: accountID, BankRef: r.BankRef,
			Source: source, MandateKey: r.MandateKey, Name: optional(r.Name), Status: optional(r.Status),
			PreviousDate: optionalDate(r.PreviousDate), PreviousAmountMinor: r.PreviousAmountMinor,
			Currency: optional(r.Currency)}, r.Content, hash[:], seenAt, a.row.Seq)
		if err != nil {
			return err
		}
		a.countMandate(out, accountID)
		k := MandateList{Kind: MandateDirectDebit, BankRef: r.BankRef}
		present[k] = append(present[k], id)
	}
	for _, l := range res.MandateLists {
		gone, back, err := a.core.SetMandatesDisappeared(ctx, l.Kind, source, l.BankRef, present[l], seenAt)
		if err != nil {
			return err
		}
		a.stats.MandatesDisappeared += gone
		a.stats.MandatesReturned += back
	}
	return nil
}

func validMandate(bankRef, key string, content json.RawMessage, hashInput []byte) bool {
	return bankRef != "" && key != "" && len(hashInput) > 0 && json.Valid(content)
}

func (a *applier) countMandate(out store.ApplyOutcome, accountID *uuid.UUID) {
	switch out {
	case store.TransactionCreated:
		a.stats.MandatesCreated++
	case store.TransactionChanged, store.TransactionOlderVersion:
		a.stats.MandatesChanged++
	}
	if accountID == nil {
		a.stats.MandatesUnresolved++
	}
}

// resolveMandate returns the account a mandate with bankRef from source belongs to: source's
// account whose source key is the bank reference; otherwise the one account of any source with
// that bank reference and no end; otherwise none (nil), and the mandate is kept unresolved.
func (a *applier) resolveMandate(ctx context.Context, source, bankRef string) (*uuid.UUID, error) {
	acc, err := a.core.GetAccount(ctx, AccountID(source, bankRef))
	if err == nil {
		return &acc.ID, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	accs, err := a.core.AccountsByBankRef(ctx, bankRef)
	if err != nil {
		return nil, err
	}
	if len(accs) != 1 {
		return nil, nil
	}
	return &accs[0].ID, nil
}

// mandateOrder is what numberKeys reads of a mandate record: its bank reference, a pointer to its
// key, and the date, amount and hash input that order records sharing a key.
type mandateOrder struct {
	bankRef string
	key     *string
	date    time.Time
	amount  *int64
	hash    []byte
}

// numberKeys returns a copy of recs in which records sharing a bank reference and key keep the
// key for the first and get "#2", "#3" ... appended for the rest, in order of date (unknown
// first), then amount (unknown first), then hash input, so the numbering does not depend on the
// order the bank lists them in. Every key is first escaped ("\" becomes "\\" and "#" becomes
// "\#"), so a "#" left unescaped is always the ledger's number and a natural key such as "X#2"
// (stored as "X\#2") can never collide with the second "X".
func numberKeys[T any](recs []T, order func(*T) mandateOrder) []T {
	out := slices.Clone(recs)
	groups := map[[2]string][]int{}
	var keys [][2]string
	escape := strings.NewReplacer(`\`, `\\`, `#`, `\#`)
	for i := range out {
		o := order(&out[i])
		*o.key = escape.Replace(*o.key)
		k := [2]string{o.bankRef, *o.key}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], i)
	}
	for _, k := range keys {
		idx := groups[k]
		if len(idx) < 2 {
			continue
		}
		slices.SortStableFunc(idx, func(x, y int) int {
			ox, oy := order(&out[x]), order(&out[y])
			if c := ox.date.Compare(oy.date); c != 0 {
				return c
			}
			if c := compareAmount(ox.amount, oy.amount); c != 0 {
				return c
			}
			return strings.Compare(string(ox.hash), string(oy.hash))
		})
		for n, i := range idx[1:] {
			o := order(&out[i])
			*o.key += "#" + strconv.Itoa(n+2)
		}
	}
	return out
}

// compareAmount orders an unknown amount before every known one.
func compareAmount(x, y *int64) int {
	switch {
	case x == nil && y == nil:
		return 0
	case x == nil:
		return -1
	case y == nil:
		return 1
	}
	return cmp.Compare(*x, *y)
}

func optionalDate(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

package ledger

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rknightion/finstore/internal/store"
)

// investmentProvider is what the ledger knows of one provider: its products and how its accounts
// are checked.
type investmentProvider struct {
	products []string
	method   string
	// tickerKeys says an instrument's provider key is its ticker.
	tickerKeys bool
}

var investmentProviders = map[string]investmentProvider{
	ProviderFidelity: {products: []string{ProductSIPP, ProductISA, ProductCashManagement}, method: methodHoldingsReplay, tickerKeys: true},
	ProviderAviva:    {products: []string{ProductPensionPlan}, method: methodStatementFlows},
}

// productAccount maps a product to its child account's type and the middle word of its slug.
var productAccount = map[string]struct{ accountType, slugWord, label string }{
	ProductSIPP:           {"pension", "sipp", "SIPP"},
	ProductISA:            {"isa", "isa", "ISA"},
	ProductCashManagement: {"investment_cash", "cash", "cash account"},
	ProductPensionPlan:    {"pension", "pension", "pension"},
}

// lastFour is the last four alphanumeric characters of an account number, lower-cased.
func lastFour(accountKey string) (string, bool) {
	var kept []rune
	for _, r := range strings.ToLower(accountKey) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			kept = append(kept, r)
		}
	}
	if len(kept) < 4 {
		return "", false
	}
	return string(kept[len(kept)-4:]), true
}

// importInvestment applies one investment file to its platform. The file is rejected, with nothing
// written, when the account is not a platform of the normaliser's provider (account_mismatch), the
// normaliser refuses it (check_failed), its result breaks the contract (bad_result), or a provider
// account it names cannot become a child of this platform (account_mismatch, child_key_shared).
//
// Otherwise its children, instruments and rows are stored, and then disappearance, statement
// winners and the reconciliation method are recomputed for every child of the platform, in slug
// order, so that the result depends on the set of files held.
func (a *applier) importInvestment(ctx context.Context, platform store.Account, n InvestmentNormaliser, f *store.ImportFile) error {
	provider := n.Provider()
	if platform.Type != typePlatform || platform.Source != InvestmentSource(provider) {
		return rejected("account_mismatch")
	}
	raw, err := n.NormaliseInvestmentFile(File{Seq: f.Seq, AccountSlug: f.AccountSlug, Format: f.Format, Body: f.Body}, a.l.loc)
	if err != nil {
		return rejected("check_failed")
	}
	res, ok := sanitiseInvestmentResult(provider, raw)
	if !ok {
		return rejected("bad_result")
	}
	children, err := a.ensureChildren(ctx, platform, res.Accounts)
	if err != nil {
		return err
	}
	if err := a.applyInvestmentRows(ctx, platform, provider, children, res, f); err != nil {
		return err
	}
	if err := a.core.InsertInvestmentImport(ctx, store.InvestmentImport{RawSeq: f.Seq, PlatformID: platform.ID,
		Format: f.Format, Kind: res.Kind, RowCount: res.RowCount, CapturedAt: res.CapturedAt, ImportedAt: f.ImportedAt}); err != nil {
		return err
	}
	return a.recomputePlatform(ctx, platform, f.ImportedAt)
}

// ensureChildren returns the child account of each record, by account key, creating those not yet
// held. A child's id is derived from the provider and account number, so an account held under
// another platform is found and refused.
func (a *applier) ensureChildren(ctx context.Context, platform store.Account, recs []InvestmentAccountRec) (map[string]store.Account, error) {
	recs = slices.Clone(recs)
	slices.SortFunc(recs, func(x, y InvestmentAccountRec) int { return cmp.Compare(x.AccountKey, y.AccountKey) })
	provider := strings.TrimPrefix(platform.Source, investmentSourcePrefix)
	out := make(map[string]store.Account, len(recs))
	for _, rec := range recs {
		four, ok := lastFour(rec.AccountKey)
		if !ok {
			return nil, rejected("bad_result")
		}
		product := productAccount[rec.Product]
		slug := platform.Slug + "-" + product.slugWord + "-" + four
		id := AccountID(platform.Source, rec.AccountKey)
		acc, err := a.core.GetAccount(ctx, id)
		switch {
		case err == nil:
			if acc.ParentID == nil || *acc.ParentID != platform.ID || acc.Slug != slug || acc.Type != product.accountType {
				return nil, rejected("account_mismatch")
			}
		case errors.Is(err, store.ErrNotFound):
			if _, err := a.core.GetAccountBySlug(ctx, slug); err == nil {
				return nil, rejected("child_key_shared")
			} else if !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
			parent := platform.ID
			acc = store.Account{ID: id, Source: platform.Source, SourceKey: rec.AccountKey, Slug: slug, Type: product.accountType,
				Feed: feedFile, Currency: platform.Currency, Name: platform.Name + " " + product.label, ParentID: &parent,
				ExpectsImports: platform.ExpectsImports}
			if err := a.core.UpsertAccount(ctx, acc); err != nil {
				return nil, err
			}
			if err := a.core.EnsureReconciliation(ctx, id, investmentProviders[provider].method); err != nil {
				return nil, err
			}
			a.stats.AccountsCreated++
		default:
			return nil, err
		}
		out[rec.AccountKey] = acc
	}
	return out, nil
}

// applyInvestmentRows stores the instruments and rows of one accepted result.
func (a *applier) applyInvestmentRows(ctx context.Context, platform store.Account, provider string, children map[string]store.Account, res InvestmentResult, f *store.ImportFile) error {
	instrument := func(key string) (uuid.UUID, error) {
		id := InstrumentID(provider, key)
		var ticker *string
		if investmentProviders[provider].tickerKeys {
			ticker = &key
		}
		return id, a.core.EnsureInstrument(ctx, store.Instrument{ID: id, Provider: provider, ProviderKey: key, Ticker: ticker})
	}
	currency := func(c string) string {
		if c == "" {
			return platform.Currency
		}
		return c
	}
	for _, r := range res.Events {
		acc := children[r.AccountKey]
		e := store.InvestmentEvent{ID: InvestmentEventID(acc.ID, platform.Source, r.SourceKey), AccountID: acc.ID,
			Source: platform.Source, SourceKey: r.SourceKey, EventType: r.EventType, ProviderType: r.ProviderType,
			InstrumentName: optional(r.InstrumentName), OrderDate: r.OrderDate, SettleDate: optionalDate(r.SettleDate),
			Units: optional(r.Units), Price: optional(r.Price), PriceUnit: optional(r.PriceUnit), GrossMinor: r.GrossMinor,
			CashMinor: r.CashMinor, FlowMinor: r.FlowMinor, Currency: currency(r.Currency)}
		if r.InstrumentKey != "" {
			id, err := instrument(r.InstrumentKey)
			if err != nil {
				return err
			}
			e.InstrumentID = &id
		}
		hash := sha256.Sum256(r.HashInput)
		if _, err := a.core.ApplyInvestmentEvent(ctx, e, r.Content, hash[:], f.ImportedAt, f.Seq); err != nil {
			return err
		}
	}
	for _, h := range res.Holdings {
		id, err := instrument(h.InstrumentKey)
		if err != nil {
			return err
		}
		if err := a.core.InsertHoldingSnapshot(ctx, store.HoldingSnapshot{AccountID: children[h.AccountKey].ID, RawSeq: f.Seq,
			InstrumentID: id, InstrumentName: h.InstrumentName, AsOf: h.AsOf, CapturedAt: *res.CapturedAt, Units: h.Units,
			Price: h.Price, PriceUnit: h.PriceUnit, ValueMinor: h.ValueMinor, BookCostMinor: h.BookCostMinor,
			Currency: currency(h.Currency)}); err != nil {
			return err
		}
	}
	for _, v := range res.Valuations {
		if err := a.core.InsertValuation(ctx, store.Valuation{AccountID: children[v.AccountKey].ID, RawSeq: f.Seq, AsOf: v.AsOf,
			CapturedAt: *res.CapturedAt, TotalMinor: v.TotalMinor, CashMinor: v.CashMinor,
			ReportedGainMinor: v.ReportedGainMinor, Currency: currency(v.Currency)}); err != nil {
			return err
		}
	}
	for _, s := range res.Statements {
		if err := a.core.InsertPensionStatement(ctx, store.PensionStatement{AccountID: children[s.AccountKey].ID, RawSeq: f.Seq,
			PeriodStart: s.PeriodStart, PeriodEnd: s.PeriodEnd, IssuedOn: s.IssuedOn, OpeningMinor: s.OpeningMinor,
			PaidInMinor: s.PaidInMinor, PaidOutMinor: s.PaidOutMinor, GrowthMinor: s.GrowthMinor, ChargesMinor: s.ChargesMinor,
			ClosingMinor: s.ClosingMinor, Charges: s.Charges, Projections: s.Projections}); err != nil {
			return err
		}
	}
	for _, n := range res.TradeNotes {
		if err := a.core.PutTradeNote(ctx, store.TradeNote{AccountID: children[n.AccountKey].ID, Reference: n.Reference,
			RawSeq: f.Seq, ISIN: n.ISIN, ExecutedAt: n.ExecutedAt, Venue: optional(n.Venue), Units: n.Units,
			ExactPrice: n.ExactPrice, GrossMinor: n.GrossMinor, FeeMinor: n.FeeMinor, TaxMinor: n.TaxMinor,
			NetMinor: n.NetMinor, SettleDate: n.SettleDate}); err != nil {
			return err
		}
	}
	return nil
}

// storedDoc is one accepted capture or statement file, read again from raw. The contribution list
// of a capture and the payments of a statement are not columns of any core table, so disappearance
// reads them the way a completed fetch reads its responses: by normalising the stored bytes.
type storedDoc struct {
	imp store.InvestmentImport
	res InvestmentResult
}

// storedDocs normalises the platform's accepted captures and statements. A file that was accepted
// and no longer normalises means the code changed without a revision bump: the row being applied
// is rejected rather than deciding disappearance from a partial set.
func (a *applier) storedDocs(ctx context.Context, platform store.Account) ([]storedDoc, error) {
	imports, err := a.core.ListInvestmentImports(ctx, platform.ID, []string{InvestmentKindCapture, InvestmentKindStatement})
	if err != nil {
		return nil, err
	}
	provider := strings.TrimPrefix(platform.Source, investmentSourcePrefix)
	docs := make([]storedDoc, 0, len(imports))
	for _, imp := range imports {
		n, ok := a.l.investments[imp.Format]
		if !ok {
			return nil, rejected("unknown_format")
		}
		f, err := store.GetImportFile(ctx, a.q, imp.RawSeq)
		if err != nil {
			return nil, err
		}
		raw, err := n.NormaliseInvestmentFile(File{Seq: f.Seq, AccountSlug: f.AccountSlug, Format: f.Format, Body: f.Body}, a.l.loc)
		if err != nil {
			return nil, rejected("check_failed")
		}
		res, ok := sanitiseInvestmentResult(provider, raw)
		if !ok {
			return nil, rejected("bad_result")
		}
		docs = append(docs, storedDoc{imp: imp, res: res})
	}
	return docs, nil
}

// recomputePlatform recomputes statement winners, disappearance and the reconciliation method of
// every child of the platform, in slug order. at is the imported_at of the row being applied.
func (a *applier) recomputePlatform(ctx context.Context, platform store.Account, at time.Time) error {
	children, err := a.core.ListChildAccounts(ctx, platform.ID)
	if err != nil {
		return err
	}
	docs, err := a.storedDocs(ctx, platform)
	if err != nil {
		return err
	}
	for _, child := range children {
		winners, err := a.recomputeWinners(ctx, child)
		if err != nil {
			return err
		}
		if err := a.recomputeDisappearance(ctx, child, docs, winners, at); err != nil {
			return err
		}
		if err := a.runInvestmentMethod(ctx, child, at); err != nil {
			return err
		}
	}
	return nil
}

// recomputeWinners walks the account's statements by issue date, then raw seq, newest first. A
// statement is a winner when its period overlaps no winner already kept. It returns the winners'
// raw seqs.
func (a *applier) recomputeWinners(ctx context.Context, acc store.Account) (map[int64]bool, error) {
	stmts, err := a.core.ListPensionStatements(ctx, acc.ID)
	if err != nil {
		return nil, err
	}
	var kept []store.PensionStatement
	winners := make(map[int64]bool, len(stmts))
	seqs := []int64{}
	for _, s := range stmts {
		if slices.ContainsFunc(kept, func(w store.PensionStatement) bool {
			return !s.PeriodStart.After(w.PeriodEnd) && !w.PeriodStart.After(s.PeriodEnd)
		}) {
			continue
		}
		kept = append(kept, s)
		winners[s.RawSeq] = true
		seqs = append(seqs, s.RawSeq)
	}
	return winners, a.core.SetStatementWinners(ctx, acc.ID, seqs)
}

// recomputeDisappearance decides which of the account's contributions and transfers out have
// disappeared, from all stored captures and statements.
//
// Contributions: the deciding capture is the newest (printed capture time, then raw seq) that
// declares the account's list complete. A contribution has disappeared when it is dated on or
// before that capture's date, is missing from it, and is in no later capture, which it never
// overrides.
//
// Transfers out: one that only losing statements contain has disappeared.
//
// A row that is not gone has disappeared_at cleared; a newly gone row takes at.
func (a *applier) recomputeDisappearance(ctx context.Context, acc store.Account, docs []storedDoc, winners map[int64]bool, at time.Time) error {
	events, err := a.core.ListInvestmentEvents(ctx, acc.ID)
	if err != nil {
		return err
	}
	var captures []storedDoc
	inWinner, inLoser := map[string]bool{}, map[string]bool{}
	for _, d := range docs {
		switch d.imp.Kind {
		case InvestmentKindCapture:
			captures = append(captures, d)
		case InvestmentKindStatement:
			if !slices.ContainsFunc(d.res.Statements, func(s PensionStatementRec) bool { return s.AccountKey == acc.SourceKey }) {
				continue
			}
			into := inLoser
			if winners[d.imp.RawSeq] {
				into = inWinner
			}
			for _, e := range d.res.Events {
				if e.AccountKey == acc.SourceKey && e.EventType == EventTransferOut {
					into[e.SourceKey] = true
				}
			}
		}
	}
	slices.SortFunc(captures, func(x, y storedDoc) int {
		if c := x.imp.CapturedAt.Compare(*y.imp.CapturedAt); c != 0 {
			return c
		}
		return cmp.Compare(x.imp.RawSeq, y.imp.RawSeq)
	})
	deciding := -1
	for i, d := range captures {
		if slices.Contains(d.res.CompleteContributionsFor, acc.SourceKey) {
			deciding = i
		}
	}
	var decidingDate time.Time
	present := map[string]bool{}
	if deciding >= 0 {
		for _, v := range captures[deciding].res.Valuations {
			if v.AccountKey == acc.SourceKey {
				decidingDate = v.AsOf
			}
		}
		for _, d := range captures[deciding:] {
			for _, e := range d.res.Events {
				if e.AccountKey == acc.SourceKey && e.EventType == EventContribution {
					present[e.SourceKey] = true
				}
			}
		}
	}
	gone := []uuid.UUID{}
	for _, e := range events {
		switch e.EventType {
		case EventContribution:
			if deciding >= 0 && !e.EffectiveDate().After(decidingDate) && !present[e.SourceKey] {
				gone = append(gone, e.ID)
			}
		case EventTransferOut:
			if inLoser[e.SourceKey] && !inWinner[e.SourceKey] {
				gone = append(gone, e.ID)
			}
		}
	}
	return a.core.SetInvestmentEventsDisappeared(ctx, acc.ID, gone, at)
}

// runInvestmentMethod runs the account's reconciliation method. at is when the row being applied
// was entered, which an outcome records as last_check_at.
func (a *applier) runInvestmentMethod(ctx context.Context, acc store.Account, at time.Time) error {
	rec, err := a.core.GetReconciliation(ctx, acc.ID)
	if err != nil {
		return err
	}
	var outcome investmentOutcome
	switch rec.Method {
	case methodHoldingsReplay:
		outcome, err = a.holdingsReplay(ctx, acc)
	case methodStatementFlows:
		outcome, err = a.statementFlows(ctx, acc)
	default:
		return nil
	}
	if err != nil {
		return err
	}
	if rec.Method == methodHoldingsReplay {
		if err := a.core.ReplaceHoldingChecks(ctx, acc.ID, outcome.checks); err != nil {
			return err
		}
	}
	outcome.apply(&rec, at)
	return a.core.PutReconciliation(ctx, rec)
}

// investmentOutcome is the verdict of one run of a method: one row of the design's outcome table.
type investmentOutcome struct {
	// checked is false when nothing is checkable yet.
	checked bool
	// pass is true when the check found no difference.
	pass bool
	// diff is the cash difference: stated minus stored. It is 0 on a units-only difference.
	diff int64
	// through is the date a pass reconciles the account through.
	through time.Time
	checks  []store.HoldingCheck
}

// apply writes the outcome to rec.
//
//	Outcome                last_check_at  ok     diff  last_pass_at  reconciled_through  breaks
//	Pass                   at             true   0     at            through             0
//	Cash difference        at             false  diff  unchanged     unchanged           1
//	Units-only difference  at             false  0     unchanged     unchanged           1
//	Nothing checkable yet  null           null   null  null          null                0
func (o investmentOutcome) apply(rec *store.Reconciliation, at time.Time) {
	if !o.checked {
		rec.LastCheckAt, rec.LastCheckOK, rec.LastDiffMinor, rec.LastPassAt, rec.ReconciledThrough = nil, nil, nil, nil, nil
		rec.BreakCount = 0
		return
	}
	ok, diff := o.pass, o.diff
	rec.LastCheckAt, rec.LastCheckOK, rec.LastDiffMinor = &at, &ok, &diff
	if o.pass {
		through := o.through
		rec.LastPassAt, rec.ReconciledThrough, rec.BreakCount = &at, &through, 0
		return
	}
	rec.BreakCount = 1
}

// holdingsReplay checks the newest checkable snapshot of an account with a ledger against the
// replay of all its stored events, so that the order of imports does not change the totals. A
// capture is checked by the valuation it holds for the account, with the holdings of the same raw
// row. Only the latest capture of a date is a candidate for that date. For an ended account only
// snapshots on or before its end are candidates.
//
// A snapshot on date D is checkable when a ledger file of the platform was imported at or after the
// snapshot's printed capture time, no event has an order or effective date of D, and no event is
// unsettled at D (order_date < D < settle_date).
//
// Units are compared over the full outer join of instruments with non-zero expected units (the
// signed movements with order_date <= D) and instruments in the snapshot, a missing side counting
// as zero. Cash is compared with the sum of cash_minor for effective dates on or before D.
func (a *applier) holdingsReplay(ctx context.Context, acc store.Account) (investmentOutcome, error) {
	if acc.ParentID == nil {
		return investmentOutcome{}, nil
	}
	latestLedger, err := a.core.LatestLedgerImport(ctx, *acc.ParentID)
	if err != nil {
		return investmentOutcome{}, err
	}
	if latestLedger == nil {
		return investmentOutcome{}, nil
	}
	valuations, err := a.core.ListValuations(ctx, acc.ID)
	if err != nil {
		return investmentOutcome{}, err
	}
	all, err := a.core.ListInvestmentEvents(ctx, acc.ID)
	if err != nil {
		return investmentOutcome{}, err
	}
	events := slices.DeleteFunc(all, func(e store.InvestmentEvent) bool { return e.DisappearedAt != nil })
	var previous *time.Time
	for i := range valuations {
		v := valuations[i]
		if previous != nil && previous.Equal(v.AsOf) {
			continue // an earlier capture of a date already considered
		}
		previous = &valuations[i].AsOf
		if acc.EndedOn != nil && v.AsOf.After(*acc.EndedOn) {
			continue
		}
		if latestLedger.Before(v.CapturedAt) || !quietOn(events, v.AsOf) {
			continue
		}
		return a.compareSnapshot(ctx, acc, v, events)
	}
	return investmentOutcome{}, nil
}

// quietOn reports that no event touches day: none is ordered or takes effect on it, and none is
// unsettled across it.
func quietOn(events []store.InvestmentEvent, day time.Time) bool {
	return !slices.ContainsFunc(events, func(e store.InvestmentEvent) bool {
		if e.OrderDate.Equal(day) || e.EffectiveDate().Equal(day) {
			return true
		}
		return e.SettleDate != nil && e.OrderDate.Before(day) && day.Before(*e.SettleDate)
	})
}

// compareSnapshot compares one capture with the replay of events.
func (a *applier) compareSnapshot(ctx context.Context, acc store.Account, v store.Valuation, events []store.InvestmentEvent) (investmentOutcome, error) {
	holdings, err := a.core.ListHoldingSnapshots(ctx, acc.ID, v.RawSeq)
	if err != nil {
		return investmentOutcome{}, err
	}
	expected := map[uuid.UUID]*big.Rat{}
	var cash int64
	scale := 0
	for _, e := range events {
		if !e.EffectiveDate().After(v.AsOf) {
			cash += e.CashMinor
		}
		if e.Units == nil || e.InstrumentID == nil || e.OrderDate.After(v.AsOf) {
			continue
		}
		units, ok := new(big.Rat).SetString(*e.Units)
		if !ok {
			return investmentOutcome{}, errors.New("ledger: stored units are not a decimal")
		}
		scale = max(scale, decimalPlaces(*e.Units))
		if e.EventType == EventSell || e.EventType == EventTransferOut {
			units.Neg(units)
		}
		if expected[*e.InstrumentID] == nil {
			expected[*e.InstrumentID] = new(big.Rat)
		}
		expected[*e.InstrumentID].Add(expected[*e.InstrumentID], units)
	}
	reported := map[uuid.UUID]*big.Rat{}
	for _, h := range holdings {
		units, ok := new(big.Rat).SetString(h.Units)
		if !ok {
			return investmentOutcome{}, errors.New("ledger: stored units are not a decimal")
		}
		scale = max(scale, decimalPlaces(h.Units))
		reported[h.InstrumentID] = units
	}
	ids := make([]uuid.UUID, 0, len(expected)+len(reported))
	for id, units := range expected {
		if units.Sign() != 0 {
			ids = append(ids, id)
		}
	}
	for id := range reported {
		if u := expected[id]; u == nil || u.Sign() == 0 {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(x, y uuid.UUID) int { return strings.Compare(x.String(), y.String()) })
	zero := new(big.Rat)
	unitsDiffer := false
	checks := make([]store.HoldingCheck, 0, len(ids))
	for _, id := range ids {
		exp, rep := expected[id], reported[id]
		if exp == nil {
			exp = zero
		}
		if rep == nil {
			rep = zero
		}
		diff := new(big.Rat).Sub(rep, exp)
		if diff.Sign() != 0 {
			unitsDiffer = true
		}
		checks = append(checks, store.HoldingCheck{AccountID: acc.ID, InstrumentID: id, CheckedAsOf: v.AsOf,
			ExpectedUnits: exp.FloatString(scale), ReportedUnits: rep.FloatString(scale), Difference: diff.FloatString(scale)})
	}
	cashDiff := v.CashMinor - cash
	return investmentOutcome{checked: true, pass: cashDiff == 0 && !unitsDiffer, diff: cashDiff, through: v.AsOf, checks: checks}, nil
}

func decimalPlaces(s string) int {
	_, frac, _ := strings.Cut(s, ".")
	return len(frac)
}

// statementFlows checks an account whose provider has no ledger against its winning statements, in
// period order: the stored contributions dated in each period must sum to its paid-in figure, and
// its opening must equal the previous statement's closing when the two periods abut. The first
// failure, oldest statement first, gives the difference (stated minus stored). A pass reconciles the
// account through its newest winning statement, and an account with none has nothing checkable.
// For an ended account only statements ending on or before its end count.
func (a *applier) statementFlows(ctx context.Context, acc store.Account) (investmentOutcome, error) {
	stmts, err := a.core.ListPensionStatements(ctx, acc.ID)
	if err != nil {
		return investmentOutcome{}, err
	}
	stmts = slices.DeleteFunc(stmts, func(s store.PensionStatement) bool {
		return !s.IsWinner || (acc.EndedOn != nil && s.PeriodEnd.After(*acc.EndedOn))
	})
	if len(stmts) == 0 {
		return investmentOutcome{}, nil
	}
	slices.SortFunc(stmts, func(x, y store.PensionStatement) int { return x.PeriodStart.Compare(y.PeriodStart) })
	events, err := a.core.ListInvestmentEvents(ctx, acc.ID)
	if err != nil {
		return investmentOutcome{}, err
	}
	out := investmentOutcome{checked: true, pass: true, through: stmts[len(stmts)-1].PeriodEnd}
	for i, s := range stmts {
		var paidIn int64
		for _, e := range events {
			d := e.EffectiveDate()
			if e.EventType == EventContribution && e.DisappearedAt == nil && !d.Before(s.PeriodStart) && !d.After(s.PeriodEnd) {
				paidIn += e.FlowMinor
			}
		}
		diff := s.PaidInMinor - paidIn
		if diff == 0 && i > 0 && stmts[i-1].PeriodEnd.AddDate(0, 0, 1).Equal(s.PeriodStart) {
			diff = s.OpeningMinor - stmts[i-1].ClosingMinor
		}
		if diff != 0 {
			out.pass, out.diff = false, diff
			return out, nil
		}
	}
	return out, nil
}

package mcpserver

import (
	"context"
	"time"

	"github.com/rknightion/finstore/internal/store"
)

// storeReader is the part of *store.Reader the adapter calls. The real reader satisfies it; a test
// puts a fake behind the adapter to reach errors the real one only gives after a 30 second wait.
type storeReader interface {
	Overview(ctx context.Context) (store.Overview, error)
	SearchTransactions(ctx context.Context, p store.SearchParams) ([]store.TransactionRow, bool, error)
	SpendingSummary(ctx context.Context, p store.SummaryParams) ([]store.SummaryGroup, bool, error)
	DescribeSchema(ctx context.Context, schema string) ([]store.Relation, error)
	Query(ctx context.Context, sql string, limits store.Limits) (store.Result, error)
}

// FromStore returns the Reader the tools read through, backed by the store's MCP reader. It maps the
// store's row types onto the tools' output types (the MCP schema) and nothing else: the reader's
// errors pass through unchanged, because store.Outcome classifies them, and the reader enforces its
// own limits (the Limits in the params say what the tool asked for, and the byte caps are the
// store's).
func FromStore(r *store.Reader) Reader { return adapter{r} }

type adapter struct{ r storeReader }

func (a adapter) Overview(ctx context.Context) (Overview, error) {
	o, err := a.r.Overview(ctx)
	if err != nil {
		return Overview{}, err
	}
	out := Overview{Accounts: make([]Account, len(o.Accounts)), Sources: make([]SyncSource, len(o.Sources))}
	for i, acc := range o.Accounts {
		gaps := make([]Gap, len(acc.Gaps))
		for j, g := range acc.Gaps {
			gaps[j] = Gap{From: g.From, To: g.To, Reason: g.Reason, DiffMinor: g.DiffMinor}
		}
		out.Accounts[i] = Account{
			AccountSlug: acc.AccountSlug, Source: acc.Source, Feed: acc.Feed, Type: acc.Type, Name: acc.Name,
			Currency: acc.Currency, Closed: acc.Closed, EndedOn: acc.EndedOn, EarliestDate: acc.EarliestDate,
			LatestDate: acc.LatestDate, TransactionCount: acc.TransactionCount,
			ReconciliationState: acc.ReconciliationState, ReconciledThrough: acc.ReconciledThrough,
			LastDiffMinor: acc.LastDiffMinor, Gaps: gaps, LatestStatementDate: acc.LatestStatementDate,
			FailedOn: acc.FailedOn,
		}
	}
	for i, s := range o.Sources {
		var days *int64
		if s.DaysToExpiry != nil {
			d := int64(*s.DaysToExpiry)
			days = &d
		}
		var imports *map[string]*string
		if s.LastImportAt != nil {
			m := make(map[string]*string, len(s.LastImportAt))
			for slug, t := range s.LastImportAt {
				m[slug] = rfc3339(t)
			}
			imports = &m
		}
		out.Sources[i] = SyncSource{
			Source: s.Source, ConsentState: s.ConsentState, ApprovedAt: rfc3339(s.ApprovedAt),
			ExpectedExpiryAt: rfc3339(s.ExpectedExpiryAt), DaysToExpiry: days, LastSyncOKAt: rfc3339(s.LastSyncOKAt),
			LastRunOutcome: s.LastRunOutcome, LastImportAt: imports,
		}
	}
	return out, nil
}

func (a adapter) SearchTransactions(ctx context.Context, p SearchParams) ([]Transaction, bool, error) {
	rows, truncated, err := a.r.SearchTransactions(ctx, store.SearchParams{
		Accounts: p.Accounts, From: day(p.From), To: day(p.To), Text: p.Text, Category: p.Category,
		Direction: p.Direction, MinAmount: p.MinAmount, MaxAmount: p.MaxAmount,
		IncludeDetail: p.IncludeDetail, Limit: p.Limits.MaxRows,
	})
	if err != nil {
		return nil, false, err
	}
	out := make([]Transaction, len(rows))
	for i, r := range rows {
		out[i] = Transaction{
			ID: r.ID, AccountSlug: r.AccountSlug, TxnDate: r.TxnDate, CreatedAt: rfc3339(r.CreatedAt),
			AmountMinor: r.AmountMinor, Currency: r.Currency, Status: r.Status, Description: r.Description,
			Counterparty: r.Counterparty, MerchantName: r.MerchantName, Category: r.Category, Notes: r.Notes,
			PotSlug: r.PotSlug, Detail: r.Detail,
		}
	}
	return out, truncated, nil
}

func (a adapter) SpendingSummary(ctx context.Context, p SummaryParams) ([]SummaryGroup, bool, error) {
	exclude := p.ExcludePotTransfers
	rows, truncated, err := a.r.SpendingSummary(ctx, store.SummaryParams{
		GroupBy: p.GroupBy, From: p.From.Format(time.DateOnly), To: p.To.Format(time.DateOnly),
		Accounts: p.Accounts, Direction: p.Direction, ExcludePotTransfers: &exclude, Limit: p.Limits.MaxRows,
	})
	if err != nil {
		return nil, false, err
	}
	out := make([]SummaryGroup, len(rows))
	for i, g := range rows {
		out[i] = SummaryGroup{Key: g.Key, Currency: g.Currency, TotalMinor: g.TotalMinor, Count: g.Count}
	}
	return out, truncated, nil
}

func (a adapter) DescribeSchema(ctx context.Context, schema string) ([]Relation, error) {
	rels, err := a.r.DescribeSchema(ctx, schema)
	if err != nil {
		return nil, err
	}
	out := make([]Relation, len(rels))
	for i, rel := range rels {
		cols := make([]Column, len(rel.Columns))
		for j, c := range rel.Columns {
			cols[j] = Column{Name: c.Name, Type: c.Type, Nullable: c.Nullable}
		}
		var comment *string
		if rel.Comment != "" {
			c := rel.Comment
			comment = &c
		}
		out[i] = Relation{Schema: rel.Schema, Name: rel.Name, Kind: rel.Kind, Comment: comment, Columns: cols}
	}
	return out, nil
}

func (a adapter) Query(ctx context.Context, sql string, limits Limits) (Result, error) {
	res, err := a.r.Query(ctx, sql, store.Limits{MaxRows: limits.MaxRows, MaxBytes: limits.MaxBytes})
	if err != nil {
		return Result{}, err
	}
	return Result{Columns: res.Columns, Rows: res.Rows, Truncated: res.Truncated, Elapsed: res.Elapsed}, nil
}

// day formats an optional date the way the store takes it: YYYY-MM-DD, empty for none.
func day(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.DateOnly)
}

// rfc3339 formats an instant in UTC, nil for nil.
func rfc3339(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

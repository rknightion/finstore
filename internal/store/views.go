package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Reconciliation states as agent.coverage reports them.
const (
	StateReconciled   = "reconciled"
	StateMismatch     = "mismatch"
	StateBalancesOnly = "balances_only"
	StateNotChecked   = "not_checked"
)

// Gap reasons as agent.coverage reports them.
const (
	GapBackfillUnfinished = "the Monzo window closed before this range was fetched"
	GapNoCompletedFetch   = "no completed fetch covers this period"
	GapNoFile             = "no file covers these days"
	GapImportDue          = "import due"
)

// Gap is one entry of agent.coverage's gaps. Dates are midnight UTC values of calendar days.
type Gap struct {
	// From is the first uncovered day; for GapNoFile, the last date of the earlier file; nil for
	// GapImportDue on an account with no statement yet.
	From *time.Time `json:"from"`
	// To is the last uncovered day; for GapNoFile, the first date of the later file.
	To        time.Time `json:"to"`
	Reason    string    `json:"reason"`
	DiffMinor *int64    `json:"diff_minor,omitempty"` // GapNoFile only: the balance difference
}

// CoverageRow is a row of agent.coverage.
type CoverageRow struct {
	AccountSlug         string     `json:"account_slug"`
	Feed                string     `json:"feed"`
	EarliestDate        *time.Time `json:"earliest_date"`
	LatestDate          *time.Time `json:"latest_date"`
	TransactionCount    int64      `json:"transaction_count"`
	ReconciliationState string     `json:"reconciliation_state"`
	ReconciledThrough   *time.Time `json:"reconciled_through"`
	LastDiffMinor       *int64     `json:"last_diff_minor"`
	Gaps                []Gap      `json:"gaps"`
}

// SyncStatusRow is a row of agent.sync_status.
type SyncStatusRow struct {
	Source           string                `json:"source"`
	ConsentState     *string               `json:"consent_state"`
	ApprovedAt       *time.Time            `json:"approved_at"`
	ExpectedExpiryAt *time.Time            `json:"expected_expiry_at"`
	DaysToExpiry     *int                  `json:"days_to_expiry"`
	LastSyncOKAt     *time.Time            `json:"last_sync_ok_at"`
	LastRunOutcome   *string               `json:"last_run_outcome"`
	LastImportAt     map[string]*time.Time `json:"last_import_at"` // slug to time; nil when the source has no file-fed account
}

// Coverage reads agent.coverage, ordered by account slug.
func Coverage(ctx context.Context, q DBTX) ([]CoverageRow, error) {
	rows, err := q.Query(ctx, `SELECT account_slug, feed, earliest_date, latest_date, transaction_count,
		reconciliation_state, reconciled_through, last_diff_minor, gaps
		FROM agent.coverage ORDER BY account_slug`)
	if err != nil {
		return nil, fmt.Errorf("store: read coverage: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CoverageRow, error) {
		var c CoverageRow
		var gaps []byte
		if err := r.Scan(&c.AccountSlug, &c.Feed, &c.EarliestDate, &c.LatestDate, &c.TransactionCount,
			&c.ReconciliationState, &c.ReconciledThrough, &c.LastDiffMinor, &gaps); err != nil {
			return c, err
		}
		var err error
		c.Gaps, err = decodeGaps(gaps)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: read coverage: %w", err)
	}
	return out, nil
}

func decodeGaps(b []byte) ([]Gap, error) {
	var raw []struct {
		From      *string `json:"from"`
		To        string  `json:"to"`
		Reason    string  `json:"reason"`
		DiffMinor *int64  `json:"diff_minor"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("decode gaps: %w", err)
	}
	gaps := make([]Gap, 0, len(raw))
	for _, g := range raw {
		to, err := time.Parse(time.DateOnly, g.To)
		if err != nil {
			return nil, fmt.Errorf("decode gap end: %w", err)
		}
		out := Gap{To: to, Reason: g.Reason, DiffMinor: g.DiffMinor}
		if g.From != nil {
			from, err := time.Parse(time.DateOnly, *g.From)
			if err != nil {
				return nil, fmt.Errorf("decode gap start: %w", err)
			}
			out.From = &from
		}
		gaps = append(gaps, out)
	}
	return gaps, nil
}

// SyncStatus reads agent.sync_status, ordered by source.
func SyncStatus(ctx context.Context, q DBTX) ([]SyncStatusRow, error) {
	rows, err := q.Query(ctx, `SELECT source, consent_state, approved_at, expected_expiry_at, days_to_expiry,
		last_sync_ok_at, last_run_outcome, last_import_at FROM agent.sync_status ORDER BY source`)
	if err != nil {
		return nil, fmt.Errorf("store: read sync status: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (SyncStatusRow, error) {
		var s SyncStatusRow
		var imports []byte
		if err := r.Scan(&s.Source, &s.ConsentState, &s.ApprovedAt, &s.ExpectedExpiryAt, &s.DaysToExpiry,
			&s.LastSyncOKAt, &s.LastRunOutcome, &imports); err != nil {
			return s, err
		}
		if imports != nil {
			if err := json.Unmarshal(imports, &s.LastImportAt); err != nil {
				return s, fmt.Errorf("decode last_import_at: %w", err)
			}
		}
		return s, nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: read sync status: %w", err)
	}
	return out, nil
}

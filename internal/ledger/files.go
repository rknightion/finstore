package ledger

import (
	"context"
	"errors"
	"time"

	"github.com/rknightion/finstore/internal/store"
)

// importFile applies one imported statement to its file-fed account.
//
// The file is rejected, with nothing written, when its account is unknown or fed by an API, its
// format has no normaliser, the normaliser refuses it (the statement fails its own checks), or
// it names a bank account (AccountRef) other than the one the account's earlier imports set.
//
// Otherwise its transactions are applied with source "import:<format>" (a row already stored from
// an overlapping statement has the same key, so the same id and hash, and is not stored again),
// one balance snapshot is stored per BalanceRec, one more for the closing balance on the
// statement's last date, and the statement itself as a core.statement row. The account's file
// gaps and reconciliation are then recomputed from all of its statements (see continuity), so the
// result depends on the set of statements held and not on the order they were imported in.
func (a *applier) importFile(ctx context.Context, f *store.ImportFile) error {
	acc, err := a.core.GetAccountBySlug(ctx, f.AccountSlug)
	if errors.Is(err, store.ErrNotFound) {
		return rejected("unknown_account")
	}
	if err != nil {
		return err
	}
	if acc.Feed != feedFile {
		return rejected("api_account")
	}
	n, ok := a.l.files[f.Format]
	if !ok {
		return rejected("unknown_format")
	}
	fr, err := n.NormaliseFile(File{Seq: f.Seq, AccountSlug: f.AccountSlug, Format: f.Format, Body: f.Body})
	if err != nil {
		return rejected("check_failed")
	}
	fr = sanitiseFileResult(fr)
	if fr.FirstDate.IsZero() || fr.LastDate.IsZero() || fr.LastDate.Before(fr.FirstDate) {
		return rejected("bad_result")
	}
	if fr.AccountRef != "" {
		switch {
		case acc.FileRef == nil:
			ref := fr.AccountRef
			acc.FileRef = &ref
			if err := a.core.UpsertAccount(ctx, acc); err != nil {
				return err
			}
		case *acc.FileRef != fr.AccountRef:
			return rejected("account_mismatch")
		}
	}
	source := importSourcePrefix + f.Format

	if err := a.core.EnsureReconciliation(ctx, acc.ID, methodFileContinuity); err != nil {
		return err
	}
	for _, t := range fr.Txns {
		if err := a.applyTxn(ctx, acc, source, t, f.ImportedAt); err != nil {
			return err
		}
	}
	for i, b := range fr.Balances {
		if err := a.insertBalance(ctx, acc, i, b); err != nil {
			return err
		}
	}
	closing := BalanceRec{OnDate: fr.LastDate, BalanceMinor: fr.ClosingMinor}
	if err := a.insertBalance(ctx, acc, len(fr.Balances), closing); err != nil {
		return err
	}
	if err := a.core.InsertStatement(ctx, store.Statement{AccountID: acc.ID, RawSeq: a.row.Seq, FirstDate: fr.FirstDate,
		LastDate: fr.LastDate, OpeningMinor: fr.OpeningMinor, ClosingMinor: fr.ClosingMinor, ExactPeriod: fr.PeriodExact}); err != nil {
		return err
	}
	return a.recomputeContinuity(ctx, acc, f.ImportedAt)
}

// recomputeContinuity rebuilds the account's file gaps and file reconciliation state from all of
// its statements.
//
// Statements are walked in order of first date, then last date descending, then raw seq, keeping
// reach: the last date covered so far, the balance on it, and whether the statement that set it
// has an exact (printed) period. A statement starting after reach does not overlap what came
// before; a gap is recorded between the two when its opening balance differs from reach's
// balance, or when both it and the statement that set reach have exact periods and its first
// date is later than the day after reach's date, so that a skipped month whose money in equals
// its money out is still found, with a difference of 0 (after_date reach's date, before_date its first date, diff opening
// minus reach's balance, raw_seq its own). A statement starting on or before reach's date
// overlaps or joins on the same day and is not checked. Either way reach moves to the
// statement's last date and closing balance when that is later than reach.
//
// break_count is the number of gaps. reconciled_through is the last date reached when there is
// no gap, else the after_date of the earliest gap. The check fields follow the gaps:
// last_check_at is the import time of the file being applied, last_check_ok whether there is no
// gap, last_diff_minor the earliest gap's difference (0 with none), and last_pass_at moves to
// last_check_at on a pass.
func (a *applier) recomputeContinuity(ctx context.Context, acc store.Account, at time.Time) error {
	stmts, err := a.core.ListStatements(ctx, acc.ID)
	if err != nil {
		return err
	}
	gaps, through := continuity(stmts)
	if err := a.core.ReplaceFileGaps(ctx, acc.ID, gaps); err != nil {
		return err
	}
	rec, err := a.core.GetReconciliation(ctx, acc.ID)
	if err != nil {
		return err
	}
	var diff int64
	if len(gaps) > 0 {
		diff = gaps[0].DiffMinor
	}
	ok := len(gaps) == 0
	rec.BreakCount = len(gaps)
	rec.ReconciledThrough = &through
	rec.LastCheckAt = &at
	rec.LastCheckOK = &ok
	rec.LastDiffMinor = &diff
	if ok {
		rec.LastPassAt = &at
	}
	return a.core.PutReconciliation(ctx, rec)
}

// continuity walks statements, which must be in ListStatements order and not empty, and returns
// the gaps in walk order and the reconciled-through date.
func continuity(stmts []store.Statement) ([]store.FileGap, time.Time) {
	var gaps []store.FileGap
	var reachDate time.Time
	var reachBalance int64
	var reachExact bool
	for i, s := range stmts {
		switch {
		case i == 0:
			reachDate, reachBalance, reachExact = s.LastDate, s.ClosingMinor, s.ExactPeriod
		case s.FirstDate.After(reachDate):
			apart := s.ExactPeriod && reachExact && s.FirstDate.After(reachDate.AddDate(0, 0, 1))
			if s.OpeningMinor != reachBalance || apart {
				gaps = append(gaps, store.FileGap{AccountID: s.AccountID, AfterDate: reachDate, BeforeDate: s.FirstDate,
					DiffMinor: s.OpeningMinor - reachBalance, RawSeq: s.RawSeq})
			}
			reachDate, reachBalance, reachExact = s.LastDate, s.ClosingMinor, s.ExactPeriod
		case s.LastDate.After(reachDate):
			reachDate, reachBalance, reachExact = s.LastDate, s.ClosingMinor, s.ExactPeriod
		}
	}
	if len(gaps) > 0 {
		return gaps, gaps[0].AfterDate
	}
	return nil, reachDate
}

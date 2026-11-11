package monzo

import (
	"context"
	"time"
)

// pageResult is what paging one account's period did.
type pageResult struct {
	// complete is true when a page that arrived in time came back short of PageSize.
	complete bool
	// completedAt is when the last page arrived, set with complete.
	completedAt time.Time
	// pages counts pages that arrived in time; rows counts distinct transaction ids among them.
	pages, rows int
	// late is true when a page arrived at or after the deadline and was discarded, or when the
	// deadline had passed before the next page was asked for.
	late bool
	// idStep is true when paging stepped past a full page of one timestamp by id. The bank skips
	// the rest of that timestamp's rows on such a step, so the listing is known to be incomplete
	// and must not be completed.
	idStep bool
}

// pageTransactions pages through accountID's transactions from cursor to before, recording every
// response under fetchID.
//
// Each page asks for rows from the created time of the last row of the page before (inclusive)
// and rows already seen are dropped by id. When a full page ends on the cursor's own timestamp,
// the timestamp cannot move, so the next step asks for rows after the last row's id instead, for
// that one step. cursorTime is the time cursor stands for, or the zero time when cursor is an id
// whose time is not known.
//
// With a non-zero deadline, a page that arrives at or after it is not progress: progress is not
// called and paging stops with late set. progress is called with the next cursor after every
// page that counts, before the next request. A cursor that would repeat one already used ends
// paging with an error instead of looping.
func (w *Worker) pageTransactions(ctx context.Context, fetchID int64, accountID, cursor string, cursorTime, before, deadline time.Time,
	progress func(ctx context.Context, next string) error,
) (pageResult, error) {
	var res pageResult
	seen := make(map[string]struct{})
	used := map[string]struct{}{cursor: {}}
	ctx = withFetch(ctx, fetchID)
	for {
		if !deadline.IsZero() && !w.now().Before(deadline) {
			res.late = true
			return res, nil
		}
		rows, err := w.api.transactionsPage(ctx, accountID, cursor, before)
		arrived := w.now()
		if err != nil {
			return res, err
		}
		if !deadline.IsZero() && !arrived.Before(deadline) {
			res.late = true
			return res, nil
		}
		res.pages++
		for _, r := range rows {
			if _, ok := seen[r.ID]; !ok {
				seen[r.ID] = struct{}{}
				res.rows++
			}
		}
		if len(rows) < PageSize {
			res.complete, res.completedAt = true, arrived
			return res, nil
		}
		last := rows[len(rows)-1]
		next := formatTime(last.Created)
		if last.Created.Equal(cursorTime) {
			// Every row of a full page shares the cursor's timestamp: asking from it again would
			// return the same page.
			next = last.ID
			res.idStep = true
		}
		if _, ok := used[next]; ok {
			// The cursor would go back to where it has been: the bank is not paging as expected.
			// Stop rather than loop, since a scheduled run has no deadline.
			return res, errBody(pathTransactions, "paging made no progress")
		}
		used[next] = struct{}{}
		if progress != nil {
			if err := progress(ctx, next); err != nil {
				return res, err
			}
		}
		cursor, cursorTime = next, last.Created
	}
}

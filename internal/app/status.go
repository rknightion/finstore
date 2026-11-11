package app

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry"
)

// consentStates is every state the consent gauge reports, so a state the consent has left reads 0
// and not "no data".
var consentStates = []string{"awaiting_approval", "active", "lost", "expired", "replaced"}

// dumpStatus is the file deploy/finstore-dump.sh writes after a good dump.
type dumpStatus struct {
	FinishedAt time.Time `json:"finished_at"`
	Bytes      int64     `json:"bytes"`
}

// readDumpStatus returns the finish time of the last good dump, or false when the file is absent
// or unreadable: no dump yet is a state the "dump stale" alert reports, not an error here.
func readDumpStatus(path string) (time.Time, bool) {
	if path == "" {
		return time.Time{}, false
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the operator's configured path
	if err != nil {
		return time.Time{}, false
	}
	var s dumpStatus
	if err := json.Unmarshal(b, &s); err != nil || s.FinishedAt.IsZero() {
		return time.Time{}, false
	}
	return s.FinishedAt, true
}

// statusPublisher turns the database's state into gauges once a minute. Everything it sends is a
// state, a count or a time; the only label taken from data is an account's short name.
type statusPublisher struct {
	st       *store.Store
	m        *telemetry.Metrics
	log      *slog.Logger
	dumpFile string
	now      func() time.Time
}

func (p *statusPublisher) run(ctx context.Context) error {
	p.m.Up(ctx)
	if at, ok := readDumpStatus(p.dumpFile); ok {
		p.m.DumpSucceeded(ctx, at)
	} else if p.dumpFile != "" {
		if _, err := os.Stat(p.dumpFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
			p.log.WarnContext(ctx, "dump status file not readable", semconv.LogOutcome, "error")
		}
	}
	db := p.st.DB()
	if _, err := store.ExpireConnectAttempts(ctx, db, p.now()); err != nil {
		return err
	}
	sources, err := store.SyncStatus(ctx, db)
	if err != nil {
		return err
	}
	for _, s := range sources {
		if s.ConsentState == nil {
			continue
		}
		p.m.ConsentState(ctx, s.Source, *s.ConsentState, consentStates)
		if s.ExpectedExpiryAt != nil {
			p.m.ConsentExpiry(ctx, s.Source, *s.ExpectedExpiryAt)
		}
		if s.LastSyncOKAt != nil {
			p.m.SyncSucceeded(ctx, s.Source, *s.LastSyncOKAt)
		}
	}
	coverage, err := store.Coverage(ctx, db)
	if err != nil {
		return err
	}
	core, err := store.NewCore(db, store.SchemaCore)
	if err != nil {
		return err
	}
	accounts, err := core.ListAccounts(ctx)
	if err != nil {
		return err
	}
	// An ended account is continued by another and is never import due.
	expects := map[string]bool{}
	for _, a := range accounts {
		expects[a.Slug] = a.Feed != feedAPI && a.ExpectsImports && a.EndedOn == nil
	}
	for _, c := range coverage {
		if c.ReconciliationState == store.StateBalancesOnly {
			continue
		}
		p.m.ReconciliationMismatch(ctx, c.AccountSlug, c.ReconciliationState == store.StateMismatch)
		// The same date the view's "import due" rule uses, so the alert and the page agree.
		if expects[c.AccountSlug] && c.LatestStatementDate != nil {
			p.m.ImportLast(ctx, c.AccountSlug, *c.LatestStatementDate)
		}
	}
	return nil
}

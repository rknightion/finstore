package ledger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
)

// Ledger is the projection: it applies raw rows, in seq order, to a core schema. It is the only
// writer of core. Everything it writes is a function of raw rows with a seq no greater than the
// row being applied, so applying the same rows live or on replay gives the same core.
type Ledger struct {
	sources map[string]SourceNormaliser
	files   map[string]FileNormaliser
	loc     *time.Location
	log     *slog.Logger
}

// New returns a Ledger. sources is keyed by raw source name (for example "monzo"); files by import
// format (for example "halifax-pdf", "manual-csv"). loc is the zone a source normaliser uses to
// turn an instant into a day; nil means UTC. A nil log discards.
func New(sources map[string]SourceNormaliser, files map[string]FileNormaliser, loc *time.Location, log *slog.Logger) *Ledger {
	if loc == nil {
		loc = time.UTC
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Ledger{sources: maps.Clone(sources), files: maps.Clone(files), loc: loc, log: log}
}

// CatchUp normalises every raw row with seq greater than the schema's projection_state.last_seq,
// in ascending seq order, into the named core schema (store.SchemaCore or store.SchemaCoreNext),
// then reconciles, and advances last_seq. The caller holds the writer lock and passes the
// connection or transaction to write on. It is safe to call when there is nothing new.
//
// Reconciliation is not a separate pass: each balance reading and each imported file is checked
// against the running sum as the row is applied, which is what makes the result the same live and
// on replay. Each row's writes and the advance of last_seq commit together (in a savepoint when q
// is a transaction), so a crash resumes after the last fully applied row. A row that cannot be
// understood is counted (Unparseable or FilesRejected), logged, and skipped with none of its
// writes; only a database error stops CatchUp.
//
// The projection records the ledger's time zone the first time it runs. CatchUp into a schema
// built with another zone returns an error wrapping ErrZoneChanged (and so
// store.ErrRenormaliseRequired) and changes nothing, because days already projected would be in
// the old zone and new ones in the new.
func (l *Ledger) CatchUp(ctx context.Context, q store.DBTX, schema string) (Stats, error) {
	core, err := store.NewCore(q, schema)
	if err != nil {
		return Stats{}, err
	}
	ps, err := core.GetProjectionState(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("ledger: read projection state: %w", err)
	}
	zone := l.loc.String()
	switch {
	case ps.Zone == nil:
		if err := core.SetZone(ctx, zone); err != nil {
			return Stats{}, err
		}
	case *ps.Zone != zone:
		return Stats{}, fmt.Errorf("%w (core %s, configured %s)", ErrZoneChanged, *ps.Zone, zone)
	}
	var stats Stats
	for row, err := range store.RawRowsAfter(ctx, q, ps.LastSeq) {
		if err != nil {
			return stats, err
		}
		rs, err := l.step(ctx, q, schema, row)
		if err != nil {
			return stats, fmt.Errorf("ledger: raw row %d (%s): %w", row.Seq, row.Kind, err)
		}
		stats.add(rs)
	}
	return stats, nil
}

// ErrZoneChanged is returned by CatchUp when the schema was built with another time zone than
// the ledger's. It wraps store.ErrRenormaliseRequired.
var ErrZoneChanged = fmt.Errorf("ledger: the configured time zone differs from the one core was built with: %w",
	store.ErrRenormaliseRequired)

// Renormalise rebuilds core from raw as the owner role. It first catches live core up (CatchUp on
// store.SchemaCore), so that rows stored without a catch-up are not reported as differences; a
// live core built with another time zone is left as it is, since it is about to be compared or
// replaced. Then store.CreateCoreNext, CatchUp into core_next from zero, and either
// store.SwapCore, or when verify is true store.DiffCore followed by store.DropCoreNext with the
// differences returned and nothing swapped.
func (l *Ledger) Renormalise(ctx context.Context, owner store.DBTX, verify bool) ([]store.CoreDiff, error) {
	if _, err := l.CatchUp(ctx, owner, store.SchemaCore); err != nil && !errors.Is(err, ErrZoneChanged) {
		return nil, fmt.Errorf("ledger: catch up live core: %w", err)
	}
	if err := store.CreateCoreNext(ctx, owner, NormaliserRev); err != nil {
		return nil, err
	}
	if _, err := l.CatchUp(ctx, owner, store.SchemaCoreNext); err != nil {
		if derr := store.DropCoreNext(context.WithoutCancel(ctx), owner); derr != nil {
			return nil, errors.Join(err, derr)
		}
		return nil, err
	}
	if verify {
		diffs, err := store.DiffCore(ctx, owner)
		if derr := store.DropCoreNext(ctx, owner); derr != nil {
			err = errors.Join(err, derr)
		}
		if err != nil {
			return nil, err
		}
		return diffs, nil
	}
	if err := store.SwapCore(ctx, owner, NormaliserRev); err != nil {
		return nil, err
	}
	return nil, nil
}

// Outcomes of a row that is skipped.
const (
	outcomeUnparseable = "unparseable"
	outcomeRejected    = "rejected"
)

// skip is returned by an apply function for a row that is counted and skipped: its writes are
// rolled back and last_seq still advances past it. category is a fixed word, never data.
type skip struct {
	outcome  string
	category string
}

func (s *skip) Error() string { return "ledger: row " + s.outcome + ": " + s.category }

func unparseable(category string) error {
	return &skip{outcome: outcomeUnparseable, category: category}
}
func rejected(category string) error { return &skip{outcome: outcomeRejected, category: category} }

// step applies one raw row and advances last_seq past it, in one transaction (a savepoint when q
// is a transaction). The row's own writes run in a nested savepoint so that a skipped row leaves
// nothing behind but the advance.
func (l *Ledger) step(ctx context.Context, q store.DBTX, schema string, row store.RawRow) (Stats, error) {
	var rs Stats
	var skipped *skip
	err := pgx.BeginFunc(ctx, q, func(tx pgx.Tx) error {
		rs, skipped = Stats{RowsProcessed: 1}, nil
		var applied Stats
		err := pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error {
			core, err := store.NewCore(sp, schema)
			if err != nil {
				return err
			}
			a := &applier{l: l, q: sp, core: core, row: row, stats: &applied}
			return a.apply(ctx)
		})
		switch {
		case errors.As(err, &skipped):
			// Any skip of an import file is a rejection of that file.
			if row.Kind == store.RawImportFile {
				skipped.outcome = outcomeRejected
			}
			if skipped.outcome == outcomeRejected {
				rs.FilesRejected++
				rs.Rejections = append(rs.Rejections, skipped.category)
			} else {
				rs.Unparseable++
			}
		case err != nil:
			return err
		default:
			rs.add(applied)
			rs.RowsProcessed = 1
		}
		core, err := store.NewCore(tx, schema)
		if err != nil {
			return err
		}
		return core.SetLastSeq(ctx, row.Seq)
	})
	if err != nil {
		return Stats{}, err
	}
	if skipped != nil {
		l.logSkip(ctx, row, skipped)
	}
	return rs, nil
}

// logSkip writes one line for a skipped row, with allow-listed fields only: never a body, an
// amount or text from the row.
func (l *Ledger) logSkip(ctx context.Context, row store.RawRow, s *skip) {
	attrs := []slog.Attr{
		slog.String(semconv.LogKind, string(row.Kind)),
		slog.String(semconv.LogOutcome, s.outcome),
		slog.String(semconv.LogErrorCategory, s.category),
	}
	switch {
	case row.APIResponse != nil:
		attrs = append(attrs,
			slog.String(semconv.LogSource, row.APIResponse.Source),
			slog.String(semconv.LogEndpoint, row.APIResponse.Endpoint),
			slog.Int(semconv.LogStatus, row.APIResponse.Status),
			slog.Int64(semconv.LogFetchID, row.APIResponse.FetchID))
	case row.FetchComplete != nil:
		attrs = append(attrs, slog.Int64(semconv.LogFetchID, row.FetchComplete.FetchID))
	case row.ImportFile != nil:
		attrs = append(attrs, slog.String(semconv.LogAccount, row.ImportFile.AccountSlug))
	}
	l.log.LogAttrs(ctx, slog.LevelWarn, "raw row skipped by the projection", attrs...)
}

func (s *Stats) add(o Stats) {
	s.RowsProcessed += o.RowsProcessed
	s.Unparseable += o.Unparseable
	s.AccountsCreated += o.AccountsCreated
	s.TransactionsCreated += o.TransactionsCreated
	s.TransactionsChanged += o.TransactionsChanged
	s.Disappeared += o.Disappeared
	s.Returned += o.Returned
	s.FilesRejected += o.FilesRejected
	s.Rejections = append(s.Rejections, o.Rejections...)
}

// applier applies one raw row on q, inside the row's savepoint.
type applier struct {
	l     *Ledger
	q     store.DBTX
	core  store.Core
	row   store.RawRow
	stats *Stats
}

func (a *applier) apply(ctx context.Context) error {
	switch a.row.Kind {
	case store.RawFetch:
		return nil
	case store.RawAPIResponse:
		return a.apiResponse(ctx, a.row.APIResponse)
	case store.RawFetchComplete:
		return a.fetchComplete(ctx, a.row.FetchComplete)
	case store.RawImportFile:
		return a.importFile(ctx, a.row.ImportFile)
	case store.RawOperatorEntry:
		return a.operatorEntry(ctx, a.row.OperatorEntry)
	}
	return fmt.Errorf("ledger: unknown raw kind %q", a.row.Kind)
}

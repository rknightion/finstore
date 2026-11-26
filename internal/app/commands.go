// Package app wires the packages into the commands of the finstore binary. Each command is a
// function that takes an Env and returns the process exit code, so the binary and the end-to-end
// test run the same code.
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/rknightion/finstore/internal/capture"
	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/importer"
	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/monzo"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry"
	"github.com/rknightion/finstore/internal/truelayer"
)

// Exit codes. Each command documents which it returns.
const (
	ExitOK         = 0
	ExitError      = 1
	ExitRejected   = 2 // import: the file failed a check
	ExitNoAccount  = 3 // import: the account is unknown or fed by an API
	ExitDifference = 4 // renormalise --verify: the rebuilt copy differs
)

// Import formats.
const (
	FormatHalifaxPDF = "halifax-pdf"
	FormatManualCSV  = "manual-csv"
)

// Operator entry kinds, as the ledger reads them from raw.operator_entry.
const (
	entryAccountAdd       = "account_add"
	entryAcceptDifference = "accept_difference"
	entryAccountEnd       = "account_end"
	entryAcceptGap        = "accept_gap"
)

const (
	sourceMonzo  = "monzo"
	feedAPI      = "api"
	runKindFile  = "import"
	maxImportLen = 32 << 20
	// rejectionRepeated is the run category for a file that was rejected before and is given again.
	rejectionRepeated = "rejected_before"
)

// investmentFormats is the registry of investment import formats: one line per format, the format
// name to its normaliser. An importer package adds its formats here and nowhere else; NewLedger
// registers them and `finstore import` accepts them.
var investmentFormats = map[string]ledger.InvestmentNormaliser{
	string(importer.FormatAvivaAnnualPDF):          importer.AvivaAnnualNormaliser{},
	string(importer.FormatAvivaCapture):            importer.AvivaCaptureNormaliser{},
	string(importer.FormatFidelityAnnualPDF):       importer.FidelityAnnualNormaliser{},
	string(importer.FormatFidelitySummaryCSV):      importer.FidelitySummaryNormaliser{},
	string(importer.FormatFidelityTradeNotePDF):    importer.FidelityTradeNoteNormaliser{},
	string(importer.FormatFidelityTransactionsCSV): importer.FidelityTransactionsNormaliser{},
	string(importer.FormatFreetradeActivityCSV):    importer.FreetradeActivityNormaliser{},
}

// rejectionText says in the operator's terms why the projection refused a file. The categories
// are the ledger's; none carries anything from the file. investment says the account is a platform
// or one of its children, whose files name no sort code.
func rejectionText(category string, investment bool) string {
	switch category {
	case "check_failed":
		if investment {
			return "the file could not be read as this format, or failed one of its checks."
		}
		return "the file could not be read as this format, or failed one of its checks (the rows, the totals and the balances must agree)."
	case "account_mismatch":
		if investment {
			return "this format or the accounts in the file do not belong to this account: an investment file imports only into a platform of its provider, and a provider account stays under its first platform."
		}
		return "the sort code and account number in the file are not the ones in the statements already imported into this account."
	case "child_key_shared":
		return "an account in the file would take a slug another account already has."
	case "unknown_format":
		return "this version does not know the format."
	default:
		return "the file was refused (" + category + ")."
	}
}

// Env is what a command runs with.
type Env struct {
	Cfg    config.Config
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Now    func() time.Time
	// Version is the build's version string, reported as service.version in telemetry.
	Version string
}

func (e Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// cliShutdownTimeout bounds how long a command waits to flush its telemetry, so a dead collector
// cannot hang it.
const cliShutdownTimeout = 5 * time.Second

// cliTelemetry is what a CLI command runs with: the providers, the instruments and a logger that
// writes to stderr and to OTLP.
type cliTelemetry struct {
	Providers *telemetry.Providers
	Metrics   *telemetry.Metrics
	Log       *slog.Logger
}

// withTelemetry runs fn as the CLI command named command (for example "account add"). It owns
// the command's telemetry: Setup under a per-invocation identity, a root span named
// "finstore <command>" that ends with the exit code, a logger whose records reach OTLP with the
// span's trace id, and a flush within cliShutdownTimeout. Telemetry never decides the outcome:
// settings that cannot export leave the command running with no telemetry, and one line on
// stderr that does not repeat the setting.
//
// Bootstrap, healthcheck, version and help never come through here. Bootstrap executes CREATE
// ROLE with a password literal, and the health check runs every 30 seconds.
func (e Env) withTelemetry(ctx context.Context, command string, fn func(ctx context.Context, t cliTelemetry) int) (code int) {
	id := telemetry.Identity{Role: telemetry.RoleCLI, Command: command, Version: e.Version, InstanceID: uuid.NewString()}
	providers, err := telemetry.Setup(ctx, e.Cfg, id)
	if err != nil {
		_, _ = fmt.Fprintln(e.Stderr, "finstore: telemetry disabled: the OTLP settings are not usable")
		off := e.Cfg
		off.OTLPEndpoint = ""
		if providers, err = telemetry.Setup(ctx, off, id); err != nil {
			return e.fail(err)
		}
	}
	metrics, err := telemetry.NewMetrics(providers.Meter)
	if err != nil {
		return e.fail(err)
	}
	ctx, span := providers.Tracer(semconv.ServiceName).Start(ctx, semconv.SpanCommandPrefix+command,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attribute.String(semconv.SpanCommand, command)))
	code = ExitError // what a panic leaves
	defer func() {
		span.SetAttributes(attribute.Int(semconv.SpanProcessExitCode, code))
		if code != ExitOK {
			telemetry.Fail(span, "error")
		}
		span.End()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cliShutdownTimeout)
		defer cancel()
		_ = providers.Shutdown(sctx)
	}()
	log := telemetry.NewLogger(e.Stderr, e.Cfg.LogLevel, providers.LogHandler)
	code = fn(ctx, cliTelemetry{Providers: providers, Metrics: metrics, Log: log})
	return code
}

func (e Env) fail(err error) int {
	_, _ = fmt.Fprintln(e.Stderr, "finstore:", err)
	return ExitError
}

// NewLedger builds the projection with every source and file format the binary knows. Both
// TrueLayer connections are registered whether or not TrueLayer is configured, so renormalise
// always reads their stored responses.
func NewLedger(loc *time.Location, log *slog.Logger, tr trace.Tracer) *ledger.Ledger {
	return ledger.New(
		map[string]ledger.SourceNormaliser{
			sourceMonzo:                            monzo.Normaliser{},
			truelayer.Source(truelayer.ConnLloyds): truelayer.Normaliser{Connection: truelayer.ConnLloyds},
			truelayer.Source(truelayer.ConnMonzo):  truelayer.Normaliser{Connection: truelayer.ConnMonzo},
		},
		map[string]ledger.FileNormaliser{
			FormatHalifaxPDF: importer.Normaliser{},
			FormatManualCSV:  importer.Normaliser{},
		},
		loc, log, tr, ledger.WithInvestmentNormalisers(investmentFormats))
}

// storeTelemetry makes a store opened by a command trace its queries into the command's span.
func storeTelemetry(t cliTelemetry) store.OpenOption {
	return store.WithTelemetry(t.Providers.TracerProvider, t.Providers.MeterProvider)
}

// observeLock reports the writer lock st holds through the process's held gauge.
func observeLock(t cliTelemetry, st *store.Store) error {
	return t.Metrics.ObserveWriterLock(st.CurrentHold)
}

// Values of the kind field on the line bootstrap logs when finstore_mcp had changed its own settings.
const (
	driftKindRoleSetting     = "role_setting"
	driftKindDatabaseSetting = "database_setting"
)

// Bootstrap creates the roles and the schemas as the superuser.
func Bootstrap(ctx context.Context, e Env) int {
	if err := e.Cfg.Require("bootstrap"); err != nil {
		return e.fail(err)
	}
	// Bootstrap exports no telemetry (see withTelemetry), so drift is reported as a log line on
	// stderr behind the same allow-list filter: counts and a kind, never the setting a login chose.
	log := telemetry.NewLogger(e.Stderr, e.Cfg.LogLevel)
	report := func(d store.MCPDrift) {
		for _, x := range []struct {
			kind string
			n    int
		}{{driftKindRoleSetting, d.Role}, {driftKindDatabaseSetting, d.Database}} {
			if x.n > 0 {
				log.WarnContext(ctx, "mcp role settings drifted and were reset", semconv.LogKind, x.kind, semconv.LogCount, x.n)
			}
		}
	}
	if err := store.Bootstrap(ctx, e.Cfg.SuperuserDatabaseURL, e.Cfg.OwnerPassword, e.Cfg.AppPassword,
		store.WithMCPDriftReport(report)); err != nil {
		return e.fail(err)
	}
	// The MCP login follows FINSTORE_MCP_PASSWORD: set turns it on with that password, empty turns it
	// off. No command requires the password.
	if err := store.EnableMCPLogin(ctx, e.Cfg.SuperuserDatabaseURL, e.Cfg.MCPPassword); err != nil {
		return e.fail(err)
	}
	_, _ = fmt.Fprintln(e.Stdout, "roles and schemas are in place")
	return ExitOK
}

// Migrate applies the migrations and the grants as the owner.
func Migrate(ctx context.Context, e Env) int {
	return e.withTelemetry(ctx, "migrate", func(ctx context.Context, t cliTelemetry) int { return migrate(ctx, e, t) })
}

func migrate(ctx context.Context, e Env, t cliTelemetry) int {
	if err := e.Cfg.Require("migrate"); err != nil {
		return e.fail(err)
	}
	err := store.Migrate(ctx, e.Cfg.OwnerDatabaseURL, ledger.NormaliserRev, storeTelemetry(t))
	if errors.Is(err, store.ErrRenormaliseRequired) {
		_, _ = fmt.Fprintln(e.Stdout, "migrated; this version reads stored data differently: run `finstore renormalise`")
		return ExitOK
	}
	if err != nil {
		return e.fail(err)
	}
	_, _ = fmt.Fprintln(e.Stdout, "migrated")
	return ExitOK
}

// Renormalise rebuilds core from raw as the owner. With verify it changes nothing and returns
// ExitDifference when the rebuilt copy differs from the live one.
func Renormalise(ctx context.Context, e Env, verify bool) int {
	return e.withTelemetry(ctx, "renormalise", func(ctx context.Context, t cliTelemetry) int {
		return renormalise(ctx, e, t, verify)
	})
}

func renormalise(ctx context.Context, e Env, t cliTelemetry, verify bool) int {
	if err := e.Cfg.Require("renormalise"); err != nil {
		return e.fail(err)
	}
	// Renormalise replays every raw row: no query spans, which would number in the hundreds of
	// thousands. The batch steps and the writer lock are still spans.
	st, err := store.Open(ctx, e.Cfg.OwnerDatabaseURL)
	if err != nil {
		return e.fail(err)
	}
	defer st.Close()
	if err := observeLock(t, st); err != nil {
		return e.fail(err)
	}
	l := NewLedger(e.Cfg.Timezone, t.Log, t.Providers.Tracer(semconv.ServiceName))
	var diffs []store.CoreDiff
	err = st.HoldWriterLock(ctx, store.HolderRenormalise, func(ctx context.Context, conn *pgx.Conn) error {
		var err error
		diffs, err = l.Renormalise(ctx, conn, verify)
		return err
	})
	if err != nil {
		return e.fail(err)
	}
	if !verify {
		_, _ = fmt.Fprintln(e.Stdout, "core rebuilt from raw")
		return ExitOK
	}
	if len(diffs) == 0 {
		_, _ = fmt.Fprintln(e.Stdout, "the rebuilt copy equals the live one")
		return ExitOK
	}
	w := tabwriter.NewWriter(e.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "TABLE\tKEY\tCOLUMN\tKIND")
	for _, d := range diffs {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", d.Table, d.Key, d.Column, d.Kind)
	}
	_ = w.Flush()
	_, _ = fmt.Fprintf(e.Stdout, "%d differences; nothing was changed\n", len(diffs))
	return ExitDifference
}

// AccountSpec describes a file-fed account to create.
type AccountSpec struct {
	Slug           string `json:"slug"`
	Type           string `json:"type"`
	Currency       string `json:"currency"`
	Name           string `json:"name"`
	ExpectsImports bool   `json:"expects_imports"`
	// Provider is required for type platform, and refused for any other type.
	Provider string `json:"provider,omitempty"`
}

// typePlatform is the account type that groups the accounts held at one investment provider.
const typePlatform = "platform"

var (
	slugRE       = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	currencyRE   = regexp.MustCompile(`^[A-Z]{3}$`)
	accountTypes = []string{"current", "joint", "savings", "mortgage", typePlatform}
	// platformProviders are the values of --provider.
	platformProviders = []string{ledger.ProviderFidelity, ledger.ProviderAviva, ledger.ProviderFreetrade}
)

func (s AccountSpec) validate() error {
	switch {
	case !slugRE.MatchString(s.Slug) || len(s.Slug) > 40:
		return errors.New("--slug must be lower-case letters, digits and single hyphens, at most 40 characters")
	case !slices.Contains(accountTypes, s.Type):
		return fmt.Errorf("--type must be one of %s", strings.Join(accountTypes, ", "))
	case !currencyRE.MatchString(s.Currency):
		return errors.New("--currency must be a three-letter code such as GBP")
	case strings.TrimSpace(s.Name) == "":
		return errors.New("--name is required")
	case s.Type == typePlatform && !slices.Contains(platformProviders, s.Provider):
		return fmt.Errorf("--type platform needs --provider, one of %s", strings.Join(platformProviders, ", "))
	case s.Type != typePlatform && s.Provider != "":
		return errors.New("--provider applies only to --type platform")
	}
	return nil
}

// withWriter opens the app store and runs fn under the writer lock in one transaction.
func withWriter(ctx context.Context, e Env, t cliTelemetry, command string, fn func(ctx context.Context, tx pgx.Tx, l *ledger.Ledger) error) error {
	if err := e.Cfg.Require(command); err != nil {
		return err
	}
	st, err := store.Open(ctx, e.Cfg.DatabaseURL, storeTelemetry(t))
	if err != nil {
		return err
	}
	defer st.Close()
	if err := observeLock(t, st); err != nil {
		return err
	}
	l := NewLedger(e.Cfg.Timezone, t.Log, t.Providers.Tracer(semconv.ServiceName))
	// The command word (account or import) is the lock's holder.
	return st.WithWriterLock(ctx, command, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, tx, l) })
}

// AccountAdd creates a file-fed account by writing an operator entry and normalising it.
func AccountAdd(ctx context.Context, e Env, spec AccountSpec) int {
	return e.withTelemetry(ctx, "account add", func(ctx context.Context, t cliTelemetry) int {
		return accountAdd(ctx, e, t, spec)
	})
}

func accountAdd(ctx context.Context, e Env, t cliTelemetry, spec AccountSpec) int {
	if err := spec.validate(); err != nil {
		return e.fail(err)
	}
	payload, err := json.Marshal(spec)
	if err != nil {
		return e.fail(err)
	}
	err = withWriter(ctx, e, t, "account", func(ctx context.Context, tx pgx.Tx, l *ledger.Ledger) error {
		core, err := store.NewCore(tx, store.SchemaCore)
		if err != nil {
			return err
		}
		if _, err := core.GetAccountBySlug(ctx, spec.Slug); err == nil {
			return fmt.Errorf("an account with the slug %q already exists", spec.Slug)
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if spec.Type == typePlatform {
			// A platform's id comes from its provider and slug, which a child's account number could equal.
			if _, err := core.GetAccountBySource(ctx, ledger.InvestmentSource(spec.Provider), spec.Slug); err == nil {
				return fmt.Errorf("the slug %q is the account number of an account of this provider", spec.Slug)
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		if _, err := store.InsertOperatorEntry(ctx, tx, store.OperatorEntry{
			Kind: entryAccountAdd, Payload: payload, EnteredAt: e.now(),
		}); err != nil {
			return err
		}
		_, err = l.CatchUp(ctx, tx, store.SchemaCore)
		return err
	})
	if err != nil {
		return e.fail(err)
	}
	_, _ = fmt.Fprintf(e.Stdout, "account %s created\n", spec.Slug)
	return ExitOK
}

// AccountList prints every account.
func AccountList(ctx context.Context, e Env) int {
	return e.withTelemetry(ctx, "account list", func(ctx context.Context, t cliTelemetry) int { return accountList(ctx, e, t) })
}

func accountList(ctx context.Context, e Env, t cliTelemetry) int {
	if err := e.Cfg.Require("account"); err != nil {
		return e.fail(err)
	}
	st, err := store.Open(ctx, e.Cfg.DatabaseURL, storeTelemetry(t))
	if err != nil {
		return e.fail(err)
	}
	defer st.Close()
	core, err := store.NewCore(st.DB(), store.SchemaCore)
	if err != nil {
		return e.fail(err)
	}
	accounts, err := core.ListAccounts(ctx)
	if err != nil {
		return e.fail(err)
	}
	w := tabwriter.NewWriter(e.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SLUG\tTYPE\tFEED\tCURRENCY\tNAME")
	for _, a := range accounts {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.Slug, a.Type, a.Feed, a.Currency, a.Name)
	}
	_ = w.Flush()
	return ExitOK
}

// AcceptDifference records that an account's current balance difference is known and accepted.
func AcceptDifference(ctx context.Context, e Env, slug, reason string) int {
	return e.withTelemetry(ctx, "account accept-difference", func(ctx context.Context, t cliTelemetry) int {
		return acceptDifference(ctx, e, t, slug, reason)
	})
}

func acceptDifference(ctx context.Context, e Env, t cliTelemetry, slug, reason string) int {
	if strings.TrimSpace(reason) == "" {
		return e.fail(errors.New("--reason is required"))
	}
	var diff int64
	err := withWriter(ctx, e, t, "account", func(ctx context.Context, tx pgx.Tx, l *ledger.Ledger) error {
		core, err := store.NewCore(tx, store.SchemaCore)
		if err != nil {
			return err
		}
		account, err := core.GetAccountBySlug(ctx, slug)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no account has the slug %q", slug)
		} else if err != nil {
			return err
		}
		if account.Feed != feedAPI {
			return fmt.Errorf("account %s is fed by statement files: a gap between two statements closes when the missing statement is imported, or with account accept-gap when the bank issued none", slug)
		}
		rec, err := core.GetReconciliation(ctx, account.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if rec.LastDiffMinor == nil || *rec.LastDiffMinor == 0 {
			return fmt.Errorf("account %s has no balance difference to accept", slug)
		}
		diff = *rec.LastDiffMinor
		payload, err := json.Marshal(struct {
			Slug      string `json:"slug"`
			DiffMinor int64  `json:"diff_minor"`
			Reason    string `json:"reason"`
		}{slug, diff, reason})
		if err != nil {
			return err
		}
		if _, err := store.InsertOperatorEntry(ctx, tx, store.OperatorEntry{
			Kind: entryAcceptDifference, Payload: payload, EnteredAt: e.now(),
		}); err != nil {
			return err
		}
		_, err = l.CatchUp(ctx, tx, store.SchemaCore)
		return err
	})
	if err != nil {
		return e.fail(err)
	}
	_, _ = fmt.Fprintf(e.Stdout, "account %s: a difference of %d minor units is recorded as accepted\n", slug, diff)
	return ExitOK
}

// AcceptGap records that the gap between two statements of a file-fed account, from after (the last
// date of the earlier statement) to before (the first date of the later one), as the coverage view
// shows it, is known and accepted: the bank issued no statement for it. It is refused, with nothing
// recorded, unless the account has that gap and the statements either side of it join (the
// earlier one's closing balance is the later one's opening balance).
func AcceptGap(ctx context.Context, e Env, slug string, after, before time.Time, reason string) int {
	return e.withTelemetry(ctx, "account accept-gap", func(ctx context.Context, t cliTelemetry) int {
		return acceptGap(ctx, e, t, slug, after, before, reason)
	})
}

func acceptGap(ctx context.Context, e Env, t cliTelemetry, slug string, after, before time.Time, reason string) int {
	if strings.TrimSpace(reason) == "" {
		return e.fail(errors.New("--reason is required"))
	}
	from, to := after.Format(time.DateOnly), before.Format(time.DateOnly)
	err := withWriter(ctx, e, t, "account", func(ctx context.Context, tx pgx.Tx, l *ledger.Ledger) error {
		core, err := store.NewCore(tx, store.SchemaCore)
		if err != nil {
			return err
		}
		account, err := core.GetAccountBySlug(ctx, slug)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no account has the slug %q", slug)
		} else if err != nil {
			return err
		}
		if account.Feed == feedAPI {
			return fmt.Errorf("account %s is fed by an API and has no statement gaps: use account accept-difference", slug)
		}
		gaps, err := core.ListFileGaps(ctx, account.ID)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(gaps, func(g store.FileGap) bool {
			return g.AfterDate.Format(time.DateOnly) == from && g.BeforeDate.Format(time.DateOnly) == to
		})
		if i < 0 {
			return fmt.Errorf("account %s has no gap from %s to %s: finstore coverage lists its gaps", slug, from, to)
		}
		if gaps[i].DiffMinor != 0 {
			return fmt.Errorf("the gap from %s to %s cannot be accepted: the closing balance before it differs from the opening balance after it, so money moved in between; import the missing statement", from, to)
		}
		payload, err := json.Marshal(ledger.AcceptGap{Slug: slug, AfterDate: from, BeforeDate: to, Reason: reason})
		if err != nil {
			return err
		}
		if _, err := store.InsertOperatorEntry(ctx, tx, store.OperatorEntry{
			Kind: entryAcceptGap, Payload: payload, EnteredAt: e.now(),
		}); err != nil {
			return err
		}
		_, err = l.CatchUp(ctx, tx, store.SchemaCore)
		return err
	})
	if err != nil {
		return e.fail(err)
	}
	_, _ = fmt.Fprintf(e.Stdout, "account %s: the gap from %s to %s is recorded as accepted\n", slug, from, to)
	return ExitOK
}

// AccountEnd records that an account has no transactions after lastDate (a calendar day), or with
// a nil lastDate clears its end, by writing an account_end operator entry and normalising it. The
// account's rows and readings after the date stay stored but leave the agent views.
func AccountEnd(ctx context.Context, e Env, slug string, lastDate *time.Time) int {
	return e.withTelemetry(ctx, "account end", func(ctx context.Context, t cliTelemetry) int {
		return accountEnd(ctx, e, t, slug, lastDate)
	})
}

func accountEnd(ctx context.Context, e Env, t cliTelemetry, slug string, lastDate *time.Time) int {
	var last *string
	if lastDate != nil {
		d := lastDate.Format(time.DateOnly)
		last = &d
	}
	payload, err := json.Marshal(ledger.AccountEnd{Slug: slug, LastDate: last})
	if err != nil {
		return e.fail(err)
	}
	err = withWriter(ctx, e, t, "account", func(ctx context.Context, tx pgx.Tx, l *ledger.Ledger) error {
		core, err := store.NewCore(tx, store.SchemaCore)
		if err != nil {
			return err
		}
		account, err := core.GetAccountBySlug(ctx, slug)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no account has the slug %q", slug)
		} else if err != nil {
			return err
		}
		if account.Type == typePlatform {
			return fmt.Errorf("account %s is a platform: end each of its accounts on its own", slug)
		}
		if _, err := store.InsertOperatorEntry(ctx, tx, store.OperatorEntry{
			Kind: entryAccountEnd, Payload: payload, EnteredAt: e.now(),
		}); err != nil {
			return err
		}
		_, err = l.CatchUp(ctx, tx, store.SchemaCore)
		return err
	})
	if err != nil {
		return e.fail(err)
	}
	if last == nil {
		_, _ = fmt.Fprintf(e.Stdout, "account %s: end cleared\n", slug)
	} else {
		_, _ = fmt.Fprintf(e.Stdout, "account %s ends on %s: its later rows are left out of the agent views\n", slug, *last)
	}
	return ExitOK
}

// Import reads one statement from standard input and imports it into the account.
func Import(ctx context.Context, e Env, slug, format, name string) int {
	return e.withTelemetry(ctx, "import", func(ctx context.Context, t cliTelemetry) int {
		return importStatement(ctx, e, t, slug, format, name)
	})
}

func importStatement(ctx context.Context, e Env, t cliTelemetry, slug, format, name string) int {
	if _, investment := investmentFormats[format]; !investment && format != FormatHalifaxPDF && format != FormatManualCSV {
		return e.fail(fmt.Errorf("--format must be one of %s", strings.Join(importFormats(), ", ")))
	}
	body, err := io.ReadAll(io.LimitReader(e.Stdin, maxImportLen+1))
	if err != nil {
		return e.fail(errors.New("the file could not be read from standard input"))
	}
	if len(body) == 0 {
		return e.fail(errors.New("standard input is empty: pipe the statement file in"))
	}
	if len(body) > maxImportLen {
		return e.fail(fmt.Errorf("the file is larger than %d MiB", maxImportLen>>20))
	}
	code := ExitOK
	var message string
	err = withWriter(ctx, e, t, "import", func(ctx context.Context, tx pgx.Tx, l *ledger.Ledger) error {
		core, err := store.NewCore(tx, store.SchemaCore)
		if err != nil {
			return err
		}
		account, err := core.GetAccountBySlug(ctx, slug)
		if errors.Is(err, store.ErrNotFound) {
			code, message = ExitNoAccount, fmt.Sprintf("no account has the slug %q; create it with `finstore account add`", slug)
			return nil
		} else if err != nil {
			return err
		}
		if account.Feed == feedAPI {
			code, message = ExitNoAccount, fmt.Sprintf("account %s is fed by an API and takes no imports", slug)
			return nil
		}
		// An investment format on a bank account is refused in the same provider-neutral words.
		_, investmentFormat := investmentFormats[format]
		investment := investmentFormat || strings.HasPrefix(account.Source, "investment:")
		// Normalise anything already waiting first, so the counts below belong to this file alone.
		if _, err := l.CatchUp(ctx, tx, store.SchemaCore); err != nil {
			return err
		}
		runID, err := store.StartRun(ctx, tx, account.Source, runKindFile, e.now())
		if err != nil {
			return err
		}
		seq, existed, err := store.InsertImportFile(ctx, tx, store.ImportFile{
			AccountSlug: slug, Format: format, FileName: name, ImportedAt: e.now(), Body: body,
		})
		if err != nil {
			return err
		}
		if existed {
			// The same bytes were stored before. Whether they were imported is another matter:
			// a file that was rejected then is still rejected now.
			imported, err := fileWasImported(ctx, core, account, seq)
			if err != nil {
				return err
			}
			result := store.RunResult{FinishedAt: e.now(), Outcome: "ok"}
			if imported {
				message = "already imported: this exact file was imported before"
			} else {
				category := rejectionRepeated
				result.Outcome, result.ErrorCategory = "failed", &category
				code = ExitRejected
				message = "rejected: this exact file was given before and rejected, so nothing was imported. " +
					"If the reader has been corrected since, `finstore renormalise` reads the stored file again."
			}
			return store.FinishRun(ctx, tx, runID, result)
		}
		stats, err := l.CatchUp(ctx, tx, store.SchemaCore)
		if err != nil {
			return err
		}
		result := store.RunResult{FinishedAt: e.now(), Outcome: "ok", RowsSeen: stats.TransactionsCreated}
		if stats.FilesRejected > 0 {
			category := "check_failed"
			if len(stats.Rejections) > 0 {
				category = stats.Rejections[len(stats.Rejections)-1]
			}
			result.Outcome, result.ErrorCategory = "failed", &category
			code = ExitRejected
			message = "rejected: " + rejectionText(category, investment) + " Nothing was imported. The file itself is kept."
		} else if investment {
			imp, err := core.GetInvestmentImport(ctx, seq)
			if err != nil {
				return err
			}
			result.RowsSeen = imp.RowCount
			message = fmt.Sprintf("imported: %d rows", imp.RowCount)
		} else {
			message = fmt.Sprintf("imported: %d new transactions, %d changed", stats.TransactionsCreated, stats.TransactionsChanged)
		}
		return store.FinishRun(ctx, tx, runID, result)
	})
	if err != nil {
		return e.fail(err)
	}
	out := e.Stdout
	if code != ExitOK {
		out = e.Stderr
	}
	_, _ = fmt.Fprintln(out, message)
	return code
}

// importFormats lists every format `finstore import` takes, in order.
func importFormats() []string {
	formats := append([]string{FormatHalifaxPDF, FormatManualCSV}, slices.Collect(maps.Keys(investmentFormats))...)
	slices.Sort(formats)
	return formats
}

// fileWasImported says whether the stored file with this seq was accepted, as opposed to rejected.
func fileWasImported(ctx context.Context, core store.Core, account store.Account, seq int64) (bool, error) {
	if strings.HasPrefix(account.Source, "investment:") {
		_, err := core.GetInvestmentImport(ctx, seq)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return err == nil, err
	}
	statements, err := core.ListStatements(ctx, account.ID)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(statements, func(s store.Statement) bool { return s.RawSeq == seq }), nil
}

// LoadCapture loads capture directories written before the service existed.
func LoadCapture(ctx context.Context, e Env, dirs []string) int {
	return e.withTelemetry(ctx, "load-capture", func(ctx context.Context, t cliTelemetry) int {
		return loadCapture(ctx, e, t, dirs)
	})
}

func loadCapture(ctx context.Context, e Env, t cliTelemetry, dirs []string) int {
	if err := e.Cfg.Require("load-capture"); err != nil {
		return e.fail(err)
	}
	if len(dirs) == 0 {
		return e.fail(errors.New("at least one --dir is required"))
	}
	st, err := store.Open(ctx, e.Cfg.DatabaseURL)
	if err != nil {
		return e.fail(err)
	}
	defer st.Close()
	if err := observeLock(t, st); err != nil {
		return e.fail(err)
	}
	// Like renormalise, a bulk load opens the store without query spans.
	tr := t.Providers.Tracer(semconv.ServiceName)
	res, err := capture.Load(ctx, st, NewLedger(e.Cfg.Timezone, t.Log, tr), dirs, e.now, tr)
	if err != nil {
		return e.fail(err)
	}
	_, _ = fmt.Fprintf(e.Stdout,
		"read %d responses from %d directories: %d loaded, %d already held, %d fetches complete\n",
		res.ResponsesRead, res.Directories, res.ResponsesLoaded, res.ResponsesSkipped, res.FetchesCompleted)
	p := res.Projection
	_, _ = fmt.Fprintf(e.Stdout,
		"normalised %d rows: %d accounts and %d transactions created, %d transactions changed, %d unparseable\n",
		p.RowsProcessed, p.AccountsCreated, p.TransactionsCreated, p.TransactionsChanged, p.Unparseable)
	return ExitOK
}

// Coverage prints the coverage view.
func Coverage(ctx context.Context, e Env, asJSON bool) int {
	return e.withTelemetry(ctx, "coverage", func(ctx context.Context, t cliTelemetry) int { return coverage(ctx, e, t, asJSON) })
}

func coverage(ctx context.Context, e Env, t cliTelemetry, asJSON bool) int {
	if err := e.Cfg.Require("coverage"); err != nil {
		return e.fail(err)
	}
	st, err := store.Open(ctx, e.Cfg.DatabaseURL, storeTelemetry(t))
	if err != nil {
		return e.fail(err)
	}
	defer st.Close()
	rows, err := store.Coverage(ctx, st.DB())
	if err != nil {
		return e.fail(err)
	}
	if asJSON {
		enc := json.NewEncoder(e.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			return e.fail(err)
		}
		return ExitOK
	}
	w := tabwriter.NewWriter(e.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ACCOUNT\tFEED\tEARLIEST\tLATEST\tTRANSACTIONS\tCHECK\tCHECKED THROUGH\tDIFFERENCE\tGAPS")
	for _, r := range rows {
		diff := "-"
		if r.LastDiffMinor != nil {
			diff = fmt.Sprint(*r.LastDiffMinor)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%d\n", r.AccountSlug, r.Feed, date(r.EarliestDate),
			date(r.LatestDate), r.TransactionCount, r.ReconciliationState, date(r.ReconciledThrough), diff, len(r.Gaps))
	}
	_ = w.Flush()
	for _, r := range rows {
		for _, g := range r.Gaps {
			_, _ = fmt.Fprintf(e.Stdout, "gap  %s  %s to %s: %s\n", r.AccountSlug, date(g.From), g.To.Format(time.DateOnly), g.Reason)
		}
	}
	return ExitOK
}

func date(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Format(time.DateOnly)
}

// Healthcheck asks the local server for /healthz. It is the container's health check, so it
// needs no database settings.
func Healthcheck(ctx context.Context, e Env) int {
	host, port, err := net.SplitHostPort(e.Cfg.ListenAddr)
	if err != nil {
		return e.fail(errors.New("FINSTORE_LISTEN_ADDR is not host:port"))
	}
	// A server bound to every address answers on loopback; one bound to a single address answers
	// only there.
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/healthz", nil)
	if err != nil {
		return e.fail(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return e.fail(errors.New("the server did not answer"))
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	if resp.StatusCode != http.StatusOK || !bytes.HasPrefix(body, []byte("ok")) {
		return e.fail(fmt.Errorf("the server answered %d", resp.StatusCode))
	}
	return ExitOK
}

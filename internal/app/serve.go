package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/monzo"
	"github.com/rknightion/finstore/internal/scheduler"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry"
	"github.com/rknightion/finstore/internal/web"
)

const (
	statusInterval  = time.Minute
	shutdownTimeout = 20 * time.Second
	// A scheduled run that has not finished in this long is abandoned; its pages are stored, no
	// completion row is written, and the next run starts afresh.
	hourlyRunLimit  = 15 * time.Minute
	nightlyRunLimit = 45 * time.Minute
)

// Hooks lets a test observe and adjust a running server. Every field is optional.
type Hooks struct {
	// Listening is called with the server's address once it accepts connections.
	Listening func(addr net.Addr)
	// Worker is called with the Monzo worker before the scheduler starts.
	Worker func(w *monzo.Worker)
	// MonzoOptions may change the worker's options before it is built.
	MonzoOptions func(o *monzo.Options)
	// NoSchedule leaves the scheduler's jobs unregistered, so a test drives every run itself.
	NoSchedule bool
}

// reader adapts the store to the pages' needs.
type reader struct{ st *store.Store }

func (r reader) Ping(ctx context.Context) error { return r.st.Ping(ctx) }

func (r reader) SyncStatus(ctx context.Context) ([]store.SyncStatusRow, error) {
	return store.SyncStatus(ctx, r.st.DB())
}

func (r reader) Coverage(ctx context.Context) ([]store.CoverageRow, error) {
	return store.Coverage(ctx, r.st.DB())
}

// Serve runs the HTTP server, the scheduler and the Monzo worker until ctx ends.
func Serve(ctx context.Context, e Env, hooks *Hooks) int {
	if err := serve(ctx, e, hooks); err != nil {
		return e.fail(err)
	}
	return ExitOK
}

func serve(ctx context.Context, e Env, hooks *Hooks) (err error) {
	if hooks == nil {
		hooks = &Hooks{}
	}
	cfg := e.Cfg
	if err := cfg.Require("serve"); err != nil {
		return err
	}
	providers, err := telemetry.Setup(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		_ = providers.Shutdown(sctx)
	}()
	log := telemetry.NewLogger(e.Stderr, cfg.LogLevel, providers.LogHandler)
	metrics, err := telemetry.NewMetrics(providers.Meter)
	if err != nil {
		return err
	}

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := checkProjection(ctx, st, cfg.Timezone); err != nil {
		return err
	}

	l := NewLedger(cfg.Timezone, log)
	opts := monzo.Options{
		ClientID: cfg.MonzoClientID, ClientSecret: cfg.MonzoClientSecret,
		APIURL: cfg.MonzoAPIURL, AuthURL: cfg.MonzoAuthURL, PublicURL: cfg.PublicURL,
		Window: cfg.MonzoWindow, Concurrency: cfg.MonzoBackfillConc,
		Location: cfg.Timezone, Now: e.Now,
	}
	if hooks.MonzoOptions != nil {
		hooks.MonzoOptions(&opts)
	}
	worker, err := monzo.New(opts, st, l, metrics, log)
	if err != nil {
		return err
	}
	defer worker.Close()
	if hooks.Worker != nil {
		hooks.Worker(worker)
	}

	pages, err := web.New(web.Options{
		AllowedEmails: cfg.AllowedEmails,
		AuthHeader:    cfg.AuthHeader,
		BankAuthURL:   cfg.MonzoAuthURL,
		IsRefusal:     func(err error) bool { return errors.Is(err, monzo.ErrCallbackRefused) },
		Location:      cfg.Timezone,
		Now:           e.now,
		Log:           log,
	}, worker, reader{st})
	if err != nil {
		return err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}
	srv := web.NewHTTPServer(cfg.ListenAddr, pages.Handler())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	if hooks.Listening != nil {
		hooks.Listening(ln.Addr())
	}

	// Pick up an approval wait or a full-history fetch that a restart interrupted.
	if err := worker.Resume(ctx); err != nil {
		log.ErrorContext(ctx, "monzo background work not resumed", semconv.LogOutcome, "error")
	}

	status := &statusPublisher{st: st, m: metrics, log: log, dumpFile: cfg.DumpStatusFile, now: e.now}
	sched := scheduler.New(e.now, log)
	if !hooks.NoSchedule {
		sched.Every("status", statusInterval, 0, status.run)
		// The jitter keeps a restart loop from hitting the bank at every start. Each run has a
		// deadline: a run holds the writer lock, and a bank that answers slowly enough could
		// otherwise keep it, and every import and command waiting behind it, for hours.
		bounded := func(kind string, limit time.Duration) scheduler.Job {
			return func(ctx context.Context) error {
				ctx, cancel := context.WithTimeout(ctx, limit)
				defer cancel()
				return worker.RunScheduled(ctx, kind)
			}
		}
		sched.Every(monzo.KindHourly, cfg.MonzoSyncInterval, min(time.Minute, cfg.MonzoSyncInterval/10),
			bounded(monzo.KindHourly, hourlyRunLimit))
		sched.Daily(monzo.KindNightly, cfg.MonzoNightlyAt.Hour, cfg.MonzoNightlyAt.Minute, cfg.Timezone,
			bounded(monzo.KindNightly, nightlyRunLimit))
	}
	schedErr := make(chan error, 1)
	schedCtx, stopSched := context.WithCancel(ctx)
	defer stopSched()
	go func() { schedErr <- sched.Run(schedCtx) }()

	log.InfoContext(ctx, "finstore serving", semconv.LogState, "started")
	select {
	case <-ctx.Done():
	case err = <-serveErr:
		// The listener failed; stop everything else and report it.
	}
	stopSched()
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	shutdownErr := srv.Shutdown(sctx)
	schedulerErr := <-schedErr
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	log.InfoContext(ctx, "finstore stopped", semconv.LogState, "stopped")
	if shutdownErr != nil {
		return fmt.Errorf("http server shutdown: %w", shutdownErr)
	}
	return schedulerErr
}

// checkProjection refuses to serve a database whose normalised record was built by code that
// read stored data differently: the operator must run migrate, and renormalise when it says so.
func checkProjection(ctx context.Context, st *store.Store, zone *time.Location) error {
	core, err := store.NewCore(st.DB(), store.SchemaCore)
	if err != nil {
		return err
	}
	state, err := core.GetProjectionState(ctx)
	if err != nil {
		return errors.New("the database is not set up for this version: run `finstore migrate`")
	}
	if state.NormaliserRev != ledger.NormaliserRev {
		return fmt.Errorf("the stored record was built by normaliser revision %d and this version is %d: run `finstore migrate`, then `finstore renormalise`",
			state.NormaliserRev, ledger.NormaliserRev)
	}
	// A transaction's day depends on the zone, so a record built in one zone cannot be added to
	// in another without mixing the two.
	if state.Zone != nil && *state.Zone != zone.String() {
		return fmt.Errorf("the stored record was built with the time zone %s and %s is %s: run `finstore renormalise`, or set the zone back",
			*state.Zone, config.EnvTimezone, zone)
	}
	return nil
}

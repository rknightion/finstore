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
	"github.com/rknightion/finstore/internal/truelayer"
	"github.com/rknightion/finstore/internal/web"
)

const (
	statusInterval  = time.Minute
	shutdownTimeout = 20 * time.Second
	// A scheduled run that has not finished in this long is abandoned; its pages are stored, no
	// completion row is written, and the next run starts afresh.
	hourlyRunLimit  = 15 * time.Minute
	nightlyRunLimit = 45 * time.Minute
	// A TrueLayer run has its own 15-minute deadline once it holds the writer lock; this bounds
	// the wait for the lock as well.
	trueLayerRunLimit = 2 * truelayer.DefaultRunTimeout
	// trueLayerJitter is the most a scheduled TrueLayer sync starts after its time.
	trueLayerJitter = 5 * time.Minute
)

// trueLayerSyncTimes are the times of day, in FINSTORE_TIMEZONE, of the scheduled TrueLayer syncs.
// A connect day spends three of the four calls a day each endpoint allows: the connect and these
// two.
var trueLayerSyncTimes = []config.TimeOfDay{{Hour: 6, Minute: 20}, {Hour: 18, Minute: 20}}

// trueLayerJob runs one scheduled sync of tw within trueLayerRunLimit.
func trueLayerJob(tw *truelayer.Worker) scheduler.Job {
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, trueLayerRunLimit)
		defer cancel()
		return tw.RunScheduled(ctx)
	}
}

// The status page's words for each connection.
const (
	monzoConnectNotice = "After you follow the link in Monzo's email, approve the request in the Monzo app " +
		"straight away: the full history can only be fetched in the first few minutes after the link."
	monzoReconnectNotice = "Reconnecting replaces the current connection as soon as you follow the link in " +
		"Monzo's email, before you approve anything. After that link, approve the request in the Monzo app " +
		"straight away: the full history can only be fetched in the first few minutes after the link."
	trueLayerReconnectNotice = "Reconnecting replaces the current connection when you come back from the bank, " +
		"and starts a new 90-day consent."
)

var (
	trueLayerLabels = map[string]string{
		truelayer.ConnLloyds: "Lloyds (TrueLayer)",
		truelayer.ConnMonzo:  "Monzo (TrueLayer)",
	}
	trueLayerNotices = map[string]string{
		truelayer.ConnLloyds: "You approve access at Lloyds through TrueLayer. The full history is fetched " +
			"as soon as you come back here.",
		truelayer.ConnMonzo: "You approve access in the Monzo app through TrueLayer. This connection reads " +
			"standing orders and direct debits, and lists cards. Monzo's own connection supplies everything else.",
	}
)

// Hooks lets a test observe and adjust a running server. Every field is optional.
type Hooks struct {
	// Listening is called with the server's address once it accepts connections.
	Listening func(addr net.Addr)
	// Worker is called with the Monzo worker before the scheduler starts.
	Worker func(w *monzo.Worker)
	// MonzoOptions may change the worker's options before it is built.
	MonzoOptions func(o *monzo.Options)
	// TrueLayerOptions may change each TrueLayer worker's options before it is built.
	TrueLayerOptions func(o *truelayer.Options)
	// TrueLayerWorker is called with each TrueLayer worker before the scheduler starts.
	TrueLayerWorker func(w *truelayer.Worker)
	// NoSchedule leaves the scheduler's jobs unregistered, so a test drives every run itself.
	NoSchedule bool
	// Scheduler is called with the scheduler once its jobs are registered, before it runs.
	Scheduler func(s *scheduler.Scheduler)
}

// reader adapts the store to the pages' needs.
type reader struct{ st *store.Store }

func (r reader) Ping(ctx context.Context) error { return r.st.Ping(ctx) }

func (r reader) ConnectAttemptSource(ctx context.Context, stateSHA256 []byte, now time.Time) (string, error) {
	return store.ConnectAttemptSource(ctx, r.st.DB(), stateSHA256, now)
}

func (r reader) SyncStatus(ctx context.Context) ([]store.SyncStatusRow, error) {
	return store.SyncStatus(ctx, r.st.DB())
}

func (r reader) Coverage(ctx context.Context) ([]store.CoverageRow, error) {
	return store.Coverage(ctx, r.st.DB())
}

// Serve runs the HTTP server, the scheduler, the Monzo worker and, when TrueLayer is configured,
// a worker for each TrueLayer connection until ctx ends.
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
	providers, err := telemetry.Setup(ctx, cfg, telemetry.Identity{
		Role: telemetry.RoleServe, Version: e.Version, InstanceID: telemetry.ServeInstanceID,
	})
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
	// A restart shows as a change in this gauge's value: serve's series keep their labels across
	// restarts, so there is no new series to see.
	metrics.ProcessStarted(ctx, e.now())

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
	sources := []web.Source{{
		Name: sourceMonzo, Label: "Monzo", ConnectPath: "/connect/monzo", CallbackPath: "/callback/monzo",
		AuthURL: cfg.MonzoAuthURL, Connector: worker,
		IsRefusal:       func(err error) bool { return errors.Is(err, monzo.ErrCallbackRefused) },
		ConnectNotice:   monzoConnectNotice,
		ReconnectNotice: monzoReconnectNotice,
	}}

	// The TrueLayer connections run only when TrueLayer is configured; their normalisers are
	// registered either way (NewLedger), so a renormalise always reads their stored responses.
	var tlWorkers []*truelayer.Worker
	if cfg.TrueLayerEnabled() {
		for _, conn := range []string{truelayer.ConnLloyds, truelayer.ConnMonzo} {
			o := truelayer.Options{
				Connection: conn, ClientID: cfg.TrueLayerClientID, ClientSecret: cfg.TrueLayerClientSecret,
				AuthURL: cfg.TrueLayerAuthURL, APIURL: cfg.TrueLayerAPIURL, PublicURL: cfg.PublicURL,
				Location: cfg.Timezone, Now: e.Now,
			}
			if hooks.TrueLayerOptions != nil {
				hooks.TrueLayerOptions(&o)
			}
			tw, err := truelayer.New(o, st, l, metrics, log)
			if err != nil {
				return err
			}
			defer tw.Close()
			if hooks.TrueLayerWorker != nil {
				hooks.TrueLayerWorker(tw)
			}
			tlWorkers = append(tlWorkers, tw)
			sources = append(sources, web.Source{
				Name: tw.Source(), Label: trueLayerLabels[conn],
				ConnectPath: "/connect/truelayer/" + conn, CallbackPath: "/callback/truelayer",
				AuthURL: cfg.TrueLayerAuthURL, Connector: tw,
				IsRefusal:       func(err error) bool { return errors.Is(err, truelayer.ErrCallbackRefused) },
				ConnectNotice:   trueLayerNotices[conn],
				ReconnectNotice: trueLayerReconnectNotice,
			})
		}
	}

	pages, err := web.New(web.Options{
		AllowedEmails: cfg.AllowedEmails,
		AuthHeader:    cfg.AuthHeader,
		Sources:       sources,
		Location:      cfg.Timezone,
		Now:           e.now,
		Log:           log,
		Tracer:        providers.Tracer(semconv.ServiceName),
	}, reader{st})
	if err != nil {
		return err
	}
	// Pick up an approval wait or a full-history fetch that a restart interrupted. Each worker's
	// Resume runs before the listener opens: a callback that arrived first could start a connect
	// run that Resume would then end as interrupted while it ran.
	if err := worker.Resume(ctx); err != nil {
		log.ErrorContext(ctx, "monzo background work not resumed", semconv.LogOutcome, "error")
	}
	// The same for each TrueLayer connection. A consent whose token is missing (only a database
	// restore does that) ends lost with token_missing and Resume says so with ErrConsentLost: the
	// service carries on, and the status page offers a reconnect.
	for _, tw := range tlWorkers {
		switch err := tw.Resume(ctx); {
		case errors.Is(err, truelayer.ErrConsentLost):
			log.WarnContext(ctx, "truelayer consent lost at start", semconv.LogSource, tw.Source(),
				semconv.LogOutcome, "lost")
		case err != nil:
			log.ErrorContext(ctx, "truelayer background work not resumed", semconv.LogSource, tw.Source(),
				semconv.LogOutcome, "error")
		}
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}
	srv := web.NewHTTPServer(cfg.ListenAddr, telemetry.HTTPHandler(pages.Handler(), providers))
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	if hooks.Listening != nil {
		hooks.Listening(ln.Addr())
	}

	status := &statusPublisher{st: st, m: metrics, log: log, dumpFile: cfg.DumpStatusFile, now: e.now}
	sched := scheduler.New(e.now, log, metrics.SchedulerObserver(providers.Tracer(semconv.ServiceName)))
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
		sched.Daily(monzo.KindNightly, cfg.MonzoNightlyAt.Hour, cfg.MonzoNightlyAt.Minute, cfg.Timezone, 0,
			bounded(monzo.KindNightly, nightlyRunLimit))
		for _, tw := range tlWorkers {
			for _, at := range trueLayerSyncTimes {
				sched.Daily(fmt.Sprintf("%s %02d:%02d", tw.Source(), at.Hour, at.Minute), at.Hour, at.Minute, cfg.Timezone, trueLayerJitter,
					trueLayerJob(tw))
			}
		}
	}
	if hooks.Scheduler != nil {
		hooks.Scheduler(sched)
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

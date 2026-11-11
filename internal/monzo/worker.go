// Package monzo is the Monzo source: OAuth with single-use refresh tokens, the time-boxed
// full-history fetch, the scheduled syncs, and the normaliser that turns stored Monzo responses
// into ledger records. It only reads: every request goes through internal/bankhttp, whose
// allow-list holds the token endpoint and six GET endpoints and nothing else.
package monzo

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/trace"

	"github.com/rknightion/finstore/internal/bankhttp"
	"github.com/rknightion/finstore/internal/consent"
	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry"
)

// Projector normalises the raw rows written so far. The ledger's CatchUp satisfies it.
type Projector interface {
	CatchUp(ctx context.Context, q store.DBTX, schema string) (ledger.Stats, error)
}

// Default option values.
const (
	DefaultWindow           = 240 * time.Second
	DefaultConcurrency      = 6
	DefaultApprovalFastPoll = 2 * time.Second
	DefaultApprovalSlowPoll = 60 * time.Second
	DefaultApprovalFastFor  = 5 * time.Minute
	DefaultApprovalGiveUp   = 24 * time.Hour
)

// Options configures a Worker. A zero duration or count takes its default.
type Options struct {
	ClientID, ClientSecret string
	// APIURL is the API base URL; AuthURL the page the owner is sent to; PublicURL this service's
	// public base URL, to which /callback/monzo is added for the redirect.
	APIURL, AuthURL, PublicURL string
	// Window is how long after the token is issued the full-history fetch may run.
	Window time.Duration
	// Concurrency is how many full-history ranges are fetched at once.
	Concurrency int
	// The approval wait polls every ApprovalFastPoll for ApprovalFastFor after the token was
	// issued, then every ApprovalSlowPoll, and ends the consent ApprovalGiveUp after issue.
	ApprovalFastPoll, ApprovalSlowPoll, ApprovalFastFor, ApprovalGiveUp time.Duration
	// Location is the zone transaction days are taken in. Nil means UTC.
	Location *time.Location
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Sleep pauses for d or until ctx ends, returning ctx.Err() then. It is used for the approval
	// poll and the bank client's retry pauses. Nil means a real timer.
	Sleep func(ctx context.Context, d time.Duration) error
	// Tracer makes the spans of bank calls; nil means none.
	Tracer trace.Tracer
}

// ErrCallbackRefused is returned by HandleCallback for a state value that is unknown, expired or
// already used. No code was exchanged and nothing changed.
var ErrCallbackRefused = consent.ErrAttemptRefused

// Worker runs the Monzo source.
type Worker struct {
	opts     Options
	st       *store.Store
	proj     Projector
	metrics  *telemetry.Metrics
	log      *slog.Logger
	consents *consent.Manager
	oauth    *oauthClient
	tokens   *tokens
	api      *api

	// backfilling counts full-history fetches in flight; a scheduled run never starts while it is
	// above zero.
	backfilling atomic.Int32

	bgCtx    context.Context
	bgCancel context.CancelFunc
	bg       sync.WaitGroup
	// mu guards cancelConnect, which cancels the approval wait or full-history fetch of the last
	// connect: a newer connect replaces its token, so its work is pointless.
	mu            sync.Mutex
	cancelConnect context.CancelFunc
}

// New returns a Worker. It sends nothing until it is used.
func New(opts Options, st *store.Store, proj Projector, m *telemetry.Metrics, log *slog.Logger) (*Worker, error) {
	switch {
	case opts.ClientID == "" || opts.ClientSecret == "":
		return nil, errors.New("monzo: client id and secret are required")
	case opts.APIURL == "" || opts.AuthURL == "" || opts.PublicURL == "":
		return nil, errors.New("monzo: API, auth and public URLs are required")
	case st == nil || proj == nil || m == nil:
		return nil, errors.New("monzo: store, projector and metrics are required")
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&opts.Window, DefaultWindow)
	def(&opts.ApprovalFastPoll, DefaultApprovalFastPoll)
	def(&opts.ApprovalSlowPoll, DefaultApprovalSlowPoll)
	def(&opts.ApprovalFastFor, DefaultApprovalFastFor)
	def(&opts.ApprovalGiveUp, DefaultApprovalGiveUp)
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.Location == nil {
		opts.Location = time.UTC
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepTimer
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if _, err := authoriseURL(opts.AuthURL, opts.ClientID, redirectURI(opts.PublicURL), "x"); err != nil {
		return nil, err
	}
	bank, err := bankhttp.New(opts.APIURL, bankhttp.MonzoRules(),
		bankhttp.WithRecorder(recorder{q: st.DB(), metrics: m}),
		bankhttp.WithClock(opts.Now), bankhttp.WithSleep(opts.Sleep),
		bankhttp.WithTelemetry(opts.Tracer, Source, m.BankObserver(Source)))
	if err != nil {
		return nil, err
	}
	w := &Worker{opts: opts, st: st, proj: proj, metrics: m, log: log}
	w.consents = consent.New(Source, opts.Now)
	w.oauth = &oauthClient{bank: bank, clientID: opts.ClientID, clientSecret: opts.ClientSecret,
		redirectURI: redirectURI(opts.PublicURL), metrics: m}
	w.tokens = &tokens{st: st, oauth: w.oauth, consents: w.consents, now: opts.Now,
		onLost: func(ctx context.Context, c store.Consent) {
			w.recordConsent(ctx, c)
			w.log.WarnContext(ctx, "monzo consent lost", semconv.LogState, c.State,
				semconv.LogErrorCategory, string(consent.ReasonRefreshRejected))
		}}
	w.api = &api{bank: bank, tokens: w.tokens, metrics: m}
	w.bgCtx, w.bgCancel = context.WithCancel(context.Background())
	return w, nil
}

func sleepTimer(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (w *Worker) now() time.Time { return w.opts.Now() }

// Normaliser returns the normaliser for stored Monzo responses.
func (w *Worker) Normaliser() ledger.SourceNormaliser { return Normaliser{} }

// AuthoriseURL starts a connect attempt and returns the Monzo page to send the owner to.
func (w *Worker) AuthoriseURL(ctx context.Context) (string, error) {
	state, err := w.consents.StartAttempt(ctx, w.st.DB())
	if err != nil {
		return "", err
	}
	return authoriseURL(w.opts.AuthURL, w.opts.ClientID, redirectURI(w.opts.PublicURL), state)
}

// HandleCallback consumes the attempt that state belongs to, exchanges code for a token pair and,
// in one transaction, stores the pair and records the new consent awaiting approval (replacing
// the previous one). The exchange and the store hold the token mutex, so they never interleave
// with a refresh, and run detached from ctx's cancellation (60 s timeout): once the bank has
// issued the pair the previous token is dead, so it must be stored. It then starts the approval wait and the full-history fetch in the
// background and returns. An unusable state returns ErrCallbackRefused without exchanging the
// code. A failed exchange gives the attempt the outcome refused and leaves the current consent in
// place.
func (w *Worker) HandleCallback(ctx context.Context, state, code string) error {
	q := w.st.DB()
	att, err := w.consents.ConsumeAttempt(ctx, q, state)
	if err != nil {
		return err
	}
	cons, err := w.tokens.exchange(ctx, code, att.ID)
	if err != nil {
		w.log.WarnContext(ctx, "monzo code exchange failed", semconv.LogErrorCategory, category(err))
		return err
	}
	w.recordConsent(ctx, cons)
	w.log.InfoContext(ctx, "monzo consent awaiting approval", semconv.LogState, cons.State)
	w.goApproval(cons.ID)
	return nil
}

// Resume restarts background work after a restart: the approval wait of a consent still awaiting
// approval, or the full-history fetch of an active consent whose window is still open (its
// deadline counts from the stored token issue time, so it is the same as before the restart).
func (w *Worker) Resume(ctx context.Context) error {
	cur, err := w.consents.Current(ctx, w.st.DB())
	if errors.Is(err, consent.ErrNoConsent) {
		return nil
	}
	if err != nil {
		return err
	}
	w.recordConsent(ctx, cur.Consent)
	if lost, err := w.loseIfTokenMissing(ctx, cur.Consent); err != nil || lost {
		return err
	}
	switch {
	case cur.State == store.ConsentAwaitingApproval:
		w.goApproval(cur.ID)
	case cur.State == store.ConsentActive && w.now().Before(cur.TokenIssuedAt.Add(w.opts.Window)):
		w.goConnect(func(ctx context.Context) { w.backfillLogged(ctx, cur.ID) })
	}
	return nil
}

// Consent returns the current consent with its days to expiry, or consent.ErrNoConsent.
func (w *Worker) Consent(ctx context.Context) (consent.Status, error) {
	return w.consents.Current(ctx, w.st.DB())
}

// Wait blocks until all background work has finished.
func (w *Worker) Wait() { w.bg.Wait() }

// Close cancels background work and waits for it to finish.
func (w *Worker) Close() {
	w.bgCancel()
	w.bg.Wait()
}

// goConnect runs fn in the background as the work of the latest connect, cancelling the work of
// the one before.
func (w *Worker) goConnect(fn func(ctx context.Context)) {
	ctx, cancel := context.WithCancel(w.bgCtx)
	w.mu.Lock()
	if w.cancelConnect != nil {
		w.cancelConnect()
	}
	w.cancelConnect = cancel
	w.mu.Unlock()
	w.bg.Add(1)
	go func() {
		defer w.bg.Done()
		defer cancel()
		fn(ctx)
	}()
}

func (w *Worker) goApproval(id int64) {
	w.goConnect(func(ctx context.Context) {
		if w.awaitApproval(ctx, id) {
			w.backfillLogged(ctx, id)
		}
	})
}

func (w *Worker) backfillLogged(ctx context.Context, id int64) {
	if err := w.runBackfill(ctx, id); err != nil {
		w.log.ErrorContext(ctx, "monzo full-history fetch failed", semconv.LogErrorCategory, category(err))
	}
}

// awaitApproval polls GET /accounts until it stops answering 403 forbidden.insufficient_permissions,
// then makes the consent active and returns true. The 403s are expected and never end the consent.
// It gives up, ending the consent as lost, ApprovalGiveUp after the token was issued. It returns
// false when the consent stops being the current one awaiting approval, or ctx ends.
func (w *Worker) awaitApproval(ctx context.Context, id int64) bool {
	var fetchID int64
	for {
		c, err := store.GetConsent(ctx, w.st.DB(), id)
		if err != nil || c.State != store.ConsentAwaitingApproval || !c.IsCurrent {
			return false
		}
		elapsed := w.now().Sub(c.TokenIssuedAt)
		if elapsed >= w.opts.ApprovalGiveUp {
			lost, err := w.consents.Lost(ctx, w.st.DB(), id, consent.ReasonApprovalTimeout)
			if err == nil {
				w.recordConsent(ctx, lost)
				w.log.WarnContext(ctx, "monzo consent lost", semconv.LogState, lost.State,
					semconv.LogErrorCategory, string(consent.ReasonApprovalTimeout))
			}
			return false
		}
		approved, err := w.pollApproval(ctx, &fetchID)
		if ctx.Err() != nil || errors.Is(err, ErrConsentLost) {
			return false
		}
		if approved {
			act, err := w.consents.Approved(ctx, w.st.DB(), id)
			if err != nil {
				w.log.ErrorContext(ctx, "monzo approval could not be recorded", semconv.LogErrorCategory, category(err))
				return false
			}
			w.recordConsent(ctx, act)
			w.log.InfoContext(ctx, "monzo consent approved", semconv.LogState, act.State)
			return true
		}
		interval := w.opts.ApprovalSlowPoll
		if elapsed < w.opts.ApprovalFastFor {
			interval = w.opts.ApprovalFastPoll
		}
		// The last pause ends at the give-up time, so the consent is lost exactly then.
		if left := c.TokenIssuedAt.Add(w.opts.ApprovalGiveUp).Sub(w.now()); left > 0 && left < interval {
			interval = left
		}
		if w.opts.Sleep(ctx, interval) != nil {
			return false
		}
	}
}

// pollApproval makes one GET /accounts under the writer lock, recorded under one meta fetch for
// the whole wait (with no sync.run: run_id 0). A 2xx answer is approval; any failure, the
// expected 403 forbidden.insufficient_permissions included, means poll again.
func (w *Worker) pollApproval(ctx context.Context, fetchID *int64) (bool, error) {
	var approved bool
	err := w.st.HoldWriterLock(ctx, store.HolderMonzoApproval, func(ctx context.Context, _ *pgx.Conn) error {
		if *fetchID == 0 {
			id, err := store.InsertFetch(ctx, w.st.DB(), store.Fetch{Source: Source, Kind: kindMeta, StartedAt: w.now()})
			if err != nil {
				return err
			}
			*fetchID = id
		}
		_, err := w.api.get(withFetch(ctx, *fetchID), pathAccounts, nil)
		approved = err == nil
		return err
	})
	return approved, err
}

// recordConsent exports a consent's state and expected expiry.
func (w *Worker) recordConsent(ctx context.Context, c store.Consent) {
	w.metrics.ConsentState(ctx, Source, c.State, consent.States())
	if c.ExpectedExpiryAt != nil {
		w.metrics.ConsentExpiry(ctx, Source, *c.ExpectedExpiryAt)
	}
}

// loseIfTokenMissing ends consent c as lost (token_missing) when it is awaiting approval or
// active and no token is stored, which only a database restore produces: a code exchange stores
// the pair and creates the consent in one transaction, under the token mutex. It reports whether
// it ended the consent, with ErrConsentLost.
func (w *Worker) loseIfTokenMissing(ctx context.Context, c store.Consent) (bool, error) {
	if c.State != store.ConsentAwaitingApproval && c.State != store.ConsentActive {
		return false, nil
	}
	_, err := store.GetOAuthToken(ctx, w.st.DB(), Source)
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	lost, err := w.consents.Lost(ctx, w.st.DB(), c.ID, consent.ReasonTokenMissing)
	if err != nil {
		return false, err
	}
	w.recordConsent(ctx, lost)
	w.log.WarnContext(ctx, "monzo consent lost", semconv.LogState, lost.State,
		semconv.LogErrorCategory, string(consent.ReasonTokenMissing))
	return true, ErrConsentLost
}

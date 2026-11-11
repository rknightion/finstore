// Package truelayer is the TrueLayer source: one connection per bank (ob-lloyds, ob-monzo), each
// its own finstore source with its own consent. It exchanges the code, approves the consent with
// the expiry TrueLayer reports, runs the connect-time fetch on the exchanged token, runs the
// scheduled syncs, and normalises the stored responses into ledger records. It only reads: every
// request goes through internal/bankhttp, whose TrueLayer allow-lists hold the token call and
// twelve GET endpoints and nothing else.
//
// A run makes its ledger reads first (the accounts, their mandates, for Lloyds their transactions
// and balances, and for Lloyds with the cards scope its cards as accounts of their own), then its
// optional reads (pending transactions, the Monzo connection's card list, and /info once a day),
// whose answers never reach a ledger table and never end a consent or change the run's outcome.
// What a run may call is decided by the scopes the /me of its own consent lists.
package truelayer

import (
	"context"
	"errors"
	"log/slog"
	"sync"
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
	DefaultSyncDays   = 35
	DefaultDailyLimit = 4
	DefaultRunTimeout = 15 * time.Minute
)

// DefaultHistoryFrom is the first day the Lloyds connect-time fetch asks for; the account's history
// at TrueLayer starts on 2024-08-22.
var DefaultHistoryFrom = time.Date(2024, 8, 1, 0, 0, 0, 0, time.UTC)

// DefaultCardHistoryFrom is the first day the Lloyds connect-time card transactions fetch asks for:
// a credit card may predate the move to Lloyds, and 2021-01-01 is the earliest start Lloyds
// accepted in the smoke test.
var DefaultCardHistoryFrom = time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)

// Options configures a Worker. A zero value takes its default.
type Options struct {
	// Connection is ConnLloyds or ConnMonzo.
	Connection             string
	ClientID, ClientSecret string
	// AuthURL is TrueLayer's auth server (the auth link and the token call); APIURL its Data API
	// host; PublicURL this service's public base URL, to which /callback/truelayer is added.
	AuthURL, APIURL, PublicURL string
	// HistoryFrom is the first day of the Lloyds connect-time transactions fetch.
	HistoryFrom time.Time
	// CardHistoryFrom is the first day of the Lloyds connect-time card transactions fetch.
	CardHistoryFrom time.Time
	// SyncDays is how many days before today a scheduled Lloyds transactions fetch starts.
	SyncDays int
	// DailyLimit is how many calls a day are made to one endpoint for one bank reference.
	DailyLimit int
	// RunTimeout bounds one run.
	RunTimeout time.Duration
	// Location is FINSTORE_TIMEZONE: the zone days are taken in. Nil means UTC.
	Location *time.Location
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Sleep pauses the auth client between retries of a rate-limited token call; nil means a real
	// timer.
	Sleep func(ctx context.Context, d time.Duration) error
	// Tracer makes the spans of bank calls; nil means none.
	Tracer trace.Tracer
}

// ErrCallbackRefused is returned by HandleCallback for a state value that is unknown, expired,
// already used or issued for another source. No code was exchanged and nothing changed.
var ErrCallbackRefused = consent.ErrAttemptRefused

// Run and fetch kinds.
const (
	KindConnect = "connect"
	KindSync    = "sync"
	kindMeta    = "meta"

	outcomeOK      = "ok"
	outcomePartial = "partial"
	outcomeFailed  = "failed"
)

// Worker runs one TrueLayer connection.
type Worker struct {
	opts     Options
	source   string
	st       *store.Store
	proj     Projector
	metrics  *telemetry.Metrics
	log      *slog.Logger
	consents *consent.Manager
	tokens   *tokens
	api      *api

	bgCtx    context.Context
	bgCancel context.CancelFunc
	bg       sync.WaitGroup
	// mu guards cancelConnect, which cancels the background work of the last connect: a newer
	// connect replaces its token, so its work is pointless.
	mu            sync.Mutex
	cancelConnect context.CancelFunc

	// afterApprove, set only by tests, runs between a connect's approval and its fetch.
	afterApprove func(ctx context.Context)
}

// New returns a Worker. It sends nothing until it is used.
func New(opts Options, st *store.Store, proj Projector, m *telemetry.Metrics, log *slog.Logger) (*Worker, error) {
	switch {
	case opts.Connection != ConnLloyds && opts.Connection != ConnMonzo:
		return nil, errors.New("truelayer: connection must be ob-lloyds or ob-monzo")
	case opts.ClientID == "" || opts.ClientSecret == "":
		return nil, errors.New("truelayer: client id and secret are required")
	case opts.AuthURL == "" || opts.APIURL == "" || opts.PublicURL == "":
		return nil, errors.New("truelayer: auth, API and public URLs are required")
	case st == nil || proj == nil || m == nil:
		return nil, errors.New("truelayer: store, projector and metrics are required")
	}
	if opts.HistoryFrom.IsZero() {
		opts.HistoryFrom = DefaultHistoryFrom
	}
	if opts.CardHistoryFrom.IsZero() {
		opts.CardHistoryFrom = DefaultCardHistoryFrom
	}
	if opts.SyncDays <= 0 {
		opts.SyncDays = DefaultSyncDays
	}
	if opts.DailyLimit <= 0 {
		opts.DailyLimit = DefaultDailyLimit
	}
	if opts.RunTimeout <= 0 {
		opts.RunTimeout = DefaultRunTimeout
	}
	if opts.Location == nil {
		opts.Location = time.UTC
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if _, err := authoriseURL(opts.AuthURL, opts.ClientID, redirectURI(opts.PublicURL), opts.Connection, "x"); err != nil {
		return nil, err
	}
	source := Source(opts.Connection)
	telemetryOpt := bankhttp.WithTelemetry(opts.Tracer, source, m.BankObserver(source))
	authOpts := []bankhttp.Option{bankhttp.WithTokenPath(bankhttp.TrueLayerAuthTokenPath), bankhttp.WithClock(opts.Now),
		telemetryOpt}
	if opts.Sleep != nil {
		authOpts = append(authOpts, bankhttp.WithSleep(opts.Sleep))
	}
	authBank, err := bankhttp.New(opts.AuthURL, bankhttp.TrueLayerAuthRules(), authOpts...)
	if err != nil {
		return nil, err
	}
	apiBank, err := bankhttp.New(opts.APIURL, bankhttp.TrueLayerAPIRules(),
		bankhttp.WithRecorder(recorder{q: st.DB(), source: source, metrics: m}),
		bankhttp.WithClock(opts.Now), bankhttp.WithoutRetries(), telemetryOpt)
	if err != nil {
		return nil, err
	}
	w := &Worker{opts: opts, source: source, st: st, proj: proj, metrics: m, log: log.With(semconv.LogSource, source)}
	w.consents = consent.New(source, opts.Now)
	oauth := &oauthClient{bank: authBank, clientID: opts.ClientID, clientSecret: opts.ClientSecret,
		redirectURI: redirectURI(opts.PublicURL), source: source, metrics: m}
	w.tokens = &tokens{st: st, oauth: oauth, consents: w.consents, source: source, now: opts.Now,
		onLost: func(ctx context.Context, c store.Consent) {
			w.recordConsent(ctx, c)
			w.log.WarnContext(ctx, "truelayer consent lost", semconv.LogState, c.State,
				semconv.LogErrorCategory, string(consent.ReasonRefreshRejected))
		}}
	w.api = &api{bank: apiBank, tokens: w.tokens, metrics: m, st: st, source: source, loc: opts.Location,
		now: opts.Now, limit: opts.DailyLimit, rec: recorder{q: st.DB(), source: source, metrics: m}}
	w.bgCtx, w.bgCancel = context.WithCancel(context.Background())
	return w, nil
}

func (w *Worker) now() time.Time { return w.opts.Now() }

// Source is the worker's source name, truelayer:<connection>.
func (w *Worker) Source() string { return w.source }

// Normaliser returns the normaliser for this connection's stored responses.
func (w *Worker) Normaliser() ledger.SourceNormaliser {
	return Normaliser{Connection: w.opts.Connection}
}

// AuthoriseURL starts a connect attempt and returns the TrueLayer auth link for this bank.
func (w *Worker) AuthoriseURL(ctx context.Context) (string, error) {
	state, err := w.consents.StartAttempt(ctx, w.st.DB())
	if err != nil {
		return "", err
	}
	return authoriseURL(w.opts.AuthURL, w.opts.ClientID, redirectURI(w.opts.PublicURL), w.opts.Connection, state)
}

// HandleCallback consumes the attempt state belongs to, exchanges code and, in one transaction,
// stores the token pair and records the new consent (replacing the previous one). It then records
// the connect run and returns. In the background it reads GET /data/v1/me under a meta fetch,
// approves the consent with the expiry /me reports, or 90 days when that call fails, and runs the
// connect-time fetch on the exchanged access token. Both wait for the writer lock, which another
// source's run may hold for minutes, so the callback does not. The run exists before the callback
// returns, so a stop before the fetch ran leaves a run that ends partial (history_failed for
// Lloyds), here on Close or by Resume after a restart. An unusable state, or one issued for
// another source, returns ErrCallbackRefused without exchanging the code. A failed exchange gives
// the attempt the outcome refused and leaves the current consent in place.
func (w *Worker) HandleCallback(ctx context.Context, state, code string) error {
	att, err := w.consents.ConsumeAttempt(ctx, w.st.DB(), state)
	if err != nil {
		return err
	}
	cons, pair, err := w.tokens.exchange(ctx, code, att.ID)
	if err != nil {
		w.log.WarnContext(ctx, "truelayer code exchange failed", semconv.LogErrorCategory, category(err))
		return err
	}
	w.recordConsent(ctx, cons)
	runID, err := store.StartRun(context.WithoutCancel(ctx), w.st.DB(), w.source, KindConnect, w.now())
	if err != nil {
		// The consent stays awaiting approval; Resume approves it at the next start.
		w.log.ErrorContext(ctx, "truelayer connect run could not be recorded", semconv.LogErrorCategory, category(err))
		return err
	}
	w.goConnect(func(ctx context.Context) { w.connect(ctx, cons.ID, pair, runID) })
	return nil
}

// Reasons a connect's fetch did not run.
var (
	errSuperseded   = errors.New("truelayer: consent no longer current")
	errTokenExpired = errors.New("truelayer: exchanged access token expired before the connect fetch")
)

// connect approves consent id and runs the connect-time fetch as run runID on the exchanged pair.
// When the fetch does not run (the consent was replaced, the worker stopped, the token expired
// while the connect waited for the lock) the run is ended by abandonConnect.
func (w *Worker) connect(ctx context.Context, consentID int64, pair tokenPair, runID int64) {
	_, me, err := w.approve(ctx, consentID, pair.access)
	if err != nil {
		w.abandonConnect(ctx, runID, consentID, err)
		return
	}
	if w.afterApprove != nil {
		w.afterApprove(ctx)
	}
	started := false
	err = w.st.HoldWriterLock(ctx, func(ctx context.Context, conn *pgx.Conn) error {
		c, err := store.GetConsent(ctx, w.st.DB(), consentID)
		if err != nil {
			return err
		}
		if c.State != store.ConsentActive || !c.IsCurrent {
			return errSuperseded
		}
		// The exchanged token is used as it is, never refreshed: once it has expired every call would
		// be refused, which would wrongly end the consent.
		if !w.now().Before(pair.expiresAt) {
			return errTokenExpired
		}
		started = true
		return w.runLocked(ctx, conn, runID, consentID, KindConnect, pair.access, me.Scopes)
	})
	switch {
	case !started:
		w.abandonConnect(ctx, runID, consentID, err)
	case err != nil:
		w.log.ErrorContext(ctx, "truelayer connect-time fetch failed", semconv.LogErrorCategory, category(err))
	}
}

// abandonedCategory is the category of a connect run that ended before its fetch was done: for
// Lloyds the full history is lost until a reconnect, which history_failed alerts on.
func (w *Worker) abandonedCategory() string {
	if w.opts.Connection == ConnLloyds {
		return categoryHistoryFailed
	}
	return categoryInterrupted
}

// abandonConnect ends connect run runID, whose fetch never ran, as partial: superseded when a newer
// connect replaced consent id (that connect fetches instead), otherwise abandonedCategory.
func (w *Worker) abandonConnect(ctx context.Context, runID, consentID int64, cause error) {
	ctx = context.WithoutCancel(ctx)
	cat := w.abandonedCategory()
	if c, err := store.GetConsent(ctx, w.st.DB(), consentID); err == nil && (!c.IsCurrent || c.State == store.ConsentReplaced) {
		cat = categorySuperseded
	}
	tally := &runTally{category: cat, partial: true}
	if err := w.finishRun(ctx, runID, KindConnect, outcomePartial, tally); err != nil {
		w.log.ErrorContext(ctx, "truelayer connect run could not be ended", semconv.LogErrorCategory, category(err))
		return
	}
	if cat == categorySuperseded {
		w.log.InfoContext(ctx, "truelayer connect superseded by a newer connect", semconv.LogRunID, runID,
			semconv.LogOutcome, outcomePartial, semconv.LogErrorCategory, cat)
		return
	}
	w.log.WarnContext(ctx, "truelayer connect ended before its fetch", semconv.LogRunID, runID,
		semconv.LogOutcome, outcomePartial, semconv.LogErrorCategory, cat, semconv.LogKind, category(cause))
}

// approve reads /data/v1/me under a meta fetch, with token when it is set and otherwise the stored
// one, and makes consent id active until the expiry it reports, or for consent.Lifetime when the
// call fails. It also returns what /me said (the zero meInfo when the call failed), so the connect
// run can take its scopes from the consent it is running under.
func (w *Worker) approve(ctx context.Context, id int64, token string) (store.Consent, meInfo, error) {
	expiry := w.now().Add(consent.Lifetime)
	var read meInfo
	err := w.st.HoldWriterLock(ctx, func(ctx context.Context, _ *pgx.Conn) error {
		fid, err := store.InsertFetch(ctx, w.st.DB(), store.Fetch{Source: w.source, Kind: kindMeta, StartedAt: w.now()})
		if err != nil {
			return err
		}
		me, err := w.api.me(withAccessToken(withFetch(ctx, fid), token))
		switch {
		case errors.Is(err, ErrConsentLost):
			return err
		case err != nil:
			w.log.WarnContext(ctx, "truelayer consent expiry unknown: assuming 90 days", semconv.LogErrorCategory, category(err))
		default:
			read = me
			if me.Expiry.After(w.now()) {
				expiry = me.Expiry
			}
		}
		return nil
	})
	if err != nil {
		return store.Consent{}, meInfo{}, err
	}
	act, err := w.consents.ApprovedUntil(ctx, w.st.DB(), id, expiry)
	if err != nil {
		return store.Consent{}, meInfo{}, err
	}
	w.recordConsent(ctx, act)
	w.log.InfoContext(ctx, "truelayer consent approved", semconv.LogState, act.State)
	return act, read, nil
}

// Resume restores state after a restart: a connect run left unfinished ends partial with
// history_failed for Lloyds (the full history was only available to the exchanged token, which a
// reconnect replaces) or interrupted for Monzo; a
// current consent with no stored token ends lost with token_missing; and a current consent still
// awaiting approval (a restart between the exchange and the approval) is approved in the
// background with /data/v1/me, or 90 days if that fails. It must run before the worker does
// anything else.
func (w *Worker) Resume(ctx context.Context) error {
	q := w.st.DB()
	ids, err := store.UnfinishedRuns(ctx, q, w.source, KindConnect)
	if err != nil {
		return err
	}
	for _, id := range ids {
		tally := &runTally{category: w.abandonedCategory(), partial: true}
		if err := w.finishRun(ctx, id, KindConnect, outcomePartial, tally); err != nil {
			return err
		}
		w.log.WarnContext(ctx, "truelayer connect run ended by a restart", semconv.LogRunID, id,
			semconv.LogOutcome, outcomePartial, semconv.LogErrorCategory, tally.category)
	}
	cur, err := w.consents.Current(ctx, q)
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
	if cur.State == store.ConsentAwaitingApproval {
		id := cur.ID
		w.goConnect(func(ctx context.Context) {
			if _, _, err := w.approve(ctx, id, ""); err != nil {
				if ctx.Err() != nil || errors.Is(err, consent.ErrIllegalTransition) {
					// Stopped, or a newer connect replaced the consent first.
					w.log.InfoContext(ctx, "truelayer approval not recorded", semconv.LogErrorCategory, category(err))
					return
				}
				w.log.ErrorContext(ctx, "truelayer approval could not be recorded", semconv.LogErrorCategory, category(err))
			}
		})
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

// recordConsent exports a consent's state and expected expiry.
func (w *Worker) recordConsent(ctx context.Context, c store.Consent) {
	w.metrics.ConsentState(ctx, w.source, c.State, consent.States())
	if c.ExpectedExpiryAt != nil {
		w.metrics.ConsentExpiry(ctx, w.source, *c.ExpectedExpiryAt)
	}
}

// loseIfTokenMissing ends consent c as lost (token_missing) when it is awaiting approval or
// active and no token is stored, which only a database restore produces. It reports whether it
// ended the consent, with ErrConsentLost.
func (w *Worker) loseIfTokenMissing(ctx context.Context, c store.Consent) (bool, error) {
	if c.State != store.ConsentAwaitingApproval && c.State != store.ConsentActive {
		return false, nil
	}
	_, err := store.GetOAuthToken(ctx, w.st.DB(), w.source)
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	lost, err := w.consents.Lost(ctx, w.st.DB(), c.ID, consent.ReasonTokenMissing)
	if err != nil {
		return false, err
	}
	w.recordConsent(ctx, lost)
	w.log.WarnContext(ctx, "truelayer consent lost", semconv.LogState, lost.State,
		semconv.LogErrorCategory, string(consent.ReasonTokenMissing))
	return true, ErrConsentLost
}

// markLost ends an active consent after a data call was refused for access.
func (w *Worker) markLost(ctx context.Context, id int64) {
	ctx = context.WithoutCancel(ctx)
	c, err := w.consents.Lost(ctx, w.st.DB(), id, consent.ReasonAccessDenied)
	if err != nil {
		w.log.ErrorContext(ctx, "truelayer consent could not be marked lost", semconv.LogErrorCategory, category(err))
		return
	}
	w.recordConsent(ctx, c)
	w.log.WarnContext(ctx, "truelayer consent lost", semconv.LogState, c.State,
		semconv.LogErrorCategory, string(consent.ReasonAccessDenied))
}

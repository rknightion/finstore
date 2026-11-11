// Package web serves the status page, the connect flow of each configured bank connection and the
// health route.
//
// The pages show states, dates and counts only: never a transaction, a balance or a payee. The
// service sits behind a proxy that signs users in and passes the signed-in address in a header;
// the pages check that address against an allow-list and answer 404 to everyone else, because the
// proxy in front may turn a 401 or 403 into a login redirect and loop. The OAuth callback and the
// health route carry no such check: the callback is protected by its single-use state value.
//
// The MCP endpoint, when one is configured, is mounted at /mcp outside that check: its callers are
// devices, tagged ones included, which have no signed-in address, so it brings its own gate.
package web

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/rknightion/finstore/internal/consent"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry"
)

// Connector starts and completes a bank connection.
type Connector interface {
	// AuthoriseURL records a new connect attempt and returns the bank's login URL for it.
	AuthoriseURL(ctx context.Context) (string, error)
	// HandleCallback consumes the attempt that state belongs to and exchanges code.
	HandleCallback(ctx context.Context, state, code string) error
}

// Reader is what the pages read.
type Reader interface {
	Ping(ctx context.Context) error
	SyncStatus(ctx context.Context) ([]store.SyncStatusRow, error)
	Coverage(ctx context.Context) ([]store.CoverageRow, error)
	// ConnectAttemptSource returns the source of the live attempt with this state hash without
	// consuming it, or store.ErrAttemptUnusable for an unknown, expired or used one.
	ConnectAttemptSource(ctx context.Context, stateSHA256 []byte, now time.Time) (string, error)
}

// Source is one bank connection the pages offer: a section on the status page, a connect route
// and a callback.
type Source struct {
	// Name is the source name as sync.consent and agent.sync_status hold it.
	Name string
	// Label names the connection on the page.
	Label string
	// ConnectPath is the POST route that starts a connect attempt; CallbackPath is where the bank
	// sends the browser back. Sources that share a callback path are told apart by the source of
	// the attempt the state belongs to, read before anything consumes it.
	ConnectPath, CallbackPath string
	// AuthURL is the bank's login URL; a submitted connect form may redirect to its origin.
	AuthURL string
	// ConnectNotice is shown above the connect button before a consent exists, ReconnectNotice
	// after. Either may be empty.
	ConnectNotice, ReconnectNotice string
	Connector                      Connector
	// IsRefusal reports whether an error from Connector.HandleCallback means the state value was
	// unknown, expired or already used.
	IsRefusal func(error) bool
}

// Options configures a Server.
type Options struct {
	// AllowedEmails are the lower-case addresses that may use the pages.
	AllowedEmails []string
	// AuthHeader names the request header carrying the signed-in address.
	AuthHeader string
	// Sources are the configured connections, in the order the status page shows them.
	Sources []Source
	// Location is the zone times are shown in.
	Location *time.Location
	Now      func() time.Time
	Log      *slog.Logger
	// Tracer makes the connect and callback spans. Nil means no spans.
	Tracer trace.Tracer
	// MCP serves the MCP endpoint at /mcp, for every method. Nil leaves /mcp unmounted, so it
	// answers 404. It is not behind the allow-list of the pages, which needs a signed-in user:
	// callers of /mcp are devices, tagged ones included, and the handler brings its own gate
	// (mcpserver.Gate).
	MCP http.Handler
}

// Server holds the HTTP handlers.
type Server struct {
	opts Options
	read Reader
	csp  string
	// callbacks maps each callback path to the sources that share it, by name.
	callbacks map[string]map[string]Source
	tracer    trace.Tracer
}

//go:embed status.html
var statusHTML string

//go:embed status.css
var statusCSS string

var statusTmpl = template.Must(template.New("status").Parse(statusHTML))

// continueTmpl is the page a connect post answers with. It moves the browser on to the bank's
// login page by itself instead of a redirect: a bank's login page may redirect onward to another
// domain (TrueLayer sends the browser to the bank's own site), and the form-action policy applies
// to every redirect after a form post, so a redirect would be blocked partway. A page that
// navigates on its own is not a form submission.
var continueTmpl = template.Must(template.New("continue").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="robots" content="noindex">
<meta http-equiv="refresh" content="0; url={{.URL}}">
<title>finstore</title>
<style>{{.CSS}}</style>
</head>
<body>
<p>Taking you to your bank to approve the connection. If nothing happens, <a id="continue" href="{{.URL}}">continue to your bank</a>.</p>
</body>
</html>
`))

// New builds a Server. It refuses an empty allow-list: a service with nobody allowed is a
// misconfiguration, not a locked-down service.
func New(opts Options, read Reader) (*Server, error) {
	if len(opts.AllowedEmails) == 0 {
		return nil, errors.New("web: the allow-list is empty")
	}
	if opts.AuthHeader == "" {
		return nil, errors.New("web: no auth header name")
	}
	if len(opts.Sources) == 0 {
		return nil, errors.New("web: no sources")
	}
	if opts.Location == nil {
		opts.Location = time.UTC
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	tracer := opts.Tracer
	if tracer == nil {
		tracer = tracenoop.NewTracerProvider().Tracer(semconv.ServiceName)
	}
	opts.Sources = slices.Clone(opts.Sources) // defaults are filled in below
	names, routes := map[string]bool{}, map[string]bool{}
	callbacks := map[string]map[string]Source{}
	for i, src := range opts.Sources {
		switch {
		case src.Name == "" || src.Label == "" || src.Connector == nil:
			return nil, fmt.Errorf("web: source %d lacks a name, a label or a connector", i)
		case !strings.HasPrefix(src.ConnectPath, "/") || !strings.HasPrefix(src.CallbackPath, "/"):
			return nil, fmt.Errorf("web: source %d lacks a connect or callback path", i)
		case names[src.Name] || routes[src.ConnectPath]:
			return nil, fmt.Errorf("web: source %d repeats a name or a connect path", i)
		}
		names[src.Name], routes[src.ConnectPath] = true, true
		u, err := url.Parse(src.AuthURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("web: source %d's bank login URL is not an absolute URL", i)
		}
		if src.IsRefusal == nil {
			src.IsRefusal = func(error) bool { return false }
			opts.Sources[i] = src
		}
		if callbacks[src.CallbackPath] == nil {
			callbacks[src.CallbackPath] = map[string]Source{}
		}
		callbacks[src.CallbackPath][src.Name] = src
	}
	sum := sha256.Sum256([]byte(statusCSS))
	// Forms post only to this site; a connect post answers with a page that moves on to the bank
	// by itself (see continueTmpl).
	csp := fmt.Sprintf("default-src 'none'; style-src 'sha256-%s'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'",
		base64.StdEncoding.EncodeToString(sum[:]))
	return &Server{opts: opts, read: read, csp: csp, callbacks: callbacks, tracer: tracer}, nil
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", s.logged("/healthz", http.HandlerFunc(s.healthz)))
	mux.Handle("GET /{$}", s.logged("/", s.allowed(http.HandlerFunc(s.status))))
	if s.opts.MCP != nil {
		mux.Handle("/mcp", s.logged("/mcp", s.opts.MCP))
	}
	for _, src := range s.opts.Sources {
		mux.Handle("POST "+src.ConnectPath, s.logged(src.ConnectPath, s.allowed(s.connect(src))))
	}
	for path, group := range s.callbacks {
		var h http.Handler
		if len(group) == 1 {
			for _, src := range group {
				h = s.callback(src)
			}
		} else {
			h = s.sharedCallback(group)
		}
		mux.Handle("GET "+path, s.logged(path, h))
	}
	return s.headers(mux)
}

// headers sets the response headers every route carries.
func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", s.csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// logged writes one line per request with the route's fixed name, never the request URL: a
// callback URL carries the authorisation code.
func (s *Server) logged(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.opts.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		level := slog.LevelDebug
		if route != "/healthz" {
			level = slog.LevelInfo
		}
		s.opts.Log.Log(r.Context(), level, "http request",
			semconv.LogEndpoint, route,
			semconv.LogStatus, rec.status,
			semconv.LogDurationMS, s.opts.Now().Sub(start).Milliseconds())
	})
}

// allowed answers 404 unless the signed-in address is on the allow-list.
func (s *Server) allowed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values(s.opts.AuthHeader)
		if len(values) != 1 {
			http.NotFound(w, r)
			return
		}
		email := strings.ToLower(strings.TrimSpace(values[0]))
		if email == "" || !slices.Contains(s.opts.AllowedEmails, email) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.read.Ping(ctx); err != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// connect starts a connect attempt for src. It accepts only a same-origin form post, so another
// site cannot start one from the signed-in user's browser.
func (s *Server) connect(src Source) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r, f := s.startFlow(r, semconv.SpanConnect, src.Name)
		defer f.end()
		if r.Header.Get("Sec-Fetch-Site") != "same-origin" {
			f.outcome(semconv.ConnectCrossSite)
			// Not 403: the proxy in front may rewrite a 403 into a login redirect.
			http.Error(w, "This request must come from the status page.", http.StatusBadRequest)
			return
		}
		target, err := src.Connector.AuthoriseURL(r.Context())
		if err != nil {
			f.startFailed()
			s.opts.Log.ErrorContext(r.Context(), "connect attempt not started",
				semconv.LogSource, src.Name, semconv.LogOutcome, "error")
			http.Error(w, "The connection could not be started. Try again.", http.StatusInternalServerError)
			return
		}
		u, err := url.Parse(target)
		if err != nil || u.Scheme != "https" && u.Scheme != "http" {
			f.startFailed()
			s.opts.Log.ErrorContext(r.Context(), "connect attempt gave no usable bank link",
				semconv.LogSource, src.Name, semconv.LogOutcome, "error")
			http.Error(w, "The connection could not be started. Try again.", http.StatusInternalServerError)
			return
		}
		f.outcome(semconv.ConnectStarted)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = continueTmpl.Execute(w, struct {
			URL *url.URL
			CSS template.CSS
		}{u, template.CSS(statusCSS)}) // #nosec G203 -- an embedded file, not input
	})
}

const (
	msgInvalidLink = "This link is not valid. Start again from the status page."
	msgRefused     = "This link is not valid or has already been used. Start again from the status page."
)

// callback completes a connect attempt of src, the only source on its callback path. Nothing here
// is behind the sign-in, so it acts only on a valid single-use state value and says nothing about
// why a value was refused.
func (s *Server) callback(src Source) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r, f := s.startFlow(r, semconv.SpanCallback, src.Name)
		defer f.end()
		q := r.URL.Query()
		state, code := q.Get("state"), q.Get("code")
		if state == "" || code == "" {
			f.outcome(semconv.ConnectInvalidLink)
			http.Error(w, msgInvalidLink, http.StatusBadRequest)
			return
		}
		ctx, cancel := callbackContext(r)
		defer cancel()
		s.complete(ctx, w, r, f, src, state, code)
	})
}

// sharedCallback completes a connect attempt of one of the sources in group, which share the
// callback path. It reads the source of the attempt the state belongs to without consuming it and
// refuses, with no effect, a state that is unknown, expired, used or issued for a source outside
// the group; only then does it hand the state to that source's connector, which consumes it. A
// bank's error redirect (?error=, sent when the owner declines or the bank fails) changes nothing:
// the attempt expires unused.
func (s *Server) sharedCallback(group map[string]Source) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The source is not known until the attempt has been read.
		r, f := s.startFlow(r, semconv.SpanCallback, semconv.SourceUnknown)
		defer f.end()
		q := r.URL.Query()
		if q.Has("error") {
			f.outcome(semconv.ConnectBankDeclined)
			// The error code is the bank's text, so neither the page nor the log repeats it.
			http.Error(w, "The bank did not give access, so nothing changed. Start again from the status page.",
				http.StatusBadRequest)
			return
		}
		state, code := q.Get("state"), q.Get("code")
		if state == "" || code == "" {
			f.outcome(semconv.ConnectInvalidLink)
			http.Error(w, msgInvalidLink, http.StatusBadRequest)
			return
		}
		ctx, cancel := callbackContext(r)
		defer cancel()
		name, err := s.read.ConnectAttemptSource(ctx, consent.HashState(state), s.opts.Now())
		switch {
		case errors.Is(err, store.ErrAttemptUnusable):
			f.outcome(semconv.ConnectRefused)
			http.Error(w, msgRefused, http.StatusBadRequest)
			return
		case err != nil:
			f.failed(semconv.ErrorAttemptRead)
			s.opts.Log.ErrorContext(r.Context(), "connect attempt not read", semconv.LogOutcome, "error")
			http.Error(w, "The connection could not be completed. Try the link again.", http.StatusServiceUnavailable)
			return
		}
		f.source(name)
		src, ok := group[name]
		if !ok {
			f.outcome(semconv.ConnectRefused)
			http.Error(w, msgRefused, http.StatusBadRequest)
			return
		}
		s.complete(ctx, w, r, f, src, state, code)
	})
}

// flow is the span of one connect or callback request. It records fixed words only: the source
// name, an outcome and an error category. Never a code, a state, a query or an error.
type flow struct{ span trace.Span }

// startFlow starts the span name for source as a child of the request's server span, and returns
// the request carrying it, so the connector's work and the log lines are inside it.
func (s *Server) startFlow(r *http.Request, name, source string) (*http.Request, flow) {
	ctx, span := s.tracer.Start(r.Context(), name,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attribute.String(semconv.SpanSource, source)))
	return r.WithContext(ctx), flow{span}
}

func (f flow) end() { f.span.End() }

// source sets the span's source once it is known.
func (f flow) source(name string) {
	f.span.SetAttributes(attribute.String(semconv.SpanSource, name))
}

// outcome records how the request ended.
func (f flow) outcome(outcome string) {
	f.span.SetAttributes(attribute.String(semconv.SpanConnectOutcome, outcome))
}

// failed records outcome failed with the error category (error.type) and marks the span failed.
func (f flow) failed(category string) {
	f.outcome(semconv.ConnectFailed)
	f.span.SetAttributes(attribute.String(semconv.SpanErrorType, category))
	telemetry.Fail(f.span, category)
}

// startFailed records a connect attempt that could not be started.
func (f flow) startFailed() {
	f.outcome(semconv.ConnectStartFailed)
	telemetry.Fail(f.span, "error")
}

// callbackContext bounds a callback's exchange. The exchange spends the code and replaces the
// previous connection at the bank, so it must finish and be stored even if the browser goes away
// half way: the request's cancellation is not passed on.
func callbackContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), callbackTimeout)
}

// complete hands state and code to src's connector and answers with the outcome.
func (s *Server) complete(ctx context.Context, w http.ResponseWriter, r *http.Request, f flow, src Source, state, code string) {
	err := src.Connector.HandleCallback(ctx, state, code)
	switch {
	case err == nil:
		f.outcome(semconv.ConnectExchanged)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case src.IsRefusal(err):
		f.outcome(semconv.ConnectRefused)
		http.Error(w, msgRefused, http.StatusBadRequest)
	default:
		f.failed(semconv.ErrorExchange)
		s.opts.Log.ErrorContext(r.Context(), "connect attempt not completed",
			semconv.LogSource, src.Name, semconv.LogOutcome, "error")
		http.Error(w, "The connection could not be completed. Start again from the status page.",
			http.StatusBadGateway)
	}
}

type pageSource struct {
	Name         string
	Label        string
	ConnectPath  string
	Notice       string
	ConsentState string
	ApprovedAt   string
	ExpiresAt    string
	DaysToExpiry string
	LastSyncOK   string
	LastOutcome  string
	Connected    bool
}

type pageGap struct{ From, To, Reason string }

type pageAccount struct {
	Slug, Feed, Earliest, Latest string
	Transactions                 int64
	State, ReconciledThrough     string
	LastImport                   string
	Gaps                         []pageGap
}

type page struct {
	CSS       template.CSS
	Generated string
	Zone      string
	Sources   []pageSource
	Accounts  []pageAccount
}

const (
	dash       = "none"
	timeLayout = "2006-01-02 15:04"
	// callbackTimeout bounds a callback's exchange. It is below the server's write timeout.
	callbackTimeout = 75 * time.Second
)

// feedLabel names where an account's data comes from: Monzo's own API, TrueLayer, or statement
// files. An API source it does not know is shown as stored.
func feedLabel(feed, source string) string {
	switch {
	case feed == "file":
		return "statement file"
	case source == "monzo":
		return "Monzo API"
	case strings.HasPrefix(source, "truelayer:"):
		return "TrueLayer"
	case source != "":
		return source
	default:
		return feed
	}
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	sources, err := s.read.SyncStatus(ctx)
	if err != nil {
		s.statusFailed(w, r)
		return
	}
	coverage, err := s.read.Coverage(ctx)
	if err != nil {
		s.statusFailed(w, r)
		return
	}
	p := page{
		CSS:       template.CSS(statusCSS), // #nosec G203 -- an embedded file, not input
		Generated: s.opts.Now().In(s.opts.Location).Format(timeLayout),
		Zone:      s.opts.Location.String(),
	}
	imports := map[string]*time.Time{}
	rows := map[string]store.SyncStatusRow{}
	for _, src := range sources {
		for slug, at := range src.LastImportAt {
			imports[slug] = at
		}
		rows[src.Source] = src
	}
	// One section per configured source, in the configured order: a source with no consent or run
	// yet has no row, and a row for a source that is not configured gets no section.
	for _, cfg := range s.opts.Sources {
		row := rows[cfg.Name]
		ps := pageSource{
			Name:         cfg.Name,
			Label:        cfg.Label,
			ConnectPath:  cfg.ConnectPath,
			Notice:       cfg.ConnectNotice,
			ConsentState: "not connected",
			ApprovedAt:   s.when(row.ApprovedAt),
			ExpiresAt:    s.when(row.ExpectedExpiryAt),
			DaysToExpiry: dash,
			LastSyncOK:   s.when(row.LastSyncOKAt),
			LastOutcome:  dash,
		}
		if row.ConsentState != nil {
			ps.ConsentState = strings.ReplaceAll(*row.ConsentState, "_", " ")
			ps.Connected = true
			ps.Notice = cfg.ReconnectNotice
		}
		if row.DaysToExpiry != nil {
			ps.DaysToExpiry = fmt.Sprint(*row.DaysToExpiry)
		}
		if row.LastRunOutcome != nil {
			ps.LastOutcome = *row.LastRunOutcome
		}
		p.Sources = append(p.Sources, ps)
	}
	for _, c := range coverage {
		a := pageAccount{
			Slug: c.AccountSlug, Feed: feedLabel(c.Feed, c.Source),
			Earliest: day(c.EarliestDate), Latest: day(c.LatestDate),
			Transactions:      c.TransactionCount,
			State:             strings.ReplaceAll(c.ReconciliationState, "_", " "),
			ReconciledThrough: day(c.ReconciledThrough),
			LastImport:        dash,
		}
		if at, ok := imports[c.AccountSlug]; ok {
			a.LastImport = s.when(at)
		}
		// A gap's balance difference is an amount, so the page leaves it out.
		for _, g := range c.Gaps {
			a.Gaps = append(a.Gaps, pageGap{From: day(g.From), To: g.To.Format(time.DateOnly), Reason: g.Reason})
		}
		p.Accounts = append(p.Accounts, a)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := statusTmpl.Execute(w, p); err != nil {
		s.opts.Log.ErrorContext(r.Context(), "status page not rendered", semconv.LogOutcome, "error")
	}
}

func (s *Server) statusFailed(w http.ResponseWriter, r *http.Request) {
	s.opts.Log.ErrorContext(r.Context(), "status not read", semconv.LogOutcome, "error")
	http.Error(w, "The status could not be read.", http.StatusServiceUnavailable)
}

func (s *Server) when(t *time.Time) string {
	if t == nil {
		return dash
	}
	return t.In(s.opts.Location).Format(timeLayout)
}

func day(t *time.Time) string {
	if t == nil {
		return dash
	}
	return t.Format(time.DateOnly)
}

// NewHTTPServer wraps a handler with the timeouts the service runs with. The write timeout
// covers a callback, which waits for one token exchange with the bank.
func NewHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
}

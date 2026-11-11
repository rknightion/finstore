// Package web serves the status page, the Monzo connect flow and the health route.
//
// The pages show states, dates and counts only: never a transaction, a balance or a payee. The
// service sits behind a proxy that signs users in and passes the signed-in address in a header;
// the pages check that address against an allow-list and answer 404 to everyone else, because the
// proxy in front may turn a 401 or 403 into a login redirect and loop. The OAuth callback and the
// health route carry no such check: the callback is protected by its single-use state value.
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

	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
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
}

// Options configures a Server.
type Options struct {
	// AllowedEmails are the lower-case addresses that may use the pages.
	AllowedEmails []string
	// AuthHeader names the request header carrying the signed-in address.
	AuthHeader string
	// BankAuthURL is the bank's login URL; a submitted connect form may redirect to its origin.
	BankAuthURL string
	// IsRefusal reports whether an error from Connector.HandleCallback means the state value was
	// unknown, expired or already used.
	IsRefusal func(error) bool
	// Location is the zone times are shown in.
	Location *time.Location
	Now      func() time.Time
	Log      *slog.Logger
}

// Server holds the HTTP handlers.
type Server struct {
	opts Options
	conn Connector
	read Reader
	csp  string
}

//go:embed status.html
var statusHTML string

//go:embed status.css
var statusCSS string

var statusTmpl = template.Must(template.New("status").Parse(statusHTML))

// New builds a Server. It refuses an empty allow-list: a service with nobody allowed is a
// misconfiguration, not a locked-down service.
func New(opts Options, conn Connector, read Reader) (*Server, error) {
	if len(opts.AllowedEmails) == 0 {
		return nil, errors.New("web: the allow-list is empty")
	}
	if opts.AuthHeader == "" {
		return nil, errors.New("web: no auth header name")
	}
	if opts.IsRefusal == nil {
		opts.IsRefusal = func(error) bool { return false }
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
	formAction := "'self'"
	if opts.BankAuthURL != "" {
		u, err := url.Parse(opts.BankAuthURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, errors.New("web: the bank login URL is not an absolute URL")
		}
		formAction += " " + u.Scheme + "://" + u.Host
	}
	sum := sha256.Sum256([]byte(statusCSS))
	csp := fmt.Sprintf("default-src 'none'; style-src 'sha256-%s'; form-action %s; frame-ancestors 'none'; base-uri 'none'",
		base64.StdEncoding.EncodeToString(sum[:]), formAction)
	return &Server{opts: opts, conn: conn, read: read, csp: csp}, nil
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", s.logged("/healthz", http.HandlerFunc(s.healthz)))
	mux.Handle("GET /{$}", s.logged("/", s.allowed(http.HandlerFunc(s.status))))
	mux.Handle("POST /connect/monzo", s.logged("/connect/monzo", s.allowed(http.HandlerFunc(s.connect))))
	mux.Handle("GET /callback/monzo", s.logged("/callback/monzo", http.HandlerFunc(s.callback)))
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

// connect starts a connect attempt. It accepts only a same-origin form post, so another site
// cannot start one from the signed-in user's browser.
func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Sec-Fetch-Site") != "same-origin" {
		// Not 403: the proxy in front may rewrite a 403 into a login redirect.
		http.Error(w, "This request must come from the status page.", http.StatusBadRequest)
		return
	}
	target, err := s.conn.AuthoriseURL(r.Context())
	if err != nil {
		s.opts.Log.ErrorContext(r.Context(), "connect attempt not started",
			semconv.LogSource, "monzo", semconv.LogOutcome, "error")
		http.Error(w, "The connection could not be started. Try again.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// callback completes a connect attempt. Nothing here is behind the sign-in, so it acts only on a
// valid single-use state value and says nothing about why a value was refused.
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state, code := q.Get("state"), q.Get("code")
	if state == "" || code == "" {
		http.Error(w, "This link is not valid. Start again from the status page.", http.StatusBadRequest)
		return
	}
	// The exchange spends the code and replaces the previous connection at the bank, so it must
	// finish and be stored even if the browser goes away half way: the request's cancellation is
	// not passed on.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), callbackTimeout)
	defer cancel()
	err := s.conn.HandleCallback(ctx, state, code)
	switch {
	case err == nil:
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case s.opts.IsRefusal(err):
		http.Error(w, "This link is not valid or has already been used. Start again from the status page.",
			http.StatusBadRequest)
	default:
		s.opts.Log.ErrorContext(r.Context(), "connect attempt not completed",
			semconv.LogSource, "monzo", semconv.LogOutcome, "error")
		http.Error(w, "The connection could not be completed. Start again from the status page.",
			http.StatusBadGateway)
	}
}

type pageSource struct {
	Name         string
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
	for _, src := range sources {
		for slug, at := range src.LastImportAt {
			imports[slug] = at
		}
		if src.Source != "monzo" {
			continue
		}
		ps := pageSource{
			Name:         src.Source,
			ConsentState: "not connected",
			ApprovedAt:   s.when(src.ApprovedAt),
			ExpiresAt:    s.when(src.ExpectedExpiryAt),
			DaysToExpiry: dash,
			LastSyncOK:   s.when(src.LastSyncOKAt),
			LastOutcome:  dash,
		}
		if src.ConsentState != nil {
			ps.ConsentState = strings.ReplaceAll(*src.ConsentState, "_", " ")
			ps.Connected = true
		}
		if src.DaysToExpiry != nil {
			ps.DaysToExpiry = fmt.Sprint(*src.DaysToExpiry)
		}
		if src.LastRunOutcome != nil {
			ps.LastOutcome = *src.LastRunOutcome
		}
		p.Sources = append(p.Sources, ps)
	}
	if len(p.Sources) == 0 {
		p.Sources = []pageSource{{
			Name: "monzo", ConsentState: "not connected", ApprovedAt: dash, ExpiresAt: dash,
			DaysToExpiry: dash, LastSyncOK: dash, LastOutcome: dash,
		}}
	}
	for _, c := range coverage {
		a := pageAccount{
			Slug: c.AccountSlug, Feed: c.Feed,
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

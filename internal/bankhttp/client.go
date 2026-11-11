// Package bankhttp is the only HTTP client the service may use to talk to a
// bank. It sends nothing that is not on its allow-list, records responses
// through an injected Recorder, and reports failures as categories only.
package bankhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/rknightion/finstore/internal/semconv"
)

// Category classifies a failed request. Nothing else about a failure leaves
// the package.
type Category string

// The complete set of error categories.
const (
	CategoryTimeout                          Category = "timeout"
	CategoryNetwork                          Category = "network"
	CategoryRateLimited                      Category = "rate_limited"
	CategoryServerError                      Category = "server_error"
	CategoryUnauthorised                     Category = "unauthorised"
	CategoryForbiddenInsufficientPermissions Category = "forbidden_insufficient_permissions"
	CategoryForbiddenVerificationRequired    Category = "forbidden_verification_required"
	CategoryBadRequest                       Category = "bad_request"
	CategoryUnexpectedStatus                 Category = "unexpected_status"
)

// ErrNotAllowed is returned, before anything is sent, for a method and path
// that match no rule.
var ErrNotAllowed = errors.New("bankhttp: request not on the allow-list")

// ErrNoRecorder is returned, before anything is sent, when a rule with Record
// set is used on a client that has no Recorder.
var ErrNoRecorder = errors.New("bankhttp: rule is recorded but no recorder is configured")

const (
	tokenMethod = http.MethodPost
	// defaultTokenPath is the token path of a client that WithTokenPath does not change.
	defaultTokenPath = "/oauth2/token" //nolint:gosec // an endpoint path, not a credential

	maxRetries429   = 6
	maxRetries5xx   = 3
	maxRetryNetwork = 3
	backoffStart    = 2 * time.Second
	backoffCeiling  = 30 * time.Second
	maxBodyBytes    = 64 << 20
)

// Rule allows one method and path. A path segment written {id} is a template:
// it matches one segment of ASCII letters, digits and hyphens, and the
// template, not the path sent, is what the Recorder and an Error carry. Record
// says whether every response to it is handed to the Recorder.
type Rule struct {
	Method, Path string
	Record       bool
}

// idSegment is the template segment.
const idSegment = "{id}"

var idPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// matches reports whether path fits template segment by segment.
func matches(template, path string) bool {
	ts, ps := strings.Split(template, "/"), strings.Split(path, "/")
	if len(ts) != len(ps) {
		return false
	}
	for i := range ts {
		if ts[i] == idSegment {
			if !idPattern.MatchString(ps[i]) {
				return false
			}
		} else if ts[i] != ps[i] {
			return false
		}
	}
	return true
}

// MonzoRules is the allow-list for the Monzo API.
func MonzoRules() []Rule {
	rules := []Rule{{Method: tokenMethod, Path: defaultTokenPath}}
	for _, p := range []string{"/ping/whoami", "/accounts", "/balance", "/pots", "/transactions", "/webhooks"} {
		rules = append(rules, Rule{Method: http.MethodGet, Path: p, Record: true})
	}
	return rules
}

// TrueLayerAuthTokenPath is the token path of TrueLayer's auth server.
const TrueLayerAuthTokenPath = "/connect/token" //nolint:gosec // an endpoint path, not a credential

// TrueLayerAuthRules is the allow-list for TrueLayer's auth server: the token call only. Its
// client is created WithTokenPath(TrueLayerAuthTokenPath).
func TrueLayerAuthRules() []Rule {
	return []Rule{{Method: tokenMethod, Path: TrueLayerAuthTokenPath}}
}

// TrueLayerAPIRules is the allow-list for TrueLayer's Data API v1: twelve reads, every one recorded.
func TrueLayerAPIRules() []Rule {
	var rules []Rule
	for _, p := range []string{"/data/v1/me", "/data/v1/accounts", "/data/v1/accounts/{id}/balance",
		"/data/v1/accounts/{id}/transactions", "/data/v1/accounts/{id}/standing_orders",
		"/data/v1/accounts/{id}/direct_debits", "/data/v1/accounts/{id}/transactions/pending",
		"/data/v1/cards", "/data/v1/cards/{id}/balance", "/data/v1/cards/{id}/transactions",
		"/data/v1/cards/{id}/transactions/pending", "/data/v1/info"} {
		rules = append(rules, Rule{Method: http.MethodGet, Path: p, Record: true})
	}
	return rules
}

// Request is one call to a bank. Form, when set, is sent as an
// application/x-www-form-urlencoded body.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Form   url.Values
	Bearer string
}

// Response is the final response of a call, byte for byte.
type Response struct {
	Status      int
	ContentType string
	Body        []byte
	StartedAt   time.Time
	Duration    time.Duration
}

// Recorded is one response handed to the Recorder. Endpoint is the rule's path,
// the template for a template rule.
type Recorded struct {
	Endpoint    string
	Query       map[string][]string
	Status      int
	ContentType string
	FetchedAt   time.Time
	Body        []byte
	// Params are the request parameters the caller put on the context with
	// WithRequestParams, or nil.
	Params map[string]string
}

type paramsKey struct{}

// WithRequestParams returns ctx carrying params, which every response recorded
// for a request made under it is handed to the Recorder with. The map is copied.
func WithRequestParams(ctx context.Context, params map[string]string) context.Context {
	return context.WithValue(ctx, paramsKey{}, maps.Clone(params))
}

func requestParams(ctx context.Context) map[string]string {
	p, _ := ctx.Value(paramsKey{}).(map[string]string)
	return maps.Clone(p)
}

// Recorder stores a response. It is called for every attempt that received a
// response, whatever its status, on a rule with Record set.
type Recorder interface {
	Record(ctx context.Context, r Recorded) error
}

// Error is the only failure a bank request reports. Its text carries the
// method, path and category and nothing else.
type Error struct {
	Method   string
	Path     string
	Category Category
	// Status is the final HTTP status, or 0 when no response arrived.
	Status int
	// BankCode is the bank's error code string (an API constant such as
	// "bad_request.invalid_time_range"), empty when absent or not well formed.
	BankCode string
}

func (e *Error) Error() string {
	return "bankhttp: " + e.Method + " " + e.Path + ": " + string(e.Category)
}

// Option configures a Client.
type Option func(*Client)

// WithRecorder sets the Recorder.
func WithRecorder(r Recorder) Option { return func(c *Client) { c.recorder = r } }

// WithTimeouts overrides the connect and per-request timeouts.
func WithTimeouts(connect, request time.Duration) Option {
	return func(c *Client) { c.connectTimeout, c.requestTimeout = connect, request }
}

// WithTokenPath sets the path of the token call (default /oauth2/token): a POST
// there is never repeated after a transport failure or a 5xx, since a refresh
// token may have been spent.
func WithTokenPath(path string) Option { return func(c *Client) { c.tokenPath = path } }

// WithoutRetries turns off every retry (429, 5xx and transport failures), so
// one call is exactly one attempt.
func WithoutRetries() Option { return func(c *Client) { c.retries = false } }

// WithClock overrides the clock.
func WithClock(now func() time.Time) Option { return func(c *Client) { c.now = now } }

// WithSleep overrides the pause between retries. It must return early with
// ctx.Err() when ctx ends.
func WithSleep(sleep func(ctx context.Context, d time.Duration) error) Option {
	return func(c *Client) { c.sleep = sleep }
}

// Observer measures bank attempts. The client calls it once for every attempt that ended and once
// for every retry it makes, and gives it the rule's template as the endpoint, never a concrete
// path.
type Observer interface {
	// Attempt reports one attempt: statusClass is "2xx", "4xx" and so on, or "no_response" when
	// the attempt got no HTTP response; d is how long it took.
	Attempt(ctx context.Context, endpoint, statusClass string, d time.Duration)
	// Retry reports that the call is being repeated: reason is rate_limited, server_error,
	// timeout or network.
	Retry(ctx context.Context, endpoint, reason string)
}

// WithTelemetry makes the client start a span for each call and for each attempt on tr, naming
// source on the call's span, and report every attempt and retry to obs. A nil tr or obs turns that
// part off. The spans and the observer see the rule's template and fixed words only: never a
// query, a concrete path, a header or a body. The client sends no trace propagation header to the
// bank, with or without this option.
func WithTelemetry(tr trace.Tracer, source string, obs Observer) Option {
	return func(c *Client) { c.tracer, c.source, c.obs = tr, source, obs }
}

// Client sends allow-listed requests to one bank.
type Client struct {
	base           *url.URL
	rules          map[string]Rule
	templates      []Rule
	tokenPath      string
	retries        bool
	recorder       Recorder
	connectTimeout time.Duration
	requestTimeout time.Duration
	now            func() time.Time
	sleep          func(ctx context.Context, d time.Duration) error
	http           *http.Client
	tracer         trace.Tracer
	source         string
	obs            Observer
}

// New returns a Client for baseURL that allows exactly rules.
func New(baseURL string, rules []Rule, opts ...Option) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("bankhttp: base URL is not an absolute URL")
	}
	c := &Client{
		base:           u,
		rules:          make(map[string]Rule, len(rules)),
		tokenPath:      defaultTokenPath,
		retries:        true,
		connectTimeout: 10 * time.Second,
		requestTimeout: 30 * time.Second,
		now:            time.Now,
		sleep:          sleepCtx,
	}
	for _, r := range rules {
		if strings.Contains(r.Path, idSegment) {
			c.templates = append(c.templates, r)
			continue
		}
		c.rules[ruleKey(r.Method, r.Path)] = r
	}
	for _, o := range opts {
		o(c)
	}
	if c.tracer == nil {
		c.tracer = tracenoop.NewTracerProvider().Tracer("")
	}
	c.http = &http.Client{
		Timeout: c.requestTimeout,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: c.connectTimeout}).DialContext,
			TLSHandshakeTimeout: c.connectTimeout,
			ForceAttemptHTTP2:   true,
		},
		// A redirect would carry the bearer token somewhere unlisted.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c, nil
}

func ruleKey(method, path string) string { return method + " " + path }

// rule returns the rule that allows method and path: an exact rule, else a
// template rule.
func (c *Client) rule(method, path string) (Rule, bool) {
	if r, ok := c.rules[ruleKey(method, path)]; ok {
		return r, true
	}
	for _, r := range c.templates {
		if r.Method == method && matches(r.Path, path) {
			return r, true
		}
	}
	return Rule{}, false
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Do sends req if it is on the allow-list. A non-2xx final response is
// returned together with a *Error.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	rule, ok := c.rule(req.Method, req.Path)
	if !ok {
		return nil, ErrNotAllowed
	}
	if rule.Record && c.recorder == nil {
		return nil, ErrNoRecorder
	}
	// The call's span names the rule's template and the fixed words only: a concrete path holds an
	// id from a bank response, a query holds anything.
	ctx, span := c.tracer.Start(ctx, semconv.SpanBankCallPrefix+req.Method+" "+rule.Path,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String(semconv.SpanSource, c.source),
			attribute.String(semconv.SpanURLTemplate, rule.Path),
			attribute.String(semconv.SpanHTTPRequestMethod, req.Method)))
	defer span.End()
	resp, err := c.do(ctx, req, rule)
	if err != nil {
		var be *Error
		if errors.As(err, &be) {
			span.SetAttributes(attribute.String(semconv.SpanErrorType, string(be.Category)))
			if be.BankCode != "" {
				span.SetAttributes(attribute.String(semconv.SpanBankCode, be.BankCode))
			}
			span.SetStatus(codes.Error, string(be.Category))
		} else {
			// The response could not be recorded: not the bank's doing, and the error text is not
			// for a span.
			span.SetAttributes(attribute.String(semconv.SpanErrorType, "internal"))
			span.SetStatus(codes.Error, "internal")
		}
	}
	return resp, err
}

// do is Do's retry loop for a request the allow-list accepted.
func (c *Client) do(ctx context.Context, req Request, rule Rule) (*Response, error) {
	isToken := req.Method == tokenMethod && req.Path == c.tokenPath
	// What leaves the package names the rule's path: a template's concrete path
	// holds an id from a bank response.
	params := requestParams(ctx)

	var n429, n5xx, nNet int
	for {
		resp, e, err := c.attempt(ctx, req, rule, n429+n5xx+nNet)
		if err != nil {
			cat := networkCategory(ctx, err)
			ne := &Error{Method: req.Method, Path: rule.Path, Category: cat}
			// A token request may have been processed and a refresh token is
			// single-use, so it is never repeated after a transport failure.
			if isToken || !c.retries || ctx.Err() != nil || nNet >= maxRetryNetwork {
				return nil, ne
			}
			if c.sleep(ctx, backoff(nNet)) != nil {
				return nil, ne
			}
			c.retried(ctx, rule.Path, string(cat))
			nNet++
			continue
		}
		if rule.Record {
			rec := Recorded{
				Endpoint:    rule.Path,
				Params:      params,
				Query:       cloneValues(req.Query),
				Status:      resp.Status,
				ContentType: resp.ContentType,
				FetchedAt:   resp.StartedAt,
				Body:        resp.Body,
			}
			if err := c.recorder.Record(ctx, rec); err != nil {
				return resp, fmt.Errorf("bankhttp: %s %s: recording response: %w", req.Method, rule.Path, err)
			}
		}
		if resp.Status >= 200 && resp.Status < 300 {
			return resp, nil
		}
		switch {
		case !c.retries:
			return resp, e
		case resp.Status == http.StatusTooManyRequests && n429 < maxRetries429:
			if c.sleep(ctx, backoff(n429)) != nil {
				return resp, e
			}
			c.retried(ctx, rule.Path, semconv.RetryReasonRateLimited)
			n429++
		case resp.Status >= 500 && !isToken && n5xx < maxRetries5xx:
			if c.sleep(ctx, backoff(n5xx)) != nil {
				return resp, e
			}
			c.retried(ctx, rule.Path, semconv.RetryReasonServerError)
			n5xx++
		default:
			return resp, e
		}
	}
}

func backoff(retry int) time.Duration {
	d := backoffStart
	for range retry {
		d *= 2
		if d >= backoffCeiling {
			return backoffCeiling
		}
	}
	return d
}

// retried tells the observer that the call is being repeated.
func (c *Client) retried(ctx context.Context, endpoint, reason string) {
	if c.obs != nil {
		c.obs.Retry(ctx, endpoint, reason)
	}
}

// attempt is one request, wrapped in its span and reported to the observer. resend is how many
// attempts of this call came before it. The span's url.full is the base URL's scheme, host and
// path plus the rule's template: no query, no userinfo, no concrete path. A response that is not
// 2xx comes back with its classification, worked out once here, for the caller to return or retry
// on; it is nil for a 2xx response and when err is set.
func (c *Client) attempt(ctx context.Context, req Request, rule Rule, resend int) (*Response, *Error, error) {
	port := c.base.Port()
	if port == "" {
		port = "80"
		if c.base.Scheme == "https" {
			port = "443"
		}
	}
	attrs := []attribute.KeyValue{
		attribute.String(semconv.SpanHTTPRequestMethod, req.Method),
		attribute.String(semconv.SpanServerAddress, c.base.Hostname()),
		attribute.String(semconv.SpanURLTemplate, rule.Path),
		attribute.String(semconv.SpanURLFull, c.base.Scheme+"://"+c.base.Host+strings.TrimRight(c.base.Path, "/")+rule.Path),
	}
	if n, err := strconv.Atoi(port); err == nil {
		attrs = append(attrs, attribute.Int(semconv.SpanServerPort, n))
	}
	if resend > 0 {
		attrs = append(attrs, attribute.Int(semconv.SpanHTTPRequestResendCount, resend))
	}
	actx, span := c.tracer.Start(ctx, req.Method+" "+rule.Path,
		trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	defer span.End()

	// The wall clock, not c.now: the client clock is injectable and a test counts its calls.
	began := time.Now()
	resp, err := c.send(actx, req)
	took := time.Since(began)

	// No usable HTTP response is no_response: none arrived, or its body could not be read.
	class := semconv.StatusClassNoResponse
	var classified *Error
	switch {
	case err != nil:
		cat := networkCategory(ctx, err)
		span.SetAttributes(attribute.String(semconv.SpanErrorType, string(cat)))
		span.SetStatus(codes.Error, string(cat))
	default:
		class = statusClass(resp.Status)
		span.SetAttributes(attribute.Int(semconv.SpanHTTPResponseStatusCode, resp.Status))
		if resp.Status < 200 || resp.Status >= 300 {
			// What leaves the package names the rule's path: a template's concrete path
			// holds an id from a bank response.
			named := req
			named.Path = rule.Path
			classified = classify(named, resp)
			span.SetAttributes(attribute.String(semconv.SpanErrorType, string(classified.Category)))
			span.SetStatus(codes.Error, string(classified.Category))
		}
	}
	if c.obs != nil {
		c.obs.Attempt(actx, rule.Path, class, took)
	}
	return resp, classified, err
}

// statusClass is "2xx", "4xx" and so on. A status outside 100 to 599 is not a usable response and
// takes the no_response class.
func statusClass(status int) string {
	if status < 100 || status > 599 {
		return semconv.StatusClassNoResponse
	}
	return strconv.Itoa(status/100) + "xx"
}

func (c *Client) send(ctx context.Context, req Request) (*Response, error) {
	u := *c.base
	u.Path = strings.TrimRight(c.base.Path, "/") + req.Path
	u.RawPath = ""
	u.RawQuery = req.Query.Encode()

	var body io.Reader
	if req.Form != nil {
		body = strings.NewReader(req.Form.Encode())
	}
	hr, err := http.NewRequestWithContext(ctx, req.Method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if req.Form != nil {
		hr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if req.Bearer != "" {
		hr.Header.Set("Authorization", "Bearer "+req.Bearer)
	}

	start := c.now()
	hresp, err := c.http.Do(hr)
	if err != nil {
		return nil, err
	}
	defer hresp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(hresp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBodyBytes {
		return nil, errors.New("response too large")
	}
	return &Response{
		Status:      hresp.StatusCode,
		ContentType: hresp.Header.Get("Content-Type"),
		Body:        data,
		StartedAt:   start,
		Duration:    c.now().Sub(start),
	}, nil
}

func cloneValues(v url.Values) map[string][]string {
	out := make(map[string][]string, len(v))
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

func networkCategory(ctx context.Context, err error) Category {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return CategoryTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return CategoryTimeout
	}
	return CategoryNetwork
}

var bankCodePattern = regexp.MustCompile(`^[a-z0-9_.]{1,100}$`)

func classify(req Request, resp *Response) *Error {
	e := &Error{Method: req.Method, Path: req.Path, Status: resp.Status}
	// The code is read from "code" (Monzo) or, failing that, "error" (OAuth and
	// TrueLayer); a field that is not a well-formed code string is ignored.
	var parsed map[string]json.RawMessage
	if json.NewDecoder(bytes.NewReader(resp.Body)).Decode(&parsed) == nil {
		for _, field := range []string{"code", "error"} {
			var code string
			if json.Unmarshal(parsed[field], &code) == nil && bankCodePattern.MatchString(code) {
				e.BankCode = code
				break
			}
		}
	}
	switch {
	case resp.Status == http.StatusTooManyRequests:
		e.Category = CategoryRateLimited
	case resp.Status >= 500:
		e.Category = CategoryServerError
	case resp.Status == http.StatusUnauthorized:
		e.Category = CategoryUnauthorised
	case resp.Status == http.StatusBadRequest:
		e.Category = CategoryBadRequest
	case resp.Status == http.StatusForbidden && e.BankCode == "forbidden.insufficient_permissions":
		e.Category = CategoryForbiddenInsufficientPermissions
	case resp.Status == http.StatusForbidden && e.BankCode == "forbidden.verification_required":
		e.Category = CategoryForbiddenVerificationRequired
	default:
		e.Category = CategoryUnexpectedStatus
	}
	return e
}

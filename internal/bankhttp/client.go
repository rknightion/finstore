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
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
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
	tokenPath   = "/oauth2/token" //nolint:gosec // an endpoint path, not a credential

	maxRetries429   = 6
	maxRetries5xx   = 3
	maxRetryNetwork = 3
	backoffStart    = 2 * time.Second
	backoffCeiling  = 30 * time.Second
	maxBodyBytes    = 64 << 20
)

// Rule allows one method and exact path. Record says whether every response
// to it is handed to the Recorder.
type Rule struct {
	Method, Path string
	Record       bool
}

// MonzoRules is the allow-list for the Monzo API.
func MonzoRules() []Rule {
	rules := []Rule{{Method: tokenMethod, Path: tokenPath}}
	for _, p := range []string{"/ping/whoami", "/accounts", "/balance", "/pots", "/transactions", "/webhooks"} {
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

// Recorded is one response handed to the Recorder.
type Recorded struct {
	Endpoint    string
	Query       map[string][]string
	Status      int
	ContentType string
	FetchedAt   time.Time
	Body        []byte
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

// WithClock overrides the clock.
func WithClock(now func() time.Time) Option { return func(c *Client) { c.now = now } }

// WithSleep overrides the pause between retries. It must return early with
// ctx.Err() when ctx ends.
func WithSleep(sleep func(ctx context.Context, d time.Duration) error) Option {
	return func(c *Client) { c.sleep = sleep }
}

// Client sends allow-listed requests to one bank.
type Client struct {
	base           *url.URL
	rules          map[string]Rule
	recorder       Recorder
	connectTimeout time.Duration
	requestTimeout time.Duration
	now            func() time.Time
	sleep          func(ctx context.Context, d time.Duration) error
	http           *http.Client
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
		connectTimeout: 10 * time.Second,
		requestTimeout: 30 * time.Second,
		now:            time.Now,
		sleep:          sleepCtx,
	}
	for _, r := range rules {
		c.rules[ruleKey(r.Method, r.Path)] = r
	}
	for _, o := range opts {
		o(c)
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
	rule, ok := c.rules[ruleKey(req.Method, req.Path)]
	if !ok {
		return nil, ErrNotAllowed
	}
	if rule.Record && c.recorder == nil {
		return nil, ErrNoRecorder
	}
	isToken := req.Method == tokenMethod && req.Path == tokenPath

	var n429, n5xx, nNet int
	for {
		resp, err := c.attempt(ctx, req)
		if err != nil {
			cat := networkCategory(ctx, err)
			e := &Error{Method: req.Method, Path: req.Path, Category: cat}
			// A token request may have been processed and a refresh token is
			// single-use, so it is never repeated after a transport failure.
			if isToken || ctx.Err() != nil || nNet >= maxRetryNetwork {
				return nil, e
			}
			if c.sleep(ctx, backoff(nNet)) != nil {
				return nil, e
			}
			nNet++
			continue
		}
		if rule.Record {
			rec := Recorded{
				Endpoint:    req.Path,
				Query:       cloneValues(req.Query),
				Status:      resp.Status,
				ContentType: resp.ContentType,
				FetchedAt:   resp.StartedAt,
				Body:        resp.Body,
			}
			if err := c.recorder.Record(ctx, rec); err != nil {
				return resp, fmt.Errorf("bankhttp: %s %s: recording response: %w", req.Method, req.Path, err)
			}
		}
		if resp.Status >= 200 && resp.Status < 300 {
			return resp, nil
		}
		e := classify(req, resp)
		switch {
		case resp.Status == http.StatusTooManyRequests && n429 < maxRetries429:
			if c.sleep(ctx, backoff(n429)) != nil {
				return resp, e
			}
			n429++
		case resp.Status >= 500 && !isToken && n5xx < maxRetries5xx:
			if c.sleep(ctx, backoff(n5xx)) != nil {
				return resp, e
			}
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

func (c *Client) attempt(ctx context.Context, req Request) (*Response, error) {
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
	var parsed struct {
		Code string `json:"code"`
	}
	if json.NewDecoder(bytes.NewReader(resp.Body)).Decode(&parsed) == nil && bankCodePattern.MatchString(parsed.Code) {
		e.BankCode = parsed.Code
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

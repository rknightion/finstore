package truelayer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rknightion/finstore/internal/bankhttp"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry"
)

// SourcePrefix begins the source name of every TrueLayer connection.
const SourcePrefix = "truelayer:"

// Source is the source name of a connection: truelayer:ob-lloyds or truelayer:ob-monzo.
func Source(connection string) string { return SourcePrefix + connection }

// errNoFetch is returned by the recorder when a recorded request was made outside a fetch.
var errNoFetch = errors.New("truelayer: response received outside a fetch")

// errDailyLimit stops a run before a call that would be the day's fifth to the same endpoint and
// bank reference.
var errDailyLimit = errors.New("truelayer: daily call limit reached")

type (
	fetchKey struct{}
	tallyKey struct{}
	tokenKey struct{}
)

// withFetch makes every response recorded under ctx belong to raw.fetch id.
func withFetch(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, fetchKey{}, id)
}

// withTally makes every request made under ctx count towards tally's run.
func withTally(ctx context.Context, t *runTally) context.Context {
	return context.WithValue(ctx, tallyKey{}, t)
}

// withAccessToken makes every call under ctx use token as it is, never refreshed: the connect-time
// fetch runs on the token the code exchange returned, the only one TrueLayer gives full history to.
func withAccessToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, tokenKey{}, token)
}

func countRequest(ctx context.Context) {
	if t, ok := ctx.Value(tallyKey{}).(*runTally); ok {
		t.requests.Add(1)
	}
}

// recorder writes every response of a recorded rule to raw.api_response under the fetch carried
// by the request's context, committed on the pool as it arrives, with the query and the request
// parameters the worker put on the context. It also counts the request.
type recorder struct {
	q       store.DBTX
	source  string
	metrics *telemetry.Metrics
}

func (r recorder) Record(ctx context.Context, rec bankhttp.Recorded) error {
	r.metrics.BankRequest(ctx, r.source, rec.Endpoint, statusClass(rec.Status))
	countRequest(ctx)
	id, ok := ctx.Value(fetchKey{}).(int64)
	if !ok || id == 0 {
		return errNoFetch
	}
	obj := map[string]any{}
	for k, vs := range rec.Query {
		switch len(vs) {
		case 0:
		case 1:
			obj[k] = vs[0]
		default:
			obj[k] = vs
		}
	}
	for k, v := range rec.Params {
		obj[k] = v
	}
	params, err := json.Marshal(obj)
	if err != nil {
		return errors.New("truelayer: encode request parameters")
	}
	_, err = store.InsertAPIResponse(ctx, r.q, store.APIResponse{
		FetchID: id, Source: r.source, Endpoint: rec.Endpoint, Params: params, Status: rec.Status,
		ContentType: rec.ContentType, FetchedAt: rec.FetchedAt, Body: rec.Body,
	})
	return err
}

// transportFailure stores a status 0 row with no body for a call that got no whole response,
// under the fetch carried by ctx, with the call's query and parameters. It is written even when ctx
// has ended, since the call may have reached the bank.
func (r recorder) transportFailure(ctx context.Context, endpoint string, query url.Values, params map[string]string, at time.Time) error {
	ctx = context.WithoutCancel(ctx)
	id, ok := ctx.Value(fetchKey{}).(int64)
	if !ok || id == 0 {
		return errNoFetch
	}
	obj := map[string]any{}
	for k, vs := range query {
		switch len(vs) {
		case 0:
		case 1:
			obj[k] = vs[0]
		default:
			obj[k] = vs
		}
	}
	for k, v := range params {
		obj[k] = v
	}
	p, err := json.Marshal(obj)
	if err != nil {
		return errors.New("truelayer: encode request parameters")
	}
	_, err = store.InsertAPIResponse(ctx, r.q, store.APIResponse{FetchID: id, Source: r.source, Endpoint: endpoint,
		Params: p, Status: 0, FetchedAt: at, Body: []byte{}})
	return err
}

func statusClass(status int) string {
	if status < 100 || status > 599 {
		return "none"
	}
	return strconv.Itoa(status/100) + "xx"
}

// api is the typed surface over the Data API client, whose retries are all off: one call is one
// recorded attempt. Before every call it counts that day's calls to the same endpoint and bank
// reference in raw.api_response and refuses the call at the limit.
type api struct {
	bank    *bankhttp.Client
	tokens  *tokens
	metrics *telemetry.Metrics
	st      *store.Store
	source  string
	loc     *time.Location
	now     func() time.Time
	limit   int
	rec     recorder
}

// call is one Data API request: an endpoint template, the TrueLayer account id that fills it, the
// bank reference the worker resolved for that account, and the query.
type call struct {
	template  string
	accountID string
	bankRef   string
	query     url.Values
}

// get sends one recorded GET, recorded under the fetch carried by ctx with the account id and
// bank reference as request parameters.
func (a *api) get(ctx context.Context, c call) (*bankhttp.Response, error) {
	n, err := store.CountCallsToday(ctx, a.st.DB(), a.source, c.template, c.bankRef, a.now().In(a.loc))
	if err != nil {
		return nil, err
	}
	if n >= a.limit {
		return nil, errDailyLimit
	}
	tok, ok := ctx.Value(tokenKey{}).(string)
	if !ok {
		if tok, err = a.tokens.access(ctx); err != nil {
			return nil, err
		}
	}
	params := map[string]string{}
	if c.accountID != "" {
		params[paramAccountID] = c.accountID
	}
	if c.bankRef != "" {
		params[paramBankRef] = c.bankRef
	}
	path := strings.Replace(c.template, "{id}", c.accountID, 1)
	resp, err := a.bank.Do(bankhttp.WithRequestParams(ctx, params),
		bankhttp.Request{Method: http.MethodGet, Path: path, Query: c.query, Bearer: tok})
	if resp == nil && err != nil {
		var be *bankhttp.Error
		if errors.As(err, &be) {
			a.metrics.BankRequest(ctx, a.source, c.template, "none")
			countRequest(ctx)
			// A transport failure (no whole response) may still have reached the bank and counted
			// against its daily limit, so it is stored too, as a status 0 row with no body, for
			// CountCallsToday to see. Normalisers ignore it: it is not a 2xx.
			if rerr := a.rec.transportFailure(ctx, c.template, c.query, params, a.now()); rerr != nil {
				return nil, errors.Join(err, rerr)
			}
		}
	}
	return resp, err
}

// meInfo is what /data/v1/me reports about the consent: when it ends and the scopes it was granted.
type meInfo struct {
	Expiry time.Time
	Scopes []string
}

// me returns the consent's end and granted scopes as /data/v1/me reports them. It is an error when
// the body names no readable consent expiry.
func (a *api) me(ctx context.Context) (meInfo, error) {
	resp, err := a.get(ctx, call{template: pathMe})
	if err != nil {
		return meInfo{}, err
	}
	items, err := decodeResults(resp.Body, pathMe)
	if err != nil {
		return meInfo{}, err
	}
	for _, raw := range items {
		var m struct {
			ConsentExpiresAt string   `json:"consent_expires_at"`
			Scopes           []string `json:"scopes"`
		}
		if decode(raw, &m) != nil {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, m.ConsentExpiresAt); err == nil {
			return meInfo{Expiry: t, Scopes: m.Scopes}, nil
		}
	}
	return meInfo{}, errBody(pathMe, "no consent expiry")
}

// info reads /data/v1/info. The answer is only stored: nothing in it is read.
func (a *api) info(ctx context.Context) error {
	_, err := a.get(ctx, call{template: pathInfo})
	return err
}

// listedAccount is an account from /data/v1/accounts: TrueLayer's id for this call, its type and
// the bank reference it is known by.
type listedAccount struct {
	id, typ, bankRef string
}

func (a *api) accounts(ctx context.Context) ([]listedAccount, error) {
	resp, err := a.get(ctx, call{template: pathAccounts})
	if err != nil {
		return nil, err
	}
	items, err := decodeResults(resp.Body, pathAccounts)
	if err != nil {
		return nil, err
	}
	out := make([]listedAccount, 0, len(items))
	for _, raw := range items {
		var x accountItem
		if decode(raw, &x) != nil || x.AccountID == "" {
			return nil, errBody(pathAccounts, "an account has no id")
		}
		out = append(out, listedAccount{id: x.AccountID, typ: x.AccountType, bankRef: x.bankRef()})
	}
	return out, nil
}

func (a *api) read(ctx context.Context, template string, acct listedAccount) error {
	_, err := a.get(ctx, call{template: template, accountID: acct.id, bankRef: acct.bankRef})
	return err
}

// transactions fetches the transactions of acct (an account, or a card with pathCardTransactions as
// template) dated from to to, both inclusive, and returns how many rows came back.
func (a *api) transactions(ctx context.Context, template string, acct listedAccount, from, to time.Time) (int, error) {
	resp, err := a.get(ctx, call{template: template, accountID: acct.id, bankRef: acct.bankRef,
		query: url.Values{paramFrom: {from.Format(time.DateOnly)}, paramTo: {to.Format(time.DateOnly)}}})
	if err != nil {
		return 0, err
	}
	items, err := decodeResults(resp.Body, template)
	return len(items), err
}

// cards reads /data/v1/cards and returns the cards a run can key (usableCards).
func (a *api) cards(ctx context.Context) (cardList, error) {
	resp, err := a.get(ctx, call{template: pathCards})
	if err != nil {
		return cardList{}, err
	}
	return usableCards(resp.Body)
}

// listCards reads /data/v1/cards for the Monzo connection, which only stores the answer.
func (a *api) listCards(ctx context.Context) error {
	_, err := a.get(ctx, call{template: pathCards})
	return err
}

// runTally collects what a run did.
type runTally struct {
	requests atomic.Int64
	mu       sync.Mutex
	rows     int
	partial  bool
	category string
	// listed is set once the account list arrived; historyPending counts the accounts whose
	// history has still to arrive whole.
	listed         bool
	historyPending int
}

func (t *runTally) setListed() {
	t.mu.Lock()
	t.listed = true
	t.mu.Unlock()
}

func (t *runTally) addPendingHistory() {
	t.mu.Lock()
	t.historyPending++
	t.mu.Unlock()
}

// historyDone records that one account's history arrived whole.
func (t *runTally) historyDone() {
	t.mu.Lock()
	t.historyPending--
	t.mu.Unlock()
}

// historyComplete reports whether every listed account's history arrived whole.
func (t *runTally) historyComplete() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.listed && t.historyPending <= 0
}

func (t *runTally) addRows(n int) {
	t.mu.Lock()
	t.rows += n
	t.mu.Unlock()
}

// fail marks the run partial and keeps the first category.
func (t *runTally) fail(category string) {
	t.mu.Lock()
	t.partial = true
	if t.category == "" {
		t.category = category
	}
	t.mu.Unlock()
}

// failHistory marks a connect run partial with categoryHistoryFailed, which outranks any category
// recorded before: it is the one that alerts.
func (t *runTally) failHistory() {
	t.mu.Lock()
	t.partial = true
	t.category = categoryHistoryFailed
	t.mu.Unlock()
}

// Run categories this package adds to the bank client's.
const (
	categoryRateLimited   = "rate_limited"
	categoryHistoryFailed = "history_failed"
	categoryConsentLost   = "consent_lost"
	// categorySuperseded ends a connect run that a newer connect of the same source replaced.
	categorySuperseded = "superseded"
	// categoryInterrupted ends a Monzo connect run that stopped before its fetch was done.
	categoryInterrupted = "interrupted"
	// categoryCardKeyShared marks a run in which two cards shared their last four digits and were skipped.
	categoryCardKeyShared = "card_key_shared"
	// categoryCardsRefused marks a run in which the bank refused a card read: the consent is kept and the
	// run makes no further card call.
	categoryCardsRefused = "cards_refused"
)

// category is the error category of err for logs and sync.run.
func category(err error) string {
	var be *bankhttp.Error
	switch {
	case errors.Is(err, errDailyLimit):
		return categoryRateLimited
	case errors.Is(err, ErrConsentLost) || isAccessDenied(err):
		return categoryConsentLost
	case errors.As(err, &be):
		return string(be.Category)
	case errors.Is(err, errUnexpectedBody):
		return "unexpected_body"
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	}
	return "internal"
}

// isRateLimited reports whether err is the daily limit or a 429: either ends the run, never the
// consent.
func isRateLimited(err error) bool {
	var be *bankhttp.Error
	return errors.Is(err, errDailyLimit) || (errors.As(err, &be) && be.Status == http.StatusTooManyRequests)
}

// isAccessDenied reports whether a data call was refused for access (401, or an access_denied
// error), which ends the consent. A refused token call is not a data call: a refresh ends the
// consent only on invalid_grant (isRejection), and a 401 there means the client credentials are
// wrong.
func isAccessDenied(err error) bool {
	var be *bankhttp.Error
	return errors.As(err, &be) && be.Path != bankhttp.TrueLayerAuthTokenPath &&
		(be.Status == http.StatusUnauthorized || be.BankCode == "access_denied")
}

// isRefused reports whether a data call was refused by the bank: a 401, a 403 or an access_denied
// error. A refused token call is not a data call. On a ledger read it ends the consent (see
// isAccessDenied); on an optional read it is only counted.
func isRefused(err error) bool {
	var be *bankhttp.Error
	return errors.As(err, &be) && be.Path != bankhttp.TrueLayerAuthTokenPath &&
		(be.Status == http.StatusUnauthorized || be.Status == http.StatusForbidden || be.BankCode == "access_denied")
}

// isUnsupported reports whether the bank answered a data call by saying it does not offer the
// endpoint: a 501, or the bank code endpoint_not_supported or not_supported.
func isUnsupported(err error) bool {
	var be *bankhttp.Error
	return errors.As(err, &be) && be.Path != bankhttp.TrueLayerAuthTokenPath &&
		(be.Status == http.StatusNotImplemented || be.BankCode == "endpoint_not_supported" || be.BankCode == "not_supported")
}

// isTokenCall reports whether err is a failed call to the token endpoint.
func isTokenCall(err error) bool {
	var be *bankhttp.Error
	return errors.As(err, &be) && be.Path == bankhttp.TrueLayerAuthTokenPath
}

// isBankFailure reports whether err is a failed request or a response that could not be read, as
// opposed to a store failure, a lost consent or a cancelled context.
func isBankFailure(err error) bool {
	var be *bankhttp.Error
	return errors.As(err, &be) || errors.Is(err, errUnexpectedBody)
}

// stops reports whether err ends the run rather than only marking it partial: anything that is not
// a bank failure, a lost consent, a refused data call, or the rate limit.
// A failed token call stops it too: every later call would only try the refresh again.
func stops(err error) bool {
	var be *bankhttp.Error
	tokenCall := errors.As(err, &be) && be.Path == bankhttp.TrueLayerAuthTokenPath
	return errors.Is(err, ErrConsentLost) || tokenCall || !isBankFailure(err) || isAccessDenied(err) || isRateLimited(err)
}

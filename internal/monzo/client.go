package monzo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/rknightion/finstore/internal/bankhttp"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry"
)

// Source is the value of every source column and attribute this package writes.
const Source = "monzo"

// PageSize is the number of transactions asked for per page, the API's maximum.
const PageSize = 100

// errNoFetch is returned by the recorder when a recorded request was made outside a fetch.
var errNoFetch = errors.New("monzo: response received outside a fetch")

type fetchKey struct{}

type tallyKey struct{}

// withTally makes every request made under ctx count towards tally's run.
func withTally(ctx context.Context, t *runTally) context.Context {
	return context.WithValue(ctx, tallyKey{}, t)
}

func countRequest(ctx context.Context) {
	if t, ok := ctx.Value(tallyKey{}).(*runTally); ok {
		t.requests.Add(1)
	}
}

// withFetch makes every response recorded under ctx belong to raw.fetch id.
func withFetch(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, fetchKey{}, id)
}

// recorder writes every response of a recorded rule to raw.api_response under the fetch carried
// by the request's context, committed on the pool as it arrives. It also counts the request.
type recorder struct {
	q       store.DBTX
	metrics *telemetry.Metrics
}

func (r recorder) Record(ctx context.Context, rec bankhttp.Recorded) error {
	r.metrics.BankRequest(ctx, Source, rec.Endpoint, statusClass(rec.Status))
	countRequest(ctx)
	id, ok := ctx.Value(fetchKey{}).(int64)
	if !ok || id == 0 {
		return errNoFetch
	}
	params, err := encodeParams(rec.Query)
	if err != nil {
		return err
	}
	_, err = store.InsertAPIResponse(ctx, r.q, store.APIResponse{
		FetchID: id, Source: Source, Endpoint: rec.Endpoint, Params: params, Status: rec.Status,
		ContentType: rec.ContentType, FetchedAt: rec.FetchedAt, Body: rec.Body,
	})
	return err
}

// encodeParams encodes query parameters as a JSON object: a parameter with one value as a string,
// one with several as an array of strings.
func encodeParams(q map[string][]string) (json.RawMessage, error) {
	obj := make(map[string]any, len(q))
	for k, vs := range q {
		switch len(vs) {
		case 0:
		case 1:
			obj[k] = vs[0]
		default:
			obj[k] = vs
		}
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, errors.New("monzo: encode request parameters")
	}
	return b, nil
}

// api is the typed surface over the bank client. Every call takes a fresh access token from
// tokens.
type api struct {
	bank    *bankhttp.Client
	tokens  *tokens
	metrics *telemetry.Metrics
}

// get sends one recorded GET. The response is recorded under the fetch carried by ctx.
func (a *api) get(ctx context.Context, path string, q url.Values) (*bankhttp.Response, error) {
	tok, err := a.tokens.access(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := a.bank.Do(ctx, bankhttp.Request{Method: http.MethodGet, Path: path, Query: q, Bearer: tok})
	if resp == nil && err != nil {
		var be *bankhttp.Error
		if errors.As(err, &be) {
			a.metrics.BankRequest(ctx, Source, path, "none")
			countRequest(ctx)
		}
	}
	return resp, err
}

// account is the part of an account object this package acts on.
type account struct {
	ID      string
	Created time.Time
	Closed  bool
}

func (a *api) accounts(ctx context.Context) ([]account, error) {
	resp, err := a.get(ctx, pathAccounts, nil)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Accounts []struct {
			ID      string `json:"id"`
			Created string `json:"created"`
			Closed  bool   `json:"closed"`
		} `json:"accounts"`
	}
	if err := decode(resp.Body, &doc); err != nil {
		return nil, errBody(pathAccounts, "not the expected JSON")
	}
	out := make([]account, 0, len(doc.Accounts))
	for _, x := range doc.Accounts {
		created, ok := parseTime(x.Created)
		if x.ID == "" || !ok {
			return nil, errBody(pathAccounts, "an account has no id or no valid created time")
		}
		out = append(out, account{ID: x.ID, Created: created, Closed: x.Closed})
	}
	return out, nil
}

func (a *api) balance(ctx context.Context, accountID string) error {
	_, err := a.get(ctx, pathBalance, url.Values{"account_id": {accountID}})
	return err
}

func (a *api) pots(ctx context.Context, accountID string) error {
	_, err := a.get(ctx, pathPots, url.Values{"current_account_id": {accountID}})
	return err
}

func (a *api) webhooks(ctx context.Context, accountID string) error {
	_, err := a.get(ctx, pathWebhooks, url.Values{"account_id": {accountID}})
	return err
}

// txRow is the part of a transaction this package pages by.
type txRow struct {
	ID      string
	Created time.Time
}

// transactionsPage fetches one page: up to PageSize transactions of accountID from since (an RFC
// 3339 time, inclusive, or a transaction id, exclusive of its timestamp) to before, with the
// merchant expanded. It returns the page's rows in order.
func (a *api) transactionsPage(ctx context.Context, accountID, since string, before time.Time) ([]txRow, error) {
	q := url.Values{
		"account_id": {accountID},
		"expand[]":   {"merchant"},
		"limit":      {strconv.Itoa(PageSize)},
		"since":      {since},
		"before":     {formatTime(before)},
	}
	resp, err := a.get(ctx, pathTransactions, q)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Transactions []struct {
			ID      string `json:"id"`
			Created string `json:"created"`
		} `json:"transactions"`
	}
	if err := decode(resp.Body, &doc); err != nil {
		return nil, errBody(pathTransactions, "not the expected JSON")
	}
	out := make([]txRow, 0, len(doc.Transactions))
	for _, x := range doc.Transactions {
		created, ok := parseTime(x.Created)
		if x.ID == "" || !ok {
			return nil, errBody(pathTransactions, "a transaction has no id or no valid created time")
		}
		out = append(out, txRow{ID: x.ID, Created: created})
	}
	return out, nil
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// category is the error category of err for logs and sync.run: the bank client's category, or
// "internal" for anything else.
func category(err error) string {
	var be *bankhttp.Error
	if errors.As(err, &be) {
		return string(be.Category)
	}
	if errors.Is(err, ErrConsentLost) {
		return "consent_lost"
	}
	// A deadline is a timeout and a real failure; only a cancellation (serve stopping, or a newer
	// connect replacing this work) is "cancelled", which the state gauges skip.
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "internal"
}

// isBankFailure reports whether err is a failed request or a response that could not be read,
// as opposed to a store failure, a lost consent or a cancelled context.
func isBankFailure(err error) bool {
	var be *bankhttp.Error
	return errors.As(err, &be) || errors.Is(err, errUnexpectedBody)
}

func hasCategory(err error, c bankhttp.Category) bool {
	var be *bankhttp.Error
	return errors.As(err, &be) && be.Category == c
}

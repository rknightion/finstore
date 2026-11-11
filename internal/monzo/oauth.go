package monzo

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rknightion/finstore/internal/bankhttp"
	"github.com/rknightion/finstore/internal/consent"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry"
)

// RefreshMargin is how much of an access token's life must remain for it to be used without a
// refresh.
const RefreshMargin = time.Hour

const tokenPath = "/oauth2/token" //nolint:gosec // an endpoint path, not a credential

// ErrConsentLost is returned when the bank rejected a token refresh, which ends the consent, and
// by a scheduled run that ended the consent after a 403 forbidden.insufficient_permissions.
var ErrConsentLost = errors.New("monzo: consent lost")

// ErrNoToken is returned when no token is stored.
var ErrNoToken = errors.New("monzo: no stored token")

// errTokenResponse is returned for a 2xx token response that cannot be understood.
var errTokenResponse = errors.New("monzo: token response is not the expected JSON")

// tokenPair is a parsed token response. Its fields are never logged or put in an error.
type tokenPair struct {
	access, refresh string
	issuedAt        time.Time
	expiresAt       time.Time
}

// oauthClient makes the two token calls.
type oauthClient struct {
	bank                   *bankhttp.Client
	clientID, clientSecret string
	redirectURI            string
	metrics                *telemetry.Metrics
}

func (o *oauthClient) post(ctx context.Context, form url.Values) (tokenPair, error) {
	form.Set("client_id", o.clientID)
	form.Set("client_secret", o.clientSecret)
	resp, err := o.bank.Do(ctx, bankhttp.Request{Method: http.MethodPost, Path: tokenPath, Form: form})
	class := "none"
	if resp != nil {
		class = statusClass(resp.Status)
	}
	o.metrics.BankRequest(ctx, Source, tokenPath, class)
	if err != nil {
		return tokenPair{}, err
	}
	var doc struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if decode(resp.Body, &doc) != nil || doc.AccessToken == "" || doc.RefreshToken == "" || doc.ExpiresIn <= 0 {
		return tokenPair{}, errTokenResponse
	}
	// The request's start is no later than the moment the bank issued the token, so a deadline
	// counted from it is never later than the bank's.
	issued := resp.StartedAt
	return tokenPair{
		access: doc.AccessToken, refresh: doc.RefreshToken, issuedAt: issued,
		expiresAt: issued.Add(time.Duration(doc.ExpiresIn) * time.Second),
	}, nil
}

// exchange trades an authorisation code for a token pair.
func (o *oauthClient) exchange(ctx context.Context, code string) (tokenPair, error) {
	return o.post(ctx, url.Values{
		"grant_type":   {"authorization_code"},
		"redirect_uri": {o.redirectURI},
		"code":         {code},
	})
}

// refresh trades a refresh token for a new pair. Monzo's refresh tokens are single use.
func (o *oauthClient) refresh(ctx context.Context, refreshToken string) (tokenPair, error) {
	return o.post(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
}

// authoriseURL is the page the owner is sent to.
func authoriseURL(authURL, clientID, redirectURI, state string) (string, error) {
	u, err := url.Parse(authURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("monzo: auth URL is not an absolute URL")
	}
	q := u.Query()
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("state", state)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// redirectURI is the callback the bank sends the owner back to.
func redirectURI(publicURL string) string {
	return strings.TrimRight(publicURL, "/") + "/callback/monzo"
}

// tokens hands out a usable access token, refreshing it when under RefreshMargin of its life
// remains. Only one goroutine refreshes at a time, and a refreshed pair is committed on the pool
// before its access token is returned.
type tokens struct {
	mu       sync.Mutex
	st       *store.Store
	oauth    *oauthClient
	consents *consent.Manager
	now      func() time.Time
	// onLost is called after a rejected refresh has ended the consent.
	onLost func(ctx context.Context, c store.Consent)
}

func (t *tokens) access(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tok, err := store.GetOAuthToken(ctx, t.st.DB(), Source)
	if errors.Is(err, store.ErrNotFound) {
		return "", ErrNoToken
	}
	if err != nil {
		return "", err
	}
	if tok.ExpiresAt.Sub(t.now()) >= RefreshMargin {
		return tok.AccessToken, nil
	}
	pair, err := t.oauth.refresh(ctx, tok.RefreshToken)
	if err != nil {
		var be *bankhttp.Error
		if errors.As(err, &be) && be.Status >= 400 && be.Status < 500 {
			return "", t.rejected(ctx, err)
		}
		// A network failure or a server error: the refresh token may or may not have been spent.
		// It is not retried here; the next caller tries again and a rejection then ends the consent.
		return "", err
	}
	// Durable before use: Monzo has already invalidated the old pair.
	if err := store.PutOAuthToken(ctx, t.st.DB(), store.OAuthToken{
		Source: Source, AccessToken: pair.access, RefreshToken: pair.refresh,
		IssuedAt: pair.issuedAt, ExpiresAt: pair.expiresAt, UpdatedAt: t.now(),
	}); err != nil {
		return "", err
	}
	return pair.access, nil
}

// rejected ends the current consent after a refresh the bank refused.
func (t *tokens) rejected(ctx context.Context, cause error) error {
	cur, err := t.consents.Current(ctx, t.st.DB())
	if err != nil {
		if errors.Is(err, consent.ErrNoConsent) {
			return errors.Join(ErrConsentLost, cause)
		}
		return err
	}
	c, err := t.consents.Lost(ctx, t.st.DB(), cur.ID, consent.ReasonRefreshRejected)
	if err != nil && !errors.Is(err, consent.ErrIllegalTransition) {
		return err
	}
	if err == nil && t.onLost != nil {
		t.onLost(ctx, c)
	}
	return errors.Join(ErrConsentLost, cause)
}

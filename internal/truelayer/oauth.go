package truelayer

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/bankhttp"
	"github.com/rknightion/finstore/internal/consent"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry"
)

// RefreshMargin is how much of an access token's life must remain for it to be used without a
// refresh. TrueLayer's access tokens last an hour.
const RefreshMargin = 5 * time.Minute

// Scopes is what a connect asks the bank for.
const Scopes = "info accounts balance transactions direct_debits standing_orders offline_access cards"

// providersAll lists the provider types the auth dialog offers; provider_id preselects one of them.
const providersAll = "uk-ob-all"

// ErrConsentLost is returned when the bank refused a token refresh (invalid_grant) or a data call
// (401 or access_denied), either of which ends the consent.
var ErrConsentLost = errors.New("truelayer: consent lost")

// ErrNoToken is returned when no token is stored.
var ErrNoToken = errors.New("truelayer: no stored token")

// errTokenResponse is returned for a 2xx token response that cannot be understood.
var errTokenResponse = errors.New("truelayer: token response is not the expected JSON")

// errTokenReplaced is returned when a refreshed pair arrives after the stored pair has been
// replaced by another writer; the refreshed pair is dropped.
var errTokenReplaced = errors.New("truelayer: stored token was replaced during a refresh")

// tokenPair is a parsed token response. Its fields are never logged or put in an error. refresh
// is empty when the response carried none.
type tokenPair struct {
	access, refresh string
	issuedAt        time.Time
	expiresAt       time.Time
}

// oauthClient makes the two token calls on TrueLayer's auth server.
type oauthClient struct {
	bank                   *bankhttp.Client
	clientID, clientSecret string
	redirectURI            string
	source                 string
	metrics                *telemetry.Metrics
}

func (o *oauthClient) post(ctx context.Context, form url.Values) (tokenPair, error) {
	form.Set("client_id", o.clientID)
	form.Set("client_secret", o.clientSecret)
	resp, err := o.bank.Do(ctx, bankhttp.Request{Method: http.MethodPost, Path: bankhttp.TrueLayerAuthTokenPath, Form: form})
	class := "none"
	if resp != nil {
		class = statusClass(resp.Status)
	}
	o.metrics.BankRequest(ctx, o.source, bankhttp.TrueLayerAuthTokenPath, class)
	if err != nil {
		return tokenPair{}, err
	}
	var doc struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if decode(resp.Body, &doc) != nil || doc.AccessToken == "" || doc.ExpiresIn <= 0 {
		return tokenPair{}, errTokenResponse
	}
	// The request's start is no later than the moment the bank issued the token, so a deadline
	// counted from it is never later than the bank's.
	issued := resp.StartedAt
	return tokenPair{access: doc.AccessToken, refresh: doc.RefreshToken, issuedAt: issued,
		expiresAt: issued.Add(time.Duration(doc.ExpiresIn) * time.Second)}, nil
}

// exchange trades an authorisation code for a token pair. The pair must carry a refresh token
// (the offline_access scope).
func (o *oauthClient) exchange(ctx context.Context, code string) (tokenPair, error) {
	p, err := o.post(ctx, url.Values{"grant_type": {"authorization_code"}, "redirect_uri": {o.redirectURI}, "code": {code}})
	if err == nil && p.refresh == "" {
		return tokenPair{}, errTokenResponse
	}
	return p, err
}

// refresh trades a refresh token for a new access token, and a new refresh token if TrueLayer
// gives one.
func (o *oauthClient) refresh(ctx context.Context, refreshToken string) (tokenPair, error) {
	return o.post(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}})
}

// authoriseURL is the TrueLayer auth link for one bank.
func authoriseURL(authURL, clientID, redirectURI, connection, state string) (string, error) {
	u, err := url.Parse(authURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("truelayer: auth URL is not an absolute URL")
	}
	if u.Path == "" {
		u.Path = "/"
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", Scopes)
	q.Set("providers", providersAll)
	q.Set("provider_id", connection)
	q.Set("state", state)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// redirectURI is the callback both connections share: the attempt the state names says which.
func redirectURI(publicURL string) string {
	return strings.TrimRight(publicURL, "/") + "/callback/truelayer"
}

// tokenCallTimeout bounds a token call and the store of its result, detached from the caller's
// cancellation: once the bank has issued a pair it must be stored.
const tokenCallTimeout = 60 * time.Second

func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), tokenCallTimeout)
}

// isRejection reports whether a failed refresh means the bank refused the grant itself (400
// invalid_grant). Anything else (a 429, a 5xx, invalid_client) leaves the consent alone.
func isRejection(err error) bool {
	var be *bankhttp.Error
	return errors.As(err, &be) && be.Status == http.StatusBadRequest && be.BankCode == "invalid_grant"
}

// tokens hands out a usable access token, refreshing it when under RefreshMargin of its life
// remains. One mutex per connection serialises every token call with the store of its result, so
// two refreshes never run at once: TrueLayer can lose the bank's token behind two refreshes in the
// same second. Each refresh stores whatever refresh token comes back.
type tokens struct {
	mu       sync.Mutex
	st       *store.Store
	oauth    *oauthClient
	consents *consent.Manager
	source   string
	now      func() time.Time
	// onLost is called after a rejected refresh has ended the consent.
	onLost func(ctx context.Context, c store.Consent)
}

func (t *tokens) access(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	q := t.st.DB()
	tok, err := store.GetOAuthToken(ctx, q, t.source)
	if errors.Is(err, store.ErrNotFound) {
		return "", ErrNoToken
	}
	if err != nil {
		return "", err
	}
	if tok.ExpiresAt.Sub(t.now()) >= RefreshMargin {
		return tok.AccessToken, nil
	}
	var consentID int64
	if cur, err := t.consents.Current(ctx, q); err == nil {
		consentID = cur.ID
	} else if !errors.Is(err, consent.ErrNoConsent) {
		return "", err
	}
	tctx, cancel := detached(ctx)
	defer cancel()
	pair, err := t.oauth.refresh(tctx, tok.RefreshToken)
	if err != nil {
		if isRejection(err) {
			return "", t.rejected(tctx, consentID, err)
		}
		return "", err
	}
	if pair.refresh == "" {
		pair.refresh = tok.RefreshToken
	}
	again, err := store.GetOAuthToken(tctx, q, t.source)
	if err != nil {
		return "", err
	}
	if again.RefreshToken != tok.RefreshToken {
		return "", errTokenReplaced
	}
	if err := store.PutOAuthToken(tctx, q, store.OAuthToken{
		Source: t.source, AccessToken: pair.access, RefreshToken: pair.refresh,
		IssuedAt: pair.issuedAt, ExpiresAt: pair.expiresAt, UpdatedAt: t.now(),
	}); err != nil {
		return "", err
	}
	return pair.access, nil
}

// rejected ends consent id after a refresh of its pair was refused, when it is still the current
// consent.
func (t *tokens) rejected(ctx context.Context, id int64, cause error) error {
	if id == 0 {
		return errors.Join(ErrConsentLost, cause)
	}
	cur, err := t.consents.Current(ctx, t.st.DB())
	if err != nil && !errors.Is(err, consent.ErrNoConsent) {
		return err
	}
	if err != nil || cur.ID != id {
		return cause
	}
	c, err := t.consents.Lost(ctx, t.st.DB(), id, consent.ReasonRefreshRejected)
	if err != nil && !errors.Is(err, consent.ErrIllegalTransition) {
		return err
	}
	if err == nil && t.onLost != nil {
		t.onLost(ctx, c)
	}
	return errors.Join(ErrConsentLost, cause)
}

// exchange trades code for a pair and, in one transaction, stores the pair and records the new
// consent for attempt attemptID, under the token mutex and detached from ctx's cancellation. A
// failed exchange gives the attempt the outcome refused. It returns the consent and the pair, whose
// access token the connect-time fetch uses as it is.
func (t *tokens) exchange(ctx context.Context, code string, attemptID int64) (store.Consent, tokenPair, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tctx, cancel := detached(ctx)
	defer cancel()
	q := t.st.DB()
	pair, err := t.oauth.exchange(tctx, code)
	if err != nil {
		return store.Consent{}, tokenPair{}, errors.Join(err, t.consents.AttemptRefused(tctx, q, attemptID))
	}
	var cons store.Consent
	err = pgx.BeginFunc(tctx, q, func(tx pgx.Tx) error {
		if err := store.PutOAuthToken(tctx, tx, store.OAuthToken{
			Source: t.source, AccessToken: pair.access, RefreshToken: pair.refresh,
			IssuedAt: pair.issuedAt, ExpiresAt: pair.expiresAt, UpdatedAt: t.now(),
		}); err != nil {
			return err
		}
		var err error
		cons, err = t.consents.Exchanged(tctx, tx, attemptID, pair.issuedAt)
		return err
	})
	return cons, pair, err
}

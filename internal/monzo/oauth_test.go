package monzo

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/consent"
	"github.com/rknightion/finstore/internal/monzotest"
	"github.com/rknightion/finstore/internal/store"
)

// connected is a harness with one open account and an active consent whose full-history fetch
// has finished.
func connected(t *testing.T, mod func(*Options)) *harness {
	t.Helper()
	h := newHarness(t, nil, mod)
	now := h.fake.Now()
	h.fake.AddAccount(monzotest.Account{ID: "acc_test_1", Type: "uk_retail", Created: now.AddDate(0, -3, 0)})
	h.fake.GenerateTransactions("acc_test_1", 40, now.AddDate(0, -3, 0), now.Add(-time.Hour), 1)
	h.connect(nil)
	if c := h.consent(); c.State != store.ConsentActive {
		t.Fatalf("consent %s after connect, want active", c.State)
	}
	return h
}

func (h *harness) token() store.OAuthToken {
	h.t.Helper()
	tok, err := store.GetOAuthToken(context.Background(), h.st.DB(), Source)
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

// nearExpiry moves the clock to 40 minutes before the access token expires.
func (h *harness) nearExpiry() {
	h.fake.Advance(h.token().ExpiresAt.Sub(h.fake.Now()) - 40*time.Minute)
}

func TestRefreshedPairIsCommittedBeforeTheNewAccessTokenIsUsed(t *testing.T) {
	t.Parallel()
	h := connected(t, nil)
	old := h.token()
	h.nearExpiry()

	// At the moment each data request reaches the bank, the token it carries must already be the
	// stored one.
	var mu sync.Mutex
	var checked, mismatched int
	h.mu.Lock()
	h.onRequest = func(r *http.Request) {
		if r.URL.Path == monzotest.TokenPath {
			return
		}
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		tok, err := store.GetOAuthToken(context.Background(), h.st.DB(), Source)
		mu.Lock()
		defer mu.Unlock()
		checked++
		if err != nil || tok.AccessToken != bearer {
			mismatched++
		}
	}
	h.mu.Unlock()
	// The first data request after the refresh fails, so nothing but the store can hold the pair.
	h.fake.FailNonJSON(pathAccounts, 4)
	if err := h.w.RunScheduled(context.Background(), KindHourly); err == nil {
		t.Fatal("RunScheduled succeeded although the account list failed")
	}
	stored := h.token()
	if stored.AccessToken == old.AccessToken || stored.RefreshToken == old.RefreshToken {
		t.Fatal("the refreshed pair was not stored")
	}
	if issued, _ := h.fake.TokenIssuedAt(); !stored.IssuedAt.Equal(issued) {
		t.Fatalf("stored issue time %v, the bank issued at %v", stored.IssuedAt, issued)
	}
	mu.Lock()
	defer mu.Unlock()
	if checked == 0 || mismatched != 0 {
		t.Fatalf("%d of %d data requests carried a token that was not yet stored", mismatched, checked)
	}
	if n := h.fake.Count(monzotest.TokenPath); n != 2 {
		t.Fatalf("%d token requests, want the exchange and one refresh", n)
	}
}

func TestConcurrentCallersNeedingARefreshMakeOneTokenRequest(t *testing.T) {
	t.Parallel()
	h := connected(t, nil)
	h.nearExpiry()
	before := h.fake.Count(monzotest.TokenPath)
	var wg sync.WaitGroup
	got := make([]string, 8)
	errs := make([]error, 8)
	for i := range got {
		wg.Go(func() { got[i], errs[i] = h.w.tokens.access(context.Background()) })
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if got[i] != got[0] {
			t.Fatal("callers received different access tokens")
		}
	}
	if n := h.fake.Count(monzotest.TokenPath) - before; n != 1 {
		t.Fatalf("%d token requests for %d concurrent callers, want 1", n, len(got))
	}
}

func TestTokenWithMoreThanAnHourLeftIsNotRefreshed(t *testing.T) {
	t.Parallel()
	h := connected(t, nil)
	h.fake.Advance(h.token().ExpiresAt.Sub(h.fake.Now()) - RefreshMargin - time.Minute)
	before := h.fake.Count(monzotest.TokenPath)
	if _, err := h.w.tokens.access(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := h.fake.Count(monzotest.TokenPath) - before; n != 0 {
		t.Fatalf("%d refreshes with over an hour left", n)
	}
}

func TestRejectedRefreshMarksTheConsentLost(t *testing.T) {
	t.Parallel()
	h := connected(t, nil)
	h.nearExpiry()
	h.fake.FailNextToken(http.StatusBadRequest)
	err := h.w.RunScheduled(context.Background(), KindHourly)
	if !errors.Is(err, ErrConsentLost) {
		t.Fatalf("RunScheduled = %v, want ErrConsentLost", err)
	}
	c := h.consent()
	if c.State != store.ConsentLost || c.EndReason == nil || *c.EndReason != string(consent.ReasonRefreshRejected) {
		t.Fatalf("consent = %s %v, want lost refresh_rejected", c.State, c.EndReason)
	}
}

func TestLostTokenResponseLeadsToARejectedRefreshAndALostConsent(t *testing.T) {
	t.Parallel()
	h := connected(t, nil)
	old := h.token()
	h.nearExpiry()
	h.fake.DropNextTokenResponse()

	// The bank rotated the pair but the answer never arrived: a network failure, not retried.
	before := h.fake.Count(monzotest.TokenPath)
	if err := h.w.RunScheduled(context.Background(), KindHourly); err == nil || errors.Is(err, ErrConsentLost) {
		t.Fatalf("first run = %v, want a network failure that is not a lost consent", err)
	}
	if n := h.fake.Count(monzotest.TokenPath) - before; n != 1 {
		t.Fatalf("%d token requests, want 1: a token request is never retried", n)
	}
	if c := h.consent(); c.State != store.ConsentActive {
		t.Fatalf("consent %s after a network failure, want still active", c.State)
	}
	if tok := h.token(); tok.RefreshToken != old.RefreshToken {
		t.Fatal("the stored pair changed although no answer arrived")
	}

	// The stored refresh token was spent: the next refresh is rejected and ends the consent.
	if err := h.w.RunScheduled(context.Background(), KindHourly); !errors.Is(err, ErrConsentLost) {
		t.Fatalf("second run = %v, want ErrConsentLost", err)
	}
	c := h.consent()
	if c.State != store.ConsentLost || c.EndReason == nil || *c.EndReason != string(consent.ReasonRefreshRejected) {
		t.Fatalf("consent = %s %v, want lost refresh_rejected", c.State, c.EndReason)
	}
}

func TestTokenResponsesAreNeverWrittenToRaw(t *testing.T) {
	t.Parallel()
	h := connected(t, nil)
	h.nearExpiry()
	if err := h.w.RunScheduled(context.Background(), KindHourly); err != nil {
		t.Fatal(err)
	}
	if n := h.fake.Count(monzotest.TokenPath); n != 2 {
		t.Fatalf("%d token requests, want an exchange and a refresh", n)
	}
	tok := h.token()
	for _, r := range h.rawRows() {
		if r.Kind != store.RawAPIResponse {
			continue
		}
		a := r.APIResponse
		if a.Endpoint == tokenPath {
			t.Fatalf("raw.api_response row %d has the token endpoint", a.Seq)
		}
		for _, secret := range []string{tok.AccessToken, tok.RefreshToken} {
			if bytes.Contains(a.Body, []byte(secret)) || bytes.Contains(a.Params, []byte(secret)) {
				t.Fatalf("raw.api_response row %d holds a token", a.Seq)
			}
		}
	}
}

func TestAuthoriseURLCarriesClientRedirectAndState(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	u, err := h.w.AuthoriseURL(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	q := p.Query()
	if p.Host != "auth.example.test" || q.Get("client_id") != monzotest.DefaultClientID ||
		q.Get("redirect_uri") != "https://finstore.example.test/callback/monzo" ||
		q.Get("response_type") != "code" || len(q.Get("state")) < 43 {
		t.Fatalf("authorise URL query = %v", q)
	}
}

func TestCallbackWithUnusableStateIsRefusedAndExchangesNoCode(t *testing.T) {
	t.Parallel()
	h := connected(t, nil)
	ctx := context.Background()
	active := h.consent()
	tokens := h.fake.Count(monzotest.TokenPath)
	h.fake.RegisterCode("code_test_unused")

	stateOf := func() string {
		u, err := h.w.AuthoriseURL(ctx)
		if err != nil {
			t.Fatal(err)
		}
		p, _ := url.Parse(u)
		return p.Query().Get("state")
	}
	if err := h.w.HandleCallback(ctx, "never-issued", "code_test_unused"); !errors.Is(err, ErrCallbackRefused) {
		t.Fatalf("unknown state: %v", err)
	}
	expired := stateOf()
	h.fake.Advance(time.Hour)
	if err := h.w.HandleCallback(ctx, expired, "code_test_unused"); !errors.Is(err, ErrCallbackRefused) {
		t.Fatalf("expired state: %v", err)
	}
	// A state already used by a callback whose exchange failed.
	used := stateOf()
	if err := h.w.HandleCallback(ctx, used, "code_test_not_registered"); err == nil || errors.Is(err, ErrCallbackRefused) {
		t.Fatalf("rejected code: %v, want an exchange failure", err)
	}
	tokens++
	if err := h.w.HandleCallback(ctx, used, "code_test_unused"); !errors.Is(err, ErrCallbackRefused) {
		t.Fatalf("used state: %v", err)
	}
	if n := h.fake.Count(monzotest.TokenPath); n != tokens {
		t.Fatalf("%d token requests, want %d: a refused state exchanged a code", n, tokens)
	}
	if c := h.consent(); c.ID != active.ID || c.State != store.ConsentActive {
		t.Fatalf("current consent changed to %d %s", c.ID, c.State)
	}
}

func TestAbandonedAttemptLeavesTheCurrentConsentInPlace(t *testing.T) {
	t.Parallel()
	h := connected(t, nil)
	active := h.consent()
	if _, err := h.w.AuthoriseURL(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.fake.Advance(2 * time.Hour)
	if err := h.w.RunScheduled(context.Background(), KindHourly); err != nil {
		t.Fatalf("RunScheduled after an abandoned attempt: %v", err)
	}
	if c := h.consent(); c.ID != active.ID || c.State != store.ConsentActive {
		t.Fatalf("current consent %d %s, want the original active one", c.ID, c.State)
	}
}

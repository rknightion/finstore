// Package truelayertest is an in-process fake of TrueLayer's auth server and Data API v1 for
// tests. Every id, name and value in it is made up. It serves several providers (banks) at once,
// each with its own consent, tokens and data, and reproduces the behaviour the service has to cope
// with: a transactions range refused when it starts too early, a history that can start later than
// asked, four calls a day per endpoint and account before 429, transaction and direct debit ids
// that change on every call and account ids that change on every account listing, and consents
// that expire or are revoked.
package truelayertest

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultClientID and DefaultClientSecret are the client credentials the fake accepts unless
	// WithClientCredentials replaces them.
	DefaultClientID     = "client-test-0001"
	DefaultClientSecret = "secret-test-0001"

	// TokenPath is the only path that accepts a POST.
	TokenPath = "/connect/token" //nolint:gosec // a URL path, not a credential

	// DailyLimit is how many calls a day one endpoint answers for one account before it answers
	// 429.
	DailyLimit = 4

	// ConsentLifetime is how long after a code exchange the consent ends, as /data/v1/me reports.
	ConsentLifetime = 90 * 24 * time.Hour

	// DefaultTokenLifetime is the access token's lifetime.
	DefaultTokenLifetime = time.Hour
)

// Account is an account at one provider. Type is TRANSACTION or SAVINGS. Opening is the balance
// before its first transaction, in minor units.
type Account struct {
	ID          string
	Type        string
	DisplayName string
	Currency    string
	SortCode    string
	Number      string
	Opening     int64
}

// Transaction is one transaction on a day. Amount is in minor units. NormalisedID is the stable
// normalised_provider_transaction_id; ProviderID the provider_transaction_id.
type Transaction struct {
	Date         time.Time
	Amount       int64
	Description  string
	Category     string
	MerchantName string
	NormalisedID string
	ProviderID   string
}

// StandingOrder is a standing order. Amounts are in minor units; a zero date is left out.
type StandingOrder struct {
	Payee       string
	Reference   string
	Frequency   string
	Status      string
	FirstDate   time.Time
	FirstAmount int64
	NextDate    time.Time
	NextAmount  int64
	FinalDate   time.Time
	FinalAmount int64
}

// DirectDebit is a direct debit. MandateID is the bank's mandate id
// (meta.provider_mandate_identification), left out when empty.
type DirectDebit struct {
	Name           string
	Status         string
	MandateID      string
	PreviousDate   time.Time
	PreviousAmount int64
}

// Request is one logged request. Time is the fake clock at arrival; Bearer the access token sent.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Bearer string
	Time   time.Time
}

// Option configures New.
type Option func(*Fake)

// WithClock sets the time source. Advance adds to whatever it returns.
func WithClock(now func() time.Time) Option { return func(f *Fake) { f.base = now } }

// WithClientCredentials sets the client id and secret the token endpoint accepts.
func WithClientCredentials(id, secret string) Option {
	return func(f *Fake) { f.clientID, f.clientSecret = id, secret }
}

// WithEarliestFrom sets the earliest from date a transactions call accepts; an earlier one is
// refused with 400 invalid_date_range. The default is 2021-01-01.
func WithEarliestFrom(d time.Time) Option { return func(f *Fake) { f.earliest = civil(d) } }

// WithConsentLifetime sets how long after a code exchange the consent ends, as /data/v1/me
// reports it. The default is ConsentLifetime.
func WithConsentLifetime(d time.Duration) Option { return func(f *Fake) { f.consentLife = d } }

// WithTokenLifetime sets the access token's lifetime.
func WithTokenLifetime(d time.Duration) Option { return func(f *Fake) { f.tokenLife = d } }

// WithRotatingRefreshTokens makes every refresh return a new refresh token. By default the refresh
// token stays the same, as TrueLayer documents.
func WithRotatingRefreshTokens() Option { return func(f *Fake) { f.rotate = true } }

// WithLocation sets the zone whose calendar day the daily call limit counts in. The default is
// Europe/London.
func WithLocation(loc *time.Location) Option { return func(f *Fake) { f.loc = loc } }

type txn struct {
	Transaction
	order int
}

type account struct {
	Account
	served []string
	txs    []*txn
	sos    []StandingOrder
	dds    []DirectDebit
	// balance overrides the balance computed from the transactions when set.
	balance *int64
}

// provider is one bank's consent, tokens and data.
type provider struct {
	id       string
	accounts []*account

	generation int
	access     string
	accessExp  time.Time
	refresh    string
	consentEnd time.Time
	revoked    bool
	expired    bool
	// historyStart hides transactions before it, as a bank that holds a shorter history.
	historyStart time.Time
}

// Fake is the fake TrueLayer. All methods are safe for concurrent use.
type Fake struct {
	mu sync.Mutex

	base         func() time.Time
	offset       time.Duration
	clientID     string
	clientSecret string
	earliest     time.Time
	tokenLife    time.Duration
	consentLife  time.Duration
	rotate       bool
	loc          *time.Location

	providers map[string]*provider
	codes     map[string]string // code -> provider
	seq       int
	calls     int
	listings  int
	order     int

	failTx  map[string]int
	fail429 map[string]int
	// failStatus is the status the calls fail429 counts down answer.
	failStatus map[string]int
	failMe     map[string]int
	daily      map[string]int

	log        []Request
	violations []Request
}

// New returns a Fake. Unless WithClock is given, its clock is a manual clock that starts at
// 2026-11-11T12:00:00Z and moves only through Advance.
func New(opts ...Option) *Fake {
	start := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		london = time.UTC
	}
	f := &Fake{
		base:         func() time.Time { return start },
		clientID:     DefaultClientID,
		clientSecret: DefaultClientSecret,
		earliest:     time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC),
		tokenLife:    DefaultTokenLifetime,
		consentLife:  ConsentLifetime,
		loc:          london,
		providers:    map[string]*provider{},
		codes:        map[string]string{},
		failTx:       map[string]int{},
		fail429:      map[string]int{},
		failStatus:   map[string]int{},
		failMe:       map[string]int{},
		daily:        map[string]int{},
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

// Now is the fake clock.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now()
}

func (f *Fake) now() time.Time { return f.base().Add(f.offset) }

// Advance moves the fake clock forward.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.offset += d
	f.mu.Unlock()
}

func (f *Fake) prov(id string) *provider {
	p, ok := f.providers[id]
	if !ok {
		p = &provider{id: id}
		f.providers[id] = p
	}
	return p
}

func (p *provider) account(id string) *account {
	for _, a := range p.accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// AddAccount adds an account to provider. An empty Currency becomes GBP.
func (f *Fake) AddAccount(providerID string, a Account) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a.Currency == "" {
		a.Currency = "GBP"
	}
	p := f.prov(providerID)
	p.accounts = append(p.accounts, &account{Account: a})
}

// AddTransaction adds a transaction to an account. Empty ids get synthetic stable ones.
func (f *Fake) AddTransaction(providerID, accountID string, t Transaction) Transaction {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.prov(providerID).account(accountID)
	if a == nil {
		panic("truelayertest: unknown account " + accountID)
	}
	f.order++
	if t.NormalisedID == "" {
		t.NormalisedID = fmt.Sprintf("norm-%06d", f.order)
	}
	if t.ProviderID == "" {
		t.ProviderID = fmt.Sprintf("prov-%06d", f.order)
	}
	t.Date = civil(t.Date)
	a.txs = append(a.txs, &txn{Transaction: t, order: f.order})
	return t
}

// SetBalance overrides an account's current balance, simulating a mismatch with its transactions.
func (f *Fake) SetBalance(providerID, accountID string, minor int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prov(providerID).account(accountID).balance = &minor
}

// AddStandingOrder adds a standing order to an account.
func (f *Fake) AddStandingOrder(providerID, accountID string, s StandingOrder) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.prov(providerID).account(accountID)
	a.sos = append(a.sos, s)
}

// RemoveStandingOrders removes every standing order of an account to payee, as when it is
// cancelled.
func (f *Fake) RemoveStandingOrders(providerID, accountID, payee string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.prov(providerID).account(accountID)
	a.sos = slices.DeleteFunc(a.sos, func(s StandingOrder) bool { return s.Payee == payee })
}

// AddDirectDebit adds a direct debit to an account.
func (f *Fake) AddDirectDebit(providerID, accountID string, d DirectDebit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.prov(providerID).account(accountID)
	a.dds = append(a.dds, d)
}

// SetHistoryStart hides a provider's transactions dated before d, as a bank that returns a shorter
// history on a later consent.
func (f *Fake) SetHistoryStart(providerID string, d time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prov(providerID).historyStart = civil(d)
}

// RegisterCode registers a single-use authorisation code for a provider.
func (f *Fake) RegisterCode(providerID, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prov(providerID)
	f.codes[code] = providerID
}

// Authorise acts as the owner approving at the bank: it reads the provider_id and state from an
// auth link, registers a fresh code for that provider and returns the code and state the bank
// would send back.
func (f *Fake) Authorise(authLink string) (code, state string, err error) {
	u, err := url.Parse(authLink)
	if err != nil {
		return "", "", err
	}
	q := u.Query()
	pid := q.Get("provider_id")
	if pid == "" || q.Get("response_type") != "code" || q.Get("client_id") != f.clientIDLocked() {
		return "", "", fmt.Errorf("truelayertest: auth link lacks provider_id, response_type=code or the client id")
	}
	f.mu.Lock()
	f.seq++
	code = fmt.Sprintf("code-test-%04d", f.seq)
	f.mu.Unlock()
	f.RegisterCode(pid, code)
	return code, q.Get("state"), nil
}

func (f *Fake) clientIDLocked() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clientID
}

// FailTransactions makes the next n transactions calls to a provider answer 500.
func (f *Fake) FailTransactions(providerID string, n int) {
	f.mu.Lock()
	f.failTx[providerID] = n
	f.mu.Unlock()
}

// Fail429 makes the next n data calls to a provider for the endpoint template (for example
// /data/v1/accounts/{id}/transactions) answer 429, whatever the daily count.
func (f *Fake) Fail429(providerID, template string, n int) {
	f.Fail(providerID, template, http.StatusTooManyRequests, n)
}

// Fail makes the next n data calls to a provider for the endpoint template answer status.
func (f *Fake) Fail(providerID, template string, status, n int) {
	f.mu.Lock()
	f.fail429[providerID+" "+template] = n
	f.failStatus[providerID+" "+template] = status
	f.mu.Unlock()
}

// FailMe makes the next n /data/v1/me calls to a provider answer 500.
func (f *Fake) FailMe(providerID string, n int) {
	f.mu.Lock()
	f.failMe[providerID] = n
	f.mu.Unlock()
}

// ExpireConsent ends a provider's consent at the bank: data calls answer 403 access_denied.
func (f *Fake) ExpireConsent(providerID string) {
	f.mu.Lock()
	f.prov(providerID).expired = true
	f.mu.Unlock()
}

// RevokeConsent withdraws a provider's consent: a refresh answers 400 invalid_grant and data calls
// with the current token answer 401.
func (f *Fake) RevokeConsent(providerID string) {
	f.mu.Lock()
	f.prov(providerID).revoked = true
	f.mu.Unlock()
}

// ConsentEnd is when the provider's current consent ends, as /data/v1/me reports it.
func (f *Fake) ConsentEnd(providerID string) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prov(providerID).consentEnd
}

// Requests returns a copy of the request log.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log)
}

// Count is the number of requests whose path, with any account id replaced by {id}, is template.
func (f *Fake) Count(template string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.log {
		if t, _ := templateOf(r.Path); t == template {
			n++
		}
	}
	return n
}

// Violations returns the requests that were neither a GET nor a POST to TokenPath. A read-only
// client leaves it empty.
func (f *Fake) Violations() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.violations)
}

type reply struct {
	status   int
	body     any
	location string
}

func oauthErr(status int, code string) reply {
	return reply{status: status, body: map[string]string{"error": code, "error_description": "synthetic error"}}
}

// ServeHTTP implements http.Handler.
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rep := f.handle(r)
	if rep.location != "" {
		http.Redirect(w, r, rep.location, http.StatusFound)
		return
	}
	b, err := json.Marshal(rep.body)
	if err != nil {
		http.Error(w, "marshal", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(rep.status)
	_, _ = w.Write(b)
}

// templateOf returns the endpoint template of a data path and the account id in it.
func templateOf(path string) (template, id string) {
	rest, ok := strings.CutPrefix(path, "/data/v1/accounts/")
	if !ok {
		return path, ""
	}
	id, kind, ok := strings.Cut(rest, "/")
	if !ok {
		return path, ""
	}
	return "/data/v1/accounts/{id}/" + kind, id
}

func (f *Fake) handle(r *http.Request) reply {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	req := Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Bearer: bearer, Time: now}
	f.log = append(f.log, req)

	isToken := r.Method == http.MethodPost && r.URL.Path == TokenPath
	if r.Method != http.MethodGet && !isToken {
		f.violations = append(f.violations, req)
		return oauthErr(http.StatusMethodNotAllowed, "method_not_allowed")
	}
	switch {
	case isToken:
		return f.token(r, now)
	case r.URL.Path == "/":
		return f.authorise(r.URL.Query())
	}

	p := f.byAccess(bearer)
	if p == nil {
		return oauthErr(http.StatusUnauthorized, "invalid_token")
	}
	if p.revoked || !now.Before(p.accessExp) {
		return oauthErr(http.StatusUnauthorized, "invalid_token")
	}
	if p.expired {
		return oauthErr(http.StatusForbidden, "access_denied")
	}
	template, servedID := templateOf(r.URL.Path)
	var acct *account
	if servedID != "" {
		for _, a := range p.accounts {
			if slices.Contains(a.served, servedID) {
				acct = a
			}
		}
		if acct == nil {
			return oauthErr(http.StatusNotFound, "account_not_found")
		}
	}
	key := p.id + " " + template
	if f.fail429[key] > 0 {
		f.fail429[key]--
		if st := f.failStatus[key]; st != http.StatusTooManyRequests {
			return oauthErr(st, "internal_server_error")
		}
		return oauthErr(http.StatusTooManyRequests, "provider_too_many_requests")
	}
	day := now.In(f.loc).Format(time.DateOnly)
	countKey := key + " " + day
	if acct != nil {
		countKey += " " + acct.ID
	}
	f.daily[countKey]++
	if f.daily[countKey] > DailyLimit {
		return oauthErr(http.StatusTooManyRequests, "provider_too_many_requests")
	}
	f.calls++
	q := r.URL.Query()
	switch template {
	case "/data/v1/me":
		if f.failMe[p.id] > 0 {
			f.failMe[p.id]--
			return oauthErr(http.StatusInternalServerError, "internal_server_error")
		}
		return results(map[string]any{
			"client_id": f.clientID, "credentials_id": fmt.Sprintf("cred-test-%04d", p.generation),
			"consent_status": "Authorised", "consent_created_at": ts(p.consentEnd.Add(-f.consentLife)),
			"consent_expires_at": ts(p.consentEnd),
			"provider":           map[string]any{"provider_id": p.id, "display_name": "Made Up Bank"},
		})
	case "/data/v1/accounts":
		return f.listAccounts(p, now)
	case "/data/v1/accounts/{id}/balance":
		return balance(acct, now)
	case "/data/v1/accounts/{id}/transactions":
		if f.failTx[p.id] > 0 {
			f.failTx[p.id]--
			return oauthErr(http.StatusInternalServerError, "internal_server_error")
		}
		return f.transactions(p, acct, q)
	case "/data/v1/accounts/{id}/standing_orders":
		return standingOrders(acct, servedID, now)
	case "/data/v1/accounts/{id}/direct_debits":
		return f.directDebits(acct, servedID, now)
	}
	return oauthErr(http.StatusNotFound, "not_found")
}

func (f *Fake) byAccess(token string) *provider {
	if token == "" {
		return nil
	}
	for _, p := range f.providers {
		if p.access == token {
			return p
		}
	}
	return nil
}

// authorise is the auth dialog: it approves at once and sends the owner back with a code.
func (f *Fake) authorise(q url.Values) reply {
	pid, redirect := q.Get("provider_id"), q.Get("redirect_uri")
	if pid == "" || redirect == "" || q.Get("client_id") != f.clientID || q.Get("response_type") != "code" {
		return oauthErr(http.StatusBadRequest, "invalid_request")
	}
	f.seq++
	code := fmt.Sprintf("code-test-%04d", f.seq)
	f.prov(pid)
	f.codes[code] = pid
	u, err := url.Parse(redirect)
	if err != nil {
		return oauthErr(http.StatusBadRequest, "invalid_request")
	}
	back := u.Query()
	back.Set("code", code)
	back.Set("state", q.Get("state"))
	u.RawQuery = back.Encode()
	return reply{location: u.String()}
}

func (f *Fake) token(r *http.Request, now time.Time) reply {
	if err := r.ParseForm(); err != nil {
		return oauthErr(http.StatusBadRequest, "invalid_request")
	}
	form := r.PostForm
	if form.Get("client_id") != f.clientID || form.Get("client_secret") != f.clientSecret {
		return oauthErr(http.StatusUnauthorized, "invalid_client")
	}
	var p *provider
	switch form.Get("grant_type") {
	case "authorization_code":
		pid, ok := f.codes[form.Get("code")]
		if !ok || form.Get("redirect_uri") == "" {
			return oauthErr(http.StatusBadRequest, "invalid_grant")
		}
		delete(f.codes, form.Get("code"))
		p = f.prov(pid)
		p.generation++
		p.revoked, p.expired = false, false
		p.consentEnd = now.Add(f.consentLife).Truncate(time.Second)
		f.seq++
		p.refresh = fmt.Sprintf("refresh-test-%04d", f.seq)
	case "refresh_token":
		for _, x := range f.providers {
			if x.refresh != "" && x.refresh == form.Get("refresh_token") {
				p = x
			}
		}
		if p == nil || p.revoked {
			return oauthErr(http.StatusBadRequest, "invalid_grant")
		}
		if f.rotate {
			f.seq++
			p.refresh = fmt.Sprintf("refresh-test-%04d", f.seq)
		}
	default:
		return oauthErr(http.StatusBadRequest, "unsupported_grant_type")
	}
	f.seq++
	p.access = fmt.Sprintf("access-test-%04d", f.seq)
	p.accessExp = now.Add(f.tokenLife)
	return reply{status: http.StatusOK, body: map[string]any{
		"access_token": p.access, "expires_in": int(f.tokenLife / time.Second), "token_type": "Bearer",
		"refresh_token": p.refresh, "scope": "info accounts balance transactions direct_debits standing_orders offline_access",
	}}
}

func results(items ...map[string]any) reply {
	if items == nil {
		items = []map[string]any{}
	}
	return reply{status: http.StatusOK, body: map[string]any{"results": items, "status": "Succeeded"}}
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// dateTS is a day as TrueLayer writes a date-only timestamp.
func dateTS(d time.Time) string { return d.Format("2006-01-02") + "T00:00:00+00:00" }

func civil(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// Money is minor units written as a JSON number with two decimals, exactly.
func Money(minor int64) json.Number {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	return json.Number(fmt.Sprintf("%s%d.%02d", sign, minor/100, minor%100))
}

func (f *Fake) listAccounts(p *provider, now time.Time) reply {
	f.listings++
	var out []map[string]any
	for _, a := range p.accounts {
		served := fmt.Sprintf("%s-%04d", a.ID, f.listings)
		a.served = append(a.served, served)
		out = append(out, map[string]any{
			"account_id": served, "account_type": a.Type, "display_name": a.DisplayName, "currency": a.Currency,
			"update_timestamp": ts(now),
			"account_number": map[string]any{
				"iban": "GB00TEST" + a.SortCode + a.Number, "number": a.Number, "sort_code": a.SortCode,
				"swift_bic": "TESTGB2L",
			},
			"provider": map[string]any{"provider_id": p.id, "display_name": "Made Up Bank"},
		})
	}
	return results(out...)
}

// sorted returns an account's transactions in date order, then the order they were added, with
// the balance after each.
func (a *account) sorted() ([]*txn, []int64) {
	txs := slices.Clone(a.txs)
	slices.SortStableFunc(txs, func(x, y *txn) int { return cmp.Or(x.Date.Compare(y.Date), cmp.Compare(x.order, y.order)) })
	after := make([]int64, len(txs))
	run := a.Opening
	for i, t := range txs {
		run += t.Amount
		after[i] = run
	}
	return txs, after
}

func balance(a *account, now time.Time) reply {
	_, after := a.sorted()
	cur := a.Opening
	if len(after) > 0 {
		cur = after[len(after)-1]
	}
	if a.balance != nil {
		cur = *a.balance
	}
	return results(map[string]any{
		"currency": a.Currency, "available": Money(cur), "current": Money(cur), "overdraft": Money(0),
		"update_timestamp": ts(now),
	})
}

func parseDay(s string) (time.Time, bool) {
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return civil(t), true
	}
	return time.Time{}, false
}

func (f *Fake) transactions(p *provider, a *account, q url.Values) reply {
	from, okFrom := parseDay(q.Get("from"))
	to, okTo := parseDay(q.Get("to"))
	if !okFrom || !okTo || to.Before(from) {
		return oauthErr(http.StatusBadRequest, "invalid_date_range")
	}
	if from.Before(f.earliest) {
		return oauthErr(http.StatusBadRequest, "invalid_date_range")
	}
	txs, after := a.sorted()
	var out []map[string]any
	for i, t := range txs {
		if t.Date.Before(from) || t.Date.After(to) || (!p.historyStart.IsZero() && t.Date.Before(p.historyStart)) {
			continue
		}
		typ := "DEBIT"
		if t.Amount > 0 {
			typ = "CREDIT"
		}
		item := map[string]any{
			// transaction_id changes on every call, as TrueLayer documents it may.
			"transaction_id": fmt.Sprintf("txn-%06d-%04d", f.calls, i), "timestamp": dateTS(t.Date),
			"description": t.Description, "amount": Money(t.Amount), "currency": a.Currency,
			"transaction_type": typ, "transaction_category": cmp.Or(t.Category, "PURCHASE"),
			"transaction_classification":         []string{},
			"running_balance":                    map[string]any{"amount": Money(after[i]), "currency": a.Currency},
			"normalised_provider_transaction_id": t.NormalisedID, "provider_transaction_id": t.ProviderID,
			"meta": map[string]any{"provider_category": "DEB", "transaction_type": typ},
		}
		if t.MerchantName != "" {
			item["merchant_name"] = t.MerchantName
		}
		out = append(out, item)
	}
	return results(out...)
}

func standingOrders(a *account, servedID string, now time.Time) reply {
	var out []map[string]any
	for _, s := range a.sos {
		item := map[string]any{
			"frequency": s.Frequency, "status": s.Status, "currency": a.Currency, "payee": s.Payee,
			"reference": s.Reference, "timestamp": ts(now),
			"meta": map[string]any{"provider_account_id": servedID},
		}
		for _, d := range []struct {
			date   time.Time
			amount int64
			name   string
		}{{s.FirstDate, s.FirstAmount, "first"}, {s.NextDate, s.NextAmount, "next"}, {s.FinalDate, s.FinalAmount, "final"}} {
			if !d.date.IsZero() {
				item[d.name+"_payment_date"] = dateTS(civil(d.date))
				item[d.name+"_payment_amount"] = Money(d.amount)
			}
		}
		out = append(out, item)
	}
	return results(out...)
}

func (f *Fake) directDebits(a *account, servedID string, now time.Time) reply {
	var out []map[string]any
	for i, d := range a.dds {
		meta := map[string]any{"provider_account_id": servedID}
		if d.MandateID != "" {
			meta["provider_mandate_identification"] = d.MandateID
		}
		item := map[string]any{
			// direct_debit_id changes on every call, as TrueLayer documents it may.
			"direct_debit_id": fmt.Sprintf("dd-%06d-%04d", f.calls, i), "name": d.Name, "status": d.Status,
			"currency": a.Currency, "timestamp": ts(now), "meta": meta,
		}
		if !d.PreviousDate.IsZero() {
			item["previous_payment_timestamp"] = dateTS(civil(d.PreviousDate))
			item["previous_payment_amount"] = Money(d.PreviousAmount)
		}
		out = append(out, item)
	}
	return results(out...)
}

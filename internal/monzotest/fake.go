// Package monzotest is an in-process fake of the Monzo developer API for
// tests. Every id and value in it is made up. It reproduces the behaviour
// observed on the real API that the service has to cope with: an id cursor
// that skips rows sharing a timestamp, a 365 day range limit, a full-history
// window that closes shortly after a token is issued, and token rotation.
package monzotest

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultClientID and DefaultClientSecret are the client credentials the
	// fake accepts unless WithClientCredentials replaces them.
	DefaultClientID     = "client_test_0001"
	DefaultClientSecret = "secret_test_0001"

	// TokenPath is the only path that accepts a POST.
	TokenPath = "/oauth2/token" //nolint:gosec // a URL path, not a credential

	// MaxRange is the longest since..before range the transactions endpoint
	// accepts.
	MaxRange = 8760 * time.Hour

	// RecentWindow is how far back a request may reach once the full-history
	// window has closed.
	RecentWindow = 90 * 24 * time.Hour

	userID = "user_test_0001"
)

// Account is a Monzo account. Type is for example uk_retail,
// uk_retail_joint, uk_monzo_flex or uk_loan.
type Account struct {
	ID          string
	Type        string
	Description string
	Created     time.Time
	Closed      bool
	// SortCode and AccountNumber are listed when set, as Monzo lists them for current, joint and
	// business accounts.
	SortCode      string
	AccountNumber string
}

// Merchant is the object embedded in a transaction when expand[]=merchant is
// requested.
type Merchant struct {
	ID       string
	Name     string
	Category string
}

// Transaction is one transaction. Amount is in minor units. Settled is an
// RFC 3339 timestamp, or empty while the transaction is pending. A
// transaction is declined when DeclineReason is not empty.
type Transaction struct {
	ID                         string
	AccountID                  string
	Created                    time.Time
	Amount                     int64
	Currency                   string
	Description                string
	DeclineReason              string
	Settled                    string
	Merchant                   *Merchant
	CanSplitTheBill            bool
	CanBeExcludedFromBreakdown bool
	CanBeMadeSubscription      bool
}

// Pot is a savings pot on an account.
type Pot struct {
	ID      string
	Name    string
	Balance int64
	Deleted bool
}

// Request is one logged request. Time is the fake clock at arrival.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Time   time.Time
}

// Option configures New.
type Option func(*Fake)

// WithClock sets the time source. Advance adds to whatever it returns.
func WithClock(now func() time.Time) Option { return func(f *Fake) { f.base = now } }

// WithClientCredentials sets the client id and secret the token endpoint
// accepts.
func WithClientCredentials(id, secret string) Option {
	return func(f *Fake) { f.clientID, f.clientSecret = id, secret }
}

// WithWindowSeconds sets how long after a token is issued any range is
// served. The default is 300.
func WithWindowSeconds(n int) Option {
	return func(f *Fake) { f.window = time.Duration(n) * time.Second }
}

// Fake is the fake Monzo API. All methods are safe for concurrent use.
type Fake struct {
	mu sync.Mutex

	base         func() time.Time
	offset       time.Duration
	clientID     string
	clientSecret string
	window       time.Duration

	accounts []Account
	txs      []*Transaction
	pots     map[string][]*Pot
	balances map[string]int64
	nextTx   int

	codes    map[string]bool
	seq      int
	access   string
	refresh  string
	issued   time.Time
	approved bool

	tokenFail int
	dropNext  bool
	delay     time.Duration
	limited   map[string]int
	nonJSON   map[string]int

	log        []Request
	counts     map[string]int
	violations []Request

	srv *httptest.Server
}

// New returns a Fake. Unless WithClock is given, its clock is a manual clock
// that starts at 2026-06-01T12:00:00Z and moves only through Advance and
// WithDelay.
func New(opts ...Option) *Fake {
	start := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	f := &Fake{
		base:         func() time.Time { return start },
		clientID:     DefaultClientID,
		clientSecret: DefaultClientSecret,
		window:       300 * time.Second,
		pots:         map[string][]*Pot{},
		balances:     map[string]int64{},
		codes:        map[string]bool{},
		limited:      map[string]int{},
		nonJSON:      map[string]int{},
		counts:       map[string]int{},
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

// Handler returns the fake as an http.Handler.
func (f *Fake) Handler() http.Handler { return f }

// Start starts an httptest server for the fake. The caller closes it.
func (f *Fake) Start() *httptest.Server {
	srv := httptest.NewServer(f)
	f.mu.Lock()
	f.srv = srv
	f.mu.Unlock()
	return srv
}

// URL is the base URL of the server started by Start.
func (f *Fake) URL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.srv == nil {
		return ""
	}
	return f.srv.URL
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

// WithDelay makes every later request advance the fake clock by d once it
// has been logged, simulating a slow response. It never sleeps.
func (f *Fake) WithDelay(d time.Duration) {
	f.mu.Lock()
	f.delay = d
	f.mu.Unlock()
}

// RegisterCode registers a single-use authorization code.
func (f *Fake) RegisterCode(code string) {
	f.mu.Lock()
	f.codes[code] = true
	f.mu.Unlock()
}

// Approve gives the current token permission to read data.
func (f *Fake) Approve() {
	f.mu.Lock()
	f.approved = true
	f.mu.Unlock()
}

// Revoke takes the permission back, as when the 90 day consent ends.
func (f *Fake) Revoke() {
	f.mu.Lock()
	f.approved = false
	f.mu.Unlock()
}

// TokenIssuedAt is when the current token was issued, and false if none was.
func (f *Fake) TokenIssuedAt() (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issued, f.access != ""
}

// FailNextToken makes the next request to the token endpoint return status
// with a JSON error body, without changing any token.
func (f *Fake) FailNextToken(status int) {
	f.mu.Lock()
	f.tokenFail = status
	f.mu.Unlock()
}

// DropNextTokenResponse makes the next successful token request rotate the
// tokens server side and then close the connection without a response.
func (f *Fake) DropNextTokenResponse() {
	f.mu.Lock()
	f.dropNext = true
	f.mu.Unlock()
}

// Fail429 makes the next n requests to path return 429.
func (f *Fake) Fail429(path string, n int) {
	f.mu.Lock()
	f.limited[path] = n
	f.mu.Unlock()
}

// FailNonJSON makes the next n requests to path return an HTML error page
// with status 502.
func (f *Fake) FailNonJSON(path string, n int) {
	f.mu.Lock()
	f.nonJSON[path] = n
	f.mu.Unlock()
}

// Requests returns a copy of the request log.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log)
}

// Count is the number of requests to path.
func (f *Fake) Count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[path]
}

// Violations returns the requests that were neither a GET nor a POST to
// TokenPath. A read-only client leaves it empty.
func (f *Fake) Violations() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.violations)
}

// AddAccount adds an account.
func (f *Fake) AddAccount(a Account) {
	f.mu.Lock()
	f.accounts = append(f.accounts, a)
	f.mu.Unlock()
}

// AddPot adds a pot to an account.
func (f *Fake) AddPot(accountID string, p Pot) {
	f.mu.Lock()
	f.pots[accountID] = append(f.pots[accountID], &p)
	f.mu.Unlock()
}

// SetBalance overrides an account's balance, simulating a mismatch with its
// transactions.
func (f *Fake) SetBalance(accountID string, balance int64) {
	f.mu.Lock()
	f.balances[accountID] = balance
	f.mu.Unlock()
}

// ClearBalance removes an override, so the balance is again the sum of the
// account's non-declined transactions.
func (f *Fake) ClearBalance(accountID string) {
	f.mu.Lock()
	delete(f.balances, accountID)
	f.mu.Unlock()
}

// AddTransaction adds a transaction and returns it as stored. An empty ID
// gets the next synthetic id, an empty Currency becomes GBP and a zero
// Created becomes the fake clock.
func (f *Fake) AddTransaction(tx Transaction) Transaction {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addTx(tx)
}

func (f *Fake) addTx(tx Transaction) Transaction {
	if tx.ID == "" {
		f.nextTx++
		tx.ID = fmt.Sprintf("tx_test_%06d", f.nextTx)
	}
	if tx.Currency == "" {
		tx.Currency = "GBP"
	}
	if tx.Created.IsZero() {
		tx.Created = f.now()
	}
	tx.Created = tx.Created.UTC()
	f.txs = append(f.txs, &tx)
	return tx
}

// UpdateTransaction applies fn to the stored transaction and reports whether
// it exists.
func (f *Fake) UpdateTransaction(id string, fn func(*Transaction)) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tx := range f.txs {
		if tx.ID == id {
			fn(tx)
			tx.Created = tx.Created.UTC()
			return true
		}
	}
	return false
}

// SettleTransaction sets the settled timestamp, as when a pending
// transaction settles.
func (f *Fake) SettleTransaction(id string, at time.Time) bool {
	return f.UpdateTransaction(id, func(tx *Transaction) {
		tx.Settled = at.UTC().Format(time.RFC3339Nano)
	})
}

// SetTransactionAmount changes a transaction's amount.
func (f *Fake) SetTransactionAmount(id string, amount int64) bool {
	return f.UpdateTransaction(id, func(tx *Transaction) { tx.Amount = amount })
}

// RemoveTransaction deletes a transaction, as when a pending row disappears.
func (f *Fake) RemoveTransaction(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.txs)
	f.txs = slices.DeleteFunc(f.txs, func(tx *Transaction) bool { return tx.ID == id })
	return len(f.txs) < n
}

// GenerateTransactions adds n synthetic transactions to an account, spread
// evenly over [from, to) in whole seconds. With groupSize k > 1, each run of
// k transactions shares one identical created timestamp. Amounts and ids are
// deterministic. Each has the merchant "Example Merchant".
func (f *Fake) GenerateTransactions(accountID string, n int, from, to time.Time, groupSize int) []Transaction {
	k := max(groupSize, 1)
	groups := (n + k - 1) / k
	span := to.Sub(from)
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Transaction, 0, n)
	for i := range n {
		g := i / k
		created := from
		if groups > 1 {
			// Divide before multiplying: span times the group index overflows for long histories.
			created = from.Add(time.Duration(float64(span) * float64(g) / float64(groups)))
		}
		out = append(out, f.addTx(Transaction{
			AccountID:   accountID,
			Created:     created.Truncate(time.Second),
			Amount:      -int64(i%97+1) * 10,
			Description: "Example Purchase " + strconv.Itoa(i+1),
			Settled:     created.Truncate(time.Second).Format(time.RFC3339Nano),
			Merchant:    &Merchant{ID: "merch_test_0001", Name: "Example Merchant", Category: "general"},
		}))
	}
	return out
}

type reply struct {
	status int
	body   any
	raw    string
	drop   bool
}

func jsonErr(status int, code string) reply {
	return reply{status: status, body: map[string]string{"code": code, "message": "synthetic error"}}
}

func tokenErr(status int, code string) reply {
	return reply{status: status, body: map[string]string{"error": code, "error_description": "synthetic error"}}
}

// ServeHTTP implements http.Handler.
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rep := f.handle(r)
	if rep.drop {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
		return
	}
	if rep.raw != "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(rep.status)
		_, _ = w.Write([]byte(rep.raw))
		return
	}
	b, err := json.Marshal(rep.body)
	if err != nil {
		http.Error(w, "marshal", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rep.status)
	_, _ = w.Write(b)
}

func (f *Fake) handle(r *http.Request) reply {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	now := f.now()
	req := Request{Method: r.Method, Path: path, Query: r.URL.Query(), Time: now}
	f.log = append(f.log, req)
	f.counts[path]++
	f.offset += f.delay

	isToken := r.Method == http.MethodPost && path == TokenPath
	if r.Method != http.MethodGet && !isToken {
		f.violations = append(f.violations, req)
		return jsonErr(http.StatusMethodNotAllowed, "method_not_allowed")
	}
	if f.limited[path] > 0 {
		f.limited[path]--
		return jsonErr(http.StatusTooManyRequests, "too_many_requests")
	}
	if f.nonJSON[path] > 0 {
		f.nonJSON[path]--
		return reply{status: http.StatusBadGateway, raw: "<html><body><h1>502 Bad Gateway</h1></body></html>"}
	}
	if path == TokenPath {
		if !isToken {
			return jsonErr(http.StatusMethodNotAllowed, "method_not_allowed")
		}
		return f.token(r, now)
	}

	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" || tok != f.access {
		return jsonErr(http.StatusUnauthorized, "unauthorized.bad_access_token")
	}
	if path == "/ping/whoami" {
		return reply{status: http.StatusOK, body: map[string]any{
			"authenticated": true, "client_id": f.clientID, "user_id": userID,
		}}
	}
	if !f.approved {
		return jsonErr(http.StatusForbidden, "forbidden.insufficient_permissions")
	}
	q := r.URL.Query()
	switch path {
	case "/accounts":
		return f.listAccounts(q.Get("account_type"))
	case "/balance":
		return f.balance(q.Get("account_id"))
	case "/pots":
		return f.listPots(q.Get("current_account_id"))
	case "/webhooks":
		return reply{status: http.StatusOK, body: map[string]any{"webhooks": []any{}}}
	case "/transactions":
		return f.transactions(q, now)
	}
	return jsonErr(http.StatusNotFound, "not_found")
}

func (f *Fake) token(r *http.Request, now time.Time) reply {
	if s := f.tokenFail; s != 0 {
		f.tokenFail = 0
		return tokenErr(s, "server_error")
	}
	if err := r.ParseForm(); err != nil {
		return tokenErr(http.StatusBadRequest, "invalid_request")
	}
	form := r.PostForm
	if form.Get("client_id") != f.clientID || form.Get("client_secret") != f.clientSecret {
		return tokenErr(http.StatusUnauthorized, "invalid_client")
	}
	keepApproval := false
	switch form.Get("grant_type") {
	case "authorization_code":
		code := form.Get("code")
		if !f.codes[code] {
			return tokenErr(http.StatusBadRequest, "invalid_grant")
		}
		delete(f.codes, code)
	case "refresh_token":
		if f.refresh == "" || form.Get("refresh_token") != f.refresh {
			return tokenErr(http.StatusBadRequest, "invalid_grant")
		}
		keepApproval = true
	default:
		return tokenErr(http.StatusBadRequest, "unsupported_grant_type")
	}
	f.seq++
	f.access = fmt.Sprintf("access_test_%04d", f.seq)
	f.refresh = fmt.Sprintf("refresh_test_%04d", f.seq)
	f.issued = now
	f.approved = f.approved && keepApproval
	if f.dropNext {
		f.dropNext = false
		return reply{drop: true}
	}
	return reply{status: http.StatusOK, body: map[string]any{
		"access_token":  f.access,
		"client_id":     f.clientID,
		"expires_in":    21600,
		"refresh_token": f.refresh,
		"token_type":    "Bearer",
		"user_id":       userID,
	}}
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func (f *Fake) listAccounts(typ string) reply {
	out := []map[string]any{}
	for _, a := range f.accounts {
		if typ != "" && a.Type != typ {
			continue
		}
		obj := map[string]any{
			"id": a.ID, "type": a.Type, "description": a.Description,
			"created": ts(a.Created), "closed": a.Closed,
		}
		if a.SortCode != "" || a.AccountNumber != "" {
			obj["sort_code"], obj["account_number"] = a.SortCode, a.AccountNumber
		}
		out = append(out, obj)
	}
	return reply{status: http.StatusOK, body: map[string]any{"accounts": out}}
}

func (f *Fake) hasAccount(id string) bool {
	return slices.ContainsFunc(f.accounts, func(a Account) bool { return a.ID == id })
}

func (f *Fake) balance(id string) reply {
	if !f.hasAccount(id) {
		return jsonErr(http.StatusNotFound, "not_found")
	}
	bal, ok := f.balances[id]
	if !ok {
		for _, tx := range f.txs {
			if tx.AccountID == id && tx.DeclineReason == "" {
				bal += tx.Amount
			}
		}
	}
	total := bal
	for _, p := range f.pots[id] {
		if !p.Deleted {
			total += p.Balance
		}
	}
	return reply{status: http.StatusOK, body: map[string]any{
		"balance": bal, "total_balance": total, "currency": "GBP", "spend_today": 0,
	}}
}

func (f *Fake) listPots(id string) reply {
	if !f.hasAccount(id) {
		return jsonErr(http.StatusNotFound, "not_found")
	}
	out := []map[string]any{}
	for _, p := range f.pots[id] {
		out = append(out, map[string]any{
			"id": p.ID, "name": p.Name, "balance": p.Balance, "currency": "GBP", "deleted": p.Deleted,
		})
	}
	return reply{status: http.StatusOK, body: map[string]any{"pots": out}}
}

func (f *Fake) transactions(q url.Values, now time.Time) reply {
	acct := q.Get("account_id")
	if !f.hasAccount(acct) {
		return jsonErr(http.StatusNotFound, "not_found")
	}
	limit := 30
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return jsonErr(http.StatusBadRequest, "bad_request.invalid_limit")
		}
		limit = min(n, 100)
	}
	var before time.Time
	if s := q.Get("before"); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return jsonErr(http.StatusBadRequest, "bad_request.invalid_before")
		}
		before = t
	}
	var start time.Time // the zero time means unbounded
	exclusive, sinceTS := false, false
	if s := q.Get("since"); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			start, sinceTS = t, true
		} else {
			i := slices.IndexFunc(f.txs, func(tx *Transaction) bool { return tx.ID == s && tx.AccountID == acct })
			if i < 0 {
				return jsonErr(http.StatusNotFound, "not_found")
			}
			// The real API's flaw: rows sharing the cursor row's timestamp are skipped.
			start, exclusive = f.txs[i].Created, true
		}
	}
	if sinceTS && !before.IsZero() && before.Sub(start) > MaxRange {
		return jsonErr(http.StatusBadRequest, "bad_request.invalid_time_range")
	}
	if now.Sub(f.issued) >= f.window && start.Before(now.Add(-RecentWindow)) {
		return jsonErr(http.StatusForbidden, "forbidden.verification_required")
	}

	var rows []*Transaction
	for _, tx := range f.txs {
		switch {
		case tx.AccountID != acct:
		case exclusive && !tx.Created.After(start):
		case !exclusive && tx.Created.Before(start):
		case !before.IsZero() && !tx.Created.Before(before):
		default:
			rows = append(rows, tx)
		}
	}
	slices.SortFunc(rows, func(a, b *Transaction) int {
		return cmp.Or(a.Created.Compare(b.Created), strings.Compare(a.ID, b.ID))
	})
	rows = rows[:min(len(rows), limit)]
	expand := slices.Contains(q["expand[]"], "merchant") || slices.Contains(q["expand"], "merchant")
	out := make([]map[string]any, 0, len(rows))
	for _, tx := range rows {
		out = append(out, txJSON(tx, expand))
	}
	return reply{status: http.StatusOK, body: map[string]any{"transactions": out}}
}

func txJSON(tx *Transaction, expand bool) map[string]any {
	var merchant any
	if m := tx.Merchant; m != nil {
		merchant = m.ID
		if expand {
			merchant = map[string]any{"id": m.ID, "name": m.Name, "category": m.Category}
		}
	}
	m := map[string]any{
		"id": tx.ID, "account_id": tx.AccountID, "created": ts(tx.Created),
		"amount": tx.Amount, "currency": tx.Currency, "description": tx.Description,
		"settled": tx.Settled, "merchant": merchant, "metadata": map[string]string{},
		"can_split_the_bill":             tx.CanSplitTheBill,
		"can_be_excluded_from_breakdown": tx.CanBeExcludedFromBreakdown,
		"can_be_made_subscription":       tx.CanBeMadeSubscription,
	}
	if tx.DeclineReason != "" {
		m["decline_reason"] = tx.DeclineReason
	}
	return m
}

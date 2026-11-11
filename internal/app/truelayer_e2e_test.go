package app_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/app"
	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/consent"
	"github.com/rknightion/finstore/internal/importer/importertest"
	"github.com/rknightion/finstore/internal/monzo"
	"github.com/rknightion/finstore/internal/monzotest"
	"github.com/rknightion/finstore/internal/scheduler"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
	"github.com/rknightion/finstore/internal/truelayer"
	"github.com/rknightion/finstore/internal/truelayertest"
)

// Every value in this file is made up: sort codes, account numbers, names and amounts.
const (
	tlPublicURL     = "https://finstore.example.test"
	lloydsJointID   = "tl-acc-joint"
	monzoPersonalID = "acc_e2e_personal"
	monzoJointID    = "acc_e2e_joint"
	srcLloyds       = "truelayer:ob-lloyds"
	srcTLMonzo      = "truelayer:ob-monzo"
	cancelledPayee  = "MADE UP GYM"
	tlDescMarker    = "TL-DESCRIPTION-MARKER-4410"
)

func civilDate(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// tlWorld is the world of TestTrueLayerEndToEnd: the Monzo world plus the fake TrueLayer and its
// two workers.
type tlWorld struct {
	*world
	tl      *truelayertest.Fake
	mu      sync.Mutex
	workers map[string]*truelayer.Worker
}

func (w *tlWorld) worker(source string) *truelayer.Worker {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.workers[source]
}

// send makes a request to the service; email signs it in when set, and site is its
// Sec-Fetch-Site.
func (w *tlWorld) send(method, path, email, site string) *http.Response {
	w.t.Helper()
	req, err := http.NewRequest(method, "http://"+w.addr+path, nil)
	if err != nil {
		w.t.Fatal(err)
	}
	if email != "" {
		req.Header.Set("X-Auth-Request-Email", email)
	}
	if site != "" {
		req.Header.Set("Sec-Fetch-Site", site)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		w.t.Fatal(err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp
}

// connectLink posts the status page's connect form for path and returns the bank link it
// redirects to.
func (w *tlWorld) connectLink(path string) *url.URL {
	w.t.Helper()
	resp := w.send(http.MethodPost, path, ownerEmail, "same-origin")
	u, err := url.Parse(continueLink(w.t, resp))
	if err != nil {
		w.t.Fatal(err)
	}
	return u
}

func (w *tlWorld) queryText(sql string, args ...any) string {
	w.t.Helper()
	conn, err := pgx.Connect(context.Background(), w.super)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var s string
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		w.t.Fatalf("%s: %v", sql, err)
	}
	return s
}

// consentAt returns the current consent of source as state/end_reason, or the error's text. It is
// safe to call off the test goroutine.
func consentAt(super, source string) string {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		return "error: " + err.Error()
	}
	defer func() { _ = conn.Close(ctx) }()
	var s string
	if err := conn.QueryRow(ctx, `SELECT state || '/' || coalesce(end_reason, '') FROM sync.consent
		WHERE source = $1 AND is_current`, source).Scan(&s); err != nil {
		return "error: " + err.Error()
	}
	return s
}

func (w *tlWorld) tokenPosts() int {
	n := 0
	for _, r := range w.tl.Requests() {
		if r.Method == http.MethodPost && r.Path == truelayertest.TokenPath {
			n++
		}
	}
	return n
}

// tlEnv is the configuration of a service with Monzo and TrueLayer, both pointing at fakes.
func tlEnv(t *testing.T, super, monzoURL, tlURL string) config.Config {
	t.Helper()
	env := map[string]string{
		config.EnvSuperuserDatabaseURL:  super,
		config.EnvOwnerDatabaseURL:      testpg.UserURL(t, super, store.RoleOwner),
		config.EnvDatabaseURL:           testpg.UserURL(t, super, store.RoleApp),
		config.EnvOwnerPassword:         "owner-password",
		config.EnvAppPassword:           "app-password",
		config.EnvListenAddr:            "127.0.0.1:0",
		config.EnvPublicURL:             tlPublicURL,
		config.EnvAllowedEmails:         ownerEmail,
		config.EnvMonzoClientID:         monzotest.DefaultClientID,
		config.EnvMonzoClientSecret:     monzotest.DefaultClientSecret,
		config.EnvMonzoAPIURL:           monzoURL,
		config.EnvMonzoAuthURL:          "https://auth.example.test/",
		config.EnvTrueLayerClientID:     truelayertest.DefaultClientID,
		config.EnvTrueLayerClientSecret: truelayertest.DefaultClientSecret,
		config.EnvTrueLayerAuthURL:      tlURL,
		config.EnvTrueLayerAPIURL:       tlURL,
		config.EnvLogLevel:              "debug",
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestTrueLayerEndToEnd drives the service with both TrueLayer connections configured, through
// the real ledger and store, the HTTP routes and both fakes: a consent left with no token at
// start, the connect routes' refusals, Monzo's own connect, Lloyds' connect with its full history
// reconciled by running balance, the callback's refusals, Monzo's mandates through TrueLayer
// attached to the accounts Monzo's own API created, a cancelled standing order disappearing, and a
// statement account ended where Lloyds takes over, whose overlapping rows show once.
func TestTrueLayerEndToEnd(t *testing.T) {
	super := testpg.Start(t)
	now := time.Now()

	mfake := monzotest.New(monzotest.WithClock(time.Now))
	msrv := mfake.Start()
	t.Cleanup(msrv.Close)
	mfake.AddAccount(monzotest.Account{ID: monzoPersonalID, Type: "uk_retail", Description: "made-up",
		Created: now.AddDate(0, 0, -60), SortCode: "223344", AccountNumber: "11111111"})
	mfake.AddAccount(monzotest.Account{ID: monzoJointID, Type: "uk_retail_joint", Description: "made-up",
		Created: now.AddDate(0, 0, -60), SortCode: "223344", AccountNumber: "22222222"})
	mfake.GenerateTransactions(monzoPersonalID, 6, now.AddDate(0, 0, -50), now.Add(-time.Hour), 1)
	mfake.GenerateTransactions(monzoJointID, 4, now.AddDate(0, 0, -50), now.Add(-time.Hour), 1)

	tfake := truelayertest.New(truelayertest.WithClock(time.Now))
	tsrv := httptest.NewServer(tfake)
	t.Cleanup(tsrv.Close)
	// Lloyds: a joint account whose history starts on 2024-08-22, where a statement-fed account
	// for the same bank account ends.
	tfake.AddAccount(truelayer.ConnLloyds, truelayertest.Account{ID: lloydsJointID, Type: "TRANSACTION",
		DisplayName: "Joint", SortCode: "11-22-33", Number: "12345678", Opening: 50000})
	lloydsRows := []truelayertest.Transaction{
		{Date: civilDate(2024, 8, 22), Amount: -1230, Description: "MADE UP SHOP"},
		{Date: civilDate(2024, 8, 27), Amount: 5000, Description: "MADE UP TRANSFER IN"},
		{Date: civilDate(2025, 3, 10), Amount: 3000, Description: "MADE UP REFUND"},
		{Date: civilDate(2025, 3, 10), Amount: -3000, Description: "MADE UP REFUND REVERSED"},
		{Date: now.AddDate(0, 0, -3), Amount: -999, Description: tlDescMarker},
	}
	for _, tx := range lloydsRows {
		tfake.AddTransaction(truelayer.ConnLloyds, lloydsJointID, tx)
	}
	tfake.AddStandingOrder(truelayer.ConnLloyds, lloydsJointID, truelayertest.StandingOrder{Payee: "MADE UP SAVER",
		Reference: "MADE UP REF", Frequency: "MONTHLY", Status: "Active", NextDate: now.AddDate(0, 1, 0), NextAmount: 10000})
	tfake.AddStandingOrder(truelayer.ConnLloyds, lloydsJointID, truelayertest.StandingOrder{Payee: cancelledPayee,
		Reference: "MADE UP MEMBER", Frequency: "MONTHLY", Status: "Active", NextDate: now.AddDate(0, 1, 0), NextAmount: 2500})
	tfake.AddDirectDebit(truelayer.ConnLloyds, lloydsJointID, truelayertest.DirectDebit{Name: "MADE UP ENERGY",
		Status: "Active", MandateID: "MANDATE-E2E-1"})
	// Monzo through TrueLayer: the two accounts Monzo's own API lists, and a pot.
	tfake.AddAccount(truelayer.ConnMonzo, truelayertest.Account{ID: "tl-mz-personal", Type: "TRANSACTION",
		DisplayName: "Personal", SortCode: "223344", Number: "11111111"})
	tfake.AddAccount(truelayer.ConnMonzo, truelayertest.Account{ID: "tl-mz-joint", Type: "TRANSACTION",
		DisplayName: "Joint", SortCode: "223344", Number: "22222222"})
	tfake.AddAccount(truelayer.ConnMonzo, truelayertest.Account{ID: "tl-mz-pot", Type: "SAVINGS",
		DisplayName: "Pot", SortCode: "223344", Number: "33333333"})
	tfake.AddStandingOrder(truelayer.ConnMonzo, "tl-mz-personal", truelayertest.StandingOrder{Payee: "MADE UP LANDLORD",
		Reference: "RENT", Frequency: "MONTHLY", Status: "Active", NextDate: now.AddDate(0, 1, 0), NextAmount: 90000})
	tfake.AddDirectDebit(truelayer.ConnMonzo, "tl-mz-joint", truelayertest.DirectDebit{Name: "MADE UP COUNCIL",
		Status: "Active", MandateID: "MANDATE-E2E-2"})

	cfg := tlEnv(t, super, msrv.URL, tsrv.URL)
	w := &tlWorld{
		world: &world{
			t: t, super: super, cfg: cfg, fake: mfake, stderr: &syncBuffer{},
			client: &http.Client{
				Timeout:       30 * time.Second,
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			},
		},
		tl: tfake, workers: map[string]*truelayer.Worker{},
	}
	if code, _ := w.command(nil, app.Bootstrap); code != app.ExitOK {
		t.Fatalf("bootstrap exit %d: %s", code, w.stderr.String())
	}
	if code, _ := w.command(nil, app.Migrate); code != app.ExitOK {
		t.Fatalf("migrate exit %d: %s", code, w.stderr.String())
	}

	// The statement-fed account Lloyds replaces: a statement running past the day Lloyds' history
	// starts, then the account ended on the day before, as the cut-over is done.
	if code, _ := w.command(nil, func(ctx context.Context, e app.Env) int {
		return app.AccountAdd(ctx, e, app.AccountSpec{Slug: "halifax-joint", Type: "joint", Currency: "GBP", Name: "made-up joint", ExpectsImports: true})
	}); code != app.ExitOK {
		t.Fatalf("account add exit %d: %s", code, w.stderr.String())
	}
	stmt := importertest.New(100000, []importertest.Row{
		{Date: "19 Aug 24", Desc: "MADE-UP BAKERY", Type: "DEB", Out: 1000},
		{Date: "22 Aug 24", Desc: "MADE UP SHOP", Type: "DEB", Out: 1230},
		{Date: "27 Aug 24", Desc: "MADE UP TRANSFER IN", Type: "FPI", In: 5000},
	})
	stmt.SortCode, stmt.Account = "11-22-33", "12345678"
	stmt.Start, stmt.End = civilDate(2024, 8, 1), civilDate(2024, 8, 31)
	if code, out := w.command(bytes.NewReader(stmt.PDF()), func(ctx context.Context, e app.Env) int {
		return app.Import(ctx, e, "halifax-joint", app.FormatHalifaxPDF, "statement.pdf")
	}); code != app.ExitOK || !strings.Contains(out, "3 new") {
		t.Fatalf("import exit %d, output %q: %s", code, out, w.stderr.String())
	}
	lastDay := civilDate(2024, 8, 21)
	if code, _ := w.command(nil, func(ctx context.Context, e app.Env) int { return app.AccountEnd(ctx, e, "halifax-joint", &lastDay) }); code != app.ExitOK {
		t.Fatalf("account end exit %d: %s", code, w.stderr.String())
	}

	// A Monzo-through-TrueLayer consent left active with no stored token, as a database restore
	// leaves it: Resume ends it at start and the service still starts.
	w.queryInt(`INSERT INTO sync.consent (source, state, token_issued_at, approved_at, expected_expiry_at, is_current)
		VALUES ($1, 'active', now() - interval '1 day', now() - interval '1 day', now() + interval '89 days', true)
		RETURNING id`, srcTLMonzo)

	var approve sync.Once
	listening := make(chan net.Addr, 1)
	// atListen is the token-less consent's state when the listener opens: Resume must have run by
	// then, or a callback arriving first could race it.
	var atListen string
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- app.Serve(ctx, app.Env{Cfg: cfg, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: w.stderr, Now: time.Now},
			&app.Hooks{
				NoSchedule: true,
				Listening: func(a net.Addr) {
					atListen = consentAt(super, srcTLMonzo)
					listening <- a
				},
				Worker: func(mw *monzo.Worker) { w.world.worker = mw },
				MonzoOptions: func(o *monzo.Options) {
					o.Sleep = func(ctx context.Context, _ time.Duration) error {
						approve.Do(mfake.Approve)
						return ctx.Err()
					}
				},
				TrueLayerWorker: func(tw *truelayer.Worker) {
					w.mu.Lock()
					w.workers[tw.Source()] = tw
					w.mu.Unlock()
				},
			})
	}()
	stopped := false
	stopServer := func() int {
		if stopped {
			return -1
		}
		stopped = true
		stop()
		select {
		case code := <-done:
			return code
		case <-time.After(60 * time.Second):
			t.Fatal("serve did not stop")
			return -1
		}
	}
	t.Cleanup(func() { stopServer() })
	select {
	case a := <-listening:
		w.addr = a.String()
	case code := <-done:
		t.Fatalf("serve exited %d before listening: %s", code, w.stderr.String())
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not start listening")
	}
	if w.worker(srcLloyds) == nil || w.worker(srcTLMonzo) == nil {
		t.Fatalf("serve did not start a worker for each TrueLayer connection: %v", w.workers)
	}
	if atListen != "lost/token_missing" {
		t.Fatalf("the token-less consent is %q when the listener opens, want lost/token_missing (Resume must run first): %s",
			atListen, w.stderr.String())
	}

	// The status page has a section per configured source.
	page := body(t, w.get("/", true))
	for _, want := range []string{
		"Connect Monzo<", `action="/connect/monzo"`,
		"Lloyds (TrueLayer) connection", "Connect Lloyds (TrueLayer)", `action="/connect/truelayer/ob-lloyds"`,
		"Monzo (TrueLayer) connection", "Reconnect Monzo (TrueLayer)", `action="/connect/truelayer/ob-monzo"`, ">lost<",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("first status page lacks %q", want)
		}
	}

	// The connect routes refuse a cross-origin post and an address not on the allow-list, and
	// start nothing.
	for _, path := range []string{"/connect/truelayer/ob-lloyds", "/connect/truelayer/ob-monzo"} {
		if resp := w.send(http.MethodPost, path, ownerEmail, "cross-site"); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("cross-origin POST %s: %d, want 400", path, resp.StatusCode)
		}
		if resp := w.send(http.MethodPost, path, "someone@example.com", "same-origin"); resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s signed in as an address not on the list: %d, want 404", path, resp.StatusCode)
		}
	}
	if n := w.queryInt(`SELECT count(*) FROM sync.connect_attempt`); n != 0 {
		t.Fatalf("refused connect posts started %d attempts", n)
	}

	// Monzo's own connect, unchanged: its accounts arrive with their bank references.
	monzoLink := w.connectLink("/connect/monzo")
	mfake.RegisterCode("code_e2e_monzo")
	if resp := w.get("/callback/monzo?code=code_e2e_monzo&state="+url.QueryEscape(monzoLink.Query().Get("state")), false); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("Monzo callback: %d: %s", resp.StatusCode, w.stderr.String())
	}
	w.world.worker.Wait()
	if n := w.queryInt(`SELECT count(*) FROM core.account WHERE source = 'monzo' AND bank_ref IN ('223344 11111111', '223344 22222222')`); n != 2 {
		t.Fatalf("%d Monzo accounts carry their bank reference, want 2", n)
	}

	// Lloyds: connect redirects to TrueLayer's auth link for the bank.
	link := w.connectLink("/connect/truelayer/ob-lloyds")
	if link.Host != strings.TrimPrefix(tsrv.URL, "http://") || link.Query().Get("provider_id") != truelayer.ConnLloyds ||
		link.Query().Get("redirect_uri") != tlPublicURL+"/callback/truelayer" {
		t.Fatalf("Lloyds connect redirected to %s", link.Redacted())
	}
	code, state, err := tfake.Authorise(link.String())
	if err != nil {
		t.Fatal(err)
	}

	// The callback refuses, with no effect, a state nobody was given, one issued for Monzo, one
	// that has expired, and TrueLayer's error redirect.
	otherState := w.connectLink("/connect/monzo").Query().Get("state")
	const expiredState = "expired-state-e2e"
	w.queryInt(`INSERT INTO sync.connect_attempt (source, state_sha256, started_at, expires_at)
		VALUES ($1, $2, now() - interval '2 hours', now() - interval '1 hour') RETURNING id`, srcLloyds, consent.HashState(expiredState))
	usedBefore := w.queryInt(`SELECT count(*) FROM sync.connect_attempt WHERE used_at IS NOT NULL OR outcome IS NOT NULL`)
	for name, target := range map[string]string{
		"unknown state":  "/callback/truelayer?code=" + code + "&state=never-issued",
		"Monzo's state":  "/callback/truelayer?code=" + code + "&state=" + url.QueryEscape(otherState),
		"expired state":  "/callback/truelayer?code=" + code + "&state=" + expiredState,
		"error redirect": "/callback/truelayer?error=access_denied&state=" + url.QueryEscape(state),
	} {
		if resp := w.get(target, false); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("callback with %s: %d, want 400", name, resp.StatusCode)
		}
	}
	// A TrueLayer state sent to Monzo's callback is refused there and not spent.
	if resp := w.get("/callback/monzo?code=code_e2e_wrong&state="+url.QueryEscape(state), false); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("TrueLayer state at the Monzo callback: %d, want 400", resp.StatusCode)
	}
	if n := w.tokenPosts(); n != 0 {
		t.Fatalf("refused callbacks made %d token calls", n)
	}
	if n := w.queryInt(`SELECT count(*) FROM sync.connect_attempt WHERE used_at IS NOT NULL OR outcome IS NOT NULL`); n != usedBefore {
		t.Fatalf("refused callbacks changed attempts: %d used or given an outcome, want %d", n, usedBefore)
	}

	// The real callback needs no sign-in, works once, and is refused when replayed.
	callback := "/callback/truelayer?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state)
	if resp := w.get(callback, false); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("Lloyds callback: %d to %q: %s", resp.StatusCode, resp.Header.Get("Location"), w.stderr.String())
	}
	if resp := w.get(callback, false); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed Lloyds callback: %d, want 400", resp.StatusCode)
	}
	if n := w.tokenPosts(); n != 1 {
		t.Fatalf("%d token calls after one good callback and a replay, want 1", n)
	}
	w.worker(srcLloyds).Wait()

	// Lloyds' full history is stored and reconciled by running balance.
	cov := w.coverage()
	joint, ok := cov["lloyds-joint"]
	if !ok {
		t.Fatalf("no lloyds-joint after the connect: %+v\n%s", cov, w.stderr.String())
	}
	if joint.TransactionCount != int64(len(lloydsRows)) || joint.ReconciliationState != store.StateReconciled || len(joint.Gaps) != 0 {
		t.Fatalf("lloyds-joint after the connect: %d transactions, state %q, gaps %+v (failed on %v)",
			joint.TransactionCount, joint.ReconciliationState, joint.Gaps, joint.FailedOn)
	}
	if m := w.queryText(`SELECT r.method FROM core.reconciliation r JOIN core.account a ON a.id = r.account_id WHERE a.slug = 'lloyds-joint'`); m != "running_balance" {
		t.Fatalf("lloyds-joint is checked by %q, want running_balance", m)
	}
	if outcome := w.queryText(`SELECT outcome FROM sync.run WHERE source = $1 AND kind = 'connect' ORDER BY id DESC LIMIT 1`, srcLloyds); outcome != "ok" {
		t.Fatalf("Lloyds connect run ended %q", outcome)
	}

	// The ended statement account and Lloyds overlap on 2024-08-22 and 2024-08-27: the agent views
	// show each of those rows once, under lloyds-joint, and halifax-joint only up to its end.
	if n := w.queryInt(`SELECT count(*) FROM agent.transactions WHERE txn_date IN ('2024-08-22', '2024-08-27')`); n != 2 {
		t.Fatalf("%d agent rows on the overlapping days, want 2", n)
	}
	if n := w.queryInt(`SELECT count(*) FROM agent.transactions WHERE txn_date IN ('2024-08-22', '2024-08-27') AND account_slug = 'lloyds-joint'`); n != 2 {
		t.Fatalf("%d of the overlapping rows are under lloyds-joint, want 2", n)
	}
	if n := w.queryInt(`SELECT count(*) FROM agent.transactions WHERE account_slug = 'halifax-joint'`); n != 1 {
		t.Fatalf("halifax-joint shows %d rows after its end, want 1", n)
	}
	if n := w.queryInt(`SELECT count(*) FROM core.transaction t JOIN core.account a ON a.id = t.account_id WHERE a.slug = 'halifax-joint'`); n != 3 {
		t.Fatalf("core holds %d halifax-joint rows, want all 3 kept", n)
	}

	// Monzo through TrueLayer: reconnecting the lost consent attaches its mandates to the accounts
	// Monzo's own API created, and creates no account. The pot's mandates are never asked for.
	accountsBefore := w.queryInt(`SELECT count(*) FROM core.account`)
	link = w.connectLink("/connect/truelayer/ob-monzo")
	if link.Query().Get("provider_id") != truelayer.ConnMonzo {
		t.Fatalf("Monzo (TrueLayer) connect redirected to %s", link.Redacted())
	}
	code, state, err = tfake.Authorise(link.String())
	if err != nil {
		t.Fatal(err)
	}
	if resp := w.get("/callback/truelayer?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), false); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("Monzo (TrueLayer) callback: %d: %s", resp.StatusCode, w.stderr.String())
	}
	w.worker(srcTLMonzo).Wait()
	if n := w.queryInt(`SELECT count(*) FROM core.account`); n != accountsBefore {
		t.Fatalf("Monzo through TrueLayer created %d accounts", n-accountsBefore)
	}
	if got := w.queryText(`SELECT account_slug FROM agent.standing_orders WHERE source = $1 AND payee = 'MADE UP LANDLORD'`, srcTLMonzo); got != "monzo-current" {
		t.Fatalf("the Monzo standing order is on %q, want monzo-current", got)
	}
	if got := w.queryText(`SELECT account_slug FROM agent.direct_debits WHERE source = $1 AND name = 'MADE UP COUNCIL'`, srcTLMonzo); got != "monzo-joint" {
		t.Fatalf("the Monzo direct debit is on %q, want monzo-joint", got)
	}
	if n := w.queryInt(`SELECT count(*) FROM agent.standing_orders WHERE account_slug IS NULL`) +
		w.queryInt(`SELECT count(*) FROM agent.direct_debits WHERE account_slug IS NULL`); n != 0 {
		t.Fatalf("%d mandates are not attached to an account", n)
	}
	for _, r := range tfake.Requests() {
		if strings.Contains(r.Path, "tl-mz-pot") && (strings.HasSuffix(r.Path, "/standing_orders") || strings.HasSuffix(r.Path, "/direct_debits")) {
			t.Fatalf("the pot's mandates were asked for: %s", r.Path)
		}
	}
	page = body(t, w.get("/", true))
	if !strings.Contains(page, "Reconnect Lloyds (TrueLayer)") || !strings.Contains(page, "Reconnect Monzo (TrueLayer)") || strings.Contains(page, ">lost<") {
		t.Fatalf("status page after both TrueLayer connects:\n%s", page)
	}

	// A standing order cancelled at the bank disappears on the next sync; the other stays.
	tfake.RemoveStandingOrders(truelayer.ConnLloyds, lloydsJointID, cancelledPayee)
	if err := w.worker(srcLloyds).RunScheduled(context.Background()); err != nil {
		t.Fatalf("Lloyds sync: %v", err)
	}
	if n := w.queryInt(`SELECT count(*) FROM agent.standing_orders WHERE payee = $1 AND disappeared_at IS NOT NULL`, cancelledPayee); n != 1 {
		t.Fatalf("the cancelled standing order has not disappeared: %d", n)
	}
	if n := w.queryInt(`SELECT count(*) FROM agent.standing_orders WHERE account_slug = 'lloyds-joint' AND disappeared_at IS NULL`); n != 1 {
		t.Fatalf("lloyds-joint has %d current standing orders, want 1", n)
	}
	if got := w.coverage()["lloyds-joint"]; got.ReconciliationState != store.StateReconciled || got.TransactionCount != int64(len(lloydsRows)) {
		t.Fatalf("lloyds-joint after the sync: %+v", got)
	}

	// Rebuilding from the stored data gives exactly the live record: both TrueLayer normalisers
	// are registered.
	if code, out := w.command(nil, func(ctx context.Context, e app.Env) int { return app.Renormalise(ctx, e, true) }); code != app.ExitOK {
		t.Fatalf("renormalise --verify exit %d:\n%s\n%s", code, out, w.stderr.String())
	}

	if code := stopServer(); code != app.ExitOK {
		t.Fatalf("serve exit %d: %s", code, w.stderr.String())
	}

	// The service only read from either bank, and nothing a bank returned or the callback carried
	// reached a log.
	if v := tfake.Violations(); len(v) != 0 {
		t.Fatalf("the service made requests to TrueLayer a read-only client never makes: %+v", v)
	}
	if v := mfake.Violations(); len(v) != 0 {
		t.Fatalf("the service made requests to Monzo a read-only client never makes: %+v", v)
	}
	logs := w.stderr.String()
	for _, marker := range []string{tlDescMarker, state, code, lloydsJointID, "12345678", "MADE UP LANDLORD"} {
		if strings.Contains(logs, marker) {
			t.Errorf("the log contains %q", marker)
		}
	}
	if !strings.Contains(logs, `"endpoint":"/callback/truelayer"`) {
		t.Errorf("no request line for the TrueLayer callback; the log check above proves less than it seems")
	}
}

// TestServeWiresTrueLayerOnlyWhenConfigured starts the service with its scheduler on: with
// TrueLayer configured it schedules each connection's syncs at 06:20 and 18:20 and offers its
// routes; with neither TrueLayer setting it runs as before, Monzo only.
func TestServeWiresTrueLayerOnlyWhenConfigured(t *testing.T) {
	super := testpg.Start(t)
	full := tlEnv(t, super, "https://api.example.test", "https://auth.truelayer.example.test")
	monzoOnly := full
	monzoOnly.TrueLayerClientID, monzoOnly.TrueLayerClientSecret = "", ""
	w := &world{t: t, super: super, cfg: full, fake: monzotest.New(monzotest.WithClock(time.Now)), stderr: &syncBuffer{},
		client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if code, _ := w.command(nil, app.Bootstrap); code != app.ExitOK {
		t.Fatalf("bootstrap: %s", w.stderr.String())
	}
	if code, _ := w.command(nil, app.Migrate); code != app.ExitOK {
		t.Fatalf("migrate: %s", w.stderr.String())
	}

	run := func(cfg config.Config) (jobs []string, page string, connect, callback int) {
		t.Helper()
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		listening := make(chan net.Addr, 1)
		done := make(chan int, 1)
		var workers int
		go func() {
			done <- app.Serve(ctx, app.Env{Cfg: cfg, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: w.stderr},
				&app.Hooks{
					Listening:       func(a net.Addr) { listening <- a },
					TrueLayerWorker: func(*truelayer.Worker) { workers++ },
					Scheduler:       func(s *scheduler.Scheduler) { jobs = s.Names() },
				})
		}()
		select {
		case a := <-listening:
			w.addr = a.String()
		case code := <-done:
			t.Fatalf("serve exited %d before listening: %s", code, w.stderr.String())
		case <-time.After(30 * time.Second):
			t.Fatal("serve did not start listening")
		}
		page = body(t, w.get("/", true))
		connect = (&tlWorld{world: w}).send(http.MethodPost, "/connect/truelayer/ob-lloyds", ownerEmail, "cross-site").StatusCode
		callback = w.get("/callback/truelayer?code=c&state=s", false).StatusCode
		stop()
		select {
		case code := <-done:
			if code != app.ExitOK {
				t.Fatalf("serve exit %d: %s", code, w.stderr.String())
			}
		case <-time.After(60 * time.Second):
			t.Fatal("serve did not stop")
		}
		if want := map[bool]int{true: 2, false: 0}[cfg.TrueLayerEnabled()]; workers != want {
			t.Fatalf("%d TrueLayer workers, want %d", workers, want)
		}
		return jobs, page, connect, callback
	}

	jobs, page, connect, callback := run(full)
	for _, want := range []string{
		srcLloyds + " 06:20", srcLloyds + " 18:20", srcTLMonzo + " 06:20", srcTLMonzo + " 18:20",
		monzo.KindHourly, monzo.KindNightly,
	} {
		if !slices.Contains(jobs, want) {
			t.Errorf("jobs %v lack %q", jobs, want)
		}
	}
	if !strings.Contains(page, "Connect Lloyds (TrueLayer)") || !strings.Contains(page, "Connect Monzo (TrueLayer)") {
		t.Errorf("configured page lacks the TrueLayer sections")
	}
	if connect != http.StatusBadRequest || callback != http.StatusBadRequest {
		t.Errorf("configured routes: connect %d, callback %d; want 400 and 400", connect, callback)
	}

	jobs, page, connect, callback = run(monzoOnly)
	if slices.ContainsFunc(jobs, func(j string) bool { return strings.HasPrefix(j, truelayer.SourcePrefix) }) {
		t.Errorf("Monzo-only service scheduled TrueLayer jobs: %v", jobs)
	}
	if !slices.Contains(jobs, monzo.KindHourly) || !slices.Contains(jobs, monzo.KindNightly) {
		t.Errorf("Monzo-only service lacks the Monzo jobs: %v", jobs)
	}
	if strings.Contains(page, "TrueLayer") || !strings.Contains(page, "Connect Monzo") {
		t.Errorf("Monzo-only page:\n%s", page)
	}
	if connect != http.StatusNotFound || callback != http.StatusNotFound {
		t.Errorf("Monzo-only routes: connect %d, callback %d; want 404 and 404", connect, callback)
	}
}

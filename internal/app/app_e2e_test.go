package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/app"
	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/importer/importertest"
	"github.com/rknightion/finstore/internal/monzo"
	"github.com/rknightion/finstore/internal/monzotest"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// Every value in this file is made up.
const (
	ownerEmail  = "owner@example.com"
	descMarker  = "DESCRIPTION-MARKER-7731"
	merchMarker = "MERCHANT-MARKER-5512"
	accountID   = "acc_e2e_current"
)

// syncBuffer is a bytes.Buffer safe to write from the server's goroutines and read from the test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type world struct {
	t      *testing.T
	super  string
	cfg    config.Config
	fake   *monzotest.Fake
	stderr *syncBuffer
	addr   string
	worker *monzo.Worker
	client *http.Client
}

// command runs one command function the way the binary would, and returns its exit code and
// standard output.
func (w *world) command(stdin io.Reader, fn func(ctx context.Context, e app.Env) int) (int, string) {
	w.t.Helper()
	var out bytes.Buffer
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	code := fn(context.Background(), app.Env{Cfg: w.cfg, Stdin: stdin, Stdout: &out, Stderr: w.stderr, Now: w.fake.Now})
	return code, out.String()
}

func (w *world) get(path string, signedIn bool) *http.Response {
	w.t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+w.addr+path, nil)
	if err != nil {
		w.t.Fatal(err)
	}
	if signedIn {
		req.Header.Set("X-Auth-Request-Email", ownerEmail)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	return resp
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (w *world) coverage() map[string]store.CoverageRow {
	w.t.Helper()
	code, out := w.command(nil, func(ctx context.Context, e app.Env) int { return app.Coverage(ctx, e, true) })
	if code != app.ExitOK {
		w.t.Fatalf("coverage exit %d: %s", code, w.stderr.String())
	}
	var rows []store.CoverageRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		w.t.Fatalf("coverage output is not JSON: %v\n%s", err, out)
	}
	by := map[string]store.CoverageRow{}
	for _, r := range rows {
		by[r.AccountSlug] = r
	}
	return by
}

func (w *world) queryInt(sql string, args ...any) int64 {
	w.t.Helper()
	conn, err := pgx.Connect(context.Background(), w.super)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var n int64
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		w.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// TestServiceEndToEnd drives the real binary's code paths against a real PostgreSQL and the fake
// Monzo: set up, connect, full-history fetch, scheduled syncs, a changed transaction, statement
// imports, a rebuild check and the health check. It is the test that catches wiring mistakes
// between packages, which each package's own tests cannot see.
func TestServiceEndToEnd(t *testing.T) {
	super := testpg.Start(t)
	fake := monzotest.New(monzotest.WithClock(time.Now))
	srv := fake.Start()
	t.Cleanup(srv.Close)

	now := fake.Now()
	fake.AddAccount(monzotest.Account{ID: accountID, Type: "uk_retail", Description: "made-up", Created: now.AddDate(0, 0, -400)})
	fake.AddPot(accountID, monzotest.Pot{ID: "pot_e2e_1", Name: "made-up pot", Balance: 12345})
	generated := fake.GenerateTransactions(accountID, 250, now.AddDate(0, 0, -399), now.Add(-time.Hour), 3)
	fake.UpdateTransaction(generated[10].ID, func(tx *monzotest.Transaction) {
		tx.Description = descMarker
		tx.Merchant = &monzotest.Merchant{ID: "merch_e2e", Name: merchMarker, Category: "general"}
	})

	env := map[string]string{
		config.EnvSuperuserDatabaseURL: super,
		config.EnvOwnerDatabaseURL:     testpg.UserURL(t, super, store.RoleOwner),
		config.EnvDatabaseURL:          testpg.UserURL(t, super, store.RoleApp),
		config.EnvOwnerPassword:        "owner-password",
		config.EnvAppPassword:          "app-password",
		config.EnvListenAddr:           "127.0.0.1:0",
		config.EnvPublicURL:            "https://finstore.example.test",
		config.EnvAllowedEmails:        ownerEmail,
		config.EnvMonzoClientID:        monzotest.DefaultClientID,
		config.EnvMonzoClientSecret:    monzotest.DefaultClientSecret,
		config.EnvMonzoAPIURL:          srv.URL,
		config.EnvMonzoAuthURL:         "https://auth.example.test/",
		config.EnvLogLevel:             "debug",
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	w := &world{
		t: t, super: super, cfg: cfg, fake: fake, stderr: &syncBuffer{},
		client: &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}

	if code, _ := w.command(nil, app.Bootstrap); code != app.ExitOK {
		t.Fatalf("bootstrap exit %d: %s", code, w.stderr.String())
	}
	if code, _ := w.command(nil, app.Bootstrap); code != app.ExitOK {
		t.Fatalf("bootstrap is not safe to run again: exit %d: %s", code, w.stderr.String())
	}

	// serve refuses a database whose tables have not been created, and says what to run.
	if code, _ := w.command(nil, func(ctx context.Context, e app.Env) int { return app.Serve(ctx, e, &app.Hooks{NoSchedule: true}) }); code != app.ExitError {
		t.Fatalf("serve before migrate: exit %d, want %d", code, app.ExitError)
	}
	if advice := w.stderr.String(); !strings.Contains(advice, "finstore migrate") {
		t.Fatalf("serve before migrate does not say to run migrate: %q", advice)
	}
	if code, _ := w.command(nil, app.Migrate); code != app.ExitOK {
		t.Fatalf("migrate exit %d: %s", code, w.stderr.String())
	}

	// Start the server. The approval is given at the worker's first pause, so its first poll sees
	// the 403 a real connect sees before the owner approves in the bank's app.
	var approve sync.Once
	listening := make(chan net.Addr, 1)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- app.Serve(ctx, app.Env{Cfg: cfg, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: w.stderr, Now: fake.Now},
			&app.Hooks{
				NoSchedule: true,
				Listening:  func(a net.Addr) { listening <- a },
				Worker:     func(mw *monzo.Worker) { w.worker = mw },
				MonzoOptions: func(o *monzo.Options) {
					o.Sleep = func(ctx context.Context, _ time.Duration) error {
						approve.Do(fake.Approve)
						return ctx.Err()
					}
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

	// The pages: closed to anyone not signed in as the owner, open for health.
	if resp := w.get("/", false); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status page without sign-in: %d, want 404", resp.StatusCode)
	}
	if resp := w.get("/healthz", false); resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
	if page := body(t, w.get("/", true)); !strings.Contains(page, "not connected") || !strings.Contains(page, "Connect Monzo") {
		t.Fatalf("first status page does not offer a connection:\n%s", page)
	}

	// Connect: the form post redirects to the bank with a state value.
	req, err := http.NewRequest(http.MethodPost, "http://"+w.addr+"/connect/monzo", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Auth-Request-Email", ownerEmail)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := w.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("connect: %d, want 303", resp.StatusCode)
	}
	bank, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || bank.Host != "auth.example.test" {
		t.Fatalf("connect redirected to %q", resp.Header.Get("Location"))
	}
	state := bank.Query().Get("state")
	if state == "" || bank.Query().Get("redirect_uri") != "https://finstore.example.test/callback/monzo" {
		t.Fatalf("authorise URL lacks state or the redirect: %s", bank.Query().Encode())
	}

	// A callback with a state nobody was given is refused and exchanges nothing.
	if resp := w.get("/callback/monzo?code=code_e2e_1&state=not-the-state", false); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback with a forged state: %d, want 400", resp.StatusCode)
	}
	if n := fake.Count("/oauth2/token"); n != 0 {
		t.Fatalf("a refused callback made %d token requests", n)
	}

	// The real callback needs no sign-in, works once, and is refused when replayed.
	fake.RegisterCode("code_e2e_1")
	callback := "/callback/monzo?code=code_e2e_1&state=" + url.QueryEscape(state)
	if resp := w.get(callback, false); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("callback: %d to %q, want 303 to /: %s", resp.StatusCode, resp.Header.Get("Location"), w.stderr.String())
	}
	if resp := w.get(callback, false); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed callback: %d, want 400", resp.StatusCode)
	}

	// The approval wait and the full-history fetch run in the background.
	w.worker.Wait()
	cov := w.coverage()
	current, ok := cov["monzo-current"]
	if !ok {
		t.Fatalf("no monzo-current account after the full-history fetch: %+v\n%s", cov, w.stderr.String())
	}
	if current.TransactionCount != 250 {
		t.Fatalf("full-history fetch stored %d transactions, want 250 (rows sharing a timestamp must all arrive)", current.TransactionCount)
	}
	if current.ReconciliationState != store.StateReconciled {
		t.Fatalf("monzo-current is %q after the fetch, want reconciled (difference %v)", current.ReconciliationState, current.LastDiffMinor)
	}
	if len(current.Gaps) != 0 {
		t.Fatalf("monzo-current has gaps after a complete fetch: %+v", current.Gaps)
	}
	if pot, ok := cov["monzo-pot"]; !ok || pot.ReconciliationState != store.StateBalancesOnly {
		t.Fatalf("pot row: %+v", cov["monzo-pot"])
	}
	if page := body(t, w.get("/", true)); !strings.Contains(page, ">active<") || !strings.Contains(page, "monzo-current") || !strings.Contains(page, "Reconnect Monzo") {
		t.Fatalf("status page after connecting:\n%s", page)
	}

	// A scheduled sync picks up a new transaction and still reconciles.
	added := fake.AddTransaction(monzotest.Transaction{AccountID: accountID, Amount: -777, Description: "made-up purchase"})
	if err := w.worker.RunScheduled(context.Background(), monzo.KindHourly); err != nil {
		t.Fatalf("hourly sync: %v", err)
	}
	cov = w.coverage()
	if got := cov["monzo-current"]; got.TransactionCount != 251 || got.ReconciliationState != store.StateReconciled {
		t.Fatalf("after the hourly sync: %d transactions, state %q", got.TransactionCount, got.ReconciliationState)
	}

	// The bank changes a transaction: the stored one gains a version, and nothing is lost.
	fake.SetTransactionAmount(added.ID, -888)
	if err := w.worker.RunScheduled(context.Background(), monzo.KindHourly); err != nil {
		t.Fatalf("second hourly sync: %v", err)
	}
	if n := w.queryInt(`SELECT version_count FROM agent.transactions WHERE amount_minor = -888`); n != 2 {
		t.Fatalf("the changed transaction has %d versions, want 2", n)
	}
	if got := w.coverage()["monzo-current"]; got.TransactionCount != 251 || got.ReconciliationState != store.StateReconciled {
		t.Fatalf("after the change: %d transactions, state %q", got.TransactionCount, got.ReconciliationState)
	}

	// The bank stops returning a pending transaction: it leaves the views and the sum.
	fake.RemoveTransaction(added.ID)
	if err := w.worker.RunScheduled(context.Background(), monzo.KindHourly); err != nil {
		t.Fatalf("third hourly sync: %v", err)
	}
	if got := w.coverage()["monzo-current"]; got.TransactionCount != 250 || got.ReconciliationState != store.StateReconciled {
		t.Fatalf("after the removal: %d transactions, state %q", got.TransactionCount, got.ReconciliationState)
	}

	// A statement-fed account: create it, import a statement, and see the refusals.
	if code, _ := w.command(nil, func(ctx context.Context, e app.Env) int {
		return app.AccountAdd(ctx, e, app.AccountSpec{Slug: "bank-joint", Type: "joint", Currency: "GBP", Name: "made-up joint", ExpectsImports: true})
	}); code != app.ExitOK {
		t.Fatalf("account add exit %d: %s", code, w.stderr.String())
	}
	if code, _ := w.command(nil, func(ctx context.Context, e app.Env) int {
		return app.AccountAdd(ctx, e, app.AccountSpec{Slug: "bank-joint", Type: "joint", Currency: "GBP", Name: "again"})
	}); code != app.ExitError {
		t.Fatalf("a second account with the same slug: exit %d, want %d", code, app.ExitError)
	}
	if code, out := w.command(nil, app.AccountList); code != app.ExitOK || !strings.Contains(out, "bank-joint") || !strings.Contains(out, "monzo-current") {
		t.Fatalf("account list exit %d:\n%s", code, out)
	}

	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
	day := func(d int) string { return month.AddDate(0, 0, d-1).Format("02 Jan 06") }
	st := importertest.New(100000, []importertest.Row{
		{Date: day(2), Desc: "MADE-UP SHOP", Type: "DEB", Out: 1250},
		{Date: day(9), Desc: "MADE-UP EMPLOYER", Type: "FPI", In: 250000},
		{Date: day(20), Desc: "MADE-UP UTILITY", Type: "DD", Out: 4599},
	})
	st.Start, st.End = month, month.AddDate(0, 1, -1)
	pdf := st.PDF()
	importPDF := func(slug string, file []byte) (int, string) {
		return w.command(bytes.NewReader(file), func(ctx context.Context, e app.Env) int {
			return app.Import(ctx, e, slug, app.FormatHalifaxPDF, "statement.pdf")
		})
	}
	if code, out := importPDF("bank-joint", pdf); code != app.ExitOK || !strings.Contains(out, "3 new") {
		t.Fatalf("import exit %d, output %q: %s", code, out, w.stderr.String())
	}
	if code, out := importPDF("bank-joint", pdf); code != app.ExitOK || !strings.Contains(out, "already imported") {
		t.Fatalf("second import of the same file: exit %d, output %q", code, out)
	}
	if code, _ := importPDF("monzo-current", pdf); code != app.ExitNoAccount {
		t.Fatalf("import into an API-fed account: exit %d, want %d", code, app.ExitNoAccount)
	}
	if code, _ := importPDF("no-such-account", pdf); code != app.ExitNoAccount {
		t.Fatalf("import into an unknown account: exit %d, want %d", code, app.ExitNoAccount)
	}
	broken := "date,type,amount,balance_after,description\n" +
		month.Format(time.DateOnly) + ",PAY,10.00,10.00,made-up\n" +
		month.AddDate(0, 0, 1).Format(time.DateOnly) + ",PAY,10.00,25.00,made-up\n"
	rawBefore := w.queryInt(`SELECT count(*) FROM raw.import_file`)
	if code, _ := w.command(strings.NewReader(broken), func(ctx context.Context, e app.Env) int {
		return app.Import(ctx, e, "bank-joint", app.FormatManualCSV, "broken.csv")
	}); code != app.ExitRejected {
		t.Fatalf("import of a statement whose balances do not follow: exit %d, want %d", code, app.ExitRejected)
	}
	if n := w.queryInt(`SELECT count(*) FROM raw.import_file`); n != rawBefore+1 {
		t.Fatalf("a rejected file must still be kept: %d files, want %d", n, rawBefore+1)
	}
	// The rejected file given again is still rejected: it was stored, never imported.
	if code, _ := w.command(strings.NewReader(broken), func(ctx context.Context, e app.Env) int {
		return app.Import(ctx, e, "bank-joint", app.FormatManualCSV, "broken.csv")
	}); code != app.ExitRejected {
		t.Fatalf("a rejected file given again: exit %d, want %d (it must not read as already imported)", code, app.ExitRejected)
	}
	if n := w.queryInt(`SELECT count(*) FROM raw.import_file`); n != rawBefore+1 {
		t.Fatalf("the repeated file was stored again: %d files, want %d", n, rawBefore+1)
	}

	// A file given with the wrong format is rejected, and can then be imported with the right one.
	if code, _ := w.command(nil, func(ctx context.Context, e app.Env) int {
		return app.AccountAdd(ctx, e, app.AccountSpec{Slug: "bank-savings", Type: "savings", Currency: "GBP", Name: "made-up savings"})
	}); code != app.ExitOK {
		t.Fatalf("account add exit %d: %s", code, w.stderr.String())
	}
	sound := "date,type,amount,balance_after,description\n" +
		month.Format(time.DateOnly) + ",PAY,10.00,10.00,made-up\n" +
		month.AddDate(0, 0, 1).Format(time.DateOnly) + ",PAY,15.00,25.00,made-up\n"
	importCSV := func(format string) (int, string) {
		return w.command(strings.NewReader(sound), func(ctx context.Context, e app.Env) int {
			return app.Import(ctx, e, "bank-savings", format, "savings.csv")
		})
	}
	if code, _ := importCSV(app.FormatHalifaxPDF); code != app.ExitRejected {
		t.Fatalf("a CSV given as a PDF: exit %d, want %d", code, app.ExitRejected)
	}
	if code, out := importCSV(app.FormatManualCSV); code != app.ExitOK || !strings.Contains(out, "2 new") {
		t.Fatalf("the same file with the right format: exit %d, output %q: %s", code, out, w.stderr.String())
	}
	if got := w.coverage()["bank-savings"]; got.TransactionCount != 2 || got.ReconciliationState != store.StateReconciled {
		t.Fatalf("bank-savings after its import: %+v", got)
	}

	// A difference can be accepted only on an account an API feeds.
	if code, _ := w.command(nil, func(ctx context.Context, e app.Env) int {
		return app.AcceptDifference(ctx, e, "bank-joint", "made-up reason")
	}); code != app.ExitError {
		t.Fatalf("accept-difference on a statement-fed account: exit %d, want %d", code, app.ExitError)
	}

	joint := w.coverage()["bank-joint"]
	if joint.TransactionCount != 3 || joint.ReconciliationState != store.StateReconciled || joint.Feed != "file" {
		t.Fatalf("bank-joint after the imports: %+v", joint)
	}
	if n := w.queryInt(`SELECT balance_minor FROM agent.balances WHERE account_slug = 'bank-joint' ORDER BY on_date DESC, at DESC LIMIT 1`); n != st.Closing {
		t.Fatalf("latest stored balance %d, want the statement's closing balance %d", n, st.Closing)
	}

	// account end records an operator entry that hides the account's rows after the date; a
	// second entry clears it. An unknown account is refused and records nothing.
	endAt := func(slug string, last *time.Time) int {
		code, _ := w.command(nil, func(ctx context.Context, e app.Env) int { return app.AccountEnd(ctx, e, slug, last) })
		return code
	}
	lastDay := month.AddDate(0, 0, 7) // after day(2), before day(9) and day(20)
	if code := endAt("bank-joint", &lastDay); code != app.ExitOK {
		t.Fatalf("account end exit %d: %s", code, w.stderr.String())
	}
	if n := w.queryInt(`SELECT count(*) FROM raw.operator_entry WHERE kind = 'account_end'`); n != 1 {
		t.Fatalf("%d account_end entries, want 1", n)
	}
	if got := w.coverage()["bank-joint"]; got.TransactionCount != 1 {
		t.Fatalf("bank-joint after its end: %+v", got)
	}
	if n := w.queryInt(`SELECT count(*) FROM agent.transactions WHERE account_slug = 'bank-joint'`); n != 1 {
		t.Fatalf("agent.transactions shows %d bank-joint rows after its end, want 1", n)
	}
	if code := endAt("no-such-account", &lastDay); code != app.ExitError {
		t.Fatalf("account end of an unknown account: exit %d, want %d", code, app.ExitError)
	}
	if code := endAt("bank-joint", nil); code != app.ExitOK {
		t.Fatalf("account end --clear exit %d: %s", code, w.stderr.String())
	}
	if n := w.queryInt(`SELECT count(*) FROM raw.operator_entry WHERE kind = 'account_end'`); n != 2 {
		t.Fatalf("%d account_end entries, want 2", n)
	}
	if got := w.coverage()["bank-joint"]; got.TransactionCount != 3 {
		t.Fatalf("bank-joint after its end was cleared: %+v", got)
	}

	// Rebuilding from the stored source data gives exactly the record the live path built.
	if code, out := w.command(nil, func(ctx context.Context, e app.Env) int { return app.Renormalise(ctx, e, true) }); code != app.ExitOK {
		t.Fatalf("renormalise --verify exit %d:\n%s\n%s", code, out, w.stderr.String())
	}
	if code, _ := w.command(nil, func(ctx context.Context, e app.Env) int { return app.Renormalise(ctx, e, false) }); code != app.ExitOK {
		t.Fatalf("renormalise exit %d: %s", code, w.stderr.String())
	}
	if got := w.coverage(); got["monzo-current"].TransactionCount != 250 || got["bank-joint"].TransactionCount != 3 || got["bank-savings"].TransactionCount != 2 {
		t.Fatalf("after the rebuild: %+v", got)
	}
	if err := w.worker.RunScheduled(context.Background(), monzo.KindNightly); err != nil {
		t.Fatalf("nightly sync after the rebuild: %v", err)
	}
	if got := w.coverage()["monzo-current"]; got.TransactionCount != 250 || got.ReconciliationState != store.StateReconciled {
		t.Fatalf("after the nightly sync: %d transactions, state %q", got.TransactionCount, got.ReconciliationState)
	}

	// The reader role sees the views and nothing underneath them.
	conn, err := pgx.Connect(context.Background(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(context.Background(), "SET ROLE "+store.RoleAgent); err != nil {
		t.Fatal(err)
	}
	var seen int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM agent.transactions WHERE description = $1`, descMarker).Scan(&seen); err != nil || seen != 1 {
		t.Fatalf("the reader role cannot read agent.transactions: %d rows, %v", seen, err)
	}
	for _, table := range []string{"raw.api_response", "secret.oauth_token", "core.transaction", "sync.consent"} {
		if _, err := conn.Exec(context.Background(), "SELECT 1 FROM "+table+" LIMIT 1"); err == nil {
			t.Errorf("the reader role can read %s", table)
		}
	}

	// The health check command follows the server.
	_, port, err := net.SplitHostPort(w.addr)
	if err != nil {
		t.Fatal(err)
	}
	health := w.cfg
	health.ListenAddr = ":" + port
	check := func() int {
		return app.Healthcheck(context.Background(), app.Env{Cfg: health, Stdout: io.Discard, Stderr: io.Discard})
	}
	if code := check(); code != app.ExitOK {
		t.Fatalf("healthcheck against the running server: exit %d", code)
	}
	// A server bound to one address is checked at that address, not at an assumed loopback.
	bound := w.cfg
	bound.ListenAddr = w.addr
	if code := app.Healthcheck(context.Background(), app.Env{Cfg: bound, Stdout: io.Discard, Stderr: io.Discard}); code != app.ExitOK {
		t.Fatalf("healthcheck with the server's own address: exit %d", code)
	}
	elsewhere := w.cfg
	elsewhere.ListenAddr = "127.0.0.2:" + port
	if code := app.Healthcheck(context.Background(), app.Env{Cfg: elsewhere, Stdout: io.Discard, Stderr: io.Discard}); code != app.ExitError {
		t.Fatalf("healthcheck for an address the server is not bound to: exit %d, want %d", code, app.ExitError)
	}
	if code := stopServer(); code != app.ExitOK {
		t.Fatalf("serve exit %d: %s", code, w.stderr.String())
	}
	if code := check(); code != app.ExitError {
		t.Fatalf("healthcheck against a stopped server: exit %d, want %d", code, app.ExitError)
	}

	// The service only ever read from the bank, and nothing a bank returned reached a log.
	if v := fake.Violations(); len(v) != 0 {
		t.Fatalf("the service made requests a read-only client never makes: %+v", v)
	}
	logs := w.stderr.String()
	for _, marker := range []string{descMarker, merchMarker, state, "code_e2e_1", accountID, "pot_e2e_1"} {
		if strings.Contains(logs, marker) {
			t.Errorf("the log contains %q", marker)
		}
	}
	if !strings.Contains(logs, `"msg":"http request"`) {
		t.Errorf("no request log line was written; the log check above proves nothing:\n%s", logs)
	}
}

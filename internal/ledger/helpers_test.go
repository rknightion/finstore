package ledger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/importer"
	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/monzo"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// idNamespace is the namespace the contract fixes for every core id. It is repeated here, not
// read from the package, so that a change to it (which would change every stored id) fails.
var idNamespace = uuid.MustParse("4c8a89ea-d117-4623-8d6b-b458877667ba")

func wantAccountID(source, key string) uuid.UUID {
	return uuid.NewSHA1(idNamespace, []byte(source+"\x1f"+key))
}

func wantTxnID(account uuid.UUID, source, key string) uuid.UUID {
	return uuid.NewSHA1(idNamespace, []byte(account.String()+"\x1f"+source+"\x1f"+key))
}

const fakeSource = "fake"

// otherSource is a second source of the fake shape, for records one source sends about another
// source's account. lloydsSource reads the fake shape under the TrueLayer Lloyds source name, so
// the ledger gives its accounts the lloyds slug prefix and the running_balance method; nothing
// here is a TrueLayer response.
const (
	otherSource  = "fake-other"
	lloydsSource = "truelayer:ob-lloyds"
)

// The fake source's made-up response shape: one JSON object that may list accounts,
// transactions and balances.
type fAcc struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Opened   string `json:"opened,omitempty"` // YYYY-MM-DD
	Parent   string `json:"parent,omitempty"`
	Closed   bool   `json:"closed,omitempty"`
	Currency string `json:"currency,omitempty"`
	Name     string `json:"name,omitempty"`
	BankRef  string `json:"bank_ref,omitempty"`
}

type fTxn struct {
	ID          string `json:"id"`
	Account     string `json:"account"`
	Amount      int64  `json:"amount"`
	Created     string `json:"created"` // RFC 3339
	Status      string `json:"status,omitempty"`
	Description string `json:"description,omitempty"`
	// RunningBalance is the balance after the row, in decimal pounds as TrueLayer gives it.
	RunningBalance *fRunning `json:"running_balance,omitempty"`
}

type fRunning struct {
	Amount   json.Number `json:"amount"`
	Currency string      `json:"currency"`
}

// fSO and fDD are made-up mandates. ID stands for a bank's per-call id: it is kept in the content
// and left out of the hash input, as a real normaliser must.
type fSO struct {
	ID          string `json:"id,omitempty"`
	BankRef     string `json:"bank_ref"`
	Key         string `json:"key"`
	Payee       string `json:"payee"`
	Reference   string `json:"reference,omitempty"`
	Frequency   string `json:"frequency,omitempty"`
	First       string `json:"first,omitempty"` // YYYY-MM-DD
	FirstAmount int64  `json:"first_amount,omitempty"`
	Next        string `json:"next,omitempty"`
	NextAmount  int64  `json:"next_amount,omitempty"`
}

type fDD struct {
	ID       string `json:"id,omitempty"`
	BankRef  string `json:"bank_ref"`
	Key      string `json:"key"`
	Name     string `json:"name"`
	Status   string `json:"status,omitempty"`
	Previous string `json:"previous,omitempty"`
	Amount   int64  `json:"amount,omitempty"`
}

type fList struct {
	Kind    string `json:"kind"`
	BankRef string `json:"bank_ref"`
}

type fBal struct {
	Account string `json:"account"`
	Balance int64  `json:"balance"`
}

type fBody struct {
	Accounts     []fAcc            `json:"accounts,omitempty"`
	Transactions []json.RawMessage `json:"transactions,omitempty"`
	Balances     []fBal            `json:"balances,omitempty"`
	SOs          []fSO             `json:"standing_orders,omitempty"`
	DDs          []fDD             `json:"direct_debits,omitempty"`
	Lists        []fList           `json:"lists,omitempty"`
}

// fakeNormaliser reads the fake shape. It ignores non-2xx responses and errors on a body that is
// not the shape.
type fakeNormaliser struct{}

func (fakeNormaliser) NormaliseResponse(r ledger.Response, loc *time.Location) (ledger.Result, error) {
	if r.Status < 200 || r.Status > 299 {
		return ledger.Result{}, nil
	}
	var b fBody
	if err := json.Unmarshal(r.Body, &b); err != nil {
		return ledger.Result{}, errors.New("fake: bad body")
	}
	var res ledger.Result
	for _, a := range b.Accounts {
		rec := ledger.AccountRec{SourceKey: a.ID, Type: a.Type, Currency: "GBP", Name: "made-up " + a.Type,
			ParentSourceKey: a.Parent, Closed: a.Closed, BankRef: a.BankRef}
		if a.Currency != "" {
			rec.Currency = a.Currency
		}
		if a.Name != "" {
			rec.Name = a.Name
		}
		if a.Opened != "" {
			d, err := time.Parse("2006-01-02", a.Opened)
			if err != nil {
				return ledger.Result{}, err
			}
			rec.OpenedOn = d
		}
		res.Accounts = append(res.Accounts, rec)
	}
	for _, raw := range b.Transactions {
		var t fTxn
		if err := json.Unmarshal(raw, &t); err != nil {
			return ledger.Result{}, err
		}
		created, err := time.Parse(time.RFC3339, t.Created)
		if err != nil {
			return ledger.Result{}, err
		}
		local := created.In(loc)
		status := ledger.StatusSettled
		if t.Status != "" {
			status = ledger.Status(t.Status)
		}
		hash, _ := json.Marshal(t)
		res.Txns = append(res.Txns, ledger.TxnRec{AccountSourceKey: t.Account, SourceKey: t.ID,
			AmountMinor: t.Amount, Currency: "GBP", CreatedAt: &created,
			TxnDate: time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC),
			Status:  status, Description: t.Description, Content: raw, HashInput: hash})
	}
	for _, bal := range b.Balances {
		res.Balances = append(res.Balances, ledger.BalanceRec{AccountSourceKey: bal.Account,
			BalanceMinor: bal.Balance, Currency: "GBP", At: r.FetchedAt})
	}
	day := func(s string) (time.Time, error) {
		if s == "" {
			return time.Time{}, nil
		}
		return time.Parse("2006-01-02", s)
	}
	amount := func(n int64) *int64 {
		if n == 0 {
			return nil
		}
		return &n
	}
	for _, so := range b.SOs {
		content, _ := json.Marshal(so)
		biz := so
		biz.ID = ""
		hash, _ := json.Marshal(biz)
		first, err := day(so.First)
		if err != nil {
			return ledger.Result{}, err
		}
		next, err := day(so.Next)
		if err != nil {
			return ledger.Result{}, err
		}
		res.StandingOrders = append(res.StandingOrders, ledger.StandingOrderRec{BankRef: so.BankRef, MandateKey: so.Key,
			Payee: so.Payee, Reference: so.Reference, Frequency: so.Frequency, FirstDate: first,
			FirstAmountMinor: amount(so.FirstAmount), NextDate: next, NextAmountMinor: amount(so.NextAmount),
			Currency: "GBP", Content: content, HashInput: hash})
	}
	for _, dd := range b.DDs {
		content, _ := json.Marshal(dd)
		biz := dd
		biz.ID = ""
		hash, _ := json.Marshal(biz)
		prev, err := day(dd.Previous)
		if err != nil {
			return ledger.Result{}, err
		}
		res.DirectDebits = append(res.DirectDebits, ledger.DirectDebitRec{BankRef: dd.BankRef, MandateKey: dd.Key,
			Name: dd.Name, Status: dd.Status, PreviousDate: prev, PreviousAmountMinor: amount(dd.Amount),
			Currency: "GBP", Content: content, HashInput: hash})
	}
	for _, l := range b.Lists {
		res.MandateLists = append(res.MandateLists, ledger.MandateList{Kind: l.Kind, BankRef: l.BankRef})
	}
	return res, nil
}

func txn(id, account string, amount int64, created time.Time) json.RawMessage {
	b, _ := json.Marshal(fTxn{ID: id, Account: account, Amount: amount, Created: created.Format(time.RFC3339)})
	return b
}

func body(b fBody) string {
	out, err := json.Marshal(b)
	if err != nil {
		panic(err)
	}
	return string(out)
}

// env is a private cluster bootstrapped and migrated as production is, with a ledger over the
// fake source and both import formats.
type env struct {
	t     *testing.T
	super string
	app   *store.Store
	owner *store.Store
	l     *ledger.Ledger
	logs  *bytes.Buffer
	clock time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	super := testpg.Start(t)
	if err := store.Bootstrap(ctx, super, "owner-secret", "app-secret"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	ownerURL := testpg.UserURL(t, super, store.RoleOwner)
	if err := store.Migrate(ctx, ownerURL, ledger.NormaliserRev); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	e := &env{t: t, super: super, logs: &bytes.Buffer{},
		clock: time.Date(2030, time.March, 1, 9, 0, 0, 0, time.UTC)}
	e.app = openStore(t, testpg.UserURL(t, super, store.RoleApp))
	e.owner = openStore(t, ownerURL)
	e.l = e.ledger(true)
	return e
}

// ledger builds the env's ledger over the fake shape (as fakeSource, lloydsSource and, when
// withOther, otherSource), Monzo's own normaliser and both import formats.
func (e *env) ledger(withOther bool) *ledger.Ledger {
	files := map[string]ledger.FileNormaliser{
		string(importer.FormatHalifaxPDF): importer.Normaliser{},
		string(importer.FormatManualCSV):  importer.Normaliser{},
	}
	sources := map[string]ledger.SourceNormaliser{fakeSource: fakeNormaliser{}, lloydsSource: fakeNormaliser{},
		"monzo": monzo.Normaliser{}}
	if withOther {
		sources[otherSource] = fakeNormaliser{}
	}
	return ledger.New(sources, files, time.UTC, slog.New(slog.NewJSONHandler(e.logs, nil)))
}

func openStore(t *testing.T, url string) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// tick returns the next instant of the test's clock, a minute after the last.
func (e *env) tick() time.Time {
	e.clock = e.clock.Add(time.Minute)
	return e.clock
}

func (e *env) core() store.Core {
	c, err := store.NewCore(e.app.DB(), store.SchemaCore)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

// catchUp runs CatchUp into core under the writer lock, as serve does.
func (e *env) catchUp() ledger.Stats {
	e.t.Helper()
	var stats ledger.Stats
	err := e.app.WithWriterLock(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stats, err = e.l.CatchUp(ctx, tx, store.SchemaCore)
		return err
	})
	if err != nil {
		e.t.Fatalf("CatchUp: %v", err)
	}
	return stats
}

// renormalise runs Renormalise as the owner under the writer lock, as the command does.
func (e *env) renormalise(verify bool) []store.CoreDiff {
	e.t.Helper()
	var diffs []store.CoreDiff
	err := e.owner.HoldWriterLock(context.Background(), func(ctx context.Context, conn *pgx.Conn) error {
		var err error
		diffs, err = e.l.Renormalise(ctx, conn, verify)
		return err
	})
	if err != nil {
		e.t.Fatalf("Renormalise: %v", err)
	}
	return diffs
}

type period struct{ start, end time.Time }

// fetch records the start of a fetch of the fake source.
func (e *env) fetch(run int64, accountKey string, p *period) int64 {
	e.t.Helper()
	return e.fetchAs(fakeSource, run, accountKey, p)
}

// fetchAs records the start of a fetch of source.
func (e *env) fetchAs(source string, run int64, accountKey string, p *period) int64 {
	e.t.Helper()
	f := store.Fetch{Source: source, Kind: "hourly", RunID: run, StartedAt: e.tick()}
	if accountKey != "" {
		f.AccountKey = &accountKey
	}
	if p != nil {
		f.PeriodStart, f.PeriodEnd = &p.start, &p.end
	}
	id, err := store.InsertFetch(context.Background(), e.app.DB(), f)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// respond stores one 200 response of fetchID.
func (e *env) respond(fetchID int64, endpoint, b string) int64 {
	e.t.Helper()
	return e.respondAs(fetchID, fakeSource, endpoint, 200, b)
}

func (e *env) respondAs(fetchID int64, source, endpoint string, status int, b string) int64 {
	e.t.Helper()
	return e.store(fetchID, source, endpoint, status, b, e.tick())
}

// respondAt stores one 200 response of fetchID fetched at at, which may be long before the
// test's clock: a response from an old capture, loaded late.
func (e *env) respondAt(fetchID int64, endpoint, b string, at time.Time) int64 {
	e.t.Helper()
	return e.store(fetchID, fakeSource, endpoint, 200, b, at)
}

func (e *env) store(fetchID int64, source, endpoint string, status int, b string, at time.Time) int64 {
	e.t.Helper()
	seq, err := store.InsertAPIResponse(context.Background(), e.app.DB(), store.APIResponse{FetchID: fetchID,
		Source: source, Endpoint: endpoint, Status: status, ContentType: "application/json",
		FetchedAt: at, Body: []byte(b)})
	if err != nil {
		e.t.Fatal(err)
	}
	return seq
}

// complete records that fetchID received every page and returns the completion time.
func (e *env) complete(fetchID int64) time.Time {
	e.t.Helper()
	return e.completeAt(fetchID, e.tick())
}

// completeAt records that fetchID completed at at.
func (e *env) completeAt(fetchID int64, at time.Time) time.Time {
	e.t.Helper()
	if _, err := store.InsertFetchComplete(context.Background(), e.app.DB(),
		store.FetchComplete{FetchID: fetchID, CompletedAt: at, Pages: 1}); err != nil {
		e.t.Fatal(err)
	}
	return at
}

// accounts stores an account list in its own fetch.
func (e *env) accounts(accs ...fAcc) {
	e.t.Helper()
	e.respond(e.fetch(1, "", nil), "/accounts", body(fBody{Accounts: accs}))
}

// balance stores a /balance reading for accountKey in run.
func (e *env) balance(run int64, accountKey string, minor int64) time.Time {
	e.t.Helper()
	e.respond(e.fetch(run, accountKey, nil), "/balance", body(fBody{Balances: []fBal{{Account: accountKey, Balance: minor}}}))
	return e.clock
}

func (e *env) operator(kind string, payload any) {
	e.t.Helper()
	p, err := json.Marshal(payload)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := store.InsertOperatorEntry(context.Background(), e.app.DB(),
		store.OperatorEntry{Kind: kind, Payload: p, EnteredAt: e.tick()}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) importFile(slug, format string, data []byte) {
	e.t.Helper()
	if _, _, err := store.InsertImportFile(context.Background(), e.app.DB(), store.ImportFile{AccountSlug: slug,
		Format: format, FileName: "made-up", ImportedAt: e.tick(), Body: data}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) account(slug string) store.Account {
	e.t.Helper()
	a, err := e.core().GetAccountBySlug(context.Background(), slug)
	if err != nil {
		e.t.Fatalf("account %s: %v", slug, err)
	}
	return a
}

func (e *env) recon(id uuid.UUID) store.Reconciliation {
	e.t.Helper()
	r, err := e.core().GetReconciliation(context.Background(), id)
	if err != nil {
		e.t.Fatalf("reconciliation: %v", err)
	}
	return r
}

func (e *env) txn(id uuid.UUID) store.Transaction {
	e.t.Helper()
	tx, err := e.core().GetTransaction(context.Background(), id)
	if err != nil {
		e.t.Fatalf("transaction: %v", err)
	}
	return tx
}

func (e *env) countTxns(account uuid.UUID) int {
	e.t.Helper()
	var n int
	if err := e.app.DB().QueryRow(context.Background(),
		"SELECT count(*) FROM core.transaction WHERE account_id = $1", account).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) lastSeq() int64 {
	e.t.Helper()
	ps, err := e.core().GetProjectionState(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return ps.LastSeq
}

func (e *env) maxRawSeq() int64 {
	e.t.Helper()
	n, err := store.MaxRawSeq(context.Background(), e.app.DB())
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// csvRow is one made-up manual-csv row; balances are computed by csvFile.
type csvRow struct {
	date, typ string
	amount    int64
	desc      string
}

// csvFile renders rows as a manual-csv statement whose balances run on from opening.
func csvFile(opening int64, rows ...csvRow) []byte {
	var b strings.Builder
	b.WriteString("date,type,amount,balance_after,description\n")
	bal := opening
	for _, r := range rows {
		bal += r.amount
		fmt.Fprintf(&b, "%s,%s,%s,%s,%s\n", r.date, r.typ, pence(r.amount), pence(bal), r.desc)
	}
	return []byte(b.String())
}

func pence(n int64) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	return fmt.Sprintf("%s%d.%02d", sign, n/100, n%100)
}

func date(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d
}

func ptr[T any](v T) *T { return &v }

func sameDay(a *time.Time, b time.Time) bool {
	return a != nil && a.Year() == b.Year() && a.Month() == b.Month() && a.Day() == b.Day()
}

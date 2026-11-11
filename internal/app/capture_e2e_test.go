package app_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/app"
	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/monzotest"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// captureEntry is one response of a made-up capture directory.
type captureEntry struct {
	endpoint string
	params   map[string]any
	status   int
	at       time.Time
	body     string
	since    string // window of a transaction page; empty for anything else
	before   string
}

type captureRange struct {
	Account  string `json:"account_id"`
	Since    string `json:"since"`
	Before   string `json:"before"`
	Complete bool   `json:"complete"`
}

// writeCapture writes a capture directory in the layout load-capture reads.
func writeCapture(t *testing.T, entries []captureEntry, ranges []captureRange) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "bodies"), 0o700); err != nil {
		t.Fatal(err)
	}
	var index strings.Builder
	for i, e := range entries {
		name := fmt.Sprintf("%06d.body", i+1)
		if err := os.WriteFile(filepath.Join(dir, "bodies", name), []byte(e.body), 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(e.body))
		line := map[string]any{
			"seq": i + 1, "source": "monzo", "endpoint": e.endpoint, "params": e.params, "kind": "made-up",
			"status": e.status, "content_type": "application/json", "fetched_at": e.at.Format(time.RFC3339Nano),
			"sha256": hex.EncodeToString(sum[:]), "bytes": len(e.body), "body_file": name,
		}
		if e.since != "" {
			line["window_since"], line["window_before"] = e.since, e.before
		}
		b, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		index.Write(b)
		index.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "index.jsonl"), []byte(index.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	run, err := json.Marshal(map[string]any{"ranges": ranges})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.json"), run, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func txn(id, account, created string, amount int64, extra string) string {
	return fmt.Sprintf(`{"id":%q,"account_id":%q,"created":%q,"amount":%d,"currency":"GBP","local_amount":%d,"local_currency":"GBP","description":"made-up","settled":%q,"merchant":null,"notes":"","category":"general","scheme":"mastercard"%s}`,
		id, account, created, amount, amount, created, extra)
}

// TestLoadCaptureEndToEnd loads a history captured before the service existed, through the real
// loader, normaliser and projection, the way the first deployment does. The first directory is
// what a flawed first pass produced: a page cursor that skipped a row, marked complete anyway.
// The second directory refetches the same period correctly. The record must end up complete and
// checked against the balances, a closed account whose balance will never match must be
// acceptable, and loading everything again must change nothing.
func TestLoadCaptureEndToEnd(t *testing.T) {
	super := testpg.Start(t)
	env := map[string]string{
		config.EnvSuperuserDatabaseURL: super,
		config.EnvOwnerDatabaseURL:     testpg.UserURL(t, super, store.RoleOwner),
		config.EnvDatabaseURL:          testpg.UserURL(t, super, store.RoleApp),
		config.EnvOwnerPassword:        "owner-password",
		config.EnvAppPassword:          "app-password",
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, super: super, cfg: cfg, fake: monzotest.New(monzotest.WithClock(time.Now)), stderr: &syncBuffer{}}
	if code, _ := w.command(nil, app.Bootstrap); code != app.ExitOK {
		t.Fatalf("bootstrap: %s", w.stderr.String())
	}
	if code, _ := w.command(nil, app.Migrate); code != app.ExitOK {
		t.Fatalf("migrate: %s", w.stderr.String())
	}

	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	at := func(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }
	since, before := "2025-01-01T00:00:00Z", "2025-05-01T00:00:00Z"
	const open, closed = "acc_cap_open", "acc_cap_closed"
	accounts := `{"accounts":[` +
		`{"id":"acc_cap_open","type":"uk_retail","description":"made-up","created":"2024-12-31T09:00:00Z","closed":false,"currency":"GBP"},` +
		`{"id":"acc_cap_closed","type":"uk_prepaid","description":"made-up","created":"2024-12-31T08:00:00Z","closed":true,"currency":"GBP"}]}`
	page := func(txns ...string) string { return `{"transactions":[` + strings.Join(txns, ",") + `]}` }
	a := txn("tx_cap_a", open, "2025-01-10T10:00:00Z", 100000, "")
	b := txn("tx_cap_b", open, "2025-02-01T12:00:00Z", -2500, "")
	c := txn("tx_cap_c", open, "2025-02-01T12:00:00Z", -1500, "") // shares b's timestamp: the first pass skipped it
	d := txn("tx_cap_d", open, "2025-03-03T08:30:00Z", -999, `,"decline_reason":"INSUFFICIENT_FUNDS"`)
	old := txn("tx_cap_old", closed, "2025-01-05T10:00:00Z", -500, "")
	balance := func(n int64) string {
		return fmt.Sprintf(`{"balance":%d,"total_balance":%d,"currency":"GBP","spend_today":0}`, n, n)
	}
	const openBalance = 100000 - 2500 - 1500 // the declined transaction does not count

	first := writeCapture(t, []captureEntry{
		{endpoint: "/accounts", status: 200, at: at(0), body: accounts},
		{endpoint: "/balance", params: map[string]any{"account_id": open}, status: 200, at: at(1), body: balance(openBalance)},
		{endpoint: "/transactions", params: map[string]any{"account_id": open, "since": since, "before": before, "limit": 100}, status: 200, at: at(2), body: page(a, b), since: since, before: before},
		{endpoint: "/transactions", params: map[string]any{"account_id": open, "since": "tx_cap_b", "before": before, "limit": 100}, status: 200, at: at(3), body: page(d), since: since, before: before},
		{endpoint: "/transactions", params: map[string]any{"account_id": closed, "since": since, "before": before, "limit": 100}, status: 200, at: at(4), body: page(old), since: since, before: before},
		{endpoint: "/balance", params: map[string]any{"account_id": open}, status: 200, at: at(5), body: balance(openBalance)},
		{endpoint: "/pots", params: map[string]any{"current_account_id": open}, status: 200, at: at(6), body: `{"pots":[{"id":"pot_cap_1","name":"made-up","balance":4200,"currency":"GBP","deleted":false,"created":"2025-01-02T00:00:00Z","updated":"2025-01-02T00:00:00Z"}]}`},
		{endpoint: "/transactions", params: map[string]any{"account_id": open, "since": "2015-01-01T00:00:00Z", "limit": 1}, status: 403, at: at(7), body: `{"code":"forbidden.verification_required","message":"made-up"}`},
		{endpoint: "/balance", params: map[string]any{"account_id": closed}, status: 200, at: at(8), body: balance(-247)},
	}, []captureRange{
		{Account: open, Since: since, Before: before, Complete: true},
		{Account: closed, Since: since, Before: before, Complete: true},
	})
	second := writeCapture(t, []captureEntry{
		{endpoint: "/transactions", params: map[string]any{"account_id": open, "since": since, "before": before, "limit": 100}, status: 200, at: at(20), body: page(a, b, c, d), since: since, before: before},
	}, []captureRange{{Account: open, Since: since, Before: before, Complete: true}})

	load := func() (int, string) {
		return w.command(nil, func(ctx context.Context, e app.Env) int { return app.LoadCapture(ctx, e, []string{first, second}) })
	}
	if code, out := load(); code != app.ExitOK {
		t.Fatalf("load-capture exit %d:\n%s\n%s", code, out, w.stderr.String())
	}

	cov := w.coverage()
	got := cov["monzo-current"]
	if got.TransactionCount != 4 {
		t.Fatalf("open account holds %d transactions, want 4: the row the first pass skipped must come from the refetch", got.TransactionCount)
	}
	if got.ReconciliationState != store.StateReconciled {
		t.Fatalf("open account is %q after the load (difference %v), want reconciled: the two balance readings must be found and checked after every page",
			got.ReconciliationState, got.LastDiffMinor)
	}
	if n := w.queryInt(`SELECT count(*) FROM core.transaction WHERE disappeared_at IS NOT NULL`); n != 0 {
		t.Fatalf("%d transactions are marked as disappeared after loading a complete history", n)
	}
	if pot := cov["monzo-pot"]; pot.ReconciliationState != store.StateBalancesOnly {
		t.Fatalf("pot row: %+v", pot)
	}
	prepaid := cov["monzo-prepaid"]
	if prepaid.TransactionCount != 1 || prepaid.ReconciliationState != store.StateMismatch || prepaid.LastDiffMinor == nil || *prepaid.LastDiffMinor != 253 {
		t.Fatalf("closed account: %+v (difference %v), want a mismatch of 253 from one balance reading", prepaid, prepaid.LastDiffMinor)
	}

	// Loading the same directories again stores nothing and changes nothing.
	responses := w.queryInt(`SELECT count(*) FROM raw.api_response`)
	if code, out := load(); code != app.ExitOK || !strings.Contains(out, " 0 loaded") {
		t.Fatalf("second load exit %d:\n%s", code, out)
	}
	if n := w.queryInt(`SELECT count(*) FROM raw.api_response`); n != responses {
		t.Fatalf("a second load stored %d more responses", n-responses)
	}

	// The closed account's difference will never change, so the operator accepts it.
	accept := func(slug string) int {
		code, _ := w.command(nil, func(ctx context.Context, e app.Env) int {
			return app.AcceptDifference(ctx, e, slug, "closed years ago; the bank's own figure")
		})
		return code
	}
	if code := accept("monzo-prepaid"); code != app.ExitOK {
		t.Fatalf("accept-difference exit %d: %s", code, w.stderr.String())
	}
	if got := w.coverage()["monzo-prepaid"]; got.ReconciliationState != store.StateReconciled {
		t.Fatalf("closed account after accepting its difference: %q", got.ReconciliationState)
	}
	if code := accept("monzo-current"); code != app.ExitError {
		t.Fatalf("accepting a difference on an account that has none: exit %d, want %d", code, app.ExitError)
	}

	// The accepted difference is part of the stored record: a rebuild reproduces it.
	if code, out := w.command(nil, func(ctx context.Context, e app.Env) int { return app.Renormalise(ctx, e, true) }); code != app.ExitOK {
		t.Fatalf("renormalise --verify exit %d:\n%s\n%s", code, out, w.stderr.String())
	}
}

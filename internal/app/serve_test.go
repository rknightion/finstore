package app_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rknightion/finstore/internal/app"
	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/monzotest"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// TestServeRunsItsScheduledJobs starts the service with its scheduler on, which the end-to-end
// test turns off to drive every run itself. The status job's visible effect in the database is
// that a connect attempt past its time is marked expired; seeing that proves the scheduler is
// wired, the job runs at start, and the app role may do what the job does.
func TestServeRunsItsScheduledJobs(t *testing.T) {
	super := testpg.Start(t)
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
		config.EnvMonzoAPIURL:          "https://api.example.test",
		config.EnvMonzoAuthURL:         "https://auth.example.test/",
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
	w.queryInt(`INSERT INTO sync.connect_attempt (source, state_sha256, started_at, expires_at)
		VALUES ('monzo', '\x00', now() - interval '2 hours', now() - interval '1 hour') RETURNING id`)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	listening := make(chan net.Addr, 1)
	done := make(chan int, 1)
	go func() {
		done <- app.Serve(ctx, app.Env{Cfg: cfg, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: w.stderr},
			&app.Hooks{Listening: func(a net.Addr) { listening <- a }})
	}()
	select {
	case <-listening:
	case code := <-done:
		t.Fatalf("serve exited %d before listening: %s", code, w.stderr.String())
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not start listening")
	}

	deadline := time.Now().Add(20 * time.Second)
	for w.queryInt(`SELECT count(*) FROM sync.connect_attempt WHERE outcome = 'expired'`) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the status job did not run within 20 s of start: %s", w.stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	stop()
	select {
	case code := <-done:
		if code != app.ExitOK {
			t.Fatalf("serve exit %d: %s", code, w.stderr.String())
		}
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not stop")
	}
}

// TestServeThatCannotListenMakesNoBankCallAndLeavesStateAlone starts the service with its
// address taken, over a database a restart would resume work from: a Monzo consent awaiting
// approval and a TrueLayer one (each with a stored token, so Resume would call the bank), and a
// TrueLayer connect run a restart would end. The service must fail on the listen before any of
// that: no bank call, no background work and nothing in the database changed.
func TestServeThatCannotListenMakesNoBankCallAndLeavesStateAlone(t *testing.T) {
	super := testpg.Start(t)
	var hits atomic.Int32
	bank := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(bank.Close)
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })

	cfg := tlEnv(t, super, bank.URL, bank.URL)
	cfg.ListenAddr = taken.Addr().String()
	w := &world{t: t, super: super, cfg: cfg, fake: monzotest.New(monzotest.WithClock(time.Now)), stderr: &syncBuffer{}}
	if code, _ := w.command(nil, app.Bootstrap); code != app.ExitOK {
		t.Fatalf("bootstrap: %s", w.stderr.String())
	}
	if code, _ := w.command(nil, app.Migrate); code != app.ExitOK {
		t.Fatalf("migrate: %s", w.stderr.String())
	}
	sources := []string{"monzo", srcLloyds}
	for _, source := range sources {
		w.queryInt(`INSERT INTO sync.consent (source, state, token_issued_at, is_current)
			VALUES ($1, 'awaiting_approval', now() - interval '1 minute', true) RETURNING id`, source)
		w.queryInt(`INSERT INTO secret.oauth_token (source, access_token, refresh_token, issued_at, expires_at, updated_at)
			VALUES ($1, 'made-up-access', 'made-up-refresh', now(), now() + interval '1 hour', now()) RETURNING 1`, source)
	}
	w.queryInt(`INSERT INTO sync.run (source, kind, started_at) VALUES ($1, 'connect', now() - interval '1 minute') RETURNING id`, srcLloyds)

	done := make(chan int, 1)
	go func() {
		done <- app.Serve(context.Background(), app.Env{Cfg: cfg, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: w.stderr, Now: time.Now},
			&app.Hooks{Listening: func(net.Addr) { t.Error("the service reported listening on a taken address") }})
	}()
	select {
	case code := <-done:
		if code == app.ExitOK {
			t.Fatalf("serve on a taken address exited %d: %s", code, w.stderr.String())
		}
	case <-time.After(60 * time.Second):
		t.Fatal("serve on a taken address did not exit")
	}
	if !strings.Contains(w.stderr.String(), "listen on "+cfg.ListenAddr) {
		t.Errorf("the failure does not name the listen: %s", w.stderr.String())
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("%d bank calls before the service failed to listen, want none", n)
	}
	for _, source := range sources {
		if got := consentAt(super, source); got != "awaiting_approval/" {
			t.Errorf("%s consent is %q after the failed start, want awaiting_approval/ (Resume must not have run)", source, got)
		}
	}
	if n := w.queryInt(`SELECT count(*) FROM sync.run WHERE finished_at IS NOT NULL`); n != 0 {
		t.Errorf("%d runs ended by a start that never listened, want none", n)
	}
}

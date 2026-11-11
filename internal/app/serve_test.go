package app_test

import (
	"context"
	"io"
	"net"
	"strings"
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

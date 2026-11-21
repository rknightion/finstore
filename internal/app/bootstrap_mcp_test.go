package app

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// bootstrap gives the finstore_mcp login the way the command does: FINSTORE_MCP_PASSWORD set turns it
// on, empty or absent turns it off. No command requires the variable.
func TestBootstrapTurnsTheMCPLoginOnAndOffWithItsPassword(t *testing.T) {
	ctx := context.Background()
	super := testpg.Start(t)
	bootstrap := func(mcpPassword string) {
		t.Helper()
		env := map[string]string{
			config.EnvSuperuserDatabaseURL: super,
			config.EnvOwnerPassword:        "owner-password",
			config.EnvAppPassword:          "app-password",
			config.EnvMCPPassword:          mcpPassword,
		}
		cfg, err := config.Load(func(k string) string { return env[k] })
		if err != nil {
			t.Fatal(err)
		}
		var stderr, stdout bytes.Buffer
		if code := Bootstrap(ctx, Env{Cfg: cfg, Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr}); code != ExitOK {
			t.Fatalf("bootstrap exit %d: %s", code, stderr.String())
		}
		if strings.Contains(stdout.String()+stderr.String(), mcpPassword) && mcpPassword != "" {
			t.Fatal("bootstrap printed the MCP password")
		}
	}
	login := func() (bool, bool) {
		t.Helper()
		conn, err := pgx.Connect(ctx, super)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close(ctx) }()
		var canLogin, hasPassword bool
		if err := conn.QueryRow(ctx, "SELECT rolcanlogin, rolpassword IS NOT NULL FROM pg_authid WHERE rolname = $1",
			store.RoleMCP).Scan(&canLogin, &hasPassword); err != nil {
			t.Fatal(err)
		}
		return canLogin, hasPassword
	}

	bootstrap("")
	if l, p := login(); l || p {
		t.Fatalf("bootstrap without FINSTORE_MCP_PASSWORD: login=%v password=%v, want NOLOGIN and none", l, p)
	}
	bootstrap("mcp-password")
	if l, p := login(); !l || !p {
		t.Fatalf("bootstrap with FINSTORE_MCP_PASSWORD: login=%v password=%v, want both", l, p)
	}
	bootstrap("mcp-password")
	if l, p := login(); !l || !p {
		t.Fatalf("a second bootstrap: login=%v password=%v, want both", l, p)
	}
	bootstrap("")
	if l, p := login(); l || p {
		t.Fatalf("bootstrap with the password removed: login=%v password=%v, want NOLOGIN and none", l, p)
	}
}

// bootstrap reports, on stderr and by the log allow-list, that finstore_mcp had changed its own
// settings, and says how many, never which setting or what it was set to.
func TestBootstrapReportsMCPSettingsTheRoleChangedForItself(t *testing.T) {
	ctx := context.Background()
	super := testpg.Start(t)
	env := map[string]string{
		config.EnvSuperuserDatabaseURL: super,
		config.EnvOwnerPassword:        "owner-password",
		config.EnvAppPassword:          "app-password",
		config.EnvMCPPassword:          "mcp-password",
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := func() string {
		t.Helper()
		var stderr, stdout bytes.Buffer
		if code := Bootstrap(ctx, Env{Cfg: cfg, Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr}); code != ExitOK {
			t.Fatalf("bootstrap exit %d: %s", code, stderr.String())
		}
		return stderr.String()
	}

	if got := bootstrap(); got != "" {
		t.Fatalf("first bootstrap logged: %q", got)
	}
	if got := bootstrap(); got != "" {
		t.Fatalf("bootstrap over an untouched role logged: %q", got)
	}

	conn, err := pgx.Connect(ctx, testpg.UserURL(t, super, store.RoleMCP))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	for _, stmt := range []string{
		"SET default_transaction_read_only = off", // the role's default makes ALTER ROLE fail
		"ALTER ROLE finstore_mcp SET statement_timeout = '1h'",
		"ALTER ROLE finstore_mcp SET work_mem = '999MB'",
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	got := bootstrap()
	line := strings.TrimSpace(got)
	if strings.Count(line, "\n") != 0 || line == "" {
		t.Fatalf("want exactly one log line on stderr, got %q", got)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("stderr is not one JSON log line: %q: %v", got, err)
	}
	if rec["level"] != "WARN" || rec["msg"] != "mcp role settings drifted and were reset" {
		t.Errorf("log line = %v, want a WARN saying the settings drifted and were reset", rec)
	}
	if rec["kind"] != "role_setting" || rec["count"] != float64(2) {
		t.Errorf("log line = %v, want kind role_setting and count 2", rec)
	}
	for _, leak := range []string{"statement_timeout", "work_mem", "999MB", "1h"} {
		if strings.Contains(line, leak) {
			t.Errorf("log line repeats what the login chose (%q): %s", leak, line)
		}
	}
	if got := bootstrap(); got != "" {
		t.Errorf("a bootstrap after the reset logged: %q", got)
	}
}

package app

import (
	"bytes"
	"context"
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

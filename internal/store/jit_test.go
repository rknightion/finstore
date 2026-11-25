package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// JIT compilation cost more than it saved on finstore's queries, so Bootstrap turns it off for the
// database. A database-level setting reaches every new session, whichever role opens it.
func TestBootstrapTurnsJITOffForNewSessions(t *testing.T) {
	ctx := context.Background()
	super := testpg.Start(t)
	bootstrap := func() {
		t.Helper()
		if err := store.Bootstrap(ctx, super, "owner-secret", "app-secret"); err != nil {
			t.Fatalf("Bootstrap: %v", err)
		}
	}
	// The cluster default is on, so the assertions below cannot pass by accident.
	if got := showJIT(t, super); got != "on" {
		t.Fatalf("before Bootstrap, jit = %q, want the default on", got)
	}
	bootstrap()

	// finstore_agent has no login: a member is a separate login role that inherits from it.
	conn := connect(t, super)
	if _, err := conn.Exec(ctx, "CREATE ROLE jit_agent_member LOGIN IN ROLE "+pgx.Identifier{store.RoleAgent}.Sanitize()); err != nil {
		t.Fatalf("create agent member: %v", err)
	}
	if err := store.EnableMCPLogin(ctx, super, "mcp-secret"); err != nil {
		t.Fatalf("EnableMCPLogin: %v", err)
	}
	check := func(when string) {
		t.Helper()
		for name, url := range map[string]string{
			"finstore_agent member": testpg.UserURL(t, super, "jit_agent_member"),
			"finstore_mcp":          testpg.UserURL(t, super, store.RoleMCP),
			"finstore_owner":        testpg.UserURL(t, super, store.RoleOwner),
		} {
			if got := showJIT(t, url); got != "off" {
				t.Errorf("%s: a new session as %s reports jit = %q, want off", when, name, got)
			}
		}
	}
	check("after Bootstrap")
	bootstrap() // idempotent: no error, and the MCP role's settings reset does not undo it
	check("after a second Bootstrap")
}

func showJIT(t *testing.T, url string) string {
	t.Helper()
	conn := connect(t, url)
	var v string
	if err := conn.QueryRow(context.Background(), "SHOW jit").Scan(&v); err != nil {
		t.Fatalf("SHOW jit: %v", err)
	}
	return v
}

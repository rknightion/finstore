// Package testpg supplies an isolated native PostgreSQL cluster for tests.
// Missing binaries or startup failure are fatal, never a skip and never a Docker fallback.
package testpg

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Superuser is the cluster's bootstrap superuser, the role Start's URL connects as.
const Superuser = "finstore_super"

// Database is the database Start creates for the test.
const Database = "finstore"

// Start starts a private cluster and returns a pgx URL for Superuser on Database, over a Unix
// socket with TCP disabled. Authentication is trust, so any role in the cluster can connect
// without a password: use UserURL to connect as another role. The cluster is stopped and removed
// when the test ends. Tests never share a cluster with another top-level test or with a
// developer's running database.
func Start(t testing.TB) string {
	t.Helper()
	bins := make(map[string]string)
	for _, name := range []string{"initdb", "pg_ctl", "postgres", "psql"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("native PostgreSQL prerequisite: %s: %v", name, err)
		}
		bins[name] = path
	}
	// Keep the Unix socket pathname below the macOS 104-byte limit even when
	// Go's per-test temporary directory name is long.
	root, err := os.MkdirTemp("", "finstore-pg-")
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "data")
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	execute := func(name string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bins[name], args...) // #nosec G204 -- binaries found on PATH, fixed arguments
		// Do not let a developer's PG environment redirect utility connections.
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "PG") {
				cmd.Env = append(cmd.Env, value)
			}
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := execute("initdb", "-D", data, "-A", "trust", "-U", Superuser, "--no-locale", "--encoding=UTF8"); err != nil {
		t.Fatalf("initdb: %v\n%s", err, out)
	}
	// Configuration is a file, not shell quoting of a potentially unusual path.
	conf := "listen_addresses = ''\nunix_socket_permissions = 0700\nunix_socket_directories = '" +
		strings.ReplaceAll(root, "'", "''") + "'\nfsync = off\nfull_page_writes = off\nsynchronous_commit = off\n"
	if err := os.WriteFile(filepath.Join(data, "postgresql.conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if out, err := execute("pg_ctl", "-D", data, "-m", "immediate", "-w", "-t", "10", "stop"); err != nil {
			t.Errorf("stop native PostgreSQL: %v\n%s", err, out)
		}
	})
	if out, err := execute("pg_ctl", "-D", data, "-l", filepath.Join(root, "server.log"), "-w", "-t", "15", "start"); err != nil {
		log, _ := os.ReadFile(filepath.Join(root, "server.log")) // #nosec G304 -- our own temporary directory
		t.Fatalf("start native PostgreSQL: %v\n%s\n%s", err, out, log)
	}
	if out, err := execute("psql", "-X", "-h", root, "-U", Superuser, "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-c", "CREATE DATABASE "+Database); err != nil {
		t.Fatalf("create native fixture database: %v\n%s", err, out)
	}
	t.Logf("native PostgreSQL cluster via %s; TCP disabled", bins["postgres"])
	return fmt.Sprintf("postgresql://%s@/%s?host=%s&sslmode=disable", Superuser, Database, url.QueryEscape(root))
}

// UserURL returns rawURL with its user replaced by user, for connecting to the same cluster and
// database as another role.
func UserURL(t testing.TB, rawURL, user string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	u.User = url.User(user)
	return u.String()
}

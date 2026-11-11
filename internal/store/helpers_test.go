package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

const testRev = 1

// cluster is a private PostgreSQL cluster bootstrapped and migrated as production is.
type cluster struct {
	super, owner, app string
}

func newCluster(t *testing.T) cluster {
	t.Helper()
	ctx := context.Background()
	super := testpg.Start(t)
	if err := store.Bootstrap(ctx, super, "owner-secret", "app-secret"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	c := cluster{
		super: super,
		owner: testpg.UserURL(t, super, store.RoleOwner),
		app:   testpg.UserURL(t, super, store.RoleApp),
	}
	if err := store.Migrate(ctx, c.owner, testRev); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return c
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

// connAs returns a superuser connection that has done SET ROLE role.
func connAs(t *testing.T, superURL, role string) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, superURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatalf("SET ROLE %s: %v", role, err)
	}
	return conn
}

func requireDenied(t *testing.T, err error, what string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("%s: want insufficient_privilege (42501), got %v", what, err)
	}
}

func ptr[T any](v T) *T { return &v }

func mustCore(t *testing.T, q store.DBTX, schema string) store.Core {
	t.Helper()
	c, err := store.NewCore(q, schema)
	if err != nil {
		t.Fatalf("NewCore: %v", err)
	}
	return c
}

// londonToday is the database's current Europe/London date, the "today" the views use.
func londonToday(t *testing.T, q store.DBTX) time.Time {
	t.Helper()
	var d time.Time
	if err := q.QueryRow(context.Background(), "SELECT (now() AT TIME ZONE 'Europe/London')::date").Scan(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

// londonMidnight is the instant a Europe/London calendar day starts.
func londonMidnight(t *testing.T, day time.Time) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
}

func day(t time.Time, offset int) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day()+offset, 0, 0, 0, 0, time.UTC)
}

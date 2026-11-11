package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Role names.
const (
	RoleOwner = "finstore_owner"
	RoleApp   = "finstore_app"
	RoleAgent = "finstore_agent"
)

// Schemas lists the five schemas Bootstrap creates, all owned by RoleOwner.
var Schemas = []string{"raw", "core", "sync", "secret", "agent"}

// Bootstrap connects as a superuser to the database named in superuserURL and creates the three
// roles, sets the two passwords, gives the owner CREATE on the database (which creating and
// renaming core_next needs) and creates the five schemas owned by the owner. Running it again
// changes nothing but the passwords.
func Bootstrap(ctx context.Context, superuserURL, ownerPassword, appPassword string) error {
	if ownerPassword == "" || appPassword == "" {
		return errors.New("store: bootstrap: both passwords must be set")
	}
	conn, err := pgx.Connect(ctx, superuserURL)
	if err != nil {
		return fmt.Errorf("store: bootstrap: connect: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		// Serialise concurrent bootstraps: CREATE ROLE has no IF NOT EXISTS.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", MigrateLockKey); err != nil {
			return fmt.Errorf("store: bootstrap: lock: %w", err)
		}
		roles := []struct{ name, attrs, password string }{
			{RoleOwner, "LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS INHERIT", ownerPassword},
			{RoleApp, "LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS INHERIT", appPassword},
			{RoleAgent, "NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS INHERIT", ""},
		}
		for _, r := range roles {
			var exists bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", r.name).Scan(&exists); err != nil {
				return fmt.Errorf("store: bootstrap: look up role %s: %w", r.name, err)
			}
			verb := "ALTER"
			if !exists {
				verb = "CREATE"
			}
			// Role names and attributes are constants; the password is quoted as a literal by
			// the server's format(%L), never concatenated here.
			var stmt string
			if r.password == "" {
				stmt = fmt.Sprintf("%s ROLE %s %s PASSWORD NULL", verb, pgx.Identifier{r.name}.Sanitize(), r.attrs)
			} else {
				tmpl := verb + " ROLE %I " + r.attrs + " PASSWORD %L"
				if err := tx.QueryRow(ctx, "SELECT format($1::text, $2::text, $3::text)",
					tmpl, r.name, r.password).Scan(&stmt); err != nil {
					return fmt.Errorf("store: bootstrap: quote password for %s: %w", r.name, err)
				}
			}
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("store: bootstrap: %s role %s: %w", verb, r.name, err)
			}
		}
		owner := pgx.Identifier{RoleOwner}.Sanitize()
		var db string
		if err := tx.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
			return fmt.Errorf("store: bootstrap: current database: %w", err)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf("GRANT CREATE, CONNECT, TEMPORARY ON DATABASE %s TO %s",
			pgx.Identifier{db}.Sanitize(), owner)); err != nil {
			return fmt.Errorf("store: bootstrap: grant database to owner: %w", err)
		}
		for _, s := range Schemas {
			id := pgx.Identifier{s}.Sanitize()
			if _, err := tx.Exec(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s AUTHORIZATION %s", id, owner)); err != nil {
				return fmt.Errorf("store: bootstrap: create schema %s: %w", s, err)
			}
			if _, err := tx.Exec(ctx, fmt.Sprintf("ALTER SCHEMA %s OWNER TO %s", id, owner)); err != nil {
				return fmt.Errorf("store: bootstrap: own schema %s: %w", s, err)
			}
		}
		return nil
	})
}

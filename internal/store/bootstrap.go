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
	// RoleMCP is the read-only login the MCP endpoint connects as. It reads agent, core, raw and
	// sync and has no right on secret. Bootstrap creates it NOLOGIN; EnableMCPLogin turns the
	// login on.
	RoleMCP = "finstore_mcp"
)

// Schemas lists the five schemas Bootstrap creates, all owned by RoleOwner.
var Schemas = []string{"raw", "core", "sync", "secret", "agent"}

// mcpAttrs are the fixed attributes of RoleMCP apart from LOGIN and the password, which
// EnableMCPLogin owns. Bootstrap sets them on every run, so a role someone loosened is tightened.
const mcpAttrs = "NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS INHERIT CONNECTION LIMIT 4"

// mcpSettings are RoleMCP's per-role settings, applied on every login. They are defaults: a login
// as the role can change them for itself (ALTER ROLE on itself is allowed, and temp_file_limit is
// the one it cannot raise, being superuser only). Bootstrap puts them back on every run, and the
// Reader does not rely on them: it sets the first four as startup parameters of its connections.
// The grants, not the settings, keep the role from writing.
var mcpSettings = []struct{ name, value string }{
	{"default_transaction_read_only", "on"},
	{"statement_timeout", "'15s'"},
	{"idle_in_transaction_session_timeout", "'30s'"},
	{"lock_timeout", "'2s'"},
	{"temp_file_limit", "'1GB'"},
}

// Bootstrap connects as a superuser to the database named in superuserURL and creates the four
// roles (the owner and the app with the two passwords, and the NOLOGIN agent and MCP roles), sets
// RoleMCP's settings, gives the owner CREATE on the database (which creating and renaming
// core_next needs) and creates the five schemas owned by the owner. Running it again changes
// nothing but the two passwords: RoleMCP's login and password are left as EnableMCPLogin set them.
//
// Bootstrap must run before Migrate, whose grants name RoleMCP.
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
		if err := bootstrapMCPRole(ctx, tx); err != nil {
			return err
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

// bootstrapMCPRole creates RoleMCP when it is absent (NOLOGIN, no password), tightens its fixed
// attributes when it exists, and sets its settings. An existing role keeps its LOGIN and password.
func bootstrapMCPRole(ctx context.Context, tx pgx.Tx) error {
	id := pgx.Identifier{RoleMCP}.Sanitize()
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", RoleMCP).Scan(&exists); err != nil {
		return fmt.Errorf("store: bootstrap: look up role %s: %w", RoleMCP, err)
	}
	stmt := "ALTER ROLE " + id + " " + mcpAttrs
	if !exists {
		stmt = "CREATE ROLE " + id + " NOLOGIN " + mcpAttrs + " PASSWORD NULL"
	}
	if _, err := tx.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("store: bootstrap: configure role %s: %w", RoleMCP, err)
	}
	// The role can change its own settings from a login (ALTER ROLE ... SET, also IN DATABASE, which
	// a role-wide setting does not override), so start from none: first every per-database row, then
	// the role-wide ones. Anything it set for itself is gone, and the constants below are all that remain.
	rows, err := tx.Query(ctx, `SELECT d.datname::text FROM pg_db_role_setting s
		JOIN pg_database d ON d.oid = s.setdatabase
		WHERE s.setrole = (SELECT oid FROM pg_roles WHERE rolname = $1) AND s.setdatabase <> 0`, RoleMCP)
	if err != nil {
		return fmt.Errorf("store: bootstrap: list per-database settings of %s: %w", RoleMCP, err)
	}
	dbs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("store: bootstrap: list per-database settings of %s: %w", RoleMCP, err)
	}
	for _, db := range dbs {
		if _, err := tx.Exec(ctx, "ALTER ROLE "+id+" IN DATABASE "+pgx.Identifier{db}.Sanitize()+" RESET ALL"); err != nil {
			return fmt.Errorf("store: bootstrap: reset settings of %s in %s: %w", RoleMCP, db, err)
		}
	}
	if _, err := tx.Exec(ctx, "ALTER ROLE "+id+" RESET ALL"); err != nil {
		return fmt.Errorf("store: bootstrap: reset settings of %s: %w", RoleMCP, err)
	}
	for _, s := range mcpSettings {
		// Names and values are constants in this file.
		if _, err := tx.Exec(ctx, "ALTER ROLE "+id+" SET "+s.name+" = "+s.value); err != nil {
			return fmt.Errorf("store: bootstrap: set %s on role %s: %w", s.name, RoleMCP, err)
		}
	}
	return nil
}

// EnableMCPLogin connects as a superuser and gives RoleMCP a login with password, or, when password
// is empty, takes the login and the password away. Bootstrap must have run first. The password is
// quoted by the server's format(%L), never concatenated here. Running it again with the same
// password changes nothing.
func EnableMCPLogin(ctx context.Context, superuserURL, password string) error {
	conn, err := pgx.Connect(ctx, superuserURL)
	if err != nil {
		return fmt.Errorf("store: enable mcp login: connect: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		// The same lock Bootstrap takes, so the two never alter the role at once.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", MigrateLockKey); err != nil {
			return fmt.Errorf("store: enable mcp login: lock: %w", err)
		}
		var stmt string
		if password == "" {
			stmt = "ALTER ROLE " + pgx.Identifier{RoleMCP}.Sanitize() + " NOLOGIN PASSWORD NULL"
		} else if err := tx.QueryRow(ctx, "SELECT format('ALTER ROLE %I LOGIN PASSWORD %L', $1::text, $2::text)",
			RoleMCP, password).Scan(&stmt); err != nil {
			return fmt.Errorf("store: enable mcp login: quote password: %w", err)
		}
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("store: enable mcp login: alter role %s: %w", RoleMCP, err)
		}
		return nil
	})
}

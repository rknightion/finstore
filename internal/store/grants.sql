-- The grant matrix. Applied by migrate and again after every core swap, as finstore_owner, which
-- owns every object named here. GRANT is idempotent.

-- finstore_app: what serve, import, account and load-capture use.
GRANT USAGE ON SCHEMA raw, core, sync, secret, agent TO finstore_app;
GRANT USAGE ON SEQUENCE raw.seq TO finstore_app;
GRANT SELECT, INSERT ON ALL TABLES IN SCHEMA raw TO finstore_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA core TO finstore_app;
GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA sync, secret TO finstore_app;
-- The bigserial keys of sync need their sequences; raw.seq is the only sequence in raw.
GRANT USAGE ON ALL SEQUENCES IN SCHEMA sync TO finstore_app;
GRANT SELECT ON ALL TABLES IN SCHEMA agent TO finstore_app;
-- goose's version table lives in sync but belongs to migrate alone.
REVOKE ALL ON TABLE sync.goose_db_version FROM finstore_app;
DO $$
BEGIN
  EXECUTE format('REVOKE ALL ON SEQUENCE %s FROM finstore_app',
                 pg_get_serial_sequence('sync.goose_db_version', 'id'));
END
$$;

-- finstore_agent: the agent views and nothing else. The views are owned by finstore_owner, so
-- they read core, sync and raw with the owner's rights.
GRANT USAGE ON SCHEMA agent TO finstore_agent;
GRANT SELECT ON ALL TABLES IN SCHEMA agent TO finstore_agent;

-- finstore_mcp: everything but secret, read only. The role's defaults make every transaction
-- read only; it has no INSERT, UPDATE, DELETE, TRUNCATE or DDL right anywhere. secret is
-- deliberately absent: never add it, and never grant on ALL SCHEMAS. The role must exist before
-- this runs: bootstrap creates it, so bootstrap runs before migrate.
GRANT USAGE ON SCHEMA agent, core, raw, sync TO finstore_mcp;
GRANT SELECT ON ALL TABLES IN SCHEMA agent, core, raw, sync TO finstore_mcp;

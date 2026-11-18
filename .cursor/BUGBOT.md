# FinStore review guide

## What this is

FinStore is a self-hosted Go service for collecting and reconciling personal bank data in PostgreSQL. `cmd/finstore/main.go` wires the CLI. `internal/app/`, `internal/monzo/` and `internal/importer/` connect synchronization and statement import to storage.

## Invariants

- All bank HTTP requests go through the read-only allowlist in `internal/bankhttp/`. Add a permitted request shape and a focused test together. Keep bank response text and statement contents in the database. Outside it, errors carry categories and telemetry uses only approved fields from `internal/semconv/`.
- `internal/store/` owns SQL, and captured `raw` rows are append-only. `internal/ledger/` normalizes and reconciles `core` in raw sequence order. Preserve transaction history and balance reconciliation across live processing, duplicate intake and replay.
- Use only synthetic data in fixtures and review evidence. Preserve the read-only MCP role and authentication boundary across `internal/mcpserver/`, `internal/app/` and `internal/store/`.

## Where to look hardest

- For OAuth, retry, transport or telemetry changes, follow the same synthetic value through log and span sinks. `internal/bankhttp/telemetry_test.go` and `internal/web/telemetry_test.go` cover telemetry boundaries. Use canaries that cannot match unrelated trace IDs. Follow `internal/monzo/sync_test.go` for callback synchronization before request-count assertions.
- Verify migration and transaction changes against the PostgreSQL 18 tests in `internal/store/` and `internal/ledger/`. Check rebuild, pending transactions and reconciliation; inspect grants when a view or MCP tool changes.
- Keep runtime options and defaults in `internal/config/`, together with operator-facing behavior in `docs/operations.md`.

## Checks already covered

The full gate checks formatting, lint, vet, race tests, module tidiness, build and govulncheck. Hosted workflows also check workflow safety, dependency changes and the container build.

## Verification

Run `just setup` and `just check </dev/null>` with the PostgreSQL 18 server binaries on `PATH`. The tests start a private cluster. Use `just check-fast </dev/null>` for workstation iteration and the full gate before commit. Test bank requests with synthetic fakes.

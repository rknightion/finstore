# Loop: finstore
tier: guarded
gate: just check
ci-required: ci-success
release-on-push: no
deploy-on-push: no
grafana-stack: robknight
tracker: github
landing: pr

Public repository: no hostname, account id, address, email or internal identifier in any tracked
file or loop artifact. No bank payload, real or redacted, in a fixture, a log, an issue or a pull
request. `just test` needs the PostgreSQL 18 server binaries on `PATH`.

Tracker model: `~/repos/agent-docs/docs/github-tracker.md` (maintainers only).

## Queue

- The queue is `just tracker queue` (Status Ready on the rknightion Project).
- A charter selects by labels and Project fields. Never queue a `needs-triage` issue or the
  Dependency Dashboard.
- Only the root writes issues and the Project, through `just tracker`; a lane returns the status and
  AC ticks it wants.
- A queue row names the issue as `#N`.

## Credentials

- The repository holds none. A deployment's bank client secret, database passwords and telemetry
  token live in that deployment's env files, outside this repository.
- Never copy a token, account id, email, address or hostname into the repo, a log, argv or a
  fixture.

## Traps

- A test fixture is written by hand to match the documented API shape. Never derive one from a real
  response, redacted or not.
- The bank HTTP client refuses anything but its allow-listed reads and the token call. A new bank
  endpoint needs an allow-list entry and a test, never a second client.
- Logging is by allow-list: a field that is not in `internal/semconv` is dropped. Never log a body,
  a header, a token or a URL.
- Monzo refuses a time range longer than 365 days, and paging by transaction id skips rows that
  share a timestamp. Page by timestamp and drop duplicates by id.
- `core` is rebuilt from `raw`. A change to normalising code needs the normaliser revision bumped
  and `finstore renormalise` on every deployment.

## Mutexes

- One deployment has one Monzo token. Never run a second process against the same OAuth client.

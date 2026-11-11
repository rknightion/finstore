# finstore

A self-hosted datastore for one person's bank data. It pulls a Monzo account through the Monzo
Developer API, imports statement files for accounts no API reaches, and keeps everything in
Postgres: every source payload byte for byte, every version of a transaction, and a check of the
stored transactions against the balances the bank reports. It is built to be queried, not looked
at: there is no budgeting and no UI beyond a status page.

## Task interface

`just check` is the gate and must pass before you commit. Run `just` with stdin from `/dev/null`.
The tests start a private PostgreSQL cluster, so the PostgreSQL 18 server binaries (`initdb`,
`pg_ctl`, `postgres`) must be on `PATH`. A missing binary fails the tests; it never skips them.

## Tracking work

Work is tracked in this repository's GitHub issues; they are the only queue. Do not add a checklist
file such as `ROADMAP.md` beside them.

- A pull request body says how it relates to an issue, on a line of its own: `Closes #N` with the
  `Acceptance evidence` table from `.github/pull_request_template.md` (one row for each acceptance
  criterion of that issue), `Refs #N` when it only relates to one, or `Task: none, <reason>`.
- A pull request that changes `.github/workflows/` or `.github/actions/` carries no closing keyword:
  write `Task: none, <reason>`, with `Refs #N` beside it if you like.
- Contributors never need the project board or the tracker tooling; a maintainer handles both.
- Issues and pull requests are public, so they carry no real identifiers: no email addresses,
  account ids, host names or addresses, and no bank data of any kind. Write the shape, not the
  instance.

## Rules that are easy to break

- **Read-only toward banks.** The service never calls an endpoint that changes anything at a bank.
  `internal/bankhttp` is the only package that talks to a bank, and it refuses any request that is
  not on its allow-list. Add an endpoint there, with a test, or not at all.
- **No bank data outside the database.** Nothing from a bank response or a statement goes into a
  log line, a metric, an error message, a test fixture, an issue or a pull request. Errors from
  the bank client are categories, never text from the response.
- **Logging is by allow-list.** `internal/semconv` lists the fields a log line may carry and the
  log handler drops the rest. Add a field there, deliberately, or do not log it.
- **Fixtures are written by hand.** A fixture matches the documented shape of an API response or a
  statement and holds made-up values. Never derive one from a real response or file, redacted or
  not.
- **Nothing names a deployment.** Hosts, addresses, bank account identifiers and people come from
  configuration. The code has no default that names anyone's setup.

## Ownership seams

- `internal/config` owns the `FINSTORE_` environment variables. There is no configuration file.
- `internal/semconv` owns every metric name, attribute key and log field.
- `internal/store` owns all SQL. `raw` is append-only: it is never updated or deleted from.
- `internal/ledger` owns `core`: `Normalise` and `Reconcile` are its only writers, and they process
  `raw` rows in sequence order, live and on replay.
- `internal/bankhttp` owns every request to a bank.

## Deeper references

- `docs/operations.md` - read before changing a command's behaviour, the schemas or the deploy
  example: it is the operator's contract.

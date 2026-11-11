# finstore

A self-hosted datastore for your own bank data, built to be queried by tools and AI agents rather
than looked at. It keeps what the bank sent, byte for byte, keeps every version of a transaction,
and checks the stored transactions against the balances the bank reports.

Status: early. It runs one person's deployment; expect rough edges and breaking changes.

## What it does

- **Monzo**, through the Monzo Developer API: a full-history download when you approve access,
  then an hourly sync. Monzo only serves full history for a few minutes after each approval, so
  the download runs the moment you approve.
- **Statement files** for accounts no API reaches. The first format is the PDF statement from the
  Halifax mobile app; a plain CSV layout covers anything you type in by hand.
- **Postgres** holds it: raw payloads, a normalised projection rebuilt from them, and read-only
  views for whatever queries it.

It never writes to a bank. It is for one person's own accounts: Monzo's developer API is offered
for personal use only.

## Running it

`docs/operations.md` is the guide and `deploy/` is a working example. In short: a Postgres 18
database, one container, a Monzo OAuth client of your own, and something in front that knows who
the user is: the example gives the service its own Tailscale node, and a reverse proxy with single
sign-on works too. Nothing should be reachable from the public internet.

```sh
docker compose up -d finstore-db
docker compose run --rm finstore-admin bootstrap
docker compose run --rm finstore-admin migrate
docker compose up -d finstore finstore-ts
```

Then open the status page and connect Monzo.

## Reading the data

Queries go through the `agent` schema: read-only views over accounts, transactions, every version
of each transaction, balances, and a coverage view that says what is held for each account, whether
it agrees with the bank's balance, and where the gaps are. The role `finstore_agent` can read those
views and nothing else: not the raw payloads, not the tokens.

## Development

`just check` is the gate. The tests need the PostgreSQL 18 server binaries on `PATH`.

## Licence

Apache-2.0. See [LICENSE](LICENSE).

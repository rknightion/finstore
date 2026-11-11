# finstore

A self-hosted datastore for your own bank data, built to be queried by tools and AI agents rather
than looked at. It keeps what the bank sent, byte for byte, keeps every version of a transaction,
and checks the stored transactions against the balances the bank reports.

Status: early. The design is settled and the build is in progress.

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

See `docs/operations.md` once it exists. In short: a Postgres 18 database, one container, a Monzo
OAuth client of your own, and a reverse proxy that puts single sign-on in front of the status page.

## Development

`just check` is the gate. The tests need the PostgreSQL 18 server binaries on `PATH`.

## Licence

Apache-2.0. See [LICENSE](LICENSE).

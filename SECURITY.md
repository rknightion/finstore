# Security

Report a vulnerability through [private vulnerability
reporting](https://github.com/rknightion/finstore/security/advisories/new), not a public issue.

Never include bank data, tokens or account identifiers in a report. Describe the shape of the
problem; a maintainer will ask for anything more over a private channel.

## What matters most here

- finstore holds an OAuth token for a bank account. With Monzo that token could move money between
  the account holder's own pots, so the code contains no call that writes to a bank and its HTTP
  client refuses any request outside a fixed allow-list of reads.
- Bank payloads live only in the database and its dumps. A change that lets one reach a log, a
  metric or an error message is a vulnerability.

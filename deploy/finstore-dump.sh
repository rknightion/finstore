#!/usr/bin/env bash
# Dumps the finstore database to one file for a file-level backup tool to pick up.
#
# The dump leaves out the contents of the `secret` schema (the bank tokens): a restored token is
# already spent, and a second copy of a live one is a hazard. After a restore, reconnect the bank.
#
# Settings come from /etc/default/finstore-dump (or the file named by FINSTORE_DUMP_CONFIG):
#   FINSTORE_DB_CONTAINER  name of the database container
#   FINSTORE_DUMP_DIR      directory for finstore.dump (created mode 700)
#   FINSTORE_STATUS_DIR    directory for dump.json, which the service reads (created mode 755)
#   FINSTORE_DB_NAME       database name (default finstore)
#   FINSTORE_DB_SUPERUSER  role to dump as (default postgres)
set -euo pipefail
umask 077

config="${FINSTORE_DUMP_CONFIG:-/etc/default/finstore-dump}"
if [[ -r "$config" ]]; then
  # shellcheck source=/dev/null
  . "$config"
fi
: "${FINSTORE_DB_CONTAINER:?set FINSTORE_DB_CONTAINER in $config}"
: "${FINSTORE_DUMP_DIR:?set FINSTORE_DUMP_DIR in $config}"
: "${FINSTORE_STATUS_DIR:?set FINSTORE_STATUS_DIR in $config}"
db="${FINSTORE_DB_NAME:-finstore}"
superuser="${FINSTORE_DB_SUPERUSER:-postgres}"

install -d -m 700 "$FINSTORE_DUMP_DIR"
install -d -m 755 "$FINSTORE_STATUS_DIR"
partial="$FINSTORE_DUMP_DIR/finstore.dump.partial"
final="$FINSTORE_DUMP_DIR/finstore.dump"
trap 'rm -f "$partial"' EXIT

# Uncompressed on purpose: a deduplicating backup tool stores a compressed dump again in full on
# every run.
docker exec "$FINSTORE_DB_CONTAINER" \
  pg_dump -U "$superuser" -d "$db" -Fc -Z0 --exclude-table-data='secret.*' >"$partial"

# A dump that cannot be listed is not a backup. Check it before it replaces the last good one.
docker exec -i "$FINSTORE_DB_CONTAINER" pg_restore --list <"$partial" >/dev/null

mv -f "$partial" "$final"
trap - EXIT

bytes="$(stat -c %s "$final")"
finished="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
status_tmp="$(mktemp "$FINSTORE_STATUS_DIR/.dump.json.XXXXXX")"
printf '{"finished_at":"%s","bytes":%s}\n' "$finished" "$bytes" >"$status_tmp"
chmod 644 "$status_tmp"
mv -f "$status_tmp" "$FINSTORE_STATUS_DIR/dump.json"
echo "finstore dump finished_at=$finished bytes=$bytes"

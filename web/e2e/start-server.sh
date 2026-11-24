#!/usr/bin/env bash
set -eu
# Only clean up resources that this process created. No app, migration or truncate runs on the maintenance DB.
[[ $SL_E2E_DB_NAME =~ ^sl_e2e_[0-9a-f]{20}$ ]]
createdb --maintenance-db="$SL_E2E_MAINTENANCE_URL" --template=template0 "$SL_E2E_DB_NAME"
cleanup() {
  dropdb --maintenance-db="$SL_E2E_MAINTENANCE_URL" --force "$SL_E2E_DB_NAME"
  psql -Xq "$SL_E2E_MAINTENANCE_URL" -v ON_ERROR_STOP=1 -c "DROP ROLE IF EXISTS ${SL_E2E_DB_NAME}_query"
  if [[ ${owns_dir:-} ]]; then rm -rf -- "$SL_E2E_RUN_DIR"; fi
}
trap cleanup EXIT
[[ $(psql -XAt "$SL_DATABASE_URL" -c 'select current_database()') == "$SL_E2E_DB_NAME" ]]
mkdir -m 700 "$SL_E2E_RUN_DIR"
owns_dir=1
"$(dirname "$0")/../../backend/bin/servicelater" &
server=$!
trap 'kill -TERM "$server" 2>/dev/null || true; wait "$server" || true; exit 0' TERM INT
wait "$server"

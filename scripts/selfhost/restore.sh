#!/usr/bin/env bash
# Always restore into a new database; never drop/overwrite the active database.
set -euo pipefail
if [ "$#" -ne 2 ] || [[ ! "$2" =~ ^restore_[a-z0-9_]+$ ]]; then
  echo 'Usage: scripts/selfhost/restore.sh BACKUP.dump restore_NEW_NAME' >&2
  exit 2
fi
test -r "$1"
docker compose exec -T postgres createdb -U neuralmail -- "$2"
docker compose exec -T postgres pg_restore -U neuralmail --exit-on-error --single-transaction -d "$2" < "$1"
echo "Restored into $2. The active database is unchanged."

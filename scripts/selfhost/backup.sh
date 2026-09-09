#!/usr/bin/env bash
# Stop writers before calling this script for a coordinated application snapshot.
set -euo pipefail
if [ "$#" -ne 1 ]; then
  echo 'Usage: scripts/selfhost/backup.sh NEW_BACKUP.dump' >&2
  exit 2
fi
umask 077
# noclobber prevents overwriting a previous backup, including through symlinks.
set -o noclobber
exec 3> "$1"
if ! docker compose exec -T postgres pg_dump -U neuralmail -d neuralmail -Fc >&3; then
  echo 'Backup failed; the output is incomplete and must not be used.' >&2
  exit 1
fi
echo 'PostgreSQL backup complete.'

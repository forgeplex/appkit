#!/usr/bin/env bash
# Run database tests against a disposable local PostgreSQL cluster. No existing
# database, service, Docker daemon, or ambient TEST_DATABASE_URL is used.
# Usage: APPKIT_POSTGRES_BIN=/path/to/postgresql/bin bash scripts/test-db-local.sh
# Optional arguments replace the default `make test-db` command.
set -euo pipefail

# Do not propagate shell startup hooks or recursive make overrides to children.
# The invoking shell may already have read BASH_ENV/ENV before this script began:
# this runner assumes a trusted host shell/tools, not a process sandbox.
unset BASH_ENV ENV
unset MAKEFLAGS MAKEOVERRIDES MFLAGS MAKEFILES GNUMAKEFLAGS

if [[ ${1:-} == --help || ${1:-} == -h ]]; then
  printf '%s\n' 'Usage: APPKIT_POSTGRES_BIN=/path/to/postgresql/bin bash scripts/test-db-local.sh [command ...]'
  printf '%s\n' 'Requires a complete local PostgreSQL installation; creates a private Unix-socket-only cluster.'
  printf '%s\n' 'Default command: make test-db. Existing TEST_DATABASE_URL is ignored. Nothing is installed.'
  exit 0
fi

if [[ $(id -u) == 0 ]]; then
  printf '%s\n' 'PostgreSQL initdb must run as a non-root user.' >&2
  exit 2
fi

appkit_db_bin=${APPKIT_POSTGRES_BIN:-}
if [[ -z $appkit_db_bin ]]; then
  if command -v postgres >/dev/null 2>&1; then
    appkit_db_bin=$(dirname "$(command -v postgres)")
  elif command -v pg_config >/dev/null 2>&1; then
    appkit_db_bin=$(pg_config --bindir)
  fi
fi
for appkit_db_tool in postgres initdb pg_ctl createdb psql; do
  if [[ -z $appkit_db_bin || ! -x $appkit_db_bin/$appkit_db_tool ]]; then
    printf 'Missing PostgreSQL server tool %s; set APPKIT_POSTGRES_BIN to a complete installation.\n' "$appkit_db_tool" >&2
    exit 2
  fi
done
appkit_db_bin=$(cd "$appkit_db_bin" && pwd -P)
appkit_db_repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)

# Ignore client/service settings that might select a shared database or inject
# unrelated connection options. Every command below also specifies its target.
unset PGDATA PGHOST PGHOSTADDR PGPORT PGDATABASE PGUSER PGPASSWORD PGPASSFILE
unset PGSERVICE PGSERVICEFILE PGOPTIONS PGSSLMODE PGTARGETSESSIONATTRS
unset TEST_DATABASE_URL
# macOS locale discovery may start auxiliary threads inside a clean environment;
# PostgreSQL requires single-threaded startup. Keep initialization/server locale
# deterministic even when the caller deliberately uses env -i.
export LC_ALL=C

# /tmp keeps socket paths below PostgreSQL's Unix-domain path length limit.
# mktemp's private directory means trust authentication is not exposed to other
# users, and listen_addresses='' prevents all TCP connections.
appkit_db_root=$(mktemp -d /tmp/appkit-db-test.XXXXXXXX)
appkit_db_data=$appkit_db_root/data
appkit_db_socket=$appkit_db_root/socket
appkit_db_started=0

appkit_db_cleanup() {
  local appkit_db_status=$?
  trap - EXIT INT TERM
  if [[ $appkit_db_started == 1 ]]; then
    if ! "$appkit_db_bin/pg_ctl" -D "$appkit_db_data" -m fast -w -t 20 stop >/dev/null 2>&1; then
      if [[ -f $appkit_db_data/postmaster.pid ]]; then
        printf 'Could not stop disposable PostgreSQL; retained exact cluster at %s.\n' "$appkit_db_root" >&2
        exit 1
      fi
    fi
  fi
  if [[ $appkit_db_status != 0 ]]; then
    for appkit_db_log in "$appkit_db_root/initdb.log" "$appkit_db_root/postgres.log"; do
      if [[ -f $appkit_db_log ]]; then
        printf 'Disposable PostgreSQL diagnostic: %s\n' "$appkit_db_log" >&2
        tail -n 60 "$appkit_db_log" >&2
      fi
    done
  fi
  # Delete only the exact private mktemp directory created by this invocation.
  if [[ $appkit_db_root =~ ^/tmp/appkit-db-test\.[A-Za-z0-9]{8}$ && -d $appkit_db_root && ! -L $appkit_db_root ]]; then
    rm -rf -- "$appkit_db_root"
    printf 'Removed disposable PostgreSQL cluster: %s\n' "$appkit_db_root" >&2
  else
    printf 'Refusing cleanup of unexpected temporary path: %s\n' "$appkit_db_root" >&2
    exit 1
  fi
  exit "$appkit_db_status"
}
trap appkit_db_cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -m 700 "$appkit_db_socket"
"$appkit_db_bin/initdb" -D "$appkit_db_data" -U appkit_test --auth=trust --encoding=UTF8 --locale=C >"$appkit_db_root/initdb.log" 2>&1
# Set before start so a readiness timeout still shuts down the child process.
appkit_db_started=1
"$appkit_db_bin/pg_ctl" -D "$appkit_db_data" -l "$appkit_db_root/postgres.log" -w -t 20 \
  -o "-c listen_addresses='' -c unix_socket_directories='$appkit_db_socket' -c max_connections=100" start
"$appkit_db_bin/createdb" -h "$appkit_db_socket" -U appkit_test appkit_test
export TEST_DATABASE_URL="postgresql:///appkit_test?host=$appkit_db_socket&user=appkit_test&sslmode=disable"
printf 'Isolated test database: %s\n' "$TEST_DATABASE_URL"
"$appkit_db_bin/psql" -X --dbname="$TEST_DATABASE_URL" --no-psqlrc --tuples-only --command='SELECT version();'
cd "$appkit_db_repo"
if [[ $# == 0 ]]; then
  make test-db "TEST_DATABASE_URL=$TEST_DATABASE_URL"
else
  "$@"
fi

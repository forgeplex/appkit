#!/usr/bin/env bash
# Compatibility smoke/DB tests for the four reviewed local consumer projects.
# Copies CURRENT Go/SQL source (including uncommitted files), not Git HEAD.
# Original repositories, config files, credentials and global services are untouched.
# Review downstream tests before using this runner with changed project code:
# environment isolation is not a sandbox for arbitrary test programs.
set -euo pipefail

# Child tools must not inherit startup hooks. The invoking shell may already
# have read them before this point; a trusted host shell is still required.
unset BASH_ENV ENV

if [[ ${1:-} == --help || ${1:-} == -h ]]; then
  printf '%s\n' 'Usage: APPKIT_POSTGRES_BIN=/path/to/postgresql/bin bash scripts/test-downstream-local.sh [email notification rbac ledger]'
  printf '%s\n' 'APPKIT_APPS_ROOT defaults to ../apps. Requires complete PostgreSQL tools, Go, rsync, and cached Go dependencies.'
  printf '%s\n' 'Builds current-source copies against this AppKit checkout, then runs -race tests in disposable Unix-socket-only databases.'
  printf '%s\n' 'Failure artifacts are retained; APPKIT_DOWNSTREAM_KEEP=1 also retains successful copies. No business files are edited.'
  exit 0
fi

appkit_ds_repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
appkit_ds_apps=$(cd "${APPKIT_APPS_ROOT:-$appkit_ds_repo/../apps}" && pwd -P)
if [[ $# == 0 ]]; then
  set -- email notification rbac ledger
fi
for appkit_ds_name in "$@"; do
  case $appkit_ds_name in
    email|notification|rbac|ledger) ;;
    *) printf 'Unknown reviewed consumer: %s\n' "$appkit_ds_name" >&2; exit 2 ;;
  esac
  test -f "$appkit_ds_apps/$appkit_ds_name/go.mod" || { printf 'Missing consumer go.mod: %s\n' "$appkit_ds_name" >&2; exit 2; }
done
command -v rsync >/dev/null
appkit_ds_go=$(command -v go)
appkit_ds_cache=$(go env GOCACHE)
appkit_ds_modcache=$(go env GOMODCACHE)
appkit_ds_gopath=$(go env GOPATH)
appkit_ds_env=(
  "PATH=$(dirname "$appkit_ds_go"):/usr/bin:/bin:/usr/sbin:/sbin"
  "GOCACHE=$appkit_ds_cache" "GOPATH=$appkit_ds_gopath" "GOMODCACHE=$appkit_ds_modcache"
  "GOENV=off" "GOWORK=off" "GOTOOLCHAIN=local"
  "LC_ALL=C"
  "GOPROXY=file://$appkit_ds_modcache/cache/download"
)
appkit_ds_root=$(mktemp -d /tmp/appkit-downstream.XXXXXXXX)
appkit_ds_cleanup() {
  local appkit_ds_status=$?
  trap - EXIT INT TERM
  if [[ $appkit_ds_status != 0 || ${APPKIT_DOWNSTREAM_KEEP:-} == 1 ]]; then
    printf 'Retained downstream source copies and test logs: %s\n' "$appkit_ds_root" >&2
  elif [[ $appkit_ds_root =~ ^/tmp/appkit-downstream\.[A-Za-z0-9]{8}$ && -d $appkit_ds_root && ! -L $appkit_ds_root ]]; then
    rm -rf -- "$appkit_ds_root"
    printf 'Removed disposable downstream source copies: %s\n' "$appkit_ds_root" >&2
  else
    printf 'Refusing cleanup of unexpected path: %s\n' "$appkit_ds_root" >&2
    exit 1
  fi
  exit "$appkit_ds_status"
}
trap appkit_ds_cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

appkit_ds_failed=0
for appkit_ds_name in "$@"; do
  appkit_ds_copy=$appkit_ds_root/$appkit_ds_name
  printf 'Consumer %s: testing CURRENT Go/SQL sources against %s\n' "$appkit_ds_name" "$appkit_ds_repo"
  # Only these source kinds are required by the reviewed consumers. Do not
  # copy symlinks, .git, ambient go.work, environment files, config or secrets.
  rsync -r --prune-empty-dirs --exclude='.git/' --exclude='node_modules/' --exclude='vendor/' \
    --exclude='config/' --exclude='.env*' --include='*/' --include='*.go' --include='*.sql' \
    --include='/go.mod' --include='/go.sum' --include='/AGENTS.md' --exclude='*' \
    "$appkit_ds_apps/$appkit_ds_name/" "$appkit_ds_copy/"
  if ! (
    cd "$appkit_ds_copy" || exit 1
    env -i "${appkit_ds_env[@]}" go mod edit "-replace=github.com/forgeplex/appkit=$appkit_ds_repo" &&
    env -i "${appkit_ds_env[@]}" go build -mod=mod ./...
  ); then
    printf 'Consumer %s: BUILD FAILED\n' "$appkit_ds_name" >&2
    appkit_ds_failed=1
    continue
  fi
  printf 'Consumer %s: BUILD PASS\n' "$appkit_ds_name"
  appkit_ds_log=$appkit_ds_root/$appkit_ds_name.test.jsonl
  if env -i "${appkit_ds_env[@]}" "APPKIT_POSTGRES_BIN=${APPKIT_POSTGRES_BIN:-}" \
    bash "$appkit_ds_repo/scripts/test-db-local.sh" bash -c '
      cd "$1" || exit 1
      shift
      exec env -i "$@" "TEST_DATABASE_URL=$TEST_DATABASE_URL" go test -mod=mod -race -count=1 -json ./...
    ' _ "$appkit_ds_copy" "${appkit_ds_env[@]}" >"$appkit_ds_log" 2>"$appkit_ds_root/$appkit_ds_name.stderr.log"; then
    printf 'Consumer %s: TEST PASS\n' "$appkit_ds_name"
  else
    printf 'Consumer %s: TEST FAILED (see %s)\n' "$appkit_ds_name" "$appkit_ds_log" >&2
    appkit_ds_failed=1
  fi
  # Go's JSON event format is stable; count test/subtest events separately from
  # no-test-file package skips. Detailed test output remains in the retained log.
  if ! awk '
    /"Action":"(pass|fail|skip)"/ && /"Test":/ {
      if (/"Action":"pass"/) passed++
      if (/"Action":"skip"/) { skipped++; print }
      if (/"Action":"fail"/) { failed++; print }
    }
    END {
      printf "Test/subtest events: pass=%d skip=%d fail=%d\n", passed, skipped, failed
      if (passed == 0) exit 1
    }
  ' "$appkit_ds_log"; then
    printf 'Consumer %s: no passing tests were observed\n' "$appkit_ds_name" >&2
    appkit_ds_failed=1
  fi
done
exit "$appkit_ds_failed"

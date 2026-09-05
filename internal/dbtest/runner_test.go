package dbtest_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The runner's resource ownership and exit handling can be checked without a
// database. Real PostgreSQL behavior remains covered by make test-db.
func TestLocalRunnerUsesPrivateDatabaseAndCleansUp(t *testing.T) {
	requireLocalRunnerHost(t)
	bin := fakePostgresTools(t)
	for _, status := range []string{"0", "17"} {
		t.Run("exit-"+status, func(t *testing.T) {
			cmd := exec.Command("bash", localRunner(t), "sh", "-c", `
case "$TEST_DATABASE_URL" in
  postgresql:///appkit_test?host=/tmp/appkit-db-test.*) ;;
  *) exit 91 ;;
esac
test -z "${PGHOST:-}" || exit 92
test -z "${PGSERVICE:-}" || exit 93
test "${LC_ALL:-}" = C || exit 94
exit "$APPKIT_FAKE_COMMAND_EXIT"
`)
			cmd.Env = append(os.Environ(),
				"APPKIT_POSTGRES_BIN="+bin,
				"APPKIT_FAKE_COMMAND_EXIT="+status,
				"TEST_DATABASE_URL=postgresql://shared.invalid/do-not-use",
				"PGHOST=shared.invalid", "PGSERVICE=do-not-use", "LC_ALL=POSIX")
			out, err := cmd.CombinedOutput()
			if status == "0" && err != nil {
				t.Fatalf("runner: %v\n%s", err, out)
			}
			if status == "17" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 17 {
					t.Fatalf("runner should preserve command exit 17: %v\n%s", err, out)
				}
			}
			var cleaned string
			for _, line := range strings.Split(string(out), "\n") {
				if name, ok := strings.CutPrefix(line, "Removed disposable PostgreSQL cluster: "); ok {
					cleaned = name
				}
			}
			if !strings.HasPrefix(cleaned, "/tmp/appkit-db-test.") {
				t.Fatalf("no cleanup confirmation: %s", out)
			}
			if _, err := os.Stat(cleaned); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("private cluster was not removed: %s: %v", cleaned, err)
			}
			if strings.Contains(string(out), "shared.invalid") {
				t.Fatalf("ambient database address reached runner output: %s", out)
			}
		})
	}
}

func TestLocalRunnerRejectsIncompleteServerTools(t *testing.T) {
	requireLocalRunnerHost(t)
	cmd := exec.Command("bash", localRunner(t))
	cmd.Env = append(os.Environ(), "APPKIT_POSTGRES_BIN="+t.TempDir())
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(out), "Missing PostgreSQL server tool") {
		t.Fatalf("incomplete tools should fail before creating a cluster: %v\n%s", err, out)
	}
}

// Use the real project Makefile and a real recursive make invocation: inspecting
// the runner's environment alone misses make's command-line variable precedence.
func TestLocalRunnerDefaultMakeCannotReuseAmbientDatabase(t *testing.T) {
	requireLocalRunnerHost(t)
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make is required for the recursive make regression")
	}
	for _, mode := range []string{"recursive-command-line", "inherited-make-environment"} {
		t.Run(mode, func(t *testing.T) {
			repo := t.TempDir()
			if err := os.Mkdir(filepath.Join(repo, "scripts"), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"Makefile", "scripts/test-db-local.sh"} {
				data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(name)))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(name)), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			bin := fakePostgresTools(t)
			log := filepath.Join(repo, "go-database.log")
			const fakeGo = `#!/usr/bin/env bash
set -eu
test "$*" = 'test -race -count=1 ./...' || exit 90
case "$TEST_DATABASE_URL" in
  postgresql:///appkit_test?host=/tmp/appkit-db-test.*\&user=appkit_test\&sslmode=disable) ;;
  *) exit 91 ;;
esac
test -z "${BASH_ENV:-}${ENV:-}${MAKEFILES:-}" || exit 92
printf '%s\n' "$TEST_DATABASE_URL" > "$APPKIT_FAKE_GO_LOG"
`
			if err := os.WriteFile(filepath.Join(bin, "go"), []byte(fakeGo), 0o700); err != nil {
				t.Fatal(err)
			}
			env := append(os.Environ(), "APPKIT_POSTGRES_BIN="+bin,
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GO=go", "APPKIT_FAKE_GO_LOG="+log,
				"TEST_DATABASE_URL=postgresql://shared.invalid/do-not-use",
				"MAKEFLAGS=", "MAKEOVERRIDES=", "MFLAGS=", "MAKEFILES=", "GNUMAKEFLAGS=",
				"BASH_ENV=", "ENV=")
			var cmd *exec.Cmd
			if mode == "recursive-command-line" {
				cmd = exec.Command(makeBin, "test-db-local", "TEST_DATABASE_URL=postgresql://shared.invalid/do-not-use")
			} else {
				injected := filepath.Join(repo, "ambient.mk")
				if err := os.WriteFile(injected, []byte("$(error ambient MAKEFILES reached inner make)\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				startup := filepath.Join(repo, "startup.sh")
				if err := os.WriteFile(startup, []byte("printf 'startup\\n' >> \"$APPKIT_FAKE_STARTUP_LOG\"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				env = append(env,
					"MAKEFLAGS= -- TEST_DATABASE_URL=postgresql://shared.invalid/makeflags",
					"MAKEOVERRIDES=TEST_DATABASE_URL=postgresql://shared.invalid/overrides",
					"MFLAGS=-n", "GNUMAKEFLAGS=-n", "MAKEFILES="+injected,
					"BASH_ENV="+startup, "ENV="+startup,
					"APPKIT_FAKE_STARTUP_LOG="+filepath.Join(repo, "startup.log"))
				cmd = exec.Command("bash", "scripts/test-db-local.sh")
			}
			cmd.Dir, cmd.Env = repo, env
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("default runner must use disposable database through real make: %v\n%s", err, out)
			}
			data, err := os.ReadFile(log)
			if err != nil || !strings.HasPrefix(string(data), "postgresql:///appkit_test?host=/tmp/appkit-db-test.") {
				t.Fatalf("real make did not execute fake go with the private DSN: %q, %v\n%s", data, err, out)
			}
			if mode == "inherited-make-environment" {
				data, err := os.ReadFile(filepath.Join(repo, "startup.log"))
				if err != nil || string(data) != "startup\n" {
					t.Fatalf("startup hooks must not propagate beyond the invoking shell: %q, %v", data, err)
				}
			}
		})
	}
}

func TestDownstreamRunnerStopsWhenCopyDirectoryIsUnavailable(t *testing.T) {
	requireLocalRunnerHost(t)
	root := t.TempDir()
	apps := filepath.Join(root, "apps")
	if err := os.MkdirAll(filepath.Join(apps, "email"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(apps, "email", "go.mod"), []byte("module example.com/email\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// A successful copy command followed by a missing destination simulates a
	// directory disappearing before cd, without modifying any real consumer.
	if err := os.WriteFile(filepath.Join(bin, "rsync"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	const fakeGo = `#!/bin/sh
if [ "$1" = env ]; then
  printf '/tmp/appkit-fake-cache\n'
  exit 0
fi
printf 'UNEXPECTED GO MUTATION: %s\n' "$*" >&2
exit 93
`
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(fakeGo), 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(filepath.Dir(localRunner(t)), "test-downstream-local.sh")
	cmd := exec.Command("bash", script, "email")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "APPKIT_APPS_ROOT="+apps,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "APPKIT_DOWNSTREAM_KEEP=0", "BASH_ENV=", "ENV=")
	out, err := cmd.CombinedOutput()
	// Failed downstream runs intentionally retain their private copy directories.
	for _, line := range strings.Split(string(out), "\n") {
		if dir, ok := strings.CutPrefix(line, "Retained downstream source copies and test logs: "); ok {
			if !strings.HasPrefix(dir, "/tmp/appkit-downstream.") || filepath.Dir(dir) != "/tmp" {
				t.Fatalf("unexpected retained directory %q", dir)
			}
			t.Cleanup(func() {
				if err := os.Remove(dir); err != nil {
					t.Errorf("remove empty test-owned downstream directory: %v", err)
				}
			})
		}
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(out), "BUILD FAILED") {
		t.Fatalf("missing copy should fail: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "UNEXPECTED GO MUTATION") {
		t.Fatalf("failed cd allowed go to run in the caller's directory:\n%s", out)
	}
}

func requireLocalRunnerHost(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("local PostgreSQL runner requires a non-root Unix user")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required for the local PostgreSQL runner")
	}
}

func localRunner(t *testing.T) string {
	t.Helper()
	name, err := filepath.Abs(filepath.Join("..", "..", "scripts", "test-db-local.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func fakePostgresTools(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	const tool = `#!/usr/bin/env bash
set -eu
mode=$(basename "$0")
data=''
operation=''
while [[ $# -gt 0 ]]; do
  case "$1" in
    -D) data=$2; shift 2 ;;
    start|stop) operation=$1; shift ;;
    *) shift ;;
  esac
done
if [[ $mode == initdb ]]; then
  mkdir -p "$data"
elif [[ $mode == pg_ctl ]]; then
  if [[ $operation == start ]]; then
    touch "$data/postmaster.pid"
  elif [[ $operation == stop ]]; then
    rm "$data/postmaster.pid"
  fi
fi
`
	for _, name := range []string{"postgres", "initdb", "pg_ctl", "createdb", "psql"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(tool), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

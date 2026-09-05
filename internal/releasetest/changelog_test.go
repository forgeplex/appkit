package releasetest

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Exercise the real release recipe, not a rewritten shell fragment. In
// particular, older Bash versions can consume the first UTF-8 byte immediately
// following an unbraced variable expansion and silently corrupt tag headings.
func TestMakeChangelogAcrossShells(t *testing.T) {
	makeBinary, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make is required for the release recipe regression")
	}
	gitBinary, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is required for annotated tag fixtures")
	}
	makefile, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, shellName := range []string{"sh", "bash", "dash", "zsh"} {
		t.Run(shellName, func(t *testing.T) {
			shell, err := exec.LookPath(shellName)
			if err != nil {
				t.Skipf("optional shell %s is unavailable", shellName)
			}
			for _, locale := range []string{"C", "C.UTF-8"} {
				t.Run(locale, func(t *testing.T) {
					dir := t.TempDir()
					if err := os.WriteFile(filepath.Join(dir, "Makefile"), makefile, 0o600); err != nil {
						t.Fatal(err)
					}
					env := releaseEnvironment(locale)
					git := func(date, input string, args ...string) {
						t.Helper()
						gitEnv := append(append([]string(nil), env...), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
						runReleaseCommand(t, dir, gitEnv, input, gitBinary, args...)
					}
					const oldDate = "2026-08-01T12:00:00+00:00"
					const newDate = "2026-09-05T12:00:00+00:00"
					const oldMessage = "旧版发布\n\n保留兼容接口。\n"
					const newMessage = "新版发布\n\n修复中文标题；完成度 100%。\n保留字面量 \\n 和 `代码`。\n"
					git(oldDate, "", "init", "-q")
					git(oldDate, "", "commit", "--allow-empty", "--no-gpg-sign", "-qm", "old fixture")
					git(oldDate, oldMessage, "tag", "-a", "v0.9.0", "-F", "-")
					git(oldDate, "", "tag", "lint/v0.9.0")
					git(newDate, "", "commit", "--allow-empty", "--no-gpg-sign", "-qm", "new fixture")
					git(newDate, newMessage, "tag", "-a", "v0.10.0", "-F", "-")
					git(newDate, "lint-only annotation must not appear\n", "tag", "-a", "lint/v0.10.0", "-F", "-")

					generate := func() []byte {
						t.Helper()
						runReleaseCommand(t, dir, env, "", makeBinary, "-s", "changelog", "SHELL="+shell)
						content, err := os.ReadFile(filepath.Join(dir, "CHANGELOG.md"))
						if err != nil {
							t.Fatal(err)
						}
						return content
					}
					first := generate()
					if !utf8.Valid(first) {
						t.Fatalf("changelog contains invalid UTF-8: %q", first)
					}
					content := string(first)
					wantVersions := "## v0.10.0（2026-09-05）\n\n" + newMessage + "\n" +
						"## v0.9.0（2026-08-01）\n\n" + oldMessage + "\n"
					if !strings.HasPrefix(content, "# Changelog\n\n按版本倒序；") || !strings.HasSuffix(content, wantVersions) {
						t.Fatalf("release headings, dates, tag bodies or version ordering changed:\n%s\nwant suffix:\n%s", content, wantVersions)
					}
					if strings.Count(content, "\n## ") != 2 || strings.Contains(content, "lint/") || strings.Contains(content, "lint-only") {
						t.Fatalf("changelog included a non-release tag:\n%s", content)
					}
					if second := generate(); !bytes.Equal(first, second) {
						t.Fatalf("repeated generation is not byte-identical:\nfirst: %q\nsecond: %q", first, second)
					}
				})
			}
		})
	}
}

func releaseEnvironment(locale string) []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") || strings.HasPrefix(key, "LC_") {
			continue
		}
		switch key {
		case "LANG", "TZ", "MAKEFLAGS", "MAKEOVERRIDES", "MFLAGS", "MAKEFILES", "GNUMAKEFLAGS", "SHELL", "BASH_ENV", "ENV":
			continue
		}
		env = append(env, entry)
	}
	return append(env, "LC_ALL="+locale, "LANG="+locale, "TZ=UTC", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=AppKit Release Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=AppKit Release Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
}

func runReleaseCommand(t *testing.T, dir string, env []string, input, command string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(input)
	cmd.WaitDelay = time.Second
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v failed: %v\n%s", command, args, err, output)
	}
}

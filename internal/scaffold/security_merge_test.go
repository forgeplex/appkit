package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const scaffoldTestWorkflowRef = "0123456789abcdef0123456789abcdef01234567"

func TestRenderDomainRequiresExplicitPinnedWorkflow(t *testing.T) {
	// With no git/go executable available, rendering must still succeed when
	// supplied a pin, and must reject missing/mutable refs without resolving one.
	t.Setenv("PATH", t.TempDir())
	for _, ref := range []string{"", "main", "v0.7.3", strings.Repeat("a", 39), scaffoldTestWorkflowRef} {
		t.Run(ref, func(t *testing.T) {
			parent := t.TempDir()
			opts := Options{Name: "sample", AppkitVersion: "v99.99.99", WorkflowRef: ref, Dir: filepath.Join(parent, "output")}
			files, err := RenderDomain(opts)
			if ref == scaffoldTestWorkflowRef {
				if err != nil {
					t.Fatal(err)
				}
				mustContain(t, ".github/workflows/ci.yml", string(files[".github/workflows/ci.yml"]), "domain-ci.yml@"+ref)
			} else if err == nil {
				t.Fatalf("unpinned workflow ref %q accepted", ref)
			}
			if entries, err := os.ReadDir(parent); err != nil || len(entries) != 0 {
				t.Fatalf("renderer wrote output: %v %v", entries, err)
			}
		})
	}
}

func TestScaffoldMakefileLintVersionFailClosed(t *testing.T) {
	makeBinary, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make unavailable")
	}
	dir := t.TempDir()
	if err := Domain(Options{Name: "sample", Dir: dir, WorkflowRef: scaffoldTestWorkflowRef}, nil); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(t.TempDir(), "probe.mk")
	if err := os.WriteFile(probe, []byte(".PHONY: probe-lint-version\nprobe-lint-version:\n\t@printf '%s\\n' '$(APPKITLINT_VERSION)'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, listing, override, want string
	}{
		{"released", "github.com/forgeplex/appkit v0.7.3", "", "v0.7.3"},
		{"workspace", "github.com/forgeplex/appkit", "", ""},
		{"explicit", "github.com/forgeplex/appkit", "v0.7.4", "v0.7.4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeGo := filepath.Join(t.TempDir(), "go")
			if err := os.WriteFile(fakeGo, []byte("#!/bin/sh\nprintf '%s\\n' '"+tc.listing+"'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			args := []string{"-s", "-f", "Makefile", "-f", probe, "probe-lint-version", "GO=" + fakeGo}
			if tc.override != "" {
				args = append(args, "APPKITLINT_VERSION="+tc.override)
			}
			cmd := exec.CommandContext(t.Context(), makeBinary, args...)
			cmd.Dir = dir
			cmd.Env = cleanScaffoldMakeEnv()
			output, err := cmd.CombinedOutput()
			if err != nil || strings.TrimSpace(string(output)) != tc.want {
				t.Fatalf("make lint version: %v %q; want %q", err, output, tc.want)
			}
			if tc.want == "" {
				cmd := exec.CommandContext(t.Context(), makeBinary, "-s", "lint", "GO="+fakeGo, "GOLANGCI=false", "ARCHLINT=false")
				cmd.Dir = dir
				cmd.Env = cleanScaffoldMakeEnv()
				output, err := cmd.CombinedOutput()
				if err == nil || !strings.Contains(string(output), "APPKITLINT_VERSION 为空") {
					t.Fatalf("missing lint version did not fail closed before linters: %v %s", err, output)
				}
			}
		})
	}
}

func cleanScaffoldMakeEnv() []string {
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		switch key {
		case "APPKITLINT_VERSION", "MAKEFLAGS", "MFLAGS", "MAKEFILES", "GNUMAKEFLAGS":
		default:
			env = append(env, item)
		}
	}
	return env
}

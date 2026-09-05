package scaffold

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Real make and real go run resolve a pinned CLI from a local file proxy. The
// synthetic tool has an extra module dependency absent from the consumer's
// go.mod/go.sum: -mod=readonly must work without editing those business files.
// No published module, network, Docker, or mutable tool reference is used.
func TestScaffoldToolsUseIndependentPinnedModule(t *testing.T) {
	makeBinary, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make required")
	}
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	proxy := t.TempDir()
	writeToolProxyModule(t, proxy, "example.test/tooldep", "v0.0.1", map[string]string{
		"go.mod": "module example.test/tooldep\n\ngo 1.26.0\n",
		"dep.go": "package tooldep\nconst Value = \"extra-tool-dependency\"\n",
	})
	for _, tool := range []struct{ module, command string }{
		{"github.com/forgeplex/appkit", "appkit"}, {"github.com/forgeplex/appkit/lint", "appkit-lint"},
	} {
		writeToolProxyModule(t, proxy, tool.module, "v0.0.1", map[string]string{
			"go.mod": "module " + tool.module + "\n\ngo 1.26.0\nrequire example.test/tooldep v0.0.1\n",
			"cmd/" + tool.command + "/main.go": fmt.Sprintf(`package main
import ("fmt"; "os"; "example.test/tooldep")
func main() { fmt.Printf("tool=%s mode=%%s dependency=%%s GOWORK=%%s GOFLAGS=%%s\n", os.Args[1], tooldep.Value, os.Getenv("GOWORK"), os.Getenv("GOFLAGS")) }
`, tool.command),
		})
	}
	cache := t.TempDir()
	// Go makes module-cache directories read-only. Restore permissions only
	// inside this test-owned cache before testing.TempDir removes it.
	t.Cleanup(func() {
		if err := filepath.WalkDir(cache, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return os.Chmod(path, 0o700)
			}
			return os.Chmod(path, 0o600)
		}); err != nil {
			t.Errorf("restore private module-cache permissions: %v", err)
		}
	})
	for _, kind := range []string{"domain", "system"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			options := Options{Name: "sample", AppkitVersion: "v0.0.1", WorkflowRef: scaffoldTestWorkflowRef}
			render := RenderDomain
			if kind == "system" {
				render = RenderSystem
			}
			files, err := render(options)
			if err != nil {
				t.Fatal(err)
			}
			writeToolFixture(t, filepath.Join(dir, "Makefile"), files["Makefile"])
			mod := []byte("module example.test/consumer\n\ngo 1.26.0\nrequire github.com/forgeplex/appkit v0.0.1\n")
			writeToolFixture(t, filepath.Join(dir, "go.mod"), mod)
			writeToolFixture(t, filepath.Join(dir, "go.sum"), nil)
			// A malformed ambient workspace must not affect fixed tool lookup or
			// execution. Business run/test must still receive this value unchanged.
			work := filepath.Join(t.TempDir(), "go.work")
			writeToolFixture(t, work, []byte("not a valid Go workspace\n"))
			env := toolMakeEnvironment(map[string]string{
				"GOWORK": work, "GOFLAGS": "-mod=readonly", "GOENV": "off", "GOTOOLCHAIN": "local",
				"GOPROXY": (&url.URL{Scheme: "file", Path: filepath.ToSlash(proxy)}).String(),
				"GOSUMDB": "off", "GOPRIVATE": "", "GONOPROXY": "none", "GOVCS": "*:off", "GOMODCACHE": cache,
			})
			modes := []string{"check"}
			if kind == "domain" {
				modes = append(modes, "schema", "lint")
				writeToolFixture(t, filepath.Join(dir, "no-docker.mk"), []byte("dev-db:\n\t@:\n"))
			} else {
				modes = append(modes, "dev")
			}
			for _, mode := range modes {
				if mode == "lint" {
					// Cache the nested module's declared identity explicitly: this
					// test exercises recipe isolation, not proxy prefix discovery.
					// Its tool zip and extra dependency still come from the proxy.
					out, err := runToolMake(t, goBinary, dir, append(env, "GOWORK=off"), "list", "-m", "-json", "github.com/forgeplex/appkit/lint@v0.0.1")
					if err != nil || !strings.Contains(out, `"Path": "github.com/forgeplex/appkit/lint"`) {
						t.Fatalf("prepare nested tool module metadata: %v\n%s", err, out)
					}
				}
				args := []string{"-s", "-f", "Makefile"}
				if kind == "domain" {
					args = append(args, "-f", "no-docker.mk")
				}
				args = append(args, mode, "GO="+goBinary, "GOLANGCI=true", "ARCHLINT=true")
				output, err := runToolMake(t, makeBinary, dir, env, args...)
				wantMode, wantTool := mode, "appkit"
				if mode == "lint" {
					wantMode, wantTool = "./...", "appkit-lint"
				}
				want := "tool=" + wantTool + " mode=" + wantMode + " dependency=extra-tool-dependency GOWORK=off GOFLAGS=-mod=readonly"
				if err != nil || !strings.Contains(output, want) {
					t.Fatalf("make %s did not isolate pinned tool: %v\n%s", mode, err, output)
				}
			}
			for _, version := range []string{"", "main", "v1.bad.3main"} {
				output, err := runToolMake(t, makeBinary, dir, env, "-s", "check", "GO="+goBinary, "APPKIT_VERSION="+version)
				if err == nil || strings.Contains(output, "tool=appkit") {
					t.Fatalf("mutable/missing version %q reached tool: %v\n%s", version, err, output)
				}
			}
			if kind == "domain" {
				if out, err := runToolMake(t, makeBinary, dir, env, "-s", "lint", "GO="+goBinary, "APPKITLINT_VERSION=v0.0.2", "GOLANGCI=true", "ARCHLINT=true"); err == nil || !strings.Contains(out, "必须与 APPKIT_VERSION 相同") {
					t.Fatalf("different lint version not rejected: %v\n%s", err, out)
				}
			}
			// A deliberately supplied local binary remains usable for unpublished
			// source development. No implicit fallback is inferred from go.work.
			if out, err := runToolMake(t, makeBinary, dir, env, "-s", "check", "GO="+goBinary, "APPKIT_VERSION=", "APPKIT=echo explicit-local-tool"); err != nil || !strings.Contains(out, "explicit-local-tool") {
				t.Fatalf("explicit local tool override failed: %v\n%s", err, out)
			}
			fakeGo := filepath.Join(t.TempDir(), "go")
			writeToolFixture(t, fakeGo, []byte("#!/bin/sh\nprintf 'business %s GOWORK=%s GOFLAGS=%s\\n' \"$*\" \"$GOWORK\" \"$GOFLAGS\"\n"))
			if err := os.Chmod(fakeGo, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{"run", "test"} {
				out, err := runToolMake(t, makeBinary, dir, env, "-s", mode, "GO="+fakeGo)
				if err != nil || !strings.Contains(out, "GOWORK="+work+" GOFLAGS=-mod=readonly") {
					t.Fatalf("business %s environment changed: %v\n%s", mode, err, out)
				}
			}
			for name, want := range map[string][]byte{"go.mod": mod, "go.sum": nil} {
				got, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("tool modified business %s: %v\n%s", name, err, got)
				}
			}
		})
	}
}

func TestSystemMakeDevWithExplicitUnreleasedTool(t *testing.T) {
	makeBinary, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make required")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "sample")
	if err := System(Options{Name: "sample", Dir: dir}, nil); err != nil {
		t.Fatal(err)
	}
	writeToolFixture(t, filepath.Join(parent, "peer", "go.mod"), []byte("module example.test/peer\n\ngo 1.26.0\n"))
	if err := os.Symlink(appkitRoot(t), filepath.Join(parent, "appkit")); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(t.TempDir(), "appkit")
	runSchemaGuidanceCommand(t, appkitRoot(t), "off", "go", "build", "-buildvcs=false", "-o", cli, "./cmd/appkit")
	env := toolMakeEnvironment(map[string]string{"GOWORK": "off", "GOFLAGS": "-mod=readonly", "GOPROXY": "off", "GOSUMDB": "off", "GOTOOLCHAIN": "local"})
	if out, err := runToolMake(t, makeBinary, dir, env, "-s", "dev", "APPKIT="+cli); err != nil {
		t.Fatalf("explicit source CLI could not create the workspace: %v\n%s", err, out)
	}
	work, err := os.ReadFile(filepath.Join(dir, "go.work"))
	if err != nil || !strings.Contains(string(work), "/appkit\n") || !strings.Contains(string(work), "/peer\n") {
		t.Fatalf("make dev did not discover real local modules: %v\n%s", err, work)
	}
}

func writeToolProxyModule(t *testing.T, root, module, version string, files map[string]string) {
	t.Helper()
	base := filepath.Join(root, filepath.FromSlash(module), "@v")
	writeToolFixture(t, filepath.Join(base, "list"), []byte(version+"\n"))
	writeToolFixture(t, filepath.Join(base, version+".mod"), []byte(files["go.mod"]))
	writeToolFixture(t, filepath.Join(base, version+".info"), []byte(fmt.Sprintf(`{"Version":%q,"Time":"2026-09-05T00:00:00Z"}`, version)))
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	for name, body := range files {
		entry, err := zw.Create(module + "@" + version + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	writeToolFixture(t, filepath.Join(base, version+".zip"), archive.Bytes())
}

func writeToolFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func toolMakeEnvironment(overrides map[string]string) []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; replaced {
			continue
		}
		switch key {
		case "APPKIT", "APPKIT_VERSION", "APPKITLINT_VERSION", "MAKEFLAGS", "MAKEOVERRIDES", "MFLAGS", "MAKEFILES", "GNUMAKEFLAGS", "BASH_ENV", "ENV":
			continue
		}
		env = append(env, entry)
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func runToolMake(t *testing.T, command, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir, cmd.Env, cmd.WaitDelay = dir, env, time.Second
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// Package acceptance exercises actual downstream Go modules against this checkout.
// It proves structural compiler compatibility and the explicitly asserted runtime
// fixture behavior, not universal wire/semantic compatibility or release delivery.
// Upgrades regenerate the candidate contract/server and keep prior consumer Go
// source with keyed DTO literals; mixed-version rolling wire compatibility is not
// established by this test.
package acceptance

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/forgeplex/appkit/internal/gen"
)

//go:embed testdata/*.tmpl
var fixtures embed.FS

// TestCrossProjectReuseAndUpgrade uses one generated contract module, one shared
// implementation module and two independent consumers. Only temporary modules
// are written. Module resolution is local/cached; external networking is disabled
// in the child Go commands (the runtime fixture uses real loopback HTTP).
func TestCrossProjectReuseAndUpgrade(t *testing.T) {
	w := newWorkspace(t)
	w.generateContract(t, contractShape{})
	baseContract := filepath.Join(t.TempDir(), "base.contract.yaml")
	write(t, baseContract, read(t, filepath.Join(w.contracts, "contract.yaml")))
	w.implementation(t, 1)
	w.configureConsumers(t, "v0.1.0", "v0.1.0")
	sources := w.consumerSources(t)

	if !t.Run("baseline", func(t *testing.T) {
		if err := gen.CheckContractCompatibility(baseContract, filepath.Join(w.contracts, "contract.yaml")); err != nil {
			t.Fatal(err)
		}
		w.checkSharedModule(t, "v0.1.0")
		w.testConsumers(t, 1)
	}) {
		t.Fatal("baseline must pass before upgrade acceptance")
	}
	if !t.Run("compatible_implementation_upgrade", func(t *testing.T) {
		w.implementation(t, 2)
		w.configureConsumers(t, "v0.1.1", "v0.1.0")
		w.checkSharedModule(t, "v0.1.1")
		w.testConsumers(t, 2)
	}) {
		t.Fatal("implementation upgrade must pass before contract acceptance")
	}
	if !t.Run("additive_contract_upgrade", func(t *testing.T) {
		// Adding an optional request field, response field and standalone DTO
		// preserves this fixture's keyed literals and existing service methods.
		w.implementation(t, 2)
		w.generateContract(t, contractShape{Additive: true})
		if err := gen.CheckContractCompatibility(baseContract, filepath.Join(w.contracts, "contract.yaml")); err != nil {
			t.Fatalf("additive fixture rejected by contract checker: %v", err)
		}
		w.configureConsumers(t, "v0.1.1", "v0.1.1")
		w.testConsumers(t, 2)
	}) {
		t.Fatal("additive upgrade must pass before negative compiler checks")
	}
	t.Run("removed_contract_type_rejected_by_both_consumers", func(t *testing.T) {
		w.generateContract(t, contractShape{Additive: true, RemoveMarker: true})
		assertContractIssue(t, gen.CheckContractCompatibility(baseContract, filepath.Join(w.contracts, "contract.yaml")),
			"types.Marker", "type_removed")
		w.configureConsumers(t, "v0.1.1", "v0.2.0")
		w.rejectConsumers(t, "undefined: sample.Marker")
	})
	t.Run("changed_method_signature_rejected_by_both_builds", func(t *testing.T) {
		w.generateContract(t, contractShape{Additive: true, BreakSignature: true})
		assertContractIssue(t, gen.CheckContractCompatibility(baseContract, filepath.Join(w.contracts, "contract.yaml")),
			"methods.Ping.request", "method_signature_changed")
		w.configureConsumers(t, "v0.1.1", "v0.2.0")
		// The shared implementation no longer satisfies generated Service.
		// Both complete consumer build graphs must fail on that actual mismatch.
		w.rejectConsumers(t, "wrong type for method Ping")
	})
	t.Run("additional_contract_model_breaks_rejected", func(t *testing.T) {
		// These are conservative model checks, not claims that compilation alone
		// establishes wire compatibility (a route change may compile perfectly).
		base := string(read(t, baseContract))
		for _, tc := range []struct{ name, old, next, location, reason string }{
			{"path", "path: /echo", "path: /echo-v2", "methods.Echo.path", "path_changed"},
			{"field_type", "{name: payload, type: string}", "{name: payload, type: int64}", "methods.Echo.request.payload", "field_type_changed:string->int64"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				candidate := filepath.Join(t.TempDir(), "candidate.contract.yaml")
				write(t, candidate, []byte(strings.Replace(base, tc.old, tc.next, 1)))
				assertContractIssue(t, gen.CheckContractCompatibility(baseContract, candidate), tc.location, tc.reason)
			})
		}
	})
	for path, before := range sources {
		if after := read(t, path); !bytes.Equal(before, after) {
			t.Errorf("consumer Go source was edited during upgrade: %s", path)
		}
	}
}

func assertContractIssue(t *testing.T, err error, location, reason string) {
	t.Helper()
	var incompatible *gen.ContractCompatibilityError
	if !errors.Is(err, gen.ErrContractIncompatible) || !errors.As(err, &incompatible) {
		t.Fatalf("want a compatibility rejection, not a parser/process error: %v", err)
	}
	for _, issue := range incompatible.Issues {
		if issue.Location == location && issue.Reason == reason {
			return
		}
	}
	t.Fatalf("compatibility rejection lacks %s / %s: %v", location, reason, err)
}

type contractShape struct {
	Additive       bool
	RemoveMarker   bool
	BreakSignature bool
}

type workspace struct {
	root, contracts, reusable string
	goVersion                 string
	sum                       []byte
	consumers                 []string
}

func newWorkspace(t *testing.T) *workspace {
	t.Helper()
	// go test runs in this package's source directory. Do not depend on
	// runtime.Caller paths, which may have been rewritten by -trimpath.
	pkgDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(pkgDir, "..", ".."))
	goVersion := ""
	for line := range strings.SplitSeq(string(read(t, filepath.Join(root, "go.mod"))), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "go" {
			goVersion = fields[1]
		}
	}
	if goVersion == "" {
		t.Fatal("framework go.mod has no go directive")
	}
	dir := t.TempDir()
	w := &workspace{root: root, contracts: filepath.Join(dir, "contracts"),
		reusable: filepath.Join(dir, "reusable"), goVersion: goVersion,
		sum:       read(t, filepath.Join(root, "go.sum")),
		consumers: []string{filepath.Join(dir, "projectalpha"), filepath.Join(dir, "projectbeta")}}
	w.module(t, w.contracts, "example.test/contracts", "", "")
	w.module(t, w.reusable, "example.test/reusable", "", "v0.1.0")
	for i, consumer := range w.consumers {
		selected := []string{"primary", "backup"}[i]
		write(t, filepath.Join(consumer, "consumer_test.go"), render(t, "consumer_test.go.tmpl", struct{ Project, Selected string }{
			Project: filepath.Base(consumer), Selected: selected,
		}))
	}
	return w
}

func (w *workspace) module(t *testing.T, dir, name, implementationVersion, contractVersion string) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "module %s\n\ngo %s\n\nrequire github.com/forgeplex/appkit v0.0.0\n", name, w.goVersion)
	if contractVersion != "" {
		fmt.Fprintf(&b, "require example.test/contracts %s\n", contractVersion)
	}
	if implementationVersion != "" {
		fmt.Fprintf(&b, "require example.test/reusable %s\n", implementationVersion)
	}
	fmt.Fprintf(&b, "\nreplace github.com/forgeplex/appkit => %s\n", strconv.Quote(filepath.ToSlash(w.root)))
	if name != "example.test/contracts" {
		fmt.Fprintf(&b, "replace example.test/contracts => %s\n", strconv.Quote(filepath.ToSlash(w.contracts)))
	}
	if implementationVersion != "" {
		fmt.Fprintf(&b, "replace example.test/reusable => %s\n", strconv.Quote(filepath.ToSlash(w.reusable)))
	}
	write(t, filepath.Join(dir, "go.mod"), []byte(b.String()))
	write(t, filepath.Join(dir, "go.sum"), w.sum)
}

func (w *workspace) configureConsumers(t *testing.T, implementationVersion, contractVersion string) {
	t.Helper()
	for _, consumer := range w.consumers {
		w.module(t, consumer, "example.test/"+filepath.Base(consumer), implementationVersion, contractVersion)
	}
}

func (w *workspace) generateContract(t *testing.T, shape contractShape) {
	t.Helper()
	input := filepath.Join(w.contracts, "contract.yaml")
	write(t, input, render(t, "contract.yaml.tmpl", shape))
	if err := gen.Contract(input, filepath.Join(w.contracts, "sample")); err != nil {
		t.Fatalf("generate contract: %v", err)
	}
	// Each negative schema must itself generate a compilable contract. A generic
	// generator failure is not evidence of a downstream compatibility failure.
	w.goOK(t, w.contracts, nil, "test", "-mod=mod", "-run=^$", "./...")
}

func (w *workspace) implementation(t *testing.T, revision int) {
	t.Helper()
	write(t, filepath.Join(w.reusable, "module.go"), render(t, "module.go.tmpl", struct{ Revision int }{revision}))
}

func (w *workspace) consumerSources(t *testing.T) map[string][]byte {
	t.Helper()
	sources := make(map[string][]byte)
	for _, consumer := range w.consumers {
		path := filepath.Join(consumer, "consumer_test.go")
		sources[path] = read(t, path)
	}
	return sources
}

func (w *workspace) checkSharedModule(t *testing.T, version string) {
	t.Helper()
	for _, consumer := range w.consumers {
		out := w.goOK(t, consumer, nil, "list", "-mod=mod", "-m", "-json", "example.test/reusable")
		var resolved struct{ Dir, Version string }
		if err := json.Unmarshal(out, &resolved); err != nil {
			t.Fatalf("decode go list in %s: %v\n%s", consumer, err, out)
		}
		if filepath.Clean(resolved.Dir) != w.reusable || resolved.Version != version {
			t.Fatalf("%s uses %+v; want shared directory %s at %s", consumer, resolved, w.reusable, version)
		}
	}
}

func (w *workspace) testConsumers(t *testing.T, revision int) {
	t.Helper()
	for _, consumer := range w.consumers {
		t.Run(filepath.Base(consumer), func(t *testing.T) {
			out := w.goOK(t, consumer, []string{"FIXTURE_REVISION=" + strconv.Itoa(revision)},
				"test", "-mod=mod", "-count=1", "-timeout=25s", "-v", ".")
			t.Logf("%s", out)
		})
	}
}

func (w *workspace) rejectConsumers(t *testing.T, diagnostic string) {
	t.Helper()
	for _, consumer := range w.consumers {
		t.Run(filepath.Base(consumer), func(t *testing.T) {
			out, err := w.goCommand(t, consumer, nil, "test", "-mod=mod", "-run=^$", ".")
			if err == nil {
				t.Fatalf("breaking contract unexpectedly compiled in %s", consumer)
			}
			if !strings.Contains(string(out), diagnostic) || !strings.Contains(string(out), "[build failed]") {
				t.Fatalf("expected compiler diagnostic %q, not generic process failure: %v\n%s", diagnostic, err, out)
			}
			t.Logf("compiler rejected expected break: %s", out)
		})
	}
}

func (w *workspace) goOK(t *testing.T, dir string, extra []string, args ...string) []byte {
	t.Helper()
	out, err := w.goCommand(t, dir, extra, args...)
	if err != nil {
		t.Fatalf("go %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return out
}

func (w *workspace) goCommand(t *testing.T, dir string, extra []string, args ...string) ([]byte, error) {
	t.Helper()
	if childRace && len(args) > 0 && args[0] == "test" {
		args = append([]string{"test", "-race"}, args[1:]...)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	// Do not inherit workspaces, module flags, proxy/checksum settings or an
	// automatic toolchain download from the developer's shell.
	overrides := append([]string{
		"GOENV=off", "GOWORK=off", "GOFLAGS=", "GOPROXY=off", "GOSUMDB=off",
		"GOPRIVATE=", "GONOPROXY=none", "GOVCS=*:off", "GOTOOLCHAIN=local",
	}, extra...)
	blocked := make(map[string]bool, len(overrides))
	for _, value := range overrides {
		key, _, _ := strings.Cut(value, "=")
		blocked[key] = true
	}
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if !blocked[key] {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, overrides...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("bounded Go command expired: go %s in %s: %v\n%s", strings.Join(args, " "), dir, ctx.Err(), out)
	}
	return out, err
}

func render(t *testing.T, name string, data any) []byte {
	t.Helper()
	tmpl, err := template.ParseFS(fixtures, "testdata/"+name)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, data); err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(name, ".go.tmpl") {
		formatted, err := format.Source(b.Bytes())
		if err != nil {
			t.Fatalf("format fixture %s: %v", name, err)
		}
		return formatted
	}
	return b.Bytes()
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

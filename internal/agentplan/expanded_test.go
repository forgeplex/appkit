package agentplan

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/forgeplex/appkit/internal/gen"
	"github.com/forgeplex/appkit/internal/scaffold"
	"github.com/forgeplex/appkit/internal/workspace"
)

type expandedGenerator struct {
	name, input, target, source string
	plan                        func(context.Context, string, string, string) (workspace.Plan, error)
	write                       func(string, string) error
}

func expandedGenerators() []expandedGenerator {
	return []expandedGenerator{
		{"events", "api/events.yaml", "api/events.gen.go", "version: 1\npackage: api\nevents:\n  - {name: Created, topic: sample.created, fields: [{name: id, type: string}]}\n", Events, gen.Events},
		{"errors", "api/codes.yaml", "api/codes.gen.go", "version: 1\npackage: api\ncodes:\n  - {code: SAMPLE_MISSING, status: 404, message: Missing}\n", Errors, gen.Errors},
		{"wrap", "api/service.go", "api/wrap.gen.go", "package api\nimport \"context\"\ntype Service interface { Ping(context.Context) error }\n", func(ctx context.Context, root, input, target string) (workspace.Plan, error) {
			return Wrap(ctx, root, input, target, "Service", "sample")
		}, func(input, target string) error { return gen.Wrap(filepath.Dir(input), "Service", "sample", target) }},
	}
}

func TestExpandedGeneratorPlanRoundTripApplyParityReplay(t *testing.T) {
	for _, tc := range expandedGenerators() {
		t.Run(tc.name, func(t *testing.T) {
			ctx, root := context.Background(), t.TempDir()
			put(t, root, tc.input, tc.source)
			put(t, root, "handwritten.txt", "untouched")
			before := expandedTree(t, root)
			plan, err := tc.plan(ctx, root, tc.input, tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, expandedTree(t, root)) {
				t.Fatal("plan mutated workspace")
			}
			again, err := tc.plan(ctx, root, tc.input, tc.target)
			if err != nil || again.Digest() != plan.Digest() {
				t.Fatalf("nondeterministic plan: %v", err)
			}
			plan = expandedRoundTrip(t, plan)
			baseline := filepath.Join(t.TempDir(), "expected.go")
			if err := tc.write(filepath.Join(root, tc.input), baseline); err != nil {
				t.Fatal(err)
			}
			for _, want := range []workspace.ApplyDisposition{workspace.ApplyCommitted, workspace.ApplyReplayed} {
				result, err := workspace.Apply(ctx, root, plan)
				if err != nil || result.Disposition != want {
					t.Fatalf("apply: %+v %v", result, err)
				}
			}
			got, err := os.ReadFile(filepath.Join(root, tc.target))
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(baseline)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("plan output differs from normal generator: %v", err)
			}
			clean, err := tc.plan(ctx, root, tc.input, tc.target)
			if err != nil {
				t.Fatal(err)
			}
			for _, change := range clean.Changes() {
				if change.Operation != workspace.OperationAssert {
					t.Fatalf("clean output scheduled for write: %+v", change)
				}
			}
		})
	}
}

func TestExpandedGeneratorPlanRejectsDrift(t *testing.T) {
	for _, tc := range expandedGenerators() {
		for _, drift := range []string{"source", "target"} {
			t.Run(tc.name+"/"+drift, func(t *testing.T) {
				ctx, root := context.Background(), t.TempDir()
				put(t, root, tc.input, tc.source)
				plan, err := tc.plan(ctx, root, tc.input, tc.target)
				if err != nil {
					t.Fatal(err)
				}
				changed := tc.input
				if drift == "target" {
					changed = tc.target
				}
				put(t, root, changed, "concurrent edit")
				before := expandedTree(t, root)
				if _, err := workspace.Apply(ctx, root, expandedRoundTrip(t, plan)); !errors.Is(err, workspace.ErrChanged) {
					t.Fatalf("drift accepted: %v", err)
				}
				if !reflect.DeepEqual(before, expandedTree(t, root)) {
					t.Fatal("rejected apply changed workspace")
				}
			})
		}
	}
}

func TestExpandedGeneratorPlanUpdatesExistingOutputMode(t *testing.T) {
	for _, tc := range expandedGenerators() {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			put(t, root, tc.input, tc.source)
			put(t, root, tc.target, "outdated generated content")
			if err := os.Chmod(filepath.Join(root, tc.target), 0o600); err != nil {
				t.Fatal(err)
			}
			plan, err := tc.plan(context.Background(), root, tc.input, tc.target)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, change := range plan.Changes() {
				if change.Path == tc.target {
					found = true
					if change.Operation != workspace.OperationUpdate || change.Mode != 0o600 {
						t.Fatalf("update did not preserve output mode: %+v", change)
					}
				}
			}
			if !found {
				t.Fatal("missing output update")
			}
			if _, err := workspace.Apply(context.Background(), root, expandedRoundTrip(t, plan)); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(root, tc.target))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("applied output mode: %v %v", info, err)
			}
		})
	}
}

func TestExpandedGeneratorPlanValidation(t *testing.T) {
	for _, tc := range expandedGenerators() {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			put(t, root, tc.input, tc.source)
			for _, invalid := range []string{"", "../escape.go", "/tmp/output.go", "a/../out.go", "api/out_test.go", "api/out.txt"} {
				if _, err := tc.plan(context.Background(), root, tc.input, invalid); !errors.Is(err, workspace.ErrInvalidPath) {
					t.Errorf("target %q: %v", invalid, err)
				}
			}
			put(t, root, "api/shared.go", tc.source)
			if _, err := tc.plan(context.Background(), root, "api/shared.go", "api/shared.go"); !errors.Is(err, workspace.ErrInvalidChange) {
				t.Errorf("input/output overlap: %v", err)
			}
			put(t, root, tc.input, "invalid content")
			before := expandedTree(t, root)
			if _, err := tc.plan(context.Background(), root, tc.input, tc.target); err == nil {
				t.Error("invalid source accepted")
			}
			if !reflect.DeepEqual(before, expandedTree(t, root)) {
				t.Fatal("invalid plan wrote output")
			}
		})
	}
	root := t.TempDir()
	if _, err := Wrap(context.Background(), root, "api/service.go", "other/wrap.go", "Service", "sys"); !errors.Is(err, workspace.ErrInvalidPath) {
		t.Fatalf("cross-package wrapper accepted: %v", err)
	}
}

func TestExpandedGeneratorPlanRejectsSymlinks(t *testing.T) {
	for _, tc := range expandedGenerators() {
		for _, linkInput := range []bool{true, false} {
			t.Run(tc.name+map[bool]string{true: "/input", false: "/output"}[linkInput], func(t *testing.T) {
				root := t.TempDir()
				put(t, root, tc.input, tc.source)
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(outside, []byte(tc.source), 0o644); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(root, tc.target)
				if linkInput {
					link = filepath.Join(root, tc.input)
					if err := os.Remove(link); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(outside, link); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
				if _, err := tc.plan(context.Background(), root, tc.input, tc.target); !errors.Is(err, workspace.ErrSymlink) {
					t.Fatalf("symlink accepted: %v", err)
				}
			})
		}
	}
}

func TestExpandedNewPlanMatrixParityReplay(t *testing.T) {
	for _, tc := range []struct {
		name, kind          string
		tenant, partitioned bool
	}{
		{"domain", "domain", false, false}, {"tenant", "domain", true, false},
		{"partitioned", "domain", false, true}, {"both", "domain", true, true},
		{"system", "system", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, root := context.Background(), t.TempDir()
			opts := scaffold.Options{Name: "sample", Module: "example.com/sample", AppkitVersion: "v0.7.3", Tenant: tc.tenant, Partitioned: tc.partitioned, Dir: filepath.Join(t.TempDir(), "ignored")}
			put(t, root, "handwritten.txt", "unchanged")
			before := expandedTree(t, root)
			plan, err := New(ctx, root, "repos/sample", tc.kind, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, expandedTree(t, root)) {
				t.Fatal("new plan wrote target files")
			}
			if _, err := os.Stat(filepath.Join(root, "repos/sample")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("new plan created target directory: %v", err)
			}
			if _, err := os.Stat(opts.Dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("renderer used Options.Dir: %v", err)
			}
			for _, change := range plan.Changes() {
				if change.Operation != workspace.OperationCreate {
					t.Fatalf("scaffold change is not create-only: %+v", change)
				}
			}
			again, err := New(ctx, root, "repos/sample", tc.kind, opts)
			if err != nil || again.Digest() != plan.Digest() {
				t.Fatalf("nondeterministic scaffold plan: %v", err)
			}
			plan = expandedRoundTrip(t, plan)
			for _, disposition := range []workspace.ApplyDisposition{workspace.ApplyCommitted, workspace.ApplyReplayed} {
				result, err := workspace.Apply(ctx, root, plan)
				if err != nil || result.Disposition != disposition {
					t.Fatalf("apply: %+v %v", result, err)
				}
			}
			baseline := t.TempDir()
			opts.Dir = baseline
			if tc.kind == "domain" {
				err = scaffold.Domain(opts, nil)
			} else {
				err = scaffold.System(opts, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, want := expandedTree(t, filepath.Join(root, "repos/sample")), expandedTree(t, baseline); !reflect.DeepEqual(got, want) {
				t.Fatal("planned scaffold differs from normal scaffold")
			}
		})
	}
}

func TestExpandedNewPlanRejectsExistingAndConcurrentFiles(t *testing.T) {
	ctx := context.Background()
	for _, existing := range []string{"go.mod", "handwritten.txt"} {
		root := t.TempDir()
		put(t, root, "new/"+existing, "keep")
		before := expandedTree(t, root)
		if _, err := New(ctx, root, "new", "domain", scaffold.Options{Name: "sample"}); !errors.Is(err, workspace.ErrInvalidChange) {
			t.Fatalf("nonempty target accepted: %v", err)
		}
		if !reflect.DeepEqual(before, expandedTree(t, root)) {
			t.Fatal("nonempty target changed")
		}
	}
	root := t.TempDir()
	plan, err := New(ctx, root, "new", "domain", scaffold.Options{Name: "sample"})
	if err != nil {
		t.Fatal(err)
	}
	put(t, root, "new/go.mod", "concurrent user file")
	before := expandedTree(t, root)
	if _, err := workspace.Apply(ctx, root, plan); !errors.Is(err, workspace.ErrChanged) {
		t.Fatalf("create-only scaffold overwrote concurrent file: %v", err)
	}
	if !reflect.DeepEqual(before, expandedTree(t, root)) {
		t.Fatal("failed scaffold apply changed workspace")
	}
}

func TestExpandedNewPlanAcceptsEmptyDirectories(t *testing.T) {
	for _, target := range []string{".", "empty"} {
		root := t.TempDir()
		if target != "." {
			if err := os.Mkdir(filepath.Join(root, target), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		plan, err := New(context.Background(), root, target, "system", scaffold.Options{Name: "sample"})
		if err != nil {
			t.Fatal(err)
		}
		if entries, err := os.ReadDir(filepath.Join(root, target)); err != nil || len(entries) != 0 {
			t.Fatalf("plan changed existing empty target: %v %v", entries, err)
		}
		if _, err := workspace.Apply(context.Background(), root, expandedRoundTrip(t, plan)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExpandedNewPlanValidation(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		target, kind string
		opts         scaffold.Options
	}{
		{"../escape", "domain", scaffold.Options{Name: "sample"}},
		{"new", "unknown", scaffold.Options{Name: "sample"}},
		{"new", "domain", scaffold.Options{Name: "Bad-Name"}},
		{"new", "system", scaffold.Options{Name: "sample", Tenant: true}},
		{"new", "system", scaffold.Options{Name: "sample", Partitioned: true}},
	} {
		if _, err := New(context.Background(), root, tc.target, tc.kind, tc.opts); err == nil {
			t.Fatalf("invalid scaffold accepted: %+v", tc)
		}
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := New(context.Background(), root, "linked/new", "domain", scaffold.Options{Name: "sample"}); !errors.Is(err, workspace.ErrSymlink) {
		t.Fatalf("symlink ancestor accepted: %v", err)
	}
}

func expandedRoundTrip(t *testing.T, plan workspace.Plan) workspace.Plan {
	t.Helper()
	encoded, err := workspace.MarshalPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := workspace.ParsePlan(encoded)
	if err != nil {
		t.Fatal(err)
	}
	again, err := workspace.MarshalPlan(parsed)
	if err != nil || !bytes.Equal(encoded, again) || parsed.Digest() != plan.Digest() {
		t.Fatalf("canonical plan round trip failed: %v", err)
	}
	return parsed
}

func expandedTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	if err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

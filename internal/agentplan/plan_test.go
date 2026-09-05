package agentplan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/forgeplex/appkit/internal/gen"
	"github.com/forgeplex/appkit/internal/workspace"
	"github.com/forgeplex/appkit/ruleset"
)

const testWorkflowRef = "0123456789abcdef0123456789abcdef01234567"

func put(t *testing.T, root, name, content string) {
	t.Helper()
	full := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func domain(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	put(t, dir, ".appkit.yml", "version: 1\ndomain: sample\nmodule: example.com/sample\n")
	return dir
}

func TestSyncPlanReadOnlyDeterministicApplyAndReplay(t *testing.T) {
	ctx := context.Background()
	root := domain(t)
	put(t, root, "user.txt", "untouched")
	plan, err := SyncPinned(ctx, root, "v0.7.2", testWorkflowRef)
	if err != nil {
		t.Fatal(err)
	}
	again, err := SyncPinned(ctx, root, "v0.7.2", testWorkflowRef)
	if err != nil || plan.Digest() != again.Digest() {
		t.Fatalf("nondeterministic: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 {
		t.Fatalf("planning wrote files: %v %v", entries, err)
	}
	if len(plan.Changes()) != 4 {
		t.Fatalf("expected config assertion and three outputs: %v", plan.Changes())
	}
	if _, err := workspace.Apply(ctx, root, plan); err != nil {
		t.Fatal(err)
	}
	if err := ruleset.CheckPinned(root, "v0.7.2", testWorkflowRef); err != nil {
		t.Fatal(err)
	}
	result, err := workspace.Apply(ctx, root, plan)
	if err != nil || result.Disposition != workspace.ApplyReplayed {
		t.Fatalf("replay: %+v %v", result, err)
	}
	clean, err := SyncPinned(ctx, root, "v0.7.2", testWorkflowRef)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range clean.Changes() {
		if c.Operation != workspace.OperationAssert {
			t.Errorf("unchanged output scheduled for write: %+v", c)
		}
	}
	if content, _ := os.ReadFile(filepath.Join(root, "user.txt")); string(content) != "untouched" {
		t.Fatal("unrelated file modified")
	}
}

func TestPlanRejectsInputOrOutputDrift(t *testing.T) {
	for _, changed := range []string{".appkit.yml", ".golangci.yml"} {
		t.Run(changed, func(t *testing.T) {
			root := domain(t)
			plan, err := SyncPinned(context.Background(), root, "test", testWorkflowRef)
			if err != nil {
				t.Fatal(err)
			}
			put(t, root, changed, "user edit")
			_, err = workspace.Apply(context.Background(), root, plan)
			if !errors.Is(err, workspace.ErrChanged) {
				t.Fatalf("expected conflict, got %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, ".go-arch-lint.yml")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial write despite conflict: %v", err)
			}
		})
	}
}

func TestRenderInputRaceRejected(t *testing.T) {
	root := domain(t)
	_, err := build(context.Background(), root, ".appkit.yml", func(data []byte) (map[string][]byte, error) {
		put(t, root, ".appkit.yml", "changed during rendering")
		return map[string][]byte{"out.txt": []byte("from old input")}, nil
	})
	if !errors.Is(err, workspace.ErrChanged) {
		t.Fatalf("old rendering bound to changed input: %v", err)
	}
}

func TestRendererUsesCapturedBytes(t *testing.T) {
	root := domain(t)
	before, err := os.ReadFile(filepath.Join(root, ".appkit.yml"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := build(context.Background(), root, ".appkit.yml", func(data []byte) (map[string][]byte, error) {
		// Simulate an unrelated writer changing and restoring the source while
		// rendering. The generator must consume its captured bytes, never reopen.
		put(t, root, ".appkit.yml", "transient contents")
		if string(data) != string(before) {
			t.Fatal("renderer did not receive captured bytes")
		}
		put(t, root, ".appkit.yml", string(before))
		return map[string][]byte{"copy.txt": data}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Apply(context.Background(), root, plan); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "copy.txt"))
	if err != nil || string(got) != string(before) {
		t.Fatalf("rendered bytes not captured bytes: %s %v", got, err)
	}
}

func TestContractPlanMatchesGenerator(t *testing.T) {
	root := t.TempDir()
	data, err := os.ReadFile("../gen/testdata/contract.yaml")
	if err != nil {
		t.Fatal(err)
	}
	put(t, root, "api/contract.yaml", string(data))
	plan, err := Contract(context.Background(), root, "api/contract.yaml", "generated")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "generated")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("planning created target: %v", err)
	}
	if _, err := workspace.Apply(context.Background(), root, plan); err != nil {
		t.Fatal(err)
	}
	if err := gen.CheckContract(filepath.Join(root, "api/contract.yaml"), filepath.Join(root, "generated")); err != nil {
		t.Fatal(err)
	}
}

func TestPlanRejectsUnsafePathsAndSymlinks(t *testing.T) {
	root := domain(t)
	for _, invalid := range []string{"", "../escape", "/tmp/out", "a/../b", "a\\b", "C:/out"} {
		if _, err := Contract(context.Background(), root, ".appkit.yml", invalid); !errors.Is(err, workspace.ErrInvalidPath) {
			t.Errorf("unsafe target %q: %v", invalid, err)
		}
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, ".github")); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncPinned(context.Background(), root, "test", testWorkflowRef); !errors.Is(err, workspace.ErrSymlink) {
		t.Fatalf("accepted symlink output ancestor: %v", err)
	}
}

func TestPlanSourceMissingAndCanceled(t *testing.T) {
	if _, err := SyncPinned(context.Background(), t.TempDir(), "test", testWorkflowRef); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing input: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SyncPinned(ctx, domain(t), "test", testWorkflowRef); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}

func TestExistingModePreservedAndUnrelatedChangesAllowed(t *testing.T) {
	root := domain(t)
	put(t, root, ".golangci.yml", "old")
	if err := os.Chmod(filepath.Join(root, ".golangci.yml"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := SyncPinned(context.Background(), root, "test", testWorkflowRef)
	if err != nil {
		t.Fatal(err)
	}
	put(t, root, "unrelated.txt", "added after plan")
	if _, err := workspace.Apply(context.Background(), root, plan); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, ".golangci.yml"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode not preserved: %v %v", info, err)
	}
}

package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func directoryFixture(t *testing.T) (string, Plan) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"migrations", "docs"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeMode(t, filepath.Join(root, "migrations/001.sql"), []byte("SELECT 1;"), 0o644)
	writeMode(t, filepath.Join(root, "docs/old.md"), []byte("old documentation"), 0o644)
	if err := os.Mkdir(filepath.Join(root, "docs/empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	var guards []DirectorySnapshot
	for _, dir := range []string{"migrations", "docs"} {
		snapshot, err := CaptureDirectory(root, dir)
		if err != nil {
			t.Fatal(err)
		}
		guards = append(guards, snapshot)
	}
	plan, err := BuildPlanWithGuards(root, []Change{
		{Path: "migrations/001.sql", Operation: OperationAssert},
		{Path: "docs/old.md", Operation: OperationDelete},
		{Path: "docs/nested/new.md", Operation: OperationCreate, Content: []byte("new documentation"), Mode: 0o644},
	}, guards)
	if err != nil {
		t.Fatal(err)
	}
	return root, plan
}

func TestDirectoryGuardMembershipAndReplay(t *testing.T) {
	root, plan := directoryFixture(t)
	guards := plan.DirectoryGuards()
	want := []DirectoryEntry{{Path: "empty", Kind: DirectoryDir}, {Path: "nested", Kind: DirectoryDir}, {Path: "nested/new.md", Kind: DirectoryFile}}
	if guards[0].Before.Path != "docs" || !slices.Equal(guards[0].After.Entries, want) {
		t.Fatalf("derived final directory = %+v", guards[0])
	}
	guards[0].Before.Entries[0].Path = "caller mutation"
	if err := plan.Validate(); err != nil {
		t.Fatalf("getter exposed mutable plan state: %v", err)
	}
	encoded, err := MarshalPlan(plan)
	if err != nil || !bytes.Contains(encoded, []byte(GuardedPlanAPIVersion)) {
		t.Fatalf("guarded wire: %s %v", encoded, err)
	}
	parsed, err := ParsePlan(encoded)
	if err != nil || parsed.Digest() != plan.Digest() || !reflect.DeepEqual(parsed.DirectoryGuards(), plan.DirectoryGuards()) {
		t.Fatalf("guarded round trip: %v", err)
	}
	for _, disposition := range []ApplyDisposition{ApplyCommitted, ApplyReplayed} {
		result, err := Apply(context.Background(), root, parsed)
		if err != nil || result.Disposition != disposition {
			t.Fatalf("apply = %+v, %v", result, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "docs/empty")); err != nil {
		t.Fatalf("preexisting empty directory removed: %v", err)
	}
	got, err := CaptureDirectory(root, "docs")
	if err != nil || !equalDirectory(got, plan.guards[0].After) {
		t.Fatalf("final membership = %+v, %v", got, err)
	}
	writeMode(t, filepath.Join(root, "migrations/002.sql"), []byte("SELECT 2;"), 0o644)
	if _, err := Apply(context.Background(), root, parsed); !errors.Is(err, ErrChanged) {
		t.Fatalf("replay accepted a new migration: %v", err)
	}
}

func TestDirectoryGuardsRejectAddedRemovedRenamedAndExtraMembers(t *testing.T) {
	for _, mutation := range []string{"add migration", "remove migration", "rename migration", "extra output", "extra empty dir"} {
		t.Run(mutation, func(t *testing.T) {
			root, plan := directoryFixture(t)
			switch mutation {
			case "add migration":
				writeMode(t, filepath.Join(root, "migrations/002.sql"), []byte("SELECT 2"), 0o644)
			case "remove migration":
				if err := os.Remove(filepath.Join(root, "migrations/001.sql")); err != nil {
					t.Fatal(err)
				}
			case "rename migration":
				if err := os.Rename(filepath.Join(root, "migrations/001.sql"), filepath.Join(root, "migrations/renamed.sql")); err != nil {
					t.Fatal(err)
				}
			case "extra output":
				writeMode(t, filepath.Join(root, "docs/user.md"), []byte("keep user content"), 0o644)
			case "extra empty dir":
				if err := os.Mkdir(filepath.Join(root, "docs/unplanned"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Apply(context.Background(), root, plan); !errors.Is(err, ErrChanged) {
				t.Fatalf("membership drift accepted: %v", err)
			}
			if got := readFile(t, filepath.Join(root, "docs/old.md")); got != "old documentation" {
				t.Fatalf("rejected apply changed existing output: %q", got)
			}
			if _, err := os.Stat(filepath.Join(root, "docs/nested")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected apply created output parents: %v", err)
			}
			assertNoTransactions(t, root)
		})
	}
}

func TestDirectoryCaptureBeforeBuildAndInputOwnership(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMode(t, filepath.Join(root, "migrations/001.sql"), []byte("one"), 0o644)
	snapshot, err := CaptureDirectory(root, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	changes := []Change{{Path: "migrations/001.sql", Operation: OperationAssert}}
	plan, err := BuildPlanWithGuards(root, changes, []DirectorySnapshot{snapshot})
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Entries[0].Path = "caller-mutated.sql"
	if err := plan.Validate(); err != nil {
		t.Fatalf("builder retained input entries: %v", err)
	}
	snapshot = plan.DirectoryGuards()[0].Before
	writeMode(t, filepath.Join(root, "migrations/002.sql"), []byte("two"), 0o644)
	if _, err := BuildPlanWithGuards(root, changes, []DirectorySnapshot{snapshot}); !errors.Is(err, ErrChanged) {
		t.Fatalf("builder silently refreshed stale membership: %v", err)
	}
}

func TestDirectoryRootAndMissingOutput(t *testing.T) {
	for _, dir := range []string{".", "docs"} {
		t.Run(dir, func(t *testing.T) {
			root := t.TempDir()
			snapshot, err := CaptureDirectory(root, dir)
			if err != nil || snapshot.Exists != (dir == ".") || snapshot.Entries == nil {
				t.Fatalf("capture = %+v %v", snapshot, err)
			}
			plan, err := BuildPlanWithGuards(root, []Change{{Path: "docs/nested/a.md", Operation: OperationCreate, Content: []byte("a"), Mode: 0o644}}, []DirectorySnapshot{snapshot})
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []ApplyDisposition{ApplyCommitted, ApplyReplayed} {
				result, err := Apply(context.Background(), root, plan)
				if err != nil || result.Disposition != want {
					t.Fatalf("root/missing guard apply = %+v %v", result, err)
				}
			}
		})
	}
}

func TestDirectoryGuardsLegacyPlansUnchanged(t *testing.T) {
	root := t.TempDir()
	changes := []Change{{Path: "out.txt", Operation: OperationCreate, Content: []byte("data"), Mode: 0o644}}
	legacy, err := BuildPlan(root, changes)
	if err != nil {
		t.Fatal(err)
	}
	optional, err := BuildPlanWithGuards(root, changes, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := MarshalPlan(legacy)
	b, _ := MarshalPlan(optional)
	if legacy.Digest() != optional.Digest() || !bytes.Equal(a, b) || bytes.Contains(b, []byte("directoryGuards")) || !bytes.Contains(b, []byte(PlanAPIVersion)) {
		t.Fatal("no-guard plan changed legacy encoding or digest")
	}
}

func TestDirectoryGuardsRejectMalformedAndTamperedDocuments(t *testing.T) {
	_, plan := directoryFixture(t)
	encoded, err := MarshalPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, modified := range [][]byte{
		bytes.Replace(encoded, []byte(GuardedPlanAPIVersion), []byte(PlanAPIVersion), 1),
		bytes.Replace(encoded, []byte(`"path":"old.md"`), []byte(`"path":"different.md"`), 1),
		bytes.Replace(encoded, []byte(`"kind":"file"`), []byte(`"kind":"socket"`), 1),
		bytes.Replace(encoded, []byte(`"directoryGuards":`), []byte(`"unknownGuards":`), 1),
	} {
		if _, err := ParsePlan(modified); err == nil {
			t.Fatalf("tampered guards accepted: %s", modified)
		}
	}
	for _, mutate := range []func(*Plan){
		func(p *Plan) { p.guards[0].After.Path = "different" },
		func(p *Plan) {
			p.guards[0].After.Entries = append(p.guards[0].After.Entries, DirectoryEntry{Path: "unplanned", Kind: DirectoryFile})
		},
		func(p *Plan) { p.guards[0].Before.Entries = []DirectoryEntry{{Path: "../escape", Kind: DirectoryFile}} },
		func(p *Plan) { p.guards[0].Before.Entries = []DirectoryEntry{{Path: "a/b", Kind: DirectoryFile}} },
		func(p *Plan) { p.guards[0].Before.Entries = nil },
		func(p *Plan) { p.guards = append(p.guards, p.guards[0]) },
	} {
		candidate, err := ParsePlan(encoded)
		if err != nil {
			t.Fatal(err)
		}
		mutate(&candidate)
		candidate.digest = digestPlan(candidate)
		if err := candidate.Validate(); !errors.Is(err, ErrInvalidPlan) {
			t.Fatalf("malformed guards accepted even with new digest: %v", err)
		}
	}
	root := t.TempDir()
	snapshot, _ := CaptureDirectory(root, "docs")
	missing, err := BuildPlanWithGuards(root, []Change{{Path: "docs/a", Operation: OperationCreate, Content: []byte("a"), Mode: 0o644}}, []DirectorySnapshot{snapshot})
	if err != nil {
		t.Fatal(err)
	}
	canonical, _ := MarshalPlan(missing)
	if _, err := ParsePlan(bytes.Replace(canonical, []byte(`"entries":[]`), []byte(`"entries":null`), 1)); err == nil {
		t.Fatal("null entries accepted despite writer canonicalizing to []")
	}
}

func TestDirectoryGuardsContainmentAndBounds(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"", "../outside", "/tmp", "a/../b", "a\\b", ".appkit-workspace-txn-fake", strings.Repeat("x", maxPlanPathBytes+1)} {
		if _, err := CaptureDirectory(root, name); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("unsafe directory %q: %v", name, err)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	for _, name := range []string{"linked", "linked/nested", "."} {
		if _, err := CaptureDirectory(root, name); !errors.Is(err, ErrSymlink) {
			t.Errorf("symlink directory %q: %v", name, err)
		}
	}
	if _, err := CaptureDirectory(filepath.Join(root, "linked"), "."); !errors.Is(err, ErrSymlink) {
		t.Fatalf("symlink workspace root accepted: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	writeMode(t, filepath.Join(root, "regular"), []byte("file"), 0o644)
	if _, err := CaptureDirectory(root, "regular"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("regular file treated as directory: %v", err)
	}
	tooMany := DirectorySnapshot{Path: "missing", Entries: make([]DirectoryEntry, MaxDirectoryEntries+1)}
	if _, err := BuildPlanWithGuards(root, []Change{{Path: "regular", Operation: OperationAssert}}, []DirectorySnapshot{tooMany}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("unbounded entries accepted: %v", err)
	}
	socketRoot, err := os.MkdirTemp("", "appkit-dir-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })
	listener, err := net.Listen("unix", filepath.Join(socketRoot, "socket"))
	if err != nil {
		t.Skipf("unix socket unavailable: %v", err)
	}
	defer listener.Close()
	if _, err := CaptureDirectory(socketRoot, "."); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("special file accepted: %v", err)
	}
}

func TestDirectoryGuardRecheckedAfterAdmissionAndAtFinal(t *testing.T) {
	for _, phase := range []string{"admission", "after operation"} {
		t.Run(phase, func(t *testing.T) {
			root, plan := directoryFixture(t)
			mutate := func() { writeMode(t, filepath.Join(root, "migrations/002.sql"), []byte("concurrent"), 0o644) }
			var err error
			if phase == "admission" {
				_, err = ApplyWithCommitGate(context.Background(), root, plan, CommitGate{
					Domain: "test.directory", SubjectDigest: digestBytes([]byte("subject")),
					Admit:   func(context.Context) (string, error) { return digestBytes([]byte("receipt")), nil },
					Recheck: func(context.Context, string) error { mutate(); return nil },
				})
			} else {
				_, err = apply(context.Background(), root, plan, applyOptions{inject: func(point applyFaultPoint, target string) error {
					if point == faultAfterOperation && target == "docs/old.md" {
						mutate()
					}
					return nil
				}})
			}
			if !errors.Is(err, ErrChanged) {
				t.Fatalf("membership change crossed commit boundary: %v", err)
			}
			if got := readFile(t, filepath.Join(root, "docs/old.md")); got != "old documentation" {
				t.Fatalf("membership failure did not restore output: %q", got)
			}
			if _, err := os.Stat(filepath.Join(root, "docs/nested")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rollback retained created directories: %v", err)
			}
			if phase == "admission" {
				assertNoTransactions(t, root)
				if errors.Is(err, ErrRollback) {
					t.Fatal("prewrite membership conflict incorrectly requires recovery")
				}
			}
		})
	}
}

func TestDirectoryGuardCrashRecovery(t *testing.T) {
	for _, tc := range []struct {
		point       applyFaultPoint
		target      string
		disposition ApplyDisposition
	}{
		{faultAfterInstall, "docs/nested/new.md", ApplyCommitted},
		// Backing up the final delete already establishes the complete final
		// public state, so recovery correctly finalizes instead of rerunning it.
		{faultAfterBackup, "docs/old.md", ApplyReplayed},
		{faultAfterOperation, "docs/old.md", ApplyReplayed},
	} {
		t.Run(string(tc.point), func(t *testing.T) {
			root, plan := directoryFixture(t)
			directoryCrash(t, root, plan, tc.point, tc.target)
			result, err := Apply(context.Background(), root, plan)
			if err != nil || result.Disposition != tc.disposition {
				t.Fatalf("recover evolving membership = %+v %v", result, err)
			}
			assertNoTransactions(t, root)
			got, err := CaptureDirectory(root, "docs")
			if err != nil || !equalDirectory(got, plan.guards[0].After) {
				t.Fatalf("recovered final directories = %+v %v", got, err)
			}
		})
	}
}

func TestDirectoryGuardRecoveryRefusesUnplannedMemberBeforeWrites(t *testing.T) {
	root, plan := directoryFixture(t)
	directoryCrash(t, root, plan, faultAfterInstall, "docs/nested/new.md")
	writeMode(t, filepath.Join(root, "migrations/002.sql"), []byte("unplanned"), 0o644)
	if _, err := Apply(context.Background(), root, plan); !errors.Is(err, ErrRecovery) {
		t.Fatalf("recovery accepted changed membership: %v", err)
	}
	if got := readFile(t, filepath.Join(root, "docs/nested/new.md")); got != "new documentation" {
		t.Fatalf("recovery mutated outputs before validating source membership: %q", got)
	}
	if err := os.Remove(filepath.Join(root, "migrations/002.sql")); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), root, plan); err != nil {
		t.Fatalf("recovery after restoring membership: %v", err)
	}
}

func TestDirectoryGuardCrashHelper(t *testing.T) {
	planPath := os.Getenv("APPKIT_DIRECTORY_CRASH_PLAN")
	if planPath == "" {
		return
	}
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := ParsePlan(data)
	if err != nil {
		t.Fatal(err)
	}
	root := os.Getenv("APPKIT_DIRECTORY_CRASH_ROOT")
	point := applyFaultPoint(os.Getenv("APPKIT_DIRECTORY_CRASH_POINT"))
	target := os.Getenv("APPKIT_DIRECTORY_CRASH_TARGET")
	_, err = apply(context.Background(), root, plan, applyOptions{inject: func(actual applyFaultPoint, name string) error {
		if actual == point && name == target {
			os.Exit(87)
		}
		return nil
	}})
	t.Fatalf("crash point not reached: %v", err)
}

func directoryCrash(t *testing.T, root string, plan Plan, point applyFaultPoint, target string) {
	t.Helper()
	data, err := MarshalPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(t.TempDir(), "guarded-plan.json")
	if err := os.WriteFile(planPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestDirectoryGuardCrashHelper$")
	command.Env = append(os.Environ(), "APPKIT_DIRECTORY_CRASH_PLAN="+planPath, "APPKIT_DIRECTORY_CRASH_ROOT="+root,
		"APPKIT_DIRECTORY_CRASH_POINT="+string(point), "APPKIT_DIRECTORY_CRASH_TARGET="+target)
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 87 {
		t.Fatalf("expected crash at %s for %s: %v %s", point, target, err, output)
	}
}

func TestDirectoryGuardRejectsOverlapAndFileDirectoryConflict(t *testing.T) {
	root := t.TempDir()
	var snapshots []DirectorySnapshot
	for _, name := range []string{"a", "a-other", "a/nested"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"a", "a-other", "a/nested"} {
		snapshot, err := CaptureDirectory(root, name)
		if err != nil {
			t.Fatal(err)
		}
		snapshots = append(snapshots, snapshot)
	}
	if _, err := BuildPlanWithGuards(root, []Change{{Path: "out", Operation: OperationAssert}}, snapshots); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("non-adjacent overlapping guards accepted: %v", err)
	}
	for _, target := range []string{"a", "a/nested"} {
		if _, err := BuildPlanWithGuards(root, []Change{{Path: target, Operation: OperationAssert}}, snapshots[:1]); err == nil {
			t.Fatalf("file/directory conflict accepted: %s", target)
		}
	}
	tooMany := make([]DirectorySnapshot, MaxDirectoryGuards+1)
	for i := range tooMany {
		tooMany[i] = DirectorySnapshot{Path: fmt.Sprintf("dir%d", i), Entries: []DirectoryEntry{}}
	}
	if _, err := BuildPlanWithGuards(root, []Change{{Path: "out", Operation: OperationAssert}}, tooMany); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("too many guards accepted: %v", err)
	}
}

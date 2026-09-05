package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestCaptureIsDeterministicAndDetectsChanges(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "appkit.yaml"), "project")
	if err := os.Mkdir(filepath.Join(root, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "config", "app.yaml"), "config")

	left, err := Capture(root, []string{"appkit.lock", "appkit.yaml", "config/app.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	right, err := Capture(root, []string{"config/app.yaml", "appkit.yaml", "appkit.lock"})
	if err != nil {
		t.Fatal(err)
	}
	if left.Digest() != right.Digest() {
		t.Fatalf("digest differs: %s != %s", left.Digest(), right.Digest())
	}
	if !reflect.DeepEqual(left.Files(), right.Files()) {
		t.Fatalf("files differ: %#v != %#v", left.Files(), right.Files())
	}
	if err := left.Verify(root); err != nil {
		t.Fatalf("Verify() = %v", err)
	}

	writeFile(t, filepath.Join(root, "appkit.yaml"), "changed")
	if err := left.Verify(root); !errors.Is(err, ErrChanged) {
		t.Fatalf("Verify(changed) = %v, want ErrChanged", err)
	}
}

func TestCaptureTracksMissingAndCreatedFiles(t *testing.T) {
	root := t.TempDir()
	snapshot, err := Capture(root, []string{"appkit.lock"})
	if err != nil {
		t.Fatal(err)
	}
	files := snapshot.Files()
	if len(files) != 1 || files[0].Exists || files[0].Digest != "" {
		t.Fatalf("Files() = %#v", files)
	}
	if err := snapshot.Verify(root); err != nil {
		t.Fatalf("Verify(missing) = %v", err)
	}
	writeFile(t, filepath.Join(root, "appkit.lock"), "lock")
	if err := snapshot.Verify(root); !errors.Is(err, ErrChanged) {
		t.Fatalf("Verify(created) = %v, want ErrChanged", err)
	}
}

func TestReadFileReturnsBoundedStableContentAndState(t *testing.T) {
	root := t.TempDir()
	writeMode(t, filepath.Join(root, "index.json"), []byte("{}\n"), 0o640)
	content, state, err := ReadFile(root, "index.json", 3)
	if err != nil || string(content) != "{}\n" || !state.Exists || state.Path != "index.json" || state.Mode.Perm() != 0o640 {
		t.Fatalf("ReadFile = (%q, %#v, %v)", content, state, err)
	}
	content[0] = 'x'
	second, _, err := ReadFile(root, "index.json", 3)
	if err != nil || string(second) != "{}\n" {
		t.Fatalf("ReadFile retained caller mutation: %q, %v", second, err)
	}
	if _, _, err := ReadFile(root, "index.json", 2); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("ReadFile oversized error = %v", err)
	}
	missing, state, err := ReadFile(root, "missing", 0)
	if err != nil || missing != nil || state.Exists || state.Path != "missing" {
		t.Fatalf("ReadFile missing = (%q, %#v, %v)", missing, state, err)
	}
}

func TestReadFileRejectsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated permissions")
	}
	root := t.TempDir()
	out := filepath.Join(t.TempDir(), "outside")
	writeFile(t, out, "secret")
	if err := os.Symlink(out, filepath.Join(root, "index")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadFile(root, "index", 1024); !errors.Is(err, ErrSymlink) {
		t.Fatalf("ReadFile symlink error = %v", err)
	}
}

func TestCaptureTreatsMissingParentAsMissingTarget(t *testing.T) {
	root := t.TempDir()
	snapshot, err := Capture(root, []string{"generated/config/appkit.lock"})
	if err != nil {
		t.Fatal(err)
	}
	files := snapshot.Files()
	if len(files) != 1 || files[0].Exists {
		t.Fatalf("Files() = %#v, want one missing file", files)
	}

	if err := os.MkdirAll(filepath.Join(root, "generated", "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Verify(root); err != nil {
		t.Fatalf("Verify(target still missing) = %v", err)
	}

	writeFile(t, filepath.Join(root, "generated", "config", "appkit.lock"), "lock")
	if err := snapshot.Verify(root); !errors.Is(err, ErrChanged) {
		t.Fatalf("Verify(created target) = %v, want ErrChanged", err)
	}
}

func TestCaptureRejectsUnsafePaths(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{
		"",
		".",
		"..",
		"../secret",
		"/absolute",
		"a/../b",
		"a/./b",
		"a//b",
		"a/",
		`a\b`,
		"a\x00b",
		string([]byte{'a', 0xff}),
	} {
		if _, err := Capture(root, []string{name}); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Capture(%q) error = %v, want ErrInvalidPath", name, err)
		}
	}
	if _, err := Capture(root, []string{"a", "a"}); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("Capture(duplicate) error = %v, want ErrInvalidPath", err)
	}
	if _, err := Capture(filepath.Join(root, "missing"), nil); err == nil {
		t.Fatal("Capture(missing root) succeeded")
	}
}

func TestCaptureRejectsSymlinkRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated permissions")
	}
	realRoot := t.TempDir()
	parent := t.TempDir()
	linkedRoot := filepath.Join(parent, "workspace")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := Capture(linkedRoot, nil); !errors.Is(err, ErrSymlink) {
		t.Fatalf("Capture(symlink root) error = %v, want ErrSymlink", err)
	}
}

func TestCaptureRejectsSymbolicLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated permissions")
	}
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret"), "secret")
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "linked-file")); err != nil {
		t.Fatal(err)
	}
	if _, err := Capture(root, []string{"linked-file"}); !errors.Is(err, ErrSymlink) {
		t.Fatalf("Capture(linked file) error = %v, want ErrSymlink", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if _, err := Capture(root, []string{"linked-dir/secret"}); !errors.Is(err, ErrSymlink) {
		t.Fatalf("Capture(linked parent) error = %v, want ErrSymlink", err)
	}
}

func TestOpenedRootDoesNotRetargetAfterRename(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("renaming an open directory is not portable on Windows")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "appkit.yaml"), "original")

	rooted, err := openWorkspaceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rooted.Close()

	moved := filepath.Join(parent, "moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "appkit.yaml"), "outside")
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}

	got, err := captureFile(rooted, "appkit.yaml")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := Capture(moved, []string{"appkit.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != expected.Files()[0].Digest {
		t.Fatalf("capture followed replacement root: digest %q, want %q", got.Digest, expected.Files()[0].Digest)
	}
}

func TestFilesReturnsCopy(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "appkit.yaml"), "project")
	snapshot, err := Capture(root, []string{"appkit.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	files := snapshot.Files()
	files[0].Path = "changed"
	if got := snapshot.Files()[0].Path; got != "appkit.yaml" {
		t.Fatalf("snapshot mutated through Files(): %q", got)
	}
}

func TestVerifyDetectsPermissionAndSameSizeContentChanges(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose portable Unix permission semantics")
	}
	root := t.TempDir()
	name := filepath.Join(root, "appkit.yaml")
	writeFile(t, name, "before")
	if err := os.Chmod(name, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Capture(root, []string{"appkit.yaml"})
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(name, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Verify(root); !errors.Is(err, ErrChanged) {
		t.Fatalf("Verify(permission change) = %v, want ErrChanged", err)
	}

	if err := os.Chmod(name, 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, name, "after!")
	if err := snapshot.Verify(root); !errors.Is(err, ErrChanged) {
		t.Fatalf("Verify(same-size content change) = %v, want ErrChanged", err)
	}
}

func TestVerifyClassifiesTypeAndSymlinkChangesAsChanged(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "appkit.yaml"), "project")
	snapshot, err := Capture(root, []string{"appkit.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "appkit.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "appkit.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Verify(root); !errors.Is(err, ErrChanged) || !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("Verify(directory replacement) = %v, want ErrChanged and ErrInvalidPath", err)
	}

	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Remove(filepath.Join(root, "appkit.yaml")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	writeFile(t, outside, "outside")
	if err := os.Symlink(outside, filepath.Join(root, "appkit.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Verify(root); !errors.Is(err, ErrChanged) || !errors.Is(err, ErrSymlink) {
		t.Fatalf("Verify(symlink replacement) = %v, want ErrChanged and ErrSymlink", err)
	}
}

func TestVerifyRejectsZeroAndTamperedSnapshots(t *testing.T) {
	root := t.TempDir()
	if err := (Snapshot{}).Verify(root); !errors.Is(err, ErrChanged) {
		t.Fatalf("zero Verify() = %v, want ErrChanged", err)
	}

	writeFile(t, filepath.Join(root, "appkit.yaml"), "project")
	snapshot, err := Capture(root, []string{"appkit.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot.files[0].Digest = "sha256:tampered"
	if err := snapshot.Verify(root); !errors.Is(err, ErrChanged) {
		t.Fatalf("tampered Verify() = %v, want ErrChanged", err)
	}

	snapshot, err = Capture(root, []string{"appkit.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot.files[0].Mode |= os.ModeDir
	snapshot.digest = digestFiles(snapshot.files)
	if err := snapshot.Verify(root); !errors.Is(err, ErrChanged) {
		t.Fatalf("non-canonical mode Verify() = %v, want ErrChanged", err)
	}
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

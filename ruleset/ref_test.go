package ruleset

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

const refTestSHA = "0123456789abcdef0123456789abcdef01234567"

func TestWorkflowBuildProvenanceBelongsToAppKit(t *testing.T) {
	for _, tc := range []struct {
		module, revision string
		found, invalid   bool
	}{
		{appkitModule, refTestSHA, true, false},
		{"example.com/consumer", refTestSHA, false, false},
		{"", refTestSHA, false, false},
		{appkitModule, "main", true, true},
		{appkitModule, "", false, false},
	} {
		info := &debug.BuildInfo{Main: debug.Module{Path: tc.module}}
		if tc.revision != "" {
			info.Settings = []debug.BuildSetting{{Key: "vcs.revision", Value: tc.revision}}
		}
		ref, found, err := appkitBuildWorkflowRef(info)
		if found != tc.found || (err != nil) != tc.invalid || (found && err == nil && ref != refTestSHA) {
			t.Fatalf("provenance %+v: %q %t %v", tc, ref, found, err)
		}
	}
}

func TestDevelWorkflowRefRejectsDownstreamHEAD(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git required")
	}
	for _, tc := range []struct{ committed, current string }{
		{"example.com/consumer", "example.com/consumer"},
		{"example.com/consumer", appkitModule},
		{appkitModule, "example.com/consumer"},
		{appkitModule, appkitModule},
	} {
		t.Run(tc.committed+"-"+tc.current, func(t *testing.T) {
			root := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			git("init", "-q")
			writeRefFile(t, filepath.Join(root, "go.mod"), "module "+tc.committed+"\n")
			git("add", "go.mod")
			git("-c", "user.name=AppKit Test", "-c", "user.email=test@example.invalid", "-c", "core.hooksPath=/dev/null", "commit", "--no-gpg-sign", "-qm", "fixture")
			want := git("rev-parse", "HEAD")
			writeRefFile(t, filepath.Join(root, "go.mod"), "module "+tc.current+"\n")
			t.Chdir(root)
			ref, err := develWorkflowRef(context.Background())
			valid := tc.committed == appkitModule && tc.current == appkitModule
			if valid && (err != nil || ref != want) {
				t.Fatalf("AppKit worktree provenance: %q %v", ref, err)
			}
			if !valid && (err == nil || !strings.Contains(err.Error(), "拒绝使用下游 HEAD")) {
				t.Fatalf("accepted consumer HEAD: %q %v", ref, err)
			}
		})
	}
}

func TestWorkflowDownloadIsolatedAndCancelable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake Go command uses a Unix shell")
	}
	root, bin := t.TempDir(), t.TempDir()
	t.Chdir(root)
	writeRefFile(t, filepath.Join(root, "go.mod"), "module example.com/consumer\n")
	writeRefFile(t, filepath.Join(root, "go.sum"), "unchanged\n")
	log := filepath.Join(t.TempDir(), "working-dir")
	t.Setenv("APPKIT_REF_TEST_LOG", log)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOWORK", "/must/not/read/go.work")
	t.Setenv("GOFLAGS", "-modfile=/must/not/write/go.mod")
	fake := filepath.Join(bin, "go")
	writeRefFile(t, fake, `#!/bin/sh
test "$GOWORK" = off && test -z "$GOFLAGS" || exit 91
test "$*" = 'mod download -json github.com/forgeplex/appkit@v1.2.3' || exit 92
pwd > "$APPKIT_REF_TEST_LOG"
printf 'scratch only\n' > go.sum
printf '{"Origin":{"Hash":"`+refTestSHA+`"}}\n'
`)
	if err := os.Chmod(fake, 0o700); err != nil {
		t.Fatal(err)
	}
	ref, err := ResolveWorkflowRefContext(context.Background(), "v1.2.3")
	if err != nil || ref != refTestSHA {
		t.Fatalf("isolated download: %q %v", ref, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil || string(data) != "unchanged\n" {
		t.Fatalf("resolver mutated caller's go.sum: %q %v", data, err)
	}
	data, err = os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	scratch := strings.TrimSpace(string(data))
	if scratch == root {
		t.Fatal("download ran in caller directory")
	}
	if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch directory not cleaned: %s %v", scratch, err)
	}
	writeRefFile(t, fake, "#!/bin/sh\nexec sleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := ResolveWorkflowRefContext(ctx, "v1.2.3"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation identity lost: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("download did not honor cancellation")
	}
}

func TestWorkflowRefRejectsInvalidAndCanceledWithoutCommands(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, invalid := range []string{"", "main", "v1.2.3", strings.Repeat("f", 39)} {
		if _, err := NormalizeWorkflowRef(invalid); err == nil {
			t.Fatalf("accepted mutable/invalid ref %q", invalid)
		}
	}
	if ref, err := NormalizeWorkflowRef(strings.ToUpper(refTestSHA)); err != nil || ref != refTestSHA {
		t.Fatalf("normalize explicit ref: %q %v", ref, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, version := range []string{"(devel)", "v1.2.3"} {
		if _, err := ResolveWorkflowRefContext(ctx, version); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled resolution %s: %v", version, err)
		}
	}
}

func writeRefFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

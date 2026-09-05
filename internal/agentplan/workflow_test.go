package agentplan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/forgeplex/appkit/internal/scaffold"
	"github.com/forgeplex/appkit/internal/workspace"
)

func TestPinnedWorkflowPlansAreOfflineAndDigestBound(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // Any accidental git/go command must fail.
	root := domain(t)
	first, err := SyncPinned(t.Context(), root, "v1.2.3", testWorkflowRef)
	if err != nil {
		t.Fatal(err)
	}
	second, err := SyncPinned(t.Context(), root, "v1.2.3", "1123456789abcdef0123456789abcdef01234567")
	if err != nil || second.Digest() == first.Digest() {
		t.Fatalf("workflow source not bound to plan digest: %v", err)
	}
	for _, ref := range []string{"", "main"} {
		if _, err := SyncPinned(t.Context(), root, "v1.2.3", ref); err == nil {
			t.Fatalf("invalid pinned ref %q accepted", ref)
		}
	}
	if _, err := New(t.Context(), root, "generated", "domain", scaffold.Options{Name: "sample", AppkitVersion: "v1.2.3", WorkflowRef: testWorkflowRef}); err != nil {
		t.Fatalf("explicit domain ref must not resolve externally: %v", err)
	}
	if _, err := New(t.Context(), root, "system", "system", scaffold.Options{Name: "sample", AppkitVersion: "v1.2.3"}); err != nil {
		t.Fatalf("system must not resolve unused workflow: %v", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 {
		t.Fatalf("planning changed target: %v %v", entries, err)
	}
}

func TestWorkflowResolutionDoesNotHoldWorkspaceLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake resolver uses a Unix shell")
	}
	for _, operation := range []string{"sync", "new"} {
		t.Run(operation, func(t *testing.T) {
			root, bin := domain(t), t.TempDir()
			ready := filepath.Join(t.TempDir(), "resolving")
			t.Setenv("APPKIT_PLAN_REF_READY", ready)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\nprintf ready > \"$APPKIT_PLAN_REF_READY\"\nexec sleep 30\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				if operation == "sync" {
					_, err = Sync(ctx, root, "v1.2.3")
				} else {
					_, err = New(ctx, root, "generated", "domain", scaffold.Options{Name: "sample", AppkitVersion: "v1.2.3"})
				}
				done <- err
			}()
			deadline := time.Now().Add(3 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("resolver did not start")
				}
				time.Sleep(5 * time.Millisecond)
			}
			lockCtx, release := context.WithTimeout(context.Background(), time.Second)
			defer release()
			if err := workspace.WithReadLock(lockCtx, root, func() error { return nil }); err != nil {
				t.Fatalf("workspace lock held during source resolution: %v", err)
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("resolution cancellation identity lost: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("resolution ignored cancellation")
			}
			if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 {
				t.Fatalf("canceled plan changed target: %v %v", entries, err)
			}
		})
	}
}

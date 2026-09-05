package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadLockBlocksApplyUntilReaderFinishes(t *testing.T) {
	root := t.TempDir()
	plan, err := BuildPlan(root, []Change{{
		Path: "target", Operation: OperationCreate, Content: []byte("value"), Mode: 0o600,
	}})
	if err != nil {
		t.Fatal(err)
	}
	readerEntered := make(chan struct{})
	releaseReader := make(chan struct{})
	readerDone := make(chan error, 1)
	go func() {
		readerDone <- WithReadLock(context.Background(), root, func() error {
			close(readerEntered)
			<-releaseReader
			return nil
		})
	}()
	select {
	case <-readerEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not acquire its lock")
	}
	applyDone := make(chan error, 1)
	go func() {
		_, applyErr := Apply(context.Background(), root, plan)
		applyDone <- applyErr
	}()
	select {
	case err := <-applyDone:
		t.Fatalf("apply passed an active reader: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseReader)
	if err := <-readerDone; err != nil {
		t.Fatal(err)
	}
	if err := <-applyDone; err != nil {
		t.Fatal(err)
	}
}

func TestReadLockRejectsCrashPartialTransactionBeforeCallback(t *testing.T) {
	root := t.TempDir()
	active := transactionDirectoryName(
		"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"00000000000000000000000000000000",
	)
	if err := os.Mkdir(filepath.Join(root, active), 0o700); err != nil {
		t.Fatal(err)
	}
	called := false
	err := WithReadLock(context.Background(), root, func() error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrRecovery) || called {
		t.Fatalf("WithReadLock() = %v, callback called %v", err, called)
	}
}

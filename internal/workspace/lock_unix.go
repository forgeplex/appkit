//go:build darwin || linux

package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"
)

const hostProcessLockSupported = true

const lockPollInterval = 10 * time.Millisecond

// acquireHostProcessLock locks the opened workspace directory inode itself.
// This avoids a removable sidecar lock file and gives aliases of the same
// directory one lock identity. The pathname handle is accepted only when it
// still names the exact directory held by os.Root, before and after locking.
func acquireHostProcessLock(ctx context.Context, root *os.Root) (func() error, error) {
	return acquireHostProcessFlock(ctx, root, syscall.LOCK_EX, "apply")
}

func acquireHostProcessReadLock(ctx context.Context, root *os.Root) (func() error, error) {
	return acquireHostProcessFlock(ctx, root, syscall.LOCK_SH, "read")
}

func acquireHostProcessFlock(ctx context.Context, root *os.Root, mode int, operation string) (func() error, error) {
	openedRoot, err := root.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("workspace: inspect root for %s lock: %w", operation, err)
	}
	if !openedRoot.IsDir() {
		return nil, fmt.Errorf("%w: opened root is not a directory", ErrLockUnsupported)
	}

	directory, err := os.Open(root.Name())
	if err != nil {
		return nil, fmt.Errorf("workspace: open root for %s lock: %w", operation, err)
	}
	closeOnError := func(primary error) (func() error, error) {
		return nil, errors.Join(primary, closeLockDirectory(directory))
	}
	directoryInfo, err := directory.Stat()
	if err != nil {
		return closeOnError(fmt.Errorf("workspace: inspect %s lock handle: %w", operation, err))
	}
	pathInfo, err := os.Lstat(root.Name())
	if err != nil {
		return closeOnError(fmt.Errorf("%w: workspace root changed before apply lock: %w", ErrChanged, err))
	}
	if pathInfo.Mode()&fs.ModeSymlink != 0 ||
		!directoryInfo.IsDir() ||
		!os.SameFile(openedRoot, directoryInfo) ||
		!os.SameFile(openedRoot, pathInfo) {
		return closeOnError(fmt.Errorf("%w: workspace root changed before apply lock", ErrChanged))
	}

	for {
		if err := ctx.Err(); err != nil {
			return closeOnError(fmt.Errorf("workspace: wait for host apply lock: %w", err))
		}
		err = syscall.Flock(int(directory.Fd()), mode|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			return closeOnError(fmt.Errorf("%w: acquire workspace %s lock: %w", ErrLockUnsupported, operation, err))
		}
		timer := time.NewTimer(lockPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return closeOnError(fmt.Errorf("workspace: wait for host apply lock: %w", ctx.Err()))
		case <-timer.C:
		}
	}

	lockedRoot, rootErr := root.Stat(".")
	lockedDirectory, directoryErr := directory.Stat()
	lockedPath, pathErr := os.Lstat(root.Name())
	if rootErr != nil || directoryErr != nil || pathErr != nil ||
		(pathErr == nil && lockedPath.Mode()&fs.ModeSymlink != 0) ||
		(rootErr == nil && !os.SameFile(openedRoot, lockedRoot)) ||
		(directoryErr == nil && !os.SameFile(openedRoot, lockedDirectory)) ||
		(pathErr == nil && !os.SameFile(openedRoot, lockedPath)) {
		primary := fmt.Errorf("%w: workspace root changed while acquiring apply lock", ErrChanged)
		if rootErr != nil {
			primary = errors.Join(primary, rootErr)
		}
		if directoryErr != nil {
			primary = errors.Join(primary, directoryErr)
		}
		if pathErr != nil {
			primary = errors.Join(primary, pathErr)
		}
		unlockErr := syscall.Flock(int(directory.Fd()), syscall.LOCK_UN)
		return closeOnError(errors.Join(primary, unlockErr))
	}

	return func() error {
		unlockErr := syscall.Flock(int(directory.Fd()), syscall.LOCK_UN)
		closeErr := closeLockDirectory(directory)
		if unlockErr != nil {
			unlockErr = fmt.Errorf("workspace: unlock workspace directory: %w", unlockErr)
		}
		return errors.Join(unlockErr, closeErr)
	}, nil
}

func closeLockDirectory(directory *os.File) error {
	if err := directory.Close(); err != nil {
		return fmt.Errorf("workspace: close apply lock handle: %w", err)
	}
	return nil
}

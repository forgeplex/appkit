package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// WithReadLock runs inspect while cooperative workspace writers are excluded.
// Before calling inspect it rejects an active transaction, so framework
// readers never consume a crash-partial target set. Completed transaction
// debris is safe because its directory rename is the durable commit decision.
//
// The callback should finish promptly and must not call Apply or recursively
// acquire another workspace lock for the same root. Writers outside the appkit
// locking protocol remain outside this guarantee.
func WithReadLock(ctx context.Context, root string, inspect func() error) (resultErr error) {
	if ctx == nil {
		panic("workspace: nil read context")
	}
	if inspect == nil {
		return fmt.Errorf("workspace: nil read callback")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("workspace: read: %w", err)
	}
	rooted, err := openWorkspaceRoot(root)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := rooted.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("workspace: close read root: %w", closeErr))
		}
	}()

	unlock, err := acquireReadLock(ctx, rooted)
	if err != nil {
		return err
	}
	defer func() {
		if unlockErr := unlock(); unlockErr != nil {
			resultErr = errors.Join(resultErr, unlockErr)
		}
	}()
	if err := verifyOpenedRootPath(rooted); err != nil {
		return err
	}
	if err := rejectActiveTransactions(ctx, rooted); err != nil {
		return err
	}
	if err := inspect(); err != nil {
		return err
	}
	if err := verifyOpenedRootPath(rooted); err != nil {
		return err
	}
	return nil
}

func rejectActiveTransactions(ctx context.Context, root *os.Root) error {
	entries, err := readRootDirectory(root)
	if err != nil {
		return fmt.Errorf("%w: list workspace transactions: %w", ErrRecovery, err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("workspace: read: %w", err)
		}
		if strings.HasPrefix(entry.Name(), transactionPrefix) {
			return fmt.Errorf("%w: active transaction %q must be recovered before reading", ErrRecovery, entry.Name())
		}
	}
	return nil
}

func acquireReadLock(ctx context.Context, root *os.Root) (func() error, error) {
	localUnlock, err := acquireProcessLock(ctx, root.Name())
	if err != nil {
		return nil, err
	}
	hostUnlock, err := acquireHostProcessReadLock(ctx, root)
	if err != nil {
		localUnlock()
		return nil, err
	}
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			releaseErr = hostUnlock()
			localUnlock()
		})
		return releaseErr
	}, nil
}

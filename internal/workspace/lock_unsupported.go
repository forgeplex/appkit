//go:build !darwin && !linux

package workspace

import (
	"context"
	"fmt"
	"os"
)

const hostProcessLockSupported = false

func acquireHostProcessLock(context.Context, *os.Root) (func() error, error) {
	return nil, fmt.Errorf("%w: supported hosts are darwin and linux", ErrLockUnsupported)
}

func acquireHostProcessReadLock(context.Context, *os.Root) (func() error, error) {
	return nil, fmt.Errorf("%w: supported hosts are darwin and linux", ErrLockUnsupported)
}

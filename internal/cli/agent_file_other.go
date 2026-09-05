//go:build !darwin && !linux

package cli

import "os"

func openPlanFile(name string) (*os.File, error) {
	// A later Apply fails closed with ErrLockUnsupported on these platforms.
	return os.Open(name)
}

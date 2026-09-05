//go:build darwin || linux

package cli

import (
	"os"
	"syscall"
)

// Nonblocking open prevents a concurrently substituted FIFO from hanging before
// fstat. The caller verifies regular type and identity before reading bytes.
func openPlanFile(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}

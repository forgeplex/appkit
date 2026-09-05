//go:build darwin || linux

package cli

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/forgeplex/appkit/internal/workspace"
)

func TestPlanFileRejectsFIFOWithoutBlocking(t *testing.T) {
	name := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(name, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPlanFile(name); !errors.Is(err, workspace.ErrInvalidPlanDocument) {
		t.Fatalf("FIFO not rejected: %v", err)
	}
}

//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestVerifyAccessDirectoryIdentityRejectsReplacement(t *testing.T) {
	root := t.TempDir()
	trusted := filepath.Join(root, "trusted")
	if err := os.Mkdir(trusted, 0o700); err != nil {
		t.Fatal(err)
	}
	var expected unix.Stat_t
	if err := unix.Lstat(trusted, &expected); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(trusted, filepath.Join(root, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(trusted, 0o700); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(trusted, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := verifyAccessDirectoryIdentity(fd, &expected, trusted); err == nil || !strings.Contains(err.Error(), "校验后被替换") {
		t.Fatalf("replacement directory identity was accepted: %v", err)
	}
}

func TestVerifyAccessDirectoryIdentityAcceptsSameDirectory(t *testing.T) {
	dir := t.TempDir()
	var expected unix.Stat_t
	if err := unix.Lstat(dir, &expected); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := verifyAccessDirectoryIdentity(fd, &expected, dir); err != nil {
		t.Fatalf("unchanged directory rejected: %v", err)
	}
}

//go:build darwin || linux

package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func publishAccessSQL(requestedParent, targetName string, data []byte) error {
	parent, err := validateAccessOutputDirectory(requestedParent)
	if err != nil {
		return err
	}
	dirfd, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("打开生成 SQL 目录 %s: %w", parent, err)
	}
	defer unix.Close(dirfd)

	if err := ensureAccessTargetAbsent(dirfd, filepath.Join(parent, targetName), targetName); err != nil {
		return err
	}
	tempName, file, err := createAccessTempFile(dirfd, targetName)
	if err != nil {
		return err
	}
	tempExists := true
	defer func() {
		_ = file.Close()
		if tempExists {
			_ = unix.Unlinkat(dirfd, tempName, 0)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("写入生成 SQL %s: %w", targetName, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("同步生成 SQL %s: %w", targetName, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭生成 SQL %s: %w", targetName, err)
	}
	if err := unix.Linkat(dirfd, tempName, dirfd, targetName, 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("拒绝覆盖已有文件 %s；权限变化必须生成新的追加 migration", filepath.Join(parent, targetName))
		}
		return fmt.Errorf("原子发布生成 SQL %s: %w", filepath.Join(parent, targetName), err)
	}

	// Linkat 成功即是发布提交点。之后只做 best-effort 清理与目录持久化，
	// 不再返回错误，避免调用方看到失败但最终 migration 已经可见。
	_ = unix.Unlinkat(dirfd, tempName, 0)
	tempExists = false
	_ = unix.Fsync(dirfd)
	return nil
}

func ensureAccessTargetAbsent(dirfd int, displayPath, targetName string) error {
	var stat unix.Stat_t
	err := unix.Fstatat(dirfd, targetName, &stat, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case err == nil && stat.Mode&unix.S_IFMT == unix.S_IFLNK:
		return fmt.Errorf("拒绝通过符号链接写入生成 SQL %s", displayPath)
	case err == nil:
		return fmt.Errorf("拒绝覆盖已有文件 %s；权限变化必须生成新的追加 migration", displayPath)
	case errors.Is(err, unix.ENOENT):
		return nil
	default:
		return fmt.Errorf("检查生成 SQL 目标 %s: %w", displayPath, err)
	}
}

func createAccessTempFile(dirfd int, targetName string) (string, *os.File, error) {
	for range 10 {
		var nonce [12]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", nil, fmt.Errorf("生成 SQL 临时文件名: %w", err)
		}
		name := "." + targetName + ".tmp-" + hex.EncodeToString(nonce[:])
		fd, err := unix.Openat(dirfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("创建生成 SQL 临时文件: %w", err)
		}
		return name, os.NewFile(uintptr(fd), name), nil
	}
	return "", nil, errors.New("创建生成 SQL 临时文件: 唯一文件名冲突")
}

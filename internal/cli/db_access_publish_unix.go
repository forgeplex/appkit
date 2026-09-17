//go:build darwin || linux

package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func publishAccessSQL(requestedParent, targetName string, data []byte) error {
	dirfd, parent, err := openValidatedAccessDirectory(requestedParent)
	if err != nil {
		return err
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

func openValidatedAccessDirectory(path string) (int, string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return -1, "", fmt.Errorf("解析生成 SQL 目录 %s: %w", path, err)
	}
	var expected unix.Stat_t
	if err := unix.Lstat(abs, &expected); err != nil {
		return -1, "", fmt.Errorf("检查生成 SQL 目录 %s: %w（目录必须预先存在，命令不会代建或改变权限）", abs, err)
	}
	if expected.Mode&unix.S_IFMT == unix.S_IFLNK {
		return -1, "", fmt.Errorf("拒绝通过符号链接目录写入生成 SQL: %s", abs)
	}
	if expected.Mode&unix.S_IFMT != unix.S_IFDIR {
		return -1, "", fmt.Errorf("生成 SQL 的父路径 %s 不是目录", abs)
	}
	parent, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return -1, "", fmt.Errorf("解析生成 SQL 目录真实路径 %s: %w", path, err)
	}
	dirfd, err := openAccessDirectory(parent)
	if err != nil {
		return -1, "", fmt.Errorf("打开生成 SQL 目录 %s: %w", parent, err)
	}
	if err := verifyAccessDirectoryIdentity(dirfd, &expected, parent); err != nil {
		_ = unix.Close(dirfd)
		return -1, "", err
	}
	return dirfd, parent, nil
}

// openAccessDirectory walks the canonical absolute path from a root directory
// descriptor. O_NOFOLLOW rejects a symlink introduced into the canonical path;
// the caller separately verifies that the final fd is the object observed
// before canonicalization, closing ordinary-directory replacement races.
func openAccessDirectory(path string) (int, error) {
	current, err := unix.Open(string(os.PathSeparator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	for _, component := range strings.Split(strings.TrimPrefix(path, string(os.PathSeparator)), string(os.PathSeparator)) {
		if component == "" {
			continue
		}
		next, err := unix.Openat(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		_ = unix.Close(current)
		if err != nil {
			return -1, err
		}
		current = next
	}
	return current, nil
}

func verifyAccessDirectoryIdentity(dirfd int, expected *unix.Stat_t, path string) error {
	var actual unix.Stat_t
	if err := unix.Fstat(dirfd, &actual); err != nil {
		return fmt.Errorf("复核生成 SQL 目录 %s: %w", path, err)
	}
	if actual.Dev != expected.Dev || actual.Ino != expected.Ino {
		return fmt.Errorf("拒绝写入校验后被替换的生成 SQL 目录: %s", path)
	}
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

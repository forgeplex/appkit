//go:build !darwin && !linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func publishAccessSQL(requestedParent, targetName string, data []byte) error {
	parent, err := validateAccessOutputDirectory(requestedParent)
	if err != nil {
		return err
	}
	target := filepath.Join(parent, targetName)
	f, err := os.CreateTemp(parent, "."+targetName+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建生成 SQL 临时文件: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("写入生成 SQL %s: %w", target, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("同步生成 SQL %s: %w", target, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭生成 SQL %s: %w", target, err)
	}
	if err := os.Link(f.Name(), target); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("拒绝覆盖已有文件 %s；权限变化必须生成新的追加 migration", target)
		}
		return fmt.Errorf("原子发布生成 SQL %s: %w", target, err)
	}
	return nil
}

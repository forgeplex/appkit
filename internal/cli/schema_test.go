package cli

// schema 对分区域域的门控：-check（共享 CI 步骤）必须跳过——分区域域永远
// 不可能产出 schema 文档，硬失败等于所有分区域域 CI 永红；非 -check 仍
// 明确拒绝，把用户指向 introspect。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePartitionedRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	yml := "version: 1\ndomain: rbac\nmodule: github.com/forgeplex/rbac\npartitioned: true\n"
	if err := os.WriteFile(filepath.Join(dir, ".appkit.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSchemaCheckSkipsPartitioned(t *testing.T) {
	dir := writePartitionedRepo(t)
	if err := runSchema([]string{"-dir", dir, "-check"}); err != nil {
		t.Fatalf("分区域域的 schema -check 应跳过，得到: %v", err)
	}
}

func TestSchemaPartitionedStillRefused(t *testing.T) {
	dir := writePartitionedRepo(t)
	err := runSchema([]string{"-dir", dir})
	if err == nil || !strings.Contains(err.Error(), "分区域域") {
		t.Fatalf("非 -check 应明确拒绝分区域域，得到: %v", err)
	}
}

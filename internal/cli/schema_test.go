package cli

// Unadopted repositories keep the shared-CI opt-in notice. Once adopted,
// partitioned repositories reconstruct and check the logical template normally.

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

func TestSchemaCheckSkipsUnadoptedPartitioned(t *testing.T) {
	t.Setenv("TEST_DATABASE_URL", "")
	dir := writePartitionedRepo(t)
	if err := runSchema([]string{"-dir", dir, "-check"}); err != nil {
		t.Fatalf("未启用的分区域域 schema -check 应跳过，得到: %v", err)
	}
}

func TestSchemaPartitionedRequiresDSN(t *testing.T) {
	t.Setenv("TEST_DATABASE_URL", "")
	dir := writePartitionedRepo(t)
	err := runSchema([]string{"-dir", dir})
	if err == nil || !strings.Contains(err.Error(), "连接串") {
		t.Fatalf("分区域域现在支持生成，但应要求连接串，得到: %v", err)
	}
}

func TestSchemaAdoptedPartitionedIsNotSkipped(t *testing.T) {
	t.Setenv("TEST_DATABASE_URL", "")
	dir := writePartitionedRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "db", "SCHEMA.md"), []byte("adopted"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runSchema([]string{"-dir", dir, "-check"})
	if err == nil || !strings.Contains(err.Error(), "连接串") {
		t.Fatalf("已启用的分区文档不能跳过，应继续检查并要求 DSN: %v", err)
	}
}

func TestSchemaModeAndArguments(t *testing.T) {
	t.Setenv("TEST_DATABASE_URL", "")
	dir := writePartitionedRepo(t)
	for _, flags := range [][]string{
		{"-mode", "unknown"}, {"-mode", "schema"}, {"-timeout", "0"}, {"extra"},
	} {
		if err := runSchema(append([]string{"-dir", dir, "-check"}, flags...)); err == nil {
			t.Errorf("invalid arguments accepted: %v", flags)
		}
	}
	if err := runSchema([]string{"-dir", dir, "-check", "-mode", "logical-template"}); err != nil {
		t.Fatalf("explicit matching mode should reach unadopted notice: %v", err)
	}
}

func TestSchemaPartitionedGenerateAndCheckPostgres(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置")
	}
	dir := writePartitionedRepo(t)
	migrationDir := filepath.Join(dir, "db", "migrations")
	if err := os.MkdirAll(migrationDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migrationDir, "0001.sql"), []byte("CREATE TABLE probe (id bigint PRIMARY KEY); COMMENT ON TABLE probe IS 'fixture';"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"-dir", dir, "-dsn", dsn, "-mode", "logical-template"}
	if err := runSchema(args); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "db", "SCHEMA.md"))
	if err != nil || !strings.Contains(string(data), "logical-template") {
		t.Fatalf("missing logical-template mode: %v", err)
	}
	if err := runSchema(append(args, "-check")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "db", "SCHEMA.md"), []byte("drift"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runSchema(append(args, "-check")); err == nil {
		t.Fatal("adopted partitioned drift silently skipped")
	}
}

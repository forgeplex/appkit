package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/forgeplex/appkit"
)

// TestAppOptionsSkipMigrationsOverridesBootstrapMigrator 覆盖完整装配路径：
// bootstrap 会统一注入 pgmigrate.Runner，服务副本再通过 AppOptions 声明
// 迁移由外部 Job 施加。这里故意登记非法 SQL；若 SkipMigrations 失效，
// 应用会在到达 ready gate 前报迁移错误。
func TestAppOptionsSkipMigrationsOverridesBootstrapMigrator(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过需要 Postgres 的测试")
	}
	t.Setenv("BPSKIP_DATABASE__URL", dsn)
	t.Setenv("BPSKIP_ADDR", "127.0.0.1:0")

	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Service: "bpskip",
			Modules: func(Deps) ([]appkit.Module, error) {
				return []appkit.Module{appkit.ModuleFunc("invalid-migration", func(reg *appkit.Registry) error {
					reg.Migrations("bootstrap_skip_migrations", fstest.MapFS{
						"0001_invalid.sql": &fstest.MapFile{Data: []byte("THIS IS NOT SQL")},
					})
					reg.OnStart(appkit.StageServer+1, func(context.Context) error {
						close(ready)
						return nil
					})
					return nil
				})}, nil
			},
			AppOptions: func(Deps) []appkit.Option {
				return []appkit.Option{appkit.SkipMigrations()}
			},
		}, RunOptions{ConfigFile: filepath.Join(t.TempDir(), "absent.yaml")})
	}()

	select {
	case <-ready:
		cancel()
	case err := <-done:
		cancel()
		t.Fatalf("SkipMigrations 未覆盖 bootstrap 注入的 Migrator: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("等待应用启动超时")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run 返回错误: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待应用关停超时")
	}
}

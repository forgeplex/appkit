package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/config"
	"github.com/forgeplex/appkit/pgtx"
)

type noopModule struct{}

func (noopModule) Name() string                      { return "noop" }
func (noopModule) Register(_ *appkit.Registry) error { return nil }

// TestDebugPprofConfigMapping 锁住配置到框架开关的映射：BPTEST_DEBUG__PPROF=true
// 必须落到 Base.Debug.Pprof。koanf 标签写错这类事编译器看不见，端点会
// 「配了但没挂上」地静默失效——这正是「配了个摆设」那类坑。
func TestDebugPprofConfigMapping(t *testing.T) {
	t.Setenv("BPTEST_DEBUG__PPROF", "true")
	base, err := config.Load[Base](config.Options{EnvPrefix: "BPTEST", Optional: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !base.Debug.Pprof {
		t.Fatal("BPTEST_DEBUG__PPROF=true 未映射到 Base.Debug.Pprof——检查 koanf 标签")
	}
}

// TestPoolOptionsReachProductionPool 验证 Options.PoolOptions 真的接到
// bootstrap 建的生产池上：AfterConnect 钩子在真实路径被调用。域要给池装
// otelpgx tracer 或会话级 GUC 时用这个注入口；此前只能整个自建池绕开
// bootstrap（迁移/outbox/幂等装配全部重写一遍）。
func TestPoolOptionsReachProductionPool(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过需要 Postgres 的测试")
	}
	// EnvPrefix 默认 TOUPPER(Service)。
	t.Setenv("BPTEST_DATABASE__URL", dsn)

	var hooked atomic.Bool
	err := Run(context.Background(), Options{
		Service: "bptest",
		Modules: func(d Deps) ([]appkit.Module, error) {
			// 摸一下池，逼出一条真实连接（pgxpool 默认惰性建连）。
			if _, err := d.Pool.Exec(context.Background(), "SELECT 1"); err != nil {
				return nil, err
			}
			return []appkit.Module{noopModule{}}, nil
		},
		PoolOptions: []pgtx.PoolOption{
			pgtx.WithAfterConnect(func(context.Context, *pgx.Conn) error {
				hooked.Store(true)
				return nil
			}),
		},
	}, RunOptions{
		MigrateOnly: true,
		ConfigFile:  filepath.Join(t.TempDir(), "不存在.yaml"), // Optional 跳过
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !hooked.Load() {
		t.Fatal("AfterConnect 钩子未被调用——PoolOptions 没接到 bootstrap 建的池上")
	}
}

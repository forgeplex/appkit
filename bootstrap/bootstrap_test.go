package bootstrap

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestSplitTargetRejectsImplicitDirectBus(t *testing.T) {
	err := Run(context.Background(), Options{
		Service: "bpsplit",
		Modules: func(Deps) ([]appkit.Module, error) { return nil, nil },
	}, RunOptions{Target: "gateway", ConfigFile: filepath.Join(t.TempDir(), "absent.yaml")})
	if err == nil || !strings.Contains(err.Error(), "禁止隐式使用进程内 DirectBus") {
		t.Fatalf("拆分 target 应在连接数据库前拒绝默认 DirectBus，实际 %v", err)
	}
}

func TestSplitTargetDirectBusRequiresExplicitOptIn(t *testing.T) {
	options := Options{
		Service: "bpsplitallow",
		Modules: func(Deps) ([]appkit.Module, error) { return nil, nil },
	}
	options.AllowDirectBusForSplit = true
	err := Run(context.Background(), options,
		RunOptions{Target: "gateway", ConfigFile: filepath.Join(t.TempDir(), "absent.yaml")})
	if err == nil || !strings.Contains(err.Error(), "未配置 database.url") {
		t.Fatalf("显式 opt-in 应越过 Bus 守卫并继续正常校验，实际 %v", err)
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

// TestAuthnIssuerRequired 配置了验签公钥但缺 iss 约束必须在启动期报错：
// 没有 iss 的验签会接受任何持私钥者签的令牌（含同密钥的其他系统令牌），
// 这是静默放宽而不是可缺省项。报错点在 Minimal/数据库逻辑之前，
// 最小模式同样覆盖。
func TestAuthnIssuerRequired(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	err = Run(context.Background(), Options{
		Service: "bpauthn",
		Modules: func(Deps) ([]appkit.Module, error) { return nil, nil },
		// Minimal 也给出：报错点在数据库分支之前，最小模式同样触发。
		Minimal:        func(Deps) ([]appkit.Module, error) { return nil, nil },
		AuthnPublicKey: pub,
	}, RunOptions{ConfigFile: filepath.Join(t.TempDir(), "absent.yaml")})
	if err == nil || !strings.Contains(err.Error(), "AuthnIssuer") {
		t.Fatalf("缺 AuthnIssuer 应启动报错并指名字段，实际 %v", err)
	}
}

func TestMissingDatabaseDoesNotImplicitlyEnterMinimal(t *testing.T) {
	minimalCalled := false
	err := Run(context.Background(), Options{
		Service: "bpfailclosed",
		Modules: func(Deps) ([]appkit.Module, error) { return nil, nil },
		Minimal: func(Deps) ([]appkit.Module, error) {
			minimalCalled = true
			return nil, nil
		},
	}, RunOptions{ConfigFile: filepath.Join(t.TempDir(), "absent.yaml")})
	if err == nil || !strings.Contains(err.Error(), "未配置 database.url") ||
		!strings.Contains(err.Error(), "-minimal") {
		t.Fatalf("缺数据库应 fail closed 并给出本地最小模式提示，实际 %v", err)
	}
	if minimalCalled {
		t.Fatal("仅提供 Options.Minimal 不应授权隐式降级")
	}
}

func TestExplicitMinimalStartsOnlyInDev(t *testing.T) {
	t.Setenv("BPMINIMAL_ADDR", "127.0.0.1:0")
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Service: "bpminimal",
			Modules: func(Deps) ([]appkit.Module, error) { return nil, nil },
			Minimal: func(d Deps) ([]appkit.Module, error) {
				if !d.IsMinimal() || d.Pool != nil || d.Bus != nil {
					return nil, fmt.Errorf("最小模式依赖不为空")
				}
				return []appkit.Module{appkit.ModuleFunc("minimal", func(reg *appkit.Registry) error {
					reg.OnStart(appkit.StageServer+1, func(context.Context) error {
						close(ready)
						return nil
					})
					return nil
				})}, nil
			},
		}, RunOptions{
			Minimal:    true,
			ConfigFile: filepath.Join(t.TempDir(), "absent.yaml"),
		})
	}()

	select {
	case <-ready:
		cancel()
	case err := <-done:
		cancel()
		t.Fatalf("显式 dev 最小模式启动失败: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("等待最小模式启动超时")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run 返回错误: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待最小模式关停超时")
	}
}

func TestExplicitMinimalRejectedOutsideDev(t *testing.T) {
	for _, env := range []string{"staging", "prod"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv("BPMINIMALENV_ENV", env)
			err := Run(context.Background(), Options{
				Service: "bpminimalenv",
				Modules: func(Deps) ([]appkit.Module, error) { return nil, nil },
				Minimal: func(Deps) ([]appkit.Module, error) { return nil, nil },
			}, RunOptions{Minimal: true, ConfigFile: filepath.Join(t.TempDir(), "absent.yaml")})
			if err == nil || !strings.Contains(err.Error(), "-minimal 仅允许 env=dev") {
				t.Fatalf("env=%s 应拒绝最小模式，实际 %v", env, err)
			}
		})
	}
}

func TestMinimalAndMigrateAreMutuallyExclusive(t *testing.T) {
	err := Run(context.Background(), Options{
		Service: "bpminimalmigrate",
		Modules: func(Deps) ([]appkit.Module, error) { return nil, nil },
		Minimal: func(Deps) ([]appkit.Module, error) { return nil, nil },
	}, RunOptions{
		Minimal:     true,
		MigrateOnly: true,
		ConfigFile:  filepath.Join(t.TempDir(), "absent.yaml"),
	})
	if err == nil || !strings.Contains(err.Error(), "-minimal 与 -migrate 不能同时使用") {
		t.Fatalf("冲突模式应 fail-fast，实际 %v", err)
	}
}

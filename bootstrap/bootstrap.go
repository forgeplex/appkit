// Package bootstrap 把域服务 main() 的固定装配收进框架。
//
// 域服务的启动顺序是死的：加载配置 → 初始化遥测 → 建连接池 → 建事件总线 →
// 装配模块 → 运行 → 反序关停。这段仪式每个域仓库一字不差，却最容易被改坏，
// 且改坏了框架无从知情——少一个 tel.Shutdown 就丢 span，pool.Close 提前一步
// 就在关停期查询失败，忘了 Migrator 就静默不跑迁移。
//
// 收进这里之后它位于 module cache（0444 只读），改不动也不必读。
// 用户仓库的 main 只声明"装哪些模块"，见 Options。
package bootstrap

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/config"
	"github.com/forgeplex/appkit/httpserver"
	"github.com/forgeplex/appkit/outbox"
	"github.com/forgeplex/appkit/pgmigrate"
	"github.com/forgeplex/appkit/pgtx"
	"github.com/forgeplex/appkit/telemetry"
)

// telShutdownTimeout 是遥测 flush 的独立预算：主流程此时已结束，
// 只等待剩余 span/metric 发出。
const telShutdownTimeout = 5 * time.Second

// EventBus 同时是 relay 的投递出口（outbox.Bus）与消费者的订阅入口
// （appkit.Subscriber）。DirectBus 是进程内实现；拆分部署换 NATS/Kafka
// 适配器，实现这两个接口即可，业务代码不变。
type EventBus interface {
	outbox.Bus
	appkit.Subscriber
}

// Database 是数据库配置。
type Database struct {
	// URL 形如 postgres://user:pass@host:5432/db?sslmode=disable。
	// 留空 = 最小模式（只起探针与 Options.Minimal 声明的模块），
	// 便于零依赖试跑；生产部署必须配置。
	URL string `koanf:"url"`
}

// Log 是日志配置。
type Log struct {
	Level  string `koanf:"level" validate:"omitempty,oneof=debug info warn error"`
	Format string `koanf:"format" validate:"omitempty,oneof=json text"`
}

// Base 是每个域服务共有的配置项。需要额外配置项时，用 Deps.Config
// 再 config.Load 一次自己的结构——同一份文件与环境变量，不必重复声明这些。
type Base struct {
	Addr     string   `koanf:"addr"`
	Env      string   `koanf:"env" validate:"omitempty,oneof=dev staging prod"`
	Database Database `koanf:"database"`
	Log      Log      `koanf:"log"`
}

// Deps 是框架已备好的依赖，传给 Options 的各回调。
type Deps struct {
	// Log 是已接入 trace 的服务日志器。
	Log *slog.Logger
	// Base 是已加载并填过默认值的框架配置。
	Base Base
	// Pool 是数据库连接池；最小模式下为 nil。
	Pool *pgxpool.Pool
	// Bus 是事件总线；最小模式下为 nil。
	Bus EventBus
	// Config 复现框架加载配置时的入参。需要额外配置项时：
	//   cfg, err := config.Load[myConfig](d.Config)
	Config config.Options
}

// IsMinimal 报告当前是否为最小模式（未配置 database.url）。
func (d Deps) IsMinimal() bool { return d.Pool == nil }

// Options 声明一个域服务的装配。除 Service 与 Modules 外均可留空。
type Options struct {
	// Service 是服务名：遥测 service.name 与环境变量前缀的来源。必填。
	Service string
	// EnvPrefix 覆盖环境变量前缀，默认 strings.ToUpper(Service)。
	EnvPrefix string
	// DefaultAddr 覆盖默认监听地址，默认 ":8080"。
	DefaultAddr string
	// Modules 声明本进程可装配的模块全集（实际启用集由 -target 决定）。
	// 完整模式下 Deps.Pool 与 Deps.Bus 非 nil。必填。
	Modules func(Deps) ([]appkit.Module, error)
	// Minimal 声明未配置 database.url 时装配的模块（通常挂个 ping 证明服务能起）。
	// 为 nil 时未配置数据库直接报错。此时 Deps.Pool 与 Deps.Bus 为 nil。
	Minimal func(Deps) ([]appkit.Module, error)
	// AppOptions 追加 appkit.Option，典型用途是 appkit.Remote 绑定外域契约。
	AppOptions func(Deps) []appkit.Option
	// NewBus 覆盖默认事件总线（默认 outbox.NewDirectBus()）。
	NewBus func() EventBus
}

// RunOptions 是运行期参数，对应 Main 解析出的命令行 flag。
type RunOptions struct {
	// Target 是本地实例化的模块集："all" 或逗号分隔的模块名。
	Target string
	// ConfigFile 是配置文件路径，可缺省（此时只读环境变量）。
	ConfigFile string
	// MigrateOnly 为 true 时只应用迁移然后退出，不监听端口、不跑后台任务。
	MigrateOnly bool
}

// Main 是域服务 main() 的全部内容：解析 flag、装配、运行；
// 失败时打印到 stderr 并以退出码 1 结束。
//
// 需要自定义 flag 或注入 ctx 时改用 Run。
func Main(o Options) {
	var r RunOptions
	flag.StringVar(&r.Target, "target", "all",
		"本地实例化的模块集：all 或逗号分隔的模块名")
	flag.StringVar(&r.ConfigFile, "config", "config/dev.yaml", "配置文件路径（可缺省）")
	flag.BoolVar(&r.MigrateOnly, "migrate", false,
		"只应用数据库迁移然后退出（K8s initContainer / Job 用）")
	flag.Parse()

	if err := Run(context.Background(), o, r); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Run 执行装配与运行，直到收到关停信号或运行期错误；
// r.MigrateOnly 为 true 时应用完迁移即返回。
func Run(ctx context.Context, o Options, r RunOptions) error {
	if o.Service == "" {
		return fmt.Errorf("bootstrap: Options.Service 不能为空")
	}
	if o.Modules == nil {
		return fmt.Errorf("bootstrap: %s: Options.Modules 不能为 nil", o.Service)
	}

	copts := config.Options{
		Files:     []string{r.ConfigFile},
		EnvPrefix: cmp.Or(o.EnvPrefix, strings.ToUpper(o.Service)),
		Optional:  true,
	}
	base, err := config.Load[Base](copts)
	if err != nil {
		return fmt.Errorf("%s: 加载配置: %w", o.Service, err)
	}
	base.Addr = cmp.Or(base.Addr, o.DefaultAddr, ":8080")
	base.Env = cmp.Or(base.Env, "dev")

	tel, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName: o.Service,
		Env:         base.Env,
		LogLevel:    base.Log.Level,
		LogFormat:   base.Log.Format,
	})
	if err != nil {
		return fmt.Errorf("%s: 初始化遥测: %w", o.Service, err)
	}
	// 遥测最后关：关停钩子产生的 span/metric 也要被 flush。
	// 用 WithoutCancel 派生——收到信号时 ctx 已取消，flush 仍须发出。
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), telShutdownTimeout)
		defer cancel()
		if err := tel.Shutdown(sctx); err != nil {
			tel.Logger.Error(o.Service+": 遥测关停失败", "err", err)
		}
	}()

	d := Deps{Log: tel.Logger, Base: base, Config: copts}
	opts := []appkit.Option{
		appkit.Target(r.Target),
		appkit.HTTPAddr(base.Addr),
		appkit.Logger(d.Log),
		appkit.Middleware(httpserver.Base(d.Log)...),
	}

	var modules []appkit.Module
	if base.Database.URL == "" {
		// 迁移模式没有数据库可迁，明确报错而不是"最小模式跑完什么也没做"地成功退出——
		// 那会让 initContainer 绿着、服务对着空库起来。
		if r.MigrateOnly {
			return fmt.Errorf("%s: -migrate 需要 database.url（配置文件的 database.url 或环境变量 %s_DATABASE__URL）",
				o.Service, copts.EnvPrefix)
		}
		if o.Minimal == nil {
			return fmt.Errorf("%s: 未配置 database.url（配置文件的 database.url 或环境变量 %s_DATABASE__URL）",
				o.Service, copts.EnvPrefix)
		}
		d.Log.Warn(o.Service + ": 未配置 database.url，最小模式启动：" +
			"仅探针与 Minimal 声明的路由，数据面（迁移/outbox/幂等/审计）未启用")
		if modules, err = o.Minimal(d); err != nil {
			return fmt.Errorf("%s: 装配最小模式模块: %w", o.Service, err)
		}
	} else {
		pool, perr := pgtx.NewPool(ctx, base.Database.URL)
		if perr != nil {
			return fmt.Errorf("%s: 连接数据库（本地开发可先 make dev-db）: %w", o.Service, perr)
		}
		defer pool.Close()
		d.Pool = pool
		d.Bus = newBus(o)

		if modules, err = o.Modules(d); err != nil {
			return fmt.Errorf("%s: 装配模块: %w", o.Service, err)
		}
		opts = append(opts,
			appkit.Migrator(pgmigrate.Runner(pool)),
			appkit.Bus(d.Bus))
	}
	if o.AppOptions != nil {
		opts = append(opts, o.AppOptions(d)...)
	}
	app := appkit.New(modules, opts...)
	if r.MigrateOnly {
		return app.Migrate(ctx)
	}
	return app.Run(ctx)
}

func newBus(o Options) EventBus {
	if o.NewBus != nil {
		return o.NewBus()
	}
	return outbox.NewDirectBus()
}

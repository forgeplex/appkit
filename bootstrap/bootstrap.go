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
	"crypto/ed25519"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/authn"
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
	// 留空时默认拒绝启动；本地零依赖试跑必须显式传 -minimal。
	// 生产部署必须配置。
	URL string `koanf:"url"`
}

// Log 是日志配置。
type Log struct {
	Level  string `koanf:"level" validate:"omitempty,oneof=debug info warn error"`
	Format string `koanf:"format" validate:"omitempty,oneof=json text"`
}

// Debug 是排障端点配置。全部默认关闭：pprof 暴露进程内部信息
// （goroutine 栈、堆采样可能含敏感数据），开启前先确认服务端口
// 不暴露公网或网络策略已挡好。
// 放配置而不是代码开关：线上排障改 configmap 重启即生效，不必发版。
type Debug struct {
	// Pprof 为 true 时挂 /debug/pprof/*（语义见 appkit.Pprof）。
	Pprof bool `koanf:"pprof"`
}

// httpSecurityConfig 是 HTTP 身份边界配置。它故意不放进公开的 Base：给
// 已导出结构体增加字段会破坏使用无键字面量的调用方。mode 无默认值，同一
// 镜像以不同 target 部署时必须由运行环境显式选择。
type httpSecurityConfig struct {
	Mode    appkit.SecurityMode    `koanf:"mode"`
	Service *serviceVerifierConfig `koanf:"service"`
}

type runtimeSecurityConfig struct {
	Security httpSecurityConfig `koanf:"security"`
}

// Base 是每个域服务共有的配置项。需要额外配置项时，用 Deps.Config
// 再 config.Load 一次自己的结构——同一份文件与环境变量，不必重复声明这些。
type Base struct {
	Addr     string   `koanf:"addr"`
	Env      string   `koanf:"env" validate:"omitempty,oneof=dev staging prod"`
	Database Database `koanf:"database"`
	Log      Log      `koanf:"log"`
	Debug    Debug    `koanf:"debug"`
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

// IsMinimal 报告当前是否为显式最小模式。
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
	// Minimal 声明显式 -minimal 模式装配的模块（通常挂个 ping 证明服务能起）。
	// 仅 env=dev 可启用；Deps.Pool 与 Deps.Bus 为 nil。提供此回调不会授权
	// database.url 缺失时自动降级，运行者仍必须显式传 -minimal。
	Minimal func(Deps) ([]appkit.Module, error)
	// AppOptions 追加 appkit.Option，典型用途是 appkit.Remote 绑定外域契约。
	AppOptions func(Deps) []appkit.Option
	// NewBus 覆盖默认事件总线（默认 outbox.NewDirectBus()）。
	NewBus func() EventBus
	// AllowDirectBusForSplit 明确允许 target != all 时仍使用默认 DirectBus。
	// 这只适用于调用方确认事件不会跨进程的特殊部署；默认拒绝，避免生产者侧
	// 无本地订阅者却误把拆分部署当成可工作的跨进程事件总线。
	AllowDirectBusForSplit bool
	// PoolOptions 追加连接池配置，随 bootstrap 建的生产池生效：
	// pgtx.WithAfterConnect 装 per-connection 钩子（otelpgx tracer、
	// 会话级 GUC），pgtx.WithMaxConns 调容量。留空即框架默认。
	// 此前域要给池加钩子只能整个自建池绕开 bootstrap——迁移/outbox/
	// 幂等装配全部重写一遍，正是这条例外最不该存在的地方。
	PoolOptions []pgtx.PoolOption
	// AuthnPublicKey 是访问令牌的 Ed25519 验签公钥（组合根只持公钥，
	// 私钥在鉴权提供方）。security.mode=user_facing 时必填，并在根链
	// Base 之后挂 authn.Middleware：
	// 验签结果（appkit.Actor）注入 ctx，reg.Require / appkit.Check 据此
	// 判定。disabled 模式不得同时提供；内部服务身份由独立配置负责。
	AuthnPublicKey ed25519.PublicKey
	// AuthnIssuer 是期望的令牌 iss（提供方按分区签发，如 "rbac-demo"）。
	// user_facing 模式必填，缺了启动报错——没有 iss 约束的验签
	// 会接受任何持私钥者签的令牌。
	AuthnIssuer string
}

// RunOptions 是运行期参数，对应 Main 解析出的命令行 flag。
type RunOptions struct {
	// Target 是本地实例化的模块集："all" 或逗号分隔的模块名。
	Target string
	// ConfigFile 是配置文件路径，可缺省（此时只读环境变量）。
	ConfigFile string
	// MigrateOnly 为 true 时只应用迁移然后退出，不监听端口、不跑后台任务。
	MigrateOnly bool
	// Minimal 显式启用无数据库最小模式，仅允许 env=dev。
	Minimal bool
}

// Main 是域服务 main() 的全部内容：解析 flag、装配、运行；
// 失败时打印到 stderr 并以退出码 1 结束。
//
// 需要自定义 flag 或注入 ctx 时改用 Run。
func Main(o Options) {
	MainWithSecurity(o, SecurityOptions{})
}

// MainWithSecurity 与 Main 使用相同的运行参数，额外接收由组合根构造的
// 服务验签器或多用户签发方配置。私钥不属于接收端配置；security.mode 仍须显式声明。
func MainWithSecurity(o Options, security SecurityOptions) {
	var r RunOptions
	flag.StringVar(&r.Target, "target", "all",
		"本地实例化的模块集：all 或逗号分隔的模块名")
	flag.StringVar(&r.ConfigFile, "config", "config/dev.yaml", "配置文件路径（可缺省）")
	flag.BoolVar(&r.MigrateOnly, "migrate", false,
		"只应用数据库迁移然后退出（K8s initContainer / Job 用）")
	flag.BoolVar(&r.Minimal, "minimal", false,
		"显式启用无数据库最小模式（仅 env=dev，只有探针与占位路由）")
	flag.Parse()

	if err := RunWithSecurity(context.Background(), o, r, security); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Run 执行装配与运行，直到收到关停信号或运行期错误；
// r.MigrateOnly 为 true 时应用完迁移即返回。
func Run(ctx context.Context, o Options, r RunOptions) error {
	return RunWithSecurity(ctx, o, r, SecurityOptions{})
}

// SecurityOptions 是可选的服务身份及多用户签发方装配。独立于既有 Options/Base，避免给
// 已导出的结构体增加字段而破坏无键字面量调用方。
type SecurityOptions struct {
	ServiceVerifier *authn.ServiceVerifier
	// UserIssuers 用于单进程多用户签发方/分区，与 Options.AuthnPublicKey /
	// AuthnIssuer 互斥。密钥与 Partition 均来自可信组合根，不从请求选配置。
	UserIssuers map[string]authn.Issuer
}

// RunWithSecurity 在连接数据库、模块装配或监听端口前验证安全配置。
// internal_service 要求有效 ServiceVerifier；mixed 还要求 Options 的单用户
// 签发方或 SecurityOptions.UserIssuers。没有凭证的请求只可访问显式公开路由及探针。
func RunWithSecurity(ctx context.Context, o Options, r RunOptions, security SecurityOptions) error {
	if o.Service == "" {
		return fmt.Errorf("bootstrap: Options.Service 不能为空")
	}
	if o.Modules == nil {
		return fmt.Errorf("bootstrap: %s: Options.Modules 不能为 nil", o.Service)
	}
	if !r.Minimal && !r.MigrateOnly && isSplitTarget(r.Target) && o.NewBus == nil && !o.AllowDirectBusForSplit {
		return fmt.Errorf("%s: -target=%q 是拆分部署，禁止隐式使用进程内 DirectBus；请通过 Options.NewBus 配置外部 Broker，或明确设置 AllowDirectBusForSplit", o.Service, r.Target)
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
	var securityConfig runtimeSecurityConfig
	if !r.MigrateOnly {
		securityConfig, err = config.Load[runtimeSecurityConfig](copts)
		if err != nil {
			return fmt.Errorf("%s: 加载 HTTP 安全配置: %w", o.Service, err)
		}
	}
	securityMode := securityConfig.Security.Mode
	base.Addr = cmp.Or(base.Addr, o.DefaultAddr, ":8080")
	base.Env = cmp.Or(base.Env, "dev")
	if r.Minimal && r.MigrateOnly {
		return fmt.Errorf("%s: -minimal 与 -migrate 不能同时使用", o.Service)
	}
	if r.Minimal && base.Env != "dev" {
		return fmt.Errorf("%s: -minimal 仅允许 env=dev，当前 env=%s", o.Service, base.Env)
	}
	if !r.MigrateOnly && securityConfig.Security.Service != nil {
		if security.ServiceVerifier != nil {
			return fmt.Errorf("%s: security.service 与 SecurityOptions.ServiceVerifier 不能同时配置", o.Service)
		}
		security.ServiceVerifier, err = securityConfig.Security.Service.verifier()
		if err != nil {
			return fmt.Errorf("%s: security.service 配置无效: %w", o.Service, err)
		}
	}
	if err := validateHTTPSecurityWithOptions(o, base.Env, securityMode, copts.EnvPrefix, r.MigrateOnly, security); err != nil {
		return err
	}

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
	if !r.MigrateOnly && (securityMode == appkit.SecurityUserFacing || securityMode == appkit.SecurityMixed) {
		// 验签挂在 Base 之后（内一层）：RequestID/AccessLog 在外层，
		// 401 响应也进访问日志。
		if len(security.UserIssuers) != 0 {
			opts = append(opts, appkit.Middleware(authn.MultiIssuer(security.UserIssuers)))
		} else {
			opts = append(opts, appkit.Middleware(authn.Middleware(o.AuthnPublicKey, o.AuthnIssuer)))
		}
	}
	if securityMode == appkit.SecurityInternalService || securityMode == appkit.SecurityMixed {
		// 用户验签先执行，服务验签器可检查 mixed 模式下两份已验证委托范围。
		// 迁移专用路径不挂 HTTP 中间件，允许没有服务验证器。
		if !r.MigrateOnly {
			opts = append(opts, appkit.Middleware(security.ServiceVerifier.Middleware))
		}
	}
	if base.Debug.Pprof {
		opts = append(opts, appkit.Pprof())
	}

	var modules []appkit.Module
	if r.Minimal {
		if o.Minimal == nil {
			return fmt.Errorf("%s: 指定了 -minimal，但 Options.Minimal 未声明最小模式模块", o.Service)
		}
		d.Log.Warn(o.Service + ": 显式最小模式启动：" +
			"仅探针与 Minimal 声明的路由，数据面（迁移/outbox/幂等/审计）未启用")
		if modules, err = o.Minimal(d); err != nil {
			return fmt.Errorf("%s: 装配最小模式模块: %w", o.Service, err)
		}
	} else if base.Database.URL == "" {
		// 迁移模式没有数据库可迁，明确报错而不是"最小模式跑完什么也没做"地成功退出——
		// 那会让 initContainer 绿着、服务对着空库起来。
		if r.MigrateOnly {
			return fmt.Errorf("%s: -migrate 需要 database.url（配置文件的 database.url 或环境变量 %s_DATABASE__URL）",
				o.Service, copts.EnvPrefix)
		}
		return fmt.Errorf("%s: 未配置 database.url（配置文件的 database.url 或环境变量 %s_DATABASE__URL）；"+
			"本地零依赖试跑请显式传 -minimal",
			o.Service, copts.EnvPrefix)
	} else {
		pool, perr := pgtx.NewPool(ctx, base.Database.URL, o.PoolOptions...)
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
	// 安全模式来自已经校验过的部署配置，必须最后写入；否则 AppOptions 可用
	// 另一个 Security Option 覆盖生产模式，绕过身份边界与路由分类校验。
	opts = append(opts, appkit.Security(securityMode))
	app := appkit.New(modules, opts...)
	if r.MigrateOnly {
		return app.Migrate(ctx)
	}
	return app.Run(ctx)
}

func validateHTTPSecurity(o Options, env string, mode appkit.SecurityMode, envPrefix string, migrateOnly bool) error {
	return validateHTTPSecurityWithOptions(o, env, mode, envPrefix, migrateOnly, SecurityOptions{})
}

func validateHTTPSecurityWithOptions(o Options, env string, mode appkit.SecurityMode, envPrefix string, migrateOnly bool, security SecurityOptions) error {
	// 迁移专用进程不监听 HTTP，安全模式与凭证配置均不参与。
	if migrateOnly {
		return nil
	}
	switch mode {
	case appkit.SecurityUnspecified:
		return fmt.Errorf("%s: 未配置 security.mode（配置文件的 security.mode 或环境变量 %s_SECURITY__MODE）；"+
			"必须显式选择 user_facing/internal_service/mixed，只有 env=dev 可选 disabled", o.Service, envPrefix)
	case appkit.SecurityDisabled:
		if env != "dev" {
			return fmt.Errorf("%s: security.mode=disabled 仅允许 env=dev，当前 env=%s", o.Service, env)
		}
		if len(o.AuthnPublicKey) > 0 || o.AuthnIssuer != "" {
			return fmt.Errorf("%s: security.mode=disabled 与 AuthnPublicKey/AuthnIssuer 冲突；要验证用户令牌请使用 user_facing", o.Service)
		}
		if security.ServiceVerifier != nil || len(security.UserIssuers) != 0 {
			return fmt.Errorf("%s: security.mode=disabled 与 ServiceVerifier/UserIssuers 冲突", o.Service)
		}
		return nil
	case appkit.SecurityUserFacing:
		if security.ServiceVerifier != nil {
			return fmt.Errorf("%s: security.mode=user_facing 与 ServiceVerifier 冲突；双身份入口须选择 mixed", o.Service)
		}
		return validateUserSecurity(o, mode, security.UserIssuers)
	case appkit.SecurityInternalService, appkit.SecurityMixed:
		if err := security.ServiceVerifier.Validate(); err != nil {
			return fmt.Errorf("%s: security.mode=%s 需要有效的服务身份配置（ServiceVerifier）: %w", o.Service, mode, err)
		}
		if mode == appkit.SecurityMixed {
			return validateUserSecurity(o, mode, security.UserIssuers)
		}
		if len(o.AuthnPublicKey) > 0 || o.AuthnIssuer != "" || len(security.UserIssuers) != 0 {
			return fmt.Errorf("%s: security.mode=internal_service 与用户验签配置冲突；双身份入口须选择 mixed", o.Service)
		}
		return nil
	default:
		return fmt.Errorf("%s: 未知 security.mode %q（允许 user_facing/internal_service/mixed/disabled）", o.Service, mode)
	}
}

func validateUserSecurity(o Options, mode appkit.SecurityMode, issuers map[string]authn.Issuer) error {
	if len(issuers) != 0 {
		if len(o.AuthnPublicKey) != 0 || o.AuthnIssuer != "" {
			return fmt.Errorf("%s: UserIssuers 与 AuthnPublicKey/AuthnIssuer 不能同时配置", o.Service)
		}
		for issuer, spec := range issuers {
			if strings.TrimSpace(issuer) == "" || len(spec.Key) != ed25519.PublicKeySize {
				return fmt.Errorf("%s: UserIssuers 需要非空 issuer 与有效 Ed25519 公钥", o.Service)
			}
		}
		return nil
	}
	if len(o.AuthnPublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("%s: security.mode=%s 需要 %d 字节 Ed25519 AuthnPublicKey", o.Service, mode, ed25519.PublicKeySize)
	}
	if strings.TrimSpace(o.AuthnIssuer) == "" {
		return fmt.Errorf("%s: security.mode=%s 需要 AuthnIssuer——没有 iss 约束的验签会接受其他系统令牌", o.Service, mode)
	}
	return nil
}

func isSplitTarget(target string) bool {
	target = strings.TrimSpace(target)
	return target != "" && target != "all"
}

func newBus(o Options) EventBus {
	if o.NewBus != nil {
		return o.NewBus()
	}
	return outbox.NewDirectBus()
}

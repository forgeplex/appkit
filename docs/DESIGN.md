# appkit 设计文档

> Go 后端框架，目标：任何业务域拿来即用；用工具链硬约束防止架构漂移；
> 每个业务域独立 repo，由组合 repo（如 psp）装配为完整系统，单二进制与微服务可切换。
>
> 本文档由多轮调研（go-kratos / go-zero / encore.dev / goa / go-kit / ServiceWeaver /
> Grafana Loki / uber-fx / Three Dots Labs DDD 系列 / Stripe 幂等模式等）+ 三个独立设计方案
> 交叉对抗评审后综合而成。

---

## 0. 研究结论（为什么是这个设计）

| 来源 | 吸取 | 拒绝 |
|---|---|---|
| go-kratos | biz/service/data 分层语义、repo 接口倒置 | 约束全靠自觉（官方承认模板不强制） |
| go-zero | "生成即约束"：结构由工具决定，手写空间小 | 私有 `.api` DSL（已有 OpenAPI，不再发明语言） |
| encore.dev | **逻辑架构与物理部署解耦**、service 边界由编译器强制 | 平台绑定、魔法注解 |
| goa | 生成物与手写代码物理隔离、生成物禁手改 | 又一门 DSL |
| go-kit | 业务逻辑与 transport 严格分离 | 手写样板承载约束（必然被绕过） |
| ServiceWeaver（已归档） | 组件接口 + 部署时决定本地/远程 | **隐藏网络边界**（其失败主因）；全有或全无的侵入性 |
| Grafana Loki | `-target` 模式：同一二进制按配置跑不同模块组合 | — |
| uber-go/fx | Module 化注册、value groups 聚合、lifecycle | 运行时反射 DI 的不透明性（自研轻量版替代） |
| Go 官方 | 极简布局（cmd/ + internal/）、接口放消费方、internal/ 编译器边界 | golang-standards/project-layout 的目录仪式 |

核心洞察：**约束按"不可绕过程度"排序为 编译器 > 代码生成 > 运行时守卫 > lint/CI > 约定**，
每条架构规则都要落在能落的最左层，并诚实标注它落在哪一层（见 §7）。
纪律靠工具和脚手架，不靠 code review 和文档——Uber monorepo 复盘证明统一 lint
挡不住架构漂移，必须有架构级检查；Grab 的答案是代码生成脚手架（Grab-Kit），与 appkit 定位一致。

本仓库群的现状佐证了痛点：ledger 的 `cmd/ledger-api/` 里业务逻辑、handler、metrics
全部堆在 main 包；各 repo 布局互不相同；合约还是手写 markdown。

---

## 1. 仓库拓扑（最重要的决定）

```
github.com/forgeplex/
├── appkit                # 框架：运行时 + CLI + lint 规则集 + CI 模板
├── psp-contracts         # ★ 唯一事实源：contract.yaml + 事件 schema + 错误码
│                         #   发布生成好的 Go module（接口/DTO/wrapper/client/server/事件类型/错误码常量）
├── auth                  # 业务域 repo（appkit new domain 生成）
├── ledger                # 业务域 repo
├── gateway               # 业务域 repo
├── merchant              # 业务域 repo
└── psp                   # 组合 repo：只做 wiring + 配置 + 部署，零业务逻辑
```

**依赖方向（编译器强制，go.mod 可审计）：**

```
        ┌─────────────┐      ┌──────────────────┐
        │   appkit    │      │ psp-contracts/go │   ← 零重依赖的生成 module
        └──────▲──────┘      └────────▲─────────┘
               │                      │
        ┌──────┴──────────────────────┴─────┐
        │   auth / ledger / gateway / merchant   ← 域 repo 之间【禁止互相 require】
        └──────────────────▲────────────────┘
                           │
                      ┌────┴────┐
                      │   psp   │  ← 唯一 import 各域 Module 入口的地方
                      └─────────┘
```

三条铁律（都由编译器/go.mod 层面保证）：

1. **域 repo 之间永不互相 require。** 跨域调用只依赖 `psp-contracts` 里生成的接口类型。
   （这消灭了对抗评审发现的两个致命问题：事件类型放各域导致的 tag 发版死锁 / 循环 require；
   以及"import 对方根包就把对方全部实现和依赖图链进自己二进制"的 MVS 污染。）
2. **契约（接口 + DTO + 事件 + 错误码）只有一个事实源**：psp-contracts。
   Go 类型全部由 contract.yaml / 事件 schema / 错误码注册表生成（`appkit gen`），
   杜绝"Go 接口和契约文档双源真理"漂移——OpenAPI 是从 contract.yaml 派生的
   导出物（文档与 oasdiff 门禁的输入），不是第二份手写事实。
3. **域 repo 的实现全部在 `internal/`**，对外只暴露一个 `Module()` 入口包，
   且这个入口只被 psp（和本域自己的 cmd/）import。

---

## 2. appkit 框架目录结构

```
appkit/                          # module github.com/forgeplex/appkit
├── go.mod                       # 严格 SemVer；CI 用 apidiff 做向后兼容门禁
├── appkit.go                    # ★ 稳定核心：Module / Registry / App / Provide / Resolve
│                                #   —— 只依赖 stdlib（http.Handler、fs.FS、context），
│                                #   不暴露 gin/pgx 类型，防止框架兼容性被第三方库绑架
├── run.go                       # App.Run：模块拓扑排序装配、-target 过滤、
│                                #   signal.NotifyContext、正序启动/逆序优雅关停；
│                                #   App.Migrate：只应用迁移即返回（initContainer 用）
├── worker.go                    # Registry.Worker：长驻后台任务的托管注册
│                                #   （关停等待 + 异常退出上报，见 §8）
├── permission.go                # 权限抽象（★ 根包，零第三方类型）：
│                                #   PermissionDecl 声明收集 / Actor 主体快照 /
│                                #   Require 统一判定（401/403/step-up 矩阵）——见 §5.4
├── bootstrap/                   # ★ 隔离层：域/组合服务 main() 的全部固定装配
│                                #   （配置→遥测→池→总线→模块→运行→反序关停）。
│                                #   用户仓库的 main 只声明"装哪些模块"，
│                                #   装配代码位于 module cache（0444）改不到
├── contract/                    # 跨模块契约绑定：Local/Remote 绑定、进程内拦截器链
│                                #   （ctx 防火墙、超时、错误规范化、span —— 见 §5）
├── callctx/                     # 可穿过 ctx 防火墙的元数据白名单（request/tenant/caller）
│                                #   —— 具名字段而非 map，业务代码无法往里塞东西
├── config/                      # koanf 分层加载 file→env→flag；unmarshal 强类型 +
│                                #   validator 校验，启动 fail-fast；按模块前缀分区
├── health/                      # liveness/readiness 分离注册表；
│                                #   SIGTERM → readyz 返 503 摘流量 → drain → shutdown
├── telemetry/                   # slog + otelslog + OTLP 三信号统一初始化；首启末停
├── httpserver/                  # 根 HTTP 中间件链（RequestID/Recover/AccessLog/OTel）
│                                #   标准库实现，模块接缝只用 http.Handler；
│                                #   模块内部想用 gin 组路由是模块自己的事
├── authn/                       # 验签机制件（golang-jwt，框架内唯一第三方
│                                #   依赖新增点）：EdDSA Bearer 令牌 + X-Step-Up
│                                #   证明 → 注入 appkit.Actor。判定不在这，在根包
│                                #   Require；claims 布局即提供方契约——见 §5.4
├── apperr/                      # Error{Code, HTTPStatus, Message, Details}
│                                #   RFC 9457 (problem+json) 映射；
│                                #   ★ 错误身份 = 错误码（apperr.Is(err, code)），
│                                #   保证单体/微服务两种模式下错误判定行为一致
├── money/                       # Money(decimal + currency) 领域值对象，不落库；
│                                #   NUMERIC 持久化走 decimal.Decimal（pgx 原生
│                                #   编解码，pgxmoney 已废弃）；全系统禁 float 金额
├── page/                        # 列表端点的分页机制件：?limit 解析校验（畸形 422
│                                #   不静默裁剪）、游标不透明编解码（JSON→base64url）、
│                                #   items+next_cursor 信封、limit+1 多取一行判
│                                #   下一页。机制归框架，排序键/keyset SQL 归域
├── idem/                        # 幂等：Stripe 式 claim 先行（独立事务 INSERT ON CONFLICT
│                                #   占位 in-progress，防双重执行竞态）+ 响应缓存 +
│                                #   同 key 异 payload 422 + recovery point
├── tx/                          # 事务边界的【接口面】：Transactor.Do(ctx, fn)、HasTx(ctx)
│                                #   —— 零驱动依赖，业务包只 import 这个
├── pgtx/                        # tx 的 pgx 实现：tx 藏 ctx、DBTX 取用（有 tx 用 tx 无则 pool）
│                                #   由框架装配注入，业务代码永不 import；
│                                #   分区路由（NewRouted，SET LOCAL search_path）与
│                                #   租户下沉（NewTenant，事务级 GUC + RLS）也在这——见 §5.5
├── outbox/                      # 同事务 Publish 落表、relay(SKIP LOCKED 轮询)、
│                                #   inbox 消费去重；Bus 接口可插拔：
│                                #   directbus(单库直投 inbox) / natsbus / kafkabus
├── job/                         # 跨副本互斥的周期任务：Postgres advisory lock
│                                #   （不建表、副本崩溃自动放锁），配 Registry.Worker 用
├── audit/                       # 审计钩子：写操作记录 actor/before/after，同事务落库
├── apptest/                     # 契约一致性套件：同一批用例跑过每个绑定（进程内 wrapper /
│                                #   远程 client），比对错误码、返回值、边界语义
│                                #   —— 把"两种部署形态语义一致"从承诺变成可运行断言
├── ruleset/                     # .golangci.yml / .go-arch-lint.yml 模板
│                                #   （由 appkit sync 物化进各 repo，CI 做 drift check）
├── ci/                          # GitHub Actions reusable workflows（域 repo 一行 uses:）
├── cmd/appkit/                  # CLI：
│                                #   new domain <name>   生成域 repo 骨架（-partitioned 分区
│                                #                       形态 / -tenant 租户形态，见 §5.5）
│                                #   new system <name>   生成组合 repo 骨架
│                                #   sync                物化/刷新 lint 配置与 CI 引用
│                                #   dev                 go work init/use 全部子 repo
│                                #   check               架构检查（含 SQL 跨 schema 引用扫描）
│                                #   schema              从 db/migrations 派生 schema 文档
│                                #                       与 ER 图（-check 做漂移检查）
└── internal/                    # 框架私有实现（拓扑排序、lifecycle runner、gin 装配等）
    └── metrics/                 # ★ 自动埋点的唯一定义处：指标名与标签集在框架内钉死，
                                 #   业务不参与——指标的成本在基数不在采集（见 §8）

appkit-lint/                     # ★ 独立 module：自研 go/analysis analyzers
                                 #   （独立是为了不把 x/tools 依赖污染进业务依赖图）
                                 #   规则见 §7；经 golangci-lint module plugin 或 vettool 接入
```

## 3. 契约仓库：psp-contracts

```
psp-contracts/
├── ledgerv1/                    # 每个域一个包：yaml 事实源与生成物同目录
│   ├── contract.yaml            # ★ 同步契约事实源：system + methods（path/doc/
│   │                            #   idempotent/request/response）+ 命名 DTO
│   ├── events.yaml              # 事件 schema（topic + payload 字段）
│   ├── codes.yaml               # 错误码注册表（LEDGER_INSUFFICIENT_FUNDS…）
│   ├── service.gen.go           # type Service interface + 传值 DTO
│   ├── wrap.gen.go              # 进程内 wrapper：方法体经 contract.Call（ProvideContract 闭环）
│   ├── client.gen.go            # 远程 client：同一接口 + 幂等方法有界重试（CodeUnavailable）
│   ├── server.gen.go            # HTTP 暴露：与 client 互为镜像的编解码
│   ├── events.gen.go            # 事件类型
│   ├── codes.gen.go             # 错误码常量与 sentinel
│   └── openapi.yaml             # 派生导出：文档 + oasdiff 门禁输入（非事实源，禁手改）
├── authv1/ merchantv1/ gatewayv1/ ...
├── go.mod                       # module github.com/forgeplex/psp-contracts
│                                #   零重依赖（stdlib + appkit 的 contract/apperr/callctx）
└── .github/workflows/           # 生成 drift check + oasdiff 门禁 + tag 发布
```

要点：
- 域 repo 和消费方 require 的是**生成好的 module**（版本锁定），而不是各自本地 codegen——
  杜绝各 repo 生成器版本不同导致的类型漂移。
- **方向是 contract.yaml → OpenAPI，不是反过来。** 初版设想 OpenAPI 手写为源、
  oapi-codegen 消费；落地时反转：契约的核心语义（粗粒度方法形态、幂等声明、
  错误身份 = 错误码）在 OpenAPI 里只能靠扩展字段旁挂，而 contract.yaml 与
  events/errors 同风格、校验报错带行号。openapi.yaml 退为派生导出——
  文档与 oasdiff 破坏性变更门禁消费它，但改契约永远从 contract.yaml 入手。
- 契约变更流程：改 contract.yaml → `appkit gen contract` 重新生成 →
  CI 跑 oasdiff（对 openapi.yaml 判破坏性）与 drift check（生成物未手改）→ tag。
- HTTP 形态唯一：全方法 POST + JSON body，错误一律 problem+json。
  契约调用是 RPC 语义而非 REST 资源语义，client/server 编解码因此只有一份约定。

## 4. 业务域 repo（以 ledger 为例，`appkit new domain ledger` 生成）

> **组织原则是 Go 惯用形态，不是 DDD 分层。** 包按"提供什么"命名（`ledger`、`postgres`、
> `http`），不按架构角色命名——`domain/app/adapter/port` 这类包名在 Go 里是反模式
> （包名读不出内容、必然 stutter）。多仓拓扑本身已完成 package-by-feature：
> 一个 repo 就是一个业务域；repo 内部只保留三条**方向性约束**（见下），不叠 DDD 楼阁。
> 参照：Go 官方 modules/layout、Ben Johnson Standard Package Layout。

```
ledger/                          # module github.com/forgeplex/ledger
├── go.mod                       # require: appkit、psp-contracts/go。
│                                # ★ CI 检查：require 里出现其他域 module 即失败
├── ledger.go                    # 唯一导出面：func Module() appkit.Module
│                                # （只被 psp 和本域 cmd/ import，别的域看不到也不需要看）
├── cmd/ledgerd/                 # 独立微服务部署入口：只装本模块，
│                                #   外域契约一律绑 Remote client（见 §5）
├── internal/
│   ├── ledger/                  # ★ 业务包：领域类型 + 不变量 + 业务逻辑/编排 + 所需接口
│   │   ├── account.go           #   按功能分【文件】；域真的大了再按功能拆子包
│   │   ├── posting.go  hold.go  #   （internal/account、internal/posting），而不是按层拆
│   │   ├── service.go           #   编排：tx.Do 事务边界、outbox 发布、recovery point、审计
│   │   ├── store.go             #   接口放消费方：Store 接口 + 外域窄接口（包住 contracts 类型）
│   │   └── errors.go            #   错误 = apperr + contracts 错误码
│   │                            # 允许 import：stdlib、appkit 无驱动包（tx/money/apperr/outbox
│   │                            #   接口面）、contracts 类型。禁止 pgx/gin/net/http（机检）
│   ├── postgres/                # ★ 全 repo 唯一允许 import pgx/sqlc 的包；实现 ledger 包的接口
│   │   ├── store.go
│   │   └── sqlc/                #   sqlc 生成物，禁手改，CI drift check
│   ├── http/                    # 对外 HTTP handler；契约端点直接挂 contracts 生成的
│   │                            #   NewHTTPHandler(包 WrapService 的实现)；业务路由只做
│   │                            #   DTO↔ledger 类型映射；禁业务规则/SQL（机检）
│   ├── inbox/                   # 事件消费者：去重后调 ledger 业务包（处理外域事件）
│   └── module/                  # Module 实现：Register 里装配路由/迁移/consumer/health
├── db/
│   ├── migrations/              # ★ 只允许操作 "ledger" schema；
│   │                            #   每域独占 Postgres schema + 独立迁移历史表
│   ├── queries/                 # sqlc .sql 源；appkit check 扫描跨 schema 表引用
│   ├── SCHEMA.md                # 表清单 + 按外键分簇的 Mermaid ER 图（人的入口）
│   └── schema/                  # 每表一份 .sql（DDL，给 agent/grep）+ .md（给 review）；
│                                #   _appkit/ 下隔离框架表。全部由 appkit schema 从
│                                #   migrations 派生，禁手改，CI drift check（见 §7）
├── .golangci.yml                # appkit sync 物化（带 appkit 版本头），CI 校验未漂移
├── .go-arch-lint.yml            # 方向性约束矩阵
└── .github/workflows/ci.yml     # 一行 uses: forgeplex/appkit/ci
```

三条方向性约束（等价于 DDD 分层想保证的全部东西，但包更少、名字是 Go 的）：

1. `internal/ledger` **零 infra import**——纯业务，单测不需要 mock 泛滥；
2. **pgx/sqlc 只出现在 `internal/postgres`**——"repo 层外无 SQL"；
3. `internal/http`、`internal/inbox` **不得 import `internal/postgres`**——
   transport 必须经 ledger 包的接口走业务逻辑，不能抄近路。

Go 惯例细节：`ledger.Service` 是具体 struct（accept interfaces, return structs），
http 包直接依赖它，不为它预备接口；接口只出现在真正需要第二实现/测试替身的地方
（Store、外部渠道、外域契约）。

## 5. 组合 repo（psp）与组合机制

```
psp/                             # module github.com/forgeplex/psp
├── go.mod                       # require appkit + psp-contracts/go + auth/ledger/gateway/merchant
├── cmd/psp/main.go              # ★ 唯一 composition root（见下）
├── config/                      # dev/staging/prod.yaml，按模块前缀分区
├── deploy/                      # K8s：同一镜像，不同 Deployment 传不同 -target
│   ├── monolith/                #   -target=all
│   └── services/                #   -target=gateway / -target=ledger / -target=relay …
├── Makefile                     # make dev → appkit dev（生成 go.work，进 .gitignore 不提交）
└── .github/workflows/           # 集成 CI：拉各域 tag 编译 + 双模式集成测试
```

### 5.1 核心接口（appkit 稳定面，只依赖 stdlib）

```go
// appkit.go
type Module interface {
    Name() string
    Register(reg *Registry) error
}

// Registry —— 模块向系统贡献能力（fx value groups 思想，手写实现，无反射魔法）
func Provide[T any](reg *Registry, ctor func(*Registry) (T, error)) // 注册契约实现（惰性构造）
func Resolve[T any](reg *Registry) (T, error)                       // 取依赖；启动期缺失 fail-fast、循环依赖报错
func (r *Registry) Mount(prefix string, h http.Handler)             // 路由（http.Handler，非 gin 类型）
func (r *Registry) Permissions(decls ...PermissionDecl)             // 声明本模块权限码（仅 Register 阶段，§5.4）
func (r *Registry) Require(code string, h http.Handler) http.Handler // 端点绑码（判定矩阵统一在框架，§5.4）
func (r *Registry) Migrations(schema string, fsys fs.FS)
func (r *Registry) Consumer(topic string, h outbox.Handler)
func (r *Registry) Health(name string, c health.Checker)
func (r *Registry) OnStart(stage int, fn func(context.Context) error) // OnStop 自动逆序
```

### 5.2 组装（psp/cmd/psp/main.go）

```go
app := appkit.New(cfg,
    appkit.Target(cfg.Target),                       // "all" | "gateway" | "ledger,relay" …
    appkit.Modules(
        auth.Module(), merchant.Module(),
        ledger.Module(), gateway.Module(),
    ),
    // target 之外的契约自动落到 Remote 绑定（contracts 生成的 HTTP client，实现同一接口）：
    appkit.Remote[ledgerv1.Service](ledgerv1.NewClient),
    appkit.Remote[authv1.Service](authv1.NewClient),
    appkit.Remote[merchantv1.Service](merchantv1.NewClient),
)
app.Run(ctx)
```

机制（Loki 式 target）：
- 在 target 集内的模块本地实例化并 `Provide` 自己的契约实现；
- 被 `Resolve` 但不在 target 集内的契约，appkit 自动用 `Remote` 构造器满足；
- 同一镜像在 K8s 里以不同 `-target` 部署，即得 modular monolith ↔ 微服务切换；
- 模块内部通过 `Resolve[merchantv1.Service](reg)` 取依赖——类型是公开的 contracts 类型，
  解析发生在模块自己的代码里，psp 只做 Provide/Remote/target 配置。

### 5.3 进程内调用 ≠ 裸方法调用（ServiceWeaver 的教训，反向落地）

跨模块契约调用（本地或远程）统一经过 `contract.Call`——**与远程语义对齐的拦截器链**。
拦截器的落点是合约仓库生成的 wrapper/client 的方法体：`appkit gen contract` 从
contract.yaml 生成同一接口的进程内 wrapper 与 HTTP client，方法体内都调
`contract.Call`；`appkit.ProvideContract` 在 Provide 处强制应用 wrapper，使裸实现
进不了 registry。examples/greeter 的 greetapi 就是全生成的最小实例。拦截器链：

1. **ctx 防火墙**：剥离事务（pgtx）与请求作用域值，只保留 trace、deadline、白名单元数据。
   —— 修复对抗评审发现的致命问题："ctx 隐式事务在进程内跨模块调用时静默泄漏，
   单体里跨模块共享事务默默能跑，拆成微服务当场爆炸"。
2. **运行时守卫**：若调用发生时 `pgtx.HasTx(ctx)` 为真 → 直接返回错误（事务内禁跨模块调用；
   静态分析对这条规则只能"提高绕过成本"，运行时守卫才是真正可执行的）。
3. **超时 + 错误规范化**：错误一律折叠为 apperr，错误身份是错误码——
   `apperr.Is(err, ledgerv1.CodeInsufficientFunds)` 在单体（原始错误）和
   微服务（RFC 9457 反序列化重建）两种模式下行为完全一致。发起时 ctx 已取消/
   超时的调用在进 fn 前就失败（`CodeUnavailable`）：跨网络时这种调用本来就发不
   出去，而进程内实现完全可能不看 ctx 照常成功——不挡住，两种形态就此分叉。
4. **观测**：跨模块调用产生与 RPC 同名的 span/metric，两种模式下监控视图一致。

这四条是**框架的承诺**，而承诺需要被验证：`apptest.Conform` 让同一批用例分别
跑过进程内 wrapper 与远程 client，比对错误码、返回值与上述边界语义。手写 client
漏了 `contract.Call`、两侧 DTO 的 json key 对不上、领域错误在 problem+json 往返后
换了码——这些都只在真正拆分部署的那天才暴露，除非有一条一致性测试提前把它逼出来。

跨模块一致性只有两条路：**同步契约调用**（视为可失败、须幂等）或 **outbox 事件**。
禁止：跨模块共享事务、跨 schema JOIN、传指针。

### 5.4 鉴权与权限：声明 / 绑定 / 判定 / step-up

鉴权是每个系统都要、但语义人人不同的一块。框架的切法是**机制与语义分离**：

- **语义归提供方**（如 rbac 域）：角色模型、通配匹配、令牌签发、挑战动作
  （TOTP/MFA）的验证——这些是产品决策，换一家就全换；
- **机制归框架**：声明收集、绑定校验、判定矩阵、证明验签——每个系统长得
  一样，写进 N 个域就是 N 份会漂移的样板。

这层抽象之前有两个结构性洞：端点绑码拼错要到运行时 403 才暴露（绑定字符串
不在任何校验视野里）；权限目录的完整性靠组合根记得手工全量注入，漏了启动
失败——脆弱环节。现在两者都由装配期校验收口。

**声明（仅 Register 阶段）**：`reg.Permissions(decl)` 声明本模块的权限码，
`Challenge: true` 标记高危码。声明只允许 Register 阶段（迟到 panic），因为
汇总在 Setup 期就要被消费——提供方（rbac）的 SyncCatalog 从
`reg.PermissionDecls()` 读**全应用目录**同步落库。目录完整性从「组合根记得
注入」变成「各域自声明」，组合根这个环节退役。

**绑定（Register/Setup 两阶段皆可）**：`reg.Require("files:delete", handler)`
返回带判定的 handler。模块内部 mux 通常在 Setup 期装配，所以两个阶段都收；
全部 Setup 跑完后框架统一校验**绑定 ⊆ 声明**——拼错的码在监听之前曝光
（报错点名模块与码），而不是等到运行时 403。跨模块绑码合法（gateway 绑
别域的码）。

**Actor（主体快照）**：验签中间件注入 ctx 的主体最小形态
`{UserID, TenantID, Perms []string, StepUpAt}`。判定永远是**集合包含**
（`slices.Contains(Perms, code)`）：通配等匹配语义归提供方，在**签发侧**
展开为精确码再进令牌。快照模型的代价是权限变更要等令牌过期才生效；换来
判定零远程调用、零每请求查库，且判定语义不依赖任何提供方——换 rbac 时
框架与全部业务域代码零改动。

**判定矩阵（框架统一，错误身份是错误码）**：

| 请求状态 | 响应 |
|---|---|
| 无 Actor / UserID 空（未认证） | 401 `UNAUTHENTICATED` |
| 已认证、Perms 不含该码 | 403 `PERMISSION_DENIED`（detail `required`=码） |
| 持 Challenge 码、证明缺失或超过 5 分钟 | 403 `STEP_UP_REQUIRED` |
| 其余 | 放行 |

**验签（authn 包，机制件）**：`authn.Middleware(pub, issuer)` 解析 Bearer
访问令牌与 `X-Step-Up` 证明——EdDSA 算法白名单（防算法混淆攻击）、iss、
exp 必填，全部过验后注入 Actor。无 Authorization 头原样放行（判公开与否是
Require 的职责——公开端点是合法状态）；**有头但验不过必须 401**：坏凭证是
攻击不是匿名，静默降级会掩盖问题。依赖方向单向：authn → 根包，组合根挂链
（或 bootstrap 两行配置），根包不 import authn。

**claims 布局 = 提供方契约**（照此签发即可写出第二个提供方、平替 rbac）：

```
访问令牌（Authorization: Bearer）：iss · sub（必填）· exp（必填）· tid（可选）· perms []string（精确码）
step-up  （X-Step-Up）           ：iss · sub（须与访问令牌一致）· exp · purpose="step-up" · iat（证明时刻）
```

**step-up 谁验什么**——三段分工：提供方验**挑战动作**（用户真的过了
MFA/TOTP），过了签一枚 step-up 令牌；框架 Authn 验**证明**（签名、iss、
sub 与访问令牌一致、purpose、exp），取 iat 进 `Actor.StepUpAt`；框架
Require 验**策略**（该码是否标了 Challenge、证明是否新鲜）。挑战的**消费方
是客户端**不是模块：Require 在单请求内联返回 403 `STEP_UP_REQUIRED` →
客户端引导用户完成挑战 → 带 `X-Step-Up` 头重试同一请求。无广播、无消费
注册——check 是中间件，不是事件。

拆分部署时每个服务自挂 Authn（Authorization/X-Step-Up 头本来就在线路上，
下游不靠上游「帮忙认证过了」）；Actor 不进 `callctx` 白名单，也不该进。

### 5.5 租户隔离：三种形态，机制归框架、语义归域

租户是多租户系统都要做、但不做约束就会每个域长出自己一套的一块——不是
风格漂移问题，是安全问题：漏一条租户 WHERE 是**静默跨租户泄漏**。切法与
鉴权（§5.4）同构——**机制归框架，语义归域**：

- **语义归域与 rbac/identity**：租户模型（扁平/层级）、生命周期（开通/
  冻结/注销）、目录表、配额——产品决策，框架不预设；
- **机制归框架**：租户身份的**来源唯一化**（authn 从令牌 tid 焊入
  callctx，头只是东西向传播的载体）与**存储层强制**（RLS，漏 WHERE 从
  泄漏变成查不到行/写入被拒）。

三种形态，`appkit new domain` 一次选定（互斥）：

| 形态 | 生成 flag | 隔离方式 | 适用 |
|---|---|---|---|
| plain | （默认） | 单租户域，无隔离需求 | 域本身就是独占的 |
| tenant | `-tenant` | 单 schema，行级（RLS） | 租户共享同一份数据模型，量级单库可容 |
| partitioned | `-partitioned` | schema 级（每租户一套 schema） | 强隔离/合规要求，或单 schema 撑不住量级 |

tenant 形态的机制三件套（全部在 `pgtx`，DDL 库函数是唯一事实源）：

1. **身份焊接**（authn）：验过的令牌 `tid` 覆盖或清零 callctx 里的
   TenantID——认证请求以令牌为准，「无租户令牌 + 伪造的 X-Tenant-Id」
   不能成立。头只在**未认证**入口存活（那是内部东西向的合法形态：
   Transport 注入 → Extract 读出）；边缘剥头是网关的职责，框架不越位。
2. **GUC 下沉**（`pgtx.NewTenant`）：每次 `Do` 开事务后把租户身份落成
   事务级 GUC `app.tenant_id`（`set_config(..., true)`，连接归池即净）；
   无租户则不设——基础设施表（outbox/idem/audit）的 relay、claim 不带
   租户，不能被强租户卡死。策略函数 `appkit_current_tenant()` 在 GUC
   缺失时 RAISE（42501）而不是匹配零行：事务外直查业务表响亮失败，
   「查询成功但什么都看不见」才是危险形态。
3. **RLS 三件套**（`pgtx.TenantPolicySQL`）：ENABLE + **FORCE** + 策略
   （USING/WITH CHECK 都比对 tenant_id）。FORCE 必须有：应用角色通常
   就是表主，不 FORCE 则 RLS 对它是装饰；WITH CHECK 拦住「把别家的
   tenant_id 写进去」。启动期 `pgtx.VerifyTenantRLS` 校验完整性：有
   tenant_id 列的表必须三件套齐，且连接角色不得 superuser/BYPASSRLS
   （否则 RLS 静默不生效），缺一样拒绝启动、点名表并附修复 SQL。

诚实标注的洞：未认证公开端点仍信任头值（公开端点本就不该查租户数据，
边缘剥头归网关）；superuser 绕过靠 Setup 期角色检查兜住（运行时 RLS 挡
不了 superuser，这是 Postgres 语义）；schema 文档如实渲染 RLS DDL
（策略被删/FORCE 被摘，漂移检查跟着变红），未 ENABLE 的「装饰态」点名
而非装作没有。

partitioned 与 tenant 不组合：schema 隔离已经足够，叠加行级只会得到两套
都要维护的机制。逃生舱不进 API：跨租户批处理逐租户 `callctx.With` + Do；
迁移内跨租户回填在同一文件里先回填后挂 policy（DDL 顺序自控）。

## 6. 请求完整路径（以"记一笔账"POST 为例）

| # | 层 | 做什么 | 禁止什么（执行手段） |
|---|---|---|---|
| 1 | httpserver 中间件链 | recover → otelgin(排除 healthz) → auth（调 authv1.Service 契约）→ **idem claim**（独立短事务 `INSERT…ON CONFLICT` 占位：已完成→回放缓存响应；进行中→409；同 key 异 payload→422）→ 出口统一 error 映射 RFC 9457、只 log 一次 | 业务逻辑（框架代码，业务写不进去） |
| 2 | internal/http | 实现 contracts 生成的 server 接口；解码、结构校验、DTO↔ledger 类型映射 | SQL、pgx、import internal/postgres、业务规则（depguard + arch-lint） |
| 3 | internal/ledger（service.go 编排） | `tx.Do(ctx, fn)` 开事务；调不变量；经 Store 接口读写；经消费方接口发事件（wiring 注入 outbox.Publisher，同事务落表）；recovery point；审计 | import gin/pgx（depguard）；事务内跨模块调用（**运行时守卫**） |
| 4 | internal/ledger（类型与不变量） | 纯函数/实体方法：借贷平衡、币种一致、Money 精度 | 一切 I/O、float64（arch-lint + analyzer） |
| 5 | internal/postgres | Store 实现；sqlc Querier 从 ctx 取 DBTX（有 tx 用 tx 无则 pool）；业务表 UNIQUE 约束兜底幂等 | 业务决策；跨 schema 表引用（appkit check 扫 .sql） |
| 6 | 提交后 | 业务表 + outbox + 幂等结果 + 审计同事务 commit → relay（`-target=relay` 或同进程 worker）SKIP LOCKED 拉取 → Bus（directbus/NATS/Kafka）→ 消费方 inbox 去重 → usecase | — |

## 7. 约束落地对照表（诚实分级）

> 原则：能左移就左移；标不到"不可绕过"的，诚实标为"提高绕过成本"。
> 强度记号：★ 绕不过｜▲ 提高绕过成本｜✗ 尚未落地（写在这里是为了不假装它已生效）。

| 规则 | 落点 | 强度 |
|---|---|---|
| 域 repo 互不依赖、看不到彼此实现 | 独立 module + internal/ + CI 检查 go.mod require 清单 | ★ 编译器级，不可绕过 |
| 跨域只经契约类型 | 唯一可见类型就是 contracts 生成接口 | ★ 编译器级 |
| 契约/事件/错误码单一事实源 | 只有生成物，无手写类型 | ★ 生成级 + drift check |
| http 包不写 SQL、业务包零 infra import | depguard + go-arch-lint：配置由 `appkit sync` 物化（同时解决 IDE 集成与 golangci-lint 无配置继承），CI 先验未漂移**再按它跑这两个检查器**，版本与域仓库 `make lint` 同源（`ruleset.GolangciLintVersion` / `ArchLintVersion`） | ▲ CI 级，nolint 需写理由 |
| 金额禁 float、ctx 不进 struct、decimal 不上 JSON 面 | appkit-lint 自研 analyzer（moneyfloat[-scope 圈定业务包] / ctxstruct / decjson）；domain-ci.yml 与域仓库 `make lint` 都跑它，版本随 go.mod 钉的 appkit 走（规则与依赖同版本升级，不搞 @main 惊喜；go.work 联调仓库退 @main） | ▲ CI 级：默认只查生产代码——测试夹具低一档，且存量域的测试不该被规则升级卡红（`-<name>.tests=true` 可连测试查） |
| 事务不泄漏到业务代码 | pgtx 回调式 API，业务只见 ctx | ★ API 设计级 |
| 事务内禁跨模块调用 | contract.Call 运行时守卫（HasTx 检查）；前提是调用经生成 wrapper（ProvideContract 强制包裹；生成物由 `appkit gen contract` 产出） | ▲ 运行时级，测试即暴露 |
| 忘发事件不可能 | outbox.Publish 是事务 API 的一等公民 | ★ API 设计级 |
| 生成物禁手改 | CI 重新生成后 `git diff --exit-code` | ▲ CI 级 |
| appkit/contracts 向后兼容 | apidiff / oasdiff 门禁 | ▲ CI 级 |
| CI 本身不可绕过 | reusable workflow + branch protection required checks | 组织级 |
| 启动装配改不坏 | `bootstrap.Main` 收走 main() 的固定装配，代码在 module cache（0444 只读）；用户仓库的 main 只声明模块清单 | ▲ 物理级：改不动，但可绕开自己写 main（骨架默认不绕） |
| 规则集不被改松 | `appkit check` 内联 `ruleset.Check`（配置缺失同样算漂移），不再只靠 CI 那一步 | ▲ 本地+CI 级 |
| 已应用的迁移不可变 | 历史表存内容 sha256，启动期逐个比对，不符即 `MIGRATION_DRIFT` 拒绝启动；`.gitattributes` 钉 `*.sql eol=lf` 消除跨平台误报 | ★ 运行时级，启动即暴露 |
| 分区域域的 schema 由调用方确定 | 迁移/查询无前缀 + 事务级 `SET LOCAL search_path`（`pgtx.NewRouted` 按 `callctx.Meta.TenantID` 查组合根注入的映射）；查询必须经 `tx.Do`，事务外落默认 search_path 即「表不存在」报错 | ★ 运行时级：路由失败（查无分区）即回滚 422，无法静默落到错误分区 |
| 分区域域的 SQL 无前缀纪律 | `partitioned: true` 时 archcheck 前缀规则翻转（任何 schema 前缀违规，无 DB 即拦）；sqlc 编译器兜底——带前缀与无前缀两个世界各自封闭，混写双向都是编译错误 | ▲ CI 级 + ★ 编译器级 |
| 单 schema 域跨租户泄漏 | RLS 三件套（ENABLE+FORCE+策略，`pgtx.TenantPolicySQL`）+ 事务级 GUC（`pgtx.NewTenant` 的 Do）；Setup 期 `pgtx.VerifyTenantRLS` 校验完整性（缺三件套/角色 superuser 或 BYPASSRLS 即拒启，点名表并附修复 SQL） | ★ 运行时级：漏写 WHERE 只会查不到行/写入被拒，且缺件启动即暴露 |
| 租户身份来源唯一（令牌 tid） | authn 验签后把 tid 焊进 callctx（有值覆盖、无值清零）——伪造的 X-Tenant-Id 在认证请求里活不下来 | ★ 中间件级；未认证入口仍信头（东西向合法形态），边缘剥头归网关——这是文档级边界，不是机检 |
| schema 文档支持分区域域 | 暂无：`appkit schema` 对 `partitioned: true` 明确报错（分区映射由组合根注入，域仓库无从枚举；先想清楚「按分区画还是按逻辑模型画」再补） | ✗ 尚未落地（写在这里是为了不假装它已生效） |
| 没人跑迁移不可能 | 登记了迁移却既无 `Migrator` 又无 `SkipMigrations()` → 启动报错；`-migrate` 无 `database.url` 亦报错 | ★ 装配级 fail-fast |
| schema 文档不与迁移脱节 | `appkit schema` 把 `db/migrations` 应用到一次性临时库（复用生产的迁移 runner）再读回 `pg_catalog`，产出因此是迁移的纯函数；CI 一步 `-check` 比对，缺文件/被手改/删表后的残留都算漂移。渲染不了的特性（分区、生成列、继承…）点名报错而非静默输出残缺 DDL；RLS 如实渲染成 DDL（含策略三件套，策略被删/FORCE 被摘即漂移），未 ENABLE 的装饰态点名标注 | ▲ CI 级，**有个洞**：`db/SCHEMA.md` 与 `db/schema/` 都不存在时打条 `::notice` 后放行——`domain-ci.yml` 经 `@main` 被全部存量域仓库共享，硬加检查会让它们在合并那一刻集体变红。代价是从不启用的仓库永远不被检查；跑过一次 `make schema` 就永久转严 |
| 表有说明（`COMMENT ON TABLE`） | 缺的表在 `db/SCHEMA.md` 表清单里标 ⚠ 缺说明并给出该补的那一句；`appkit schema -check` 在 CI 里逐表打 `::warning` 注解 | ▲ 软约束：不阻断 CI（刻意的，存量仓库不会突然红），且 `db/SCHEMA.md` 带 `linguist-generated` 在 PR diff 里默认折叠——⚠ 没人主动打开就看不见，::warning 注解是让它浮出水面的那一半 |
| 长驻任务死了必被发现 | `Registry.Worker` 托管：异常退出上报主循环并触发关停（不再是"探针绿着、事件停摆"） | ★ API 设计级 |
| ctx 只能传白名单元数据 | `callctx.Meta` 是具名字段的 struct 而非 map，防火墙剥值后只放回它 | ★ 编译器级：塞不进去 |
| 周期任务多副本不重跑 | `job.Every` 用 Postgres advisory lock（session 级，连接断开自动释放） | ▲ API 设计级：正确写法零成本，裸 ticker 拦不住 |
| 指标基数不失控 | 标签值只能是代码常量或 `internal/metrics` 收敛过的枚举；SQL 动词过白名单，未识别塌缩为 `other` | ▲ API 设计级：业务传不进框架指标，但自建 meter 仍可自伤 |
| 已死的 ctx 不落到实现上 | `contract.Call` 在进 fn 前查 `ctx.Err()`——跨网络时这种调用本来就发不出去 | ★ 运行时级：两种形态由构造一致 |
| 两种部署形态语义一致 | `apptest.Conform` 让同一批用例跑过每个绑定，比对错误码/返回值/边界语义 | ▲ 测试级：写了才有；但不写就只剩口头承诺 |
| 脚手架的 sqlc NUMERIC override 真能过 pgx | override 只指向 decimal.Decimal（pgx 经 Valuer/Scanner 原生编解码）；`internal/scaffold` 的 `TestDomainNumericOverrideRoundTrips` 把渲染后声明的类型真库读写一遍（27 位小数 + NULL）。编译测试对"扫描不了 OID 1700"结构性失明——money.Money 那次教训就是编译全绿、运行即炸 | ▲ 测试级：需 `TEST_DATABASE_URL`（`make test-db`），无 DB 时 skip |
| 出站 HTTP 也带上 `callctx` 白名单 | `callctx.Transport` 装进 `http.Client` 一次即可（不必逐调用点写 `Inject`）；`apptest.Conform` 填了 `Binding.SeenMeta` 就当场验请求头 | ▲ API 设计级 + 测试级：漏点从「每个调用点」收敛到「一处装配」，且验得到。但两者都得自己接——不填 `SeenMeta` 就仍是口头承诺 |
| 端点权限绑定 ⊆ 已声明码 | Registry 收集绑定（Register/Setup 期都可能产生，模块内部 mux 在 Setup 装配），全部 Setup 之后、监听之前统一校验，拼错的码点名（模块、码）报错 | ★ 装配期硬失败 |
| 权限判定语义统一（集合包含 + step-up 新鲜度） | `reg.Require` 是唯一判定入口，401/403/STEP_UP 矩阵在框架；业务域不再各写一份会漂移的判定 | ★ API 设计级 |
| 验签只在 authn 一处、模块不自解凭证 | 组合根挂链 + 脚手架注释教学 | ▲ 提高绕过成本：模块仍可自解 Authorization 头绕过，无机制挡 |
| 列表端点分页形态统一（limit 解析/游标编码/响应信封） | `page` 包：`Parse` 值域校验（畸形 422，不静默裁剪）+ 游标不透明编解码（JSON→base64url）+ `List` 信封 + `Trim` 的 limit+1 多取一行技巧；机制归框架，排序键与 keyset SQL 归域 | 约定级：用不用在域，自写手翻页无机检——形态统一靠采纳与 review |

逃生舱（防止约束被政治性推翻）：带理由的 `//nolint`（nolintlint 强制）、
go-arch-lint 的存量违规"技术债合法化"清单、跨域报表/对账走**事件驱动读模型**
（官方模式，而不是放开跨 schema JOIN）。

## 8. 数据与运维约定

- **每域一或多个 Postgres schema**（单体单库多 schema；拆分后可平移为多库），独立迁移历史表；
  连接池按数据库共享而非按模块独占（防连接数爆炸）。默认形态是每域恰好一个
  （schema 名 = 域名）；「多个」只以分区域域这一种受控形态出现，见下条。
- **分区域域（`appkit new domain <name> -partitioned`）**：一套代码、N 份数据分区，
  schema 由调用方确定。场景是 psp 的商户/运营/代理商三套独立用户体系共用一份
  rbac 域代码但数据要强隔离——体系 A 的查询根本看不到体系 B 的表，漏过滤的失败
  形态是「表不存在」而不是「数据泄露」。机制三件套：① 分区映射（租户身份 →
  schema）**定义在组合根自己的配置文件**（koanf 原生支持 `map[string]string`），
  由组合根读取后经 `module.Options.Schemas` 注入——框架不暗读全局配置；②
  `pgtx.NewRouted` 每次 `Do` 开启事务后按 `callctx.Meta.TenantID` 解析分区并
  `SET LOCAL search_path`（事务结束自动还原，连接归还池时不带走）；③ 迁移与
  查询全部无 schema 前缀，同一份无前缀 FS 经 `reg.Migrations` 注册到每个分区，
  pgmigrate 应用前同样 `SET LOCAL` 并自动建 schema——**新分区 = 组合根映射加一条
  + 重启，零代码零手写 SQL**。硬纪律：分区域的查询必须经 `tx.Do`——事务外
  `pgtx.From` 落在默认 search_path 上，无前缀表不存在即报错（失败响亮，绝不
  静默读写错误分区）。带前缀世界与无前缀世界各自封闭：混写形态在 sqlc 编译
  与 `appkit check` 双向都是硬错误，跨域访问的静态保证从 archcheck 前缀扫描
  **转移**为 sqlc 编译解析，强度不降级。已知的洞：`appkit schema` 暂不支持
  该形态（明确报错而非产出误导内容，见 §7）。
- **租户域（`appkit new domain <name> -tenant`）**：单 schema、行级隔离，
  机制与边界见 §5.5。选型对照：租户共享同一份数据模型、量级单库可容 →
  tenant；强隔离/合规或单 schema 撑不住量级 → partitioned；域本身独占 →
  默认。三种形态一次选定、互斥生成。
- **迁移的施加时机是部署决策**：默认随进程启动（多副本经 advisory lock 串行，安全但
  N 个副本同时改 schema）；规模上来后改为 initContainer/Job 里跑 `<svc> -migrate`
  （只应用迁移即退出，不监听端口），服务副本再以 `appkit.SkipMigrations()` 起来。
  两条路径用的是同一份模块声明，不存在"迁移清单和服务清单不是同一份"的漂移。
  已应用的迁移内容不可变（sha256 守卫），改结构一律新增文件。
- **outbox/inbox/幂等/审计表每 schema 一套**，由脚手架的首个 migration 生成。
- **relay 投递语义**：claim/lease 两段式（短事务租约后释放连接再投递，杜绝
  hold-and-wait 连接池死锁）；至少一次投递 + inbox 按 (consumer, event_id) 去重；
  失败指数退避、超过重试上限进死信（failed_at），毒消息不阻塞后续事件；
  批内保序尽力而为，跨批/退避后不保证全局序。
  死信有恢复通道：`outbox.DeadLetters`（`appkit outbox` 子命令即其运维面）
  列出失败原因、修好后按事件 ID 放回——attempts 归零、立即到期、按完整
  重试预算重走；放回只动死信态的行，未死或已发布的事件不受影响。
- **幂等 claim 带 fencing token**：TTL 接管后旧持有者的 Complete/Release 必然失败，
  不存在双写窗口；TTL 必须大于 handler 最长执行时间。
- **幂等指纹与键作用域可注入**（`idem.WithCanonicalizer` / `idem.WithKeyScope`，
  纯新增选项，默认行为不变）：默认指纹绑定原始字节，金额 "80" 与 "80.00"
  的重试会被判异 payload 而 422——入站 DTO 走 `money.ParseCanonical` 的域把
  同款口径接进中间件后，等值形态重试即回放，中间件层与业务层两套指纹不再
  各算各的。多租户/多账本用作用域键隔离键空间：存储键 = scope + 0x1f + key，
  键与作用域先过控制字节校验，跨作用域碰撞不可拼造（手拼 `{tenant}:` 前缀
  做不到这点）。换规范化口径是单向门：存量 payload_hash 全部失配而 completed
  记录不过期，旧键持续 422 到客户端换键为止。
- **Bus 可插拔**：单体单库用 directbus（relay 直投各域 inbox，无 broker）；
  拆分部署切 NATS/Kafka——同一 Bus 接口，部署形态切换不改业务代码。
  模块经 `reg.Consumer(topic, handler)` 声明消费（handler 用 outbox.Inbox 包去重），
  App 以 `appkit.Bus(...)` 装配；声明了消费者却未配 Bus 属启动错误（fail-fast）。
- **业务层发事件不碰 infra 类型**：wiring 期构造 `outbox.NewPublisher(pool, schema)`，
  业务包按"接口放消费方"依赖单方法接口 `Publish(ctx, evt) error`。
- **长驻任务一律经 `reg.Worker(name, run)`**：框架起 goroutine、关停等它退出、异常退出
  上报主循环并触发关停。自己起 goroutine 的三种典型写错（关停不等、关停预算耗尽不放手、
  崩了没人管）都收在这一处。周期任务再套 `job.Every(pool, job.Task{...})` 拿跨副本互斥。
- **可观测性自动就位，业务不写埋点**：契约调用、outbox 投递与死信、周期任务、数据库查询
  四条路径由框架产出 RED 指标（HTTP 入站由 otelhttp 出，不重复埋）；outbox 积压深度与
  最老待投递年龄以 gauge 观测（告警看年龄而不是条数）。标签集在 `internal/metrics` 钉死，
  业务无法追加维度——指标事故几乎都源于"顺手加一个标签"。
- **跨边界元数据走 `callctx` 白名单**：ctx 防火墙剥掉一切值，只有 request/tenant/caller
  三个具名字段被放回。HTTP 入站（中间件）与事件 meta（outbox/relay）两条由框架自动
  接好，异步链路也串得起 request id；**出站 HTTP 那一段要自己接**：把
  `callctx.Transport` 装进 `http.Client`（一处），而不是逐调用点写 `Inject`。
  appkit 不生成 client，但装没装上验得到：给 `apptest.Conform` 的 Binding 填
  `SeenMeta`，服务端交出「这次看到了什么」，漏了的形态当场红——见 §7 表末。
- **本地开发**：`appkit dev` 自动 `go work init/use`（go.work 不提交）；
  发版联动用 Renovate 自动升 require。私有库配 `GOPRIVATE=github.com/forgeplex/*`。
- **对账（reconciliation）是 PSP 一等公民**：appkit 提供 recovery point 表约定与
  对账 worker 骨架（比较 gateway 侧与 ledger 侧状态、外部渠道对账文件），不是运维脚本。

## 9. 风险与代价（诚实评估）

1. **appkit 是全系统单点**。MVS 依赖钻石要求 appkit 近乎永久向后兼容；核心稳定面
   只依赖 stdlib 正是为此（gin/pgx 换代不波及 Module 契约）。仍需一个实质"框架 owner"。
2. **多 repo 税是结构性的**：改一个跨域功能 = contracts、域 repo、psp 三处 PR + tag。
   go.work 只救本地不救发版。appkit 能减摩（dev/sync/Renovate/reusable CI），不能消除。
3. **双模式语义漂移压不死**：接口一致 ≠ 语义一致。ctx 防火墙、运行时守卫、拦截器对齐
   能压制大头，但"依赖低延迟的细碎调用"要靠 CI 定期以拆分模式跑集成测试 + review 文化。
4. **约束维护税**：analyzer 误报、golangci-lint 大版本升级、生成器上游演进都由 appkit
   吸收。前 6 个月需要高频修规则，否则团队对约束体系的信任会流失。
5. **仪式成本**：简单 CRUD 也要走三层 + 两套 codegen。这是刻意选择——脚手架把仪式的
   编写成本压到接近零，换取五年后代码仍然长在同一个形状上。

## 10. 落地顺序与状态

1. ✅ **appkit M0**（2026-08 已实现）：appkit.go（Module/Registry/Provide/Resolve）+ run.go +
   config + httpserver + apperr + telemetry + health + contract 边界。
2. ✅ **数据面**（2026-08 已实现）：tx/pgtx + money(+pgxmoney) + pgmigrate + outbox + idem。
   示例见 examples/greeter（双模式组合演示）。
3. ✅ **契约生成（2026-09 补全 client/server/openapi）**：`appkit gen contract`
   （contract.yaml → service.gen.go 接口与 DTO + wrap.gen.go + client.gen.go
   远程 client 含幂等有界重试 + server.gen.go + openapi.yaml 派生导出）、
   `appkit gen events|errors`（yaml → 事件类型/错误码常量）+ `ProvideContract` 闭环。
   生成物过 apptest.Conform 双绑定的证明常住于 internal/gen/genfixture；
   examples/greeter/greetapi 是全生成契约包的最小实例。剩余部分属于
   psp-contracts 仓库自身（建仓、搬入各域合约、oasdiff 门禁、tag 发布）。
4. ✅ **CLI + ruleset + lint**（2026-08 已实现）：
   - `appkit new domain|system`（域骨架 24 文件，基础迁移由 outbox/idem/audit.MigrationSQL
     运行期拼装——库函数是 DDL 唯一事实源；生成仓库可离线完整编译且通过自身 check/sync）
   - `appkit dev`（go.work 联调）、`appkit sync [--check]`（物化 golangci-lint v2 /
     go-arch-lint v3 配置 + CI 引用，带生成头与漂移检测）
   - `appkit check`（go.mod 域间依赖铁律、import 方向矩阵、SQL 跨 schema 扫描、迁移编号、
     规则集漂移）
   - `appkit schema [-check]`（迁移 → 一次性临时库 → `pg_catalog` → `db/SCHEMA.md` +
     `db/schema/`：表清单、按外键连通分量分簇的 Mermaid ER 图、每表 DDL/表格/一跳邻域图。
     `appkit check` 保持纯静态不碰 DB，所以这是独立命令 + 独立 CI 步骤）
   - `audit` 包（同事务审计）、`ruleset` 包、`.github/workflows/domain-ci.yml`（reusable）
   - `lint/` 嵌套 module：`moneyfloat`（金额禁浮点，-scope 圈定业务包）、`ctxstruct`
     两个 go/analysis analyzer + `appkit-lint` multichecker
5. ✅ **隔离与运维面**（2026-08 已实现）：把"每个仓库一字不差、却最容易被改坏"的部分
   全部收进框架，用户仓库只留声明：
   - `bootstrap`（main() 的全部装配，位于只读 module cache）、`Registry.Worker`
     （长驻任务托管）、`App.Migrate` + `-migrate` + `SkipMigrations`
   - `pgmigrate` 迁移内容 sha256 不可变守卫（配 `.gitattributes` 的 `*.sql eol=lf`）
   - `callctx`（穿越 ctx 防火墙的元数据白名单，HTTP 头 ↔ ctx ↔ 事件 meta 三向传播）
   - `job`（advisory lock 跨副本互斥的周期任务）
   - `internal/metrics`（四条路径的 RED 指标 + outbox 积压 gauge，标签集框架内钉死）
   - `apptest.Conform`（契约一致性套件：同一批用例过每个绑定，比对错误码/返回值/
     边界语义）+ `contract.Call` 在进 fn 前拦掉已死的 ctx——**§5.3 的四件套至此
     既是承诺也是可运行的断言**
6. ⬜ **试点迁移 ledger**（已有最清晰的数据层：sqlc + outbox 已在用），验证约束体系。
7. ⬜ **psp 组合 repo**：先 `-target=all` 单体上线，拆分是之后的部署决策而非架构决策。

# ADR-0047：应用宿主与流式契约

- 状态：Accepted（Phase 0 决策；由 PR [#49](https://github.com/forgeplex/appkit/pull/49) 于 2026-09-23 合并生效）
- 日期：2026-09-23
- 关联：[#47 扩展 AppKit 应用宿主与流式契约](https://github.com/forgeplex/appkit/issues/47)、[#48 Phase 0](https://github.com/forgeplex/appkit/issues/48)
- 基线：`main@7f798494f70d9e30a1035a2f22ce1154381ffd7b`，release `v0.9.5`

本文冻结语义决策，不冻结候选 Go API 的最终名称和签名。实现子 Issue 应依据合并后的 ADR 定义公开类型；若实现发现语义冲突，先修订 ADR，再调整代码。

## 背景

AppKit 的 `App.Run` 当前把 HTTP Listener 和显式 HTTP SecurityMode 作为运行前提；`bootstrap.RunWithSecurity` 的完整 Profile 还装配 PostgreSQL 和 Bus。`Worker` 与 `ManagedSubscriber` 已覆盖部分长驻任务生命周期，`contract.Call` 则定义了 Unary 跨模块边界。Wen 的长事件流、CLI、Slack Socket、Relay 和 Approval 需要宿主级生命周期与流式边界，但 Agent、Session、Provider 选择、回放和领域终态仍属于 Wen。

AppKit 公开 API 经过 MVS 被下游共享。新增能力必须是纯新增；根包和 `tx` 不引入第三方类型；`contract.Call` 的事务守卫、Context Firewall、超时、错误规范化和观测保持不变。

## 决策

### 1. 生命周期 API 与实例状态

1. `App.Run(ctx)` 继续是阻塞式进程入口：它处理 SIGINT/SIGTERM，父 Context 取消、HTTP Serve 故障或受管关键任务异常会触发关停。正常收到信号或父 Context 取消本身不作为运行错误返回。
2. 后续新增的 `App.Start(ctx)` 是嵌入式 API，不注册 OS Signal Handler。传入 Context 同时用于同步启动阶段，并代表宿主的运行期；其取消触发一次优雅关停。`Start` 仅在 Host 达到 Ready 后返回 Running Handle。
3. `Wait` 可多次及并发调用；所有调用返回同一运行终态错误。显式 `Shutdown(ctx)` 与父 Context 取消竞争时，共用同一个关停协调器；首次关停原因保留，后续运行错误与清理错误通过 `errors.Join` 保留可检查的错误链。`Shutdown` 的 Context 作为本次等待/关停预算，预算耗尽后转入强制关闭并继续清理。
4. 一个 `App` 是单次使用实例。`Run`、`Start`、`Migrate` 中任一入口开始后都消费该实例；重复调用、在 `Migrate` 后对同一实例 `Start/Run`、或启动失败后重试均 fail-fast。调用方要运行服务和迁移时，应建立独立 App 实例，或使用框架明确提供的组合入口。
5. Host 状态为：

   ```text
   New → Registering → Resolving → Migrating → SettingUp
       → Starting → Ready → Draining → Stopping → Stopped
   ```

   启动失败经 `Stopping → Stopped`；迁移专用入口只走 `New → Registering → Migrating → Stopped`，不 Resolve、不 Setup、不启动 Service、不监听。Ready 之前不对外报告就绪。

### 2. Drain、取消和资源清理

受管 Service 按 `Start resource → launch Run → Ready` 启动；关停先撤销 readiness 并关闭新工作入口，再依启动逆序执行 Service Drain，之后取消 Service Run Context、等待 Run 退出，最后 Close。Run Context 在 Drain 期间保持有效。Drain、Wait、Close 使用有总预算的关停 Context；一个阶段超时不能跳过其他资源清理。

Service `Start` 一旦被调用，无论返回成功还是部分失败，都视为可能持有资源并需要调用一次 `Close`。尚未启动的 Run 不等待。Service 在 Host 进入 Draining 之前意外退出时，默认 Critical；显式声明为 Optional 的 Service 退出只记录状态和错误，不自动重启。普通 Worker 的当前行为保持不变，不将其静默改成该 Service 语义。

当前 `App.Run` 会在调用 `shutdown` 前取消普通 `runCtx`；`ManagedSubscriber` 用独立 `busCtx` 保证 Bus 可在 Drain 时继续处理在途消息。新 Service 语义要求在 Draining 阶段继续使用有效的 Service Run Context；抽取 Host Kernel 时必须保留旧 Worker 兼容行为。

### 3. Host Profile 与 Signal 所有权

- Headless：没有业务入站 Listener，不要求 HTTP SecurityMode；仍有程序化 readiness 和 telemetry。声明 HTTP route 或 pprof 时启动 fail-fast。
- Probe-only：仅开放健康/就绪 Listener，不等同于 Headless。新 Probe Listener 默认绑定 loopback；非 loopback 监听必须显式配置网络边界和认证策略。已有主 HTTP 端口的探针语义不因新增 Profile 改变。
- One-shot：执行一次受管函数，完成后退出；不安装 OS Signal Handler，由调用方 Context 控制取消。
- Embedded：调用方控制 Host 的 Start/Wait/Shutdown；不获取进程级 Signal 所有权。
- `bootstrap.Core` 负责配置、日志、Telemetry、生命周期与清理栈，不接管 OS Signal。只有 `bootstrap.Runner/Main` 作为进程入口时注册信号并桥接到 Host。
- 入站 HTTP SecurityMode 只在实际启用业务 HTTP Listener 时校验。Headless 的出站凭证、Slack/Relay/MCP 自身认证和 Probe Listener 安全策略仍须由对应能力明确配置；“没有业务 HTTP”不代表“没有安全边界”。

### 4. Streaming Contract 语义

1. Stream 是独立于 `contract.Call` 的调用形态，不继承 Unary 的 5 秒默认超时。每条 Stream 有根 Context、最大时长和 idle 策略；调用方取消根 Context 或 Close 会取消整条 Stream。
2. `Open` 的同步错误表示 Stream 未建立。建立后的生产/网络错误通过 `Recv` 返回，正常结束通过 `io.EOF`。应用终态（例如 `run.done`）必须作为应用 DTO 事件，不与 EOF 混同。
3. `Recv(ctx)` 的 Context 仅取消当前等待读取操作；根 Stream Context 才取消整条 Stream。取消后的 `Recv` 可重试。每条 Stream 同时只允许一个 Send 与一个 Recv；一个 Send 和一个 Recv 可并发。传输按成功 Send 的顺序保序，超过有界容量后 Send 背压等待，不可建立无界队列。
4. 一旦 Stream 终态确定，后续 `Recv` 稳定重复返回相同终态：正常终态为 `io.EOF`，框架/Transport 失败返回同一可检查错误；单次 `Recv(ctx)` 超时或取消不是 Stream 终态。Close 幂等，取消根 Stream 并等待生产端按 `CloseTimeout` 退出；超时返回框架错误并执行 Transport 强制关闭。Go 无法强杀忽略 Context 的生产 goroutine，契约实现必须响应取消，泄漏测试是必要证据。
5. Local/Remote 两侧共享事务守卫、Context Firewall、超时、错误码和 conformance tests。Firewall 仅传递取消/预算信号、OpenTelemetry Span 和 `callctx.Meta` 白名单，不传任意 Context Value 或事务句柄；事务内开跨模块 Stream fail-fast。Span 覆盖 Open 至终态；活跃数、时长、事件/字节量及错误指标不带 payload、tenant、caller 等高基数或敏感标签。
6. HTTP/SSE 在响应头发送前使用 `problem+json`；发送后发生错误时通过约定的脱敏终态 frame 表达错误码，不试图改写 HTTP Status。不能保证对端收到终态 frame，因此断线恢复与业务去重属于应用 cursor/replay 责任。`Last-Event-ID` 无效或过期时由应用层拒绝并给出明确的应用错误，不由 AppKit 猜测或静默重置 cursor。
7. AppKit 不重试或持久化实时 Stream，不把 Token/Run 事件转入 Bus/Outbox，不定义业务 cursor 和应用终态。

### 5. 首期 Transport 与 SSE 请求模型

首期顺序为 Local Stream，然后 HTTP Server Stream。SSE 采用 **POST 请求体创建并承载同一个流式响应**，明确定位为 fetch/HTTP 流式响应，不宣称为浏览器原生 `EventSource`。这保留 JSON 命令体和 Authorization Header 的能力，不引入 AppKit 管理的两阶段 Run 资源。

SSE 使用 `Last-Event-ID` 将应用 cursor 原样交给应用层。AppKit 不承诺自动重连、不存储 replay；客户端重连、无效/过期 cursor 和重复事件由调用方契约定义。默认普通 HTTP WriteTimeout 不变，流式 Route 使用显式、独立的最大时长、idle timeout、heartbeat、flush 与 proxy buffering 策略。

WebSocket Bidi 放在 SSE/Local conformance 之后单独交付。Upgrade 前必须完成 Origin allowlist、认证、授权、路由分类和身份重建；身份进入连接 Context，不从 frame/query string 读取 tenant/caller。Transport 不在 Upgrade 后自动刷新凭证；认证器提供凭证有效期时，过期即停止接收/发送应用帧、取消连接 Context，并以脱敏策略关闭码结束连接。每条连接的收发队列都必须有帧数和字节上限，达到上限时背压，不静默丢帧。Upgrade 后错误使用脱敏 frame。Transport 追踪每条 hijacked 连接，停止接收新连接、发送关闭信号、按预算 drain，最后强制关闭；不能依赖 `http.Server.Shutdown`。

#### WebSocket 资源边界（ADR-0047 补充，2026-09-24 接受）

- AppKit Hub/Transport 执行连接与消息速率限额；限额由应用显式提供，必须有限且为正，不设置猜测的框架默认容量。
- `MaxConnections` 以单个 Hub 为边界；pending Upgrade reservation 与 active connection 共用原子计数。它不是跨进程或跨副本配额。未显式配置限额的既有 `NewWebSocketHub()` 仍可构造，但不得接受 Upgrade；容量拒绝须发生在 Upgrade 与应用 handler 之前，使用稳定、脱敏的 problem 响应。
- 每条连接分别限制入站和出站应用 `data` 帧的帧数速率、JSON payload 字节速率，并分别配置有限 burst。入站 payload 按 `frame.data` 原始 JSON 字节数计量；出站按应用值编码后的 JSON 字节数计量。`half_close`、`end`、`error` 控制/终态帧不占用应用消息速率预算。既有 `MaxMessageBytes` 继续限制单条消息，且单条消息必须能被 byte burst 接纳。
- 速率预算暂时耗尽时采用可取消的 paced backpressure；不得丢弃、重排已接纳的数据帧。等待受现有 Stream 最大时长、idle、关闭、连接取消及 Host drain 语义约束。速率状态仅在连接本地维护，不按未验证的 forwarded IP、Subject 或其他动态身份建表。
- 新公开类型、构造入口、合法范围和容量错误映射由独立实现子 Issue [#88](https://github.com/forgeplex/appkit/issues/88) 冻结；不得改变既有导出签名或向既有导出结构体追加字段。

### 6. Contract Schema 与兼容

- 现有 `contract.yaml version: 1` 保持严格 Unary 语义，生成输出逐字节无关变化。
- Streaming 使用 `version: 2`，明确声明 `unary`、`server_stream`、`bidi_stream`。V2 生成的 Streaming interface 与既有 V1 Unary `Service` 分开，避免把方法加到既有 Go interface 上破坏下游实现者。
- V2 先生成 OpenAPI vendor extension；只有当规范工具链无法表达/校验流语义时，后续子 Issue 才评估 AsyncAPI。V1 OpenAPI 不改变。
- Compatibility Checker 拒绝既有方法改变调用形态、删除/改变既有字段、requiredness 变化、终态语义不兼容，以及对既有生成 Go interface 的隐式扩宽。新增接口必须显式版本化。

### 7. Registry Contributions

Contribution 使用独立于普通/具名绑定的集合命名空间，键为 `(Go 类型, name)`；同类型内重名在 Register 阶段失败，不同类型同名允许。结果先按类型身份，再按 name 稳定排序；携带 Module 来源。

Contribution 构造器在统一依赖解析阶段 eager 构造一次，以维持现有 Registry 的启动期 fail-fast 和循环检测强度；集合解析只读取已缓存值。只有被 target 选中的模块能注册 Contribution。构造器只能通过 Registry 依赖其他绑定，不以 Contribution 名称提供授权、租户或隔离语义。Provider 优先级、能力匹配和 fallback 仍由应用 Gateway 决定。

### 8. Generation/Reload 和 v1.0 稳定面

通用 Generation/Reload、热替换和自动重启不属于本 Epic 首期交付。Wen 自有 reload 若被垂直切片启用，由 Wen 验证 generation pinning；该结果不代表 AppKit 提供通用 reload。

Headless、Streaming、Contribution 新公开 API 暂不自动进入 v1.0 稳定承诺。#42 冻结稳定面时，只有在 ADR、兼容检查、Wen 与第二个非 Wen 消费方通过真实升级/运行验收后，才能显式提升为稳定面；否则将其列为实验性或明确排除，不阻塞既有 v1.0 Go/No-Go。

## 后续子 Issue 切片提案

以下是本 ADR 合并后再创建的工作图；候选 API 以实现子 Issue 冻结的契约为准，不在本表预先承诺签名。除特别标注外，每个切片都要求自己的本地测试和 required CI 证据。

```text
Phase 0 (#48, 本 Issue)
├─ Host 生命周期与 Managed Service
│  ├─ Bootstrap Profiles / Signal 所有权
│  ├─ HTTP Server Stream / SSE ──┐
│  └─ WebSocket Bidi ───────────┤
├─ Local Streaming Contract / Conformance
│  ├─ HTTP Server Stream / SSE ─┤
│  ├─ Contract Schema v2 / Gen ─┤
│  └─ WebSocket Bidi ───────────┘
├─ Registry Contributions（可与 Host 并行）
└─ Wen 与第二非 Wen 消费方垂直验收（依赖所选能力实现）
```

| 后续切片 | 前置 | 独立验收边界 |
|---|---|---|
| Host Kernel + Managed Service | Phase 0 | Host 状态机、并发 Start/Wait/Shutdown、Drain/Cancel/Wait/Close 顺序与预算；保留现有 Worker 行为 |
| Bootstrap Profiles + Signal Ownership | Host Kernel | Headless、Probe-only、One-shot、Embedded、Runner/Main 的安全与 OS Signal 边界 |
| Local Streaming Contract + Conformance | Phase 0 | Local 双向调用形态、取消/错误/EOF/背压/Firewall 与 conformance 测试 |
| HTTP Server Stream + SSE | Host Kernel、Local Streaming | POST 流式响应、cursor 透传、已提交响应后的脱敏错误、HTTP 关停和 proxy 策略 |
| Contract Schema v2 + Generator | Local Streaming | V1 生成物逐字节稳定；V2 调用形态、OpenAPI 扩展和兼容检查 |
| WebSocket Bidi | Host Kernel、Local Streaming | Upgrade 前鉴权/Origin、身份重建、有界队列、连接跟踪、Close frame 和强制关停 |
| Registry Contributions | Phase 0 | `(type,name)` 冲突、target 过滤、eager 构造、来源和确定性排序 |
| 消费方垂直验收 | 对应 AppKit 切片 | 固定消费方 commit 完成升级/运行验收；不把编译通过等同于业务验收 |

**第二个非 Wen 用例**定为 `forgeplex/notification`，用于通知服务这一非支付、非 Agent 场景。固定基线为 `main@cd1a2fae4afb4dc469d8deff41c1432ee228e196`；验收责任人为 `@pleamon`（本 Issue 作者，且对 AppKit 与 notification 仓库具有 admin 权限，已于 2026-09-23 核验）。本 Phase 0 仅完成固定 commit 的 build/race 基线；真正的 API 升级、服务运行和业务验收留到对应实现切片，且须另行记录其精确环境和结果。

本图是依赖提案，不提前创建实现子 Issue。Phase 0 的 ADR 合并后，应按此边界拆分并将新 Issue 链接回 #47；若实现顺序或证据显示依赖不成立，先更新本 ADR/图。

## 现有行为测试矩阵

下列行为已由 `main@7f798494` 的测试锁定。Phase 0 仅补现有缺口，不复制同义断言。

| 行为 | 现有或新增测试 | 已锁定事实 |
|---|---|---|
| Register/Resolve/Setup/分阶段 Start/Ready/Stop 排序 | `appkit_test.go:193` `TestRunLifecycleOrder`；`appkit_test.go:385` `TestShutdownMirrorsStartOrder` | 启动按 stage 升序，停止按 stage 逆序且同 stage 注册逆序 |
| 启动失败、已启动资源清理和总预算 | `appkit_test.go:302`、`:336`、`:472` | 错误返回；只等待已启动 stage；取消已启动 Worker；卡住的 OnStop 不阻止后续清理 |
| HTTP 监听和运行故障 | `appkit_test.go:449`、`:523` | Bind 失败同步返回；Serve 故障触发关停 |
| ManagedSubscriber | `appkit_test.go:666`、`:696`、`:746`、`:760`、`:777`、`:794`、`:815`、`:832` | Connect/Run/Drain/Cancel/Wait/Close；Drain 期间独立 Bus Run Context 有效；部分连接清理与 readiness |
| Worker 与迁移专用路径 | `worker_test.go:15`、`:51`、`:82`、`:114`、`:140`、`:229`、`:275` | 正常取消、异常退出、关停预算、未启动跳过、迁移不解析/Setup/Start、不足迁移执行器 fail-fast |
| OS 信号关停 | `signal_test.go` `TestRunStopsOnOSSignal`（本 PR 新增，非 Windows 子进程测试） | SIGINT 与 SIGTERM 均由 `App.Run` 消费，执行 OnStop 并以成功状态退出 |
| Unary 契约边界 | `contract/contract_test.go:18`、`:41`、`:75`、`:158`、`:188`、`:207` | 事务守卫、取消、Firewall 白名单、超时、协作式超时、错误规范化 |
| Bootstrap/Profile/HTTP 安全 | `bootstrap/bootstrap_test.go`、`bootstrap/security_test.go`、`security_test.go` | 显式安全模式、`-minimal` 与 `-migrate` 边界、拆分 Bus fail-fast、路由分类和身份边界 |
| Contract 生成和兼容 | `internal/gen/contract_compat_test.go`、`contract_check_test.go` | V1 兼容变更拒绝、稳定生成物、只读 drift 检查 |

这些测试只描述现状；测试名称和断言不构成新 API 签名冻结。

## 下游消费方矩阵

下游固定源代码应从 Git commit 导出到隔离目录后测试；不得用带未提交改动的共享 checkout 代替固定版本。Issue #48 的候选矩阵如下：

| 场景 | 消费项目 | 固定 commit | 本轮状态 |
|---|---|---|---|
| 支付/账本 | `forgeplex/ledger` | `25b7dea716f7655b2bfbf98ef1f93c8e8293ee1e` | GitHub `main` 固定 SHA；Go build 通过；`go test -race` 21 pass、0 fail。需 `integration` build tag 的 PostgreSQL/API 集成测试未启用 |
| 非支付通知 | `forgeplex/notification` | `cd1a2fae4afb4dc469d8deff41c1432ee228e196` | GitHub `main` 固定 SHA；Go build 通过；`go test -race` 58 pass、12 PostgreSQL test skip、0 fail |
| AI Agent 垂直切片 | `forgeplex/wen-go` | `e4cbafe7698ceb8535806023c33e6203ec63b45f`（#47 试点固定基线） | 此 SHA 于 2026-09-23 核验仍是 `codex/v2` 分支；Wen 运行时验收属于后续阶段 |

测试使用隔离克隆分别 checkout ledger/notification 的固定 commit，再由 `scripts/test-downstream-local.sh` 复制 Go/SQL 文件到临时消费者副本；共享 checkout 的未提交改动未进入测试。脚本两项目的 Go build 均通过，但它因 `/opt/homebrew/bin/postgres` 缺失而未能启动独立 PostgreSQL 集群。随后在隔离副本里清除数据库连接环境后执行 `go test -race -count=1 ./...`：notification 的 12 个 DB 集成用例显式 skip；ledger 的 DB/API integration 文件有 `integration` build tag，因此默认测试图不包含它们。没有运行真实 PostgreSQL 集成测试。

## 本次 Phase 0 验证记录

| 层级 | 结果 |
|---|---|
| 源码/本地 | Go 1.26.6 下 `make check`、`make test-lint` 通过；`go test -race . -run '^TestRunStopsOnOSSignal$' -count=3` 通过；根 module DB 集成测试按无 DSN 默认 skip |
| API 兼容 | 本 PR 仅新增测试与 ADR，未改运行时导出 API；按 CI 固定工具版本本地运行 `apidiff`，相对 `v0.9.5` 无 incompatible；required CI `#35831470225` 通过 |
| 下游固定提交 | ledger `25b7dea` build PASS / 21 race test pass / 0 fail；notification `cd1a2fa` build PASS / 58 race test pass / 12 PostgreSQL test skip / 0 fail。完整隔离 DB 验收未运行：主机缺 PostgreSQL server binary |
| CI/PR | PR [#49](https://github.com/forgeplex/appkit/pull/49) exact head `fc813fc89d4b3cd1986f8118190225e733100eb8`；required `ci` run `35831470225` 通过（含 race、PostgreSQL 18.6 集成、lint、rules、apidiff）；Wen Review 正式 APPROVED；squash merge commit `08dead92e432d9e76f46e91d08527063bdd03e6a`；#48 已关闭 |
| 发布/运行时/业务 | Phase 0 不发布、不部署；Wen 运行时和业务验收尚未执行 |

# appkit

forgeplex 的 Go 后端运行时框架：任何业务域拿来即用；用工具链硬约束防止架构漂移；
每个业务域独立 repo，由组合 repo 装配为完整系统，单二进制与微服务按 `-target` 切换。

**使用手册：[docs/GUIDE.md](docs/GUIDE.md)**（从零构建一个系统的完整步骤，以 SSO 为例）。
**设计文档：[docs/DESIGN.md](docs/DESIGN.md)**（仓库拓扑、约束分级、组合机制、请求路径）。

## 包一览

| 包 | 职责 |
|---|---|
| `appkit`（根） | 稳定核心：`Module` / `Registry` / `Provide` / `Resolve` / `App.Run`，只依赖标准库 |
| `contract` | 跨模块契约调用边界：事务守卫、ctx 防火墙、超时、错误规范化 |
| `config` | koanf 分层配置（file→env）+ 强类型校验，启动 fail-fast |
| `apperr` | 统一错误形态：错误身份 = 错误码，RFC 9457 problem+json |
| `health` | liveness/readiness 探针注册表 |
| `telemetry` | slog + OpenTelemetry 三信号统一初始化 |
| `httpserver` | 根 HTTP 中间件链（RequestID/Recover/AccessLog/OTel），标准库风格 |
| `tx` / `pgtx` | 事务边界接口面（零驱动依赖）/ pgx 实现，事务藏 ctx |
| `pgmigrate` | schema 级迁移 runner（每域独占 Postgres schema） |
| `outbox` | 事务性事件外发 + relay + inbox 幂等消费 |
| `idem` | Stripe 式幂等键中间件（claim 先行，防双重执行） |
| `money` | 金额值类型（decimal + currency），禁 float |
| `audit` | 同事务审计（actor/action/before/after，随业务事务同生共死） |
| `ruleset` | golangci-lint v2 / go-arch-lint v3 配置模板（经 `appkit sync` 物化） |
| `lint`（嵌套 module） | 自研 analyzer：`moneyfloat`（金额禁浮点）、`ctxstruct` |

## CLI

```sh
go run github.com/forgeplex/appkit/cmd/appkit help
```

| 命令 | 作用 |
|---|---|
| `appkit doctor` | 环境体检：GOPRIVATE、git 私有仓库凭据、go.work、docker，附修复命令 |
| `appkit new domain <name>` | 生成业务域仓库骨架（含 outbox/idem/audit 基础迁移，离线可编译） |
| `appkit new system <name>` | 生成组合仓库骨架（`-target` 单体↔微服务） |
| `appkit dev` | go.work 多仓本地联调（幂等） |
| `appkit sync [--check]` | 物化/校验 lint 配置与 CI 引用（漂移即 CI 失败） |
| `appkit check` | 架构检查：域间依赖铁律、import 方向矩阵、SQL 跨 schema、迁移编号 |
| `appkit gen events\|errors\|wrap` | 从 yaml 生成事件/错误码；从接口生成 contract.Call 拦截 wrapper |

## 快速开始

见 [examples/greeter](examples/greeter)：两个模块 + 组合根，
`-target=all` 单二进制、`-target=gateway` 拆分部署（另一模块自动落到远程绑定）。
新项目直接 `appkit new domain <name>`，生成即合规（骨架自身通过 `check` 与 `sync --check`）。

## 测试

```sh
make test
# 数据层集成测试需要：
TEST_DATABASE_URL=postgres://localhost:5432/appkit_test make test
```

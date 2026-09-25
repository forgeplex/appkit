# v1.0 稳定面与版本支持政策（候选）

状态：#42 的候选政策，供试点证据评审；本文件不等于 v1.0 Go/No-Go、发版或下游业务验收。

## 稳定面

除下方明确列出的实验 API 外，已发布的公开 Go API、文档化配置、CLI 输入和版本化 manifest，作为 v1.0 稳定候选面。`internal/` 不属于公开承诺。AppKit 主 module 与嵌套 `lint` module 分别解析；发版按仓库规则锁步打 tag。

| 面 | v1.0 候选承诺 | 边界与证据 |
|---|---|---|
| Go API | 主 module 与嵌套 `lint` module 中已发布公开包的导出标识符保持源码兼容；新增能力优先加法表达。根包核心与 `tx` 的标准库边界继续保持。 | CI 分别对两个 module 运行 `apidiff`，基线为最新发布 tag；它覆盖各 module 的导出 API，包括实验 API。通过 apidiff 只证明 API 形状兼容，不证明默认行为、配置或业务兼容。 |
| 配置 | GUIDE 中记录的键名、含义、验证与默认值构成配置契约；已有合法配置不得被静默重解释。 | 新配置应可选并有安全默认值。改变稳定配置的既有含义，须作为不兼容变更处理；安全修复例外必须在发布说明中写明影响和迁移办法。 |
| CLI | GUIDE 中记录的命令、参数和退出语义构成稳定输入面；可加新命令/参数，不得静默改写现有含义。 | 面向人的帮助/诊断文本不是机器协议。机器可读输出只有在带稳定版本 schema 并明确列入本表后才算稳定；带 `alpha` 的格式仍属实验。 |
| Manifest / 契约 | `.appkit.yml v1`、`db/access.yaml v1`、`codes.yaml v1`、`events.yaml v1` 和 `contract.yaml v1` 是版本化输入；已发布版本继续按原语义解析。`contract.yaml v1` 继续只表示 Unary。 | `contract.yaml v2` 的 Streaming 部分仍实验性。新增字段须保留旧版含义；不兼容语义使用新版本，不做隐式升级。`refs.Spec` 描述的是消费方业务资源规范，AppKit 只校验结构，不拥有其业务含义或版本策略。 |
| 生成物 | 稳定输入的生成结果必须能用对应 AppKit 版本编译，并由 drift 检查器验证；改生成格式需有升级说明。 | 生成文件是派生物，不手改。`contract.yaml v1` 生成物继续逐字节稳定；其他生成器/脚手架升级须审阅实际 diff，脚手架产物须通过自身 `appkit check` 与编译。任意业务语义不由生成器保证。 |
| 数据库迁移 | 已应用 migration 的编号、内容和 checksum 不可变；框架结构变化通过新的追加 migration 表达。 | 每个下游负责在隔离数据库验证其真实升级与前向修复。API 兼容或框架本地 DB 测试不等于任一业务库迁移已验收。 |

### 暂不纳入 v1.0 稳定承诺的 API

按已接受的 [ADR-0047 §8](adr/0047-app-host-and-streaming.md#8-generationreload-和-v10-稳定面)，以下新增 API 不因已合并或有本地测试而自动稳定：

- Headless / 新 Host Profile 入口：`appkit.Headless`、`App.Start`、`RunningApp`、`ManagedService` / `ManagedServiceFactory`，以及 `bootstrap` 的 `Core`、`Runner`、`ProfileOptions`、`ProfileDeps`、`Capabilities`、`ProbeOptions`、`RunningProfile` 及其配套生命周期 API。
- Streaming：`contract` 的 `Stream`、`ClientStream`、`StreamConfig`、`StreamHandler`、`OpenLocal` 和 `contract/streamtest`；`httpserver` 的 SSE 与 WebSocket handler、Hub、配置、限额和 secure client API。
- Registry Contribution：`appkit.Contribution`、`Contribute`、`ResolveContributions`。

这些能力只有在 ADR 与兼容检查就绪，且 Wen 和第二个非 Wen 消费方完成真实升级/运行验收后，才可逐项提升；否则保持实验性或明确排除，不阻塞 Go/No-Go。Streaming 当前仍是实验性；GUIDE 示例正确不代表下游运行验收已经完成。

“实验性”表示不承诺其业务语义/长期稳定面，不表示 CI 放行破坏性 API 变更：当前 `apidiff` 仍检查以上所有导出 API。任何兼容门禁调整都必须单独评审并保留稳定 API 的保护。

## 弃用、默认行为与版本号

- 稳定 Go API 的弃用注释使用 Go 的 `Deprecated:` 约定，写出替代入口和迁移说明；稳定 v1 API 不在 v1 主版本内移除。配置键、CLI 输入、manifest/schema 也不得在 v1 内静默删除或改变含义。v2 是进行不兼容清理的最早版本，但不预定其发布日期；当前不承诺按月份或发布次数计的弃用窗口。
- 稳定配置与 API 的默认行为在 v1 内保持兼容。确需改变时先提供显式 opt-in/迁移路径；若仍破坏已记录用法，按不兼容版本处理。安全或正确性修复可以修正有风险的既有行为，但必须在 patch 发布说明中清楚说明影响、规避方式和迁移步骤。
- 版本号遵循仓库既有 SemVer 纪律：修 bug、内部重构和兼容性加法用 patch；值得所有消费者重新评估的默认语义变化用 minor；稳定面不兼容变更只能进入下一 major。主 module 与 `lint/` 的 tag 锁步。

## 支持与安全修复

目前不承诺固定的维护时长、响应时限、历史 minor 支持窗口或安全修复回补窗口；消费者应优先升级到最新发布 patch。AppKit 维护者负责评估并修复框架自身缺陷/漏洞；消费项目负责升级到修复版并处理自身配置与业务风险。当前不据此推导 SLA 或旧版本回补义务。待 #43 确认维护负责人、两个消费项目及其版本后，再决定正式支持哪些版本线、回补责任和相应资源；该决定应在 Go/No-Go 前更新本文件。

## 责任边界与验收门

- AppKit 负责框架机制、公开接口、配置/CLI/manifest 契约、生成器、基础设施机制件、兼容门禁和准确的升级说明。
- 消费项目负责业务语义、业务配置值、数据转换与前向修复、密钥/身份配置，以及部署环境、容量、回滚和业务验收。AppKit 示例不把 Slack、Relay、Provider 等产品集成归入框架能力。
- 源码测试、required CI、发布产物、运行时观察和业务验收分别留证，不相互替代。#41 的真实业务试点和 #43 的两个消费者证据未完成前，不把文档矩阵宣称为已运行验证。
- #42 的最终稳定面须吸收 #41 试点反馈；支持负责人/版本线依赖 #43；#44 才执行最终 Go/No-Go 和受保护发布验收。

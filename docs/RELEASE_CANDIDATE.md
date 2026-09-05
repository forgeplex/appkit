# v0.9.0 模块复用版本：升级清单

这是 v0.9.0 的历史升级记录。v0.9.1 已补齐下文当时未交付的服务认证与安全客户端，
当前接入方式见 [SERVICE_AUTH.md](SERVICE_AUTH.md)，新增验收见
[FRAMEWORK_ACCEPTANCE.md](FRAMEWORK_ACCEPTANCE.md)。历史版本限制不代表当前能力。

本文件对应已确认的 **v0.9.0** 发布范围；历史上撤回的 v0.8.0 不可重新使用。
实际发布状态以 GitHub 的 tag / Release 为准。本文件不是 annotated tag message
或 CHANGELOG 的替代；按 AGENTS.md 以 annotated tag 正文为版本说明的事实源。

本版将本地模块复用/Agent 工作流与远端 main 的安全修复整合。整合时的远端基线是
`6c02802464fe3ad574a77ffb7b919dfdd070ee2c`。原本地工作区没有被覆盖，候选分支为
`codex/appkit-reuse-release`。本地验收记录见 [FRAMEWORK_ACCEPTANCE.md](FRAMEWORK_ACCEPTANCE.md)。

## 本版内容

- 同一契约的命名实例：精确匹配类型与名称，保留无名绑定，不允许跨名字回退。
- Agent 的 plan/apply/replay、选定文件快照、锁、恢复日志与 schema 目录 guard；
  contract 模型兼容检查；共享纯渲染器，不执行项目自带生成脚本。
- partition 与 tenant 独立传播/隔离、多签发方用户验签、分区＋行级租户组合、
  进程内显式只读跨租户、分区域 logical-template schema 文档与五种脚手架。
- 合并远端显式安全模式、分类根路由、严格身份清洗、显式 dev minimal、
  outbox 启停/失败关闭与不可变 CI 引用；补齐新 Partition 的身份清洗。
- 四独立 Go module 的复用/升级验收、隔离 PostgreSQL 运行器、实际四业务项目
  源码副本上的编译与真实数据库 race 验证。

## 升级前必须处理

1. **启动安全模式**：`App.Run` 必须显式选 `Security`；bootstrap 必须配置
   `security.mode`。旧应用遗漏时会在迁移/Setup/监听前拒绝启动。`App.Migrate`
   是独立迁移入口，不要求 HTTP 模式。不要把 `Disabled` 当生产迁移捷径。
2. **路由分类**：严格模式拒绝旧 `reg.Mount` 的未分类根路由。按端点真实语义
   改为 `MountPublic`、`MountAuthenticated`、`MountPermission` 或
   `MountInternalService`；不能为了开机把业务端点统一改 Public。
3. **身份来源**：严格 HTTP 边界清除外部 Actor/ServicePrincipal、partition、
   tenant、caller 及 unsigned 身份头。`authn.MultiIssuer` 根据验签后的 iss
   与组合根配置重建分区，tid 重建租户。内部 HTTP 同样不能仅凭 X-Partition /
   X-Tenant-Id 恢复身份；出站传播头并不等于服务授权。
4. **服务身份限制**：已有 ServicePrincipal、服务路由守卫与模式矩阵，但标准
   service JWT 验证器尚未实现；bootstrap 对 internal/mixed 仍然拒绝启动。
   本候选不声称已经交付生产级服务间认证。
5. **Partition 迁移**：此前用 TenantID 当 schema 路由键的业务，应明确改为
   `Meta.Partition`，检查调用方、事件生产/消费、组合根分区映射；真正的业务
   merchant/tenant 保留在 TenantID。框架不会自动改历史事件或业务记录。
6. **RLS 迁移**：`VerifyTenantRLS` 现在要求 isolation 与 read-all 两条策略。
   须在新增迁移中先用 `TenantScopeSQL` 刷新上下文函数，再对每张租户表调用
   `TenantPolicySQL` 更新策略；分区域分别用 `TenantScopeSQLBare` 与
   `TenantPolicySQLBare`。不能只换策略或修改历史迁移文件。上线前用普通
   非 BYPASSRLS 角色验证。读全部标记须在外层事务前
   显式设置并先完成权限检查；不跨契约/事件传播，也没有“写全部”模式。
7. **最小模式**：无数据库不再隐式降级。只在 dev 环境显式 `-minimal`；正常
   运行必须补齐所需基础设施配置。无需迁移时使用明确的 SkipMigrations 配置。
8. **规则分发**：升级依赖后运行 `appkit sync` 并审查完整 workflow SHA。
   devel 工具不得用下游 HEAD 作为框架版本；离线时显式给已核验的
   `-workflow-ref`。模块缓存可被版本解析更新，但目标仓库只在实际写入时变化。
9. **事件投递**：DirectBus 无订阅者返回 `ErrNoSubscriber`，outbox 保留事件重试，
   不再把无人消费算作成功。拆分 target 默认拒绝隐式 DirectBus，须用 `NewBus`
   配置外部 broker，或仅在确知单进程语义的场景显式 `AllowDirectBusForSplit`。
   托管 Bus 会在消费者注册后启动、停机排空；不要依赖旧的静默丢弃行为。

公开 API 的 apidiff 零 incompatible 仅证明声明兼容，**不证明默认启动行为
不变**。四业务项目的 build/test 通过也不是已执行上述应用装配或数据升级。

## 发布流程

通过受保护 PR 合并，等待 required `ci` 成功。
main 禁止直接覆盖/force-push；不绕过 required checks。选择已通过验收的合并
提交，按仓库规程同时创建主 annotated tag 与对应 `lint/` tag；生成 CHANGELOG
镜像也通过正常 PR，随后推送允许创建的 tags，GitHub Release 正文与 tag 一致。
不重打旧 tag，不自动升级、部署或迁移下游项目。email 当前没有 Git 仓库，
其本地测试修复不能伪称已提交或已发布。

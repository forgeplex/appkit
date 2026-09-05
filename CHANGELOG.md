# Changelog

按版本倒序；每条是其 annotated tag message 的镜像，事实源是 tag，本文件禁手改（发版后跑 `make changelog` 重新生成）。网页版见 [Releases](https://github.com/forgeplex/appkit/releases)。

## v0.9.2（2026-09-05）

v0.9.2 — optional resource references for reusable domains

This release adds an opt-in refs capability so reusable domains can represent project-specific external references without importing another domain or forcing merchant fields into every resource.

- Add refs.Values: detached single-valued reference snapshots, deterministic JSON, bounded inputs, and strict rejection of duplicate keys, null values, malformed Unicode and ambiguous JSON.
- Add versioned refs.Spec and refs.Schema: declared reference roles, explicit opaque/UUID ID formats, required references, immutable updates and partial-filter validation. Validation is structural; target existence, ownership, authorization and concurrency control remain explicit domain responsibilities.
- Add contract.yaml type: refs, generating refs.Values in requests, replies, named DTOs and arrays, plus OpenAPI object schemas. Requests containing refs use bounded, unambiguous JSON decoding. Contracts without refs retain their existing generated output.
- Add a runnable order example for PSP merchant/account/channel references and a separate no-merchant project, with API, storage and security guidance.
- Preserve public API compatibility with v0.9.1. No database migration, automatic BaseModel field, identity propagation, JSONB driver adapter, SQL/index generator or event-YAML refs type is introduced.

Verified with full framework checks, race tests against disposable PostgreSQL, lint and materialized-rule integration tests, generated HTTP round trips, fuzz testing and the public API compatibility gate.

Upgrade AppKit and the appkit-lint module together to v0.9.2, then run the version-pinned appkit sync to refresh generated CI/rules. Existing domains can upgrade without adding refs to their tables; adopt resource schemas only where the domain needs extensible references.

## v0.9.1（2026-09-05）

服务身份、安全远程契约与业务接入收尾

补齐 v0.9.0 明确保留的服务 JWT 机制和接入代码。本次只交付代码、测试与框架版本，不执行部署或现有业务数据库迁移。

新增与修复：

- authn.ServiceVerifier / ServiceSigner：Ed25519 短期服务 JWT，静态 issuer + kid + subject 信任表，精确单一 audience，必填 exp/iat，固定类型/用途，最长 5 分钟。
- tenant / merchant / partition 委托须显式授权；未配置策略拒绝非空范围。Caller 从已验证服务 subject 重建，拒绝签名声明与身份头冲突，mixed 中两份凭证范围必须一致。
- bootstrap 支持 security.service 声明式公钥和精确委托规则，以及 MainWithSecurity / RunWithSecurity 的 ServiceVerifier 和多用户签发方注入；缺失或无效配置仍在数据库/模块装配/监听前失败。
- 迁移专用路径完全跳过 HTTP 安全配置加载与验证器构造，避免无关认证配置阻断迁移命令。
- 生成 NewSecureClient 和 contract.NewSecureHTTPClient：强制 HTTPS、凭证 provider 与 audience，锁定 origin，禁止重定向、跳过 TLS 验证、cookie jar 和 HTTP trailer；每次尝试获取凭证，不转发用户或上一跳的令牌。
- 用户签发方配置深拷贝，拒绝重复 Authorization 与 service/step-up 令牌冒充用户访问令牌。
- apptest.ConformWithMeta 支持业务合法的租户/分区测试标识，并检查分区传播；保留原 Conform 签名与默认行为。
- 域/组合脚手架 Makefile 使用按依赖版本固定的独立 CLI module，修复只读业务依赖图被工具依赖污染的问题；更新安全装配示例与文档。

升级提示：

- 现有用户访问令牌的 purpose 仅允许省略或 access；step-up 仍使用独立 X-Step-Up。服务凭证使用 X-Service-Authorization，不替代用户 Actor。
- 旧 NewClient 保留兼容，但不提供服务认证；生产内部契约请重新生成并显式使用 NewSecureClient，接收端路由使用 MountInternalService。
- 空租户/分区不是“全部”。业务入口仍须校验资源归属与请求范围；账本 ID、全局表或后台任务不能被机械改成 tenant。
- v0.9.0 的显式安全模式、Partition/TenantID 分离和既有 RLS 升级要求继续适用；有数据的项目仍需自己的新增迁移和升级演练，不能改历史 SQL。

边界：服务 JWT 是短期 bearer token，不是一次性凭证；本版不提供在线吊销、自动 JWKS、mTLS/SPIFFE 或生产密钥托管。签发策略和组合根仍是受信代码。原有业务仓库的代码修复不等于这些仓库已经发布或上线。

验证：

- make check、隔离 PostgreSQL 18.6 全框架 race、独立 lint module、真实规则检查器 E2E、五形态脚手架编译/check 全部通过；go mod tidy 无漂移，相对 v0.9.0 公开 API 零 incompatible。
- 四项目最终源码副本完整 build/隔离 DB race：email 135、notification 78、RBAC 376、ledger 504 个 test/subtest 通过，均无失败；ledger 仅两个可选性能播种测试未启用。
- 业务代码补齐签名服务入口、RBAC 分区与原权限语义、认证/授权先于幂等缓存、ledger 重放前资源归属复核、email HMAC 渠道与回执联合匹配；未修改历史 DDL，也未操作现有数据库。

文档：
- [服务认证与轮换](https://github.com/forgeplex/appkit/blob/v0.9.1/docs/SERVICE_AUTH.md)
- [框架与业务代码验收](https://github.com/forgeplex/appkit/blob/v0.9.1/docs/FRAMEWORK_ACCEPTANCE.md)

## v0.9.0（2026-09-05）

模块复用、Agent 工作流与显式安全边界

面向多个独立 Go 项目复用模块，以及 Agent 可审查、可恢复的代码生成流程。
本版整合 v0.7.2 之后的框架功能与安全修复；v0.8.0 历史版本号不复用。

新增与修复：

- 按 Go 类型与名称精确绑定实例；保留无名绑定，不跨名称回退。实例名称不是租户或授权边界。
- plan/apply/replay、输入输出快照、冲突检查和恢复日志；支持规则、契约、事件、错误码、wrapper、脚手架与 schema 文档。
- 独立 Partition 与 TenantID、多签发方用户验签、分区＋行级隔离、显式只读跨租户及五种脚手架。
- 分区域 schema 逻辑模板、目录成员 guard、契约生成漂移与保守模型兼容检查。
- 不可变 workflow SHA、显式离线 -workflow-ref；自动来源解析支持取消，在工作区锁外执行，不把下游 HEAD 当作框架来源。
- 修复 SkipMigrations 配置传递，接入受管 Broker 的启动、停机排空与失败关闭；CI 的 Action、工具与数据库镜像固定来源。
- 修复 CHANGELOG 生成器的 shell 变量边界，确保不同 shell 下保留完整版本标题，并增加真实 make/tag 回归。

升级必读：

- 默认启动行为改变。App.Run 必须声明 Security；bootstrap 必须配置 security.mode。严格模式拒绝旧 Mount 的未分类根路由，须按实际语义分类，不能把业务端点统一改为 Public。
- 严格 HTTP 边界不再信任外部 Actor/ServicePrincipal、身份上下文及 unsigned partition/tenant/caller 头，须由已验证凭证重建身份。
- 无数据库不再隐式降级；零依赖试跑仅允许 dev 显式 -minimal。
- 拆分 target 默认拒绝隐式 DirectBus；须通过 NewBus 配置外部 Broker，或用 AllowDirectBusForSplit 明确承担仅进程内投递的例外。无订阅者返回 ErrNoSubscriber，outbox 保留事件重试，不再把无人消费确认为投递成功。
- 曾借 TenantID 路由 schema 的项目须迁至 Partition，并检查调用方、事件生产/消费与组合根映射。
- 旧 RLS 必须新增迁移：先用 TenantScopeSQL 刷新上下文函数，再为各表更新 TenantPolicySQL 两条策略；分区域使用对应 Bare 版本，勿改历史迁移。用非 superuser/BYPASSRLS 角色验收。

已知边界：

- 标准 service JWT 验证器尚未交付；bootstrap 的 internal_service/mixed 仍拒绝启动，不宣称已完成生产级服务间认证。
- 跨租户能力只显式开放读取，须事先授权、在外层事务前设置且不跨契约/事件传播；没有“写全部”模式。
- schema 规划会在临时数据库执行可信 SQL，不是沙箱；逻辑模板不检查运行分区。apply 不执行业务迁移、部署或下游升级。
- 文件提交可恢复，但不是对所有外部读者原子可见的事务；计划摘要只绑定选定输入输出，不是完整仓库证明或制品签名。
- 公开 API 相对 v0.7.2 的 apidiff 零 incompatible，仅证明声明兼容，不代表旧应用无需调整即可启动。

验收：本地 make check、PostgreSQL 18.6 全仓 race、lint module、真实规则 E2E、五形态编译/check、真实 CLI 离线与超时回归均通过。四业务源码副本均编译及真实数据库 race 通过；仅 ledger 两项性能播种 skip。业务仓库原有 go.sum/规则漂移单列，不冒充所有业务工程门禁或生产升级已完成。

详细说明：
- [升级清单](https://github.com/forgeplex/appkit/blob/v0.9.0/docs/RELEASE_CANDIDATE.md)
- [Agent 工作流](https://github.com/forgeplex/appkit/blob/v0.9.0/docs/AGENT_WORKFLOW.md)
- [验收范围与限制](https://github.com/forgeplex/appkit/blob/v0.9.0/docs/FRAMEWORK_ACCEPTANCE.md)

## v0.7.2（2026-09-03）

page 包：列表端点的分页机制件

为什么：分页此前每个端点自己发明——limit 解析、游标编码、响应
信封各写各的，一个仓库里很快两三种格式并存（ledger 已实证：两种
手写游标编码 + 手写 limit 裁剪，解析函数 ~25 行纯机制代码）。其余
域还没有列表端点，一个域验证了形态、其余尚未发明，正是收编窗口；
等各家发明完再收，迁移成本翻几倍。切分同权限（v0.6.0）/租户
（v0.7.0）的既有模式：机制归框架，语义归域。

这一版干了什么：

- page.Parse：?limit 缺省 50、上限 200（防 ?limit=1000000 拖库的
  DoS 面），畸形值一律 422 而非静默裁剪——被悄悄压成 50 条时客户端
  以为拿到了全部，响亮失败才看得见；宽窄行列表用 WithDefault/
  WithMax 定制值域。?cursor 原样透传。
- page.Encode/Decode[T]：游标编解码，JSON → base64url（可直接进
  query）。对客户端是不透明契约：只回传、不解析、不构造。不签名——
  伪造游标最多翻到别的页（keyset WHERE 照样全量过滤），不是安全
  边界，不值得密钥管理成本。
- page.List[T] 信封：items + next_cursor，字段缺席即最后一页。
- page.Trim：落实「limit+1 多取一行判下一页」技巧，游标锚点 =
  返回的末行（不是被截的那行——keyset (排序键) < 游标 从末行
  之后继续）。
- 文档：DESIGN §7 对照表诚实标约定级（用不用在域，自写手翻页无
  机检）；GUIDE 新章给 handler 全链骨架、sqlc keyset 标准形与
  tiebreaker 纪律（时间戳做键必加 id 兜住同刻并列）、为何不 offset
  （深翻页线性变慢 + 页间漂移）。

留在域里的（语义）：cursor 结构体定义（排序键每表不同）、sqlc 里
的 keyset WHERE 子句（框架不碰 SQL 拼接）、要不要 total。明确不做：
offset/page 码 API、双向翻页、游标签名、total/COUNT。

影响面：纯加法新包，apidiff 相对 v0.7.1 零 incompatible；不用它的
仓库行为不变。已有列表端点（如 ledger 的旧游标串）不必迁移——旧
格式继续工作，新端点用框架。

## v0.7.1（2026-09-02）

pgtx 修缮：事务收尾兜住 Goexit + DB 对齐 sqlc 批处理

为什么：两处都是框架该兜的底，而不是该让下游立规矩绕行的坑。
其一，Do 用 recover 兜异常退出，但 runtime.Goexit 不是 panic（testing
的 t.Fatal/FailNow 即此实现）——recover 拿到 nil，回滚分支不执行，
事务既不提交也不回滚，连接被占住直到进程退出；集成测试里「事务内
断言失败」是高频写法，一旦这么写，泄漏的是真连接且测试本体可能
还是绿的，极难归因。其二，DB 接口缺 SendBatch——sqlc 只要见到任一
条批处理注解（:batchexec/:batchmany/:copyfrom 等）就会给整包共享的
DBTX 加上该方法，pgtx.From 的返回值进不了 sqlc.New，事务感知的
调用链断掉，下游只能自造一份 dbtxFrom 复制语义绕开——两份逻辑
并存，pgtx 修了语义下游感知不到。

这一版干了什么：

- Do 收尾挪进无条件 defer（committed 标记区分提交/回滚），recover
  整个删掉：defer 在返回、panic、Goexit 三种退出路径下都执行，连接
  必归还；panic 原样穿出不再经 recover 转手。回滚失败仍并进返回
  错误（ErrTxClosed 无害忽略），错误/panic 的既有语义不变。
- DB 补 SendBatch：pgx v5 里 *pgxpool.Pool 与 pgx.Tx 的签名完全一致，
  接口的实现断言两行不用动。测试含编译期形状断言（复刻 sqlc 批处理
  生成的 DBTX）与 From().SendBatch 在事务内提交的集成冒烟。
- 回归反证：Goexit 测试（单连接池让泄漏显形——Goexit 后第二次 Do
  必须还拿得到连接）在旧代码上挂死、新代码 0.02s 通过。make check /
  test-lint / test-db -race 真库全绿。

影响面：apidiff 相对 v0.7.0 计一行 incompatible——pgtx.DB 加方法对
接口的实现方是形式上的破坏，但 DB 全仓只有框架自身返回、下游只消费
不实现，实际是修「与 sqlc 批处理 DBTX 不兼容」这个 bug 而非改语义，
故 patch 而非 minor。用 sqlc 批处理的仓库升级后可删自造的 dbtxFrom，
直接把 pgtx.From(ctx, pool) 喂给 sqlc.New；在事务内写 t.Fatal 的
测试从此合法且不再泄漏连接。

Fixes #1
Fixes #2

## v0.7.0（2026-09-02）

租户机制归框架：authn 焊 callctx + 单 schema 域 RLS 强制 + 第三种域形态 -tenant

为什么：租户此前框架只收了「传播」（callctx 三路径自动），没收「解析
入口」（租户身份从请求哪里来、谁可信）与「存储强制」（单 schema 域的
跨租户过滤）——每个域自己实现，风格漂移只是表象，漏一条租户 WHERE 是
静默跨租户泄漏。这版与权限抽象（v0.6.0）同构切分：机制归框架（身份
来源唯一化 + RLS 存储强制），语义归域（租户模型/层级/生命周期/目录表，
归各域与 rbac/identity）。域形态从此三选一：默认 plain（单租户）/
-tenant（行级 RLS）/ -partitioned（schema 隔离），互斥、生成时选定。

这一版干了什么：

- authn → callctx 焊接（身份来源唯一化）：验过的令牌 tid 焊进
  callctx——有值覆盖入站头带来的值、无值清零，「无租户令牌 + 伪造的
  X-Tenant-Id」不能成立。头只在未认证入口存活（那是内部东西向的合法
  形态：Transport 注入 → Extract 读出）；边缘剥头归网关，框架不越位。
  业务代码取租户唯一入口 `callctx.From(ctx).TenantID`。
- pgtx 机制件（DDL 库函数是唯一事实源）：`NewTenant(pool)` 每次 Do 开
  事务后把租户落成事务级 GUC `app.tenant_id`（set_config 事务级，连接
  归池即净；无租户不设——基础设施表的 relay/claim 不带租户，不能被
  卡死）。`TenantScopeSQL` 建策略函数（GUC 缺失 RAISE 42501 而非匹配
  零行——事务外直查业务表响亮失败，「查询成功但什么都看不见」才是
  危险形态）。`TenantPolicySQL` 三件套：ENABLE + **FORCE**（表主通常
  就是应用角色，不 FORCE 则 RLS 是装饰）+ 策略（USING/WITH CHECK 都
  比对 tenant_id，拦住「把别家的租户写进去」）。
- `pgtx.VerifyTenantRLS` 启动期守卫（Setup 期调用，晚于迁移，看到的是
  终态）：有 tenant_id 列的表必须三件套齐，且连接角色不得 superuser/
  BYPASSRLS（否则 RLS 静默不生效）——缺一样拒绝启动，点名全部问题表
  并附修复 SQL。
- schemadoc 补 RLS 渲染（此前直接 unsupported 报错，租户域进不了
  schema 工作流）：策略三件套如实渲染进 db/schema/<表>.sql，策略被删/
  FORCE 被摘即漂移检查变红；未 ENABLE 的「装饰态」点名标注而非装作
  没有。
- 脚手架 -tenant 形态（与 -partitioned 互斥）：module.go 走 NewTenant +
  Setup 首行 VerifyTenantRLS；0001 基础迁移追加策略函数；0002 样例迁移
  （tenant_id 列 + 租户打头索引 + 三件套 + COMMENT）建真表照抄；生成
  仓库的 AGENTS/README 教 RLS 写法与数据库角色要求（非 superuser、
  NOBYPASSRLS）。
- 文档：DESIGN §5.5 租户隔离（三形态表 + 诚实标注的洞：未认证入口信
  头、superuser 绕过靠启动校验兜住）+ §7 两行；GUIDE §3.2 租户域章节
  （含逃生舱：跨租户批处理逐租户 callctx.With + Do；迁移内回填先回填
  后挂 policy）+ 鉴权章节租户身份小段 + §4 幂等键作用域示例改为
  callctx 取租户。

影响面：一处**行为变化**——authn 认证请求中 callctx.TenantID 由令牌
决定（头值被覆盖/清零）。既有「令牌无 tid + 依赖 X-Tenant-Id 头租户」
的部署升级后头不再生效，须在令牌里签发 tid 或改造为东西向内部调用；
其余纯加法，apidiff 相对 v0.6.0 零 incompatible，不生成 -tenant 域、
不挂 RLS 的仓库行为不变。0.x 期间号是节拍：这版改了一条既有语义并
新增框架面（租户子系统），值得每个多租户部署读一遍 GUIDE §3.2，故
minor 而非 patch。

## v0.6.0（2026-09-02）

权限抽象进框架：声明 / 绑定 / 判定 / step-up

为什么：给系统接鉴权时，每个域要各复制一份约 90 行的 JWT 验签 + 权限
判定样板，且设计上有两个结构性洞——端点绑码拼错要到运行时 403 才暴露
（绑定字符串不在任何校验视野里）；权限目录的完整性靠组合根记得手工
全量注入，漏了启动失败。这版把**机制**收进框架（声明收集、绑定校验、
判定矩阵、证明验签），**语义**留给可插拔的提供方域（角色模型、通配
匹配、令牌签发、MFA 挑战——如 rbac）：换提供方，业务域与框架代码
零改动。

这一版干了什么：

- 根包 permission.go（零第三方类型，守根包依赖纯净）：模块在 Register
  阶段用 `reg.Permissions` 声明权限码（迟到声明 panic，Setup 期汇总即被
  提供方消费）；`reg.PermissionDecls()` 输出全应用目录（提供方 Setup 期
  同步落库，组合根手工注入环节退役）；`reg.Require(code, h)` 端点绑码，
  判定矩阵统一在框架——401 UNAUTHENTICATED / 403 PERMISSION_DENIED /
  403 STEP_UP_REQUIRED；全部 Setup 之后校验**绑定 ⊆ 声明**，拼错的码
  在监听之前曝光并点名模块与码（★ 装配期硬失败）。
- Actor 主体快照：`{UserID, TenantID, Perms, StepUpAt}`。判定永远是
  集合包含——通配等匹配语义归提供方，在签发侧展开为精确码；权限变更
  随令牌过期生效，判定零远程调用、零每请求查库。
- 新 authn 包（机制件，引入 golang-jwt/v5）：`authn.Middleware(pub,
  issuer)` 验 Bearer 令牌与 X-Step-Up 证明——EdDSA 算法白名单（防算法
  混淆攻击）+ iss + exp 必查验签，注入 Actor。无 Authorization 头放行
  （判公开与否是 Require 的职责）；有头验不过 401——坏凭证是攻击不是
  匿名。claims 布局即框架与提供方之间的契约（iss/sub/exp/tid/perms；
  step-up 另有 purpose/iat，sub 须与访问令牌一致），照此签发即可写出
  第二个提供方。
- step-up 分工：提供方验挑战动作并签 step-up 令牌；框架验证明（签名/
  iss/sub/purpose/exp，取 iat）；Require 验策略（Challenge 标记 + 5 分钟
  新鲜度）。消费方是客户端：403 STEP_UP_REQUIRED → 完成 MFA → 带
  X-Step-Up 重试，无广播、无消费注册。
- apperr 补 `STEP_UP_REQUIRED` 码与 `Unauthenticated`/`PermissionDenied`
  构造捷径。
- bootstrap 两行接入：`AuthnPublicKey` / `AuthnIssuer`（配钥不配 iss
  启动报错——没有 iss 约束的验签会接受任何持私钥者签的令牌）。
- 脚手架两种域形态（普通/分区域）的 module.go 带权限声明样例与教学
  注释；DESIGN §5.4 设计节 + §7 约束表三行、GUIDE 新增鉴权接入章节
  （声明 → 绑码 → 组合根两行 → step-up 客户端流程 → 提供方契约）。

影响面：纯加法，apidiff 相对 v0.5.5 零 incompatible；不配置
AuthnPublicKey 的系统行为不变。0.x 期间号是节拍：这版是新框架面
（权限子系统），域仓库的接入方式（声明 + 绑码 + 组合根挂验签）值得
每个升级者读一遍 GUIDE 鉴权章节，故 minor 而非 patch。

## v0.5.5（2026-09-02）

分区域域 CI 解锁

为什么：rbac 是第一个分区域域（partitioned: true），它的 CI 首跑暴露了
共享 workflow（domain-ci.yml，经 @main 被全部域仓复用）里的两个死锁——
单 schema 域从未踩到，分区域域踩上就是永红，且域仓库侧无解。

这一版修了什么：

- `appkit schema -check` 对分区域域改为 ::notice 后退出 0。schema 文档
  对分区域域是「永远不可能启用」（没有单一 schema 可画），共享 CI 步骤
  对它硬失败等于让所有分区域域 CI 永红；与「未启用」同等处理。非 -check
  路径行为不变：明确拒绝并指向 introspect。
- domain-ci.yml 统一解析 APPKIT_TOOL（域仓 go.mod 钉的 appkit 版本，
  go.work 联调退 @main），四处 go run appkit 工具（check / sync --check /
  schema -check / appkit-lint）全部钉 @版本。不带版本会用域仓的 go.sum
  编译 appkit cmd，而 go mod tidy 会剥掉域仓 import 图之外的 sums——
  「appkit check 能跑」与「go.mod 整洁检查」两头必红一头。

影响面：分区域域 CI 从永红变可绿；单 schema 域行为不变。

## v0.5.4（2026-09-02）

真实客户端 IP 边缘解析 + MIT License

为什么：取真实客户端 IP 是限流、风控、审计的普遍前置需求。让各模块
各自解析 X-Forwarded-For，等于把信任决策（哪些代理网段可信）散落到
业务代码里——伪造头一路通行。信任决策必须在 HTTP 边缘做一次，
模块经 ctx 只读，不可能重算、也不可能被伪造头骗到。

这一版加了什么：

- `httpserver.ClientIP(trusted ...netip.Prefix)`：根中间件，在链上
  解析一次真实客户端 IP 存进 ctx。对端不在可信网段：直接用 TCP 对端
  地址，请求头一律不信；对端在可信网段：依次信 X-Client-IP、
  X-Forwarded-For 从右往左第一个不可信地址（可信代理逐跳追加，
  伪造注入永远在最左，走不到答案）；全链可信时取最左。零个可信
  网段 = 永远用对端，是安全默认：直连部署直接可用，前面有代理时
  组合根把网段配上。
- `callctx.WithClientIP` / `callctx.ClientIP`：ctx 内存取。刻意不进
  Meta 白名单——防火墙剥掉它是有意为之：下游域看到的「对端 IP」
  是上一跳地址，与「原始客户端 IP」语义不同；真有跨边界需求
  （审计链路把起点 IP 带进异步事件）时业务可自行写入 Event.Meta。
- MIT License：repo 已从 private 翻为 public，公开前先定授权——
  无 license 的公开代码在法律上不可被使用。

纯加法，apidiff 相对 v0.5.3 零 incompatible。

## v0.5.3（2026-09-01）

分区域域（partitioned domain）：一套代码、N 份数据分区，schema 由调用方确定

为什么：psp 的商户/运营/代理商是三套独立用户体系，要共用同一份 rbac 域代码，
但数据要强隔离——体系 A 的查询根本看不到体系 B 的表，漏过滤的失败形态是
「表不存在」而不是「数据泄露」。此前 schema 名烧死在迁移文件、const Schema、
sqlc 前缀与 .appkit.yml 四处，运行时无法改。

这一版加了什么：

- `pgtx.NewRouted(pool, route)`：路由 Transactor。每次 Do 开启事务后调用
  route（通常从 callctx.Meta.TenantID 查组合根注入的分区映射）并
  SET LOCAL search_path；事务结束自动还原，连接归还池时不带走设置。
  查无分区即回滚失败，绝不静默落到默认 search_path。
- `outbox` / `idem` / `audit` 各加 `MigrationSQLBare()`：无 schema 前缀的
  基础设施表 DDL，供分区域域的迁移使用；带前缀版不变，两版列定义一致。
- `pgmigrate`：应用每个迁移文件前 SET LOCAL search_path 到该 set 的
  schema——同一份无前缀 FS 可注册到 N 个分区（reg.Migrations 每分区一次），
  schema 不存在时自动建。存量带前缀迁移不受影响（显式限定名不参与
  search_path 解析）。新分区 = 组合根映射加一条 + 重启，零代码零手写 SQL。
- `appkit new domain <name> -partitioned`：生成无前缀世界骨架——专用 module
  模板（Options.Schemas 注入、路由 Transactor、routed Publisher、每分区
  outbox relay）、无前缀基础迁移、`.appkit.yml` 写 `partitioned: true`。
- 分区映射的定义放组合根自己的配置文件（koanf 支持 map[string]string），
  经 module.Options.Schemas 注入——框架不暗读全局配置。
- `archcheck`：partitioned 域的 SQL 前缀规则翻转，任何 schema 前缀都是违规；
  与 sqlc 编译器的「带前缀/无前缀两个世界各自封闭」互为冗余，跨域访问的
  静态保证从前缀扫描转移为编译解析，强度不降级。
- `appkit schema` 对分区域域明确报错暂不支持（分区映射由组合根注入，域仓库
  无从枚举），已在 DESIGN §7 记 ✗ 待补。

硬纪律：分区域的查询必须经 tx.Do——事务外 pgtx.From 落在默认 search_path
上，无前缀表不存在即报错（失败响亮，不静默读写错误分区）。

兼容性：公开 API 纯新增（NewRouted、三个 MigrationSQLBare、
ruleset.AppConfig.Partitioned），apidiff 相对 v0.5.2 零 incompatible；
Transactor 保持可比较（路由函数收在指针后）。既有单 schema 域零感知，
不需要任何改动。

详见 docs/DESIGN.md §8「分区域域」与 docs/GUIDE.md §3.1。

## v0.5.2（2026-09-01）

v0.5.2: 契约流水线落地——contract.yaml 一次生成五件套，零破坏性、零必做动作

这版全是纯加法：apidiff 相对 v0.5.1 零 incompatible，已有代码的默认行为
一概未动，升级不需要任何动作。

★ appkit gen contract：契约包的创作入口从「接口手写 + gen wrap」换成
contract.yaml 先行（与 events.yaml 同风格的 yaml 事实源），一次产出
五份生成物——service.gen.go（Service 接口 + 传值 DTO）、wrap.gen.go
（进程内 wrapper，复用既有 wrap 链路）、client.gen.go（HTTP client，
实现同一接口）、server.gen.go（NewHTTPHandler）、openapi.yaml
（OpenAPI 3.1 派生导出，供文档与 oasdiff 门禁）。DESIGN §3 的方向
就此反转并记录在案：契约的核心语义（粗粒度方法形态、幂等声明、
错误身份 = 错误码）在 OpenAPI 里只能靠扩展字段旁挂，所以事实是
contract.yaml → OpenAPI，不是反过来。

生成 client 焊掉了两种静默失效形态：NewClient 复制传入的
http.Client 并焊上 callctx.Transport（忘了装 Transport 的形态不
存在）；标了 idempotent 的方法对可用性故障做有界重试（3 次、
100ms 起步线性退避，遇 ctx 取消立即收手）。生成 server 的 serve
兜底把请求头里的 callctx 合并回 ctx——裸挂 handler 不经
httpserver 中间件也不丢元数据，挂在中间件后面时是幂等重放。

终证在 internal/gen/genfixture：同一份 contract.yaml 的本地
wrapper 与「生成 client 打生成 server」的真 HTTP 回环，过同一批
apptest.Conform 用例——「部署形态是启动参数」在生成物上成立。
examples/greeter 的 greetapi 已改为全生成，是这条链路的最小实例。

另有两件小事：pprof 排障端点收进配置开关（默认关闭，开了才挂
/debug/pprof）；make tag 现在同时打 lint/vX.Y.Z 路标——lint 是
嵌套 module，下游以伪版本引用时需要同节拍的路标 tag。

## v0.5.1（2026-08-31）

v0.5.1: 三个安静的注入口——池选项透传、死信放回、测试样板收敛

- bootstrap：Options.PoolOptions 透传给生产连接池。域要给池装 otelpgx
  tracer 或会话级 GUC（statement_timeout 等）时，此前只能整个自建池
  绕开 bootstrap——迁移/outbox/幂等装配全部重写一遍；现在
  pgtx.PoolOption 直接挂在 Options 上，例外路径消失。
- outbox：死信有了恢复通道。relay 重试达上限（failed_at 置位）的事件
  原先没有任何出路，只能手写 SQL 改表；outbox.DeadLetters 与
  `appkit outbox` 子命令补上「列出失败原因 → 修好消费方 → 按事件 ID
  （或 -all）放回」，attempts 归零、立即到期，bug 没修好则按完整
  重试预算再次死信，放回只动死信态的行。
- 测试：「env 守卫 + 随机 schema + DROP CASCADE」七处样板收进
  internal/dbtest，标识符转义统一 pgx.Identifier。

零默认行为变更，纯加法与内部收敛，故 patch（分寸见 AGENTS.md 发版节）。

## v0.5.0（2026-08-31）

v0.5.0: 金额边界焊死——decimal 持久化、decjson 机检、幂等指纹可注入

三件事一条线：金额/幂等在边界上的字节形态不再是约定。

money：NUMERIC 持久化走 decimal.Decimal（脚手架 sqlc 全局 override），
Money 定位领域值对象不落库；币种放宽 3-6 位（USDT/USDC/WBTC）；
ParseCanonical 入站只收规范形态（拒 "+80"/"080"/"8e1"/负零）；
pgxmoney 标记 Deprecated。

lint：decjson 检查器——decimal 类型禁止上 JSON 面；appkit-lint 正式接进
域仓库 make lint 与 CI（版本随 go.mod 钉的 appkit 走）。

idem：WithCanonicalizer 让领域规范化口径接进中间件指纹，"80"/"80.00"
的重试拿到回放而非 422；WithKeyScope 作用域键隔离键空间（跨作用域碰撞
不可拼造），替代手拼 "{tenant}:" 前缀。

全部为加法：apidiff 相对 v0.4.0 零 incompatible。

## v0.4.0（2026-08-31）

v0.4.0: schema 设计从「在脑子里重放迁移历史」变成一份可 review 的生成物

迁移是不可变的追加日志，一张表的真实形状散在 N 个文件里：0003 建表、0007 加列、
0012 改默认值。这个版本新增 appkit schema，把它捞出来。

把 db/migrations 应用到一次性临时库（复用生产的 pgmigrate.Runner）再读回
pg_catalog，产出：

  db/SCHEMA.md          表清单 + 按外键连通分量分组的 Mermaid ER 图（人的入口）
  db/schema/<表>.sql    DDL 形态（agent 写迁移时 grep）
  db/schema/<表>.md     表格 + 反向引用 + 1 跳邻域图（人 review 时读）
  db/schema/_appkit/    框架表隔离，不进业务 ER 图

产出是 db/migrations 的纯函数，与本地库被谁手工 ALTER 过无关；换一台 Postgres
实例重跑 -check 零漂移。渲染不了的特性（分区、生成列、RLS、继承、排他约束…）
点名报错，绝不静默输出残缺 DDL——有人会读它并当真。

框架自己的四张基础设施表补上 COMMENT ON TABLE，并加了从 pg_catalog 读回来验的
机检。缺说明的表在 db/SCHEMA.md 里标 ⚠，框架得先守自己立的规矩。

公开 API 无签名变化，apidiff 相对 v0.3.1 零 incompatible。升级后既有代码无需
改动。

CI 那一步是自动的：domain-ci.yml 经 @main 共享，appkit schema -check 会随之
出现。但**启用门**让它在 db/SCHEMA.md 与 db/schema/ 都不存在时打一条 notice
后退出 0——所以什么都不做，CI 也不会变红。

想启用，注意 Makefile 与 .gitattributes 是 appkit new domain 的模板产物，
appkit sync 不管它们（sync 只物化 .golangci.yml / .go-arch-lint.yml /
.github/workflows/ci.yml），得手工补两处：

  # Makefile
  schema: dev-db
  	$(GO) run github.com/forgeplex/appkit/cmd/appkit schema -dsn '$(DEV_DB_URL)'

  # .gitattributes —— 生成物在 PR diff 里折叠，让「有人手改了它」更打眼
  db/SCHEMA.md             linguist-generated=true
  db/schema/**             linguist-generated=true

然后 make schema，提交产出。跑过这一次，drift check 就对该仓库永久转严。

产出禁止手改（有 drift check，缺失 / 内容漂移 / 删表后的残留都算）。改表结构
照旧新开迁移文件，建表时顺手写 COMMENT ON TABLE——表的用途属于 schema 设计，
写在迁移里才会跟着表一起演进。

## v0.3.1（2026-08-31）

fix: apptest 元数据传播检查不再多跑非幂等调用

## v0.3.0（2026-08-31）

v0.3.0: 出站 callctx 白名单从「纯约定」走到「装配一次 + 验得到」

两项新增能力，都是把 DESIGN §7 表末那条 ✗ 往左移：

- callctx.Transport：http.RoundTripper，装进 client 一次即可，出站请求自动
  带上白名单。漏点从「每个调用点」收敛到「一处装配」。Caller 做成显式字段，
  出站该写自己的服务名而不是透传上一跳。
- apptest.Binding.SeenMeta：填了它，Conform 就验得到白名单真的穿过了每一种
  绑定——那条走请求头，返回值里看不见。不比 Caller（出站改写它是对的）。

公开 API 仅新增，apidiff 零 incompatible。下游升级后无需改动既有代码；
要用新能力则：出站 client 装 Transport，契约测试的 Binding 填 SeenMeta。

## v0.2.2（2026-08-30）

v0.2.2: 规则集端到端测试——物化规则首次被真正消费

不改任何规则（规则本身在 v0.2.1 已修正），本版把那次事故的教训固化：

- ruleset 层结构测试：每个组件必须把自己列进 mayDependOn（零成本、常驻）
- scaffold 层端到端测试：生成带子包的域仓库，真跑钉版本的 golangci-lint
  与 go-arch-lint，既要求零误报、也要求已知违规必须被报出（opt-in，CI 常跑）
- make test-rules + CI 步骤 + AGENTS.md 验证清单

下游无需动作：公开 API 与规则模板与 v0.2.1 完全一致。

## v0.2.1（2026-08-30）

fix(ruleset): 让物化的 lint 规则容得下有子包的域

## v0.2.0（2026-08-30）

v0.2.0 —— 五项框架托底能力 + 物化规则真正在 CI 执行

相对 v0.1.1 全部为新增，apidiff 零 incompatible。

新增能力
- callctx：穿过 contract ctx 防火墙的元数据白名单（request/tenant/caller），
  HTTP 入站、契约调用、事件 meta 三条路径自动传播。
- Registry.Worker + job.Every：托管长驻任务与周期任务（advisory lock，多副本不重跑）。
- App.Migrate / -migrate / SkipMigrations：迁移施加时机变成部署决策。
- RED 指标自动埋点：契约调用、outbox 投递与死信、周期任务、DB 查询。
- apptest.Conform：同一批用例跑过 local 与 remote 绑定，证明拆分部署行为不变。
- bootstrap：收拢总线/迁移器/遥测装配，生成物里不再出现。

收紧
- contract.Call 进 fn 前查 ctx.Err()，已死的 ctx 返回 CodeUnavailable。

工具链
- domain-ci.yml 在规则集漂移检查之后真跑 golangci-lint 与 go-arch-lint；
  版本由 ruleset.GolangciLintVersion / ArchLintVersion 单点定义。

升级须知
下游域仓库需各跑一次 `appkit sync`：.golangci.yml 与 .go-arch-lint.yml 内容有变，
否则 CI 的漂移检查会红。

## v0.1.1（2026-08-30）

v0.1.1 —— AI agent 操作规程

生成的域仓库/组合仓库自带 AGENTS.md + CLAUDE.md：仓库地图、三条方向性约束、
加功能的固定六步、惯用法与完成的定义。appkit 仓库根部落一份维护者视角的规程。

公开 API 零变更（apidiff 相对 v0.1.0 零 incompatible）。

## v0.1.0（2026-08-30）

appkit v0.1.0：首个发布版本

稳定核心（Module/Registry/Provide/Resolve/Run，仅依赖标准库）、
数据面（tx/pgtx/money/pgmigrate/outbox/idem/audit）、契约边界（contract）、
工具链（doctor/new/sync/dev/check/gen + ruleset + appkit-lint）。
自此公开 API 按 SemVer 演进，CI apidiff 门禁保证向后兼容。


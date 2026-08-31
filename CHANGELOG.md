# Changelog

按版本倒序；每条是其 annotated tag message 的镜像，事实源是 tag，本文件禁手改（发版后跑 `make changelog` 重新生成）。网页版见 [Releases](https://github.com/forgeplex/appkit/releases)。

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


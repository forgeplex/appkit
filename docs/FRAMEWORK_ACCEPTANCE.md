# 可复用框架基线验收

本次验收回答：两个独立项目能否复用同一份模块实现，Agent 能否安全生成框架代码，
以及兼容升级与数据库隔离是否有可重复执行的证据。它不等于业务系统上线验收。

## 必须通过的检查

| 范围 | 可重复执行的证据 |
|---|---|
| 同一模块跨项目复用 | `internal/acceptance` 创建 contracts / reusable / projectalpha / projectbeta 四个真正的 Go module；`go list -m` 确认两消费者指向同一个实现目录 |
| 实例与部署选择 | 两消费者选择不同具名实例；本地 wrapper 与真实 loopback HTTP 均通过同一组边界断言 |
| 边界语义 | 事务内禁调用、ctx 白名单/防火墙、协作式超时、错误规范化；race 标记传入子进程 |
| 兼容升级 | 共享实现 v0.1.0→v0.1.1，以及可选字段/DTO 扩展后，两个消费项目 Go 源码保持不变并再次通过 |
| 不兼容变更 | 删除 DTO、改变方法签名被模型检查器拒绝，两个消费构建图也出现指定的真实编译错误；修改 path/字段类型有独立拒绝断言 |
| 数据库隔离 | PostgreSQL 真库测试覆盖 tenant RLS、显式只读跨租户、schema 分区、分区＋tenant、无租户普通域 |
| 数据库可靠性 | 真库测试覆盖 commit/rollback/savepoint/panic、迁移重放/并发/校验和、outbox/idem/audit/job 与 schema 文档 |
| Agent 文件工作流 | sync、contract、events、errors、wrap、新 domain/system 的只读计划、规范序列化、apply、replay、冲突与路径拒绝 |
| Schema 计划与文档 | 显式授权的 scratch-DB 规划、捕获迁移 fs.FS、输入/输出目录 guard、过时生成文件删除与手写文件拒绝、无 DB apply/replay；分区逻辑模板与 tenant RLS 真库验证 |
| 现有业务项目兼容性 | `scripts/test-downstream-local.sh` 复制四个现有项目当前 Go/SQL 源码后替换框架依赖，编译并在独立 DB 执行 race 测试；既有失败单列，不计作通过 |
| 脚手架 | 普通、tenant、partitioned、两者组合、system 五种产物实际编译；内存渲染与直接生成逐文件相同 |
| 框架兼容性 | 根 module 的 apidiff 对最新发布 tag 不得有 incompatible；运行默认行为变化单列升级要求，不能由签名兼容推断启动兼容 |
| 工程门禁 | fmt、vet、build、全量测试、独立 lint module、真实规则检查器 E2E，不能把 skip 当通过 |

验收夹具没有 RBAC、订单、账本等业务实现。版本测试使用临时 Go module 的本地
`replace`，验证解析关系与源码升级；没有伪装成 tag 分发、远程 registry 或供应链测试。
Go 子进程关闭 module 网络下载，依赖来自已准备的本地缓存；HTTP 测试只用 loopback。
四 module 的 HTTP 夹具验证 contract 传输边界，不提供生产级服务身份验证；App
组合测试显式声明 UserFacing 模式但不挂业务路由。实际严格 HTTP 身份链另由
authn 的真实 App/loopback 集成测试覆盖，不把未验签的元数据透传当可信身份。
现有项目兼容性检查是独立于上述四 module 夹具的第二层证据，结果见下文。

## 本地执行

```sh
make check
make test-lint
make test-acceptance
make test-rules
```

`test-acceptance` 已被普通 `go test ./...` 包含；单独入口便于定位失败并查看负例输出。
第一次运行需先让 Go 工具链准备框架依赖；验收的子项目不会自行访问代理下载。
规则检查器已完整缓存时，可避免网络上的版本元数据查询：

```sh
GOPROXY="file://$(go env GOMODCACHE)/cache/download" make test-rules
```

显式 `make test-rules` 的工具准备失败现在会失败，不再降级为 skip。普通全量测试
未设置 `APPKIT_RULES_E2E=1` 时仍跳过这个单独的工具集成阶段。

数据库必须使用可丢弃实例。已有完整 PostgreSQL server 安装时：

```sh
APPKIT_POSTGRES_BIN=/path/to/postgresql/bin make test-db-local
```

把路径替换为包含 postgres/initdb/pg_ctl/createdb/psql 的真实 bin 目录。
运行器不安装软件、忽略已有 `TEST_DATABASE_URL`，创建私有目录与 Unix socket，
关闭 TCP 监听，运行 `make test-db`，最后停止并清理自己创建的实例。
它清除继承的 Make 参数/启动文件覆盖，并向内层 make 显式绑定临时 DSN；即使外层
命令行设置了另一个 `TEST_DATABASE_URL`，也不会把它传给数据库测试。宿主 shell
启动阶段与测试代码本身仍需可信，运行器不是通用执行沙箱。
测试若失败会保留退出码并输出临时服务日志；停止失败则保留目录供诊断，绝不误删运行中数据库。
不要为了验收把生产或现有开发库的 URL 交给测试命令。

现有业务模块在 `../apps/{email,notification,rbac,ledger}` 时：

```sh
APPKIT_POSTGRES_BIN=/path/to/postgresql/bin make test-downstream-local
# 也可只检查指定项目；APPKIT_APPS_ROOT 可覆盖父目录。
APPKIT_POSTGRES_BIN=/path/to/postgresql/bin \
  bash scripts/test-downstream-local.sh notification ledger
```

该入口复制**当前源码，包含未提交的 Go/SQL 文件**，不是 Git HEAD；不复制 .git、
配置、环境文件或符号链接。只在副本里设置当前 AppKit 的本地 replace；原业务源码
与依赖不改。子进程清除环境、禁用 go.work/全局 Go 配置，用缓存的文件代理准备
依赖；每个项目独立临时 DB。测试代码本身不是沙箱，项目新增外部调用测试后应先
审查再执行。失败返回非零并保留副本与日志，不能通过忽略失败冒充兼容。

## 2026-09-05 首轮实测记录（远端安全合并前）

- PostgreSQL 18.6 的隔离实例上实际运行全仓 `make test-db`（含 race），通过。
- schema 的 planner/CLI/introspection 单独执行 race 真库回归通过；覆盖 schema 及
  logical-template、迁移成员漂移、离线 apply、取消后精确临时库清理。
- 真实 CLI smoke：新建 partitioned＋tenant 域，schema 计划包含 16 项文件操作；
  无数据库 apply 为 committed，不可达 DSN 下重放为 replayed；13 份产物均带逻辑
  模板标记，恢复临时 DSN 后 `schema -check -mode logical-template` 无漂移。
- 最后一轮全仓 DB 验收故意给外层 make 一个不可达的占位 DSN，内层实际仍绑定
  本次私有 Unix socket，全部通过；递归 Make 覆盖回归另由 fake-tools 测试守住。
- 四 module 复用/升级测试，普通与 race 均通过；负例是预期的编译失败而非下载失败。
- 两个固定版本规则检查器通过本地文件代理实际执行，0 skip；干净域通过，五个
  故意植入的 depguard/archlint/wrapcheck/forbidigo 违规全部被检出。
- `make check`、`make test-lint` 和新 CLI 的实际计划/应用/重放通过；相对 v0.7.2
  的 module 级 apidiff 为零 incompatible。API 比较不把 internal 包算作公开承诺。
- 所有数据库测试实例均使用独立 Unix socket，测试后自动停止并移除；未触及业务
  数据库、系统服务或业务仓库源文件。

### 现有业务项目的实测结果

四个项目的当前源码副本均能引用当前 AppKit 编译，四个数据库集成包均通过。
完整测试结果不能概括成“四仓全绿”：

| 项目 | 当前 AppKit 的 race 结果 | 与原发布依赖的对照 |
|---|---|---|
| email | 115 个 test/subtest 通过；1 个 SMTP 超时测试报告数据竞态 | AppKit v0.5.3 同样复现，测试 channel 发送与 close 竞争 |
| notification | 70 个通过，0 skip、0 fail | 当前源码无需修改 |
| rbac | 340 个通过；混合 WebAuthn/TOTP 用例失败（子用例＋父用例共 2 个 fail 事件） | AppKit v0.7.2 同样复现，留存日志的定向重复 10 次有 1 次失败；测试 helper 从 map 取挑战时未区分因子 |
| ledger | 492 个通过；2 个显式 benchmark seed 测试未启用，0 fail | 当前源码无需修改；不把可选种子 skip 算作功能通过 |

email 不是 Git 仓库；notification 检查时 HEAD 为 `cd1a2fa` 且干净；rbac、ledger
含原有未提交修改，验收副本也包含它们。330 个 Go/SQL 源文件验收前后逐字节一致。
这证明现有模块在该源码状态下的框架接入与测试表现，不表示已修改业务 go.mod、
上线或完成真实数据升级。该首轮把两个既有测试问题保留为下游待办，未改业务代码
或删测试；后续修复见下节。
表中的数量是 Go JSON 的 test/subtest 事件，包含父子测试，不是独立顶层测试数。
可定位到 `email/internal/module/provider/provider_test.go` 的
`TestSMTPSendTimeoutOnSilentServer`（146 行发送、150 行 close）；RBAC 的
`internal/rbac/webauthn_test.go` 的 `TestWebAuthnLogin/混合因子双挑战`（363 行失败），
根因在 `internal/rbac/mfa_test.go` 的 `loginChallenge`（51–53 行选取未按因子过滤）。
RBAC 的 `internal/postgres` 包 18 项全部通过。

## 2026-09-05 后续修复与发布候选验收

经确认只修复两处业务测试问题，没有改业务用例、依赖版本、数据库或部署配置：

- email 的 SMTP 夹具由发送方关闭连接 channel，先关闭 listener 再排空连接；
  断言确实收到 timeout。定向 race 30 次通过，全 provider 包 race 3 次通过。
- RBAC 的 TOTP/WebAuthn helper 使用本次 Login 响应的 challenge ID，验证租户、
  用户、用途与待决状态；混合因子回归覆盖旧挑战、交叉因子拒绝及各自成功。
  五组相关测试 race 20 次通过；仅两份测试文件提交为 `0843919`。
  原有 seed.go 修改保持原样。email 无 Git 仓库，修复仅本地落盘。

原框架组与合并远端安全修复后的候选组**分别**完整执行了一次四仓验收。
候选组每份副本的 go.mod 明确指向候选 worktree，而不是旧工作区或发布版本：

| 项目 | 候选组 pass | skip | fail |
|---|---:|---:|---:|
| email | 116 | 0 | 0 |
| notification | 70 | 0 | 0 |
| rbac | 342 | 0 | 0 |
| ledger | 492 | 2（仅性能播种） | 0 |

四仓 build 及 PostgreSQL 集成包全部通过。330 个 Go/SQL 文件的测试副本与
修复后的源文件逐字节一致。数量仍为 test/subtest 事件，不能把性能播种 skip
计作功能通过。候选组日志目录为 `/tmp/appkit-downstream.gszr59bH`；原框架组
为 `/tmp/appkit-downstream.gMIPDXBG`，两组证据不可混用。

候选还通过：`make check`、隔离 PostgreSQL 18.6 上的全仓 `make test-db`
（含 race）、`make test-lint`、真实 `make test-rules`（0 skip）、五种脚手架
真实编译与架构检查、go.mod tidy 无漂移、相对 v0.7.2 的公开 API 零 incompatible。
新增真实 App.Run/loopback 验证严格边界与 MultiIssuer；规则来源测试验证离线
显式 SHA、上下文取消、锁外解析和拒绝把下游 HEAD 当作框架来源。

业务工程门禁必须单列，不能把测试通过写成“所有门禁已过”：email/RBAC 使用
各自原版本（v0.5.3/v0.7.2）的 lint 均通过；`GOWORK=off GOFLAGS=-mod=readonly`
下的 `make check` 均因**既有 go.sum 缺少 CLI 的 x/mod 条目**失败。
用同版本的独立检查器诊断，RBAC 通过，email 的三份物化规则存在既有漂移。
此次没有借测试修复更新这些依赖元数据或规则。它们仍是业务仓库完整工程验收的
待办，不影响已记录的候选 build/race 结果，也不能从那些结果推断待办已消失。

候选合并的远端安全规则会改变默认启动行为；路由、安全配置、分区键与 RLS
迁移要求见 [RELEASE_CANDIDATE.md](RELEASE_CANDIDATE.md)。本次没有替业务项目
完成这些应用升级，更未上线或做生产数据迁移。

## v0.9.1 服务身份与业务接入收尾（2026-09-05）

本轮范围经确认是**框架与业务接入代码、隔离验收、框架版本发布**，不部署，
不连接或迁移现有业务数据库。上面的 v0.9.0 记录保留原时点结论，不代表本轮仍
缺少服务身份或仍受旧工具依赖问题阻塞。

### 框架

- `ServiceVerifier` / `ServiceSigner` 实现短期 Ed25519 服务 JWT：静态可信
  issuer/kid/subject、单一 audience、必填 exp/iat、用途隔离与显式委托策略。
  非空 tenant/merchant/partition 不能靠请求头或默认策略获得授权。
- bootstrap 支持声明式接收端公钥/精确委托规则及组合根注入，多用户签发方与
  internal/mixed 的配置在基础设施和模块装配前验证。迁移专用路径完全跳过
  HTTP 安全配置读取，连畸形的 HTTP 安全时长也不会干扰数据库专用命令。
- 生成 `NewSecureClient` 保留原 `NewClient` 签名；安全入口强制 HTTPS、
  origin、显式凭证 provider，拒绝重定向、不安全 TLS、cookie jar 与 trailers。
  每跳凭证重新取得，不转发用户或上一跳凭证。根边界的不可信头快照仅供冲突
  检查，不进入可信身份，也不穿过 ctx 防火墙。
- 真实 generated client → TLS 终止代理 → 严格 `App.Run` → 分类内部路由 →
  generated handler/wrapper，验证服务凭证及本地/远程契约语义；不能拿旧的
  无认证四 module 传输夹具替代这条身份链证据。
- `ConformWithMeta` 使用合法业务租户/分区跑全部一致性检查，并检查 Partition
  传播；不改变原 `Conform` 签名。签名委托策略仍独立于测试元数据。
- 两类 Makefile 模板以固定版本独立工具 module 执行 CLI，保留业务 GOFLAGS；
  实测含额外 CLI 依赖的本地模块代理，证明不会写业务 go.mod/go.sum。显式
  本地 CLI 覆盖、真实 `make dev` 与五种脚手架编译/check 也通过。

最终框架通过 `make check`、隔离 PostgreSQL 18.6 上的全仓 DB race、独立 lint
module、真实规则检查器 E2E（0 skip）、go mod tidy 无漂移。相对 v0.9.0 的
module 级 apidiff 零 incompatible；使用 CI 固定的 e88cd73687aa 修订（本地缓存
中的同修订 canonical pseudo-version），不更换比较工具或忽略不兼容输出。
DB 实例均为临时 Unix socket 集群，运行后停止并清理。

### 四个现有项目的代码升级

- email / notification 内部路由要求已验证服务主体及非空租户，请求 tenant 必须
  与签名范围一致；公开 ping、追踪和 webhook 保持各自真实语义。email webhook
  验 HMAC 后以渠道与回执 ID 联合定位消息，并保留归属复核。
- RBAC 的 schema 路由改用 Partition，TenantID 仍是业务租户；内部契约验证
  服务范围，真实 TLS 契约对拍使用标准安全客户端。公开登录 realm 只选择已
  配置命名空间，不授予租户/用户/服务身份，不能覆盖已签名分区。受保护路由
  先认证及执行原领域权限语义，再查幂等缓存；支持原有通配及 view/read 权限。
- ledger 先验证 Actor 与原权限，再在每次缓存重放前复核资源归属；幂等 scope
  由版本化、逐字段编码的 partition/tenant/user/ledger ID 组成。回归覆盖跨
  租户/用户、合法重放、权限撤销、资源归属变动和删除。旧 scope 缓存不回退
  读取，后续业务上线需协调旧请求重试窗口，详见业务安全升级文档。
- 四个业务 Makefile 的 CLI 依赖隔离及物化规则漂移已修复；临时 v0.9.0 工具
  基线上的 `GOWORK=off GOFLAGS=-mod=readonly make check` 均通过。框架候选
  build/race 使用副本中的本地 replace；业务最终发布依赖与规则来源应在
  v0.9.1 两个模块 tag 可解析后同步，并另行执行最终只读 check/lint。

没有改历史迁移 SQL，没有将共享 provider、后台任务或 ledger ID 机械改成
RLS tenant。业务原有未提交内容保留；email 仍无 Git 仓库。业务代码本地更新
不等于业务仓库已提交、发布或部署。

最终源码副本均 build 通过，并在各自隔离 PostgreSQL 18.6 数据库运行 race：

| 项目 | pass（test/subtest） | skip | fail |
|---|---:|---:|---:|
| email | 135 | 0 | 0 |
| notification | 78 | 0 | 0 |
| rbac | 376 | 0 | 0 |
| ledger | 504 | 2（仅性能播种） | 0 |

RBAC/ledger 最终证据在 `/tmp/appkit-downstream.VLb9wr0E`；email/notification
在回执联合查询与错误包装修复后重新完整运行，最终证据在
`/tmp/appkit-downstream.HB1U1Lcx`。两组副本的 Go/SQL 文件均逐字节核对当前
业务源码一致，RBAC 原有 seed.go 修改的 SHA-256 保持不变。不能把表中的
父子测试事件算作独立顶层用例，也不能把 ledger 的性能播种 skip 算作功能通过。

## 有意保持的边界

兼容检查针对 AppKit 的 contract.yaml 生成模型与这组消费方式，不能证明业务语义、
任意 OpenAPI、无键字段初始化或任意新旧客户端/服务端滚动混用兼容。
数据库验证是本次运行的 PostgreSQL 版本，不是全部驱动/数据库/操作系统认证。

`apply` 不执行部署、数据库迁移或发布；`plan schema -allow-temp-db` 是明确授权的
临时数据库执行入口，不能称为“无外部副作用”。它捕获全部迁移与文档目录成员，
仅删除带生成头的陈旧文档，apply 不再连接 DB。分区域文档是逻辑模板，未对运行
中的分区做巡检。SQL 执行权限与信任边界见 [AGENT_WORKFLOW.md](AGENT_WORKFLOW.md)。

消费方绑定 manifest、完整 catalog/resolver、签名制品、沙箱及状态迁移平台不属于
当前基线的前置条件。业务数据升级演练、生产发布仍需各项目自己的验收。
框架整合保存在本地候选分支 `codex/appkit-reuse-release`，原工作区不被覆盖。
上述本地验收结束时尚未推送或发布。之后已确认 v0.9.0 的版本与范围；正式发布
通过受保护 PR 与远端 CI，最终状态以 GitHub tag / Release 为准。

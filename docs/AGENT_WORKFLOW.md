# AppKit：模块复用与 Agent 变更流程

## 实施方向

AppKit 是现有项目和后续可复用模块的主运行时。保留独立 Go module、契约仓库、
组合根和现有 `Target` 部署方式；版本继续由 `go.mod` / `go.sum` 管理。
从 mkit 吸收命名实例、确定性文件计划及恢复机制，不引入 mkit 运行时依赖。
mkit 保留为设计与机制参考，不再作为需要同步建设的第二套业务底座。

本轮只改框架，不迁移或实现 RBAC、订单、ledger、email、notification 等业务。
既有 tenant / partition 机制保持独立，业务 merchant_id 不加入通用实例标识。

## 同一契约的多个实例

新增 `ProvideNamed`、`ProvideValueNamed`、`ProvideContractNamed`、`ResolveNamed`、
`MustResolveNamed`、`RemoteNamed`。它们以 `(Go 类型, 名字)` 标识实例，
旧 `Provide` / `Resolve` / `Remote` 的无名绑定保持原有行为。

在模块的 Register 中登记，依赖构造器或 Setup 中解析。例如下面两个 string 值
演示实例键的独立性，实际契约接口应使用 `ProvideContractNamed` 加生成 wrapper：

```go
appkit.ProvideValueNamed[string](reg, "primary", "https://primary.example")
appkit.ProvideValueNamed[string](reg, "secondary", "https://secondary.example")
primary, err := appkit.ResolveNamed[string](reg, "primary")
```

名字必须匹配 `[a-z][a-z0-9._-]*`，不会自动修正空白或大小写。解析只查精确名字：
同名本地实现优先，同名 `RemoteNamed` 兜底；缺失就报错，不回退到无名或其他实例。
具名本地依赖与无名依赖共享启动解析、缓存和循环检查，未使用的远程实现仍惰性构造。
重复本地注册在 Register 时拒绝，重复 `RemoteNamed` 在应用注册阶段拒绝。

这是实例选择 API，不是消费方 binding manifest，也不自动实例化同一 Module 多次。
`Module.Name()`、数据库 schema、路由挂载和后台任务名字仍须由组合根正确区分。
名字不是 tenant、partition、merchant 或授权边界。具体安全隔离见 TENANTS.md。

## 先计划，再应用

以下命令在 AppKit 仓库内运行，用现有契约夹具验证完整流程。需要 Go 和 `mktemp`；
计划保存在工作区之外，避免输出重定向影响被检查的文件。

```sh
task_workspace=$(mktemp -d)
task_plan_dir=$(mktemp -d)
cp internal/gen/testdata/contract.yaml "$task_workspace/contract.yaml"
go run ./cmd/appkit plan contract -dir "$task_workspace" \
  -in contract.yaml -target generated > "$task_plan_dir/contract-plan.json"
go run ./cmd/appkit apply -dir "$task_workspace" \
  -plan "$task_plan_dir/contract-plan.json"
go run ./cmd/appkit gen contract -check \
  -in "$task_workspace/contract.yaml" -dir "$task_workspace/generated"
# 相同计划、相同最终状态：返回 replayed。
go run ./cmd/appkit apply -dir "$task_workspace" \
  -plan "$task_plan_dir/contract-plan.json"
```

已安装 `appkit` 后，在含 `.appkit.yml` 的域仓库根目录同步规则：

```sh
task_plan_dir=$(mktemp -d)
appkit plan sync -dir . > "$task_plan_dir/sync-plan.json"
# 审查 JSON 后执行；也可用 -digest 绑定审查时记录的 planDigest。
appkit apply -dir . -plan "$task_plan_dir/sync-plan.json"
appkit sync -dir . -check
```

`plan` 不写目标文件或锁文件。它使用原有生成器，记录输入文件和每个输出的
存在性、权限、内容指纹，以及预期最终状态。未变文件用 `assert`，不会重写；
更新保留文件权限，新文件使用 0644。`sync` 只涉及三份规则文件及 `.appkit.yml`；
`contract` 只涉及五份契约产物及输入 YAML。不会读取/执行项目里的任意生成脚本。
此外支持 `plan events` / `plan errors` / `plan wrap` / `plan new`。
`plan schema` 是需要单独授权的例外：不写仓库文件，但会执行临时数据库操作，见下文。

### 事件、错误码、wrapper 与新项目

下面命令在 AppKit 仓库内运行。单文件生成使用 `-target` 指定输出 .go 文件；
wrapper 的 `-in` 明确选择包含接口声明的一个源文件，不枚举会变化的源目录。
其输入和输出必须在同一包目录；生成后依然需要编译整个包。

```sh
task_workspace=$(mktemp -d)
task_plan_dir=$(mktemp -d)
cp internal/gen/testdata/events.yaml "$task_workspace/events.yaml"
cp internal/gen/testdata/codes.yaml "$task_workspace/codes.yaml"
go run ./cmd/appkit plan events -dir "$task_workspace" \
  -in events.yaml -target events.gen.go > "$task_plan_dir/events.json"
go run ./cmd/appkit apply -dir "$task_workspace" -plan "$task_plan_dir/events.json"
go run ./cmd/appkit plan errors -dir "$task_workspace" \
  -in codes.yaml -target codes.gen.go > "$task_plan_dir/errors.json"
go run ./cmd/appkit apply -dir "$task_workspace" -plan "$task_plan_dir/errors.json"
cp internal/gen/genfixture/service.gen.go "$task_workspace/service.gen.go"
go run ./cmd/appkit plan wrap -dir "$task_workspace" -in service.gen.go \
  -target wrap.gen.go -iface Service -system greet > "$task_plan_dir/wrap.json"
go run ./cmd/appkit apply -dir "$task_workspace" -plan "$task_plan_dir/wrap.json"
go run ./cmd/appkit plan new domain demo -dir "$task_workspace" \
  -target demo -tenant -partitioned > "$task_plan_dir/new.json"
go run ./cmd/appkit apply -dir "$task_workspace" -plan "$task_plan_dir/new.json"
go run ./cmd/appkit plan new system demosystem -dir "$task_workspace" \
  -target demosystem > "$task_plan_dir/system.json"
go run ./cmd/appkit apply -dir "$task_workspace" -plan "$task_plan_dir/system.json"
```

`plan new` 的 `-dir` 是已存在工作区根，`-target` 是其下的相对目录（可用 `.`）。
计划时目标须不存在或为空；计划只包含 create，不能覆盖任何已有生成路径。
apply 前后新增的无关文件不属于计划目标，框架不会删除它们，也不承诺冻结整个目录。
新项目的参数和内置模板体现为完整计划内容，不会在 apply 时重新渲染。
普通 `new` 与 plan 使用同一个 renderer，基础迁移仍只调用框架库函数取得 DDL。

`apply` 验证计划格式和指纹，再在工作区锁内复核前置条件。输入或输出被修改会返回
`workspace_conflict`，不得通过手改计划里的摘要解决，应重新计划并审查。
无关文件变化不阻断应用；`snapshotDigest` 是所选文件集摘要，不是整个仓库摘要。
带目录 guard 的 schema 计划还绑定指定目录的完整递归成员清单。

### Schema 文档计划与分区逻辑模板

在域仓库中，使用一个**可丢弃的本地 PostgreSQL 实例**，连接用户需有建库权限：

```sh
task_plan_dir=$(mktemp -d)
# TEST_DATABASE_URL 由本地测试环境提供；不要指向生产或共享业务实例。
appkit plan schema -dir . -allow-temp-db -timeout 2m \
  > "$task_plan_dir/schema.json"
# 审查后执行；apply 不使用 DSN，也不会再次执行迁移。
appkit apply -dir . -plan "$task_plan_dir/schema.json"
appkit schema -dir . -check
```

`-allow-temp-db` 显式允许在随机命名的一次性数据库中执行**可信迁移 SQL**。
这不是 SQL 沙箱：数据库角色能执行的集群级操作、扩展或外部访问不由 AppKit 限制。
因此不可把未知仓库的 SQL 交给有权访问业务实例的账号。DSN 来自 `-dsn` 或
`TEST_DATABASE_URL`，不写入计划；连接错误隐藏连接详情。迁移/输出本身仍可能含
敏感内容，计划文件须按源码级别保管。取消后清理使用独立短时限，清理失败明确
报出临时库名，不能把操作失败当作“从未创建数据库”。

规划时捕获 `.appkit.yml`、`db/migrations` 的成员与全部文件内容，再将不可变内存
快照交给生产迁移 runner。`db/schema` 的递归成员（包括空目录）及既有文档同样
在渲染前捕获。新增、删除、改名、改内容都会使旧计划失效，包括新的迁移 SQL。
输出为 `db/SCHEMA.md` 和 `db/schema/`：过时生成文件会明确列为 `delete`；
已有空目录保留。任何旧输出缺标准生成头都会在连接 DB 前拒绝规划，避免覆盖或
清理手写文件；请先将手写内容移到生成目录之外。

`partitioned: true` 自动选择 **logical-template**：将无前缀迁移在代表 schema
（配置中的 domain）回放一次，所有产物明确标记为逻辑模板，不枚举、不检查实际
运行中的分区。分区加 tenant 的 RLS/策略也如实呈现。普通域保持原有文档格式。
直接 `appkit schema` 支持 `-mode auto|schema|logical-template` 作为配置一致性断言；
已启用的分区域 `-check` 会严格检查，尚未启用仍保留清晰的 notice。

schema 的输入与已有输出累计最多 8 MiB，计划写入载荷另有 8 MiB 限制。
应在迁移可复现的 PostgreSQL 版本、扩展与模板库环境中规划；含随机/外部状态的
SQL 可能导致不同规划结果，不承诺跨环境的绝对纯函数。apply 只应用已捕获的文档字节，
不代表业务数据库已经迁移，也不证明运行中 schema 没有漂移。

`gen contract -check` 独立执行只读漂移检查，列出缺失、内容变化或非普通文件；
不会创建输出目录，也不删除目录里未知文件。它验证生成一致性，不验证两个契约版本
是否向后兼容。跨版本兼容仍需要现有 apidiff / oasdiff 门禁与调用测试。

### 契约升级检查

```sh
go run ./cmd/appkit contract-check \
  -base internal/gen/testdata/contract.yaml \
  -candidate internal/gen/testdata/contract.yaml
```

实际升级时将 `-base` 换成已发布基线、`-candidate` 换成候选 YAML。
命令只读、stdout 输出版本化 JSON；不兼容返回 `error.code=contract_incompatible`、
有序 `data.issues` 和退出码 3。帮助为 0，参数错误为 2，读取/解析失败为非零。

这是 AppKit 模型的保守门禁：拒绝旧方法/类型/字段删除、方法路径/参数形态、字段
类型/requiredness、system/package 与重试语义改变；新增方法扩大 Service 接口，
会破坏旧实现，同样拒绝。允许新 DTO 及已有请求/响应里的可选字段扩展；首次给
无请求/无响应方法添加字段会改变 Go 签名，仍拒绝。`required` 字段新增也拒绝。
它不推断实现的业务语义，不替代消费者编译、调用测试或通用 OpenAPI 兼容检查。
四个独立 Go module 的真实升级验证见 [FRAMEWORK_ACCEPTANCE.md](FRAMEWORK_ACCEPTANCE.md)。

## Agent JSON 与退出码

`plan` 成功输出唯一规范 JSON 文档，`kind` 为 `WorkspacePlan`。
纯文件计划的 `apiVersion` 保持 `appkit.dev/workspace-plan/v1alpha1`；
schema 使用带 `directoryGuards` 的 `appkit.dev/workspace-plan/v1alpha2`。
新 apply 仍接受规范的旧版计划；旧 apply 不认识新版时明确拒绝。
包含 `planDigest`、`snapshotDigest`、`finalDigest` 和有序 `changes`；
写入内容位于 `contentBase64`。可解码查看内容，但不要重排、格式化后覆盖原计划文件：
解析器要求原始规范字节，包括结尾换行。`-digest` 是可选的人工审查摘要校验。

`apply` 成功及新命令的失败响应使用 `appkit.dev/agent-result/v1alpha1` /
`CommandResult`，含 `command`、`ok`，以及 `data` 或 `error`。
成功数据含 `planDigest` 和 `disposition`（`committed` / `replayed`）；
失败含稳定 `error.code` 与供人阅读的 `message`。进入 apply 内核后的失败同时
保留 `data.planDigest` 和已知 `data.disposition`：例如文件已提交、清理或解锁失败，
仍会返回 `ok: false` 但标明 `committed`。缺少 disposition 或没收到响应不能推断
“没有写入”；保留原计划并按恢复流程处理。stdout 仅输出 JSON，
帮助和 flag 诊断在 stderr；`-h` 只输出帮助、不输出结果文档。
原有 CLI 命令未统一改为 JSON。

| 退出码 | 含义 |
|---|---|
| 0 | 成功或帮助 |
| 1 | 操作失败或宿主不支持所需锁 |
| 2 | 参数、路径或计划无效 |
| 3 | 工作区前置条件冲突 |
| 4 | 恢复或回滚需要处理 |
| 5 | 取消或超时 |

`-timeout` 默认 30s，覆盖锁等待和协作式执行取消；无法强制中断每一个操作系统调用。
使用编译后的 `appkit` 读取这些退出码：`go run` 会将子程序失败包装成自身退出码。

## 恢复与信任边界

应用阶段在同一文件系统暂存新文件、备份旧文件，并在第一次公开替换前持久化日志。
提交错误会尝试逆序回滚；进程异常退出后，应保留原计划并用完全相同的 `apply` 重试。
不同计划遇到未恢复事务会拒绝执行。若重试仍报恢复错误，先保存工作区和日志做诊断，
不要手删 `.appkit-workspace-*`。计划文件及恢复目录可能含源码，应按仓库敏感度保管。

这是带恢复的多文件提交，不是所有外部读者可见的原子文件系统事务。
macOS/Linux 上，遵守同一协议的读写使用目录 inode 的 flock 排他/共享锁；
不遵守锁的编辑器和旧直接写入命令仍可并发修改，框架尽可能检测目标漂移，
不能保证阻止恶意本地写入者。不要混用并发的直接生成与 apply。
不支持该锁协议的平台明确拒绝，不降级成无锁写入。

计划摘要证明内容一致性，不证明作者身份或批准权限。`apply` 是显式文件变更工具，
能执行有效计划中的 create/update/delete/assert；schema 入口会为过时的生成文档产生删除。
只应用来源可信且已审查的计划，不把任意外部 JSON 当作授权，也不要在计划中放入密钥。
`apply` 不执行数据库迁移、部署、业务模块升级、远程调用或制品签名校验；
只有显式授权的 `plan schema` 会在临时数据库执行迁移。

## 后续边界

下一阶段可在真实组合场景需要时增加消费方 binding 清单、模块描述及更完整的契约版本治理。
本轮未承诺移植 mkit 的完整 catalog / resolver / signed
artifact / sandbox / state-transfer 平台，也不把这些研究项作为 AppKit 当前改造的前置条件。

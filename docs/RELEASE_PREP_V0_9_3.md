# v0.9.3 修复与本地验收记录

本文记录进入发布流程前的修复与本地验收；最终发布状态以远端 tag 和 Release 为准。

## 基线与范围

- 工作分支：`codex/appkit-v093-prep`，基于远端 main `86e1cb3`，包含已发布
  `v0.9.2` 的 refs、严格 HTTP 安全边界、服务认证与 workflow SHA 固定能力。
- 旧 AppKit 工作区未清理、覆盖、切分支或提交。处理前后的 291 个源码/文档文件
  SHA-256 一致，原有 114 个未提交/未跟踪文件保持原样。
- 只提取尚未发布的 schema-tool 源码与必要脚手架接线，没有把旧目录当成新版覆盖。
- 本地验收阶段没有提交、推送、PR、tag、Release、部署或业务库操作。本文是修复交接记录，
  不是 annotated tag / CHANGELOG 的替代事实源。

## 修复内容

### 可选 sqlc schema 快照

新增 `appkit schema-tool`，把框架维护的 Go 工具安装到域的
`internal/postgres/schematool`。在一次性随机数据库回放迁移，生成
`db/schema.sql` 与 `db/schema.lock.json`；支持离线摘要检查和数据库重放检查。
新脚手架保持 `sqlc.yaml` 默认读取 migrations，首次采用快照需显式切换。

修复安装后的测试误伤：未采用快照的多 SQL 项、多 schema 输入、非 PostgreSQL
和旧格式配置不再被快照专用限制拒绝。任何实际 SQL 输入指向快照，或快照/lock
已存在，都会启用严格校验；覆盖 YAML alias/merge、相对路径与多项绕过。
采用后仍只支持一个 PostgreSQL SQL 项及一个快照输入。

工具不更改现有域依赖、Makefile、sqlc 配置或已应用迁移；`make schema` 仍是文档
生成，`make schema-sqlc` 才是新脚手架的快照生成。具体入口见 GUIDE 对应章节。
迁移 SQL 和测试服务器必须可信；快照不是数据库备份，也不是任意 SQL 的沙箱。

### 嵌套事务范围固定

`pgtx.Do` 记录 tenant 模式、tenant ID、read-all、路由模式、解析后的 schema，
并将其绑定到实际事务/savepoint 句柄。嵌套调用必须匹配，拒绝会发生在 Begin
savepoint 与业务 callback 之前；外层可处理拒绝后继续，数据库范围不变。
同范围、不同 Transactor 的正常 savepoint 组合继续支持。

兼容收紧：不再接受手工 `tx.With(ctx, rawPgxTx)` 后进入 `pgtx.Do`；无法核验
作用域或替换过句柄的 ctx 会被拒绝。改用最外层 `pgtx.Do` 创建事务，并沿用其
callback ctx。此检查不是业务鉴权，也不防止受信代码直接执行 SET LOCAL SQL。

## 本地验收（2026-09-06，Asia/Shanghai）

- `make check`：fmt、vet、build、全仓非 DB 测试通过。
- `make test-lint`：独立 lint module 通过。
- `make test-rules`：两个真实规则检查器实际运行，干净脚手架通过，植入违规被检出。
- `go mod tidy -diff`：无依赖漂移，未修改 go.mod/go.sum。
- 私有 PostgreSQL 18.6、Unix socket、关闭 TCP：全仓 `go test -race -count=1 ./...`
  通过。新增 schema catalog 重放、伪造哈希拒绝与四种脚手架采用测试均实际运行。
- 四种脚手架均使用已发布 AppKit v0.9.2，完成 schema 生成、sqlc 生成、来源检查、
  数据库重放、架构检查与生成项目编译，不通过本地 replace 假装发布依赖可用。
- 用 Go overlay 将实现替换为 v0.9.2，再运行新隔离回归：旧实现实际放行 tenant
  与 schema 重绑；修复后拒绝，并验证外层数据库设置不变、callback 未执行。
- 事务句柄比较另覆盖不可比较类型及 interface 字段包含 slice 的情形，拒绝而非 panic。
- 与 CI 同版 apidiff 比较 v0.9.2：29 个非 internal 包，0 incompatible、0 API changes。
  该结果仅证明公开 API 类型兼容，不代表上述安全行为收紧没有调用方迁移成本。
- 所有本轮测试数据库均已停止并移除；没有使用任何既有 TEST_DATABASE_URL。

发布仍须按 AGENTS.md 走实现 PR、required CI、合并、annotated 主 tag 与同提交
lint tag、CHANGELOG 镜像及 Release 流程；本地验收不代替这些远端检查。

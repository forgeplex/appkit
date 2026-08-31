# appkit —— AI Agent 操作规程（框架仓库）

这是 forgeplex 的 Go 后端框架本体。改这里的代码影响所有下游域仓库，
规则比业务仓库更严。设计原理见 docs/DESIGN.md，使用方视角见 docs/GUIDE.md。

## 不可破坏的承诺

1. **公开 API 向后兼容**：appkit 经 MVS 依赖钻石被全部域仓库共享，破坏性
   变更 = 全域联动升级。CI 有 apidiff 门禁（基线 = 最新发版 tag）。
   新能力用「新增」表达，不改已有签名/语义。
2. **根包只依赖标准库**：appkit.go/registry.go/app.go/run.go 的 API 面
   不得出现 gin/pgx/otel 等第三方类型（http.Handler、fs.FS、context 级抽象）。
   `tx` 包同样零驱动依赖（pgx 实现在 `pgtx`）。
3. **约束左移**：新增架构规则时按 编译器 > 代码生成 > 运行时守卫 > lint/CI >
   约定 的顺序找落点，并在 docs/DESIGN.md §7 的对照表里诚实标注强度。
4. **进程内 ≠ 裸调用**：contract 边界的四件套（事务守卫/ctx 防火墙/超时/
   错误规范化）保证单体与微服务语义一致，任何改动不得削弱。

## 布局速查

- 运行时：根包（Module/Registry/Run）、`contract`、`config`、`apperr`、
  `health`、`telemetry`、`httpserver`、`tx`/`pgtx`、`pgmigrate`、`outbox`、
  `idem`、`money`、`audit`
- 工具链：`cmd/appkit` + `internal/cli`（薄壳）+ `internal/{scaffold,archcheck,gen,doctor}`
- 规则分发：`ruleset`（golangci/arch-lint/CI 模板，appkit sync 物化到各仓库）
  ——appkit 只生产、自己不消费，golden 测试只锁「模板文本没变」，锁不住
  「规则从写下来那天就是错的」；**改规则模板必跑 `make test-rules`**
- `lint/`：独立嵌套 module（go/analysis analyzer），有自己的 go.mod
- `internal/scaffold/templates/`：生成骨架的模板——**改模板必改
  internal/scaffold 的测试**，且生成物必须自身通过 appkit check 与编译

## 关键纪律

- 基础设施表 DDL 的唯一事实源是库函数（`outbox.MigrationSQL` 等），
  模板/文档里不落 DDL 副本；
- 错误一律 `*apperr.Error`，错误身份 = 错误码；SQL 标识符拼接必须过
  白名单校验或 `pgx.Identifier` 转义；
- 金额禁 float；事务经 `tx.Do` 回调、句柄藏 ctx；
- 生成物（sqlc/golden）禁手改，CI 做 drift check；
- 文档改动要与行为同步：GUIDE 里每条命令都必须真实可执行。

## 验证

```sh
make check                      # fmt + vet + build + test（DB 集成测试缺省 skip）
make test-db TEST_DATABASE_URL=postgres://...   # -race 且含 DB 集成测试
make test-lint                  # lint/ 是嵌套 module，不在 ./... 里，必须单独跑
make test-rules                 # 改 ruleset/templates 后：真跑两个检查器验规则（需网络）
go run ./cmd/appkit new domain t -dir /tmp/t   # 改脚手架后：生成物可编译且自过 check
```

完成的定义：上述全绿 + 若动了公开 API，apidiff 相对最新 tag 零 incompatible。

## 发版

版本更新说明的事实源是 **annotated tag message**——正文要写清这一版干了
什么、为什么；CHANGELOG.md 与 GitHub Releases 都是它的镜像。三件套顺序固定：

```sh
git tag -a vX.Y.Z -F /tmp/msg            # 1. tag：正文即事实源
make changelog && git add CHANGELOG.md   # 2. 重生成镜像（禁手改）并提交
gh release create vX.Y.Z --verify-tag \
  --title vX.Y.Z --notes-file /tmp/msg   # 3. Release 正文 = tag message
git push origin main vX.Y.Z
```

版本号走 SemVer，但 0.x 期间号是节拍不是承诺：**minor 留给改默认行为/
语义、或值得每个升级者读一遍说明的节点**（v0.5.0 焊金额边界是例），
纯加法的可选注入口（新 Option 字段、新运维命令）、内部重构与修 bug
一律 patch——每个小注入口都 minor 会让号虚涨，读者看到 minor 该期待
实质节点。apidiff 门禁保证向后兼容，正常不会出现需要 major 的破坏性
变更。

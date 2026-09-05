# CI 供应链与仓库保护

## 不可变引用

`appkit sync` 从 `go mod download -json` 的 module provenance 读取 appkit release
对应的 Git commit，把完整 40 位 SHA 写进域仓库的 reusable workflow；release 版本只保留
在同一行上方作为审计注释。源码联调使用 AppKit 自身构建来源，或在校验工作区及
HEAD 的 module 身份后读取 AppKit worktree 的 `HEAD`；不使用下游仓库的提交。
不会生成 `@main`，解析不到完整 commit 时直接失败。

`sync` / `new domain` 及其 `plan` 形态支持显式 `-workflow-ref`，只接受完整
40 位 SHA；适合经人工核验的离线构建。显式值通过格式检查不等于来源证明，
使用者仍须核验它属于 AppKit 且可获取。自动解析在工作区锁外执行，支持取消与
超时；Go 下载在隔离临时目录执行，不修改调用方 go.mod/go.sum，但可能更新模块缓存。

appkit 自身 workflow 的第三方 Action、Go 工具和 Postgres service image 也固定到
commit 或 digest。Dependabot 每周更新 GitHub Actions 与两个 Go module；Renovate 的
Docker digest 规则负责 service image。所有升级都必须走独立 PR 和完整 CI。

## GitHub ruleset

仓库管理员须在 **Settings → Rules → Rulesets** 建立并启用：

1. `main` branch ruleset：要求 pull request、分支合并前保持最新、required check `ci`，
   禁止 force-push 和删除；bypass 只给仓库管理员的紧急通道，并要求记录理由。
2. `v*` 与 `lint/v*` tag ruleset：限制创建，禁止更新和删除；只有发版负责人可创建。

ruleset 是 GitHub 侧控制面，不在仓库文件里假装已生效。每次权限或 CI 名称变更后，
管理员都要复核规则目标、required check 名称和 bypass actor。

## 紧急升级

1. 新建专用分支，只更新受影响的 Action SHA、Go revision 或 image digest；在旁边保留
   人类可读版本注释。
2. 核对上游 release/changelog 与 commit 来源，运行完整 CI；不得临时改回 major tag、
   `@latest` 或 `@main`。
3. 通过受保护 PR 合入。若 reusable workflow 有变化，发布 appkit 新版本，再在下游升级
   `go.mod` 并运行 `appkit sync`；确认生成 diff 同时更新版本注释与完整 SHA。

## 回滚

1. 找到最后一个已知安全的 appkit release commit 或依赖 digest，新建回滚 PR；不要移动
   现有 release tag。
2. appkit 本身回滚依赖后跑完整 CI。下游把 `go.mod` 降回对应 release，再运行
   `appkit sync`，由生成器恢复匹配的 workflow SHA。
3. 若必须使用 ruleset bypass，限定单次操作、记录事件与理由，完成后立即撤销临时 actor；
   随后用普通 PR 补齐审计记录和长期修复。

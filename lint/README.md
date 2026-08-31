# appkit-lint

forgeplex 自研 go/analysis 检查器集合（独立嵌套 module，避免把 `golang.org/x/tools`
依赖污染进业务依赖图）。规则定位见 appkit `docs/DESIGN.md` §7。

## 检查器

| 检查器 | 规则 | flag |
|---|---|---|
| `moneyfloat` | 禁止 `float32`/`float64` 出现在 struct 字段类型、变量/常量声明类型、函数参数与返回值、显式类型转换中。金额与业务数值改用 `money.Money` 或 `decimal` | `-scope`：正则，仅检查 import path 匹配的包；为空检查全部包 |
| `ctxstruct` | 禁止 struct 字段类型为 `context.Context`（ctx 只走函数参数，Go 官方反模式） | 无 |
| `decjson` | 禁止 decimal 包类型出现在带 json tag 的 struct 字段上：金额的 JSON 形态必须是字符串（DTO 层显式转换）。裸 decimal 出站受 `MarshalJSONWithoutQuotes` 全局开关影响（任何依赖翻掉它，全进程金额静默变 JSON number），入站会接受 JSON number（0.1 走 float64 语义） | 无 |

三个检查器都默认**只查生产代码**（`_test.go` 不查），`-<name>.tests=true`
连测试一起查——测试夹具低一档，且 domain-ci.yml 经 @main 被全部存量域仓库
共享，规则升级不该让它们的测试文件一夜变红。

注意：`moneyfloat` 故意不区分"金额"与"合法数学计算"——time 换算、统计学
代码里的 float 同样会被报告。请用 `-scope` 把检查圈定在业务包，例如
`-moneyfloat.scope='^github.com/forgeplex/ledger/internal/ledger'`，
而不是期待检查器自行豁免。

## CI 接入（已生效）

`domain-ci.yml` 有一步 appkit-lint，版本随域仓库 go.mod 钉的 appkit 走
（`go list -m` 取版本，go.work 联调仓库退 @main）；域仓库的 `make lint`
用同一套取法——本地与 CI 跑的是同一个二进制。

## 用法：go vet -vettool

```sh
go install github.com/forgeplex/appkit/lint/cmd/appkit-lint@latest

# 全部规则
go vet -vettool=$(go env GOPATH)/bin/appkit-lint ./...

# moneyfloat 圈定业务包
go vet -vettool=$(go env GOPATH)/bin/appkit-lint \
    -moneyfloat.scope='^github\.com/forgeplex/ledger/internal/ledger' ./...
```

也可以直接运行（multichecker 支持单独启用某个检查器）：

```sh
appkit-lint -ctxstruct=false -moneyfloat.scope='^scoped/pay$' ./...
```

## golangci-lint module plugin 接入

golangci-lint v2 支持 [module plugin](https://golangci-lint.run/plugins/module-plugins/)：
在 `.custom-gcl.yml` 里声明 `github.com/forgeplex/appkit/lint` 为 plugin module，
提供一个返回 `[]*analysis.Analyzer{moneyfloat.Analyzer, ctxstruct.Analyzer}` 的
`register.Plugin` 入口包，执行 `golangci-lint custom` 构建定制二进制，然后在
`.golangci.yml` 的 `linters.settings.custom` 下以 `type: module` 启用，settings 里
可传 `scope`。该配置由 `appkit sync` 物化到各域 repo（里程碑 4）。

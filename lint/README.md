# appkit-lint

forgeplex 自研 `go/analysis` 检查器集合（独立嵌套 module，避免把 `golang.org/x/tools`
依赖污染进业务依赖图）。规则定位见 appkit `docs/DESIGN.md` §7。

## 检查器

| 检查器 | 规则 | flag |
|---|---|---|
| `moneyfloat` | 禁止 `float32`/`float64` 出现在 struct 字段类型、变量/常量声明类型、函数参数与返回值、显式类型转换中。金额与业务数值改用 `money.Money` 或 `decimal` | `-scope`：正则，仅检查 import path 匹配的包；为空检查全部包 |
| `ctxstruct` | 禁止 struct 字段类型为 `context.Context`（ctx 只走函数参数，Go 官方反模式） | 无 |
| `decjson` | 禁止 decimal 包类型出现在带 json tag 的 struct 字段上：金额的 JSON 形态必须是字符串（DTO 层显式转换）。裸 decimal 出站受 `MarshalJSONWithoutQuotes` 全局开关影响（任何依赖翻掉它，全进程金额静默变 JSON number），入站会接受 JSON number（0.1 走 float64 语义） | 无 |
| `nogoroutine` | 禁止生产代码直接使用裸 `go` 语句；后台任务请改用 `reg.Worker` 托管（关停等待、超时与异常退出上报） | `-scope`：正则，为空检查全部包 |
| `authheader` | 禁止直接从 `http.Header` 读取受框架保护的身份/凭证头（`Authorization`、`X-Step-Up`、`X-Tenant-Id`、`X-Partition`、`X-Caller`、`X-Actor-*`）；请改用 `appkit.ActorFrom(ctx)` 或 `callctx.From(ctx)` | `-scope`：正则，为空检查全部包 |
| `errcmp` | 禁止对 `err.Error()` 做字符串比对（`==`/`!=`/`strings.Contains`/`HasPrefix`/`HasSuffix`/`EqualFold`/`Compare`）；错误判定必须用 `apperr.Is` 或 `errors.Is`/`errors.As`（日志输出不受影响） | `-scope`：正则，为空检查全部包 |
| `pgtxbypass` | 禁止绕过 `pgtx.From(ctx, pool)` 直接在 `*pgxpool.Pool` 上调用 `Query`/`QueryRow`/`Exec`/`SendBatch`/`CopyFrom`（会逃出事务、分区域 `search_path` 与租户 RLS GUC；`Ping`/`Begin` 等管理方法放行） | `-scope`：正则，为空检查全部包 |
| `jsonany` | 禁止 `json.Unmarshal` 直接解码到 `any` / `map[string]any` / `[]any`（JSON number 会静默转成 `float64` 绕过金额防护）；请改用具名 DTO struct 或 `json.NewDecoder` + `UseNumber()` | `-scope`：正则，为空检查全部包 |
| `canonicalmoney` | 禁止在 HTTP 入站层直接调用 `decimal.NewFromString`/`RequireFromString`，强制使用 `money.ParseCanonical` 或 `money.ParseCanonicalDecimal` | `-scope`：正则，默认匹配 `internal/http` |
| `txparam` | 禁止在函数参数或 struct 字段中显式传递/持有 `pgx.Tx`（事务句柄须经 `tx.Do` 藏在 `ctx` 中，经 `pgtx.From(ctx, pool)` 获取） | `-scope`：正则，为空检查全部包 |
| `callctxwrite` | 禁止在 HTTP 层直接调用 `callctx.With` 覆写身份上下文（身份只能由 `authn` 重建） | `-scope`：正则，默认匹配 `internal/http` |
| `timenow` | 禁止直接调用 `time.Now()`，强制注入时钟以保证核心账务/计费逻辑的确定性单测 | `-scope`：正则，**为空时默认不启用（opt-in）** |

全部检查器默认**只查生产代码**（`_test.go` 不查），`-<name>.tests=true` 连测试一起查——
测试夹具低一档，且 `domain-ci.yml` 随版本推送到各域仓库，规则升级不该让测试文件一夜变红。
其中 `nogoroutine`、`authheader`、`errcmp`、`pgtxbypass`、`jsonany`、`canonicalmoney`、`txparam`、`callctxwrite`、`timenow`
同时自动跳过带标准 Go 生成头（`// Code generated ... DO NOT EDIT.`）的文件（如 `sqlc` / `schematool`）。

### 行级带理由豁免

确有合理例外时，可在目标行尾或紧邻上一行写统一豁免注释（缺理由会直接报错，与 `nolintlint` 对齐）：

```go
//appkit:ignore nogoroutine -- 有界并发 fan-out，由 errgroup 等待回收
go func() { ... }()
```

多个检查器可用逗号分隔：`//appkit:ignore nogoroutine,authheader -- <理由>`。

注意：`moneyfloat` 故意不区分"金额"与"合法数学计算"——time 换算、统计学
代码里的 float 同样会被报告。请用 `-scope` 把检查圈定在业务包，例如
`-moneyfloat.scope='^github.com/forgeplex/ledger/internal/ledger'`，
而不是期待检查器自行豁免。

## CI 接入（已生效）

`domain-ci.yml` 有一步 `appkit-lint`，版本随域仓库 `go.mod` 钉的 appkit 走
（`go list -m` 取版本）；域仓库的 `make lint` 用同一套取法——本地与 CI 跑的是同一个二进制。

## 用法：go vet -vettool

```sh
go install github.com/forgeplex/appkit/lint/cmd/appkit-lint@latest

# 全部规则
go vet -vettool=$(go env GOPATH)/bin/appkit-lint ./...

# moneyfloat / timenow 圈定业务包
go vet -vettool=$(go env GOPATH)/bin/appkit-lint \
    -moneyfloat.scope='^github\.com/forgeplex/ledger/internal/ledger' \
    -timenow.scope='^github\.com/forgeplex/ledger/internal/ledger' ./...
```

也可以直接运行（multichecker 支持单独开关某个检查器）：

```sh
appkit-lint -ctxstruct=false -moneyfloat.scope='^scoped/pay$' ./...
```

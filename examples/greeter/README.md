# greeter — appkit 双模式最小示例

不依赖数据库的完整示例：两个业务模块装配进同一个二进制，用 `-target`
在「单体」与「拆分」两种部署形态间切换。部署形态是启动参数，不是架构决策。

## 结构

| 路径 | 角色 |
|---|---|
| `greetapi/` | 契约包：接口 + DTO + 错误码。**顶替合约仓库生成物的位置**——真实项目中它由 psp-contracts 从 OpenAPI 生成、以独立 module 发版，提供方与消费方都只 import 它 |
| `greet/` | 提供方模块：`Provide` 一个 `greetapi.Service` 本地实现，`Mount GET /greet/{name}`；handler 里演示 apperr → `httpserver.WriteError` 统一出口 |
| `gateway/` | 消费方模块：`Resolve[greetapi.Service]`，每次调用经 `contract.Call` 过契约边界（守卫/防火墙/超时/错误规范化），`Mount GET /hello/{name}` |
| `main.go` | 组合根：flag `-target` → `config.Load` → `telemetry.Init` → `appkit.New(...).Run`；`appkit.Remote` 注册远程兜底 |

## 跑法

单体形态（两个模块同进程，gateway Resolve 到 greet 的本地实现）：

```sh
go run ./examples/greeter -target=all
```

拆分形态（greet 不在 target 集内，gateway 的 Resolve 落到 `appkit.Remote`
注册的远程兜底——示例用一个假 client 顶替，真实项目传 contracts 生成的
HTTP client 构造器，实现同一接口）：

```sh
go run ./examples/greeter -target=gateway
```

验证：

```sh
curl localhost:8080/greet/Ada              # {"message":"Hello, Ada!"}（target 含 greet 时）
curl 'localhost:8080/hello/Ada?lang=zh'    # {"message":"你好，Ada！","via":"gateway"}
curl localhost:8080/hello/Ada              # target=gateway 时可见 "(via remote greet)"
curl 'localhost:8080/greet/Ada?lang=fr'    # 422 problem+json，code=GREET_UNSUPPORTED_LANG
curl localhost:8080/readyz                 # 探针总是挂载，哪怕 target 里没有任何路由
```

## 配置

分层加载：`greeter.yaml`（工作目录下，可缺省）→ `GREETER_*` 环境变量覆盖：

```sh
GREETER_ADDR=:9090 GREETER_LOG__LEVEL=debug go run ./examples/greeter -target=all
```

设置 `OTEL_EXPORTER_OTLP_ENDPOINT` 后自动装配 OTLP trace/metric；
不设置则保持 noop，本地开发零成本。

## 示例演示的框架约束

- **跨模块只经契约类型**：gateway 只 import `greetapi`，看不到 greet 的实现。
- **进程内调用 ≠ 裸方法调用**：`contract.Call` 让本地调用同样带独立超时、
  ctx 防火墙与错误规范化；事务内发起契约调用直接失败（`TX_BOUNDARY`）。
- **错误身份 = 错误码**：`apperr.Is(err, greetapi.CodeUnsupportedLang)` 在
  本地实现与远程 client 两种形态下判定一致。

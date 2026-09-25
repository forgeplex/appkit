# ADR-0048：出站客户端与配置驱动观测

- 状态：Proposed（待评审；不冻结 Go API 名称和签名）
- 日期：2026-09-25
- 关联：[Epic #97](https://github.com/forgeplex/appkit/issues/97)、[Phase 0 #98](https://github.com/forgeplex/appkit/issues/98)、[已完成的宿主与流式 Epic #47](https://github.com/forgeplex/appkit/issues/47)
- 基线：`main@21b7eb80e2bc15972bbab78d60e88b077ea2fee9`，release `v0.9.9`

本 ADR 是新能力的架构提案。它描述语义、边界和测试要求；具体 Go 类型/函数签名由后续实现 issue 冻结。ADR 合并前可修改，未接受前不得据此宣称客户端 API 稳定。

## 背景

AppKit 已有契约代码生成的 Unary HTTP client、安全 `contract.NewSecureHTTPClient`、SSE 服务端 `POST + SSE` adapter、WSS 双向 client、`contract.ClientStream` 以及 Local/WSS streaming。SSE 远端 client 尚缺。流生命周期和 SSE/WSS Transport 已有自动 span/metric 埋点。

Bootstrap 已从 YAML 加载服务配置并自动初始化 Telemetry；但 `telemetry.Config` 不含 exporter endpoint 或独立 signal 开关，trace/metric SDK 当前主要由 `OTEL_EXPORTER_OTLP_ENDPOINT` 是否存在来门控。业务服务不应各自构造 OTel provider，也不应为框架 client 重复埋点。

AppKit 是共享 Go 框架：新增公开 API 必须纯加法；根包/tx 不能暴露第三方类型；既有导出结构体不能因追加字段破坏无键字面量调用方。AppKit 管通用传输、生命周期、安全和观测，不承接 Provider、Relay、Slack、Session、业务终态等应用语义。

## 目标与非目标

目标：

- 为普通 HTTP 与流式远端调用提供可复用、统一安全和自动观测的客户端基础。
- 补齐 SSE client，并复用既有 WSS client；让不同流形态共享生命周期与错误原则而不抹平协议能力差异。
- 从服务配置文件选择 trace/metric exporter、设置端点和采样；Bootstrap 负责自动装配与 shutdown flush。
- 以本地测试、fake collector、conformance 和 API 兼容门禁验证框架能力。

非目标：

- AppKit 不配置或实现任何业务 Provider/Relay/Slack API、凭证语义、重试业务策略、session、replay 或领域终态。
- 首期不增加 gRPC、任意 transport plugin、自动重连或持久化流。
- 首期不加入 OTLP Log exporter；现有日志继续由 AppKit 写 stdout，级别/格式使用服务配置。每种 signal 的启停仅适用于框架当前支持的 exporter。
- 不把真实下游部署或业务/UAT 混同为 AppKit 框架验收。

## 方案

1. **各协议独立实现和配置**：短期最少抽象，但连接池、安全、propagation、标签与超时会逐渐分叉。
2. **共享出站基础 + 协议适配器（推荐）**：HTTP、SSE、WSS 复用安全/transport/telemetry 基础；各 adapter 保留 Unary、server-stream、bidi 的语义。保留既有生成式 HTTP API 与 WSS Dial 行为，新增能力不迫使下游迁移。
3. **首期建立可插拔的通用远端 Stream 框架**：扩展性高，但当前会过早冻结 transport plugin、配置发现与通用流模型，也容易错误地将 SSE 单向能力说成双向。

选择方案 2。实现顺序为配置/兼容基线、共享 HTTP client 基础、SSE client、现有 WSS client 的统一装配、跨 adapter conformance 与观测验证。

## 决策提案

### 1. 客户端分层

- 已生成的 V1/V2 Unary HTTP client 保持现有签名和语义；不因新通用 client 强迫业务服务替换生成 API。
- 新增的通用 HTTP 基础负责安全 transport、连接复用、超时、trace propagation 和低基数 client 指标；不擅自加入自动 retry。调用方可注入受限的标准库 HTTP transport 配置，但不能绕过 secure client 的 TLS/redirect/credential 防护。
- WSS 复用 `httpserver.DialSecureWebSocket` 与生成式 `Dial<Method>WebSocketV2`，之后通过共享 outbound foundation 统一 metrics、trace 和配置来源；不另起一套协议实现。
- 客户端目标地址由消费方配置/组合根拥有；telemetry collector 地址属于 AppKit 的 telemetry 配置。AppKit 不把业务服务注册表或业务 endpoint 命名强加给各域服务。

### 2. Streaming client 与传输形态

- 共用终态、取消、错误规范化、预算和 conformance 原则；不改变既有 `contract.ClientStream` / `OpenLocal` 行为。
- SSE 首期实现与现有 AppKit server adapter 匹配的 **一次 JSON POST + 同响应 SSE server-stream**。打开后只有接收事件与关闭/取消；不能在同一 SSE 响应上继续发送请求消息。
- WebSocket 保持双向 Send/Recv/CloseSend。代码形状可共享生命周期细节，但类型/能力声明必须区分 Server Streaming 与 Bidirectional Streaming。
- 首期 SSE client 不宣称浏览器原生 `EventSource` 兼容、不自动重连、不保存或重放 `Last-Event-ID`；它将 opaque cursor 暴露给消费方。以后若有明确消费者需要原生 EventSource GET，再独立评审。
- HTTP 错误在响应流建立前使用结构化问题错误；建立后发生的错误按协议返回脱敏稳定错误码。连接失败、EOF、取消和应用事件终态保持可区分。

### 3. 配置驱动 Telemetry

- Bootstrap 从其现有 YAML 配置加载 private runtime telemetry schema，并将解析/验证后的配置传给新增的 Telemetry 初始化入口；**业务代码无需调用 `telemetry.Init`、`otel.SetTracerProvider` 或创建 exporter**。
- 配置按 signal 独立控制框架已支持的 traces 与 metrics exporter；每个 signal 可有显式启用开关、OTLP/HTTP endpoint 和必要的 timeout/sampling。service name/environment 继续由 Bootstrap 的服务名与运行环境产生，不允许 YAML 覆写身份来源。
- 默认不启用远端 trace/metric 导出；instrumentation 留在 AppKit 自动执行，未启用 exporter 时使用 noop provider。日志保持 stdout，由已有 `log.level` / `log.format` 配置控制；本 ADR 不新增远程日志 exporter。
- YAML 是显式配置源；现有配置 loader 的应用级环境变量继续覆盖 YAML。已有标准 `OTEL_EXPORTER_OTLP_ENDPOINT` 环境变量作为兼容路径保留，具体覆盖/回退优先级须在实现 issue 中用行为测试冻结。endpoint 可写普通配置；认证 header/token、私钥和证书私密材料只从环境变量或 secret provider 读取，不能落入普通 YAML 或错误日志。
- exporter 构造失败必须 fail-fast 并给出脱敏配置路径/原因；shutdown 在其他资源结束后 flush，并受关停预算约束。

### 4. 自动观测、安全与数据边界

- HTTP client span 覆盖请求至响应/错误；Stream client span 覆盖打开至 EOF/终态/取消，传播 W3C trace context。
- 指标至少覆盖 client 请求/握手时长、结果、活动流、时长、收发帧与字节、取消/错误；具体名称以实现 issue 定稿。标签只允许固定 route/method、transport、outcome、稳定 error code 等低基数值。
- 不记录 request/response payload、URL query、Authorization、Cookie、JWT、用户/租户/调用者标识或任意错误消息；不把原始 URL/path 当指标维度。
- 客户端取消、最大时长、idle、关闭预算、TLS 验证、Origin 与消息大小限制不能因为 instrumentation 包装而失效。

### 5. 公开 API 与配置兼容

- 不向既有导出结构体 `bootstrap.Base`、`bootstrap.RunOptions`、`bootstrap.Options`、`telemetry.Config` 追加字段；配置读入使用私有 runtime config，新行为通过新增类型/入口表达。
- 保留根包及 `tx` 的第三方依赖边界。第三方 OTel/net 类型不得新增至 AppKit 根包 API。
- YAML 字段、默认值、环境覆盖、端点 scheme 和 exporter 失败行为属于受版本约束的配置契约；合并实现前须有文档与验证测试。

## 行为基线与验收

| 形态 | 能力 | client 语义 | 必须覆盖的终态 |
|---|---|---|---|
| Unary HTTP | 一次 request/response | 既有生成客户端，自动安全/观测 | 成功、问题响应、超时、取消 |
| SSE | JSON POST + server events | 打开后只 Recv，不支持 bidi | event、EOF、HTTP 前置错误、流中错误、断连、取消、cursor 透传 |
| WSS | 双向 | Send/Recv/CloseSend，复用现有 secure dialer | 半关闭、EOF、错误、凭证过期、限额、关停 |
| Local Stream | 双向进程内 | 既有 `ClientStream` | 背压、并发限制、取消、Close 和 handler 退出 |

实现 issue 的验收至少包括：YAML enable/disable 与独立端点验证；环境变量覆盖优先级；fake OTLP collector 收到预期 signal；业务模块不调用 OTel 初始化仍能自动观测；安全标签/日志拒绝敏感字段；HTTP/SSE/WSS 客户端的安全、取消和错误测试；相应 adapter 的 conformance；`make check`、`make test-lint`、必要的 `make test-rules` 及相对最新 tag 的零 incompatible apidiff。

## 后续切片

```text
Epic #97 出站客户端与配置驱动观测
└─ Phase 0 #98 本 ADR / 行为基线（本 PR）
   ├─ telemetry YAML schema + Bootstrap 自动装配
   ├─ 共享 outbound HTTP foundation / 既有 Unary client 接缝
   ├─ SSE server-stream client
   ├─ 既有 WSS client 的共享配置/观测接入
   └─ 流形态 conformance、fake collector 与下游固定消费者验证
```

每个实现切片应由独立 issue 冻结 API 名称/签名、错误语义、配置字段和验收命令；本提案未接受之前，不实现破坏性变更或宣称 Streaming 已稳定。

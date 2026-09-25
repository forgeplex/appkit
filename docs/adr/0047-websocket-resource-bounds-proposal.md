# ADR-0047 补充：WebSocket 连接与速率边界

- 状态：Accepted（ADR-0047 补充；2026-09-24 由 PR #89 接受）
- 关联：[#47 AppKit 应用宿主与流式契约](https://github.com/forgeplex/appkit/issues/47)、[#86 WebSocket 连接与速率限额策略](https://github.com/forgeplex/appkit/issues/86)
- 决策基线：AppKit `main@8f5e88bede7415029fbe4a42a71b7592ddd738a0`；最新发布 `v0.9.6`

## 背景与已核实缺口

Epic #47 的 WebSocket 验收要求 frame 大小、连接数、写队列和速率都有边界。当前实现已限制每条流的队列帧数/字节数和单帧 payload 大小，但 Hub 的 pending/active 连接数无总量上限，也没有应用帧速率限制。Accepted ADR-0047 §4–5 定义了有界队列、取消、Origin/身份和 Host drain，却没有指定连接准入、帧速率、限额作用域或默认值。

这是范围缺口，不代表已发现生产代码漏洞。本补充将方案 B 纳入 Accepted ADR-0047；它定义后续实现必须满足的契约，不表示当前传输代码已经执行这些限额。

## 接受的决策（方案 B）

1. **由 AppKit 执行、由应用显式给定有限预算。** Hub 启动或接受 WebSocket 前必须持有有效的有限限额；不得默认为无限，也不在框架中臆造适合所有应用的容量阈值。配置可用新的公开类型/构造入口表达；不得更改已发布签名，也不得靠向已有导出结构体追加字段实现。
2. **连接上限按 Hub 计数。** Pending upgrade reservation 与已升级连接共同占用 `MaxConnections` 槽位；预约必须在 Upgrade 前原子申请，失败时不执行 Upgrade，返回脱敏的 problem 响应和稳定的容量错误。一个 Hub 的集合是上限边界，不暗示跨进程或跨副本全局配额。
3. **速率上限按连接、按方向执行。** 入站和出站分别限制应用消息帧数/秒及 JSON payload 字节数/秒，并各有有限 burst。仅使用连接本地状态，不按未验证的 forwarded IP、Subject 或其他动态身份创建限流表；`MaxMessageBytes` 继续限制单条 payload。
4. **速率耗尽采用有界等待/背压，不丢帧。** 在现有队列上限和 `MaxDuration`/`IdleTimeout`/取消预算内等待 token；等待可取消。超时、取消或连接关闭继续使用既有终态语义。不得静默丢弃已接受事件，也不因正常限速制造应用终态错误帧。
5. **过量新连接 fail-closed。** 在 Upgrade 前拒绝超出 Hub 容量的 reservation；请求不进入身份重建后的应用 handler，不泄露凭证或内部限额细节。
6. **兼容边界。** WebSocket API 尚未包含在 `v0.9.6` 发布基线中，但实现仍需保持 AppKit 已发布 API 零不兼容。对当前 main 已新增的无界 Hub 用法，需在实现 Issue 中明确迁移：建议未显式配置限额的 Hub 不可接受 Upgrade，而不是保留隐式无限生产路径。

本补充不冻结 Go 类型名、构造器签名、正限额的具体最小/最大值、HTTP problem 状态码或 token bucket 内部算法；这些在实现子 Issue [#88](https://github.com/forgeplex/appkit/issues/88) 中按已有 `apperr`、超时和背压契约落定并测试。

## 未采用的备选方案

- **方案 A：内建有限默认值并允许覆盖。** 可让旧构造入口默认有界，但当前没有 Wen/非 Wen 负载证据支持通用默认容量；默认值可能造成容量回归或实际保护不足。
- **方案 C：把连接数和速率都交给入口代理。** 对多副本全局配额有价值，但不能满足 Epic 所写的 AppKit Host/Transport 资源边界；若选择此项，必须修订 #47 验收范围，并明确代理不在 AppKit 运行时证据内。

## 决策与实现状态

- 方案 B 已由用户要求开始实现后选定；ADR 补充由 PR #89 按 required `ci` 与 Wen Review 流程接受。
- 实现边界与本地/CI 验收矩阵见子 Issue #88；源码尚未实现。

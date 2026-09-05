# 服务身份与安全远程契约

服务凭证证明调用方是谁，不证明它可以访问任意租户。AppKit 把签名验证、路由
分类、委托授权、业务权限分开：四者都成立，内部业务请求才能成功。用户 Actor
不会因一次服务认证而被创建，也不会自动随远程调用转发。

## 接收端

标准 bootstrap 在连接数据库、装配模块、监听端口之前检查配置。`user_facing`
仍使用 `Options.AuthnPublicKey/AuthnIssuer`；`internal_service` 要求服务验签器；
`mixed` 两种都要。单进程多用户签发方使用 `MainWithSecurity` / `RunWithSecurity`
的 `SecurityOptions.UserIssuers`，不能同时配置旧的单签发方字段。

以下配置使用占位公钥，必须换成签发方的真实 **Ed25519 公钥**才能启动；接收端
不需要私钥。公钥允许标准 base64 的 32 字节原始公钥，或单个 PKIX `PUBLIC KEY`
PEM。列表中的 issuer/key ID 可以含点，不能把 issuer URL 写成 koanf 的键路径。

```yaml
security:
  mode: internal_service
  service:
    audience: email
    max_ttl: 5m
    clock_skew: 5s
    issuers:
      - issuer: service-ca
        subjects: [notification]
        keys:
          - id: key-2026-09
            public_key: REPLACE_WITH_BASE64_ED25519_PUBLIC_KEY
    delegations:
      - issuer: service-ca
        subject: notification
        tenant_id: merchant-a
        partition: eu
```

`bootstrap.Main` / `Run` 自动读取 `security.service`。需要数据库/外部策略等自定义
授权时，使用 `authn.NewServiceVerifier` 构造验证器，再通过
`SecurityOptions.ServiceVerifier` 注入；不能与文件中的 `security.service` 同时提供。
构造器返回错误；nil/零值验证器不能让内部模式通过启动检查。

配置委托规则是精确的 **issuer + subject + tenant + merchant + partition** 元组，
不是字段分别匹配的多组集合，也没有通配符。没有规则时只允许无委托的服务身份。
空 tenant/partition 不是“全部”：租户业务入口还必须拒绝缺失 scope，并检查请求中的
资源、body/query tenant 与已验证 scope 一致。全局数据、后台任务与账本 ID 不应被
框架擅自改写为租户模型；是否采用 RLS、如何分区由业务域决定。

内部 HTTP 契约用 `reg.MountInternalService` 挂载；面向用户的路由继续使用
`MountAuthenticated` / `MountPermission`。`MountPublic` 只用于真正公开的入口；
不能把根 mux 整体设为 Public 来绕过检查。独立 webhook 等入口仍须自己的签名、
资源归属及重放控制。`-migrate` 不启动 HTTP，因此不构造或要求 HTTP 验签器。

## 服务 JWT 格式

服务令牌放在 `X-Service-Authorization: Bearer <token>`，用户令牌仍在
`Authorization`。固定 `alg=EdDSA`、`typ=appkit-service+jwt`、`purpose=service`。
必须包含可信 `iss`、该签发方白名单中的 `sub`、准确的单一 `aud`、`exp`、`iat`
以及可信公钥的 `kid`。最长生命周期为 5 分钟；可用 MaxTTL 收紧，ClockSkew
最多 30 秒且不扩大 `exp-iat`。签发默认 1 分钟，私钥与配置均在构造时复制。

`tid`、`mid`、`partition` 都是可选的签名委托。自定义
`ServiceDelegationPolicy` 在验签之后执行；签发端也应配置独立的授权策略。
nil policy 拒绝任何非空委托，提供自定义 policy 时还可拒绝空委托。策略必须并发
安全并响应 context 取消，不应从请求携带的元数据本身推断授权。

验签器只按静态可信表选择公钥，不访问 token 的 URL，不接受 `jku/jwk/x5u/crit`
扩展、重复 JSON 字段或重复凭证头。用户验证器拒绝 service、step-up 及其他用途
冒充 access token；访问令牌的 purpose 可省略或为 `access`。这些互斥验证与固定
算法的设计遵循 [JWT 安全最佳实践 RFC 8725](https://www.rfc-editor.org/rfc/rfc8725.html)。

严格 App 边界先清掉所有 unsigned 身份头和预置 Actor/ServicePrincipal。只保留
4 项不可信身份头的隔离快照供验签器检查冲突；它不包含令牌、不进入可信 meta、
也不穿过契约防火墙。有效服务令牌遇到不同或重复的 tenant/partition/caller/merchant
头会被拒绝。Caller 仅来自验签后的 sub。在 mixed 中同时出示两类凭证时，tenant
和 partition 必须完全一致，空与非空也视为不一致。

## 出站客户端

重新运行 `appkit gen contract` 后，生成物同时提供：

- `NewClient(base, caller, hc)`：保留兼容，供显式 legacy/dev 和契约单元测试；
  它不建立服务身份，不能作为生产内部调用的认证方案。
- `NewSecureClient(base, contract.SecureClientOptions)`：返回 `(*Client, error)`；
  必须提供 HTTPS、Audience 与 ServiceCredentialProvider。

将签发器连接到 provider 的核心装配如下（`signer` 由可信组合根构造，私钥不进代码库）：

```go
provider := contract.ServiceCredentialProviderFunc(func(ctx context.Context, scope contract.ServiceScope) (contract.ServiceCredential, error) {
    token, expires, err := signer.SignWithExpiry(ctx, scope.Audience, authn.ServiceDelegation{
        TenantID: scope.TenantID,
        Partition: scope.Partition,
    })
    return contract.ServiceCredential{Token: token, ExpiresAt: expires}, err
})
client, err := emailv1.NewSecureClient("https://email.internal", contract.SecureClientOptions{
    Audience: "email",
    Credentials: provider,
})
```

出站层在每次 HTTP 尝试时获取凭证，检查到期时间，先经过 ctx 防火墙再向 provider
传入显式 scope；不缓存令牌，不转发用户 Authorization、cookie 或上一跳服务凭证。
RequestID 可以明文传播，tenant/partition 必须由 provider 授权后签入新 JWT。
框架提供的签发器不会自动读取 ctx 并签发；provider 同样是受信应用代码，不是沙箱。

安全客户端锁定 origin 和 TLS 主机名，要求 TLS 1.2+，禁止跳过证书验证、任意
RoundTripper、自定义 TLS dialer、cookie jar、密钥日志与重定向。自定义私有 CA 可
通过标准 `http.Transport.TLSClientConfig.RootCAs` 提供；测试使用真实 TLS 服务和
测试根证书，不使用 `InsecureSkipVerify`。返回的客户端/transport 不得再被调用方
改写；框架不试图隔离恶意 Go 代码。

## 密钥轮换与边界

先把新旧两个 kid 的公钥都加入接收方配置，再把调用方签发器切到新 kid，等待旧
令牌最长生命周期和允许时钟偏差过去，最后移除旧公钥。验证器会复制配置；修改
原 map 不是热更新，需构造新实例并通过应用自己的配置发布机制替换。

服务 JWT 是短期 bearer credential，不是一次性交易令牌：持有者在有效期内可以
重放。HTTPS 用于保护传输；资金操作和其他非幂等业务仍需幂等键/事务约束。
没有在线吊销、自动 JWKS 拉取、mTLS/SPIFFE、用户权限委托或生产密钥托管承诺。
框架 HTTP listener 可处于受控 TLS ingress 后；禁止把裸 HTTP 当作无条件安全网络，
不能信任任意 `X-Forwarded-*` 来证明加密链路。本文不执行部署或业务数据库迁移。

## 验证入口

需要合法业务租户/分区的认证绑定，使用 `apptest.ConformWithMeta` 给全部调用
提供同一组有效元数据，provider 与接收端仍独立授权这些值。它还检查 Partition
传播；原 `Conform` 的签名和默认哨兵保持不变，Caller 可按每跳服务身份变化。

```sh
go test -race ./authn ./bootstrap ./contract ./internal/gen
make test-acceptance
make test-rules
```

测试包含真实 App/loopback、TLS、错误 audience/签名/主体、过期/未来 iat、密钥
轮换、令牌用途混淆、委托拒绝、header 冲突、配置快照和出站重定向等负例。

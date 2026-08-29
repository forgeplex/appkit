# appkit 使用手册

> 以"从零构建一个 SSO 系统"为全程实例。设计原理见 [DESIGN.md](DESIGN.md)，
> 本手册只讲怎么做。所有命令都经过实际执行验证。

## 0. 你最终会得到什么

```
github.com/forgeplex/
├── sso-contracts        # 合约仓库：跨域接口 + DTO + 事件 + 错误码（唯一事实源）
├── identity             # 域仓库：用户与组织目录
├── authn                # 域仓库：认证、会话、令牌签发
├── clients              # 域仓库：接入应用（RP/OAuth client）注册
└── sso                  # 组合仓库：wiring + 配置 + 部署，零业务逻辑
```

同一个 sso 二进制：`-target=all` 是单体，`-target=authn` 是微服务——
部署形态是启动参数，不是架构决策。

## 1. 前置条件

```sh
# Go 1.26+；私有仓库拉取：
go env -w GOPRIVATE=github.com/forgeplex/*

# 构建 CLI（appkit 发版打 tag 后可改用 go install github.com/forgeplex/appkit/cmd/appkit@latest）
git clone git@github.com:forgeplex/appkit.git
cd appkit && go build -o ~/bin/appkit ./cmd/appkit
```

本地跑集成测试需要一个 Postgres（任何方式，容器即可）：

```sh
docker run -d --name sso-dev-pg -e POSTGRES_PASSWORD=dev -e POSTGRES_DB=sso \
  -p 127.0.0.1:55432:5432 postgres:18-alpine
```

## 2. 第一步：拆域（最重要的决定，先想清楚再敲命令）

一个域 = 一个仓库 = 一个 Go module = 一个独占的 Postgres schema。拆分依据：

- **有独立的业务不变量和数据属主**才算一个域，不是"按名词拆表"；
- **宁少勿多**：边界拿不准就先合并，appkit 的模块化让日后拆出去是机械工作，
  而拆错了合回来要跨仓库搬代码；
- 域之间只有两种交互：**同步契约调用**（可失败、须幂等）或 **outbox 事件**。
  如果你发现两个"域"之间需要共享事务或频繁互调，它们其实是一个域。

SSO 的合理起点是三个域：

| 域 | 职责 | 独立成域的理由 |
|---|---|---|
| `identity` | 用户/组织目录、凭据存储 | 数据属主清晰；可被未来其他系统复用 |
| `authn` | 登录、会话、OIDC/OAuth2 令牌签发 | 高频热路径，未来最可能独立扩容 |
| `clients` | 接入应用注册、redirect URI、密钥 | 低频管理面，变更节奏与 authn 完全不同 |

## 3. 第二步：生成域仓库

```sh
mkdir sso-system && cd sso-system
appkit new domain identity -dir identity   # -module 可自定义 module path，默认 github.com/forgeplex/<name>
appkit new domain authn    -dir authn
appkit new domain clients  -dir clients
```

每个仓库生成 17 个文件，**生成即合规**（骨架自身通过 `appkit check` 与
`appkit sync --check`）。以 identity 为例，你需要知道的文件：

| 文件 | 是什么 | 你要做什么 |
|---|---|---|
| `internal/identity/` | ★ 业务包：领域类型、不变量、用例编排、所需接口 | **主战场**。零 infra import（lint 机检） |
| `internal/postgres/` | Store 实现，全仓唯一可 import pgx/sqlc 的包 | 实现业务包声明的 Store 接口 |
| `internal/http/` | transport：DTO ↔ 业务类型映射 | 实现 handler，禁业务规则/SQL（机检） |
| `internal/inbox/` | 外域事件消费者 | 按 topic 写 handler |
| `internal/module/module.go` | 唯一 wiring 落点：Provide/Mount/Migrations/Health | 每加一个用例/路由在这里挂 |
| `db/migrations/0001_appkit_base.sql` | outbox/inbox/幂等/审计四张基础表（生成期由框架库函数拼装） | 别动；自己的表从 `0002_` 开始 |
| `db/queries/` + `sqlc.yaml` | SQL 唯一存在地；sqlc 生成到 `internal/postgres/sqlc` | 写 `.sql`，`make gen` |
| `identity.go` | 唯一导出面：`Module()`（只有组合仓库和本仓 cmd/ 会 import） | 一般不动 |
| `cmd/identityd/` | 独立微服务部署入口 | 一般不动 |
| `.appkit.yml` | 框架配置：`check`/`sync`/`dev` 读取 | 接入合约仓库后填 `contracts:` |

生成后立刻执行（每个域仓库都要）：

```sh
cd identity
appkit sync      # 物化 .golangci.yml / .go-arch-lint.yml / CI 引用
appkit dev       # appkit 未发版期间：生成 go.work 引用本地 appkit
make check && make test
```

### 三条方向性约束（写代码前记住，工具会替你把关）

1. `internal/identity` **零 infra import**——禁 pgx/gin/net/http；
2. **pgx/sqlc 只准出现在 `internal/postgres`**（cmd/ 与 internal/module 的装配代码除外）；
3. `internal/http`、`internal/inbox` **禁止 import `internal/postgres`**——必须经业务包接口。

违规不是靠自觉发现的：`appkit check` 和物化的 lint 在本地与 CI 都会拦下来。

## 4. 第三步：写第一个用例（identity 的"创建用户"）

**① 迁移与查询**（只允许操作 `identity` schema，跨 schema 引用会被 `appkit check` 拒绝）：

```sql
-- db/migrations/0002_users.sql
CREATE TABLE identity.users (
    id            uuid PRIMARY KEY,
    email         text NOT NULL UNIQUE,
    password_hash text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);
```

```sql
-- db/queries/users.sql
-- name: InsertUser :exec
INSERT INTO identity.users (id, email, password_hash) VALUES ($1, $2, $3);
```

`make gen` 生成类型安全的查询代码。

**② 业务包**（`internal/identity/`）——用例的标准形态：

```go
// service.go：事务边界 + 不变量 + 事件，一个都不缺
func (s *Service) CreateUser(ctx context.Context, in CreateUser) (User, error) {
    if err := in.validate(); err != nil {          // 不变量校验，纯函数
        return User{}, err
    }
    u := newUser(in)
    err := s.txr.Do(ctx, func(ctx context.Context) error {
        if err := s.store.InsertUser(ctx, u); err != nil {   // Store 接口，实现在 postgres 包
            return err
        }
        evt, err := ssoevents.UserCreated{UserID: u.ID, Email: u.Email}.Event()
        if err != nil {
            return err
        }
        return s.pub.Publish(ctx, evt)             // outbox：与业务写同事务落表
    })
    return u, err
}
```

事务提交后，框架的 outbox relay 自动把事件投递给订阅方——你不需要也**不能**
"手动发消息"（事务外发布会被运行时守卫拒绝，忘发事件则根本写不出来）。

**③ Store 实现**（`internal/postgres/`）：从 ctx 取事务（`pgtx.From`），调 sqlc 生成代码。

**④ handler**（`internal/http/`）：解码 → 调 `Service` → `httpserver.WriteError` 统一出错出口
（RFC 9457 problem+json，错误码即错误身份）。

**⑤ 挂进 module**（`internal/module/module.go`）：`reg.Mount("POST /users", ...)`。

金额/审计/幂等这三个 PSP 级原语在 SSO 里同样可用：登录接口套
`idem.Middleware`（客户端带 `Idempotency-Key` 头），敏感变更用 `audit.Recorder`
（与业务写同事务）。

## 5. 第四步：契约仓库（域间协作的唯一通道）

域仓库**永不互相 require**（`appkit check` 第一条就查这个）。authn 要校验
identity 的凭据？两边共同依赖合约仓库：

```sh
mkdir sso-contracts && cd sso-contracts && go mod init github.com/forgeplex/sso-contracts/go
mkdir identityv1
```

**错误码与事件用 yaml 声明、生成代码**（不手写，杜绝双源漂移）：

```yaml
# identityv1/codes.yaml
version: 1
package: identityv1
codes:
  - { code: IDENTITY_USER_NOT_FOUND,     status: 404, message: user not found }
  - { code: IDENTITY_INVALID_CREDENTIAL, status: 401, message: invalid credential }
```

```yaml
# identityv1/events.yaml
version: 1
package: identityv1
events:
  - name: UserCreated
    topic: identity.user.created.v1
    fields:
      - { name: user_id, type: string, required: true }
      - { name: email,   type: string, required: true }
```

```sh
appkit gen errors -in identityv1/codes.yaml  -out identityv1/codes.gen.go
appkit gen events -in identityv1/events.yaml -out identityv1/events.gen.go
```

**契约接口手写一次，wrapper 生成**。接口必须是"ctx + 至多一个请求 DTO →
至多一个响应 + error"的粗粒度形态（`gen wrap` 会拒绝其他签名——这本身是
框架约束：契约按网络边界设计，DESIGN §5.3）：

```go
// identityv1/service.go
type Service interface {
    VerifyCredential(ctx context.Context, req VerifyCredentialRequest) (VerifyCredentialReply, error)
}
```

```sh
appkit gen wrap -src identityv1 -iface Service -system identity -out identityv1/service_wrap.gen.go
```

然后：

- **提供方 identity**：`.appkit.yml` 填 `contracts: github.com/forgeplex/sso-contracts/go`，
  module 里用 `appkit.ProvideContract` 注册——实现被强制包上 `identityv1.WrapService`
  （每次跨模块调用都经 `contract.Call`：事务守卫、ctx 防火墙、超时、错误规范化）：

  ```go
  appkit.ProvideContract(reg,
      func(*appkit.Registry) (identityv1.Service, error) { return newContractImpl(svc), nil },
      func(s identityv1.Service) identityv1.Service { return identityv1.WrapService(s, 0) })
  ```

- **消费方 authn**：`Setup` 里 `appkit.MustResolve[identityv1.Service](reg)`，
  拿到的永远是已包裹的实现。**事务内发起契约调用会直接报错**
  （`TX_BOUNDARY`）——跨域一致性走事件，不走共享事务。
- **消费事件**：authn 的 module 里
  `reg.Consumer(identityv1.TopicUserCreated, outbox.Inbox(pool, "authn", "authn", handler))`
  ——inbox 按 (consumer, event_id) 去重，handler 天然幂等。

## 6. 第五步：组合仓库

```sh
appkit new system sso -dir sso
cd sso && appkit dev
```

按 `cmd/sso/main.go` 里的注释样例完成装配（这是全系统唯一的组合根）：

```go
pool, err := pgtx.NewPool(ctx, cfg.Database.URL)     // 各域共用连接池（按库共享）
// ...
bus := outbox.NewDirectBus()                          // 单体单库：进程内直投，无 broker
app := appkit.New(
    []appkit.Module{
        identity.Module(identity.Options{Log: log, Pool: pool}),
        authn.Module(authn.Options{Log: log, Pool: pool}),
        clients.Module(clients.Options{Log: log, Pool: pool}),
    },
    appkit.Target(target),
    appkit.Bus(bus),
    appkit.Migrator(pgmigrate.Runner(pool)),          // 启动时按 schema 应用各域迁移
    appkit.Middleware(httpserver.Base(log)...),
    appkit.HTTPAddr(cfg.Addr),
    appkit.Logger(log),
    // target 之外的域自动落到远程绑定（HTTP client 实现同一契约接口）：
    appkit.Remote[identityv1.Service](func(*appkit.Registry) (identityv1.Service, error) {
        return identityv1.NewClient(cfg.Endpoints.Identity), nil
    }),
)
return app.Run(ctx)
```

`go.mod` require 三个域 + 合约仓库；本地联调 `appkit dev -root ..` 一条命令搞定
（go.work 已在 .gitignore，不提交）。

**部署矩阵（同一镜像）：**

| 场景 | 命令 |
|---|---|
| 单体（起步推荐） | `./sso -target=all` |
| authn 独立扩容 | `./sso -target=authn` + 其余 `-target=identity,clients` |
| 专职 relay 角色 | `./sso -target=relay`（模块把 relay 挂在自己的 OnStart，target 里含哪个域就跑哪个域的 relay） |

拆分部署后把 `outbox.DirectBus` 换成 NATS/Kafka 实现（同一 `Bus` 接口），
业务代码零改动。

## 7. 第六步：跑起来

```sh
cd sso
make run                                   # -target=all，读 config/dev.yaml
curl localhost:8080/healthz                # 存活探针
curl localhost:8080/readyz                 # 就绪探针（含各域 postgres 检查）
curl -X POST localhost:8080/users -d '{"email":"a@b.c","password":"..."}' \
     -H 'Idempotency-Key: 5f0c...'         # 重发同 key 同 body → 回放缓存响应
```

测试：单测不需要任何环境；数据层集成测试用环境变量开启：

```sh
TEST_DATABASE_URL='postgres://postgres:dev@127.0.0.1:55432/sso?sslmode=disable' make test
```

## 8. 第七步：CI 与约束收口

每个域仓库的 `.github/workflows/ci.yml`（`appkit sync` 生成）只有一行引用：

```yaml
jobs:
  ci:
    uses: forgeplex/appkit/.github/workflows/domain-ci.yml@main
```

这条流水线做：gofmt → vet → build → `test -race`（带 Postgres service）→
`appkit check` → `appkit sync --check`（lint 配置漂移即失败）→ `go mod tidy` 漂移检查。
配合 branch protection，**约束在 CI 不可绕过**；`//nolint` 必须写理由（nolintlint）。

自研 analyzer（金额禁浮点、ctx 禁存 struct）：

```sh
go install github.com/forgeplex/appkit/lint/cmd/appkit-lint@latest
go vet -vettool=$(which appkit-lint) -moneyfloat.scope 'internal/(identity|authn)' ./...
```

## 9. 规则速查

| 你想做 | 正确做法 | 错误做法（会被什么拦住） |
|---|---|---|
| 域 A 调域 B | 合约接口 + `Resolve`，事务外调用 | 直接 require B 仓库（check/编译失败）；事务内调用（运行时守卫报 TX_BOUNDARY） |
| 跨域取数据做报表 | 订阅事件构建本域读模型 | 跨 schema JOIN（check 拒绝 SQL） |
| 发领域事件 | 用例事务内 `pub.Publish` | 事务外发（守卫拒绝）；直连 broker（业务包 import 不到） |
| 处理外域事件 | `reg.Consumer` + `outbox.Inbox` 包裹 | 裸 handler（重复投递=重复执行） |
| 写 SQL | `db/queries/*.sql` + sqlc | handler/service 里拼 SQL（depguard 拦 pgx import） |
| 表示金额 | `money.Money` | float64（appkit-lint 报错） |
| 返回错误 | 合约错误码（`apperr.Is(err, identityv1.CodeXxx)` 单体/微服务行为一致） | 字符串比对、裸 errors.New 跨层 |

## FAQ

**Q：一开始就要拆三个仓库吗？**
不必。可以先只建 `identity` 一个域 + `sso` 组合仓库跑通全链路，authn/clients
的雏形先作为 identity 里的普通代码，边界清晰后再 `new domain` 迁出——
迁移成本主要是搬文件，因为方向性约束保证了代码本来就没有纠缠。

**Q：`appkit dev` 和发版是什么关系？**
本地联调用 go.work（不提交）；跨仓库正式依赖靠 tag：改了合约仓库 → 打 tag →
域仓库升 require → 域仓库打 tag → 组合仓库升 require。appkit 未发 v0.1.0 前，
所有仓库都靠 `appkit dev` 联调。

**Q：模块还需要哪些生命周期钩子？**
一般不需要。连接池、迁移、HTTP、relay、优雅关停都由框架或骨架接好；
只有自定义后台任务才用 `reg.OnStart(appkit.StageWorker, ...)` + `reg.OnStop`
（stop 里等 worker 退出务必 `select` 上 ctx，见 outbox/relay.go 顶部示例）。

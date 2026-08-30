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

## 1. 前置条件（私有仓库必读，跳过必翻车）

forgeplex 全部仓库是私有的。Go 工具链拉取私有模块需要**两件事都配好**，
缺一件就会得到两种典型报错：

```sh
# ① 让 go 跳过公共代理与校验库（否则 sum.golang.org 对私有模块必然 404）
go env -w GOPRIVATE='github.com/forgeplex/*'

# ② 让 git 对 forgeplex 走 SSH 而不是匿名 https
#   （否则 "fatal: could not read Username for 'https://github.com'"）
git config --global url."git@github.com:forgeplex/".insteadOf "https://github.com/forgeplex/"
```

CI 环境（无 SSH key）用令牌变体，二选一替代 ②：

```sh
# GitHub Actions 等：写 ~/.netrc
printf 'machine github.com\nlogin x-access-token\npassword %s\n' "$GITHUB_TOKEN" >> ~/.netrc
```

配完跑一次体检（每台新机器、每条新 CI 流水线都值得跑）：

```sh
appkit doctor
```

它会检查 Go 版本、GOPRIVATE 覆盖面、git 凭据、docker，以及——在仓库目录里——
是否处于 go.work 工作区（go.mod 尚未 require appkit 发版版本、或要联调本地
未发布改动时，编译**必须**在工作区内，否则 go 会去远程解析 appkit 版本）。
任何 ✗ 都附带可直接复制的修复命令。

**故障对照表：**

| 你看到的报错 | 缺了什么 |
|---|---|
| `reading https://sum.golang.org/lookup/...: 404 Not Found` | ① GOPRIVATE |
| `fatal: could not read Username for 'https://github.com'` | ② git SSH 重写 / 令牌 |
| `go: finding module for package github.com/forgeplex/appkit/...`（本地开发时） | 不在 go.work 工作区：仓库目录跑 `appkit dev` |

安装 CLI（appkit 自 v0.1.0 起按 SemVer 发版）：

```sh
go install github.com/forgeplex/appkit/cmd/appkit@latest
```

要用未发布的最新改动时才需要源码构建：`git clone` 后
`go build -o ~/bin/appkit ./cmd/appkit`。

本地数据库不用提前准备：域仓库骨架自带 `make dev-db`（一次性 docker Postgres，
端口 54329）与 `make run-db`。只有跑组合仓库或自建库时才需要自己起一个
Postgres——注意选端口前先确认没被占用（`lsof -iTCP:<端口> -sTCP:LISTEN`）。

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

每个仓库生成 21 个文件（含 `new` 时自动物化的 lint / CI 配置——不需要再跑
`appkit sync`，它只在升级 appkit 后用来刷新），**生成即合规**（骨架自身通过
`appkit check` 与 `appkit sync --check`）。以 identity 为例，你需要知道的文件：

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
| `cmd/identityd/` | 独立微服务部署入口（`database.url` 留空 = 最小模式） | 一般不动 |
| `config/dev.yaml` | 运行配置；任意键可被 `IDENTITYD_*` 环境变量覆盖 | 完整模式时填 `database.url` |
| `.appkit.yml` | 框架配置：`check`/`sync`/`dev` 读取 | 接入合约仓库后填 `contracts:` |

生成后立刻执行（每个域仓库都要）：

```sh
cd identity
appkit dev       # 生成 go.work。★ appkit 未发版期间，appkit 的 checkout 必须
                 # 位于同一根目录下才会被纳入（不在则按 dev 的提示手动
                 # go work use <appkit 路径>），否则依赖解析会走远程
make run         # 零依赖试跑：最小模式（仅 /healthz /readyz /identity/ping）
make run-db      # 完整模式：自动起一次性开发 Postgres（docker）并注入 database.url
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

`make gen` 生成类型安全的查询代码（sqlc 版本已钉死、经 `go run` 执行，无需预装；
基础迁移里已含 `CREATE SCHEMA`，sqlc 的静态分析开箱即通）。

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
        // 事件类型来自 §5 的合约仓库生成物（identityv1）；可先完成 §5 再回来接这两行。
        evt, err := identityv1.UserCreated{UserID: u.ID, Email: u.Email}.Event()
        if err != nil {
            return err
        }
        return s.pub.Publish(ctx, evt)             // outbox：与业务写同事务落表
    })
    return u, err
}
```

事务提交后，模块自带的 outbox relay（骨架已装配，`Options.Bus` 注入时启动）
自动把事件投递给订阅方——你不需要也**不能**"手动发消息"（事务外发布会被
运行时守卫拒绝，忘发事件则根本写不出来）。

**③ Store 实现**（`internal/postgres/`）：从 ctx 取事务（`pgtx.From`），调 sqlc 生成代码。

**④ handler**（`internal/http/`）：解码 → 调 `Service` → `httpserver.WriteError` 统一出错出口
（RFC 9457 problem+json，错误码即错误身份）。

**⑤ 挂进 module**（`internal/module/module.go`）：`reg.Mount("POST /identity/users", ...)`。
需要幂等的写接口（支付、建用户）用中间件包住 handler，客户端带 `Idempotency-Key` 头：

```go
idemMW := idem.Middleware(idem.NewStore(m.opts.Pool, Schema), m.opts.Log)
reg.Mount("POST /identity/users", idemMW(usersHandler))
```

敏感变更再加 `audit.Recorder`（与业务写同事务）；金额一律 `money.Money`。

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

// DTO 一律传值、可序列化（按网络边界设计）。
type VerifyCredentialRequest struct {
    Email    string `json:"email"`
    Password string `json:"password"`
}
type VerifyCredentialReply struct {
    UserID string `json:"user_id"`
    OK     bool   `json:"ok"`
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
pool, err := pgtx.NewPool(ctx, cfg.Database.URL)     // 各域共用连接池（按库共享；
// ...                                               //  骨架 config 已含 database 段）
bus := outbox.NewDirectBus()                          // 单体单库：进程内直投，无 broker
app := appkit.New(
    []appkit.Module{
        // 同一个 bus 传给每个模块：模块用它跑自己的 outbox relay。
        identity.Module(identity.Options{Log: log, Pool: pool, Bus: bus}),
        authn.Module(authn.Options{Log: log, Pool: pool, Bus: bus}),
        clients.Module(clients.Options{Log: log, Pool: pool, Bus: bus}),
    },
    appkit.Target(target),
    appkit.Bus(bus),                                  // 消费者订阅端
    appkit.Migrator(pgmigrate.Runner(pool)),          // 启动时按 schema 应用各域迁移
    appkit.Middleware(httpserver.Base(log)...),
    appkit.HTTPAddr(cfg.Addr),
    appkit.Logger(log),
    // target 之外的域落到远程绑定：注入实现同一契约接口的 HTTP client。
    // ★ client 目前需手写（对着契约接口实现，错误经 apperr.FromProblem 重建，
    //   错误码身份跨网络不变）；`appkit gen client` 在路线图上。
    // appkit.Remote[identityv1.Service](func(*appkit.Registry) (identityv1.Service, error) {
    //     return identityclient.New(cfg.Endpoints.Identity), nil
    // }),
)
return app.Run(ctx)
```

`go.mod` require 三个域 + 合约仓库——**这是打 tag 发版之后的形态**；未发版期间
不要写这些 require（对不存在版本的 require 会去代理拉取而失败），依赖全部由
`appkit dev -root ..` 生成的 go.work 提供（go.work 在 .gitignore，不提交；
发版节奏见 FAQ）。

**部署矩阵（同一镜像）：**

| 场景 | 命令 |
|---|---|
| 单体（起步推荐） | `./sso -target=all` |
| authn 独立扩容 | `./sso -target=authn` + 其余 `-target=identity,clients` |
| 专职 relay 角色 | `./sso -target=relay`（模块把 relay 挂在自己的 OnStart，target 里含哪个域就跑哪个域的 relay） |

拆分部署后把 `outbox.DirectBus` 换成 NATS/Kafka 实现（同一 `Bus` 接口），
业务代码零改动。

## 7. 第六步：跑起来

单个域仓库（identity 目录内）：

```sh
make run        # 最小模式：零依赖，database.url 留空
curl localhost:8080/identity/ping          # → pong (最小模式)

make run-db     # 完整模式：自动起一次性 Postgres + 迁移 + relay
curl localhost:8080/readyz                 # → {"status":"ready"}
curl -i localhost:8080/identity/ping       # → 204（占位用例走了真实事务边界）
```

组合仓库（sso 目录内，`config/dev.yaml` 填好 database.url 后）：

```sh
make run                                   # -target=all 单体
curl localhost:8080/healthz                # 存活探针
curl localhost:8080/readyz                 # 就绪探针（含各域 postgres 检查）
curl -X POST localhost:8080/identity/users \
     -d '{"email":"a@b.c","password":"..."}' \
     -H 'Idempotency-Key: 5f0c...'         # 重发同 key 同 body → 回放缓存响应
                                           #（响应头 Idempotency-Replayed: true）
```

端口被占时不用改文件：`SSO_ADDR=:18080 make run`（域仓库同理，
`IDENTITYD_ADDR=...`）——任意配置键都能这样覆盖。

测试：单测不需要任何环境。`TEST_DATABASE_URL` 是**给你将来写的数据层集成
测试**的全系统约定（appkit 自身与 CI 流水线都用它；骨架初始没有测试文件）：

```sh
TEST_DATABASE_URL='postgres://postgres:dev@127.0.0.1:54329/identity_dev?sslmode=disable' make test
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
域仓库升 require → 域仓库打 tag → 组合仓库升 require。appkit 自 v0.1.0 起
按 SemVer 发版（CI 有 apidiff 门禁保证向后兼容），域仓库直接 require 版本即可；
`appkit dev` 只在联调本地未发布改动时需要。

**Q：模块还需要哪些生命周期钩子？**
一般不需要。连接池、迁移、HTTP、relay、优雅关停都由框架或骨架接好；
只有自定义后台任务才用 `reg.OnStart(appkit.StageWorker, ...)` + `reg.OnStop`
（stop 里等 worker 退出务必 `select` 上 ctx，见 outbox/relay.go 顶部示例）。

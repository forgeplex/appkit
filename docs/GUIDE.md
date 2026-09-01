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

每个仓库生成 24 个文件（含 `new` 时自动物化的 lint / CI 配置——不需要再跑
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
| `db/SCHEMA.md` + `db/schema/` | 表结构总览 + ER 图 + 每表详情，从迁移派生（要连 DB，所以 `new` 时不生成） | 改过迁移就 `make schema`；**禁止手改**（见 4.1） |
| `identity.go` | 唯一导出面：`Module()`（只有组合仓库和本仓 cmd/ 会 import） | 一般不动 |
| `cmd/identityd/` | 独立微服务部署入口：只声明"装哪些模块"，装配本身在 `appkit/bootstrap` | 一般不动 |
| `config/dev.yaml` | 运行配置；任意键可被 `IDENTITYD_*` 环境变量覆盖 | 完整模式时填 `database.url` |
| `.appkit.yml` | 框架配置：`check`/`sync`/`dev` 读取 | 接入合约仓库后填 `contracts:` |
| `.gitattributes` | 钉 `*.sql eol=lf`（迁移校验和跨平台一致）+ 折叠生成物 | 别动（`appkit sync` 维护） |

**三类代码是分开的，这是刻意的**：框架在 module cache 里（0444 只读，改不到）；
框架生成的公共代码（lint/CI 配置、sqlc 产物、基础迁移）带生成头且由 `appkit check`
守着漂移；你写的代码只在 `internal/` 那几个包里。启动装配这种"每个仓库一字不差、
改坏了框架无从知情"的部分已经收进 `bootstrap`——`cmd/identityd/main.go` 只剩一份
模块清单，二十来行。

生成后立刻执行（每个域仓库都要）：

```sh
cd identity
appkit dev       # 生成 go.work。★ appkit 未发版期间，appkit 的 checkout 必须
                 # 位于同一根目录下才会被纳入（不在则按 dev 的提示手动
                 # go work use <appkit 路径>），否则依赖解析会走远程
make run         # 零依赖试跑：最小模式（仅 /healthz /readyz /identity/ping）
make run-db      # 完整模式：自动起一次性开发 Postgres（docker）并注入 database.url
make check && make lint && make test   # = CI 跑的那几条
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
COMMENT ON TABLE identity.users IS '登录主体：email 唯一，密码只存 argon2id 摘要。';
```

`COMMENT ON TABLE` 不是可选的客套话：表的用途属于 schema 设计，写在迁移里才会
跟着表一起演进；写在别处的说明，改表的人不会同步。缺了它 `db/SCHEMA.md` 会标
⚠（见下），不阻断 CI，但 review 时看得见。

```sql
-- db/queries/users.sql
-- name: InsertUser :exec
INSERT INTO identity.users (id, email, password_hash) VALUES ($1, $2, $3);
```

`make gen` 生成类型安全的查询代码（sqlc 版本已钉死、经 `go run` 执行，无需预装；
基础迁移里已含 `CREATE SCHEMA`，sqlc 的静态分析开箱即通）。

改完迁移再跑 `make schema`（见 4.1），把 schema 文档一并更新后提交。

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
运行时守卫拒绝，忘发事件则根本写不出来）。消费方持续失败到重试上限的事件进
死信（不再占用投递热路径），修好消费方后一条命令放回重投：

```sh
appkit outbox -schema <域> -dsn <...>          # 列出死信：ID、失败原因、失败时间
appkit outbox -schema <域> -dsn <...> <ID>…    # 按事件 ID 放回（-all 全放）
```

**③ Store 实现**（`internal/postgres/`）：从 ctx 取事务（`pgtx.From`），调 sqlc 生成代码。

**④ handler**（`internal/http/`）：解码 → 调 `Service` → `httpserver.WriteError` 统一出错出口
（RFC 9457 problem+json，错误码即错误身份）。

**⑤ 挂进 module**（`internal/module/module.go`）：`reg.Mount("POST /identity/users", ...)`。
需要幂等的写接口（支付、建用户）用中间件包住 handler，客户端带 `Idempotency-Key` 头：

```go
idemMW := idem.Middleware(idem.NewStore(m.opts.Pool, Schema), m.opts.Log)
reg.Mount("POST /identity/users", idemMW(usersHandler))
```

默认指纹绑定原始字节：客户端重试时若重新序列化（金额 "80" 变 "80.00"、字段
顺序或空白变化），同键会被判为异 payload 而 422。入站 DTO 已走
`money.ParseCanonical` 的域，把同款口径接进中间件层，等值形态的重试就能拿到
回放；多租户/多账本再用作用域键隔离键空间（别手拼 `{tenant}:` 前缀——前缀
是可拼造出碰撞的）：

```go
idemMW := idem.Middleware(idem.NewStore(m.opts.Pool, Schema), m.opts.Log,
	idem.WithCanonicalizer(func(r *http.Request, body []byte) ([]byte, error) {
		var req struct {
			Amount string `json:"amount"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, apperr.New(apperr.CodeInvalidArgument, http.StatusBadRequest,
				"body 不是 JSON").WithCause(err)
		}
		m, err := money.ParseCanonical(req.Amount, "USD")
		if err != nil {
			return nil, err // 非规范金额在 claim 之前拒绝，不留悬挂的 in_progress
		}
		return []byte(m.Amount().String()), nil // "80"/"80.00" 都是 "80"
	}),
	idem.WithKeyScope(func(r *http.Request, _ string) (string, error) {
		return tenantFromRequest(r), nil // 从鉴权材料取，别信请求头自报的租户
	}),
)
```

哈希纪律（method/path 绑定、分隔符防歧义拼接）留在框架里，实现方只须保证
「等值请求 → 相同字节」。换规范化口径是单向门：存量记录的 payload_hash 全部
失配、completed 又不过期，旧键会持续 422 到客户端换键为止——接入时选一次，
别来回换。作用域模式下键与作用域含控制字节会被 400 拒绝（RFC 7230 头值
本就不允许），这是分隔符不可伪造的前提。

敏感变更再加 `audit.Recorder`（与业务写同事务）；金额存储/运算用
`decimal.Decimal`（sqlc 脚手架已全局 override NUMERIC），需要"币种+金额"
绑定时用领域层的 `money.Money`（不落库，币种另列存），JSON 边界一律字符串。

### 4.1 看清整个域的 schema（`make schema`）

迁移是不可变的追加日志，所以一张表的真实形状会散落在 `0003` 建表、`0007` 加列、
`0012` 改默认值里。想知道"这个域现在长什么样"，不该靠在脑子里重放整条历史。

```sh
make schema      # = appkit schema -dsn "$DEV_DB_URL"（需 docker，会先起开发库）
```

它把 `db/migrations` 应用到一个**一次性临时库**（用的就是服务启动时的那个迁移
runner），读回 catalog，然后删库。开发库本身不受影响，产出是 `db/migrations` 的
纯函数——和你本地有没有手工 `ALTER` 过无关。产出这些：

```
db/SCHEMA.md              总览：表清单 + 按外键分簇的 ER 图     ← 人的入口
db/schema/users.sql       DDL 形态                              ← 写迁移前先读它
db/schema/users.md        表格 + 反向引用 + 一跳邻域图          ← review 时读
db/schema/_appkit/        outbox/inbox/幂等/审计等框架表，隔离摆放
```

等 identity 长出 credentials 与 login_attempts，`db/SCHEMA.md` 的正文是这样：

````markdown
## 表清单

| 表 | 列 | 说明 |
| --- | --- | --- |
| [`credentials`](schema/credentials.md) | 5 | 一个用户可以挂多种凭据；secret 按 kind 解释。 |
| [`login_attempts`](schema/login_attempts.md) | 4 | ⚠ 缺说明 |
| [`users`](schema/users.md) | 4 | 登录主体：email 唯一，密码只存 argon2id 摘要。 |

## 关系图

### 簇 1 · 3 张表

```mermaid
erDiagram
    users ||--o{ credentials : "user_id"
    users ||--o{ login_attempts : "user_id"
```

## 枚举类型

| 类型 | 取值 |
| --- | --- |
| `credential_kind` | `password` · `totp` · `webauthn` |
````

三件事值得注意：

- **⚠ 缺说明** 是 `login_attempts` 的建表迁移里漏了 `COMMENT ON TABLE`。这是软约束：
  只标注、不阻断 CI（存量仓库不会因此突然变红），但 review 时看得见。
- **上百张表怎么看**：全局图按外键连通分量分簇，一簇一张；某一簇仍然挤到看不动时，
  点表名进它自己的页面看**一跳邻域图**（该表 + 直接外键邻居）。
- 图是 **Mermaid**：GitHub 原生渲染，且在 git diff 里读得懂——PNG 做不到这一点。

每张表的 `.sql` 是完整 DDL，给写下一个迁移的人（和 agent）读：

```sql
-- db/schema/credentials.sql
-- Code generated by appkit schema. DO NOT EDIT.
-- 一个用户可以挂多种凭据；secret 按 kind 解释。
CREATE TABLE identity.credentials (
    id         bigint                   GENERATED ALWAYS AS IDENTITY NOT NULL,
    user_id    uuid                     NOT NULL,
    kind       identity.credential_kind NOT NULL,
    secret     bytea                    NOT NULL,
    created_at timestamp with time zone NOT NULL DEFAULT now(),
    CONSTRAINT credentials_pkey PRIMARY KEY (id),
    CONSTRAINT credentials_user_id_fkey FOREIGN KEY (user_id) REFERENCES identity.users(id)
);

CREATE INDEX credentials_user_idx ON identity.credentials USING btree (user_id);

COMMENT ON TABLE identity.credentials IS '一个用户可以挂多种凭据；secret 按 kind 解释。';
COMMENT ON COLUMN identity.credentials.secret IS '密文，明文永不落库。';
```

**产出禁手改**：`appkit schema -check` 在 CI 里比对，缺文件、被手改、以及删表之后
残留的陈旧文件都算漂移。渲染不了的特性（分区表、生成列、RLS、表继承……）不会被
静默略过——命令会点名那张表并失败，因为一份看起来像 DDL 却漏了约束的文件比没有更危险。

还没启用的仓库（`db/SCHEMA.md` 与 `db/schema/` 都不存在）CI 只打一条提示后放行；
跑过一次 `make schema` 并提交产出，这个仓库就永久转严。

## 5. 第四步：契约仓库（域间协作的唯一通道）

域仓库**永不互相 require**（`appkit check` 第一条就查这个）。authn 要校验
identity 的凭据？两边共同依赖合约仓库：

```sh
mkdir sso-contracts && cd sso-contracts && go mod init github.com/forgeplex/sso-contracts/go
mkdir identityv1
```

**契约、错误码与事件都用 yaml 声明、生成代码**（不手写，杜绝双源漂移）：

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

**契约接口同样由 yaml 声明、全量生成**。事实源是 `contract.yaml`（与 events.yaml
同风格），`appkit gen contract` 一次产出五份文件——`service.gen.go`（Service
接口 + 传值 DTO）、`wrap.gen.go`（进程内 wrapper）、`client.gen.go`（HTTP
client，实现同一接口）、`server.gen.go`（`NewHTTPHandler`）、`openapi.yaml`
（派生导出，供文档与 oasdiff 门禁消费）：

```yaml
# identityv1/contract.yaml
version: 1
package: identityv1
system: identity
methods:
  - name: VerifyCredential
    path: /verify-credential
    doc: 校验用户凭据；凭据错误返回 IDENTITY_INVALID_CREDENTIAL。
    idempotent: true
    request:
      - { name: email,    type: string, required: true }
      - { name: password, type: string, required: true }
    response:
      - { name: user_id, type: string }
      - { name: ok,      type: bool }
```

```sh
appkit gen contract -in identityv1/contract.yaml -dir identityv1
```

方法形态被钉死在"ctx + 至多一个请求 DTO → 至多一个响应 + error"的粗粒度上
（空 request 即无参，空 response 即仅 error）——契约按网络边界设计
（DESIGN §5.3）。`idempotent: true` 的方法，生成 client 会对可用性故障做
有界重试；`doc` 必填——契约是给别的团队读的。

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

### 给契约加一条一致性测试（`apptest`）

上面那句"进程内和远程语义一致"是框架的承诺，但**承诺归承诺，装配归装配**。
client 指向了错的环境、实现没走 `ProvideContract` 那条链路、领域错误在
problem+json 往返后换了码——这些都只在真正拆分部署的那天才炸，而那天你多半正在
救别的火。`apptest.Conform` 让同一批用例分别跑过两个绑定，当场比对。

测试写在**提供方的域仓库**里（这里是 identity）：只有它同时拿得到真实实现和真实
的 HTTP handler，把 client 指向 `httptest.Server` 就得到了一条真跑网络的绑定。

```go
func TestIdentityConformance(t *testing.T) {
    impl := identityv1.WrapService(newContractImpl(svc), 0)
    srv := httptest.NewServer(identityv1.NewHTTPHandler(impl))
    t.Cleanup(srv.Close)

    apptest.Conform(t,
        []apptest.Binding[identityv1.Service]{
            {Name: "local", Service: impl},
            {Name: "remote", Service: identityv1.NewClient(srv.URL, "authn", nil)},
        },
        []apptest.Case[identityv1.Service]{
            {Name: "凭据正确", Do: verify("a@b.c", "right"),
                Want: identityv1.VerifyCredentialReply{UserID: "u-1", OK: true},
                Idempotent: true},
            {Name: "凭据错误", Do: verify("a@b.c", "wrong"),
                WantCode: identityv1.CodeBadCredential},
        })
}

func verify(email, pw string) func(context.Context, identityv1.Service) (any, error) {
    return func(ctx context.Context, s identityv1.Service) (any, error) {
        return s.VerifyCredential(ctx, identityv1.VerifyCredentialRequest{Email: email, Password: pw})
    }
}
```

每条用例除了你写的期望，还自动附带四条不用写的断言：**返回值跨形态 DeepEqual**、
**事务内调用被拒**（`TX_BOUNDARY`）、**ctx 已取消时不落到实现上**（`UNAVAILABLE`）、
以及标了 `Idempotent` 的连调两次结果一致。少于两个绑定 `Conform` 直接失败——
只跑一个形态的不叫一致性测试。

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
    // target 之外的域落到远程绑定：注入生成的 HTTP client（实现同一契约接口，
    // 错误经 apperr.FromProblem 重建——错误码身份跨网络不变）。
    // appkit.Remote[identityv1.Service](func(*appkit.Registry) (identityv1.Service, error) {
    //     return identityv1.NewClient(cfg.Endpoints.Identity, "sso", nil), nil
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

## 8. 第七步：上线前要知道的四件事

这四件事在单实例本地跑不出问题，一上多副本就全是坑。框架都接好了，
你只需要知道开关在哪。

### ① 迁移什么时候跑

默认随进程启动。多副本同时起来也安全（同 schema 经 advisory lock 串行），
但 N 个副本轮流改 schema 不是你想要的部署方式。规模上来后改成两步：

```sh
./identityd -migrate -config=config/prod.yaml   # initContainer / Job：应用完迁移即退出
./identityd -target=all -config=config/prod.yaml
```

`-migrate` 用的是与正常启动同一份模块声明，不存在"迁移清单和服务清单不是同一份"。
配了 `-migrate` 却没有 `database.url` 会直接报错——空转成功比失败更危险。
服务副本要显式声明迁移由外部施加时，加 `appkit.SkipMigrations()`：

```go
AppOptions: func(bootstrap.Deps) []appkit.Option {
    return []appkit.Option{appkit.SkipMigrations()}
},
```

连接池要装 per-connection 钩子或调容量时，**不要自建池绕开 bootstrap**——那会把
迁移/outbox/幂等的装配全部重写一遍。`PoolOptions` 直接透传给生产池：

```go
PoolOptions: []pgtx.PoolOption{
    pgtx.WithAfterConnect(func(ctx context.Context, c *pgx.Conn) error {
        _, err := c.Exec(ctx, "SET statement_timeout = '5s'") // 会话级 GUC
        return err
    }),
},
```

**已应用的迁移不可再改**：历史表记着每个文件的 sha256，改动已应用的文件会让服务
以 `MIGRATION_DRIFT` 拒绝启动，错误里带修复方法。要改结构就新增一个迁移文件——
改老文件不会让库跟着变，只会让库和代码静默分叉，通常到生产才暴露。

### ② 后台任务用 `reg.Worker`，别自己起 goroutine

```go
reg.Worker("reconcile", myLoop)   // 框架起 goroutine、关停等它退出、崩了报上来
```

自己 `go func()` 的三种写法每次都要重写一遍，且每次都容易漏：关停不等它（数据半路
截断）、`OnStop` 里没 `select` ctx（关停预算耗尽也不放手，进程吊死）、run 崩了没人管
（探针依然绿着，事件却已停摆）。这三件都收在 `Worker` 里了。

### ③ 周期任务必须跨副本互斥

"每小时清理一次过期数据"在两个副本上就是每小时跑两遍。多数时候只是浪费，
偶尔是重复入账、重复发通知：

```go
reg.Worker("cleanup", job.Every(m.opts.Pool, job.Task{
    Name:     "identity.cleanup",   // 集群内唯一，带域前缀
    Interval: time.Hour,
    Run:      svc.CleanupExpired,
}))
```

锁是 Postgres 的 session 级 advisory lock：不建表、不留垃圾，副本被 `kill -9`
连接一断就自动放锁。抢不到锁的副本跳过本轮（不排队——周期任务迟一轮无所谓，
排队积压才是事故）。只想加锁不要循环时用 `job.WithLock(ctx, pool, name, fn)`。

### ④ 指标已经有了，别自己埋

框架自动产出这些（业务代码一行不写；未配 OTLP 端点时全局 provider 是 noop，近乎零成本）：

| 指标 | 标签 | 看它做什么 |
|---|---|---|
| `appkit.contract.call.duration` | system, method, outcome, error_code | 跨模块调用的 RED——进程内与远程同口径 |
| `appkit.outbox.delivery.duration` | topic, outcome | 投递速率与失败率 |
| `appkit.outbox.dead` | topic | 进死信的事件数，非零就该有人看：`appkit outbox` 列出失败原因，修好后按 ID 放回 |
| `appkit.outbox.pending` / `.oldest_pending.age` | schema | **告警看年龄不看条数**：积压 1000 条 200ms 清掉是正常的，积压 3 条最老的躺了 20 分钟说明投递停了 |
| `appkit.job.run.duration` | job, outcome（ok/error/skipped） | 任务悄悄停了（曲线归零）比任务报错更难发现 |
| `appkit.db.query.duration` | db.operation, outcome | 最常见的故障源，也最常没埋点 |

HTTP 入站不在此列：`otelhttp` 已经产出 `http.server.request.duration`（带正确的
`http.route`），再埋一遍就是双重计数。

标签集由框架钉死、业务加不进去，这是故意的：指标的成本在基数不在采集，
一个把 URL、错误消息或用户 id 当标签的埋点足以打爆后端存储。`db.operation`
只取白名单内的 SQL 动词，认不出的一律塌缩成 `other`。

需要自己加指标时用 `otel.Meter("你的域名")` 自建，别往框架标签上挂维度。

### ⑤ 线上排障：pprof 端点

性能问题（CPU 尖刺、goroutine 泄漏、锁竞争）光靠指标看不出所以然，
需要 profile。框架把标准 pprof 端点收进配置开关：

```yaml
debug:
  pprof: true    # 主端口挂 /debug/pprof/*（goroutine/heap/profile/trace…）
```

**默认关闭**——pprof 暴露进程内部信息（goroutine 栈、堆采样可能含
敏感数据），开启前先确认服务端口不暴露公网。开关走配置而不是代码：
线上排障改 configmap 重启即生效，不必发版。

```sh
kubectl port-forward deploy/ledger 8080 &
go tool pprof -http=:6060 http://127.0.0.1:8080/debug/pprof/heap
go tool pprof -http=:6060 "http://127.0.0.1:8080/debug/pprof/profile?seconds=30"
```

### 附：跨服务的 request id 是怎么串起来的

`contract` 的 ctx 防火墙会剥掉 ctx 里的一切值（进程内调用不该比跨网络调用能多传
东西），但 `callctx.Meta` 的三个字段例外——request id、tenant id、caller
在真实的跨网络调用里本来就会传：

- HTTP 入站：`httpserver` 中间件从请求头读进 ctx（自动）；
- 契约调用（进程内）：防火墙剥值后把它放回（自动）；
- 事件：outbox 发布时快照进 `Event.Meta`，relay 投递前还原进 ctx——异步链路上也连得起来（自动）。

出站 HTTP 这一段也由生成物接管：`appkit gen contract` 产出的 client 在
`NewClient` 里把传入的 `http.Client` 复制一份、焊上
`callctx.Transport{Caller: caller}` 再发请求——"忘了装 Transport"的静默
失效形态不存在。`caller` 参数填**自己**的服务名；它的语义是"谁调的我"，
留空会把上一跳的名字原样透传下去，下游据此做的归因、限流与配额也就跟着错。
链路起点看 request id，不看 caller。

裸挂 server handler（不经 `httpserver` 中间件）时元数据也不会丢：生成的
`serve` 兜底会把请求头里的 callctx 合并回 ctx；正常挂在 `httpserver.Base`
后面时这步是幂等重放，request id 的生成与回写仍是外层中间件的职责。

**要验它接没接上**，给 `apptest.Conform` 的每个 `Binding` 填上 `SeenMeta`：

```go
spy := &metaSpy{inner: echo.NewService()} // 记下服务端每次看到的 callctx.Meta
seen := func() callctx.Meta { return spy.Last() }

apptest.Conform(t,
    []apptest.Binding[echov1.Service]{
        {Name: "local", Service: echov1.WrapService(spy, 0), SeenMeta: seen},
        {Name: "remote", Service: echov1.NewClient(srv.URL, "echo", nil), SeenMeta: seen},
    },
    []apptest.Case[echov1.Service]{
        // 检查跑在这条上：它要额外发一次真实调用，得有 Idempotent 授权。
        {Name: "回显", Do: echoCall("hi"), Want: ..., Idempotent: true},
        ...
    })
```

两个绑定共用同一个 spy，比的就是"同一个实现、两种到达方式"。远程那条漏了
`Transport`，request id 与租户到不了实现，这项当场红。

两处容易踩的：它**不比 `Caller`**——出站 client 本就应该把它改写成自己的服务名，
跨形态不一致恰恰是对的；它只跑在**声明了 `Idempotent` 且期望成功**的用例上——
要额外发一次真实调用，非幂等的经不起白跑一遍（一条"创建订单"会被创建两次），
`Idempotent` 正是你已经声明过的"重复执行是安全的"。一条这样的用例都没有时，
`Conform` 会直接告诉你这项配置成了摆设。

填不填仍是自愿的，但填一次就永久有效，且填的时机是写契约测试时，
不是三个月后加第 N 个调用点时。

它是 struct 的具名字段而不是 map，所以业务代码没法自己往里塞东西（那等于把防火墙拆了）。
要读当前值：`callctx.From(ctx)`。

## 9. 第八步：CI 与约束收口

每个域仓库的 `.github/workflows/ci.yml`（`appkit sync` 生成）只有一行引用：

```yaml
jobs:
  ci:
    uses: forgeplex/appkit/.github/workflows/domain-ci.yml@main
```

这条流水线做：gofmt → vet → build → `test -race`（带 Postgres service）→
`appkit check` → `appkit sync --check`（lint 配置漂移即失败）→ `appkit schema -check`
（改了迁移没跑 `make schema` 即失败，见 4.1）→ **golangci-lint +
go-arch-lint** → `go mod tidy` 漂移检查。
顺序不是随手排的：**先验规则集未漂移，再按规则集跑检查器**——反过来就可能在
一份被改松的配置上跑出绿色。`appkit check` 本身也查规则集漂移（含配置文件被删），
所以本地 `make check` 就能拦下"把规则改松让检查变绿"这条路，不必等 CI。
配合 branch protection，**约束在 CI 不可绕过**；`//nolint` 必须写理由（nolintlint）。

两个检查器的版本由 `ruleset.GolangciLintVersion` / `ruleset.ArchLintVersion` 钉死，
域仓库 `make lint` 从同一处渲染——本地跑的和 CI 跑的是同一个二进制，
不会出现"本地绿、CI 红"。升级 appkit 后 `appkit sync` 刷新配置，
`make lint` 自然跟着换版本。

自研 analyzer（金额禁浮点、ctx 禁存 struct、decimal 不上 JSON 面）**已接进域仓库
CI**：`domain-ci.yml` 里有一步 appkit-lint，版本随域仓库 go.mod 钉的 appkit 走
（规则与依赖同版本升级）；域仓库 `make lint` 用同一套取法，本地与 CI 跑的是
同一个二进制。默认只查生产代码，`-<name>.tests=true` 连测试一起查。需要单独
手跑时：

```sh
go run github.com/forgeplex/appkit/lint/cmd/appkit-lint@v0.4.0 -moneyfloat.scope 'internal/(identity|authn)' ./...
```

## 10. 规则速查

| 你想做 | 正确做法 | 错误做法（会被什么拦住） |
|---|---|---|
| 域 A 调域 B | 合约接口 + `Resolve`，事务外调用 | 直接 require B 仓库（check/编译失败）；事务内调用（运行时守卫报 TX_BOUNDARY） |
| 跨域取数据做报表 | 订阅事件构建本域读模型 | 跨 schema JOIN（check 拒绝 SQL） |
| 发领域事件 | 用例事务内 `pub.Publish` | 事务外发（守卫拒绝）；直连 broker（业务包 import 不到） |
| 处理外域事件 | `reg.Consumer` + `outbox.Inbox` 包裹 | 裸 handler（重复投递=重复执行） |
| 写 SQL | `db/queries/*.sql` + sqlc | handler/service 里拼 SQL（depguard 拦 pgx import） |
| 表示金额 | `decimal.Decimal`（存储/运算，sqlc 全局 override NUMERIC）+ `money.Money`（需币种绑定时，领域层）+ JSON 边界字符串（入站用 `money.ParseCanonical`） | float64、裸 decimal 上 JSON 面（都在 appkit-lint 里，make lint 与 CI 都跑） |
| 返回错误 | 合约错误码（`apperr.Is(err, identityv1.CodeXxx)` 单体/微服务行为一致） | 字符串比对、裸 errors.New 跨层 |
| 跑后台任务 | `reg.Worker(name, run)` | 自己 `go func()`（关停不等它、崩了没人管） |
| 跑周期任务 | `reg.Worker(name, job.Every(pool, ...))` | 裸 `time.Ticker`（多副本每轮重复执行） |
| 改数据库结构 | 新增 `000N_xxx.sql`（建表同一文件里写 `COMMENT ON TABLE`）+ `make schema` | 改已应用的迁移文件（启动报 MIGRATION_DRIFT）；手改 `db/schema/`（CI 报漂移） |
| 搞清楚现有表长什么样 | 读 `db/SCHEMA.md` / `db/schema/<表>.sql` | 翻整条迁移历史在脑子里重放 |
| 加业务指标 | `otel.Meter("你的域名")` 自建 | 往框架指标上加标签（基数无界） |
| 传 request id / 租户 | `callctx.Meta`（入站/契约/事件自动，生成 client 出站自动焊 `Transport`） | 自己往 ctx 塞值（跨契约调用会被防火墙剥掉） |
| 确认拆分部署不会变行为 | 提供方域仓库里写一条 `apptest.Conform`（local + remote 两个绑定） | 只测本地实现（远程那条路径第一次跑就是在生产） |

## FAQ

**Q：一开始就要拆三个仓库吗？**
不必。可以先只建 `identity` 一个域 + `sso` 组合仓库跑通全链路，authn/clients
的雏形先作为 identity 里的普通代码，边界清晰后再 `new domain` 迁出——
迁移成本主要是搬文件，因为方向性约束保证了代码本来就没有纠缠。

**Q：`appkit dev` 和发版是什么关系？**
本地联调用 go.work（不提交）；跨仓库正式依赖靠 tag：改了合约仓库 → 打 tag →
域仓库升 require → 域仓库打 tag → 组合仓库升 require。appkit 自 v0.1.0 起
按 SemVer 发版（CI 有 apidiff 门禁保证向后兼容），域仓库直接 require 版本即可；
`appkit dev` 只在联调本地未发布改动时需要。每个版本的更新说明见
[../CHANGELOG.md](../CHANGELOG.md) 与
[GitHub Releases](https://github.com/forgeplex/appkit/releases)。

**Q：想让 AI agent 参与开发？**
`appkit new` 生成的每个仓库自带 `AGENTS.md`（+`CLAUDE.md` 引用）：仓库地图、
三条方向性约束、加功能的固定六步、惯用法与"完成的定义"。agent 打开仓库即知
规则；违规也会被 `make check` 与 lint 拦下——约束体系对人和 AI 一视同仁。

**Q：模块还需要哪些生命周期钩子？**
一般不需要。连接池、迁移、HTTP、relay、优雅关停都由框架或骨架接好；
自定义后台任务用 `reg.Worker(name, run)` 一行（框架负责起协程、关停等待、
异常上报）。`reg.OnStart` / `reg.OnStop` 留给真正需要自定义时序的场景。

**Q：我能改坏框架吗？**
框架代码在 module cache 里（`0444` 只读），改不到。启动装配也已经收进
`appkit/bootstrap`，你的 `main.go` 只剩模块清单。框架生成的公共代码
（lint/CI 配置、sqlc 产物、基础迁移）留在你仓库里但带生成头，改了会被
`appkit check` 拦下——包括"把规则改松"这一手。你写的代码在 `internal/`，
和上面两类不共处一处。

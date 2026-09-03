# 多租户组合教程：运营平台 + 多商户

这份教程回答一个系统级问题：**产品同时服务「运营方」和「N 个商户」时，
租户怎么建、域怎么写、权限怎么授、组合根怎么拼**。

单域的多租户机制（分区域 / 租户域的生成变体、RLS、租户身份从哪来）在
[GUIDE](GUIDE.md) §3.1 / §3.2——本篇是它们之上的组合层，讨论的是这些机制
怎么协同成一个产品形态。通读 GUIDE §1–§7（建域、写用例、组合、跑起来）
是前置；全篇以 example 组合仓库为实证参照，代码块凡标注「示意」的是
教学示例，其余均可在真实仓库里找到。

一个总纲先立起来，后面每节都是它的展开：

> **域提供机制（本租户谓词、权限码、双端点面），组合根决定策略（谁持
> 哪些码、哪些角色、拼哪些模块）。「运营平台」是组合层概念，任何域
> 仓库里都不该出现它。**

## 1. 三层身份，一个分区

系统的全部租户身份只有三层：

```
分区（rbac 分区键 → schema，物理隔离）
 ├── 平台租户（tenant = 平台自己的 uuid）
 │     └── 运营人员：platform-admin / ops-staff 角色，持跨租户权限码
 └── 商户租户 × N（tenant = 每商户一个 uuid）
       └── 商户员工：merchant-admin / merchant-staff 角色，只持本租户码
```

三个论断，每个都反直觉但都是这个模型的地基：

**① 运营平台自己也是一个租户。** 它不是特权层，没有「上帝租户」。平台
的管理员、平台自己的文件和邮件，都落在平台租户的 `tenant_id` 下——和
商户的数据走完全相同的隔离机制。平台与商户的差别只在**权限授予**（第 4
节），不在数据通道。

**② 分区的维度是「数据要不要互相看见」，不是「前端有几个」。** 运营
要管商户（重置密码、冻结店铺），所以运营与商户放**同一个 rbac 分区**
（一套 schema、行级隔离）。加了第二个、第三个前端不构成开新分区的理由；
只有当两组租户的数据彻底互不相关（或合规要求物理隔离）时才拆分区。
example 的单分区形态（`partitionKey = "demo"` → schema `rbac_demo`）就是
这个最小例：一个分区内一个管理员租户起步，商户租户随业务逐个开。

**③ 分区键 ≠ 业务租户。** `callctx` 里这两个维度各走各的：分区键决定
事务路由到哪个 schema，业务租户决定行落在哪个 `tenant_id`。混用会有
真实事故——example 早期验证码邮件死信，就是 rbac 的分区键 `demo` 经
outbox 事件 meta → relay → `callctx` 传播，被 email 域当成业务租户查渠道
（`tenant_id = 'demo'` 零匹配）。判例见第 7 节③。

## 2. 域选形态：一张表定终身

GUIDE §3.2 末尾给过选择口诀，放到运营+多商户语境里展开：

| 域 | 形态 | 理由 |
| --- | --- | --- |
| rbac | 分区域（`-partitioned`） | 账户体系按分区物理隔离，分区即 schema |
| order / docs 等业务域 | 租户域（`-tenant`） | 全部商户共模型、量级单库可容，RLS 行级隔离 |
| files / email / notification | 已有能力域 | 租户语义现成（第 6 节），零改动直接用 |
| merchant | **按需** | 见下 |

```sh
appkit new domain order -tenant -dir ../order
```

**merchant 域的按需判据**：如果「商户」只是 tenant_id 的集合加一点配置，
rbac 的租户目录就够了，**不要建域**；只有当商户有生命周期不变量——入驻
审核流、保证金、冻结仲裁、结算配置版本化——它才配得上一个箱子。为
「多商户」三个字先建一个 CRUD 域是常见过度设计，等第一个不变量出现
再建，域的边界反而清楚。

## 3. 域怎么写：双用例面

跨租户管理（运营看所有商户的订单）在域内的形态是**两组用例、两个端点、
两份 DTO**，不是同一端点按调用方身份 if-else：

```go
// internal/order/service.go —— 商户视角：租户从 ctx 来，谓词焊在 SQL 里
func (s *Service) MyOrders(ctx context.Context, page Page) ([]OrderView, error)

// internal/order/service_admin.go —— 运营视角：跨租户分页，handler 先查码
func (s *Service) AllOrders(ctx context.Context, f AdminFilter, page Page) ([]AdminOrderView, error)
```

（示意。真实判例看 rbac 域：它的管理端点与登录端点就是同一域内不同用例面。）

三条纪律：

1. **My\* 永远不带租户参数**——租户从 `callctx.From(ctx).TenantID` 取，
   SQL 恒带 `WHERE tenant_id = $1`。漏写谓词由 RLS 兜底（GUIDE §3.2
   机制三：查不到别家行，写不进别家 tenant_id）。
2. **All\* 的门是权限码，不是身份**。handler 上绑 `order:read-all` 这类
   域声明的码；谁持码由组合根第 4 节决定。域从头到尾不知道「运营」
   是谁——换一个组合、把同一域卖给纯单租户系统，All\* 用例零改动。
3. **两份 DTO 天然解决字段裁剪**。「运营界面显示 10 个字段、商户界面
   显示 5 个」若是**商户不该知道**（成本价、风控分），裁剪发生在两份
   DTO 的序列化里，前端不渲染不算数；若只是**页面排版**，商户端少编
   几个字段即可。判断口诀：商户打开 devtools 看到 API 响应里多出来的
   字段，算不算事故？算 → 两份 DTO 分开；不算 → 前端自己裁。

## 4. 权限码：声明 → 授予 → 判定

四个环节，各自有唯一落点（GUIDE「鉴权与权限」章有完整机制面）：

**① 声明（域仓库，Register 阶段）。** 各域经 `reg.Permissions` 自声明，
rbac 启动时从 `reg.PermissionDecls()` 汇总落库。这是 v0.7.x 的约定——
组合根手工注入目录的旧环节已退役：

```go
// 域 Register 的第一件事（apps/files/internal/module/module.go 实例）
reg.Permissions(files.PermissionCatalog()...)
```

目录定义在域的 internal 包、根包透出，组合根引用不手抄字符串：

```go
// apps/files/files.go
const (
	PermUpload   = files.PermUpload
	PermDownload = files.PermDownload
	PermDelete   = files.PermDelete
)
func PermissionCatalog() []appkit.PermissionDecl { return files.PermissionCatalog() }
```

跨租户码（如 `order:read-all`）与普通码同机制，只是授予面不同——
建议在 Description 里写明跨租户语义，管理界面读得到。

**② 授予（组合根，SystemRoles）。** 「谁持哪些码」是部署面决策，落
在组合根的 `rbac.Partitions` 里：

```go
// apps/example/cmd/example/main.go 实例（节选）
Partitions: map[string]rbac.PartitionSpec{
	partitionKey: {
		Schema: "rbac_demo",
		SystemRoles: []rbac.SystemRoleDef{
			{Code: "admin", Name: "管理员", PermissionCodes: adminCodes},
			{Code: "viewer", Name: "只读", PermissionCodes: []string{"users:view", ...}},
		},
	},
},
```

运营+多商户系统里，这里就是第 1 节租户身份矩阵的落点：平台角色持
跨租户码，商户角色只持本租户用得上的码。角色码本身也是组合事实——
`merchant-admin`、`platform-admin` 这些名字属于你的系统，不属于任何域。

**③ 快照（rbac，登录时）。** 令牌里带权限快照（perms claims），验签
后 `Actor` 即可用——权限判定不打库。

**④ 判定（两个门）。** 域内端点用 `reg.Require(code, handler)` 绑码
（demo.go 有一字不差的判定矩阵：未认证 401 / 无码 403 / 有码 200，
挑战码还须新鲜 step-up 证明）；框架侧由组合根挂 `authn.Middleware` 验签。
files / ledger 域的 HTTP 面尚未绑码（判定归组合根/网关的部署现状）——
新域从第一天就绑，域内判定与框架机制并存互补。

## 5. 组合根：从单组合到双组合

**起步：单组合根，单前端，按角色渲染。** example 就是这个形态——一份
`main.go` 装配全部域，web 登录后 admin 看到「管理」页、普通用户看不到。
这个形态的上限是「两类用户共用一个部署单元」，多数系统在此阶段能跑
很久。

**分家：两个组合根，同一个库、同一个分区。** 当运营前端与商户前端
由不同团队迭代、或运营端要做网络隔离（金库区）时，拆：

```
apps/
├── rbac/  files/  email/  order/ …        # 域仓库（货架，互不 require）
├── admin/                                 # 运营平台组合根
│   └── cmd/admin/
│       ├── main.go          # wiring：同一批域，admin 角色持跨租户码
│       ├── adapters_*.go    # 运营侧翻译器（商户审核通过 → 欢迎邮件）
│       └── web/             # 运营前端
└── portal/                                # 商户门户组合根
    └── cmd/portal/
        ├── main.go          # wiring：同一批域，商户角色集（无跨租户码）
        ├── adapters_*.go    # 商户侧翻译器（订单完成 → 站内信）
        └── web/             # 商户前端
```

拆的物理动作是**新建一份几百行的 wiring + 各自的前端**，域仓库一行不动；
两个组合根连同一个数据库、同一个 rbac 分区（第 1 节论断②）。翻译器
箱子跟着产品线走——哪条业务流的事件翻译，归那条流的组合根（跨域协作
的完整模式见 example 的 logincode.go：流程归属主域、通道域零改动、
翻译归组合根）。

再往后，流量大了把 portal 的 order 模块单独部署（`-target`）即得微服务
形态；反过来两个组合先各跑一个小单体也完全可以。**部署形态是启动参数，
不是架构决策**——这句话的根基是域仓库对组合方式一无所知。

## 6. 能力域的租户语义是现成的

拼系统时先查货架，别重造：

- **files**：`owner` 列严格隔离（全部查询带 owner 谓词，秒传去重键是
  `(owner, sha256)`）；物理对象表 blobs 刻意无租户——内容寻址的字节
  去重天然跨租户，但元数据互不可见。平台素材给商户用，是可见性需求
  出现后再做的域语义演进，起步别加。
- **email**：`tenant_id` 行隔离；发送候选在租户没配渠道时兜底全局渠道
  （`tenant_id = ''`），管理列表仍严格本租户——「平台配一套发信渠道、
  商户不各自配也能收到事务邮件」开箱即用。
- **notification / audit / outbox**：同一模式——业务表带租户，基础设施
  表（outbox / inbox / 幂等 / 审计基础设施部分）不带。

## 7. 判例集（每个都真实发生过）

**① 代操作落被操作租户。** 运营帮商户重置密码、传文件，调用落库必须
落在**商户的 tenant_id**，不是平台的——否则数据边界串了。跨租户管理
的写路径 = All\* 用例 + 显式指定目标租户 + 平台侧权限码，三者缺一不可。

**② 新前端 ≠ 新分区。** 参见第 1 节论断②。反例代价：把商户门户开在
独立分区，运营就管不了商户了，跨分区管理没有机制支撑。

**③ 事件链路会传播租户身份。** outbox 事件带发布时的 callctx meta，
relay 投递时还原成 `callctx`——下游域 `tenantOf(ctx)` 取到的是**发布方
当时的租户**。验证码邮件死信判例：发布方分区键被下游当业务租户。跨域
事件翻译器（组合根）是修正租户语义的正确位置，别让下游域猜。

**④ 域仓库 appkit 版本要对齐。** MVS 钻石下，组合根能用的新 API 需要
各域仓库先升：files 钉在 v0.5.3 时组合根看不到 `PermissionDecl`。升级
是常规操作（向后兼容由 apidiff 门禁保证），`go get github.com/forgeplex/appkit@vX.Y.Z && go mod tidy` 即可。

**⑤ 字段裁剪先分语义。** 第 3 节纪律 3 的 devtools 口诀。「不渲染 ≠
不可见」——安全裁剪必须发生在可信边界内（域的 DTO / 组合根），展示
裁剪在前端。把 UI 偏好写进域（每改版一次域发一次版）是反模式。

**⑥ 目录自声明，组合根不代抄。** 组合根里出现 `reg.Permissions` 声明
别的域的码，说明那个域欠账了——去域仓库补（第 4 节①的形态），组合根
的代声明 shim 生命周期应按天计。example 的 files-perms / ledger-perms
两个 shim 从落地到退役就是完整示范。

## 8. 速查

| 问题 | 答案 | 详 |
| --- | --- | --- |
| 运营平台是什么 | 一个普通租户，差别只在权限授予 | §1① |
| 商户怎么隔离 | 租户域：tenant_id 列 + RLS | §2 / GUIDE §3.2 |
| 什么时候开新分区 | 数据互不相关 / 合规要求，与前端数量无关 | §1② |
| 跨租户管理怎么写 | All\* 用例 + 域声明跨租户码 + 组合根授予 | §3 / §4 |
| 字段 10 vs 5 | 不该知道 → 双 DTO；纯排版 → 前端裁 | §3③ |
| 运营/商户何时拆组合根 | 前端团队分头 / 网络隔离；拆时同库同分区 | §5 |
| 平台发信、商户收信 | email 全局渠道兜底，零配置 | §6 |
| 跨域事件里租户错了 | 修组合根翻译器，别让下游猜 | §7③ |

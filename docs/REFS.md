# 可选业务引用：refs

`github.com/forgeplex/appkit/refs` 统一具名业务引用的值、契约定义和校验。
它解决“通用 order 可以表达 merchant/account/channel，也能用于完全没有 merchant 的项目”，
不把所有资源变成同一个 BaseModel，不代替租户、业务状态或权限模型。

本能力自 v0.9.2 提供，是新增的可选 API；v0.9.1 不包含它。

## 1. 本轮提供什么

| 已提供 | 不在本轮范围内 |
|---|---|
| 不可通过外部 map 修改的 `refs.Values`、严格 JSON 编解码 | JSONB 驱动适配、`sql.Scanner` / `driver.Valuer`、自动 sqlc override |
| 版本化 `Spec` / `Schema`，允许键、类型、必填及不可变规则 | 自动解析对象、商户账户归属契约、鉴权 |
| 完整资源、更新前后快照、部分筛选条件校验 | 查询构建器、AND/OR 查询执行、分页执行、性能保证 |
| 契约 YAML 可选 `type: refs` → DTO `refs.Values` | 事件 YAML 的 `type: refs`、自动给所有表加字段 |
| 两种订单契约的可运行内存示例与回归测试 | 数据迁移、在线部署、启动期建索引或动态 DDL |

## 2. 值与资源规范分开

领域对象主动选择使用 refs，核心字段照旧独立：

```go
type Order struct {
    ID       string      `json:"id"`
    TenantID string      `json:"tenant_id"`
    Refs     refs.Values `json:"refs"`
    // Amount、Currency、Status 等仍是订单自己的显式字段。
}
```

PSP 的 JSON 值是一个扁平对象，不是四条关联记录，也不是身份上下文：

```json
{
  "merchant_id": "11111111-1111-4111-8111-111111111111",
  "merchant_account_id": "22222222-2222-4222-8222-222222222222",
  "channel_group_id": "33333333-3333-4333-8333-333333333333",
  "channel_id": "44444444-4444-4444-8444-444444444444"
}
```

`refs.NewValues(map[string]string)` 校验通用形状并复制输入，`Values.Map()` 返回副本；
修改输入或输出 map 都不会修改值本身。零值表示空引用，序列化为 `{}`。
`Get` / `Len` 用于读取；没有 key 表示缺席，不用空字符串或 JSON `null` 表示没有引用。
值序列化按键稳定排序，反序列化完整替换、不合并；失败保留接收者原值。

通用形状的硬限制：最多 64 项，每个键最多 64 字节，每个 ID 最多 256 字节，
直接 JSON 解码最多 128 KiB。键为小写 ASCII 段，以字母开头，其后允许字母、数字和下划线，
段间用点分隔，例如 `merchant_id`、`psp.merchant_account_id`。
ID 必须是非空有效 UTF-8，不能含空白或控制字符；不自动 trim、转大小写或补格式。
`DecodeJSON` 拒绝重复键（包括转义后相同的键）、非字符串值、数组、嵌套对象、
null、尾随第二个 JSON 值、畸形 UTF-8 和无配对的 Unicode surrogate。

类型声明与真实业务事实不同：框架接受一个有效 UUID，只说明其格式合法，
不说明对象存在、属于当前租户，或允许当前调用者使用。

## 3. 定义共享、版本化的资源契约

以下声明应属于资源的共享契约代码，调用方、提供方、事件生产者/消费者采用同一规范。
组合根选择已声明的规范并注入适配器，不能按每个实例的环境变量悄悄改规则。

```go
schema, err := refs.NewSchema(refs.Spec{
    Domain: "order", Resource: "psp_order", Version: 1,
    Definitions: []refs.Definition{
        {Key: "merchant_id", Target: "merchant.merchant", Format: refs.FormatUUID, Required: true, Immutable: true},
        {Key: "merchant_account_id", Target: "merchant.account", Format: refs.FormatUUID, Required: true, Immutable: true},
        {Key: "channel_group_id", Target: "channel.group", Format: refs.FormatUUID, Immutable: true},
        {Key: "channel_id", Target: "channel.channel", Format: refs.FormatUUID, Immutable: true},
    },
})
if err != nil {
    return err // 无效规范应在装配期失败，不带病接流量。
}
```

`Domain` / `Resource` 使用不超过 64 字节的小写 ASCII 单段标识符；`Version` 必须非零。
`Target` 至少两段、总长不超过 128 字节，每段不超过 64 字节，表达 ID 指向什么，
不会据此查数据库或发起网络请求。`Format` 必须显式声明：
`FormatOpaque` 使用通用字符串 ID 规则，`FormatUUID` 要求非零、规范小写、36 字符带连字符的 UUID，
不额外限制 UUID version / variant。
`Schema.Spec()` 返回独立副本；构造后修改原始 `Spec` 不会修改已构造的规范。

`order.psp_order/v1` 与不含 merchant 的 `order.store_order/v1` 可以使用同一 `Order` Go 类型，
但它们具有不同的资源契约身份。版本不自动塞进每个 refs JSON；契约端点、事件信封或资源记录
必须让接收方知道应使用哪个规范。若多个版本共存且无法从端点确定，域需要单独存储/传输规范标识。
AppKit 不提供全局 schema registry 或自动版本协商。

新增键也要考虑旧版严格解码方会拒绝它。修改目标资源、ID 格式、必填性或含义，必须审查兼容性、
数据回填和升级顺序；不能因为 `Version` 是一个数字就自动获得兼容性。
`appkit contract-check` 比较生成器的契约 YAML，不会自动比较 Go 代码中的两个 `refs.Spec`。

## 4. 四种校验入口

| 入口 | 检查内容 | 不承担的责任 |
|---|---|---|
| `schema.DecodeJSON(raw)` | 严格 JSON 解码 + 完整资源规则 | 对象存在性、权限、数据库写入 |
| `schema.Validate(values)` | 已声明键、格式、所有必填键 | 账户归属、阶段业务规则 |
| `schema.ValidateFilter(values)` | 已声明键和格式，不要求所有必填键；空筛选合法 | 权限、租户约束、查询执行、代价控制 |
| `schema.ValidateUpdate(before, after)` | 两个完整快照都合法；已存在的 immutable 值不可修改或删除 | 数据库并发控制、自动 PATCH 合并 |

例如商户和账户在创建时必填，渠道可在路由时首次赋值：

```go
initial, err := schema.DecodeJSON(createRefsJSON)
if err != nil {
    return err
}
// 解析 merchant 契约并验证 scope / 所属关系，再开启本地写事务。

filter, err := refs.DecodeJSON(filterJSON)
if err != nil {
    return err
}
if err := schema.ValidateFilter(filter); err != nil { // 可只带 merchant_id + channel_id。
    return err
}

if err := schema.ValidateUpdate(initial, routedRefs); err != nil { // 可首次赋值。
    return err
}
```

`ValidateUpdate` 接收的是完整旧值和完整新值，不是 patch；旧值必须来自可信的持久化快照，
不能相信客户端提交的“修改前数据”。若 HTTP 使用 PATCH，域需明确缺席与删除的
协议、先与当前快照合并，再校验完整结果。必填和不可变同时成立：已保存的商户账户不能被清空或替换。
“channel 与 channel_group 必须同时提供”或“只有某状态才可路由”仍是订单的业务规则。
完整快照规则先于不可变比较：删除一个同时 Required 与 Immutable 的键时，先返回必填缺失错误。
示例把渠道定义成一次赋值；允许重路由的真实产品应定义自己的状态机/尝试记录，不要原样照搬该限制。

并发更新时不能拿过期的 before 校验通过就覆盖新数据。域应在同一个本地事务内加行锁读取并校验，
或使用 `WHERE version = expected_version` 的乐观锁，冲突后重读、重校验。框架的纯值校验不是数据库约束。

错误均为 `*apperr.Error`，可以使用 `apperr.Is(err, refs.CodeRequiredReference)` 判断，
不比较错误字符串；错误不包含原始 ID。

| 错误码常量 | 含义 | HTTP 状态 |
|---|---|---|
| `CodeInvalidSchema` | 规范配置无效或未构造 | 500 |
| `CodeInvalidValues` | 通用值/严格 JSON 不合法 | 422 |
| `CodeUnknownReference` | 资源未声明的键 | 422 |
| `CodeRequiredReference` | 完整资源缺少必填引用 | 422 |
| `CodeInvalidID` | ID 不符合声明格式 | 422 |
| `CodeImmutableReference` | 修改/删除已赋值的不可变引用 | 409 |

## 5. 归属、安全与事务

生产调用路径应是：

1. 验证调用身份与操作权限，取得可信 tenant/partition scope。
2. 校验 refs 结构与对应资源规则。
3. 通过商户、渠道域的窄契约，校验或解析账户→商户、渠道→渠道组，确保在允许的 scope 内。
4. 在 order 自己的事务内写订单、refs 和 outbox 事件；更新还要进行并发控制。

需要独立部署时，契约调用使用经过验证的服务身份与显式 scope 委托；服务验签成功仍不代表能操作任意对象。
这些调用在 order 事务外进行，遵守 `contract.Call` 事务边界。
如果需要“账户冻结与订单创建严格互斥”，必须额外设计授权/预留协议或重审域边界；先查询后写入不提供跨域原子性。

`refs` 不会进入 `callctx`，不会改变 Actor/Principal、TenantID 或 Partition，也不沿外部 ID 自动执行 RLS。
`merchant_id` 只有在产品明确把商户作为租户时才可能与 TenantID 代表同一对象；仍须由可信业务规则保证一致，
不能用请求体的商户 ID 覆盖认证所得 tenant。JSONB 查询始终需要原本的租户隔离与权限控制。

## 6. 契约生成与手写入口

契约 YAML 在实际需要的字段上选择 `type: refs`：

```yaml
version: 1
package: orderapi
system: order
methods:
  - name: Create
    path: /create
    doc: 校验资源引用与业务归属后创建订单。
    request:
      - {name: refs, type: refs}
    response:
      - {name: order_id, type: string}
      - {name: refs, type: refs}
```

生成 Go DTO 使用 `Refs refs.Values`，OpenAPI 表达字符串值的对象。
`required: true` 是生成器现有的 string 字段规则，不用于声明 refs 子键必填；后者在 `Spec` 中定义。
含 refs 的生成 HTTP 请求上限为 1 MiB、64 层嵌套，会拒绝无效 refs、重复 JSON 键
（包括转义/大小写折叠导致的 DTO 字段覆盖）、非对象的请求根和尾随第二个 JSON 值，
边界解码错误使用现有 `INVALID_ARGUMENT`（422）。refs 值本身仍受上一节更小的限制。
未知外层 DTO 字段继续允许，以保留契约向前兼容；无 refs 的请求不改变原有生成行为。
进程内和远程服务实现仍然都必须调用 `Schema.Validate`，传输验证不替代领域验证。
`ValidateFilter` 也不把筛选条件变成已授权对象列表。

手写 handler 必须限制整个请求体大小并使用严格的边界解码；直接解码 refs 对象可用
`Schema.DecodeJSON`。只在普通 struct 中嵌入 `refs.Values` 不会自动让整个外层 JSON 的重复字段被拒绝，
例如两次出现外层 `refs` 字段，需要 handler 自己严格校验或使用生成器提供的边界。
普通 Go 事件 DTO 可以使用 `refs.Values` 编码；当前 `gen events` YAML 还没有 `type: refs`。

## 7. JSONB 是域的存储选择，不是自动适配

可以把 `refs.Values.MarshalJSON()` 的结果交给域的 postgres adapter，存进 order 自己的 JSONB 列；
读取后重新严格解码和校验。选择固定列的域也能在边界转换成同样的引用值。
如果增加查询投影，要指定唯一事实源及更新方式，不允许 JSONB 和普通列各自被业务独立写入。

下面仅是**已有 orders 表的查询/索引设计示意**，不是 AppKit 提供的迁移或已测量的生产查询。
需由所属域在新迁移中建立索引、在 `db/queries/*.sql` 中用 sqlc 实现 SQL，不能启动时从客户端键名拼 DDL。

```sql
-- 经过 ValidateFilter 后，把整个扁平筛选对象绑定为 $2；多个键同时匹配。
-- $1 必须来自可信 scope，并保留域原有的 RLS；这里展示第一页。
SELECT id, refs, created_at
FROM orders
WHERE tenant_id = $1 AND refs @> $2::jsonb
ORDER BY created_at DESC, id DESC
LIMIT $3;

CREATE INDEX orders_refs_gin ON orders USING gin (refs jsonb_path_ops);

-- 高频“某商户最新订单”可使用匹配表达式的独立索引。
CREATE INDEX orders_merchant_latest
ON orders (tenant_id, (refs->>'merchant_id'), created_at DESC, id DESC);
```

`@>` 可用 GIN 支持包含匹配，`jsonb_path_ops` 不覆盖所有 JSONB 操作；须按实际条件与数据分布验证计划。
参见 [PostgreSQL JSONB 索引说明](https://www.postgresql.org/docs/18/datatype-json.html#JSON-INDEXING)。
GIN 不提供时间排序；上面的 B-tree 表达式索引要配 `refs->>'merchant_id' = $2` 的查询，
不承诺同一个 `@>` 查询会自动使用它。参见 [PostgreSQL 排序与索引](https://www.postgresql.org/docs/18/indexes-ordering.html)。
后续页还需 `(created_at, id)` 游标条件；公开端点的条数上限、可用筛选组合、超时和性能验收都归所属域。
没有一条通用索引可以保证任意组合筛选与排序都便宜。

JSONB 更新仍锁整行，不是每个引用单独锁。大量关联、多值关系或各引用需要独立状态/历史时，
应考虑独立关联表；少量固定高频维度也可以直接用普通列。
参见 [PostgreSQL JSON 文档设计](https://www.postgresql.org/docs/18/datatype-json.html#JSON-DOC-DESIGN)。

## 8. 本地验证

在 AppKit 仓库根目录执行：

```sh
go run ./examples/refsorder
go test ./refs ./examples/refsorder
```

[示例说明](../examples/refsorder/README.md) 包含 PSP 四引用与无 merchant 订单、校验成功/失败案例，
以及生成契约到临时目录的命令。示例的授权和所有权解析器是明确标注的测试桩；不访问数据库、不运行真实业务。

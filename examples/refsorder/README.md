# 可选 refs：同一订单模型，两个业务契约

在 AppKit 仓库根目录运行，不需要数据库、网络服务或业务凭证：

```sh
go run ./examples/refsorder
go test ./examples/refsorder
```

示例使用同一个 `Order` Go 类型，分别装配 `order.psp_order/v1` 与
`order.store_order/v1` 引用规范。前者有 merchant、merchant account、
channel group、channel 四个引用；后者没有 merchant，只引用 store 和 salesperson。
两者是不同的资源契约，不会按启动环境重新解释同一个契约版本。

示例还展示严格 JSON 解码、完整资源校验、部分条件筛选校验，以及渠道引用的
首次赋值与不可变保护。测试覆盖错误 ID、未知键、重复 JSON 键、关联不一致、
缺少可信 scope、跨租户 scope 和不可变引用修改。

`fakeAuthorizedScope` 和 `fakeOwnershipResolver` **都是固定测试桩**，不提供真实
认证、授权或 merchant/channel 查询能力。输出是待持久化的候选订单：没有服务、
SQL、事务、迁移、存储或实际列表查询。线上必须由真实鉴权和域契约取代这些测试桩；
本地写事务只能保证 order 自己的数据原子性，不能与外域账户冻结保持原子一致。

`contract.yaml` 演示契约生成器的可选 `type: refs`。可在临时目录查看生成物：

```sh
refs_example_dir=$(mktemp -d)
go run ./cmd/appkit gen contract -in examples/refsorder/contract.yaml -dir "$refs_example_dir"
```

生成 DTO 使用 `refs.Values`；字段的格式与传输校验不等于资源规则或业务授权。
提供方仍要调用对应的 `Schema.Validate`，并通过业务契约校验对象归属。
当前事件 YAML 生成器未增加 `type: refs`，手写 Go 事件 DTO 可以使用 `refs.Values`。

完整 API 约定与 JSONB 存储/查询边界见 [docs/REFS.md](../../docs/REFS.md)。

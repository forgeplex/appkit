// Package appkit 是 forgeplex 后端服务的运行时框架。
//
// 设计文档见 docs/DESIGN.md。本包是全框架的稳定面：模块接缝上只出现
// 标准库类型（http.Handler、fs.FS、context.Context）与本仓库内零驱动依赖的
// 小包（health、tx、apperr）。gin、pgx、otel 等第三方类型不得出现在本包 API 中。
package appkit

import (
	"context"
)

// Module 是业务域接入框架的唯一契约。
//
// Register 只做「声明」：Provide 契约实现、Mount 路由、登记迁移/消费者/健康检查/
// 生命周期钩子。任何需要解析依赖的构造必须放进 Provide 的构造函数或 Setup 回调，
// 由装配阶段统一执行——因此模块的注册顺序不影响结果。
type Module interface {
	Name() string
	Register(reg *Registry) error
}

// HookFunc 是生命周期钩子。ctx 在框架关停时被取消。
type HookFunc func(ctx context.Context) error

// 启动阶段：OnStart 钩子按 stage 升序执行，同 stage 按注册顺序。
// 关停时 OnStop 钩子按启动逆序执行：stage 降序、同 stage 注册逆序。
const (
	// StageInfra 用于基础设施（连接池、schema 迁移等）。
	StageInfra = 10
	// StageWorker 用于后台工作者（outbox relay、消费者等）。
	StageWorker = 20
	// StageServer 是框架启动 HTTP 服务器的阶段；模块钩子一般不用。
	StageServer = 30
)

// Event 是跨模块事件的传输形态（经 outbox 发布、经 inbox 去重消费）。
// Payload 是合约仓库定义的事件 schema 的 JSON 序列化。
type Event struct {
	ID      string
	Topic   string
	Payload []byte
	Meta    map[string]string
}

// EventHandler 处理一条事件。返回错误则投递会被重试，实现必须幂等
// （框架的 inbox 去重只保证「至少一次投递、至多一次生效」协作成立）。
type EventHandler func(ctx context.Context, evt Event) error

// Subscriber 是事件总线的订阅端最小接口（outbox.DirectBus 实现了它）。
// 通过 Bus 选项注入后，框架在装配阶段把 Registry.Consumer 登记的消费者
// 逐条 Subscribe；有消费者却未注入 Bus 会在启动期报错。
type Subscriber interface {
	Subscribe(topic string, h EventHandler)
}

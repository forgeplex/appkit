// Package gateway 是示例消费方模块：经契约边界调用 greet 域。
// 它只 import greetapi——拿到本地实现还是远程 client 由组合根的
// target/Remote 配置决定，本包代码在两种形态下一行不改。
package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/examples/greeter/greetapi"
	"github.com/forgeplex/appkit/httpserver"
)

// Pattern 是本模块唯一路由。导出供测试挂载同一 pattern。
const Pattern = "GET /hello/{name}"

// Module 返回 gateway 模块。
func Module(log *slog.Logger) appkit.Module { return &module{log: log} }

type module struct {
	log *slog.Logger
	// h 在 Setup 阶段装配（依赖解析之后）；Mount 必须发生在 Register 阶段，
	// 因此路由经一层间接引用到 h——Setup 先于 HTTP 监听，不会打到 nil。
	h http.Handler
}

func (m *module) Name() string { return "gateway" }

func (m *module) Register(reg *appkit.Registry) error {
	reg.Setup(func(context.Context) error {
		// greet 在 target 集内 → 本地实现；不在 → 组合根注册的 Remote 兜底。
		// 两者都没有时在装配阶段 fail-fast，而不是第一个请求 500。
		m.h = NewHandler(m.log, appkit.MustResolve[greetapi.Service](reg))
		return nil
	})
	reg.Mount(Pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.h.ServeHTTP(w, r)
	}))
	return nil
}

// helloReply 是 gateway 自己的响应形态：聚合下游契约结果，不透传对方 DTO。
type helloReply struct {
	Message string `json:"message"`
	Via     string `json:"via"`
}

// NewHandler 返回 /hello 处理器。对 svc 的每次调用都必须经 contract.Call：
// 进程内调用同样带运行时守卫（事务内直接失败）、ctx 防火墙、独立超时与
// 错误规范化——单体与微服务两种形态下语义一致（DESIGN §5.3）。
// 真实项目中 contract.Call 藏在合约仓库生成的 wrapper/client 方法体内。
func NewHandler(log *slog.Logger, svc greetapi.Service) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := greetapi.GreetRequest{
			Name: r.PathValue("name"),
			Lang: r.URL.Query().Get("lang"),
		}
		reply, err := contract.Call(r.Context(), "greet", "Greet", 0,
			func(ctx context.Context) (greetapi.GreetReply, error) {
				return svc.Greet(ctx, req)
			})
		if err != nil {
			httpserver.WriteError(log, w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(helloReply{Message: reply.Message, Via: "gateway"})
	})
}

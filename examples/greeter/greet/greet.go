// Package greet 是示例提供方模块，演示业务域模块的标准形态：
// Provide 契约实现供同进程消费方 Resolve，Mount 自己的 HTTP 路由。
// 真实域 repo 中实现全部在 internal/，对外只导出 Module()。
package greet

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/examples/greeter/greetapi"
	"github.com/forgeplex/appkit/httpserver"
)

// Pattern 是本模块唯一路由（标准库 ServeMux 语法）。导出供测试挂载同一 pattern。
const Pattern = "GET /greet/{name}"

// Module 返回 greet 模块。真实域 repo 中这是唯一导出入口（DESIGN §1 铁律 3）。
func Module(log *slog.Logger) appkit.Module { return &module{log: log} }

type module struct{ log *slog.Logger }

func (m *module) Name() string { return "greet" }

// Register 只做声明：契约实现经 ProvideContract 进 Registry（裸实现进不来——
// wrapper 由 greetapi.WrapService 提供，方法体经 contract.Call 过边界），
// 路由由框架挂到根 mux。
func (m *module) Register(reg *appkit.Registry) error {
	svc := NewService()
	appkit.ProvideContract(reg,
		func(*appkit.Registry) (greetapi.Service, error) { return svc, nil },
		func(v greetapi.Service) greetapi.Service { return greetapi.WrapService(v, 0) })
	reg.MountPublic(Pattern, NewHandler(m.log, svc))
	return nil
}

// NewService 返回 greetapi.Service 的本地实现。
func NewService() greetapi.Service { return service{} }

type service struct{}

func (service) Greet(_ context.Context, req greetapi.GreetRequest) (greetapi.GreetReply, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return greetapi.GreetReply{}, apperr.InvalidArgument("name 不能为空")
	}
	switch req.Lang {
	case "", "en":
		return greetapi.GreetReply{Message: fmt.Sprintf("Hello, %s!", name)}, nil
	case "zh":
		return greetapi.GreetReply{Message: fmt.Sprintf("你好，%s！", name)}, nil
	default:
		// 错误 sentinel 由 codes.yaml 生成：错误身份 = 错误码，
		// WithDetail 返回副本，共享模板安全。
		return greetapi.GreetReply{}, greetapi.ErrGreetUnsupportedLang.WithDetail("lang", req.Lang)
	}
}

// NewHandler 返回 /greet 处理器，对应真实域 repo 的 internal/http：
// 只做解码与 DTO 映射，业务错误原样上抛，统一经 httpserver.WriteError
// 出口映射为 RFC 9457——整条路径只在出口 log 一次。
func NewHandler(log *slog.Logger, svc greetapi.Service) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply, err := svc.Greet(r.Context(), greetapi.GreetRequest{
			Name: r.PathValue("name"),
			Lang: r.URL.Query().Get("lang"),
		})
		if err != nil {
			httpserver.WriteError(log, w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	})
}

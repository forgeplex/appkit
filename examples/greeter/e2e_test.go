package main

// App 的装配面只在 Run 内部走通且测试不起真端口，因此按模块级测两条数据通路：
// greet 模块 Mount 的 handler，以及 gateway 经 contract.Call 调
// greet 本地实现 / 远程 client 的路径——这正是双模式切换真正切换的东西。

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/examples/greeter/gateway"
	"github.com/forgeplex/appkit/examples/greeter/greet"
	"github.com/forgeplex/appkit/examples/greeter/greetapi"
	"github.com/forgeplex/appkit/tx"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) apperr.Problem {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var p apperr.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("解析 problem 失败: %v（body=%s）", err, rec.Body.String())
	}
	return p
}

func TestGreetService(t *testing.T) {
	svc := greet.NewService()
	tests := []struct {
		name     string
		req      greetapi.GreetRequest
		wantMsg  string
		wantCode string
	}{
		{name: "默认英文", req: greetapi.GreetRequest{Name: "Ada"}, wantMsg: "Hello, Ada!"},
		{name: "显式en", req: greetapi.GreetRequest{Name: "Ada", Lang: "en"}, wantMsg: "Hello, Ada!"},
		{name: "中文", req: greetapi.GreetRequest{Name: "Ada", Lang: "zh"}, wantMsg: "你好，Ada！"},
		{name: "空白名字", req: greetapi.GreetRequest{Name: "  "}, wantCode: apperr.CodeInvalidArgument},
		{name: "不支持的语言", req: greetapi.GreetRequest{Name: "Ada", Lang: "fr"}, wantCode: greetapi.CodeUnsupportedLang},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reply, err := svc.Greet(context.Background(), tt.req)
			if tt.wantCode != "" {
				if !apperr.Is(err, tt.wantCode) {
					t.Fatalf("err = %v, want code %s", err, tt.wantCode)
				}
				if _, ok := errors.AsType[*apperr.Error](err); !ok {
					t.Fatalf("对外错误类型 = %T, want *apperr.Error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Greet() 失败: %v", err)
			}
			if reply.Message != tt.wantMsg {
				t.Fatalf("Message = %q, want %q", reply.Message, tt.wantMsg)
			}
		})
	}
}

// TestGreetHandler 用与 App 根 mux 相同的 pattern 挂载 greet 的 handler，
// 覆盖成功路径与 apperr → problem+json 出口。
func TestGreetHandler(t *testing.T) {
	tests := []struct {
		name       string
		url        string
		wantStatus int
		wantMsg    string
		wantCode   string
	}{
		{name: "正常问候", url: "/greet/Ada", wantStatus: http.StatusOK, wantMsg: "Hello, Ada!"},
		{name: "lang查询参数", url: "/greet/Ada?lang=zh", wantStatus: http.StatusOK, wantMsg: "你好，Ada！"},
		{name: "空白名字422", url: "/greet/%20", wantStatus: http.StatusUnprocessableEntity,
			wantCode: apperr.CodeInvalidArgument},
		{name: "不支持语言422", url: "/greet/Ada?lang=fr", wantStatus: http.StatusUnprocessableEntity,
			wantCode: greetapi.CodeUnsupportedLang},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.Handle(greet.Pattern, greet.NewHandler(discard(), greet.NewService()))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.url, nil))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d（body=%s）", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantCode != "" {
				if p := decodeProblem(t, rec); p.Code != tt.wantCode {
					t.Fatalf("problem code = %q, want %q", p.Code, tt.wantCode)
				}
				return
			}
			var reply greetapi.GreetReply
			if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
				t.Fatalf("解析响应失败: %v（body=%s）", err, rec.Body.String())
			}
			if reply.Message != tt.wantMsg {
				t.Fatalf("Message = %q, want %q", reply.Message, tt.wantMsg)
			}
		})
	}
}

// TestGatewayHandler 覆盖契约边界语义：本地实现与远程 client 走同一接口、
// 业务错误码穿过边界不变、事务内调用被运行时守卫拦截。
func TestGatewayHandler(t *testing.T) {
	tests := []struct {
		name       string
		svc        greetapi.Service
		url        string
		inTx       bool
		wantStatus int
		wantMsg    string
		wantCode   string
	}{
		{name: "本地实现经契约边界", svc: greet.NewService(),
			url: "/hello/Ada?lang=zh", wantStatus: http.StatusOK, wantMsg: "你好，Ada！"},
		{name: "业务错误码跨契约边界不变", svc: greet.NewService(),
			url: "/hello/Ada?lang=fr", wantStatus: http.StatusUnprocessableEntity,
			wantCode: greetapi.CodeUnsupportedLang},
		{name: "事务内调用被运行时守卫拦截", svc: greet.NewService(),
			url: "/hello/Ada", inTx: true, wantStatus: http.StatusInternalServerError,
			wantCode: apperr.CodeTxBoundary},
		{name: "远程client同一接口兜底", svc: remoteClient{},
			url: "/hello/Ada", wantStatus: http.StatusOK, wantMsg: "Hello, Ada! (via remote greet)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.Handle(gateway.Pattern, gateway.NewHandler(discard(), tt.svc))
			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			if tt.inTx {
				// 模拟 pgtx 把事务句柄藏进 ctx 后发起契约调用的场景。
				req = req.WithContext(tx.With(req.Context(), struct{}{}))
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d（body=%s）", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantCode != "" {
				if p := decodeProblem(t, rec); p.Code != tt.wantCode {
					t.Fatalf("problem code = %q, want %q", p.Code, tt.wantCode)
				}
				return
			}
			var hr struct {
				Message string `json:"message"`
				Via     string `json:"via"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &hr); err != nil {
				t.Fatalf("解析响应失败: %v（body=%s）", err, rec.Body.String())
			}
			if hr.Message != tt.wantMsg {
				t.Fatalf("message = %q, want %q", hr.Message, tt.wantMsg)
			}
			if hr.Via != "gateway" {
				t.Fatalf("via = %q, want gateway", hr.Via)
			}
		})
	}
}

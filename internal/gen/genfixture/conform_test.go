package genfixture

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/apptest"
	"github.com/forgeplex/appkit/callctx"
)

// TestGeneratedArtifactsConform 是合约仓库生成物的终证：同一份
// contract.yaml 生成的进程内 wrapper 与远程 client（打生成的 server
// handler，真 HTTP 回环）过同一批 Conform 用例——「部署形态是启动参数」
// 在这条契约上成立。两个绑定共用同一个 stub，SeenMeta 读同一处。
func TestGeneratedArtifactsConform(t *testing.T) {
	stub := &stubService{}
	local := WrapService(stub, 0)
	srv := httptest.NewServer(NewHTTPHandler(WrapService(stub, 0)))
	t.Cleanup(srv.Close)
	remote := NewClient(srv.URL, "conform-test", nil)

	apptest.Conform(t,
		[]apptest.Binding[Service]{
			{Name: "local", Service: local, SeenMeta: func() callctx.Meta { return stub.meta }},
			{Name: "remote", Service: remote, SeenMeta: func() callctx.Meta { return stub.meta }},
		},
		[]apptest.Case[Service]{
			{Name: "问候", Idempotent: true, Want: GreetReply{Message: "hi 张三"},
				Do: func(ctx context.Context, svc Service) (any, error) {
					return svc.Greet(ctx, GreetRequest{Name: "张三"})
				}},
			{Name: "命名DTO数组响应", Idempotent: true, Want: SearchReply{Entries: []Entry{{EntryID: "e-1", Amount: "1.00"}}, NextCursor: "e|next"},
				Do: func(ctx context.Context, svc Service) (any, error) {
					return svc.Search(ctx, SearchRequest{Prefix: "e"})
				}},
			{Name: "业务错误码跨形态不变", WantCode: "GENFIXTURE_BOOM",
				Do: func(ctx context.Context, svc Service) (any, error) {
					stub.err = apperr.New("GENFIXTURE_BOOM", 400, "boom")
					defer func() { stub.err = nil }()
					return svc.Greet(ctx, GreetRequest{Name: "x"})
				}},
		})
}

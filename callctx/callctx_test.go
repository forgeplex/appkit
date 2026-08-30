package callctx_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/forgeplex/appkit/callctx"
)

func TestWithFromZeroValue(t *testing.T) {
	// 没放过值时 From 返回零值，调用方不必判空。
	if m := callctx.From(context.Background()); !m.IsZero() {
		t.Fatalf("空 ctx 应返回零值: %+v", m)
	}
	want := callctx.Meta{RequestID: "r1", TenantID: "t1", Caller: "gateway"}
	if got := callctx.From(callctx.With(context.Background(), want)); got != want {
		t.Fatalf("取回不一致: %+v", got)
	}
}

func TestMergeOnlyOverridesNonEmpty(t *testing.T) {
	ctx := callctx.With(context.Background(), callctx.Meta{RequestID: "r1", Caller: "gateway"})
	// 认证中间件后补租户，不该抹掉前面已填的字段。
	got := callctx.From(callctx.Merge(ctx, callctx.Meta{TenantID: "acme"}))
	want := callctx.Meta{RequestID: "r1", TenantID: "acme", Caller: "gateway"}
	if got != want {
		t.Fatalf("Merge 结果不符: %+v", got)
	}
	// 非空字段才覆盖。
	got = callctx.From(callctx.Merge(ctx, callctx.Meta{RequestID: "r2"}))
	if got.RequestID != "r2" || got.Caller != "gateway" {
		t.Fatalf("Merge 覆盖不符: %+v", got)
	}
}

func TestInjectExtractRoundTrip(t *testing.T) {
	want := callctx.Meta{RequestID: "r1", TenantID: "acme", Caller: "gateway"}
	h := http.Header{}
	callctx.Inject(want, h.Set)
	if got := callctx.Extract(h.Get); got != want {
		t.Fatalf("HTTP 头往返不一致: %+v", got)
	}
	// 空字段不写头：不制造 X-Tenant-Id: "" 这种下游要特判的噪声。
	h2 := http.Header{}
	callctx.Inject(callctx.Meta{RequestID: "r1"}, h2.Set)
	if len(h2) != 1 {
		t.Fatalf("只应写出一个头: %v", h2)
	}
}

func TestMapRoundTripPreservesBusinessMeta(t *testing.T) {
	want := callctx.Meta{RequestID: "r1", TenantID: "acme"}
	biz := map[string]string{"source": "csv-import"}
	got := callctx.ToMap(want, biz)
	if got["source"] != "csv-import" {
		t.Fatalf("业务自己的 meta 被覆盖了: %v", got)
	}
	if m := callctx.FromMap(got); m != want {
		t.Fatalf("事件 meta 往返不一致: %+v", m)
	}
	// 零值不该凭空造出 map。
	if callctx.ToMap(callctx.Meta{}, nil) != nil {
		t.Fatal("零值 Meta 不应新建 map")
	}
}

func TestLogAttrsSkipsEmpty(t *testing.T) {
	if n := len(callctx.Meta{}.LogAttrs()); n != 0 {
		t.Fatalf("零值不应产生日志属性: %d", n)
	}
	attrs := callctx.Meta{RequestID: "r1", TenantID: "acme"}.LogAttrs()
	if len(attrs) != 2 || attrs[0].Key != "request_id" || attrs[1].Key != "tenant_id" {
		t.Fatalf("日志属性不符: %v", attrs)
	}
}

// ---- Transport ----

// recorder 记下经过的请求并短路真实网络。
type recorder struct{ got *http.Request }

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.got = req
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

// TestTransportEndToEnd 走一遍真的 HTTP：客户端装 Transport，服务端用 Extract 读。
// 这是这个类型唯一真正要证明的事——两端对得上。单测注入端写了什么、单测提取端
// 读到什么，都可能各自自洽而合不上；只有让请求真的过一次网络才算数。
//
// 顺带覆盖两件事：Base 为 nil 时确实走 http.DefaultTransport（不然连不上
// httptest 的监听端口）；Transport.Caller 覆盖掉 ctx 里上一跳的 "gateway"。
func TestTransportEndToEnd(t *testing.T) {
	var got callctx.Meta
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = callctx.Extract(r.Header.Get)
	}))
	defer srv.Close()

	ctx := callctx.With(context.Background(), callctx.Meta{
		RequestID: "r1", TenantID: "acme", Caller: "gateway",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: callctx.Transport{Caller: "ledger"}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	want := callctx.Meta{RequestID: "r1", TenantID: "acme", Caller: "ledger"}
	if got != want {
		t.Fatalf("服务端收到的元数据不符:\n得到 %+v\n期望 %+v", got, want)
	}
}

// TestTransportDoesNotMutateRequest 守住 RoundTripper 的约定：不得改动传入的
// 请求。net/http 会重试、会在重定向链上复用同一个 *Request，就地改头意味着
// 上一次的租户 id 可能跟着下一次请求发出去——跨租户串数据，最坏的一类 bug。
func TestTransportDoesNotMutateRequest(t *testing.T) {
	rec := &recorder{}
	ctx := callctx.With(context.Background(), callctx.Meta{RequestID: "r1", TenantID: "acme"})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (callctx.Transport{Base: rec, Caller: "ledger"}).RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if v := req.Header.Get(callctx.HeaderTenantID); v != "" {
		t.Errorf("传入的请求被就地改了头: %s=%q", callctx.HeaderTenantID, v)
	}
	if v := rec.got.Header.Get(callctx.HeaderTenantID); v != "acme" {
		t.Errorf("下游没收到租户: %q", v)
	}
}

// TestTransportCallerSemantics 钉住 Caller 字段的两种语义。留空即透传上一跳，
// 这多半不是使用者想要的（文档已警告），但既然是可观测行为就得锁住——将来谁
// 想改成「留空时不写 Caller」，得先在这里看见自己在改语义。
func TestTransportCallerSemantics(t *testing.T) {
	tests := []struct {
		name       string
		transport  string // Transport.Caller
		ctxCaller  string
		wantCaller string
	}{
		{"非空则覆盖上一跳", "ledger", "gateway", "ledger"},
		{"留空则透传上一跳", "", "gateway", "gateway"},
		{"两处都空则不写", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			ctx := callctx.With(context.Background(), callctx.Meta{
				RequestID: "r1", Caller: tt.ctxCaller,
			})
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.invalid", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (callctx.Transport{Base: rec, Caller: tt.transport}).RoundTrip(req); err != nil {
				t.Fatal(err)
			}
			if got := rec.got.Header.Get(callctx.HeaderCaller); got != tt.wantCaller {
				t.Errorf("Caller = %q, want %q", got, tt.wantCaller)
			}
		})
	}
}

// TestTransportZeroMetaAddsNoHeaders：ctx 里没有元数据时不写任何头，不制造
// X-Tenant-Id: "" 这种下游要特判的噪声（与 Inject 的行为一致）。
func TestTransportZeroMetaAddsNoHeaders(t *testing.T) {
	rec := &recorder{}
	req, err := http.NewRequest(http.MethodGet, "http://example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (callctx.Transport{Base: rec}).RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{callctx.HeaderRequestID, callctx.HeaderTenantID, callctx.HeaderCaller} {
		if v := rec.got.Header.Get(h); v != "" {
			t.Errorf("不该写出 %s，实际 %q", h, v)
		}
	}
}

// TestTransportNilHeader：手工构造的 *http.Request 可能 Header 为 nil，
// 往 nil map 写会 panic。这条不是理论问题——它只在某个手写 client 恰好这么
// 构造请求时炸，而那时栈顶是框架代码。
func TestTransportNilHeader(t *testing.T) {
	rec := &recorder{}
	ctx := callctx.With(context.Background(), callctx.Meta{RequestID: "r1"})
	req := (&http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Scheme: "http", Host: "example.invalid"},
	}).WithContext(ctx)
	req.Header = nil
	if _, err := (callctx.Transport{Base: rec}).RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if got := rec.got.Header.Get(callctx.HeaderRequestID); got != "r1" {
		t.Errorf("RequestID = %q, want r1", got)
	}
}

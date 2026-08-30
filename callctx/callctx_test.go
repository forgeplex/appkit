package callctx_test

import (
	"context"
	"net/http"
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

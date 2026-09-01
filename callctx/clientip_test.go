package callctx_test

import (
	"context"
	"net/netip"
	"testing"

	"github.com/forgeplex/appkit/callctx"
)

func TestClientIPRoundtrip(t *testing.T) {
	ctx := context.Background()
	if ip := callctx.ClientIP(ctx); ip.IsValid() {
		t.Fatalf("无值时应返回零值，得到 %v", ip)
	}
	want := netip.MustParseAddr("203.0.113.7")
	got := callctx.ClientIP(callctx.WithClientIP(ctx, want))
	if got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// ClientIP 不属于 Meta 白名单：防火墙语义下 ctx 值被剥掉、Meta 保留，
// 这里锁住「ClientIP 与 Meta 是两套存储」——进白名单是显式决策，
// 不是顺手的事。
func TestClientIPNotInMeta(t *testing.T) {
	ip := netip.MustParseAddr("203.0.113.7")
	ctx := callctx.WithClientIP(context.Background(), ip)
	if m := callctx.From(ctx); !m.IsZero() {
		t.Fatalf("写 ClientIP 不应影响 Meta，得到 %+v", m)
	}
	if attrs := callctx.From(ctx).LogAttrs(); len(attrs) != 0 {
		t.Fatalf("Meta 日志字段不应出现 client ip：%v", attrs)
	}
}

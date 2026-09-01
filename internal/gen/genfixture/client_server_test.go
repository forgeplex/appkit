package genfixture

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/callctx"
	"github.com/forgeplex/appkit/tx"
)

// remoteStub 是「真实部署形态」的对拍装配：生成的 server handler 里包着
// 生成的 wrapper（边界内侧再经一次 contract.Call），client 走真 HTTP。
func remoteStub(t *testing.T, stub *stubService) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(NewHTTPHandler(WrapService(stub, 0)))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "genfixture-test", nil), srv
}

// TestClientServerRoundTrip 生成的 client 与生成的 server 经真 HTTP 对拍：
// 值、数组响应与错误码在两端一致。
func TestClientServerRoundTrip(t *testing.T) {
	stub := &stubService{}
	client, _ := remoteStub(t, stub)
	ctx := context.Background()

	reply, err := client.Greet(ctx, GreetRequest{Name: "张三"})
	if err != nil || reply.Message != "hi 张三" {
		t.Errorf("Greet = (%+v, %v)，期望 (hi 张三, nil)", reply, err)
	}
	stats, err := client.Stats(ctx)
	if err != nil || stats.Served != 7 {
		t.Errorf("Stats = (%+v, %v)，期望 (7, nil)", stats, err)
	}
	if err := client.Reset(ctx, ResetRequest{Scope: "all"}); err != nil {
		t.Errorf("Reset: %v", err)
	}
	if err := client.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
	search, err := client.Search(ctx, SearchRequest{Prefix: "e"})
	if err != nil || len(search.Entries) != 1 || search.Entries[0].Amount != "1.00" || search.NextCursor != "e|next" {
		t.Errorf("Search = (%+v, %v)", search, err)
	}
}

// TestClientServerErrorCodes 错误身份跨网络不变：apperr 保码，
// 裸错误折叠 INTERNAL——与进程内 wrapper 的规范化规则同源。
func TestClientServerErrorCodes(t *testing.T) {
	cases := []struct {
		name     string
		inner    error
		wantCode string
	}{
		{"apperr 保码", apperr.New("GENFIXTURE_BOOM", 400, "boom"), "GENFIXTURE_BOOM"},
		{"裸错误折叠", errors.New("raw failure"), apperr.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := remoteStub(t, &stubService{err: tc.inner})
			for _, c := range wrapCalls(client) {
				if err := c.call(context.Background()); !apperr.Is(err, tc.wantCode) {
					t.Errorf("%s: 期望错误码 %s，得到 %v", c.name, tc.wantCode, err)
				}
			}
		})
	}
}

// TestClientTxBoundary 事务内调用在 client 侧就被 contract.Call 挡下，
// 请求根本不会发出去。
func TestClientTxBoundary(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, "genfixture-test", nil)

	err := client.Ping(tx.With(context.Background(), "fake-tx"))
	if !apperr.Is(err, apperr.CodeTxBoundary) {
		t.Fatalf("期望 CodeTxBoundary，得到 %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("事务内调用不应发出 HTTP 请求，实际命中 %d 次", hits.Load())
	}
}

// TestClientMetaPropagation 白名单经生成 client 自动穿过 HTTP 边界——
// NewClient 把 callctx.Transport 焊死在装配处，不存在「忘装」的形态。
func TestClientMetaPropagation(t *testing.T) {
	stub := &stubService{}
	client, _ := remoteStub(t, stub)

	want := callctx.Meta{RequestID: "req-1", TenantID: "t-1"}
	ctx := callctx.With(context.Background(), want)
	if _, err := client.Greet(ctx, GreetRequest{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	got := stub.meta
	if got.RequestID != want.RequestID || got.TenantID != want.TenantID {
		t.Errorf("实现侧看到 %+v，期望 request/tenant 穿透 %+v", got, want)
	}
	// Caller 语义是「谁调的我」：client 填自己的名字覆盖 ctx 里的值。
	if got.Caller != "genfixture-test" {
		t.Errorf("Caller = %q，期望 genfixture-test", got.Caller)
	}
}

// flakyGateway 前 failTimes 次以 503 拒绝，之后透传给真 handler——
// 模拟对端可用性故障后恢复。
func flakyGateway(failTimes int64, inner http.Handler, hits *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= failTimes {
			apperr.WriteProblem(w, apperr.Unavailable(errors.New("down")))
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// TestClientRetriesIdempotent 幂等方法对 CodeUnavailable 做有界重试：
// 两次 503 后第三次成功，调用方无感。
func TestClientRetriesIdempotent(t *testing.T) {
	var hits atomic.Int64
	stub := &stubService{}
	srv := httptest.NewServer(flakyGateway(2, NewHTTPHandler(WrapService(stub, 0)), &hits))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, "genfixture-test", nil)

	reply, err := client.Greet(context.Background(), GreetRequest{Name: "x"})
	if err != nil || reply.Message != "hi x" {
		t.Fatalf("重试后应成功，得到 (%+v, %v)", reply, err)
	}
	if hits.Load() != 3 {
		t.Fatalf("期望 3 次尝试（1+2 重试），实际 %d", hits.Load())
	}
}

// TestClientNoRetryNonIdempotent 非幂等方法不重试——重复执行不安全，
// 失败后交给调用方决策。
func TestClientNoRetryNonIdempotent(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(flakyGateway(1<<30, http.NotFoundHandler(), &hits))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, "genfixture-test", nil)

	err := client.Reset(context.Background(), ResetRequest{Scope: "all"})
	if !apperr.Is(err, apperr.CodeUnavailable) {
		t.Fatalf("期望 CodeUnavailable，得到 %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("非幂等方法不应重试，实际命中 %d 次", hits.Load())
	}
}

// TestClientRetryExhausted 重试封顶：持续不可用时报最后一次的
// CodeUnavailable，总尝试次数 = retryMaxAttempts。
func TestClientRetryExhausted(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(flakyGateway(1<<30, http.NotFoundHandler(), &hits))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, "genfixture-test", nil)

	start := time.Now()
	err := client.Ping(context.Background())
	if !apperr.Is(err, apperr.CodeUnavailable) {
		t.Fatalf("期望 CodeUnavailable，得到 %v", err)
	}
	if hits.Load() != retryMaxAttempts {
		t.Fatalf("期望 %d 次尝试，实际 %d", retryMaxAttempts, hits.Load())
	}
	// 间隔固定 100ms/200ms：总耗时下界可断言（上界不断言，CI 机器可能慢）。
	if elapsed := time.Since(start); elapsed < retryBackoff {
		t.Fatalf("重试间隔未生效：%v 内完成了 %d 次尝试", elapsed, hits.Load())
	}
}

// TestClientRetryAbortsOnCancel 重试等待期间 ctx 取消立即返回，
// 不空转剩余退避。
func TestClientRetryAbortsOnCancel(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(flakyGateway(1<<30, http.NotFoundHandler(), &hits))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, "genfixture-test", nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := client.Ping(ctx)
	if !apperr.Is(err, apperr.CodeUnavailable) {
		t.Fatalf("期望 CodeUnavailable，得到 %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("ctx 已取消的调用不应发出请求（contract.Call 挡下），实际 %d 次", hits.Load())
	}
}

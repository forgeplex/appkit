package apptest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/callctx"
	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/httpserver"
)

// —— 被测契约：顶替合约仓库生成物的位置（接口 + DTO + 错误码）——

type echoReq struct {
	Text string `json:"text"`
}

type echoReply struct {
	Echo string `json:"echo"`
}

const codeEmpty = "ECHO_EMPTY"

type echoService interface {
	Echo(ctx context.Context, req echoReq) (echoReply, error)
}

// localEcho 是域内实现：只认领域类型，不知道自己会被怎么绑定。
type localEcho struct{}

func (localEcho) Echo(_ context.Context, req echoReq) (echoReply, error) {
	if strings.TrimSpace(req.Text) == "" {
		return echoReply{}, apperr.New(codeEmpty, http.StatusUnprocessableEntity, "text 不能为空")
	}
	return echoReply{Echo: req.Text}, nil
}

// wrappedEcho 是 `appkit gen wrap` 生成物的手写等价物（进程内绑定）。
type wrappedEcho struct{ inner echoService }

func (w wrappedEcho) Echo(ctx context.Context, req echoReq) (echoReply, error) {
	return contract.Call(ctx, "echo", "Echo", 0, func(ctx context.Context) (echoReply, error) {
		return w.inner.Echo(ctx, req)
	})
}

// metaSpy 包住域内实现，记下每次调用时**服务端**看到的白名单。两个绑定共用
// 同一个 spy，SeenMeta 就都读同一处——这正是要比对的「同一个实现，两种到达方式」。
// 它同时数调用次数：Conform 额外发的真实调用有没有超出授权，只有从实现这一侧
// 才数得准（被边界守卫挡回的那些压根到不了这里）。
type metaSpy struct {
	inner echoService
	mu    sync.Mutex
	last  callctx.Meta
	calls int
}

func (s *metaSpy) Echo(ctx context.Context, req echoReq) (echoReply, error) {
	s.mu.Lock()
	s.last = callctx.From(ctx)
	s.calls++
	s.mu.Unlock()
	return s.inner.Echo(ctx, req)
}

func (s *metaSpy) Last() callctx.Meta {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// Calls 返回真正到达实现的调用次数。
func (s *metaSpy) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// echoHandler 是提供方进程的 HTTP 出口：错误经 problem+json 出去。
// 外面套真的 httpserver.RequestID 中间件（见 newEchoServer），入站这半条路
// 用的就是框架自己的实现，不是手写等价物。
func echoHandler(svc echoService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req echoReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			apperr.WriteProblem(w, apperr.InvalidArgument("请求体不是合法 JSON"))
			return
		}
		reply, err := svc.Echo(r.Context(), req)
		if err != nil {
			apperr.WriteProblem(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	})
}

// remoteEcho 是合约仓库生成的 HTTP client 的手写等价物（远程绑定）：
// 同样经 contract.Call，所以边界语义与进程内绑定同源。
//
// client 可换是有意的：装了 callctx.Transport 的是接对了的写法，裸
// http.DefaultClient 就是漏了白名单的那种——两者都要有，否则证不出检查会红。
type remoteEcho struct {
	base   string
	client *http.Client
}

func (c remoteEcho) Echo(ctx context.Context, req echoReq) (echoReply, error) {
	return contract.Call(ctx, "echo", "Echo", 0, func(ctx context.Context) (echoReply, error) {
		body, err := json.Marshal(req)
		if err != nil {
			return echoReply{}, apperr.Internal(err)
		}
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/echo", bytes.NewReader(body))
		if err != nil {
			return echoReply{}, apperr.Internal(err)
		}
		hreq.Header.Set("Content-Type", "application/json")
		resp, err := c.client.Do(hreq)
		if err != nil {
			return echoReply{}, apperr.Unavailable(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return echoReply{}, apperr.Unavailable(err)
		}
		if resp.StatusCode != http.StatusOK {
			return echoReply{}, apperr.FromProblem(resp.StatusCode, raw)
		}
		var reply echoReply
		if err := json.Unmarshal(raw, &reply); err != nil {
			return echoReply{}, apperr.Internal(err)
		}
		return reply, nil
	})
}

func echoCall(text string) func(context.Context, echoService) (any, error) {
	return func(ctx context.Context, svc echoService) (any, error) {
		return svc.Echo(ctx, echoReq{Text: text})
	}
}

// newEchoServer 起一个提供方进程：真的 httpserver.RequestID 中间件负责入站
// 那半条路（从请求头 Extract 进 ctx），spy 记下实现最终看到了什么。
func newEchoServer(t *testing.T, spy *metaSpy) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(httpserver.RequestID()(echoHandler(spy)))
	t.Cleanup(srv.Close)
	return srv
}

// TestConformLocalAndRemote 是本包的主证明：进程内 wrapper 与真跑 HTTP 的
// client 拿同一批用例过一遍，全绿即"部署形态是启动参数"这句话在这条契约上成立。
//
// 两个绑定共用同一个 spy，SeenMeta 都读它——于是这条测试连带证明了 callctx
// 白名单在两种到达方式下都到得了实现，而不只是错误码和返回值对得上。
func TestConformLocalAndRemote(t *testing.T) {
	t.Parallel()
	spy := &metaSpy{inner: localEcho{}}
	srv := newEchoServer(t, spy)
	seen := func() callctx.Meta { return spy.Last() }

	Conform(t,
		[]Binding[echoService]{
			{Name: "local", Service: wrappedEcho{inner: spy}, SeenMeta: seen},
			{Name: "remote", SeenMeta: seen, Service: remoteEcho{
				base: srv.URL,
				// 接对了的写法：装配一次，此后每个请求自动带上白名单。
				client: &http.Client{Transport: callctx.Transport{Caller: "apptest-remote"}},
			}},
		},
		[]Case[echoService]{
			{Name: "回显", Do: echoCall("hi"), Want: echoReply{Echo: "hi"}, Idempotent: true},
			{Name: "空白文本是领域错误", Do: echoCall("  "), WantCode: codeEmpty},
		})
}

// TestSeenMetaCatchesUninstrumentedClient 是这项检查的非空证明：换成裸
// http.DefaultClient（＝忘了装 Transport 的手写 client），服务端就什么也
// 看不到，metaDiff 必须报出来。
//
// 不走 Conform 而是直接调用+比对，是因为"期望某个 t 会失败"没法用 *testing.T
// 表达；本文件下半段的反向用例一律用这个办法——把判定抽成纯函数再直接喂它。
func TestSeenMetaCatchesUninstrumentedClient(t *testing.T) {
	t.Parallel()
	spy := &metaSpy{inner: localEcho{}}
	srv := newEchoServer(t, spy)

	ctx := callctx.With(t.Context(), probeMeta)
	client := remoteEcho{base: srv.URL, client: http.DefaultClient}
	if _, err := client.Echo(ctx, echoReq{Text: "hi"}); err != nil {
		t.Fatalf("调用本身应当成功（漏白名单不影响业务返回，这正是它安静的原因）: %v", err)
	}
	// 请求成功、返回值正确、错误码正确——Conform 原有的五项全绿，只有这一项会红。
	if msg := metaDiff("remote", probeMeta, spy.Last()); msg == "" {
		t.Error("裸 client 没带白名单，metaDiff 却认为一切正常——这项检查是空的")
	}
}

// TestMetaPropagationNeedsIdempotentGrant 守住「Conform 不替你多跑有副作用的调用」。
//
// 元数据传播检查要额外发一次真实请求。首版对所有期望成功的用例都发，于是一条
// 「创建订单」用例填上 SeenMeta 之后就会真的创建两单——测试框架擅自制造副作用，
// 比不做这项检查危险得多。现在它要 Case.Idempotent 授权，而 Idempotent 的语义
// 恰好就是「重复执行是安全的」。
//
// 直接调 checkMetaPropagation 而不走 Conform：要数的是**额外**调用，从实现那侧
// 数才准，而且不必去猜 Conform 其余各项各自发了几次。
func TestMetaPropagationNeedsIdempotentGrant(t *testing.T) {
	t.Parallel()
	spy := &metaSpy{inner: localEcho{}}
	bindings := []Binding[echoService]{
		{Name: "local", Service: wrappedEcho{inner: spy}, SeenMeta: spy.Last},
		{Name: "remote", Service: wrappedEcho{inner: spy}, SeenMeta: spy.Last},
	}

	t.Run("非幂等用例不跑，且一次额外调用都不发", func(t *testing.T) {
		before := spy.Calls()
		if ran := checkMetaPropagation(t, bindings, Case[echoService]{
			Name: "创建订单", Do: echoCall("hi")}); ran {
			t.Error("没声明 Idempotent 却跑了传播检查——那会替使用者多创建一单")
		}
		if n := spy.Calls() - before; n != 0 {
			t.Errorf("非幂等用例上多发了 %d 次真实调用，want 0", n)
		}
	})

	t.Run("幂等用例才跑，每个绑定各一次", func(t *testing.T) {
		before := spy.Calls()
		if ran := checkMetaPropagation(t, bindings, Case[echoService]{
			Name: "回显", Do: echoCall("hi"), Idempotent: true}); !ran {
			t.Fatal("声明了 Idempotent 的成功用例必须跑传播检查，否则这项配置形同虚设")
		}
		if n, want := spy.Calls()-before, len(bindings); n != want {
			t.Errorf("发了 %d 次真实调用，want %d（每个绑定各一次）", n, want)
		}
	})

	t.Run("期望失败的用例不跑", func(t *testing.T) {
		// 失败可能发生在到达实现之前，spy 什么也没记到，比出来是假阳性。
		if ran := checkMetaPropagation(t, bindings, Case[echoService]{
			Name: "空白文本", Do: echoCall("  "), WantCode: codeEmpty, Idempotent: true}); ran {
			t.Error("期望失败的用例不该跑传播检查")
		}
	})
}

// —— 以下是"这套断言真的会红"的证明。断言库最危险的失效方式是永远绿，
// 所以每条判定都要有反向用例。——

func TestObserve(t *testing.T) {
	t.Parallel()
	t.Run("成功", func(t *testing.T) {
		r := observe("local", echoReply{Echo: "hi"}, nil)
		if r.code != "" || r.fail != "" {
			t.Fatalf("code=%q fail=%q, want 全空", r.code, r.fail)
		}
	})
	t.Run("apperr 取码", func(t *testing.T) {
		if r := observe("local", nil, apperr.New(codeEmpty, 422, "x")); r.code != codeEmpty {
			t.Fatalf("code = %q, want %q", r.code, codeEmpty)
		}
	})
	t.Run("非 apperr 直接判失败", func(t *testing.T) {
		r := observe("remote", nil, errors.New("裸错误"))
		if r.fail == "" {
			t.Fatal("裸 error 穿过契约边界必须被判失败，否则错误规范化形同虚设")
		}
		if !strings.Contains(r.fail, "contract.Call") {
			t.Errorf("失败信息该指向根因（漏了 contract.Call），got: %s", r.fail)
		}
	})
}

func TestResultCheck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		r        result
		wantCode string
		want     any
		wantFail bool
	}{
		{name: "期望成功且成功", r: result{val: echoReply{Echo: "hi"}}},
		{name: "期望成功却失败", r: result{code: codeEmpty}, wantFail: true},
		{name: "期望失败且码对", r: result{code: codeEmpty}, wantCode: codeEmpty},
		{name: "期望失败却成功", r: result{val: echoReply{}}, wantCode: codeEmpty, wantFail: true},
		{name: "期望失败但码不对", r: result{code: apperr.CodeInternal}, wantCode: codeEmpty, wantFail: true},
		{name: "Want 相等", r: result{val: echoReply{Echo: "hi"}}, want: echoReply{Echo: "hi"}},
		{name: "Want 不等", r: result{val: echoReply{Echo: "ho"}}, want: echoReply{Echo: "hi"}, wantFail: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := tt.r.check("local", tt.wantCode, tt.want)
			if (msg != "") != tt.wantFail {
				t.Fatalf("check() = %q, wantFail = %v", msg, tt.wantFail)
			}
		})
	}
}

func TestCrossDiff(t *testing.T) {
	t.Parallel()
	names := []string{"local", "remote"}
	tests := []struct {
		name    string
		results []result
		wantN   int
	}{
		{name: "两边都成功且返回值相同", wantN: 0, results: []result{
			{val: echoReply{Echo: "hi"}}, {val: echoReply{Echo: "hi"}}}},
		{name: "两边同码失败", wantN: 0, results: []result{
			{code: codeEmpty}, {code: codeEmpty}}},
		// 最典型的真实事故：提供方改了 json key，消费方没跟上，
		// 远程往返后字段静默变空值——进程内那条路径完全看不出来。
		{name: "JSON 往返丢字段", wantN: 1, results: []result{
			{val: echoReply{Echo: "hi"}}, {val: echoReply{}}}},
		{name: "一边成功一边失败", wantN: 1, results: []result{
			{val: echoReply{Echo: "hi"}}, {code: apperr.CodeInternal}}},
		{name: "错误码不同", wantN: 1, results: []result{
			{code: codeEmpty}, {code: apperr.CodeInternal}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if msgs := crossDiff(names, tt.results); len(msgs) != tt.wantN {
				t.Fatalf("crossDiff() 报了 %d 条，want %d：%v", len(msgs), tt.wantN, msgs)
			}
		})
	}
}

func TestWantBoundaryCode(t *testing.T) {
	t.Parallel()
	if msg := wantBoundaryCode("local", apperr.CodeTxBoundary,
		apperr.New(apperr.CodeTxBoundary, 500, "x"), "why"); msg != "" {
		t.Fatalf("码相符不该报错，got %q", msg)
	}
	if msg := wantBoundaryCode("local", apperr.CodeTxBoundary, nil, "why"); msg == "" {
		t.Fatal("调用成功了却期望被边界拒绝，必须报错")
	}
	if msg := wantBoundaryCode("local", apperr.CodeTxBoundary,
		apperr.New(apperr.CodeInternal, 500, "x"), "why"); msg == "" {
		t.Fatal("错误码不符必须报错")
	}
}

func TestMetaDiff(t *testing.T) {
	t.Parallel()
	want := probeMeta
	tests := []struct {
		name   string
		got    callctx.Meta
		wantOK bool
	}{
		{"全部到齐", want, true},
		// 最要紧的一条：出站 client 把 Caller 改写成自己的服务名是**正确**行为
		// （callctx.Transport 的 Caller 字段就是干这个的）。拿它当断言会把接对了
		// 的 client 判红，而一条会误报的规则很快就没人看了。
		{"Caller 被改写仍算通过", callctx.Meta{
			RequestID: want.RequestID, TenantID: want.TenantID, Caller: "ledger"}, true},
		{"Caller 为空仍算通过", callctx.Meta{
			RequestID: want.RequestID, TenantID: want.TenantID}, true},
		// 漏装 Transport 的典型症状：什么都没到。
		{"什么都没传过来", callctx.Meta{}, false},
		// 只接了一半的更隐蔽：日志串得起来，租户却丢了——下游选错数据边界。
		{"只有 request id，租户丢了", callctx.Meta{RequestID: want.RequestID}, false},
		{"只有租户，request id 丢了", callctx.Meta{TenantID: want.TenantID}, false},
		{"传成了别的值", callctx.Meta{
			RequestID: want.RequestID, TenantID: "别人的租户"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if msg := metaDiff("remote", want, tt.got); (msg == "") != tt.wantOK {
				t.Errorf("metaDiff() = %q，期望通过 = %v", msg, tt.wantOK)
			}
		})
	}
}

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
	"testing"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/contract"
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

// echoHandler 是提供方进程的 HTTP 出口：错误经 problem+json 出去。
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
type remoteEcho struct{ base string }

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
		resp, err := http.DefaultClient.Do(hreq)
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

// TestConformLocalAndRemote 是本包的主证明：进程内 wrapper 与真跑 HTTP 的
// client 拿同一批用例过一遍，全绿即"部署形态是启动参数"这句话在这条契约上成立。
func TestConformLocalAndRemote(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(echoHandler(localEcho{}))
	t.Cleanup(srv.Close)

	Conform(t,
		[]Binding[echoService]{
			{Name: "local", Service: wrappedEcho{inner: localEcho{}}},
			{Name: "remote", Service: remoteEcho{base: srv.URL}},
		},
		[]Case[echoService]{
			{Name: "回显", Do: echoCall("hi"), Want: echoReply{Echo: "hi"}, Idempotent: true},
			{Name: "空白文本是领域错误", Do: echoCall("  "), WantCode: codeEmpty},
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

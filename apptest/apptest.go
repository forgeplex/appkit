// Package apptest 把「单体与微服务语义一致」从设计承诺变成可运行的断言。
//
// 框架保证契约边界的四件套（事务守卫 / ctx 防火墙 / 独立超时 / 错误规范化）
// 在进程内绑定与远程 client 上表现一致，但**保证是保证，实现是实现**：
// 手写的 client 忘了走 contract.Call、两侧 DTO 的 json key 对不上、领域错误
// 在 problem+json 往返后换了码——这些都会让"部署形态是启动参数"这句话失效，
// 而且只在真正拆分部署的那天才暴露。
//
// Conform 让同一批用例分别跑过每个绑定，逐项比对可观测行为：
//
//	apptest.Conform(t,
//	    []apptest.Binding[greetv1.Service]{
//	        {Name: "local", Service: greetv1.WrapService(greet.NewService(), 0)},
//	        {Name: "remote", Service: greetv1.NewClient(srv.URL)},
//	    },
//	    []apptest.Case[greetv1.Service]{
//	        {Name: "正常问候", Do: greetCall("Ada", "zh"),
//	            Want: greetv1.GreetReply{Message: "你好，Ada！"}, Idempotent: true},
//	        {Name: "不支持的语言", Do: greetCall("Ada", "fr"),
//	            WantCode: greetv1.CodeUnsupportedLang},
//	    })
//
// 测试专用包：只在 _test.go 里 import。
package apptest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/tx"
)

// Binding 是被测契约接口的一种绑定形态。Name 只用于测试输出，
// 习惯上是 "local" 与 "remote"。
type Binding[S any] struct {
	Name    string
	Service S
}

// Case 是一条契约用例：一次调用加上对结果的期望。
//
// Do 必须只做一次契约调用并原样返回其结果——Conform 会用不同的 ctx 反复
// 调用它来检验边界语义，Do 里夹带别的逻辑会让失败信息指错地方。
type Case[S any] struct {
	Name string
	Do   func(ctx context.Context, svc S) (any, error)

	// WantCode 非空表示期望失败，且 apperr 错误码等于它；空则期望成功。
	WantCode string

	// Want 非 nil 时断言每个绑定的返回值与它 reflect.DeepEqual。
	// 留空则只做绑定之间的相互比对（跨形态一致即可）。
	Want any

	// Idempotent 声明这个调用重复执行应当得到相同结果。置 true 后
	// Conform 会连调两次并比对——重试与 at-least-once 投递下这是前提。
	Idempotent bool
}

// Conform 用 cases 逐条比对 bindings 的可观测行为。每条用例断言：
//
//  1. 成功/失败与错误码符合 Case 的期望，且失败时错误是 *apperr.Error；
//  2. 返回值在各绑定之间 DeepEqual——JSON 往返丢字段在这里现形；
//  3. 事务内发起调用被拒（CodeTxBoundary），每个绑定都要拒；
//  4. ctx 已取消时调用直接失败（CodeUnavailable），不落到实现上；
//  5. Idempotent 用例连调两次结果一致。
//
// 少于两个绑定会直接失败：只跑一个形态的不叫一致性测试。
func Conform[S any](t *testing.T, bindings []Binding[S], cases []Case[S]) {
	t.Helper()
	if len(bindings) < 2 {
		t.Fatalf("Conform 需要至少两个绑定（如 local + remote），got %d——"+
			"只跑一个形态证明不了两种形态一致", len(bindings))
	}
	if len(cases) == 0 {
		t.Fatal("Conform 需要至少一条用例")
	}
	names := make([]string, len(bindings))
	for i, b := range bindings {
		if b.Name == "" {
			t.Fatal("Binding.Name 不能为空（失败信息全靠它区分形态）")
		}
		names[i] = b.Name
	}

	for _, c := range cases {
		if c.Do == nil {
			t.Fatalf("用例 %q 没有 Do", c.Name)
		}
		t.Run(c.Name, func(t *testing.T) {
			results := make([]result, len(bindings))
			for i, b := range bindings {
				results[i] = invoke(t, b, c)
			}
			for _, msg := range crossDiff(names, results) {
				t.Error(msg)
			}
			checkTxGuard(t, bindings, c)
			checkCanceled(t, bindings, c)
			checkIdempotent(t, bindings, c)
		})
	}
}

// result 是一次调用的可观测结果：错误码（成功为空）与返回值。
// 失败原因 fail 非空表示这次调用连"可观测结果"都算不上（如错误类型不对）。
type result struct {
	code string
	val  any
	fail string
}

// invoke 跑一个绑定并对照 Case 的期望做绝对断言。
func invoke[S any](t *testing.T, b Binding[S], c Case[S]) result {
	t.Helper()
	var got result
	t.Run(b.Name, func(t *testing.T) {
		val, err := c.Do(t.Context(), b.Service)
		got = observe(b.Name, val, err)
		if got.fail != "" {
			t.Fatal(got.fail)
		}
		if msg := got.check(b.Name, c.WantCode, c.Want); msg != "" {
			t.Fatal(msg)
		}
	})
	return got
}

// observe 把一次调用的返回折算成可观测结果，顺带守住
// 「跨契约边界的错误一律 *apperr.Error」。
func observe(binding string, val any, err error) result {
	if err == nil {
		return result{val: val}
	}
	e, ok := errors.AsType[*apperr.Error](err)
	if !ok {
		return result{fail: fmt.Sprintf(
			"[%s] 契约边界返回的错误类型 = %T, want *apperr.Error——"+
				"错误规范化没生效（client 是否漏了 contract.Call？）: %v", binding, err, err)}
	}
	return result{code: e.Code(), val: val}
}

// check 对照 Case 的期望做绝对断言，返回给人看的失败原因；符合期望时返回空串。
// 只收期望值而不收 Case，是为了不让被测接口的类型参数渗进 result 的方法签名。
func (r result) check(binding, wantCode string, want any) string {
	switch {
	case wantCode != "" && r.code == "":
		return fmt.Sprintf("[%s] 期望失败（code=%s），却成功了：%#v", binding, wantCode, r.val)
	case wantCode != "" && r.code != wantCode:
		return fmt.Sprintf("[%s] 错误码 = %q, want %q", binding, r.code, wantCode)
	case wantCode == "" && r.code != "":
		return fmt.Sprintf("[%s] 期望成功，却失败了：code=%s", binding, r.code)
	}
	if want != nil && !reflect.DeepEqual(r.val, want) {
		return fmt.Sprintf("[%s] 返回值 = %#v, want %#v", binding, r.val, want)
	}
	return ""
}

// crossDiff 做跨绑定比对：这才是一致性测试真正测的东西。
// 返回每条不一致的说明，全部一致时返回 nil。
func crossDiff(names []string, results []result) []string {
	var msgs []string
	base, baseName := results[0], names[0]
	for i := 1; i < len(results); i++ {
		got, name := results[i], names[i]
		if got.code != base.code {
			msgs = append(msgs, fmt.Sprintf(
				"错误码跨形态不一致：%s=%s，%s=%s——错误身份 = 错误码，"+
					"这里不一致意味着调用方的判定逻辑换个部署形态就失效",
				baseName, orOK(base.code), name, orOK(got.code)))
			continue
		}
		if got.code == "" && !reflect.DeepEqual(got.val, base.val) {
			msgs = append(msgs, fmt.Sprintf(
				"返回值跨形态不一致：\n  %s = %#v\n  %s = %#v\n"+
					"常见原因：两侧 DTO 的 json key 对不上、字段未导出、"+
					"时间/小数精度在序列化中被截断",
				baseName, base.val, name, got.val))
		}
	}
	return msgs
}

func orOK(code string) string {
	if code == "" {
		return "<成功>"
	}
	return code
}

// wantBoundaryCode 断言边界语义类的错误码，不符时返回给人看的原因。
func wantBoundaryCode(binding, code string, err error, why string) string {
	if apperr.Is(err, code) {
		return ""
	}
	return fmt.Sprintf("[%s] err = %v, want %s——%s", binding, err, code, why)
}

// checkTxGuard 断言事务内发起契约调用被每个绑定拒绝。远程 client 若没走
// contract.Call，这条就会红——那正是它要抓的漏网之鱼。
func checkTxGuard[S any](t *testing.T, bindings []Binding[S], c Case[S]) {
	t.Helper()
	t.Run("事务内调用被拒", func(t *testing.T) {
		// tx.With 模拟 pgtx 把事务句柄藏进 ctx 之后发起跨模块调用。
		ctx := tx.With(t.Context(), struct{}{})
		for _, b := range bindings {
			_, err := c.Do(ctx, b.Service)
			if msg := wantBoundaryCode(b.Name, apperr.CodeTxBoundary, err,
				"事务内跨模块调用必须被拒绝，一致性走事件不走共享事务"); msg != "" {
				t.Error(msg)
			}
		}
	})
}

// checkCanceled 断言 ctx 已取消时调用直接失败：跨网络时这种调用发不出去，
// 进程内不该反而"成功"。
func checkCanceled[S any](t *testing.T, bindings []Binding[S], c Case[S]) {
	t.Helper()
	t.Run("ctx已取消时不落到实现", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		for _, b := range bindings {
			_, err := c.Do(ctx, b.Service)
			if msg := wantBoundaryCode(b.Name, apperr.CodeUnavailable, err,
				"已取消的 ctx 必须在边界上挡住，否则同一段代码拆成远程调用后行为就变了"); msg != "" {
				t.Error(msg)
			}
		}
	})
}

// checkIdempotent 连调两次比对结果。只对显式声明幂等的用例做。
func checkIdempotent[S any](t *testing.T, bindings []Binding[S], c Case[S]) {
	t.Helper()
	if !c.Idempotent {
		return
	}
	t.Run("重复调用结果一致", func(t *testing.T) {
		for _, b := range bindings {
			first, err1 := c.Do(t.Context(), b.Service)
			second, err2 := c.Do(t.Context(), b.Service)
			r1, r2 := observe(b.Name, first, err1), observe(b.Name, second, err2)
			if r1.fail != "" || r2.fail != "" {
				t.Error(cmp.Or(r1.fail, r2.fail))
				continue
			}
			for _, msg := range crossDiff(
				[]string{b.Name + " 第一次", b.Name + " 第二次"}, []result{r1, r2}) {
				t.Errorf("%s\n声明了 Idempotent 的调用不能有副作用差异", msg)
			}
		}
	})
}

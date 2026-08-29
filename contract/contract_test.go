package contract_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/contract"
	"github.com/forgeplex/appkit/tx"
)

// 测试专用 ctx key，模拟请求作用域值。
type reqKey struct{}

func TestCallTxGuard(t *testing.T) {
	ctx := tx.With(context.Background(), "fake-tx-handle")

	called := false
	_, err := contract.Call(ctx, "ledger", "PostEntries", 0, func(context.Context) (string, error) {
		called = true
		return "ok", nil
	})

	if err == nil {
		t.Fatal("事务内契约调用应被守卫拦截")
	}
	if !apperr.Is(err, apperr.CodeTxBoundary) {
		t.Errorf("err = %v，want apperr.Is(err, CodeTxBoundary)", err)
	}
	if called {
		t.Error("守卫拦截后 fn 不应被调用")
	}
}

func TestFirewall(t *testing.T) {
	t.Run("剥离全部 ctx 值", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), reqKey{}, "request-scoped")
		ctx = tx.With(ctx, "fake-tx")

		fw := contract.Firewall(ctx)

		if fw.Value(reqKey{}) != nil {
			t.Errorf("防火墙泄漏了请求作用域值：%v", fw.Value(reqKey{}))
		}
		if tx.HasTx(fw) {
			t.Error("防火墙泄漏了事务句柄")
		}
	})

	t.Run("保留取消传播", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		fw := contract.Firewall(parent)

		select {
		case <-fw.Done():
			t.Fatal("取消前 Done 不应关闭")
		default:
		}

		cancel()
		select {
		case <-fw.Done():
		case <-time.After(time.Second):
			t.Error("父 ctx 取消后防火墙 ctx 未随之取消")
		}
	})

	t.Run("保留 deadline", func(t *testing.T) {
		deadline := time.Now().Add(time.Minute)
		parent, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()

		got, ok := contract.Firewall(parent).Deadline()
		if !ok || !got.Equal(deadline) {
			t.Errorf("Deadline() = (%v, %v), want (%v, true)", got, ok, deadline)
		}
	})
}

func TestCallTimeout(t *testing.T) {
	start := time.Now()
	_, err := contract.Call(context.Background(), "ledger", "Slow", 30*time.Millisecond,
		func(ctx context.Context) (string, error) {
			select {
			case <-time.After(5 * time.Second):
				return "late", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		})

	if err == nil {
		t.Fatal("超时调用应返回错误")
	}
	if !apperr.Is(err, apperr.CodeUnavailable) {
		t.Errorf("超时应折叠为 UNAVAILABLE，got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("错误链应保留 context.DeadlineExceeded，got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("调用未在超时预算内返回：%v", elapsed)
	}
}

func TestCallErrorNormalize(t *testing.T) {
	sentinel := errors.New("driver: broken pipe")

	tests := []struct {
		name      string
		fnErr     error
		wantCode  string
		keepCause bool
	}{
		{"普通错误折叠为 INTERNAL", sentinel, apperr.CodeInternal, true},
		{"apperr 保留原码", apperr.Conflict("version mismatch"), apperr.CodeConflict, false},
		{"业务错误码透传", apperr.New("LEDGER_INSUFFICIENT_FUNDS", 409, "余额不足"), "LEDGER_INSUFFICIENT_FUNDS", false},
		{"包裹的 apperr 识别外层码", apperr.Internal(sentinel), apperr.CodeInternal, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := contract.Call(context.Background(), "ledger", "Fail", 0,
				func(context.Context) (int, error) { return 0, tt.fnErr })

			if err == nil {
				t.Fatal("应返回错误")
			}
			var ae *apperr.Error
			if !errors.As(err, &ae) {
				t.Fatalf("返回类型应为 *apperr.Error，got %T", err)
			}
			if !apperr.Is(err, tt.wantCode) {
				t.Errorf("apperr.Is(err, %q) = false，err = %v", tt.wantCode, err)
			}
			if tt.keepCause && !errors.Is(err, sentinel) {
				t.Errorf("cause 未保留在错误链：%v", err)
			}
		})
	}
}

func TestCallSuccess(t *testing.T) {
	type result struct{ Balance int64 }

	outer := context.WithValue(context.Background(), reqKey{}, "request-scoped")
	var innerSawValue any
	got, err := contract.Call(outer, "ledger", "GetBalance", 0,
		func(ctx context.Context) (result, error) {
			innerSawValue = ctx.Value(reqKey{})
			if _, ok := ctx.Deadline(); !ok {
				t.Error("契约调用内应有超时 deadline")
			}
			return result{Balance: 42}, nil
		})

	if err != nil {
		t.Fatalf("成功路径不应返回错误：%v", err)
	}
	if got.Balance != 42 {
		t.Errorf("返回值 = %+v, want Balance=42", got)
	}
	if innerSawValue != nil {
		t.Errorf("fn 的 ctx 应经过防火墙，仍见到值：%v", innerSawValue)
	}
}

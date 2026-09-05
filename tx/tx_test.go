package tx_test

import (
	"context"
	"testing"

	"github.com/forgeplex/appkit/tx"
)

// 测试专用 ctx key，模拟请求作用域的其他值。
type otherKey struct{}

func TestWithValueHasTx(t *testing.T) {
	handle := &struct{ name string }{"fake-tx"}

	tests := []struct {
		name       string
		ctx        context.Context
		wantHasTx  bool
		wantHandle any
	}{
		{"裸 ctx 无事务", context.Background(), false, nil},
		{"With 之后有事务", tx.With(context.Background(), handle), true, handle},
		{"嵌套 With 取最内层", tx.With(tx.With(context.Background(), "outer"), handle), true, handle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tx.HasTx(tt.ctx); got != tt.wantHasTx {
				t.Errorf("HasTx = %v, want %v", got, tt.wantHasTx)
			}
			if got := tx.Value(tt.ctx); got != tt.wantHandle {
				t.Errorf("Value = %v, want %v", got, tt.wantHandle)
			}
		})
	}
}

func TestStrip(t *testing.T) {
	t.Run("无事务时原样返回", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), otherKey{}, "keep")
		if got := tx.Strip(ctx); got != ctx {
			t.Errorf("Strip 无事务 ctx 应原样返回")
		}
	})

	t.Run("剥离事务但保留其他值", func(t *testing.T) {
		base := context.WithValue(context.Background(), otherKey{}, "keep")
		withTx := tx.With(base, "fake-tx")

		stripped := tx.Strip(withTx)

		if tx.HasTx(stripped) {
			t.Error("Strip 后 HasTx 仍为 true")
		}
		if tx.Value(stripped) != nil {
			t.Errorf("Strip 后 Value = %v, want nil", tx.Value(stripped))
		}
		if got := stripped.Value(otherKey{}); got != "keep" {
			t.Errorf("Strip 丢失了其他 ctx 值：%v", got)
		}
		// 原 ctx 不受影响。
		if !tx.HasTx(withTx) {
			t.Error("Strip 不应修改原 ctx")
		}
	})

	t.Run("保留取消传播", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		stripped := tx.Strip(tx.With(parent, "fake-tx"))

		cancel()
		select {
		case <-stripped.Done():
		default:
			t.Error("父 ctx 取消后 Strip 结果未随之取消")
		}
	})
}

func TestReadAllTenantsMarker(t *testing.T) {
	if tx.ReadsAllTenants(context.Background()) {
		t.Fatal("裸 ctx 不应带读全部标记")
	}
	ctx := tx.WithReadAllTenants(context.Background())
	if !tx.ReadsAllTenants(ctx) {
		t.Fatal("WithReadAllTenants 之后应带标记")
	}
	// 标记随派生 ctx 存活（业务用例打标记后再 Do，Do 内派生的 ctx 仍见它）；
	// Strip 只剥事务句柄，不动它。
	if !tx.ReadsAllTenants(tx.Strip(tx.With(ctx, "fake-tx"))) {
		t.Fatal("Strip 不应剥掉读全部标记")
	}
}

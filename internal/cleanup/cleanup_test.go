package cleanup

import (
	"context"
	"testing"
	"time"
)

func TestContextIsCancellationSafeAndBounded(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()

	ctx, cancel := Context(parent)
	defer cancel()
	if err := ctx.Err(); err != nil {
		t.Fatalf("父 context 已取消，但 cleanup context 不应立即结束: %v", err)
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("cleanup context 必须有 deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > Timeout {
		t.Fatalf("cleanup deadline 剩余 %v，want (0, %v]", remaining, Timeout)
	}
}

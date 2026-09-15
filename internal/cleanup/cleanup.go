// Package cleanup 提供取消安全但有上限的收尾 context。
//
// 业务请求取消后，数据库回滚、租约收尾和幂等结果落库仍应尽力完成，
// 但不能用无限期的 context.WithoutCancel 把连接或 goroutine 永久拖住。
package cleanup

import (
	"context"
	"time"
)

// Timeout 是框架内部收尾操作的统一最大预算。
const Timeout = 5 * time.Second

// Context 忽略 parent 的取消/原 deadline，同时给收尾操作一个独立上限。
func Context(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), Timeout)
}

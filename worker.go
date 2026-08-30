package appkit

import (
	"context"
	"errors"
	"fmt"
)

// Worker 注册一个长驻后台任务（outbox relay、消费循环、定时器…）：框架在
// StageWorker 起 goroutine 执行 run，关停时等它退出。
//
// 这么写而不是自己起 goroutine，是因为正确的写法每次都一样、又每次都容易写错：
// 少了 OnStop 就是关停不等它、数据半路截断；OnStop 里少 select ctx 就是关停
// 预算耗尽也不放手、整个进程吊死；run 崩了没人管就是 relay 静默停摆、
// 事件只落表不外发，而探针依然绿着。这三件事都收在这里。
//
// run 必须在 ctx 取消时尽快返回。返回 nil 或 context.Canceled 视为正常退出；
// 返回其它错误视为异常，框架据此进入关停（同 HTTP 服务异常退出）。
func (r *Registry) Worker(name string, run func(ctx context.Context) error) {
	full := r.current + "/" + name
	done := make(chan error, 1)
	r.OnStart(StageWorker, func(ctx context.Context) error {
		go func() {
			err := run(ctx)
			done <- err
			// ctx 已取消 = 正常关停路径，退出由 OnStop 消费，不必惊动主循环。
			if err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
				r.reportWorkerExit(full, err)
			}
		}()
		return nil
	})
	r.OnStop(func(ctx context.Context) error {
		select {
		case err := <-done:
			if err == nil || errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("worker %q: %w", full, err)
		case <-ctx.Done():
			return fmt.Errorf("worker %q 未在关停预算内退出: %w", full, ctx.Err())
		}
	})
}

// reportWorkerExit 把 worker 的异常退出报给主循环（带缓冲、非阻塞：
// 第一个死掉的 worker 决定关停原因，后续的由 OnStop 汇总）。
func (r *Registry) reportWorkerExit(name string, err error) {
	select {
	case r.workerErr <- fmt.Errorf("appkit: worker %q 异常退出: %w", name, err):
	default:
	}
}

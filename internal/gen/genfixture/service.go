// Package genfixture 是代码生成器的可编译夹具：手写契约接口与 DTO，
// 检入由 appkit gen 生成的三个文件（service_wrap.gen.go / events.gen.go /
// codes.gen.go），保证生成物随全仓编译与测试持续验证。
//
// 生成物同时充当 golden：internal/gen 的 TestGolden 逐字节比对重新生成的
// 输出。修改本文件或 testdata 下的 yaml 后，在仓库根目录执行
//
//	go test ./internal/gen -run TestGolden -update
//
// 重写生成物。等价的 CLI 调用：
//
//	appkit gen wrap -src internal/gen/genfixture -iface Service -system greet -out internal/gen/genfixture/service_wrap.gen.go
//	appkit gen events -in internal/gen/testdata/events.yaml -out internal/gen/genfixture/events.gen.go
//	appkit gen errors -in internal/gen/testdata/codes.yaml -out internal/gen/genfixture/codes.gen.go
package genfixture

import "context"

// GreetRequest 是请求 DTO：契约边界传值、可序列化。
type GreetRequest struct {
	Name string `json:"name"`
}

// GreetReply 是响应 DTO。
type GreetReply struct {
	Message string `json:"message"`
}

// ResetRequest 是仅 error 方法的请求 DTO。
type ResetRequest struct {
	Scope string `json:"scope"`
}

// StatsReply 是无请求方法的响应 DTO。
type StatsReply struct {
	Served int64 `json:"served"`
}

// Service 覆盖契约方法的全部四种合法形态（DESIGN §5.3 的粗粒度约束）。
type Service interface {
	// Greet：req + resp。
	Greet(ctx context.Context, req GreetRequest) (GreetReply, error)
	// Stats：无 req，有 resp。
	Stats(ctx context.Context) (StatsReply, error)
	// Reset：有 req，仅 error。
	Reset(ctx context.Context, req ResetRequest) error
	// Ping：仅 ctx，仅 error。
	Ping(ctx context.Context) error
}

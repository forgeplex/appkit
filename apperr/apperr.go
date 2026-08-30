// Package apperr 定义全系统统一的错误形态。
//
// 错误身份是错误码（Code），不是 Go 的错误值：apperr.Is(err, code) 在
// 进程内（原始错误）与跨网络（RFC 9457 反序列化重建）两种情况下行为一致，
// 这是单体/微服务双模式语义对齐的基石。错误码常量由合约仓库生成。
package apperr

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
)

// 框架保留错误码。业务错误码由合约仓库（psp-contracts）定义。
const (
	CodeInternal         = "INTERNAL"             // 未分类内部错误，对外不泄漏细节
	CodeInvalidArgument  = "INVALID_ARGUMENT"     // 请求不合法
	CodeNotFound         = "NOT_FOUND"            //
	CodeConflict         = "CONFLICT"             // 并发/状态冲突
	CodeUnauthenticated  = "UNAUTHENTICATED"      //
	CodePermissionDenied = "PERMISSION_DENIED"    //
	CodeUnavailable      = "UNAVAILABLE"          // 依赖不可用/超时，可重试
	CodeTxBoundary       = "TX_BOUNDARY"          // 事务内发起跨模块调用（运行时守卫）
	CodeIdempotency      = "IDEMPOTENCY_CONFLICT" // 同幂等键不同 payload
	CodeMigrationDrift   = "MIGRATION_DRIFT"      // 已应用的迁移内容被改动（启动期守卫）
)

// Error 是唯一跨层传播的错误类型。值不可变：With* 方法返回副本，
// 因此包级 sentinel（var ErrX = apperr.New(...)）可以安全共享。
type Error struct {
	code    string
	status  int
	message string
	details map[string]any
	cause   error
}

// New 构造一个错误模板。message 面向调用方，必须不含内部细节。
func New(code string, status int, message string) *Error {
	return &Error{code: code, status: status, message: message}
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.code, e.message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.code, e.message)
}

// Unwrap 暴露内部链，供 errors.Is/As 检查底层错误；内部链不会被序列化外泄。
func (e *Error) Unwrap() error { return e.cause }

func (e *Error) Code() string            { return e.code }
func (e *Error) Status() int             { return e.status }
func (e *Error) Message() string         { return e.message }
func (e *Error) Details() map[string]any { return maps.Clone(e.details) }

func (e *Error) clone() *Error {
	c := *e
	c.details = maps.Clone(e.details)
	return &c
}

// WithMessage 返回替换了 message 的副本。
func (e *Error) WithMessage(format string, args ...any) *Error {
	c := e.clone()
	c.message = fmt.Sprintf(format, args...)
	return c
}

// WithDetail 返回追加了一条结构化细节的副本。细节会随响应外泄，勿放内部信息。
func (e *Error) WithDetail(key string, value any) *Error {
	c := e.clone()
	if c.details == nil {
		c.details = make(map[string]any, 1)
	}
	c.details[key] = value
	return c
}

// WithCause 返回携带内部错误链的副本。cause 只用于日志与 errors.Is/As，不外泄。
func (e *Error) WithCause(cause error) *Error {
	c := e.clone()
	c.cause = cause
	return c
}

// Is 支持 errors.Is(err, template)：同码即同错误，忽略 message/details 差异。
func (e *Error) Is(target error) bool {
	if t, ok := errors.AsType[*Error](target); ok {
		return e.code == t.code
	}
	return false
}

// Is 报告 err（或其链上任一错误）的错误码是否为 code。
func Is(err error, code string) bool {
	e, ok := errors.AsType[*Error](err)
	return ok && e.code == code
}

// From 把任意 error 规范化为 *Error：已是 *Error 原样返回，
// 其余折叠为 CodeInternal（对外只暴露"internal error"，细节留在错误链里供日志）。
func From(err error) *Error {
	if err == nil {
		return nil
	}
	if e, ok := errors.AsType[*Error](err); ok {
		return e
	}
	return New(CodeInternal, http.StatusInternalServerError, "internal error").WithCause(err)
}

// 常用构造捷径。
func Internal(cause error) *Error {
	return New(CodeInternal, http.StatusInternalServerError, "internal error").WithCause(cause)
}

func InvalidArgument(format string, args ...any) *Error {
	return New(CodeInvalidArgument, http.StatusUnprocessableEntity, fmt.Sprintf(format, args...))
}

func NotFound(format string, args ...any) *Error {
	return New(CodeNotFound, http.StatusNotFound, fmt.Sprintf(format, args...))
}

func Conflict(format string, args ...any) *Error {
	return New(CodeConflict, http.StatusConflict, fmt.Sprintf(format, args...))
}

func Unavailable(cause error) *Error {
	return New(CodeUnavailable, http.StatusServiceUnavailable, "dependency unavailable").WithCause(cause)
}

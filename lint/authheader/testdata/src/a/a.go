package a

import "net/http"

const tenantHeader = "X-Tenant-Id"

func f(r *http.Request) {
	_ = r.Header.Get("Authorization") // want `禁止直接读取身份/凭证请求头`
	_ = r.Header.Get("x-step-up")     // want `禁止直接读取身份/凭证请求头`
	_ = r.Header.Values(tenantHeader) // want `禁止直接读取身份/凭证请求头`
	_ = r.Header["X-Partition"]       // want `禁止直接读取身份/凭证请求头`
	_ = r.Header.Get("X-Caller")      // want `禁止直接读取身份/凭证请求头`

	// 普通非身份头允许读取。
	_ = r.Header.Get("Idempotency-Key")
	_ = r.Header.Get("Content-Type")

	//appkit:ignore authheader -- 代理网关转发校验
	_ = r.Header.Get("Authorization")
}

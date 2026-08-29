package apperr

import (
	"encoding/json"
	"maps"
	"net/http"
)

// Problem 是 RFC 9457 (application/problem+json) 响应体。
// code 是扩展成员，承载错误身份；跨网络重建后 apperr.Is 行为不变。
type Problem struct {
	Type    string         `json:"type,omitempty"`
	Title   string         `json:"title"`
	Status  int            `json:"status"`
	Detail  string         `json:"detail,omitempty"`
	Code    string         `json:"code"`
	Details map[string]any `json:"details,omitempty"`
}

// Problem 把错误转为 RFC 9457 形态。内部错误链（cause）不出现在结果里。
func (e *Error) Problem() Problem {
	return Problem{
		Title:   http.StatusText(e.status),
		Status:  e.status,
		Detail:  e.message,
		Code:    e.code,
		Details: maps.Clone(e.details),
	}
}

// WriteProblem 把 err 规范化后写为 problem+json 响应。
// 服务端出口统一走这里，保证响应形态唯一。
func WriteProblem(w http.ResponseWriter, err error) {
	p := From(err).Problem()
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// FromProblem 在客户端把 problem+json 响应体重建为 *Error。
// 解析失败时折叠为对应 HTTP 状态的 CodeInternal/CodeUnavailable。
func FromProblem(status int, body []byte) *Error {
	var p Problem
	if err := json.Unmarshal(body, &p); err != nil || p.Code == "" {
		code := CodeInternal
		if status >= 502 && status <= 504 {
			code = CodeUnavailable
		}
		return New(code, status, http.StatusText(status))
	}
	e := New(p.Code, statusOr(p.Status, status), p.Detail)
	e.details = maps.Clone(p.Details)
	return e
}

func statusOr(v, fallback int) int {
	if v != 0 {
		return v
	}
	return fallback
}

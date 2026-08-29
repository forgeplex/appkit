package apperr_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/forgeplex/appkit/apperr"
)

func TestProblemExcludesCause(t *testing.T) {
	secret := "pgx: password authentication failed"
	e := apperr.Internal(errors.New(secret))

	p := e.Problem()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	if strings.Contains(body, secret) {
		t.Errorf("Problem 序列化泄漏了 cause：%s", body)
	}
	if strings.Contains(body, "cause") {
		t.Errorf("Problem 序列化不应含 cause 字段：%s", body)
	}
	if p.Code != apperr.CodeInternal || p.Status != http.StatusInternalServerError {
		t.Errorf("Problem = %+v", p)
	}
}

func TestWriteProblem(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantDetail string
	}{
		{"apperr 原样映射", apperr.NotFound("account a-1"), http.StatusNotFound, apperr.CodeNotFound, "account a-1"},
		{"未知错误折叠为 INTERNAL", errors.New("boom"), http.StatusInternalServerError, apperr.CodeInternal, "internal error"},
		{"带 detail 的错误", apperr.InvalidArgument("bad").WithDetail("field", "amount"), http.StatusUnprocessableEntity, apperr.CodeInvalidArgument, "bad"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			apperr.WriteProblem(rec, tt.err)

			if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("Content-Type = %q, want application/problem+json", ct)
			}
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			var p apperr.Problem
			if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
				t.Fatalf("body 非法 JSON: %v", err)
			}
			if p.Code != tt.wantCode || p.Status != tt.wantStatus || p.Detail != tt.wantDetail {
				t.Errorf("body = %+v, want code=%q status=%d detail=%q", p, tt.wantCode, tt.wantStatus, tt.wantDetail)
			}
		})
	}
}

// 往返：服务端 WriteProblem → 客户端 FromProblem，apperr.Is 行为必须一致。
func TestFromProblemRoundtrip(t *testing.T) {
	tests := []struct {
		name string
		err  *apperr.Error
	}{
		{"NotFound", apperr.NotFound("account a-1")},
		{"Conflict 带 detail", apperr.Conflict("version").WithDetail("expected", "3")},
		{"业务错误码", apperr.New("LEDGER_INSUFFICIENT_FUNDS", http.StatusConflict, "余额不足")},
		{"Internal 带 cause", apperr.Internal(errors.New("hidden"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			apperr.WriteProblem(rec, tt.err)

			got := apperr.FromProblem(rec.Code, rec.Body.Bytes())

			if !apperr.Is(got, tt.err.Code()) {
				t.Errorf("往返后 apperr.Is(got, %q) = false", tt.err.Code())
			}
			if !errors.Is(got, tt.err) {
				t.Errorf("往返后 errors.Is(got, 原错误模板) = false")
			}
			if got.Status() != tt.err.Status() {
				t.Errorf("status = %d, want %d", got.Status(), tt.err.Status())
			}
			if got.Message() != tt.err.Message() {
				t.Errorf("message = %q, want %q", got.Message(), tt.err.Message())
			}
			if got.Unwrap() != nil {
				t.Errorf("重建的错误不应带 cause：%v", got.Unwrap())
			}
			if want := tt.err.Details(); want != nil {
				for k, v := range want {
					if got.Details()[k] != v {
						t.Errorf("detail[%q] = %v, want %v", k, got.Details()[k], v)
					}
				}
			}
		})
	}
}

func TestFromProblemParseFailure(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     []byte
		wantCode string
	}{
		{"502 折叠为 UNAVAILABLE", http.StatusBadGateway, []byte("<html>bad gateway</html>"), apperr.CodeUnavailable},
		{"503 折叠为 UNAVAILABLE", http.StatusServiceUnavailable, []byte("not json"), apperr.CodeUnavailable},
		{"504 折叠为 UNAVAILABLE", http.StatusGatewayTimeout, nil, apperr.CodeUnavailable},
		{"500 非网关折叠为 INTERNAL", http.StatusInternalServerError, []byte("oops"), apperr.CodeInternal},
		{"404 折叠为 INTERNAL", http.StatusNotFound, []byte("not json"), apperr.CodeInternal},
		{"合法 JSON 但缺 code 同样折叠", http.StatusBadGateway, []byte(`{"title":"Bad Gateway","status":502}`), apperr.CodeUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := apperr.FromProblem(tt.status, tt.body)
			if got.Code() != tt.wantCode {
				t.Errorf("Code() = %q, want %q", got.Code(), tt.wantCode)
			}
			if got.Status() != tt.status {
				t.Errorf("Status() = %d, want %d", got.Status(), tt.status)
			}
			if got.Message() != http.StatusText(tt.status) {
				t.Errorf("Message() = %q, want %q", got.Message(), http.StatusText(tt.status))
			}
		})
	}
}

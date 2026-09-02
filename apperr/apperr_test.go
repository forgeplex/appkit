package apperr_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/forgeplex/appkit/apperr"
)

func TestIsCodeIdentity(t *testing.T) {
	base := apperr.New(apperr.CodeNotFound, http.StatusNotFound, "no such account")

	tests := []struct {
		name string
		err  error
		code string
		want bool
	}{
		{"同码命中", base, apperr.CodeNotFound, true},
		{"异码不命中", base, apperr.CodeConflict, false},
		{"fmt.Errorf 包裹后仍命中", fmt.Errorf("查询失败: %w", base), apperr.CodeNotFound, true},
		{"WithDetail 副本仍命中", base.WithDetail("id", "a-1"), apperr.CodeNotFound, true},
		{"WithMessage 副本仍命中", base.WithMessage("换个说法"), apperr.CodeNotFound, true},
		{"非 apperr 错误不命中", errors.New("plain"), apperr.CodeNotFound, false},
		{"nil 不命中", nil, apperr.CodeNotFound, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := apperr.Is(tt.err, tt.code); got != tt.want {
				t.Errorf("apperr.Is(%v, %q) = %v, want %v", tt.err, tt.code, got, tt.want)
			}
		})
	}
}

func TestErrorsIsTemplate(t *testing.T) {
	tmpl := apperr.New(apperr.CodeConflict, http.StatusConflict, "template")

	tests := []struct {
		name   string
		err    error
		target error
		want   bool
	}{
		{"同码异 message 视为同错误", apperr.Conflict("其他描述"), tmpl, true},
		{"同码带 detail 视为同错误", tmpl.WithDetail("k", "v"), tmpl, true},
		{"包裹后仍匹配模板", fmt.Errorf("outer: %w", apperr.Conflict("x")), tmpl, true},
		{"异码不匹配", apperr.NotFound("x"), tmpl, false},
		{"普通错误不匹配", errors.New("plain"), tmpl, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errors.Is(tt.err, tt.target); got != tt.want {
				t.Errorf("errors.Is(%v, %v) = %v, want %v", tt.err, tt.target, got, tt.want)
			}
		})
	}
}

func TestFrom(t *testing.T) {
	known := apperr.NotFound("gone")
	plain := errors.New("driver: connection refused")

	tests := []struct {
		name       string
		in         error
		wantNil    bool
		wantCode   string
		wantStatus int
	}{
		{"nil 返回 nil", nil, true, "", 0},
		{"已是 apperr 原样返回", known, false, apperr.CodeNotFound, http.StatusNotFound},
		{"未知错误折叠为 INTERNAL", plain, false, apperr.CodeInternal, http.StatusInternalServerError},
		{"包裹的 apperr 被识别", fmt.Errorf("w: %w", known), false, apperr.CodeNotFound, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := apperr.From(tt.in)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("From(nil) = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("From 返回 nil")
			}
			if got.Code() != tt.wantCode || got.Status() != tt.wantStatus {
				t.Errorf("From(%v) = (%q, %d), want (%q, %d)", tt.in, got.Code(), got.Status(), tt.wantCode, tt.wantStatus)
			}
		})
	}

	t.Run("折叠后 cause 保留在错误链", func(t *testing.T) {
		got := apperr.From(plain)
		if !errors.Is(got, plain) {
			t.Errorf("errors.Is(From(plain), plain) = false，cause 丢失")
		}
		if got.Message() != "internal error" {
			t.Errorf("对外 message = %q，不应泄漏内部细节", got.Message())
		}
	})
}

func TestImmutability(t *testing.T) {
	sentinel := apperr.New(apperr.CodeInvalidArgument, http.StatusUnprocessableEntity, "bad input")

	derivedDetail := sentinel.WithDetail("field", "amount")
	derivedCause := sentinel.WithCause(errors.New("inner"))
	derivedMsg := sentinel.WithMessage("changed")

	if sentinel.Details() != nil {
		t.Errorf("WithDetail 污染了 sentinel：Details() = %v", sentinel.Details())
	}
	if sentinel.Unwrap() != nil {
		t.Errorf("WithCause 污染了 sentinel：Unwrap() = %v", sentinel.Unwrap())
	}
	if sentinel.Message() != "bad input" {
		t.Errorf("WithMessage 污染了 sentinel：Message() = %q", sentinel.Message())
	}

	if derivedDetail == sentinel || derivedCause == sentinel || derivedMsg == sentinel {
		t.Error("With* 应返回副本而非原值")
	}
	if v := derivedDetail.Details()["field"]; v != "amount" {
		t.Errorf("副本 detail = %v, want %q", v, "amount")
	}

	// 二次派生不得影响一次派生（clone 必须深拷贝 details）。
	derivedDetail.WithDetail("field", "overwritten")
	if v := derivedDetail.Details()["field"]; v != "amount" {
		t.Errorf("二次 WithDetail 污染了一次派生：%v", v)
	}

	// Details() 返回的 map 是副本，改它不影响错误本身。
	m := derivedDetail.Details()
	m["field"] = "mutated"
	if v := derivedDetail.Details()["field"]; v != "amount" {
		t.Errorf("Details() 未做防御性拷贝：%v", v)
	}
}

func TestShortcuts(t *testing.T) {
	cause := errors.New("boom")

	tests := []struct {
		name       string
		err        *apperr.Error
		wantCode   string
		wantStatus int
	}{
		{"Internal", apperr.Internal(cause), apperr.CodeInternal, http.StatusInternalServerError},
		{"InvalidArgument", apperr.InvalidArgument("字段 %s 非法", "amount"), apperr.CodeInvalidArgument, http.StatusUnprocessableEntity},
		{"NotFound", apperr.NotFound("account %s", "a-1"), apperr.CodeNotFound, http.StatusNotFound},
		{"Conflict", apperr.Conflict("version mismatch"), apperr.CodeConflict, http.StatusConflict},
		{"Unavailable", apperr.Unavailable(cause), apperr.CodeUnavailable, http.StatusServiceUnavailable},
		{"Unauthenticated", apperr.Unauthenticated("authentication required"), apperr.CodeUnauthenticated, http.StatusUnauthorized},
		{"PermissionDenied", apperr.PermissionDenied("permission denied"), apperr.CodePermissionDenied, http.StatusForbidden},
		{"StepUpRequired", apperr.StepUpRequired("step-up authentication required"), apperr.CodeStepUpRequired, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err.Code() != tt.wantCode {
				t.Errorf("Code() = %q, want %q", tt.err.Code(), tt.wantCode)
			}
			if tt.err.Status() != tt.wantStatus {
				t.Errorf("Status() = %d, want %d", tt.err.Status(), tt.wantStatus)
			}
		})
	}

	t.Run("带 cause 的捷径保留错误链", func(t *testing.T) {
		for _, e := range []*apperr.Error{apperr.Internal(cause), apperr.Unavailable(cause)} {
			if !errors.Is(e, cause) {
				t.Errorf("%s：cause 未入链", e.Code())
			}
		}
	})

	t.Run("格式化 message", func(t *testing.T) {
		e := apperr.NotFound("account %s", "a-1")
		if e.Message() != "account a-1" {
			t.Errorf("Message() = %q", e.Message())
		}
	})
}

package appkit

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/forgeplex/appkit/apperr"
)

// permEnv 搭一个声明了普通码与 Challenge 码的 Registry，返回它和一个
// 始终 200 的目标 handler（经 Require 包装后判定矩阵只看响应）。
func permEnv(t *testing.T) (*Registry, http.HandlerFunc) {
	t.Helper()
	reg := newRegistry()
	reg.current = "files"
	reg.Permissions(
		PermissionDecl{Code: "files:read", Name: "文件读取", Category: "files"},
		PermissionDecl{Code: "files:delete", Name: "文件删除", Category: "files", Challenge: true},
	)
	reg.current = ""
	return reg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestPermissionsCollectSorted(t *testing.T) {
	reg, _ := permEnv(t)
	got := reg.PermissionDecls()
	if len(got) != 2 || got[0].Code != "files:delete" || got[1].Code != "files:read" {
		t.Fatalf("声明应按码排序输出，实际 %+v", got)
	}
	if !got[0].Challenge || got[1].Challenge {
		t.Fatalf("Challenge 标记应随声明保留，实际 %+v", got)
	}
}

func TestPermissionsDuplicatePanics(t *testing.T) {
	reg, _ := permEnv(t)
	reg.current = "other"
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("跨模块重复声明应 panic")
		}
		if !strings.Contains(fmt.Sprint(r), "files") || !strings.Contains(fmt.Sprint(r), "files:read") {
			t.Fatalf("报错应指出两个模块与码，实际 %v", r)
		}
	}()
	reg.Permissions(PermissionDecl{Code: "files:read", Name: "撞码"})
}

func TestPermissionsEmptyCodePanics(t *testing.T) {
	reg := newRegistry()
	reg.current = "files"
	defer func() {
		if recover() == nil {
			t.Fatal("空权限码应 panic")
		}
	}()
	reg.Permissions(PermissionDecl{Name: "没码"})
}

func TestPermissionsAfterRegisterPanics(t *testing.T) {
	reg, _ := permEnv(t)
	reg.registered = true
	reg.current = "files"
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Register 阶段之后的声明应 panic")
		}
		if !strings.Contains(fmt.Sprint(r), "Register") {
			t.Fatalf("报错应说明声明只允许 Register 阶段，实际 %v", r)
		}
	}()
	reg.Permissions(PermissionDecl{Code: "late:code", Name: "迟到"})
}

// doReq 对 wrapped 发起带 actor 的请求，返回 (状态码, 规范化错误码)。
func doReq(t *testing.T, wrapped http.Handler, actor *Actor) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/files", nil)
	if actor != nil {
		req = req.WithContext(WithActor(req.Context(), *actor))
	}
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	code := ""
	if rec.Body.Len() > 0 {
		code = apperr.FromProblem(rec.Code, rec.Body.Bytes()).Code()
	}
	return rec.Code, code
}

func TestRequireMatrix(t *testing.T) {
	reg, target := permEnv(t)
	del := reg.Require("files:delete", target)
	read := reg.Require("files:read", target)

	// 未认证：无 Actor → 401。
	if s, c := doReq(t, del, nil); s != 401 || c != apperr.CodeUnauthenticated {
		t.Fatalf("无 Actor 应 401/%s，实际 %d/%s", apperr.CodeUnauthenticated, s, c)
	}
	// 认证但缺码 → 403。
	empty := Actor{UserID: "u1", Perms: []string{"files:read"}}
	if s, c := doReq(t, del, &empty); s != 403 || c != apperr.CodePermissionDenied {
		t.Fatalf("缺码应 403/%s，实际 %d/%s", apperr.CodePermissionDenied, s, c)
	}
	// 有码但 Challenge 未带证明 → 403 STEP_UP。
	deleter := Actor{UserID: "u1", Perms: []string{"files:delete"}}
	if s, c := doReq(t, del, &deleter); s != 403 || c != apperr.CodeStepUpRequired {
		t.Fatalf("无证明的 Challenge 码应 403/%s，实际 %d/%s", apperr.CodeStepUpRequired, s, c)
	}
	// 有码 + 证明过期 → 403 STEP_UP。
	stale := deleter
	stale.StepUpAt = time.Now().Add(-stepUpMaxAge - time.Second)
	if s, c := doReq(t, del, &stale); s != 403 || c != apperr.CodeStepUpRequired {
		t.Fatalf("过期证明应 403/%s，实际 %d/%s", apperr.CodeStepUpRequired, s, c)
	}
	// 有码 + 新鲜证明 → 200。
	fresh := deleter
	fresh.StepUpAt = time.Now().Add(-stepUpMaxAge + time.Minute)
	if s, _ := doReq(t, del, &fresh); s != 200 {
		t.Fatalf("新鲜证明应放行，实际 %d", s)
	}
	// 非 Challenge 码不需要证明 → 200。
	if s, _ := doReq(t, read, &empty); s != 200 {
		t.Fatalf("有码即应放行，实际 %d", s)
	}
	// Actor 存在但 UserID 为空（畸形凭证）→ 401 而非放行。
	if s, _ := doReq(t, read, &Actor{Perms: []string{"files:read"}}); s != 401 {
		t.Fatalf("空 UserID 应 401，实际 %d", s)
	}
}

func TestCheck(t *testing.T) {
	ctx := WithActor(context.Background(), Actor{UserID: "u1", Perms: []string{"files:read"}})
	if !Check(ctx, "files:read") {
		t.Fatal("持有码应通过")
	}
	if Check(ctx, "files:delete") {
		t.Fatal("未持有的码不应通过")
	}
	if Check(context.Background(), "files:read") {
		t.Fatal("未认证不应通过")
	}
}

func TestValidatePermBindings(t *testing.T) {
	reg, target := permEnv(t)
	reg.Require("files:read", target) // 已声明：合法（含跨模块绑定）。
	if err := reg.validatePermBindings(); err != nil {
		t.Fatalf("已声明的绑定不应报错: %v", err)
	}

	reg2, target2 := permEnv(t)
	reg2.current = "gateway"
	reg2.Require("files:reed", target2) // 拼错的码。
	err := reg2.validatePermBindings()
	if err == nil || !strings.Contains(err.Error(), "gateway") || !strings.Contains(err.Error(), "files:reed") {
		t.Fatalf("拼错码应报错并指出模块与码，实际 %v", err)
	}
}

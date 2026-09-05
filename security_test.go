package appkit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/callctx"
)

func TestRunRequiresExplicitSecurityMode(t *testing.T) {
	app := New(nil, HTTPAddr("127.0.0.1:0"))
	err := app.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "未声明 HTTP 安全模式") {
		t.Fatalf("未声明安全模式应在监听前失败，实际 %v", err)
	}
}

func TestStrictModeRejectsUnclassifiedRoute(t *testing.T) {
	m := ModuleFunc("files", func(reg *Registry) error {
		reg.Mount("GET /files", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		return nil
	})
	app := New([]Module{m}, Security(SecurityUserFacing), HTTPAddr("127.0.0.1:0"))
	err := app.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), `模块 "files" 的路由 "GET /files"`) ||
		!strings.Contains(err.Error(), "MountPublic") {
		t.Fatalf("裸 Mount 应被点名并拒绝，实际 %v", err)
	}
}

func TestStrictModeRejectsRouteMountedDuringSetup(t *testing.T) {
	m := ModuleFunc("late", func(reg *Registry) error {
		reg.Setup(func(context.Context) error {
			reg.Mount("GET /late", http.NotFoundHandler())
			return nil
		})
		return nil
	})
	app := New([]Module{m}, Security(SecurityMixed), HTTPAddr("127.0.0.1:0"))
	err := app.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), `模块 "late" 的路由 "GET /late"`) {
		t.Fatalf("Setup 期挂的裸路由也应在监听前拒绝，实际 %v", err)
	}
}

func TestClassifiedRouteRejectsNilHandlerAtStartup(t *testing.T) {
	m := ModuleFunc("nil-handler", func(reg *Registry) error {
		reg.MountAuthenticated("GET /nil", nil)
		return nil
	})
	app := New([]Module{m}, Security(SecurityUserFacing), HTTPAddr("127.0.0.1:0"))
	err := app.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), `路由 "GET /nil" 的 handler 不能为 nil`) {
		t.Fatalf("nil handler 应在监听前失败，实际 %v", err)
	}
}

func TestRouteSecurityModeMatrix(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	tests := []struct {
		name    string
		mode    SecurityMode
		mount   func(*Registry)
		pprof   bool
		wantErr string
	}{
		{"user public", SecurityUserFacing, func(r *Registry) { r.MountPublic("GET /x", ok) }, false, ""},
		{"user authenticated", SecurityUserFacing, func(r *Registry) { r.MountAuthenticated("GET /x", ok) }, false, ""},
		{"user permission", SecurityUserFacing, func(r *Registry) { r.MountPermission("GET /x", "x:read", ok) }, false, ""},
		{"user rejects internal", SecurityUserFacing, func(r *Registry) { r.MountInternalService("GET /x", ok) }, false, "InternalService"},
		{"internal public", SecurityInternalService, func(r *Registry) { r.MountPublic("GET /x", ok) }, false, ""},
		{"internal service", SecurityInternalService, func(r *Registry) { r.MountInternalService("GET /x", ok) }, false, ""},
		{"internal rejects user", SecurityInternalService, func(r *Registry) { r.MountAuthenticated("GET /x", ok) }, false, "用户身份路由"},
		{"internal rejects permission", SecurityInternalService, func(r *Registry) { r.MountPermission("GET /x", "x:read", ok) }, false, "用户身份路由"},
		{"mixed accepts user", SecurityMixed, func(r *Registry) { r.MountAuthenticated("GET /x", ok) }, false, ""},
		{"mixed accepts permission", SecurityMixed, func(r *Registry) { r.MountPermission("GET /x", "x:read", ok) }, false, ""},
		{"mixed accepts internal", SecurityMixed, func(r *Registry) { r.MountInternalService("GET /x", ok) }, false, ""},
		{"user rejects legacy mount", SecurityUserFacing, func(r *Registry) { r.Mount("GET /x", ok) }, false, "未声明安全分类"},
		{"internal rejects legacy mount", SecurityInternalService, func(r *Registry) { r.Mount("GET /x", ok) }, false, "未声明安全分类"},
		{"mixed rejects legacy mount", SecurityMixed, func(r *Registry) { r.Mount("GET /x", ok) }, false, "未声明安全分类"},
		{"disabled keeps legacy mount", SecurityDisabled, func(r *Registry) { r.Mount("GET /x", ok) }, true, ""},
		{"user rejects pprof", SecurityUserFacing, func(*Registry) {}, true, "pprof"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := newRegistry()
			reg.current = "demo"
			tc.mount(reg)
			err := reg.validateRouteSecurity(tc.mode, tc.pprof)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("应通过，实际 %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("应包含 %q，实际 %v", tc.wantErr, err)
			}
		})
	}
}

func TestRouteGuards(t *testing.T) {
	target := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	reg := newRegistry()
	reg.current = "demo"
	reg.Permissions(PermissionDecl{Code: "demo:read"})
	reg.MountAuthenticated("GET /user", target)
	reg.MountInternalService("GET /internal", target)
	reg.MountPermission("GET /permission", "demo:read", target)
	if err := reg.validatePermBindings(); err != nil {
		t.Fatalf("已声明权限绑定应通过校验: %v", err)
	}

	statusAndCode := func(h http.Handler, ctx context.Context) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		code := ""
		if rec.Body.Len() > 0 {
			code = apperr.FromProblem(rec.Code, rec.Body.Bytes()).Code()
		}
		return rec.Code, code
	}

	status, code := statusAndCode(reg.mounts[0].handler, context.Background())
	if status != http.StatusUnauthorized || code != apperr.CodeUnauthenticated {
		t.Fatalf("用户路由无 Actor 应 401，实际 %d/%s", status, code)
	}
	userCtx := WithActor(context.Background(), Actor{UserID: "u1"})
	if status, _ := statusAndCode(reg.mounts[0].handler, userCtx); status != http.StatusNoContent {
		t.Fatalf("用户路由有 Actor 应放行，实际 %d", status)
	}
	status, code = statusAndCode(reg.mounts[1].handler, context.Background())
	if status != http.StatusUnauthorized || code != apperr.CodeUnauthenticated {
		t.Fatalf("内部路由无 ServicePrincipal 应 401，实际 %d/%s", status, code)
	}
	serviceCtx := WithServicePrincipal(context.Background(), ServicePrincipal{Subject: "ledger"})
	if status, _ := statusAndCode(reg.mounts[1].handler, serviceCtx); status != http.StatusNoContent {
		t.Fatalf("内部路由有 ServicePrincipal 应放行，实际 %d", status)
	}
	status, code = statusAndCode(reg.mounts[2].handler, context.Background())
	if status != http.StatusUnauthorized || code != apperr.CodeUnauthenticated {
		t.Fatalf("权限路由无 Actor 应 401，实际 %d/%s", status, code)
	}
	status, code = statusAndCode(reg.mounts[2].handler, userCtx)
	if status != http.StatusForbidden || code != apperr.CodePermissionDenied {
		t.Fatalf("权限路由缺码应 403，实际 %d/%s", status, code)
	}
	permitted := WithActor(context.Background(), Actor{UserID: "u1", Perms: []string{"demo:read"}})
	if status, _ := statusAndCode(reg.mounts[2].handler, permitted); status != http.StatusNoContent {
		t.Fatalf("权限路由持码应放行，实际 %d", status)
	}
}

func TestServicePrincipalAudienceIsSnapshot(t *testing.T) {
	audience := []string{"ledger"}
	ctx := WithServicePrincipal(context.Background(), ServicePrincipal{
		Subject: "gateway", Audience: audience,
	})
	audience[0] = "mutated-before-read"
	got, ok := ServicePrincipalFrom(ctx)
	if !ok || got.Audience[0] != "ledger" {
		t.Fatalf("注入时应复制 audience，实际 %+v ok=%v", got, ok)
	}
	got.Audience[0] = "mutated-after-read"
	again, _ := ServicePrincipalFrom(ctx)
	if again.Audience[0] != "ledger" {
		t.Fatalf("读取时应返回 audience 副本，实际 %+v", again)
	}
}

func TestIdentityBoundaryDropsUnverifiedIdentityAndAllowsVerifiedRebuild(t *testing.T) {
	var verifierSawUntrusted bool
	verifier := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, hasActor := ActorFrom(r.Context())
			_, hasService := ServicePrincipalFrom(r.Context())
			m := callctx.From(r.Context())
			verifierSawUntrusted = hasActor || hasService || m.Partition != "" || m.TenantID != "" || m.Caller != "" ||
				r.Header.Get(callctx.HeaderPartition) != "" ||
				r.Header.Get(callctx.HeaderTenantID) != "" || r.Header.Get(callctx.HeaderCaller) != "" ||
				r.Header.Get("X-Merchant-Id") != ""

			m.Partition = "verified-partition"
			m.TenantID = "verified-tenant"
			ctx := callctx.With(WithActor(r.Context(), Actor{UserID: "verified-user", TenantID: m.TenantID}), m)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}

	var gotMeta callctx.Meta
	var gotActor Actor
	var gotActorOK bool
	target := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMeta = callctx.From(r.Context())
		gotActor, gotActorOK = ActorFrom(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	app := New(nil, Security(SecurityUserFacing), Middleware(verifier))
	h := app.wrap(target)

	req := httptest.NewRequest(http.MethodGet, "/private", nil)
	req.Header.Set(callctx.HeaderPartition, "forged-header-partition")
	req.Header.Set(callctx.HeaderTenantID, "forged-header-tenant")
	req.Header.Set(callctx.HeaderCaller, "forged-caller")
	req.Header.Set("X-Merchant-Id", "forged-merchant")
	ctx := callctx.With(req.Context(), callctx.Meta{
		RequestID: "req-1", Partition: "forged-context-partition", TenantID: "forged-context-tenant", Caller: "forged-context-caller",
	})
	ctx = WithActor(ctx, Actor{UserID: "forged-user"})
	ctx = WithServicePrincipal(ctx, ServicePrincipal{Subject: "forged-service"})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("验证后请求应放行，实际 %d", rec.Code)
	}
	if verifierSawUntrusted {
		t.Fatal("验签中间件看到 unsigned header 或边界外预置 principal")
	}
	if !gotActorOK || gotActor.UserID != "verified-user" || gotMeta.Partition != "verified-partition" || gotMeta.TenantID != "verified-tenant" {
		t.Fatalf("验签中间件应能在边界内重建身份，actor=%+v ok=%v meta=%+v", gotActor, gotActorOK, gotMeta)
	}
	if gotMeta.RequestID != "req-1" || gotMeta.Caller != "" {
		t.Fatalf("边界应保留 request id、清除 caller，实际 %+v", gotMeta)
	}
}

func TestDisabledModeDoesNotApplyIdentityBoundary(t *testing.T) {
	var gotMeta callctx.Meta
	var gotActor Actor
	var gotActorOK bool
	var gotTenantHeader string
	target := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMeta = callctx.From(r.Context())
		gotActor, gotActorOK = ActorFrom(r.Context())
		gotTenantHeader = r.Header.Get(callctx.HeaderTenantID)
		w.WriteHeader(http.StatusNoContent)
	})
	app := New(nil, Security(SecurityDisabled))
	req := httptest.NewRequest(http.MethodGet, "/dev", nil)
	req.Header.Set(callctx.HeaderTenantID, "dev-tenant")
	ctx := callctx.With(req.Context(), callctx.Meta{TenantID: "dev-tenant", Caller: "dev-caller"})
	req = req.WithContext(WithActor(ctx, Actor{UserID: "dev-user"}))
	rec := httptest.NewRecorder()
	app.wrap(target).ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent || !gotActorOK || gotActor.UserID != "dev-user" ||
		gotMeta.TenantID != "dev-tenant" || gotMeta.Caller != "dev-caller" || gotTenantHeader != "dev-tenant" {
		t.Fatalf("disabled 应保留兼容行为，status=%d actor=%+v/%v meta=%+v header=%q",
			rec.Code, gotActor, gotActorOK, gotMeta, gotTenantHeader)
	}
}

func TestHTTPServerOptionCannotReplaceProtectedHandler(t *testing.T) {
	app := New(nil,
		Security(SecurityUserFacing),
		HTTPServer(func(server *http.Server) {
			server.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTeapot)
			})
		}),
	)
	protected := app.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	server := app.buildServer(protected)
	rec := httptest.NewRecorder()
	server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("HTTPServer 回调不得替换安全 handler，实际 %d", rec.Code)
	}
}

func TestDisabledModePreservesHTTPServerHandlerOverride(t *testing.T) {
	app := New(nil,
		Security(SecurityDisabled),
		HTTPServer(func(server *http.Server) {
			server.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTeapot)
			})
		}),
	)
	server := app.buildServer(http.NotFoundHandler())
	rec := httptest.NewRecorder()
	server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("disabled 应保留 HTTPServer Handler 历史覆盖行为，实际 %d", rec.Code)
	}
}

func TestPprofIsProtectedAsInternalService(t *testing.T) {
	app := New(nil, Security(SecurityInternalService), Pprof())
	mux, err := app.buildMux()
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	app.wrap(mux).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("pprof 无服务身份应 401，实际 %d", rec.Code)
	}

	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := WithServicePrincipal(r.Context(), ServicePrincipal{Subject: "ops"})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	app = New(nil, Security(SecurityInternalService), Pprof(), Middleware(inject))
	mux, err = app.buildMux()
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	app.wrap(mux).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pprof 有已验证服务身份应放行，实际 %d", rec.Code)
	}
}

package idem

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit/apperr"
)

// ---- 不需要 Postgres 的单测 ----

func TestMigrationSQL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		schema   string
		wantTbl  string
		wantFrag []string
	}{
		{
			name:    "普通schema",
			schema:  "ledger",
			wantTbl: `"ledger"."idempotency_keys"`,
			wantFrag: []string{
				"key          text PRIMARY KEY",
				"payload_hash bytea NOT NULL",
				"owner_token  uuid NOT NULL",
				"state        text NOT NULL CHECK (state IN ('in_progress', 'completed'))",
				"headers      jsonb",
			},
		},
		{
			name:    "需转义的schema",
			schema:  `we"ird`,
			wantTbl: `"we""ird"."idempotency_keys"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sql := MigrationSQL(tc.schema)
			if !strings.Contains(sql, tc.wantTbl) {
				t.Errorf("缺少表名 %s:\n%s", tc.wantTbl, sql)
			}
			for _, frag := range tc.wantFrag {
				if !strings.Contains(sql, frag) {
					t.Errorf("缺少片段 %q:\n%s", frag, sql)
				}
			}
		})
	}
}

func TestMiddlewarePassThroughWithoutKey(t *testing.T) {
	t.Parallel()
	// pool 为 nil：无幂等键的请求绝不能碰数据库，碰了就 panic 即测试失败。
	mw := Middleware(NewStore(nil, "s"), slog.New(slog.DiscardHandler))

	tests := []struct {
		name   string
		method string
		body   string
	}{
		{name: "GET", method: http.MethodGet},
		{name: "POST带body", method: http.MethodPost, body: `{"a":1}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotBody string
			h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				gotBody = string(b)
				w.WriteHeader(http.StatusTeapot)
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, "/x", strings.NewReader(tc.body)))

			if rec.Code != http.StatusTeapot {
				t.Fatalf("status = %d, want 418（应放行到 handler）", rec.Code)
			}
			if gotBody != tc.body {
				t.Fatalf("handler 收到的 body = %q, want %q", gotBody, tc.body)
			}
			if rec.Header().Get(HeaderReplayed) != "" {
				t.Fatal("放行请求不应带 Idempotency-Replayed")
			}
		})
	}
}

func TestPayloadHash(t *testing.T) {
	t.Parallel()
	base := payloadHash("POST", "/pay", []byte("b"))
	tests := []struct {
		name   string
		method string
		path   string
		body   string
		same   bool
	}{
		{name: "完全相同", method: "POST", path: "/pay", body: "b", same: true},
		{name: "异body", method: "POST", path: "/pay", body: "x"},
		{name: "异method", method: "PUT", path: "/pay", body: "b"},
		{name: "异path", method: "POST", path: "/refund", body: "b"},
		{name: "字段歧义拼接", method: "POST/pay", path: "", body: "b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := payloadHash(tc.method, tc.path, []byte(tc.body))
			if bytes.Equal(got, base) != tc.same {
				t.Fatalf("hash 相等性 = %v, want %v", !tc.same, tc.same)
			}
		})
	}
}

func TestCaptureWriter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		writeHeader  int
		writes       []string
		limit        int
		wantStatus   int
		wantBuf      string
		wantOverflow bool
	}{
		{name: "隐式200并捕获", writes: []string{"hello"}, limit: 100, wantStatus: 200, wantBuf: "hello"},
		{name: "显式状态", writeHeader: 201, writes: []string{"x"}, limit: 100, wantStatus: 201, wantBuf: "x"},
		{name: "恰好等于上限不超", writes: []string{"12345"}, limit: 5, wantStatus: 200, wantBuf: "12345"},
		{name: "超限丢缓存仍透传", writes: []string{"aaaa", "bbbb"}, limit: 5, wantStatus: 200, wantBuf: "", wantOverflow: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			cw := &captureWriter{ResponseWriter: rec, limit: tc.limit}
			if tc.writeHeader != 0 {
				cw.WriteHeader(tc.writeHeader)
			}
			var full strings.Builder
			for _, s := range tc.writes {
				full.WriteString(s)
				if _, err := cw.Write([]byte(s)); err != nil {
					t.Fatal(err)
				}
			}
			if cw.status != tc.wantStatus {
				t.Errorf("status = %d, want %d", cw.status, tc.wantStatus)
			}
			if got := cw.buf.String(); got != tc.wantBuf {
				t.Errorf("缓存 = %q, want %q", got, tc.wantBuf)
			}
			if cw.overflow != tc.wantOverflow {
				t.Errorf("overflow = %v, want %v", cw.overflow, tc.wantOverflow)
			}
			if got := rec.Body.String(); got != full.String() {
				t.Errorf("透传给客户端的 body = %q, want %q", got, full.String())
			}
		})
	}
}

// TestOversizeRequestBodyReturns413 验证超限请求体在 claim 之前就被拒绝：
// pool 为 nil，若中间件碰数据库即 panic 失败，证明没有留下任何 claim。
func TestOversizeRequestBodyReturns413(t *testing.T) {
	t.Parallel()
	var runs atomic.Int32
	h := Middleware(NewStore(nil, "s"), slog.New(slog.DiscardHandler), WithMaxRequestBytes(8))(
		http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			runs.Add(1)
		}))

	rec := doReq(t, h, http.MethodPost, "/pay", "big-body", strings.Repeat("x", 9))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	if p := decodeProblem(t, rec.Body); p.Code != apperr.CodeInvalidArgument {
		t.Fatalf("problem code = %q, want %q", p.Code, apperr.CodeInvalidArgument)
	}
	if runs.Load() != 0 {
		t.Fatalf("超限请求不应执行 handler，执行次数 = %d", runs.Load())
	}
}

// ---- 需要 Postgres 的测试（TEST_DATABASE_URL）----

var schemaSeq atomic.Int64

func testStore(t *testing.T) (*pgxpool.Pool, *Store) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过需要 Postgres 的测试")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("连接池: %v", err)
	}
	t.Cleanup(pool.Close)

	schema := fmt.Sprintf("idem_test_%d_%d", time.Now().UnixNano(), schemaSeq.Add(1))
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("建 schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	if _, err := pool.Exec(ctx, MigrationSQL(schema)); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	return pool, NewStore(pool, schema)
}

func rowState(t *testing.T, s *Store, key string) (state string, found bool) {
	t.Helper()
	err := s.pool.QueryRow(context.Background(),
		"SELECT state FROM "+s.tbl+" WHERE key = $1", key).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("查记录: %v", err)
	}
	return state, true
}

func decodeProblem(t *testing.T, body io.Reader) apperr.Problem {
	t.Helper()
	var p apperr.Problem
	if err := json.NewDecoder(body).Decode(&p); err != nil {
		t.Fatalf("解析 problem: %v", err)
	}
	return p
}

func doReq(t *testing.T, h http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set(HeaderKey, key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestFirstExecuteThenReplay(t *testing.T) {
	_, store := testStore(t)
	var runs atomic.Int32
	h := Middleware(store, slog.New(slog.DiscardHandler))(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			runs.Add(1)
			b, _ := io.ReadAll(r.Body) // 断言中间件回填了 body
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"echo":%s}`, b)
		}))

	first := doReq(t, h, http.MethodPost, "/pay", "key-1", `{"amount":42}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("首次 status = %d, want 201", first.Code)
	}
	if first.Header().Get(HeaderReplayed) != "" {
		t.Fatal("首次执行不应带 Idempotency-Replayed")
	}
	if runs.Load() != 1 {
		t.Fatalf("handler 执行次数 = %d, want 1", runs.Load())
	}
	if state, ok := rowState(t, store, "key-1"); !ok || state != StateCompleted {
		t.Fatalf("首次执行后记录 state = %q（found=%v）, want completed", state, ok)
	}

	replay := doReq(t, h, http.MethodPost, "/pay", "key-1", `{"amount":42}`)
	if runs.Load() != 1 {
		t.Fatalf("重放不应再执行 handler，执行次数 = %d", runs.Load())
	}
	if replay.Code != http.StatusCreated {
		t.Fatalf("重放 status = %d, want 201", replay.Code)
	}
	if got := replay.Header().Get(HeaderReplayed); got != "true" {
		t.Fatalf("Idempotency-Replayed = %q, want true", got)
	}
	if got := replay.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("重放 Content-Type = %q, want application/json", got)
	}
	if replay.Body.String() != first.Body.String() {
		t.Fatalf("重放 body = %q, want %q", replay.Body.String(), first.Body.String())
	}
}

func TestConflictResponses(t *testing.T) {
	_, store := testStore(t)
	ctx := context.Background()
	const path, body = "/pay", `{"amount":1}`
	hash := payloadHash(http.MethodPost, path, []byte(body))

	tests := []struct {
		name           string
		seed           func(t *testing.T, key string)
		wantStatus     int
		wantCode       string
		wantBody       string
		wantReplayed   bool
		wantRetryAfter string
	}{
		{
			name: "异payload一律422",
			seed: func(t *testing.T, key string) {
				other := payloadHash(http.MethodPost, path, []byte(`{"amount":999}`))
				if ok, _, _, err := store.Claim(ctx, key, other); err != nil || !ok {
					t.Fatalf("预置 claim: ok=%v err=%v", ok, err)
				}
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   apperr.CodeIdempotency,
		},
		{
			name: "in_progress未过期409",
			seed: func(t *testing.T, key string) {
				if ok, _, _, err := store.Claim(ctx, key, hash); err != nil || !ok {
					t.Fatalf("预置 claim: ok=%v err=%v", ok, err)
				}
			},
			wantStatus:     http.StatusConflict,
			wantCode:       apperr.CodeConflict,
			wantRetryAfter: "1",
		},
		{
			name: "completed回放存储响应",
			seed: func(t *testing.T, key string) {
				ok, token, _, err := store.Claim(ctx, key, hash)
				if err != nil || !ok {
					t.Fatalf("预置 claim: ok=%v err=%v", ok, err)
				}
				err = store.Complete(ctx, key, token, http.StatusAccepted,
					map[string][]string{"Content-Type": {"text/plain"}}, []byte("stored"))
				if err != nil {
					t.Fatalf("预置 complete: %v", err)
				}
			},
			wantStatus:   http.StatusAccepted,
			wantBody:     "stored",
			wantReplayed: true,
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var runs atomic.Int32
			h := Middleware(store, slog.New(slog.DiscardHandler))(
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					runs.Add(1)
				}))
			key := fmt.Sprintf("conflict-%d", i)
			tc.seed(t, key)

			rec := doReq(t, h, http.MethodPost, path, key, body)
			if runs.Load() != 0 {
				t.Fatalf("冲突路径不应执行 handler，执行次数 = %d", runs.Load())
			}
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantCode != "" {
				if p := decodeProblem(t, rec.Body); p.Code != tc.wantCode {
					t.Fatalf("problem code = %q, want %q", p.Code, tc.wantCode)
				}
			}
			if tc.wantBody != "" && rec.Body.String() != tc.wantBody {
				t.Fatalf("body = %q, want %q", rec.Body.String(), tc.wantBody)
			}
			if got := rec.Header().Get(HeaderReplayed) == "true"; got != tc.wantReplayed {
				t.Fatalf("replayed = %v, want %v", got, tc.wantReplayed)
			}
			if got := rec.Header().Get("Retry-After"); got != tc.wantRetryAfter {
				t.Fatalf("Retry-After = %q, want %q", got, tc.wantRetryAfter)
			}
		})
	}
}

// TestConcurrentSameKeyExecutesOnce 是核心竞态断言：两个并发同键请求，
// handler 只允许执行一次；另一个要么 409 要么拿到回放。
func TestConcurrentSameKeyExecutesOnce(t *testing.T) {
	_, store := testStore(t)
	var runs atomic.Int32
	h := Middleware(store, slog.New(slog.DiscardHandler))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			runs.Add(1)
			time.Sleep(300 * time.Millisecond) // 拉开窗口，保证另一请求撞上 in_progress
			_, _ = w.Write([]byte("done"))
		}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	type result struct {
		status   int
		replayed bool
		body     string
		err      error
	}
	results := make([]result, 2)
	var (
		start sync.WaitGroup
		done  sync.WaitGroup
	)
	start.Add(1)
	for i := range results {
		done.Go(func() {
			start.Wait()
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/pay", strings.NewReader(`{"amount":7}`))
			req.Header.Set(HeaderKey, "race-key")
			resp, err := srv.Client().Do(req)
			if err != nil {
				results[i] = result{err: err}
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			results[i] = result{
				status:   resp.StatusCode,
				replayed: resp.Header.Get(HeaderReplayed) == "true",
				body:     string(b),
			}
		})
	}
	start.Done()
	done.Wait()

	if got := runs.Load(); got != 1 {
		t.Fatalf("handler 执行次数 = %d, want 1", got)
	}
	var fresh, other int
	for _, r := range results {
		if r.err != nil {
			t.Fatalf("请求失败: %v", r.err)
		}
		switch {
		case r.status == http.StatusOK && !r.replayed:
			fresh++
			if r.body != "done" {
				t.Errorf("首执行 body = %q, want done", r.body)
			}
		case r.status == http.StatusConflict,
			r.status == http.StatusOK && r.replayed && r.body == "done":
			other++
		default:
			t.Errorf("意外结局: %+v", r)
		}
	}
	if fresh != 1 || other != 1 {
		t.Fatalf("结局分布 fresh=%d other=%d, want 1/1（%+v）", fresh, other, results)
	}
}

// TestStaleInProgressTakeover 验证超时的 in_progress claim 可被同 payload 请求接管。
func TestStaleInProgressTakeover(t *testing.T) {
	_, store := testStore(t)
	store = store.WithTTL(50 * time.Millisecond)
	const path, body = "/pay", `{"amount":3}`
	hash := payloadHash(http.MethodPost, path, []byte(body))

	// 模拟一次死掉的执行：claim 后永不 Complete。
	if ok, _, _, err := store.Claim(context.Background(), "dead-key", hash); err != nil || !ok {
		t.Fatalf("预置 claim: ok=%v err=%v", ok, err)
	}
	time.Sleep(120 * time.Millisecond)

	var runs atomic.Int32
	h := Middleware(store, slog.New(slog.DiscardHandler))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			runs.Add(1)
			_, _ = w.Write([]byte("recovered"))
		}))
	rec := doReq(t, h, http.MethodPost, path, "dead-key", body)

	if runs.Load() != 1 {
		t.Fatalf("接管后应执行 handler，执行次数 = %d", runs.Load())
	}
	if rec.Code != http.StatusOK || rec.Body.String() != "recovered" {
		t.Fatalf("接管执行响应 = %d %q, want 200 recovered", rec.Code, rec.Body.String())
	}
	if state, ok := rowState(t, store, "dead-key"); !ok || state != StateCompleted {
		t.Fatalf("接管完成后 state = %q（found=%v）, want completed", state, ok)
	}
}

// TestOversizeResponseReleasesClaim 验证超限响应不入库：claim 被释放，重试重新执行。
func TestOversizeResponseReleasesClaim(t *testing.T) {
	_, store := testStore(t)
	big := bytes.Repeat([]byte("x"), maxCaptureBytes+1)
	var runs atomic.Int32
	h := Middleware(store, slog.New(slog.DiscardHandler))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			runs.Add(1)
			_, _ = w.Write(big)
		}))

	first := doReq(t, h, http.MethodPost, "/big", "big-key", "b")
	if first.Code != http.StatusOK || first.Body.Len() != len(big) {
		t.Fatalf("首次响应 = %d %d 字节, want 200 %d 字节（客户端不受缓存上限影响）",
			first.Code, first.Body.Len(), len(big))
	}
	if _, found := rowState(t, store, "big-key"); found {
		t.Fatal("超限响应后 claim 应被释放（记录不存在）")
	}

	second := doReq(t, h, http.MethodPost, "/big", "big-key", "b")
	if second.Code != http.StatusOK {
		t.Fatalf("重试 status = %d, want 200", second.Code)
	}
	if got := runs.Load(); got != 2 {
		t.Fatalf("释放后重试应重新执行，执行次数 = %d, want 2", got)
	}
}

// TestOversizeRequestBodyRetrySameKey 验证超限 body 拒绝后不留 claim：
// 同 key 换合法 body 重试可正常执行，不会撞上 409/422。
func TestOversizeRequestBodyRetrySameKey(t *testing.T) {
	_, store := testStore(t)
	var runs atomic.Int32
	h := Middleware(store, slog.New(slog.DiscardHandler), WithMaxRequestBytes(8))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			runs.Add(1)
			_, _ = w.Write([]byte("ok"))
		}))

	first := doReq(t, h, http.MethodPost, "/pay", "retry-413", strings.Repeat("x", 9))
	if first.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限请求 status = %d, want 413", first.Code)
	}
	if _, found := rowState(t, store, "retry-413"); found {
		t.Fatal("超限请求不应留下 claim 记录")
	}

	second := doReq(t, h, http.MethodPost, "/pay", "retry-413", "ok")
	if second.Code != http.StatusOK || second.Body.String() != "ok" {
		t.Fatalf("重试响应 = %d %q, want 200 ok", second.Code, second.Body.String())
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("重试应正常执行 handler，执行次数 = %d, want 1", got)
	}
}

// TestTakeoverFencesLateComplete 是 fencing 的核心断言：持有者仍存活时因
// TTL 过期被接管，接管者先完成落库；迟到的原持有者 Complete 必须被拒绝，
// 库里与后续回放都是接管者的响应。
func TestTakeoverFencesLateComplete(t *testing.T) {
	_, store := testStore(t)
	store = store.WithTTL(100 * time.Millisecond)
	const path, body = "/pay", `{"amount":9}`

	var (
		runs    atomic.Int32
		started = make(chan struct{}) // 慢 handler 已开始（claim 已建立）
		unblock = make(chan struct{}) // 放行慢 handler
	)
	h := Middleware(store, slog.New(slog.DiscardHandler))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if runs.Add(1) == 1 {
				close(started)
				<-unblock
				_, _ = w.Write([]byte("slow-original"))
				return
			}
			_, _ = w.Write([]byte("takeover"))
		}))

	slowDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { slowDone <- doReq(t, h, http.MethodPost, path, "fence-key", body) }()
	<-started
	time.Sleep(250 * time.Millisecond) // 超过 TTL（数据库时钟判定，留余量）

	takeover := doReq(t, h, http.MethodPost, path, "fence-key", body)
	if takeover.Code != http.StatusOK || takeover.Body.String() != "takeover" {
		t.Fatalf("接管请求响应 = %d %q, want 200 takeover", takeover.Code, takeover.Body.String())
	}
	if takeover.Header().Get(HeaderReplayed) != "" {
		t.Fatal("接管请求是真实执行，不应带 Idempotency-Replayed")
	}

	// 放行原持有者：客户端仍收到已发出的响应，但落库被 fencing 拒绝。
	close(unblock)
	slow := <-slowDone
	if slow.Body.String() != "slow-original" {
		t.Fatalf("原持有者客户端 body = %q, want slow-original", slow.Body.String())
	}
	if got := runs.Load(); got != 2 {
		t.Fatalf("handler 执行次数 = %d, want 2", got)
	}

	var stored []byte
	err := store.pool.QueryRow(context.Background(),
		"SELECT body FROM "+store.tbl+" WHERE key = $1 AND state = 'completed'", "fence-key").Scan(&stored)
	if err != nil {
		t.Fatalf("查完成记录: %v", err)
	}
	if string(stored) != "takeover" {
		t.Fatalf("库里的响应 = %q, want takeover（迟到的 Complete 不得覆盖接管者）", stored)
	}

	replay := doReq(t, h, http.MethodPost, path, "fence-key", body)
	if replay.Header().Get(HeaderReplayed) != "true" || replay.Body.String() != "takeover" {
		t.Fatalf("回放 = replayed=%q body=%q, want true takeover",
			replay.Header().Get(HeaderReplayed), replay.Body.String())
	}
}

// TestReplayRestoresFullHeaders 验证回放恢复全量响应头（含多值头与 Location），
// hop-by-hop 头与 Set-Cookie 不落库。
func TestReplayRestoresFullHeaders(t *testing.T) {
	_, store := testStore(t)
	h := Middleware(store, slog.New(slog.DiscardHandler))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "/orders/42")
			w.Header().Add("Link", `</orders?page=1>; rel="first"`)
			w.Header().Add("Link", `</orders?page=2>; rel="next"`)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Set-Cookie", "sid=secret")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":42}`))
		}))

	first := doReq(t, h, http.MethodPost, "/orders", "hdr-key", `{"amount":5}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("首次 status = %d, want 201", first.Code)
	}

	replay := doReq(t, h, http.MethodPost, "/orders", "hdr-key", `{"amount":5}`)
	if replay.Code != http.StatusCreated {
		t.Fatalf("回放 status = %d, want 201", replay.Code)
	}
	if replay.Header().Get(HeaderReplayed) != "true" {
		t.Fatal("回放应带 Idempotency-Replayed: true")
	}
	if got := replay.Header().Get("Location"); got != "/orders/42" {
		t.Fatalf("回放 Location = %q, want /orders/42", got)
	}
	wantLink := []string{`</orders?page=1>; rel="first"`, `</orders?page=2>; rel="next"`}
	if got := replay.Header().Values("Link"); !slices.Equal(got, wantLink) {
		t.Fatalf("回放 Link = %v, want %v（多值头需全量恢复）", got, wantLink)
	}
	if got := replay.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("回放 Content-Type = %q, want application/json", got)
	}
	if got := replay.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Fatalf("Set-Cookie 不应被回放，got %v", got)
	}
	if got := replay.Header().Values("Connection"); len(got) != 0 {
		t.Fatalf("hop-by-hop 头 Connection 不应被回放，got %v", got)
	}
	if replay.Body.String() != first.Body.String() {
		t.Fatalf("回放 body = %q, want %q", replay.Body.String(), first.Body.String())
	}
}

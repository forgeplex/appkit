// Package idem 实现 Stripe 式幂等键 HTTP 中间件。
//
// 核心是防双重执行竞态：执行 handler 之前先以独立短事务
// INSERT ... ON CONFLICT DO NOTHING 抢占（claim）幂等键——抢到唯一执行权才执行，
// 完成后把响应存回记录，后续同键请求回放存储的响应。执行中断（panic/断连/进程死亡）
// 留下的 in_progress 记录超过 TTL 后允许被接管重试；接管会更换记录的 owner_token
// 作 fencing——原持有者迟到的 Complete/Release 会被拒绝，不会覆盖接管者。
// 业务表的 UNIQUE 约束是最后一道兜底（见 docs/DESIGN.md §6）。
//
// 默认指纹绑定原始字节；领域有规范化口径时经 WithCanonicalizer 注入，多租户/
// 多账本的键空间隔离经 WithKeyScope 注入——都是加法，默认行为不变。
package idem

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forgeplex/appkit/apperr"
)

const (
	// HeaderKey 是客户端声明幂等键的请求头；缺失时中间件直接放行。
	HeaderKey = "Idempotency-Key"
	// HeaderReplayed 标记响应来自存储回放，而非本次真实执行。
	HeaderReplayed = "Idempotency-Replayed"

	// DefaultTTL 是 in_progress 记录的接管时限：超时仍未完成的 claim
	// 视为持有者已死，后续同键同 payload 的请求可以接管重试。
	//
	// TTL 必须大于 handler 的最长执行时间：持有者仍存活时被接管，
	// fencing 会保证它迟到的落库被拒绝（记录不被污染），但业务副作用
	// 可能已经发生两次，只能靠业务表 UNIQUE 约束兜底。
	DefaultTTL = 60 * time.Second

	// DefaultMaxRequestBytes 是带幂等键请求的请求体上限。中间件必须整读
	// body 计算指纹，无上限即放任恶意大 body 耗尽内存。
	DefaultMaxRequestBytes int64 = 1 << 20

	// maxCaptureBytes 是响应缓存上限。超限的响应无法完整回放，
	// 完成时改为释放 claim 让重试重新执行，绝不回放截断的响应。
	maxCaptureBytes = 1 << 20
)

// 记录状态。claim 抢到即 in_progress，响应存回后转为 completed（终态）。
const (
	StateInProgress = "in_progress"
	StateCompleted  = "completed"
)

// MigrationSQL 返回幂等表的建表语句，供域 repo 嵌入自己 schema 的首个迁移
// （DESIGN §8：outbox/inbox/幂等/审计表每 schema 一套）。schema 需已存在。
func MigrationSQL(schema string) string {
	tbl := pgx.Identifier{schema, "idempotency_keys"}.Sanitize()
	return `CREATE TABLE IF NOT EXISTS ` + tbl + ` (
    key          text PRIMARY KEY,
    payload_hash bytea NOT NULL,
    owner_token  uuid NOT NULL,
    state        text NOT NULL CHECK (state IN ('in_progress', 'completed')),
    status       int,
    headers      jsonb,
    body         bytea,
    created_at   timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz
);

COMMENT ON TABLE ` + tbl + ` IS '幂等键：claim 先行占位防双重执行，完成后缓存响应供重放。';
COMMENT ON COLUMN ` + tbl + `.payload_hash IS '同 key 异 payload 判 422 的依据。';
COMMENT ON COLUMN ` + tbl + `.owner_token IS 'claim 的持有者，超时接管时据此判断归属。';`
}

// Store 是幂等记录的存取层。每个操作都是独立短事务（单语句自动提交），
// 绝不参与业务事务——claim 必须在业务执行前就对并发请求可见。
type Store struct {
	pool *pgxpool.Pool
	tbl  string
	ttl  time.Duration
}

// NewStore 构造 Store，接管时限为 DefaultTTL。
func NewStore(pool *pgxpool.Pool, schema string) *Store {
	return &Store{
		pool: pool,
		tbl:  pgx.Identifier{schema, "idempotency_keys"}.Sanitize(),
		ttl:  DefaultTTL,
	}
}

// WithTTL 调整 in_progress 记录的接管时限，返回 s 本身便于链式构造。
// ttl 必须大于 handler 的最长执行时间（见 DefaultTTL 的说明）。
func (s *Store) WithTTL(ttl time.Duration) *Store {
	s.ttl = ttl
	return s
}

// Record 是没抢到 claim 时返回的现存记录，供调用方决定 422/回放/409。
type Record struct {
	PayloadHash []byte
	State       string
	Status      int
	Headers     map[string][]string
	Body        []byte
}

// newOwnerToken 生成 fencing 令牌（UUIDv4 文本）。crypto/rand.Read 自
// Go 1.24 起保证不返回错误。
func newOwnerToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Claim 尝试抢占 key。返回 claimed=true 表示本请求获得唯一执行权，token 是
// 本次持有权的 fencing 令牌，Complete/Release 时必须回传；否则返回现存记录。
// 陈旧 in_progress（超过 TTL）且 payload 相同的记录会被原子接管——UPDATE 自带
// state 与时限条件并写入新 token，多个接管者只有一个能赢。
func (s *Store) Claim(ctx context.Context, key string, payloadHash []byte) (claimed bool, token string, existing *Record, err error) {
	// 有限重试覆盖两个窄竞态窗口：INSERT 冲突后记录恰被 Release（重新可抢）、
	// 接管 UPDATE 被并发者抢先（重读后按新状态判定）。
	for range 3 {
		tok := newOwnerToken()
		tag, err := s.pool.Exec(ctx,
			`INSERT INTO `+s.tbl+` (key, payload_hash, state, owner_token) VALUES ($1, $2, 'in_progress', $3) ON CONFLICT (key) DO NOTHING`,
			key, payloadHash, tok)
		if err != nil {
			return false, "", nil, fmt.Errorf("idem: claim 插入: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return true, tok, nil, nil
		}

		rec, stale, err := s.get(ctx, key)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, "", nil, err
		}
		if rec.State == StateInProgress && stale && bytes.Equal(rec.PayloadHash, payloadHash) {
			tag, err := s.pool.Exec(ctx,
				`UPDATE `+s.tbl+` SET created_at = now(), owner_token = $3
				 WHERE key = $1 AND state = 'in_progress' AND created_at < now() - make_interval(secs => $2)`,
				key, s.ttl.Seconds(), tok)
			if err != nil {
				return false, "", nil, fmt.Errorf("idem: claim 接管: %w", err)
			}
			if tag.RowsAffected() == 1 {
				return true, tok, nil, nil
			}
			continue
		}
		return false, "", rec, nil
	}
	return false, "", nil, fmt.Errorf("idem: 键 %q 的 claim 竞态重试耗尽", key)
}

// get 读取记录；stale 的判定用数据库时钟（与接管 UPDATE 的条件同源），
// 避免应用与数据库时钟偏差导致预判与接管条件不一致。
func (s *Store) get(ctx context.Context, key string) (*Record, bool, error) {
	var (
		rec   Record
		stale bool
	)
	err := s.pool.QueryRow(ctx,
		`SELECT payload_hash, state, COALESCE(status, 0), COALESCE(headers, '{}'::jsonb), body,
		        created_at < now() - make_interval(secs => $2)
		 FROM `+s.tbl+` WHERE key = $1`,
		key, s.ttl.Seconds()).
		Scan(&rec.PayloadHash, &rec.State, &rec.Status, &rec.Headers, &rec.Body, &stale)
	if err != nil {
		return nil, false, fmt.Errorf("idem: 读取记录: %w", err)
	}
	return &rec, stale, nil
}

// Complete 把响应存回记录并转为 completed。owner_token 是 fencing 条件：
// claim 已被接管（token 已更换）或已完成时更新 0 行，返回错误由调用方记日志，
// 绝不覆盖接管者的记录。
func (s *Store) Complete(ctx context.Context, key, token string, status int, headers map[string][]string, body []byte) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE `+s.tbl+` SET state = 'completed', status = $3, headers = $4, body = $5, completed_at = now()
		 WHERE key = $1 AND state = 'in_progress' AND owner_token = $2`,
		key, token, status, headers, body)
	if err != nil {
		return fmt.Errorf("idem: 存储响应: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("idem: 键 %q 的 claim 已易主或已完成，fencing 拒绝落库", key)
	}
	return nil
}

// Release 删除仍由 token 持有且在 in_progress 的 claim，让后续重试重新执行。
// owner_token 是 fencing 条件：已被接管或已完成时删除 0 行，返回错误由调用方
// 记日志，绝不误删接管者的 claim。
func (s *Store) Release(ctx context.Context, key, token string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM `+s.tbl+` WHERE key = $1 AND state = 'in_progress' AND owner_token = $2`, key, token)
	if err != nil {
		return fmt.Errorf("idem: 释放 claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("idem: 键 %q 的 claim 已易主或已完成，fencing 拒绝释放", key)
	}
	return nil
}

// payloadHash 是幂等键绑定的请求指纹：method + path + body。
// 查询串不参与——幂等键的语义是「同一操作的重试」。分隔符防止字段间歧义拼接。
func payloadHash(method, path string, body []byte) []byte {
	h := sha256.New()
	io.WriteString(h, method)
	io.WriteString(h, "\n")
	io.WriteString(h, path)
	io.WriteString(h, "\n")
	h.Write(body)
	return h.Sum(nil)
}

var (
	errPayloadMismatch = apperr.New(apperr.CodeIdempotency, http.StatusUnprocessableEntity,
		"同一 Idempotency-Key 携带了不同的请求内容")
	errInFlight = apperr.New(apperr.CodeConflict, http.StatusConflict,
		"同一 Idempotency-Key 的请求正在处理中")
	errBodyTooLarge = apperr.New(apperr.CodeInvalidArgument, http.StatusRequestEntityTooLarge,
		"请求体超过幂等中间件允许的上限")
	errKeyPartInvalid = apperr.New(apperr.CodeInvalidArgument, http.StatusBadRequest,
		"幂等键或作用域含控制字符（HTTP 头值不允许，RFC 7230）")
)

// Option 调整 Middleware 的行为。
type Option func(*mwConfig)

// mwConfig 经导出的 Option 签名泄漏进 API 面，字段类型受 apidiff 约束：
// 函数不可比较，平铺进来会把整个结构变成不可比较类型（相对存量 tag 即
// incompatible），所以注入函数收在指针后面的 injectedOptions 里。
type mwConfig struct {
	maxRequestBytes int64
	injected        *injectedOptions
}

type injectedOptions struct {
	canonicalizer Canonicalizer
	keyScope      KeyScope
}

// WithMaxRequestBytes 调整带幂等键请求的请求体上限（默认 DefaultMaxRequestBytes）。
func WithMaxRequestBytes(n int64) Option {
	return func(c *mwConfig) { c.maxRequestBytes = n }
}

// Canonicalizer 把请求体规范化为幂等指纹的输入材料。默认恒等（原始字节），
// 因而对字节敏感：客户端重试时重新序列化（金额 "80" 变 "80.00"、字段顺序或
// 空白变化），同键会被判为异 payload 而 422。领域已有规范化口径时（入站 DTO
// 走 money.ParseCanonical）把它接到这里，等值形态的重试即可拿到回放。
//
// 返回值由中间件与 method、path 一起过 sha256（分隔符防歧义拼接，见
// payloadHash），实现方不必自己造哈希纪律，只须保证：等值请求得到相同字节。
// 返回错误即在 claim 之前拒绝请求（*apperr.Error 原样透传，其余按 400 包装），
// 不留悬挂的 in_progress 记录。求值时 r.Body 已回填为完整请求体，可再读。
type Canonicalizer func(r *http.Request, body []byte) ([]byte, error)

// WithCanonicalizer 替换指纹的规范化口径（默认恒等）。
//
// 换口径是单向门：存量记录的 payload_hash 全部失配，而 completed 记录不过期，
// 旧键会持续 422 直到客户端换键。只在接入时选一次，别来回换。
func WithCanonicalizer(fn Canonicalizer) Option {
	return func(c *mwConfig) { c.inject().canonicalizer = fn }
}

// KeyScope 从请求解析幂等键的作用域（租户、账本、机构……），返回 "" 表示
// 本请求不设作用域、键原样使用。在 body 读取之前求值——fn 里读 r.Body 是
// 错的（那时还没读）。返回错误即在 claim 之前拒绝请求（透传规则同
// Canonicalizer）。
type KeyScope func(r *http.Request, key string) (string, error)

// keyScopeSep 是作用域与键的分隔符：ASCII unit separator（0x1f）。
// 不用 ":"——键来自请求头、作用域常来自路径或业务标识，两边都可能含 ":"；
// 不用 NUL——Postgres 的 text 类型容不下 0x00。0x1f 在 RFC 7230 的头值里
// 非法，配置了作用域的中间件会拒绝含控制字节的键与作用域（见 validKeyPart），
// 于是两边谁都拼不出这个分隔符，跨作用域碰撞不存在。
const keyScopeSep = "\x1f"

// WithKeyScope 给幂等键加作用域：实际存储键为 scope + 0x1f + 原键。
// 作用域之间键空间互不可见——不同租户的同名键不再是假冲突，也不能靠
// 回放/409 的响应差异探测别的作用域是否用过某键。比手拼 "{tenant}:"
// 前缀多出的这点确定性（前缀可拼造碰撞），就是这个选项存在的理由。
func WithKeyScope(fn KeyScope) Option {
	return func(c *mwConfig) { c.inject().keyScope = fn }
}

// inject 惰性建 injectedOptions，让 Option 不依赖构造顺序。
func (c *mwConfig) inject() *injectedOptions {
	if c.injected == nil {
		c.injected = &injectedOptions{}
	}
	return c.injected
}

// validKeyPart 报告键或作用域可安全参与拼接：不含控制字节与 DEL。
// 这是作用域分隔符不可伪造性的来源，也是 Postgres text 的安全区。
func validKeyPart(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return false
		}
	}
	return true
}

// identityCanonicalizer 是默认口径：原始字节，不加不减。
var identityCanonicalizer Canonicalizer = func(_ *http.Request, body []byte) ([]byte, error) {
	return body, nil
}

// rejectInjectedErr 把注入函数（规范化/作用域）的错误写为 problem 响应：
// *apperr.Error 原样透传（实现方可自定状态码），其余按 400 包一层。
func rejectInjectedErr(w http.ResponseWriter, err error, message string) {
	if ae, ok := errors.AsType[*apperr.Error](err); ok {
		apperr.WriteProblem(w, ae)
		return
	}
	apperr.WriteProblem(w, apperr.New(apperr.CodeInvalidArgument,
		http.StatusBadRequest, message).WithCause(err))
}

// nonReplayableHeader 报告响应头是否不落库回放：hop-by-hop 头随连接而非
// 响应存在，Set-Cookie 携带会话凭据，回放给后来的请求都是错的。
var nonReplayableHeader = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Set-Cookie":          true,
}

// Middleware 返回幂等中间件，置于 Recover 之内、业务 handler 之外
// （DESIGN §6 的链位）。无 Idempotency-Key 头的请求直接放行。
func Middleware(store *Store, log *slog.Logger, opts ...Option) func(http.Handler) http.Handler {
	cfg := mwConfig{
		maxRequestBytes: DefaultMaxRequestBytes,
		injected:        &injectedOptions{canonicalizer: identityCanonicalizer},
	}
	for _, o := range opts {
		o(&cfg)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(HeaderKey)
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}

			// 作用域在 body 读取之前求值（见 KeyScope）。键与作用域先过
			// 控制字节校验（分隔符不可伪造的前提），错误在 claim 之前拒绝。
			if inj := cfg.injected; inj != nil && inj.keyScope != nil {
				if !validKeyPart(key) {
					apperr.WriteProblem(w, errKeyPartInvalid)
					return
				}
				scope, err := inj.keyScope(r, key)
				if err != nil {
					rejectInjectedErr(w, err, "幂等键作用域解析失败")
					return
				}
				if scope != "" {
					if !validKeyPart(scope) {
						apperr.WriteProblem(w, errKeyPartInvalid)
						return
					}
					key = scope + keyScopeSep + key
				}
			}

			// 指纹计算必须整读 body，上限防止恶意大 body 耗尽内存。
			// 超限发生在 claim 之前，413 后同 key 重试可正常重新执行。
			r.Body = http.MaxBytesReader(w, r.Body, cfg.maxRequestBytes)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
					apperr.WriteProblem(w, errBodyTooLarge)
					return
				}
				apperr.WriteProblem(w, apperr.New(apperr.CodeInvalidArgument,
					http.StatusBadRequest, "无法读取请求体").WithCause(err))
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			canonical, err := cfg.injected.canonicalizer(r, body)
			if err != nil {
				rejectInjectedErr(w, err, "请求体规范化失败")
				return
			}
			hash := payloadHash(r.Method, r.URL.Path, canonical)

			claimed, token, existing, err := store.Claim(r.Context(), key, hash)
			if err != nil {
				apperr.WriteProblem(w, apperr.Unavailable(fmt.Errorf("idem: claim: %w", err)))
				return
			}
			if !claimed {
				replyExisting(w, existing, hash)
				return
			}

			cw := &captureWriter{ResponseWriter: w, limit: maxCaptureBytes}
			// handler panic 原样上抛（外层 Recover 负责响应）：不走下面的
			// Complete，记录保持 in_progress，超过 TTL 后由后续重试接管。
			next.ServeHTTP(cw, r)

			// 响应已发出，落库失败只能记日志。WithoutCancel：handler 正常
			// 完成后客户端断连不应把已产生的业务结果丢成 in_progress。
			cctx := context.WithoutCancel(r.Context())
			if cw.overflow {
				log.Warn("idem: 响应超过缓存上限，释放 claim（重试将重新执行）", "key", key)
				if err := store.Release(cctx, key, token); err != nil {
					log.Error("idem: 释放 claim 失败（可能已被接管）", "key", key, "err", err)
				}
				return
			}
			status := cw.status
			if status == 0 {
				// handler 一字未写：net/http 在返回后隐式发 200。
				status = http.StatusOK
			}
			headers := map[string][]string{}
			for k, vs := range cw.Header() {
				if nonReplayableHeader[k] {
					continue
				}
				headers[k] = vs
			}
			if err := store.Complete(cctx, key, token, status, headers, cw.buf.Bytes()); err != nil {
				log.Error("idem: 存储响应失败（fencing 拒绝或数据库错误），本次响应不会被回放",
					"key", key, "err", err)
			}
		})
	}
}

// replyExisting 处理没抢到 claim 的三种结局。payload 指纹先于状态判定：
// 同键异 payload 是客户端错误，无论现存记录处于什么状态。
func replyExisting(w http.ResponseWriter, rec *Record, hash []byte) {
	switch {
	case !bytes.Equal(rec.PayloadHash, hash):
		apperr.WriteProblem(w, errPayloadMismatch)
	case rec.State == StateCompleted:
		for k, vs := range rec.Headers {
			w.Header()[http.CanonicalHeaderKey(k)] = vs
		}
		w.Header().Set(HeaderReplayed, "true")
		status := rec.Status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write(rec.Body)
	default:
		w.Header().Set("Retry-After", "1")
		apperr.WriteProblem(w, errInFlight)
	}
}

// captureWriter 边透传边缓存响应。超过 limit 后放弃缓存（客户端不受影响），
// 由调用方释放 claim——宁可让重试重新执行，也不存下无法完整回放的响应。
type captureWriter struct {
	http.ResponseWriter
	status   int
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (w *captureWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *captureWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if !w.overflow {
		if w.buf.Len()+len(b) > w.limit {
			w.overflow = true
			w.buf.Reset()
		} else {
			w.buf.Write(b)
		}
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap 供 http.ResponseController 透传 Flush/Hijack 等能力。
func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *captureWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

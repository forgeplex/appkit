// Package page 是列表端点的分页机制件：参数解析与校验、游标编解码、
// 统一响应信封、「多取一行判下一页」的标准技巧。
//
// 机制归框架、语义归域（同权限/租户的切分）：
//   - 机制（本包）：?limit 解析与值域校验（畸形即 422，不静默裁剪——
//     ?limit=10⁶ 被悄悄压成 50 条时客户端以为拿到了全部）、游标的不透明
//     编解码（JSON → base64url）、items + next_cursor 信封、Trim 的
//     limit+1 技巧。
//   - 语义（域）：cursor 结构体定义（排序键是什么每表不同）、sqlc 里的
//     keyset WHERE 子句（框架不碰 SQL 拼接）、要不要 total。排序键必须
//     唯一——时间戳做键必加 id 兜住同刻并列，否则翻页在并列处漏行。
//
// 游标对客户端是不透明契约：只回传、不解析、不构造。编码不签名：伪造
// 游标最多翻到别的页（keyset WHERE 照样跑全量过滤），不是安全边界，
// 不值得密钥管理成本。
package page

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/forgeplex/appkit/apperr"
)

const (
	// DefaultLimit 是 ?limit 缺省时的页大小。
	DefaultLimit = 50
	// MaxLimit 是 ?limit 的上限：无上限即放任 ?limit=1000000 把宽表整段
	// 拖进内存，是每个列表端点自带的 DoS 面。
	MaxLimit = 200
)

// Params 是 Parse 的产物。Limit 已校验归一（缺省用默认值，值域内原样）；
// Cursor 是原始字符串，形状校验留给 Decode[T]——类型在域手里，Parse 没有
// 类型参数。
type Params struct {
	Limit  int
	Cursor string
}

// Option 调整 Parse 的值域。宽窄行的合理页大小不同（审计流水 20、窄日志
// 200），用默认值域覆盖不了的列表再定制。
type Option func(*config)

type config struct {
	def int
	max int
}

// WithDefault 设置 ?limit 缺省时的页大小。
func WithDefault(n int) Option { return func(c *config) { c.def = n } }

// WithMax 设置 ?limit 的上限。n 大于等于默认值才有意义，但这里不拦——
// 配置错是启动期一眼能看见的事，不值得 API 面多一个错误分支。
func WithMax(n int) Option { return func(c *config) { c.max = n } }

// Parse 从查询串解析分页参数（参数名固定 limit 与 cursor——参数名漂移
// 正是本包要消灭的）。畸形值一律 422 INVALID_ARGUMENT 而非静默纠正：
// 超上限被裁剪时客户端以为拿到了要的页，响亮失败让写错的一眼看见
// （detail 带 field/value 与上限）。游标非空时原样透传，由域经 Decode
// 验形状后使用。
func Parse(r *http.Request, opts ...Option) (Params, error) {
	cfg := config{def: DefaultLimit, max: MaxLimit}
	for _, o := range opts {
		o(&cfg)
	}
	p := Params{Limit: cfg.def, Cursor: r.URL.Query().Get("cursor")}
	s := r.URL.Query().Get("limit")
	if s == "" {
		return p, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return Params{}, apperr.InvalidArgument("分页参数 limit 必须是整数").
			WithDetail("field", "limit").
			WithDetail("value", s)
	}
	if n <= 0 || n > cfg.max {
		return Params{}, apperr.InvalidArgument("分页参数 limit 超出值域 [1, %d]", cfg.max).
			WithDetail("field", "limit").
			WithDetail("value", s).
			WithDetail("max", cfg.max)
	}
	p.Limit = n
	return p, nil
}

// Encode 把游标值编码为不透明字符串：JSON 序列化后 base64url（无 padding，
// 可直接进 query string）。域通常把排序键直接放进返回行，编码行本身即可。
func Encode[T any](v T) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("page: 编码游标: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Decode 解开 Encode 的产物。坏 base64、坏 JSON 都返回 422 INVALID_ARGUMENT
// （客户端构造了不该构造的东西），调用方原样透传即可。JSON 合法但字段值
// 非法（如坏 uuid）不在此层——值域是域的知识，域 Decode 后自校验并自包 422。
func Decode[T any](s string) (T, error) {
	var zero T
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return zero, apperr.InvalidArgument("分页游标不是合法的编码").
			WithDetail("field", "cursor").WithCause(err)
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return zero, apperr.InvalidArgument("分页游标不是合法的结构").
			WithDetail("field", "cursor").WithCause(err)
	}
	return v, nil
}

// List 是列表端点的统一响应信封：items + next_cursor。NextCursor 为 nil
// （字段缺席）即最后一页；有下一页时是末行游标的 Encode 产物，客户端
// 原样回传 ?cursor=。
type List[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor,omitempty"`
}

// Trim 落实「limit+1 多取一行」技巧：查询用 limit+1 取回，len(rows) 超过
// limit 即有下一页。返回截断后的 items 与下一页游标锚点 next——锚点是
// 返回的末行（不是被截掉的那行：keyset (排序键) < 游标 从末行之后继续，
// 用被截行会跳过整个本页末尾）。next 非 nil 时由调用方 Encode 进信封。
func Trim[T any](rows []T, limit int) (items []T, next *T) {
	if len(rows) <= limit {
		return rows, nil
	}
	items = rows[:limit]
	return items, &rows[limit-1]
}

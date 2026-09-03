package page_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/page"
)

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		query     string
		opts      []page.Option
		wantLimit int
		wantErr   bool
	}{
		{name: "缺省用默认值", query: "", wantLimit: page.DefaultLimit},
		{name: "正常值", query: "?limit=20", wantLimit: 20},
		{name: "上限边界内", query: "?limit=200", wantLimit: 200},
		{name: "超上限拒绝", query: "?limit=201", wantErr: true},
		{name: "超大值拒绝", query: "?limit=1000000", wantErr: true},
		{name: "零拒绝", query: "?limit=0", wantErr: true},
		{name: "负数拒绝", query: "?limit=-5", wantErr: true},
		{name: "非整数拒绝", query: "?limit=abc", wantErr: true},
		{name: "浮点拒绝", query: "?limit=1.5", wantErr: true},
		{name: "自定义默认值", query: "", opts: []page.Option{page.WithDefault(20)}, wantLimit: 20},
		{name: "自定义上限", query: "?limit=500", opts: []page.Option{page.WithMax(500)}, wantLimit: 500},
		{name: "自定义上限仍然拒绝超限", query: "?limit=501", opts: []page.Option{page.WithMax(500)}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/things"+tc.query, nil)
			p, err := page.Parse(r, tc.opts...)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse 应当报错，得到 %+v", p)
				}
				if !apperr.Is(err, apperr.CodeInvalidArgument) {
					t.Fatalf("错误码 = %v, want %s", err, apperr.CodeInvalidArgument)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if p.Limit != tc.wantLimit {
				t.Errorf("Limit = %d, want %d", p.Limit, tc.wantLimit)
			}
		})
	}
}

func TestParseCursorPassthrough(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/things?cursor=whatever%C2%A3", nil)
	p, err := page.Parse(r)
	if err != nil {
		t.Fatal(err)
	}
	if p.Cursor != "whatever£" {
		t.Fatalf("Cursor = %q, want 原样透传", p.Cursor)
	}
}

func TestParseErrorDetails(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/things?limit=9999", nil)
	_, err := page.Parse(r)
	ae, ok := errors.AsType[*apperr.Error](err)
	if !ok {
		t.Fatalf("错误应是 *apperr.Error，得到 %T", err)
	}
	if ae.Status() != 422 {
		t.Errorf("状态码 = %d, want 422", ae.Status())
	}
	d := ae.Details()
	if d["field"] != "limit" || d["value"] != "9999" || d["max"] != page.MaxLimit {
		t.Errorf("detail 应带 field/value/max，得到 %v", d)
	}
}

// row 模拟「排序键就在返回行里」的常见形态：keyset 列即游标内容。
type row struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

func TestEncodeDecodeRoundtrip(t *testing.T) {
	t.Parallel()
	cur := row{CreatedAt: time.Date(2026, 9, 3, 12, 0, 0, 123456789, time.UTC), ID: "0192abcd"}
	s, err := page.Encode(cur)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(s, "+/=") {
		t.Errorf("游标应可直接进 query（base64url 无 padding），得到 %q", s)
	}
	got, err := page.Decode[row](s)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(cur.CreatedAt) || got.ID != cur.ID {
		t.Fatalf("roundtrip 丢失: %+v, want %+v", got, cur)
	}
	// 纳秒精度必须无损：游标锚点抖动会在同刻并列处漏行。
	if got.CreatedAt.Nanosecond() != 123456789 {
		t.Errorf("纳秒精度丢失: %v", got.CreatedAt)
	}
}

func TestDecodeBadCursor(t *testing.T) {
	t.Parallel()
	// 合法 base64url 但不是 JSON。
	badJSON, _ := page.Encode("不是结构")
	tests := []struct {
		name   string
		cursor string
	}{
		{name: "坏 base64", cursor: "!!!not-base64!!!"},
		{name: "合法编码但坏 JSON", cursor: badJSON},
		{name: "空串", cursor: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := page.Decode[row](tc.cursor)
			if !apperr.Is(err, apperr.CodeInvalidArgument) {
				t.Fatalf("错误码 = %v, want %s", err, apperr.CodeInvalidArgument)
			}
		})
	}
}

func TestTrim(t *testing.T) {
	t.Parallel()
	mk := func(n int) []row {
		rows := make([]row, 0, n)
		for i := range n {
			rows = append(rows, row{ID: strings.Repeat("a", i+1)})
		}
		return rows
	}
	tests := []struct {
		name      string
		rows      int
		limit     int
		wantItems int
		wantNext  bool
	}{
		{name: "恰好一页无更多", rows: 3, limit: 3, wantItems: 3, wantNext: false},
		{name: "多取一行判有下一页", rows: 4, limit: 3, wantItems: 3, wantNext: true},
		{name: "不足一页", rows: 2, limit: 3, wantItems: 2, wantNext: false},
		{name: "空结果", rows: 0, limit: 3, wantItems: 0, wantNext: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows := mk(tc.rows)
			items, next := page.Trim(rows, tc.limit)
			if len(items) != tc.wantItems {
				t.Errorf("len(items) = %d, want %d", len(items), tc.wantItems)
			}
			if (next != nil) != tc.wantNext {
				t.Fatalf("next = %v, want 有值=%v", next, tc.wantNext)
			}
			if next != nil {
				// 游标锚点是返回的末行，不是被截掉的那行。
				want := items[len(items)-1]
				if next.ID != want.ID {
					t.Errorf("游标锚点 = %v, want 末行 %v", next.ID, want.ID)
				}
			}
		})
	}
}

// TestListEndpointE2E 走完整链路（httptest，无 DB）：Parse → Decode →
// keyset 过滤（内存模拟）→ Trim → Encode → List 信封。验证翻页到底、
// 422 problem 形态。
func TestListEndpointE2E(t *testing.T) {
	t.Parallel()
	// 五行数据，limit=2：三页，第三页只一行、next_cursor 缺席。
	data := []row{
		{CreatedAt: time.Date(2026, 9, 3, 12, 0, 5, 0, time.UTC), ID: "e"},
		{CreatedAt: time.Date(2026, 9, 3, 12, 0, 4, 0, time.UTC), ID: "d"},
		{CreatedAt: time.Date(2026, 9, 3, 12, 0, 3, 0, time.UTC), ID: "c"},
		{CreatedAt: time.Date(2026, 9, 3, 12, 0, 2, 0, time.UTC), ID: "b"},
		{CreatedAt: time.Date(2026, 9, 3, 12, 0, 1, 0, time.UTC), ID: "a"},
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		params, err := page.Parse(r)
		if err != nil {
			apperr.WriteProblem(w, err)
			return
		}
		var cur row
		if params.Cursor != "" {
			if cur, err = page.Decode[row](params.Cursor); err != nil {
				apperr.WriteProblem(w, err)
				return
			}
		}
		// keyset：取排序键严格小于游标（游标为空即从头）的行，limit+1 多取一行。
		fetched := make([]row, 0, params.Limit+1)
		for _, rw := range data {
			if !cur.CreatedAt.IsZero() && !rw.CreatedAt.Before(cur.CreatedAt) {
				continue
			}
			if len(fetched) == params.Limit+1 {
				break
			}
			fetched = append(fetched, rw)
		}
		items, next := page.Trim(fetched, params.Limit)
		resp := page.List[map[string]string]{Items: make([]map[string]string, 0, len(items))}
		for _, it := range items {
			resp.Items = append(resp.Items, map[string]string{"id": it.ID})
		}
		if next != nil {
			c, err := page.Encode(*next)
			if err != nil {
				apperr.WriteProblem(w, apperr.Internal(err))
				return
			}
			resp.NextCursor = &c
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	get := func(t *testing.T, query string) (body string, status int) {
		t.Helper()
		resp, err := http.Get(srv.URL + "/things" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(b), resp.StatusCode
	}

	// 第一页。
	body, status := get(t, "?limit=2")
	if status != 200 {
		t.Fatalf("状态码 = %d, want 200（body: %s）", status, body)
	}
	var p1 struct {
		Items      []map[string]string `json:"items"`
		NextCursor *string             `json:"next_cursor"`
	}
	if err := json.Unmarshal([]byte(body), &p1); err != nil {
		t.Fatal(err)
	}
	if len(p1.Items) != 2 || p1.Items[0]["id"] != "e" || p1.Items[1]["id"] != "d" {
		t.Fatalf("第一页 items = %v", p1.Items)
	}
	if p1.NextCursor == nil {
		t.Fatal("第一页应有 next_cursor")
	}

	// 用游标续翻两页到底。
	cursor := *p1.NextCursor
	pages := 1
	for {
		body, status = get(t, "?limit=2&cursor="+cursor)
		if status != 200 {
			t.Fatalf("续翻状态码 = %d（body: %s）", status, body)
		}
		var pn struct {
			Items      []map[string]string `json:"items"`
			NextCursor *string             `json:"next_cursor"`
		}
		if err := json.Unmarshal([]byte(body), &pn); err != nil {
			t.Fatal(err)
		}
		pages++
		if pn.NextCursor == nil {
			if len(pn.Items) != 1 || pn.Items[0]["id"] != "a" {
				t.Fatalf("末页 items = %v, want 仅剩 a", pn.Items)
			}
			break
		}
		cursor = *pn.NextCursor
		if pages > 5 {
			t.Fatal("翻页未终止")
		}
	}
	if pages != 3 {
		t.Errorf("共翻 %d 页, want 3", pages)
	}

	// 畸形游标：422 problem + 错误码。
	body, status = get(t, "?limit=2&cursor=%21%21")
	if status != 422 {
		t.Fatalf("畸形游标状态码 = %d, want 422（body: %s）", status, body)
	}
	if !strings.Contains(body, apperr.CodeInvalidArgument) {
		t.Errorf("422 响应应带错误码 %s: %s", apperr.CodeInvalidArgument, body)
	}
	// 畸形 limit 同理。
	_, status = get(t, "?limit=abc")
	if status != 422 {
		t.Fatalf("畸形 limit 状态码 = %d, want 422", status)
	}
}

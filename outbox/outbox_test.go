package outbox_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/outbox"
	"github.com/forgeplex/appkit/tx"
)

func panics(fn func()) (p bool) {
	defer func() { p = recover() != nil }()
	fn()
	return
}

func TestSchemaValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		schema    string
		wantPanic bool
	}{
		{name: "普通名字", schema: "ledger", wantPanic: false},
		{name: "含数字下划线", schema: "outbox_test_1", wantPanic: false},
		{name: "下划线开头", schema: "_x", wantPanic: false},
		{name: "空字符串", schema: "", wantPanic: true},
		{name: "数字开头", schema: "1abc", wantPanic: true},
		{name: "大写字母", schema: "Ledger", wantPanic: true},
		{name: "连字符", schema: "a-b", wantPanic: true},
		{name: "点号", schema: "a.b", wantPanic: true},
		{name: "引号注入", schema: `a"; DROP TABLE t; --`, wantPanic: true},
		{name: "空格", schema: "a b", wantPanic: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctors := map[string]func(){
				"MigrationSQL": func() { outbox.MigrationSQL(tc.schema) },
				"NewRelay":     func() { outbox.NewRelay(nil, tc.schema, outbox.NewDirectBus()) },
				"NewPublisher": func() { outbox.NewPublisher(nil, tc.schema) },
				"Inbox":        func() { outbox.Inbox(nil, tc.schema, "c", nil) },
			}
			for name, fn := range ctors {
				if got := panics(fn); got != tc.wantPanic {
					t.Errorf("%s(%q) panic = %v, want %v", name, tc.schema, got, tc.wantPanic)
				}
			}
		})
	}
}

func TestMigrationSQL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		schema string
		wants  []string
	}{
		{
			name:   "ledger schema",
			schema: "ledger",
			wants: []string{
				`"ledger".outbox`, `"ledger".inbox`,
				"published_at", "event_id",
				// claim/lease 与退避、死信所需的列。
				"attempts", "next_attempt_at", "claimed_until", "failed_at", "last_error",
				// 部分索引须排除死信，否则死信永远留在热路径索引里。
				"WHERE published_at IS NULL AND failed_at IS NULL",
				// inbox 去重键是 (consumer, event_id)。
				"consumer", "PRIMARY KEY (consumer, event_id)",
			},
		},
		{
			name:   "下划线 schema",
			schema: "my_domain",
			wants:  []string{`"my_domain".outbox`, `"my_domain".inbox`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sql := outbox.MigrationSQL(tc.schema)
			for _, w := range tc.wants {
				if !strings.Contains(sql, w) {
					t.Errorf("MigrationSQL(%q) 缺少 %q:\n%s", tc.schema, w, sql)
				}
			}
		})
	}
}

// fakeTx 经接口内嵌满足 pgx.Tx，用于在无 DB 的情况下给 ctx 打事务标记。
type fakeTx struct{ pgx.Tx }

func TestPublishGuards(t *testing.T) {
	t.Parallel()
	base := context.Background()
	txCtx := tx.With(base, fakeTx{})
	tests := []struct {
		name     string
		ctx      context.Context
		schema   string
		evt      appkit.Event
		wantCode string
	}{
		{
			name: "事务外调用", ctx: base, schema: "s",
			evt:      appkit.Event{Topic: "t"},
			wantCode: apperr.CodeTxBoundary,
		},
		{
			name: "schema 不合法", ctx: txCtx, schema: "1bad",
			evt:      appkit.Event{Topic: "t"},
			wantCode: apperr.CodeInvalidArgument,
		},
		{
			name: "topic 为空", ctx: txCtx, schema: "s",
			evt:      appkit.Event{},
			wantCode: apperr.CodeInvalidArgument,
		},
		{
			name: "ID 不是 uuid", ctx: txCtx, schema: "s",
			evt:      appkit.Event{ID: "not-a-uuid", Topic: "t"},
			wantCode: apperr.CodeInvalidArgument,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// 全部守卫用例都应在触碰 db 之前返回，db 传 nil 即可。
			err := outbox.Publish(tc.ctx, nil, tc.schema, tc.evt)
			if err == nil {
				t.Fatal("Publish 应当报错")
			}
			if !apperr.Is(err, tc.wantCode) {
				t.Fatalf("错误码 = %v, want %s", err, tc.wantCode)
			}
		})
	}
}

func TestInboxRejectsMissingID(t *testing.T) {
	t.Parallel()
	h := outbox.Inbox(nil, "s", "c", func(context.Context, appkit.Event) error {
		t.Fatal("不应调用 next")
		return nil
	})
	err := h(context.Background(), appkit.Event{Topic: "t"})
	if !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("缺 ID 的事件应返回 INVALID_ARGUMENT，得到 %v", err)
	}
}

// consumer 为空时去重维度缺失，构造期必须 fail-fast。
func TestInboxRejectsEmptyConsumer(t *testing.T) {
	t.Parallel()
	if !panics(func() { outbox.Inbox(nil, "s", "", nil) }) {
		t.Fatal("Inbox 空 consumer 应 panic")
	}
}

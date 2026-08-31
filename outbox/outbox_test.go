package outbox_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/forgeplex/appkit"
	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/callctx"
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
				// 框架自己也守「建表就写说明」这条：机检见
				// internal/schemadoc 的 TestFrameworkTablesAllDocumented。
				`COMMENT ON TABLE "ledger".outbox IS '`,
				`COMMENT ON TABLE "ledger".inbox IS '`,
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

// captureDB 只记录 Exec 的参数，用于在无数据库的情况下断言写进 outbox 的内容。
type captureDB struct{ args []any }

func (d *captureDB) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	d.args = args
	return pgconn.CommandTag{}, nil
}
func (d *captureDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }
func (d *captureDB) QueryRow(context.Context, string, ...any) pgx.Row        { return nil }

// TestPublishSnapshotsCallMeta 验证发布时把 callctx 白名单快照进事件 meta：
// 事件是异步的，投递时原请求早就结束了，不在这里存下来链路就断了。
// 业务自己写的 meta 必须原样保留。
func TestPublishSnapshotsCallMeta(t *testing.T) {
	t.Parallel()
	ctx := tx.With(context.Background(), "fake-tx")
	ctx = callctx.With(ctx, callctx.Meta{RequestID: "r1", TenantID: "acme"})

	db := &captureDB{}
	err := outbox.Publish(ctx, db, "ledger", appkit.Event{
		Topic:   "ledger.posted.v1",
		Payload: []byte(`{}`),
		Meta:    map[string]string{"source": "csv"},
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	raw, ok := db.args[3].([]byte)
	if !ok {
		t.Fatalf("meta 参数类型不符: %T", db.args[3])
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("meta 反序列化: %v", err)
	}
	if m := callctx.FromMap(got); m.RequestID != "r1" || m.TenantID != "acme" {
		t.Fatalf("白名单未落进事件 meta: %v", got)
	}
	if got["source"] != "csv" {
		t.Fatalf("业务 meta 被覆盖: %v", got)
	}
}

// TestPublishLeavesCallerMetaUntouched 验证不修改调用方传进来的 map。
func TestPublishLeavesCallerMetaUntouched(t *testing.T) {
	t.Parallel()
	ctx := callctx.With(tx.With(context.Background(), "fake-tx"),
		callctx.Meta{RequestID: "r1"})
	mine := map[string]string{"source": "csv"}
	if err := outbox.Publish(ctx, &captureDB{}, "ledger",
		appkit.Event{Topic: "t", Meta: mine}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(mine) != 1 {
		t.Fatalf("调用方的 meta 被就地改了: %v", mine)
	}
}

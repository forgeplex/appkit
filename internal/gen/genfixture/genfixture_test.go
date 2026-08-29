package genfixture

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/tx"
)

// 编译期断言：生成的 wrapper 实现契约接口。
var (
	_ Service = wrappedService{}
	_ Service = WrapService((*stubService)(nil), 0)
)

// stubService 是可观测的桩实现。
type stubService struct {
	calls []string
	err   error
}

func (s *stubService) Greet(_ context.Context, req GreetRequest) (GreetReply, error) {
	s.calls = append(s.calls, "Greet")
	return GreetReply{Message: "hi " + req.Name}, s.err
}

func (s *stubService) Stats(context.Context) (StatsReply, error) {
	s.calls = append(s.calls, "Stats")
	return StatsReply{Served: 7}, s.err
}

func (s *stubService) Reset(_ context.Context, _ ResetRequest) error {
	s.calls = append(s.calls, "Reset")
	return s.err
}

func (s *stubService) Ping(context.Context) error {
	s.calls = append(s.calls, "Ping")
	return s.err
}

// wrapCalls 把四个方法统一为 func(ctx) error，供表驱动复用。
func wrapCalls(w Service) []struct {
	name string
	call func(ctx context.Context) error
} {
	return []struct {
		name string
		call func(ctx context.Context) error
	}{
		{"Greet", func(ctx context.Context) error { _, err := w.Greet(ctx, GreetRequest{Name: "张三"}); return err }},
		{"Stats", func(ctx context.Context) error { _, err := w.Stats(ctx); return err }},
		{"Reset", func(ctx context.Context) error { return w.Reset(ctx, ResetRequest{Scope: "all"}) }},
		{"Ping", func(ctx context.Context) error { return w.Ping(ctx) }},
	}
}

// TestWrapServiceTxBoundary 证明 wrapper 真走了 contract.Call：
// 在 tx.With 标记的 ctx 下调用一律返回 CodeTxBoundary，且 inner 未被触达。
func TestWrapServiceTxBoundary(t *testing.T) {
	txCtx := tx.With(context.Background(), "fake-tx-handle")
	stub := &stubService{}
	for _, tc := range wrapCalls(WrapService(stub, 0)) {
		t.Run(tc.name, func(t *testing.T) {
			stub.calls = nil
			err := tc.call(txCtx)
			if !apperr.Is(err, apperr.CodeTxBoundary) {
				t.Fatalf("期望 CodeTxBoundary，得到 %v", err)
			}
			if len(stub.calls) != 0 {
				t.Fatalf("事务内调用不应触达 inner，实际调用了 %v", stub.calls)
			}
		})
	}
}

// TestWrapServicePassthrough 正常路径：值与错误码原样透传。
func TestWrapServicePassthrough(t *testing.T) {
	stub := &stubService{}
	w := WrapService(stub, time.Second)

	reply, err := w.Greet(context.Background(), GreetRequest{Name: "张三"})
	if err != nil {
		t.Fatalf("Greet: %v", err)
	}
	if reply.Message != "hi 张三" {
		t.Errorf("Greet 返回 %q，期望 %q", reply.Message, "hi 张三")
	}
	stats, err := w.Stats(context.Background())
	if err != nil || stats.Served != 7 {
		t.Errorf("Stats 返回 (%v, %v)，期望 (7, nil)", stats.Served, err)
	}
	if err := w.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

// TestWrapServiceErrorNormalized 错误经边界规范化：apperr 保码、裸错误折叠为 INTERNAL。
func TestWrapServiceErrorNormalized(t *testing.T) {
	cases := []struct {
		name     string
		inner    error
		wantCode string
	}{
		{"apperr 保码", apperr.New("GENFIXTURE_BOOM", 400, "boom"), "GENFIXTURE_BOOM"},
		{"裸错误折叠", errors.New("raw failure"), apperr.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := WrapService(&stubService{err: tc.inner}, 0)
			for _, c := range wrapCalls(w) {
				if err := c.call(context.Background()); !apperr.Is(err, tc.wantCode) {
					t.Errorf("%s: 期望错误码 %s，得到 %v", c.name, tc.wantCode, err)
				}
			}
		})
	}
}

// TestEntryPostedRoundTrip 事件序列化往返：Event() 填 topic 留空 ID，Parse 还原。
func TestEntryPostedRoundTrip(t *testing.T) {
	in := EntryPosted{
		EntryID:  "e-1",
		Amount:   "10.50",
		At:       time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC),
		Attempts: 3,
		Final:    true,
	}
	evt, err := in.Event()
	if err != nil {
		t.Fatal(err)
	}
	if evt.Topic != TopicEntryPosted {
		t.Errorf("topic = %q，期望 %q", evt.Topic, TopicEntryPosted)
	}
	if evt.ID != "" {
		t.Errorf("ID 应留空由 outbox 填充，实际 %q", evt.ID)
	}
	got, err := ParseEntryPosted(evt)
	if err != nil {
		t.Fatal(err)
	}
	if !got.At.Equal(in.At) {
		t.Errorf("At = %v，期望 %v", got.At, in.At)
	}
	got.At = in.At
	if got != in {
		t.Errorf("往返不一致: %+v != %+v", got, in)
	}
}

// TestParseEntryPostedErrors 解析失败路径。
func TestParseEntryPostedErrors(t *testing.T) {
	ok, err := EntryPosted{EntryID: "e-1"}.Event()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		mutate  func() (topic string, payload []byte)
		wantSub string
	}{
		{"topic 不匹配", func() (string, []byte) { return "other.topic", ok.Payload }, "topic 不匹配"},
		{"payload 非法", func() (string, []byte) { return TopicEntryPosted, []byte("{") }, "解析 payload"},
		{"必填字段为空", func() (string, []byte) { return TopicEntryPosted, []byte(`{"entry_id":""}`) }, "entry_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			topic, payload := tc.mutate()
			evt := ok
			evt.Topic, evt.Payload = topic, payload
			_, err := ParseEntryPosted(evt)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("期望错误含 %q，得到 %v", tc.wantSub, err)
			}
		})
	}
}

// TestGeneratedCodes 错误码生成物：常量值、sentinel 三元组与 Is 判定。
func TestGeneratedCodes(t *testing.T) {
	cases := []struct {
		err     *apperr.Error
		code    string
		status  int
		message string
	}{
		{ErrLedgerInsufficientFunds, "LEDGER_INSUFFICIENT_FUNDS", 422, "insufficient funds"},
		{ErrLedgerEntryNotFound, "LEDGER_ENTRY_NOT_FOUND", 404, "entry not found"},
		{ErrLedgerDuplicateID, "LEDGER_DUPLICATE_ID", 409, "duplicate entry id"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			if tc.err.Code() != tc.code || tc.err.Status() != tc.status || tc.err.Message() != tc.message {
				t.Errorf("sentinel = (%s, %d, %q)，期望 (%s, %d, %q)",
					tc.err.Code(), tc.err.Status(), tc.err.Message(), tc.code, tc.status, tc.message)
			}
			if !apperr.Is(tc.err, tc.code) {
				t.Errorf("apperr.Is(%s) = false", tc.code)
			}
		})
	}
}

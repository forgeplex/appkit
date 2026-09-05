package pgtx

import (
	"context"
	"strings"
	"testing"

	"github.com/forgeplex/appkit/tx"
	"github.com/jackc/pgx/v5"
)

// Embedded interfaces supply pgx.Tx's methods; these tests only compare
// handles, so invoking any method would itself be a test failure.
type comparableHandle struct {
	pgx.Tx
	value any
}

func TestDoRejectsNonComparableHandleBeforeBegin(t *testing.T) {
	for _, handle := range []pgx.Tx{
		comparableHandle{value: []string{"x"}},
		comparableHandle{value: map[string]string{"x": "y"}},
		comparableHandle{value: [1]any{[]string{"x"}}},
		sliceHandle{values: []string{"x"}},
	} {
		// A private marker fixture exercises the actual Do guard. The nil
		// embedded Tx makes any accidental Begin call panic, so this asserts
		// rejection before touching the handle as well as before the callback.
		ctx := context.WithValue(tx.With(context.Background(), handle), scopeKey{},
			transactionMarker{scope: transactionScope{}, handle: handle})
		ran := false
		err := New(nil).Do(ctx, func(context.Context) error { ran = true; return nil })
		if err == nil || !strings.Contains(err.Error(), "事务句柄") || ran {
			t.Fatalf("Do must reject incomparable handle before Begin/callback: err=%v ran=%v", err, ran)
		}
	}
}

type sliceHandle struct {
	pgx.Tx
	values []string
}

func TestSameTxRejectsNonComparableValues(t *testing.T) {
	a, b := &comparableHandle{}, &comparableHandle{}
	for _, tc := range []struct {
		name string
		a, b pgx.Tx
		want bool
	}{
		{"same pointer", a, a, true},
		{"different pointer", a, b, false},
		{"nil mismatch", a, nil, false},
		{"both nil", nil, nil, true},
		{"slice type", sliceHandle{values: []string{"x"}}, sliceHandle{values: []string{"x"}}, false},
		// The struct type is comparable, but its interface field contains a
		// slice. reflect.Type.Comparable alone would still allow a panic.
		{"slice inside interface", comparableHandle{value: []string{"x"}}, comparableHandle{value: []string{"x"}}, false},
		{"different value comparability", comparableHandle{value: "x"}, comparableHandle{value: []string{"x"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameTx(tc.a, tc.b); got != tc.want {
				t.Fatalf("sameTx = %v, want %v", got, tc.want)
			}
		})
	}
}

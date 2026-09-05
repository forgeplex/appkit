package pgtx

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

// Embedded interfaces supply pgx.Tx's methods; these tests only compare
// handles, so invoking any method would itself be a test failure.
type comparableHandle struct {
	pgx.Tx
	value any
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

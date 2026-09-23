//go:build !darwin && !linux

package cli

import (
	"fmt"
	"runtime"
)

func publishAccessSQL(_, _ string, _ []byte) error {
	return fmt.Errorf("%s 不支持安全的 db-access -out 原子发布；请省略 -out 输出到 stdout", runtime.GOOS)
}

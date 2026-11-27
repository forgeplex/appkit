// Package scopeflag 提供按包 import path 正则过滤的共享 flag.Value 实现。
package scopeflag

import (
	"flag"
	"fmt"
	"regexp"
)

// RegexFlag 在 flag 解析期即校验正则，非法值直接报错而不是运行期崩溃。
type RegexFlag struct {
	re *regexp.Regexp
}

var _ flag.Value = (*RegexFlag)(nil)

func (f *RegexFlag) String() string {
	if f == nil || f.re == nil {
		return ""
	}
	return f.re.String()
}

func (f *RegexFlag) Set(s string) error {
	if s == "" {
		f.re = nil
		return nil
	}
	re, err := regexp.Compile(s)
	if err != nil {
		return fmt.Errorf("-scope 正则无效: %w", err)
	}
	f.re = re
	return nil
}

// Match 当未设置正则时按 defaultWhenEmpty 返回；设置了正则时返回匹配结果。
func (f *RegexFlag) Match(pkgPath string, defaultWhenEmpty bool) bool {
	if f == nil || f.re == nil {
		return defaultWhenEmpty
	}
	return f.re.MatchString(pkgPath)
}

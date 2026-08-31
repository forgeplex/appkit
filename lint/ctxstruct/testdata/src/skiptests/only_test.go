// Package skiptests 验证默认不查 _test.go：违规无 want 注释，
// 默认跑零报告才通过。
package skiptests

import "context"

type fakeStore struct {
	ctx context.Context
}

// Package skiptests 验证默认不查 _test.go：违规无 want 注释，
// 默认跑零报告才通过。
package skiptests

func f() float64 { return float64(1) }

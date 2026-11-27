package a

import "strings"

func f(err error) (bool, string) {
	if err.Error() == "not found" { // want `禁止对 err\.Error\(\) 做字符串比对`
		return true, ""
	}
	if strings.Contains(err.Error(), "duplicate") { // want `禁止用 strings\.Contains 对 err\.Error\(\) 做字符串比对`
		return true, ""
	}
	if strings.HasPrefix(err.Error(), "timeout") { // want `禁止用 strings\.HasPrefix 对 err\.Error\(\) 做字符串比对`
		return true, ""
	}
	//appkit:ignore errcmp -- 第三方驱动未导出错误类型
	if strings.Contains(err.Error(), "legacy") {
		return true, ""
	}
	// 日志与字符串输出不受影响。
	return false, "failed: " + err.Error()
}

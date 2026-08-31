// appkit-lint 是 forgeplex 自研 go/analysis 检查器集合。
//
// 经 go vet -vettool 或 golangci-lint module plugin 接入，用法见 lint/README.md。
package main

import (
	"golang.org/x/tools/go/analysis/multichecker"

	"github.com/forgeplex/appkit/lint/ctxstruct"
	"github.com/forgeplex/appkit/lint/decjson"
	"github.com/forgeplex/appkit/lint/moneyfloat"
)

func main() {
	multichecker.Main(
		moneyfloat.Analyzer,
		ctxstruct.Analyzer,
		decjson.Analyzer,
	)
}

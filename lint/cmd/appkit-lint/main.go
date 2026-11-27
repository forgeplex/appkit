// appkit-lint 是 forgeplex 自研 go/analysis 检查器集合。
//
// 经 go vet -vettool 或 golangci-lint module plugin 接入，用法见 lint/README.md。
package main

import (
	"golang.org/x/tools/go/analysis/multichecker"

	"github.com/forgeplex/appkit/lint/authheader"
	"github.com/forgeplex/appkit/lint/callctxwrite"
	"github.com/forgeplex/appkit/lint/canonicalmoney"
	"github.com/forgeplex/appkit/lint/ctxstruct"
	"github.com/forgeplex/appkit/lint/decjson"
	"github.com/forgeplex/appkit/lint/errcmp"
	"github.com/forgeplex/appkit/lint/jsonany"
	"github.com/forgeplex/appkit/lint/moneyfloat"
	"github.com/forgeplex/appkit/lint/nogoroutine"
	"github.com/forgeplex/appkit/lint/pgtxbypass"
	"github.com/forgeplex/appkit/lint/timenow"
	"github.com/forgeplex/appkit/lint/txparam"
)

func main() {
	multichecker.Main(
		moneyfloat.Analyzer,
		ctxstruct.Analyzer,
		decjson.Analyzer,
		nogoroutine.Analyzer,
		authheader.Analyzer,
		errcmp.Analyzer,
		pgtxbypass.Analyzer,
		jsonany.Analyzer,
		timenow.Analyzer,
		canonicalmoney.Analyzer,
		txparam.Analyzer,
		callctxwrite.Analyzer,
	)
}

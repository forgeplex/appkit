// appkit 是框架 CLI：脚手架、架构检查、规则集分发、代码生成、本地多仓联调。
// 各子命令实现在 internal/cli 下，经 init 注册。
package main

import (
	"os"

	"github.com/forgeplex/appkit/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}

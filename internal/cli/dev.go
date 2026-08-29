// dev 子命令薄壳：flag 解析后转发 internal/scaffold。
package cli

import (
	"flag"
	"os"

	"github.com/forgeplex/appkit/internal/scaffold"
)

func init() {
	register(Command{Name: "dev", Summary: "生成/刷新 go.work，纳入 root 下全部子模块", Run: runDev})
}

func runDev(args []string) error {
	fs := flag.NewFlagSet("dev", flag.ContinueOnError)
	root := fs.String("root", "..", "扫描含 go.mod 的一级子目录的根目录")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return scaffold.Dev(".", *root, os.Stdout)
}

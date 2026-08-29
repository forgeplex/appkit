// new 子命令薄壳：flag 解析后转发 internal/scaffold。
package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/forgeplex/appkit/internal/scaffold"
)

func init() {
	register(Command{Name: "new", Summary: "生成仓库骨架（domain <name> | system <name>）", Run: runNew})
}

func runNew(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("用法: appkit new domain|system <name> [-module <path>] [-dir <path>]")
	}
	kind, name := args[0], args[1]
	fs := flag.NewFlagSet("new "+kind, flag.ContinueOnError)
	mod := fs.String("module", "", "生成仓库的 module path（默认 github.com/forgeplex/<name>）")
	dir := fs.String("dir", "", "输出目录（默认 ./<name>）")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	opts := scaffold.Options{Name: name, Module: *mod, Dir: *dir, AppkitVersion: Version()}
	switch kind {
	case "domain":
		return scaffold.Domain(opts, os.Stdout)
	case "system":
		return scaffold.System(opts, os.Stdout)
	default:
		return fmt.Errorf("未知的骨架类型 %q（可用：domain、system）", kind)
	}
}

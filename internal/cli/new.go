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
		return fmt.Errorf("用法: appkit new domain|system <name> [-module <path>] [-dir <path>] [-partitioned] [-tenant]")
	}
	kind, name := args[0], args[1]
	fs := flag.NewFlagSet("new "+kind, flag.ContinueOnError)
	mod := fs.String("module", "", "生成仓库的 module path（默认 github.com/forgeplex/<name>）")
	dir := fs.String("dir", "", "输出目录（默认 ./<name>）")
	partitioned := fs.Bool("partitioned", false,
		"分区域域：一套代码、N 份数据分区（schema 由调用方经租户身份路由，仅 domain）")
	tenant := fs.Bool("tenant", false,
		"租户域：单 schema、行级隔离（业务表挂 RLS，租户身份经事务级 GUC 下沉到存储，仅 domain）")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	opts := scaffold.Options{Name: name, Module: *mod, Dir: *dir, AppkitVersion: Version()}
	switch kind {
	case "domain":
		opts.Partitioned = *partitioned
		opts.Tenant = *tenant
		return scaffold.Domain(opts, os.Stdout)
	case "system":
		if *partitioned {
			return fmt.Errorf("-partitioned 只适用于 domain（组合仓库没有自己的 schema）")
		}
		if *tenant {
			return fmt.Errorf("-tenant 只适用于 domain（组合仓库没有自己的 schema）")
		}
		return scaffold.System(opts, os.Stdout)
	default:
		return fmt.Errorf("未知的骨架类型 %q（可用：domain、system）", kind)
	}
}

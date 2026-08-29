// appkit check —— 架构检查子命令的薄注册壳，全部实现见 internal/archcheck。
package cli

import (
	"flag"
	"fmt"

	"github.com/forgeplex/appkit/internal/archcheck"
)

func init() {
	register(Command{
		Name:    "check",
		Summary: "架构检查：go.mod 依赖 / import 边界 / SQL 跨 schema / 迁移序号",
		Run:     runCheck,
	})
}

func runCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	dir := fs.String("dir", ".", "仓库根目录（须含 .appkit.yml）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	vs, err := archcheck.Run(*dir)
	if err != nil {
		return err
	}
	if len(vs) == 0 {
		fmt.Println("检查通过，无违规")
		return nil
	}
	for _, v := range vs {
		fmt.Println(v.String())
	}
	return fmt.Errorf("共 %d 处违规", len(vs))
}

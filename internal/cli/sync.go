// appkit sync：把规则集（lint 配置 + CI 引用）物化进目标仓库。实现在 ruleset 包。
package cli

import (
	"flag"
	"fmt"

	"github.com/forgeplex/appkit/ruleset"
)

func init() {
	register(Command{
		Name:    "sync",
		Summary: "物化 lint 规则集与 CI 配置（--check 只做漂移检查）",
		Run:     runSync,
	})
}

func runSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	dir := fs.String("dir", ".", "目标仓库目录（须含 .appkit.yml）")
	check := fs.Bool("check", false, "只比对不写入，漂移时报错")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *check {
		if err := ruleset.Check(*dir, Version()); err != nil {
			return err
		}
		fmt.Println("规则集无漂移")
		return nil
	}
	paths, err := ruleset.Sync(*dir, Version())
	if err != nil {
		return err
	}
	for _, p := range paths {
		fmt.Println("已写入", p)
	}
	return nil
}

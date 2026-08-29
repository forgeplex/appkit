// gen 子命令薄壳：只做 flag 解析，实现全部在 internal/gen。
package cli

import (
	"errors"
	"flag"

	"github.com/forgeplex/appkit/internal/gen"
)

func init() {
	register(Command{Name: "gen", Summary: "代码生成：gen events|errors|wrap", Run: runGen})
}

func runGen(args []string) error {
	if len(args) == 0 {
		return errors.New("用法: appkit gen <events|errors|wrap> [flags]")
	}
	fs := flag.NewFlagSet("gen "+args[0], flag.ContinueOnError)
	switch args[0] {
	case "events", "errors":
		in := fs.String("in", "", "输入 yaml 文件")
		out := fs.String("out", "", "输出 Go 文件")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if args[0] == "events" {
			return gen.Events(*in, *out)
		}
		return gen.Errors(*in, *out)
	case "wrap":
		src := fs.String("src", "", "契约接口所在包目录")
		iface := fs.String("iface", "", "接口名")
		system := fs.String("system", "", "系统名（span 命名与错误归属）")
		out := fs.String("out", "", "输出 Go 文件（与 -src 同包目录）")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return gen.Wrap(*src, *iface, *system, *out)
	default:
		return errors.New("未知 gen 子命令 " + args[0] + "（可用: events|errors|wrap）")
	}
}

package cli

import (
	"errors"
	"flag"

	"github.com/forgeplex/appkit/internal/archcheck"
	"github.com/forgeplex/appkit/internal/scaffold"
)

func init() {
	register(Command{
		Name: "schema-tool", Summary: "安装/更新独立的 sqlc schema 快照工具（只写工具源码，不操作数据库）",
		Run: runSchemaTool,
	})
}

func runSchemaTool(args []string) error {
	f := flag.NewFlagSet("schema-tool", flag.ContinueOnError)
	dir := f.String("dir", ".", "域仓库根目录")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("schema-tool 不接受位置参数")
	}
	cfg, err := archcheck.LoadConfig(*dir)
	if err != nil {
		return err
	}
	if cfg.Kind != archcheck.KindDomain {
		return errors.New("schema-tool 只适用于拥有迁移的 domain 仓库")
	}
	return scaffold.InstallSchemaTool(*dir)
}

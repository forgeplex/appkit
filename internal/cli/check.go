// appkit check —— 架构检查子命令的薄注册壳，全部实现见 internal/archcheck。
package cli

import (
	"flag"
	"fmt"

	"github.com/forgeplex/appkit/internal/archcheck"
	"github.com/forgeplex/appkit/ruleset"
)

func init() {
	register(Command{
		Name:    "check",
		Summary: "架构检查：go.mod 依赖 / import 边界 / SQL 跨 schema / 迁移序号 / 规则集漂移",
		Run:     runCheck,
	})
}

func runCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	dir := fs.String("dir", ".", "仓库根目录（须含 .appkit.yml）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := archcheck.LoadConfig(*dir)
	if err != nil {
		return err
	}
	vs, err := archcheck.Run(*dir)
	if err != nil {
		return err
	}
	for _, v := range vs {
		fmt.Println(v.String())
	}

	// 规则集漂移与架构违规一起报：物化的 lint/CI 配置是框架产物，
	// 改松它们就能让检查变绿——本地就要拦住，不能只靠 CI。
	driftErr := checkRuleset(*dir, cfg.Kind)

	switch {
	case len(vs) > 0 && driftErr != nil:
		fmt.Println(driftErr)
		return fmt.Errorf("共 %d 处违规，且规则集有漂移", len(vs))
	case len(vs) > 0:
		return fmt.Errorf("共 %d 处违规", len(vs))
	case driftErr != nil:
		return driftErr
	}
	fmt.Println("检查通过，无违规")
	return nil
}

// checkRuleset 比对物化规则集。组合仓库不物化规则集（ruleset 只覆盖域仓库），跳过。
// 配置缺失同样算漂移——缺失意味着 lint 压根没在跑，比内容改动更该拦。
func checkRuleset(dir, kind string) error {
	if kind == archcheck.KindSystem {
		return nil
	}
	return ruleset.Check(dir, Version())
}

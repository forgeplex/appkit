package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/forgeplex/appkit/internal/doctor"
)

func init() {
	register(Command{
		Name:    "doctor",
		Summary: "诊断私有仓库/工作区环境（GOPRIVATE、git 凭据、go.work、docker）",
		Run:     runDoctor,
	})
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	dir := fs.String("dir", ".", "待诊断的仓库目录（无 go.mod 时只做全局检查）")
	prefix := fs.String("prefix", "github.com/forgeplex", "私有 module 前缀")
	if err := fs.Parse(args); err != nil {
		return err
	}

	checks := doctor.Run(doctor.Options{Dir: *dir, Prefix: *prefix})
	for _, c := range checks {
		mark := map[doctor.Status]string{doctor.OK: "✓", doctor.Warn: "!", doctor.Fail: "✗"}[c.Status]
		fmt.Printf("%s %s：%s\n", mark, c.Name, c.Detail)
		if c.Fix != "" && c.Status != doctor.OK {
			fmt.Printf("    修复: %s\n", c.Fix)
		}
	}
	if doctor.HasFailure(checks) {
		return fmt.Errorf("存在必须修复的环境问题（✗ 项）")
	}
	fmt.Fprintln(os.Stdout, "环境就绪")
	return nil
}

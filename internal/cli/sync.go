// appkit sync：把规则集（lint 配置 + CI 引用）物化进目标仓库。实现在 ruleset 包。
package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

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
	workflowRef := fs.String("workflow-ref", "", "AppKit workflow 的完整 40 位 commit SHA（省略时解析当前版本）")
	timeout := fs.Duration("timeout", 30*time.Second, "workflow 来源解析超时")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *timeout <= 0 {
		return fmt.Errorf("sync 不接受位置参数，且 -timeout 必须为正数")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	ref, err := resolveCLIWorkflowRef(ctx, Version(), *workflowRef)
	if err != nil {
		return err
	}
	if *check {
		if err := ruleset.CheckPinned(*dir, Version(), ref); err != nil {
			return err
		}
		fmt.Println("规则集无漂移")
		return nil
	}
	paths, err := ruleset.SyncPinned(*dir, Version(), ref)
	if err != nil {
		return err
	}
	for _, p := range paths {
		fmt.Println("已写入", p)
	}
	return nil
}

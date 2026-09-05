// appkit schema —— schema 文档子命令的薄注册壳，全部实现见 internal/schemadoc。
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/forgeplex/appkit/internal/archcheck"
	"github.com/forgeplex/appkit/internal/schemadoc"
)

func init() {
	register(Command{
		Name:    "schema",
		Summary: "从迁移生成 schema 文档与 ER 图（支持分区逻辑模板；-check 检查漂移）",
		Run:     runSchema,
	})
}

func runSchema(args []string) error {
	fs := flag.NewFlagSet("schema", flag.ContinueOnError)
	dir := fs.String("dir", ".", "仓库根目录（须含 .appkit.yml）")
	dsn := fs.String("dsn", "", "Postgres 连接串，缺省取 $TEST_DATABASE_URL")
	check := fs.Bool("check", false, "只比对不写入，漂移时报错")
	mode := fs.String("mode", "auto", "文档模式断言：auto|schema|logical-template（须与 partitioned 配置一致）")
	timeout := fs.Duration("timeout", 2*time.Minute, "临时库迁移与 catalog 检查的超时（清理另有短时限）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *timeout <= 0 {
		return errors.New("schema 不接受位置参数，且 -timeout 必须为正数")
	}
	if *mode != "auto" && *mode != "schema" && *mode != "logical-template" {
		return errors.New("-mode 只允许 auto、schema 或 logical-template")
	}
	cfg, err := archcheck.LoadConfig(*dir)
	if err != nil {
		return err
	}
	if cfg.Kind == archcheck.KindSystem {
		// 组合仓库没有自己的迁移，schema 归各域仓库自己维护。
		return errors.New("组合仓库没有 db/migrations，schema 文档由各域仓库自己生成")
	}
	actualMode := "schema"
	if cfg.Partitioned {
		actualMode = "logical-template"
	}
	if *mode != "auto" && *mode != actualMode {
		return fmt.Errorf("-mode=%s 与仓库 partitioned=%t 不匹配（应为 %s）", *mode, cfg.Partitioned, actualMode)
	}
	// 启用门先问、DSN 后要：未启用的仓库不该为了被告知「未启用」而先准备一个数据库。
	if *check {
		on, err := schemadoc.Adopted(*dir)
		if err != nil {
			return err
		}
		if !on {
			// 未启用不是失败：domain-ci.yml 经 @main 被全部存量域仓库共享，
			// 让它们在合并那一刻集体变红是不能接受的。打一条 GitHub Actions
			// 注解（在 workflow 摘要里可见）然后退出 0，跑过一次就永久转严。
			fmt.Printf("::notice title=schema 文档未启用::%s。跑 `make schema` 并提交产出即可启用。\n", schemadoc.ErrNotAdopted)
			return nil
		}
	}

	if *dsn == "" {
		*dsn = os.Getenv("TEST_DATABASE_URL")
	}
	if *dsn == "" {
		return errors.New("缺少数据库连接串：加 -dsn 或设置 TEST_DATABASE_URL（本地可用 make dev-db 起一个）")
	}

	o := schemadoc.Options{Dir: *dir, DSN: *dsn, Schema: cfg.Domain, Partitioned: cfg.Partitioned}
	if cfg.Partitioned {
		fmt.Printf("schema 文档模式：logical-template（逻辑模板；代表 schema %s；不检查运行时分区）\n", cfg.Domain)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	if *check {
		// 缺 COMMENT 是软约束：打 ::warning 注解（GitHub 摘要与 PR 里可见），
		// 不让 CI 变红——哪怕有漂移硬失败也先点名，一次把两类问题给全。
		missing, err := schemadoc.Check(ctx, o)
		for _, t := range missing {
			fmt.Printf("::warning title=缺表说明::%s 没有 COMMENT ON TABLE——在建表迁移里补一句，表的用途跟着表一起演进。\n", t)
		}
		if err != nil {
			return err
		}
		fmt.Println("schema 文档无漂移")
		return nil
	}
	paths, err := schemadoc.Generate(ctx, o)
	if err != nil {
		return err
	}
	for _, p := range paths {
		fmt.Println("已写入", p)
	}
	return nil
}

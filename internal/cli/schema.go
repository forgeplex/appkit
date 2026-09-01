// appkit schema —— schema 文档子命令的薄注册壳，全部实现见 internal/schemadoc。
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/forgeplex/appkit/internal/archcheck"
	"github.com/forgeplex/appkit/internal/schemadoc"
)

func init() {
	register(Command{
		Name:    "schema",
		Summary: "从 db/migrations 生成 schema 文档与 ER 图（-check 只做漂移检查）",
		Run:     runSchema,
	})
}

func runSchema(args []string) error {
	fs := flag.NewFlagSet("schema", flag.ContinueOnError)
	dir := fs.String("dir", ".", "仓库根目录（须含 .appkit.yml）")
	dsn := fs.String("dsn", "", "Postgres 连接串，缺省取 $TEST_DATABASE_URL")
	check := fs.Bool("check", false, "只比对不写入，漂移时报错")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := archcheck.LoadConfig(*dir)
	if err != nil {
		return err
	}
	if cfg.Kind == archcheck.KindSystem {
		// 组合仓库没有自己的迁移，schema 归各域仓库自己维护。
		return errors.New("组合仓库没有 db/migrations，schema 文档由各域仓库自己生成")
	}
	if cfg.Partitioned {
		// 分区域域一份无前缀迁移落到 N 个分区 schema，没有单一 schema 可画——
		// 支持它需要先想清楚「文档按分区画还是按逻辑模型画」，首版明确拒绝。
		return errors.New("schema 文档暂不支持分区域域（partitioned: true）——分区映射由组合根注入，本仓库无从枚举；要看分区 schema 的结构，直接对一个分区库跑 introspect")
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

	o := schemadoc.Options{Dir: *dir, DSN: *dsn, Schema: cfg.Domain}
	ctx := context.Background()
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

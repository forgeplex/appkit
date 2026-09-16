package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/forgeplex/appkit/internal/dbaccess"
)

func init() {
	register(Command{Name: "db-access", Summary: "校验并渲染 PostgreSQL service-role 权限契约", Run: runDBAccess})
}

func runDBAccess(args []string) error { return dbAccess(args, os.Stdout, os.Stderr) }

func dbAccess(args []string, out, diagnostics io.Writer) error {
	if len(args) == 0 {
		return errors.New("用法: appkit db-access <validate|render|check> [flags]")
	}
	f := flag.NewFlagSet("db-access "+args[0], flag.ContinueOnError)
	f.SetOutput(diagnostics)
	manifestPath := f.String("manifest", "db/access.yaml", "数据库权限 manifest")
	switch args[0] {
	case "validate":
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return errors.New("db-access validate 不接受位置参数")
		}
		m, err := dbaccess.LoadFile(*manifestPath)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "数据库权限 manifest 有效：service=%s login=%s permission=%s\n", m.Service, m.Roles.Login.Name, m.Roles.Permission.Name)
		return nil
	case "render":
		outputPath := f.String("out", "", "新 SQL 文件；省略则写 stdout（已有文件绝不覆盖）")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return errors.New("db-access render 不接受位置参数")
		}
		sql, err := loadAndRenderAccess(*manifestPath)
		if err != nil {
			return err
		}
		if *outputPath == "" {
			_, err = out.Write(sql)
			return err
		}
		return writeNewAccessSQL(*outputPath, sql)
	case "check":
		sqlPath := f.String("sql", "", "待比对的生成 SQL 文件")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 || *sqlPath == "" {
			return errors.New("db-access check 需要 -sql，且不接受位置参数")
		}
		want, err := loadAndRenderAccess(*manifestPath)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(*sqlPath)
		if err != nil {
			return fmt.Errorf("读取生成 SQL %s: %w", *sqlPath, err)
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("数据库权限 SQL 漂移：%s 与 %s 当前期望输出不一致；生成新的追加 migration，不要覆盖已应用 migration", *sqlPath, *manifestPath)
		}
		fmt.Fprintf(out, "数据库权限 SQL 无漂移：%s\n", *sqlPath)
		return nil
	default:
		return fmt.Errorf("未知 db-access 子命令 %q（可用: validate|render|check）", args[0])
	}
}

func loadAndRenderAccess(path string) ([]byte, error) {
	m, err := dbaccess.LoadFile(path)
	if err != nil {
		return nil, err
	}
	return dbaccess.RenderSQL(*m)
}

func writeNewAccessSQL(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建生成 SQL 目录: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("拒绝覆盖已有文件 %s；权限变化必须生成新的追加 migration", path)
		}
		return fmt.Errorf("创建生成 SQL %s: %w", path, err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("写入生成 SQL %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭生成 SQL %s: %w", path, err)
	}
	complete = true
	return nil
}

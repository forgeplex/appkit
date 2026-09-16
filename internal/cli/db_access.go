package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
		sqlPath := f.String("sql", "", "待比对的生成 SQL 文件（只查生成物漂移，不连接数据库）")
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
	requestedParent := filepath.Dir(path)
	parent, err := validateAccessOutputDirectory(requestedParent)
	if err != nil {
		return err
	}
	path = filepath.Join(parent, filepath.Base(path))
	if target, err := os.Lstat(path); err == nil {
		if target.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("拒绝通过符号链接写入生成 SQL %s", path)
		}
		return fmt.Errorf("拒绝覆盖已有文件 %s；权限变化必须生成新的追加 migration", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("检查生成 SQL 目标 %s: %w", path, err)
	}
	f, err := os.CreateTemp(parent, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建生成 SQL 临时文件: %w", err)
	}
	tempPath := f.Name()
	tempExists := true
	defer func() {
		_ = f.Close()
		if tempExists {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("写入生成 SQL %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("同步生成 SQL %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭生成 SQL %s: %w", path, err)
	}
	currentParent, err := validateAccessOutputDirectory(requestedParent)
	if err != nil {
		return err
	}
	if currentParent != parent {
		return fmt.Errorf("生成 SQL 目录在写入期间发生变化: %s", requestedParent)
	}
	if err := os.Link(tempPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("拒绝覆盖已有文件 %s；权限变化必须生成新的追加 migration", path)
		}
		return fmt.Errorf("原子发布生成 SQL %s: %w", path, err)
	}
	if err := os.Remove(tempPath); err != nil {
		return fmt.Errorf("清理生成 SQL 临时文件: %w", err)
	}
	tempExists = false
	directory, err := os.Open(parent)
	if err != nil {
		return fmt.Errorf("打开生成 SQL 目录 %s: %w", parent, err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return fmt.Errorf("同步生成 SQL 目录 %s: %w", parent, err)
	}
	return nil
}

func validateAccessOutputDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("解析生成 SQL 目录 %s: %w", path, err)
	}
	requested, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("检查生成 SQL 目录 %s: %w（目录必须预先存在，命令不会代建或改变权限）", abs, err)
	}
	if requested.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("拒绝通过符号链接目录写入生成 SQL: %s", abs)
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("解析生成 SQL 目录真实路径 %s: %w", path, err)
	}
	volume := filepath.VolumeName(abs)
	current := volume + string(os.PathSeparator)
	for _, part := range strings.Split(strings.TrimPrefix(abs, current), string(os.PathSeparator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("检查生成 SQL 目录 %s: %w（目录必须预先存在，命令不会代建或改变权限）", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("拒绝通过符号链接目录写入生成 SQL: %s", current)
		}
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("检查生成 SQL 目录 %s: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("生成 SQL 的父路径 %s 不是目录", abs)
	}
	return abs, nil
}

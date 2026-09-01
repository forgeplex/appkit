package archcheck

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// schemaRefRe 捕获「关键字 + 空白 + 标识符 + 点」形态的 schema 限定引用。
// 只锚定在 FROM/JOIN/INTO/UPDATE/TABLE/REFERENCES/EXISTS 之后，
// 因此列别名（SELECT o.id）与函数调用（SELECT pg_catalog.lower(x)）不会误报。
var schemaRefRe = regexp.MustCompile(`(?i)\b(FROM|JOIN|INTO|UPDATE|TABLE|REFERENCES|EXISTS)\s+"?([a-z_][a-z0-9_]*)"?\."?`)

// systemSchemas 是允许引用的系统 schema。
var systemSchemas = map[string]bool{
	"pg_catalog":         true,
	"information_schema": true,
	"public":             true,
}

// CheckSQL 扫描 db/**/*.sql 中的跨 schema 引用：
// 捕获到的 schema 名既不是本域 domain 也不是系统 schema 即违规
// （每域独占 Postgres schema，跨域数据走契约调用或事件读模型）。
//
// 分区域域（partitioned: true）的规则翻转：任何 schema 前缀都是违规——
// 落位由事务级 search_path 路由决定，混入前缀形态破坏 sqlc 编译自洽性
// （一个世界里有前缀、一个世界里没有）。sqlc 编译器也会拦，但那是 make gen
// 才跑；这里在 make check（无 DB）就给出行号。
func CheckSQL(dir string, cfg *Config) ([]Violation, error) {
	dbDir := filepath.Join(dir, "db")
	if _, err := os.Stat(dbDir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var vs []Violation
	err := filepath.WalkDir(dbDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".sql") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("读取 %s 失败: %v", path, err)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		for _, m := range schemaRefRe.FindAllSubmatchIndex(data, -1) {
			schema := strings.ToLower(string(data[m[4]:m[5]]))
			if systemSchemas[schema] {
				continue
			}
			line := 1 + bytes.Count(data[:m[0]], []byte("\n"))
			if cfg.Partitioned {
				vs = append(vs, Violation{
					File: rel,
					Line: line,
					Msg:  fmt.Sprintf("分区域域的 SQL 不得带 schema 前缀（抓到 %s.）：落位由事务级 search_path 路由，混写前缀形态会让 sqlc 编译失败", schema),
				})
				continue
			}
			if schema == cfg.Domain {
				continue
			}
			vs = append(vs, Violation{
				File: rel,
				Line: line,
				Msg:  fmt.Sprintf("跨 schema 引用 %s（本域 schema 为 %s；跨域数据走契约调用或事件读模型）", schema, cfg.Domain),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return vs, nil
}

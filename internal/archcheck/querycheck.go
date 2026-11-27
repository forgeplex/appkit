package archcheck

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const allowUnsafeSQLMarker = "appkit:allow-unsafe-sql"

var (
	// SELECT [DISTINCT] * 或 SELECT a.*（不误报 COUNT(*)）。
	selectStarRe = regexp.MustCompile(`(?is)\bSELECT\s+(?:DISTINCT(?:\s+ON\s*\([^)]*\))?\s+)?(?:(?:[a-z_][a-z0-9_]*|"[^"]+")\.)?\*(?:\s|,|$|\bFROM\b)`)
	// 顶层 UPDATE / DELETE 语句（排除 RETURNING/ON CONFLICT DO UPDATE 等子句：只看语句开头）。
	updateStmtRe = regexp.MustCompile(`(?is)^\s*(?:WITH\b.*)?\bUPDATE\b`)
	deleteStmtRe = regexp.MustCompile(`(?is)^\s*(?:WITH\b.*)?\bDELETE\s+FROM\b`)
	whereRe      = regexp.MustCompile(`(?i)\bWHERE\b`)
)

// CheckQueries 扫描 db/queries/*.sql 中的高危 SQL 写法：
//  1. 禁止 SELECT * / SELECT t.*（表加列会导致 sqlc 结构漂移或泄露敏感列）；
//  2. 禁止缺少 WHERE 条件的 UPDATE / DELETE（防误操作全表修改或清空）。
//
// 确有必要时，在语句前一行或行内写 `-- appkit:allow-unsafe-sql: <原因>` 豁免。
func CheckQueries(dir string) ([]Violation, error) {
	qDir := filepath.Join(dir, "db", "queries")
	if _, err := os.Stat(qDir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var vs []Violation
	err := filepath.WalkDir(qDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
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
		for _, stmt := range splitSQLStatementsWithMarker(string(data), allowUnsafeSQLMarker) {
			if stmt.cleaned == "" || stmt.allowUnsafe {
				continue
			}
			if selectStarRe.MatchString(stmt.cleaned) {
				vs = append(vs, Violation{
					File: rel,
					Line: stmt.line,
					Msg:  "SQL 查询禁止 SELECT *：请显式列出所需列（避免加列导致 sqlc 结构漂移或泄露敏感列）；确需豁免请加 -- appkit:allow-unsafe-sql: <原因>",
				})
			}
			if (updateStmtRe.MatchString(stmt.cleaned) || deleteStmtRe.MatchString(stmt.cleaned)) &&
				!whereRe.MatchString(stmt.cleaned) {
				vs = append(vs, Violation{
					File: rel,
					Line: stmt.line,
					Msg:  "危险 SQL：UPDATE / DELETE 缺少 WHERE 条件（会修改或删除整张表）；确需全表操作请加 -- appkit:allow-unsafe-sql: <原因>",
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return vs, nil
}

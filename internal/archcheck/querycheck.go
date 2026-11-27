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
)

// isTopLevelWriteWithoutWhere 按 SQL 顶层词法（depth == 0，跳过单/双引号、
// dollar-quote 与括号子查询/CTE 内部）判断语句是否为缺少顶层 WHERE 的
// UPDATE / DELETE。避免子查询、CTE 或字符串字面量里的 WHERE 绕过全表写门禁，
// 也避免 WITH ... INSERT ... ON CONFLICT DO UPDATE 被误判。
func isTopLevelWriteWithoutWhere(sql string) bool {
	tokens := topLevelSQLWords(sql)
	if len(tokens) == 0 {
		return false
	}
	verbIdx := -1
	switch tokens[0] {
	case "UPDATE", "DELETE":
		verbIdx = 0
	case "WITH":
		for i := 1; i < len(tokens); i++ {
			switch tokens[i] {
			case "SELECT", "INSERT", "MERGE", "VALUES", "TABLE":
				return false
			case "UPDATE", "DELETE":
				verbIdx = i
			}
			if verbIdx >= 0 {
				break
			}
		}
	default:
		return false
	}
	if verbIdx < 0 {
		return false
	}
	for _, tok := range tokens[verbIdx+1:] {
		if tok == "RETURNING" {
			break
		}
		if tok == "WHERE" {
			return false
		}
	}
	return true
}

// topLevelSQLWords 提取括号嵌套深度为 0 的 SQL 关键字/未加引号标识符（全大写），
// 跳过单引号字符串、双引号标识符与 PostgreSQL dollar-quote 字面量。
func topLevelSQLWords(s string) []string {
	var words []string
	depth := 0
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\'':
			i++
			for i < len(s) {
				if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		case c == '"':
			i++
			for i < len(s) {
				if s[i] == '"' {
					if i+1 < len(s) && s[i+1] == '"' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		case c == '$':
			if tag, ok := matchDollarTag(s[i:]); ok {
				end := strings.Index(s[i+len(tag):], tag)
				if end < 0 {
					return words
				}
				i += len(tag) + end + len(tag)
				continue
			}
			i++
		case c == '(':
			depth++
			i++
		case c == ')':
			if depth > 0 {
				depth--
			}
			i++
		case isSQLIdentStart(c):
			start := i
			for i < len(s) && isSQLIdentPart(s[i]) {
				i++
			}
			if depth == 0 {
				words = append(words, strings.ToUpper(s[start:i]))
			}
		default:
			i++
		}
	}
	return words
}

func matchDollarTag(s string) (string, bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", false
	}
	if s[1] == '$' {
		return "$$", true
	}
	if !isSQLIdentStart(s[1]) {
		return "", false
	}
	for i := 2; i < len(s); i++ {
		if s[i] == '$' {
			return s[:i+1], true
		}
		if !isSQLIdentPart(s[i]) {
			return "", false
		}
	}
	return "", false
}

func isSQLIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isSQLIdentPart(c byte) bool {
	return isSQLIdentStart(c) || (c >= '0' && c <= '9')
}

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
			if isTopLevelWriteWithoutWhere(stmt.cleaned) {
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

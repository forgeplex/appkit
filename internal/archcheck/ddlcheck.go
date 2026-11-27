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

const allowUnsafeDDLMarker = "appkit:allow-unsafe-ddl"

var (
	// ALTER TABLE ... ADD [COLUMN] ... NOT NULL（无 DEFAULT / GENERATED）
	alterAddColRe = regexp.MustCompile(`(?is)^\s*ALTER\s+TABLE\b.*\bADD\s+(?:COLUMN\s+)?(?:IF\s+NOT\s+EXISTS\s+)?(?:"[^"]+"|[a-z_][a-z0-9_]*)\s+`)
	// ALTER TABLE ... ALTER [COLUMN] ... [SET DATA] TYPE ...
	alterColTypeRe = regexp.MustCompile(`(?is)^\s*ALTER\s+TABLE\b.*\bALTER\s+(?:COLUMN\s+)?(?:"[^"]+"|[a-z_][a-z0-9_]*)\s+(?:SET\s+DATA\s+)?TYPE\b`)
	// CREATE [UNIQUE] INDEX CONCURRENTLY ...
	indexConcurrentlyRe = regexp.MustCompile(`(?is)^\s*CREATE\s+(?:UNIQUE\s+)?INDEX\s+CONCURRENTLY\b`)
	// DROP TABLE ... / TRUNCATE ...
	dropTableRe = regexp.MustCompile(`(?is)^\s*DROP\s+TABLE\b`)
	truncateRe  = regexp.MustCompile(`(?is)^\s*TRUNCATE\b`)
	// CREATE TABLE / ALTER TABLE 中禁止浮点列类型（REAL / DOUBLE PRECISION / FLOAT / FLOAT4 / FLOAT8）
	tableDDLRe     = regexp.MustCompile(`(?is)^\s*(?:CREATE\s+(?:UNLOGGED\s+|TEMP\s+|TEMPORARY\s+)?TABLE|ALTER\s+TABLE)\b`)
	floatColTypeRe = regexp.MustCompile(`(?i)\b(?:DOUBLE\s+PRECISION|FLOAT4|FLOAT8|FLOAT(?:\s*\(\s*\d+\s*\))?|REAL)\b`)

	notNullRe   = regexp.MustCompile(`(?i)\bNOT\s+NULL\b`)
	defaultRe   = regexp.MustCompile(`(?i)\bDEFAULT\b`)
	generatedRe = regexp.MustCompile(`(?i)\bGENERATED\s+(?:ALWAYS|BY\s+DEFAULT)\s+AS\b`)
	// 排除 ADD CONSTRAINT / ADD PRIMARY KEY / ADD FOREIGN KEY / ADD UNIQUE / ADD CHECK
	addConstraintRe = regexp.MustCompile(`(?is)^\s*ALTER\s+TABLE\b.*\bADD\s+(?:CONSTRAINT|PRIMARY\s+KEY|FOREIGN\s+KEY|UNIQUE|CHECK)\b`)
)

// CheckMigrationDDL 对 db/migrations/*.sql 执行静态 DDL 风险检查（参考 squawk 规则，
// 结合 pgmigrate 事务型迁移机制）：
//  1. 禁止 CREATE INDEX CONCURRENTLY：pgmigrate 每个迁移文件运行在单事务内，
//     PostgreSQL 会在运行期直接拒绝事务内 CONCURRENTLY；
//  2. 禁止 ALTER TABLE ADD COLUMN ... NOT NULL 且无 DEFAULT（对非空存量表失败/锁表）；
//  3. 禁止 ALTER TABLE ALTER COLUMN ... TYPE（触发 ACCESS EXCLUSIVE 全表重写）；
//  4. 禁止无显式确认的 DROP TABLE / TRUNCATE（不可逆删数据）。
//
// 确有必要时，在语句前一行或行内注释写 `-- appkit:allow-unsafe-ddl: <原因>` 豁免
// （CONCURRENTLY 不可豁免，因 pgmigrate 事务内必然失败）。
func CheckMigrationDDL(dir string) ([]Violation, error) {
	migDir := filepath.Join(dir, "db", "migrations")
	entries, err := os.ReadDir(migDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %v", migDir, err)
	}

	var vs []Violation
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		full := filepath.Join(migDir, e.Name())
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, fmt.Errorf("读取 %s 失败: %v", full, err)
		}
		rel := "db/migrations/" + e.Name()
		for _, stmt := range splitSQLStatements(string(data)) {
			if stmt.cleaned == "" {
				continue
			}
			// CONCURRENTLY 在 pgmigrate 事务内必然失败，不允许豁免。
			if indexConcurrentlyRe.MatchString(stmt.cleaned) {
				vs = append(vs, Violation{
					File: rel,
					Line: stmt.line,
					Msg:  "pgmigrate 每个迁移运行在事务内，PostgreSQL 禁止事务中执行 CREATE INDEX CONCURRENTLY；请改用普通索引或在维护窗口于事务外创建",
				})
				continue
			}
			if stmt.allowUnsafe {
				continue
			}
			switch {
			case alterAddColRe.MatchString(stmt.cleaned) &&
				!addConstraintRe.MatchString(stmt.cleaned) &&
				notNullRe.MatchString(stmt.cleaned) &&
				!defaultRe.MatchString(stmt.cleaned) &&
				!generatedRe.MatchString(stmt.cleaned):
				vs = append(vs, Violation{
					File: rel,
					Line: stmt.line,
					Msg:  "危险 DDL：ALTER TABLE ADD COLUMN NOT NULL 缺少 DEFAULT（对存量非空表会失败并持锁）；请加 DEFAULT 或分步先可空后回填，确需执行请加注释 -- appkit:allow-unsafe-ddl: <原因>",
				})
			case alterColTypeRe.MatchString(stmt.cleaned):
				vs = append(vs, Violation{
					File: rel,
					Line: stmt.line,
					Msg:  "危险 DDL：ALTER COLUMN TYPE 可能触发 ACCESS EXCLUSIVE 全表重写；推荐 expand-and-contract，若确认仅改元数据请加注释 -- appkit:allow-unsafe-ddl: <原因>",
				})
			case dropTableRe.MatchString(stmt.cleaned):
				vs = append(vs, Violation{
					File: rel,
					Line: stmt.line,
					Msg:  "危险 DDL：迁移含不可逆 DROP TABLE；若确认数据已下线请加注释 -- appkit:allow-unsafe-ddl: <原因>",
				})
			case truncateRe.MatchString(stmt.cleaned):
				vs = append(vs, Violation{
					File: rel,
					Line: stmt.line,
					Msg:  "危险 DDL：迁移含不可逆 TRUNCATE；若确认清空表数据请加注释 -- appkit:allow-unsafe-ddl: <原因>",
				})
			}
			if tableDDLRe.MatchString(stmt.cleaned) && floatColTypeRe.MatchString(stmt.cleaned) {
				vs = append(vs, Violation{
					File: rel,
					Line: stmt.line,
					Msg:  "危险 DDL：表结构禁止使用浮点列类型（REAL / DOUBLE PRECISION / FLOAT），金额与业务数值请用 NUMERIC；若确属非金额科学计算列请加注释 -- appkit:allow-unsafe-ddl: <原因>",
				})
			}
		}
	}
	return vs, nil
}

type sqlStmt struct {
	cleaned     string // 剥离注释后的语句文本
	line        int    // 首条有效 SQL token 所在 1-based 行号
	allowUnsafe bool   // 前置/行内注释是否含 appkit:allow-unsafe-ddl
}

// splitSQLStatements 按顶层分号切分 SQL 语句，正确跳过单引号字符串、
// 美元引号（$$...$$ / $tag$...$tag$）、行注释（--）与块注释（/*...*/），
// 并捕获附着在语句上的 appkit:allow-unsafe-ddl 豁免注释。
func splitSQLStatements(src string) []sqlStmt {
	return splitSQLStatementsWithMarker(src, allowUnsafeDDLMarker)
}

func splitSQLStatementsWithMarker(src, marker string) []sqlStmt {
	var stmts []sqlStmt
	var buf strings.Builder
	stmtLine := 0
	allowUnsafe := false
	line := 1

	flush := func() {
		cleaned := strings.TrimSpace(buf.String())
		if cleaned != "" {
			stmts = append(stmts, sqlStmt{
				cleaned:     cleaned,
				line:        stmtLine,
				allowUnsafe: allowUnsafe,
			})
		}
		buf.Reset()
		stmtLine = 0
		allowUnsafe = false
	}

	n := len(src)
	for i := 0; i < n; {
		ch := src[i]
		if ch == '\n' {
			line++
			buf.WriteByte(ch)
			i++
			continue
		}
		// 行注释 -- ...
		if ch == '-' && i+1 < n && src[i+1] == '-' {
			start := i
			for i < n && src[i] != '\n' {
				i++
			}
			comment := src[start:i]
			if strings.Contains(comment, marker) {
				allowUnsafe = true
			}
			continue
		}
		// 块注释 /* ... */
		if ch == '/' && i+1 < n && src[i+1] == '*' {
			start := i
			i += 2
			depth := 1
			for i < n && depth > 0 {
				if src[i] == '\n' {
					line++
					i++
				} else if src[i] == '/' && i+1 < n && src[i+1] == '*' {
					depth++
					i += 2
				} else if src[i] == '*' && i+1 < n && src[i+1] == '/' {
					depth--
					i += 2
				} else {
					i++
				}
			}
			if strings.Contains(src[start:i], marker) {
				allowUnsafe = true
			}
			buf.WriteByte(' ')
			continue
		}
		// 单引号字符串 '...'（支持 '' 转义）
		if ch == '\'' {
			if stmtLine == 0 {
				stmtLine = line
			}
			buf.WriteByte(ch)
			i++
			for i < n {
				c := src[i]
				if c == '\n' {
					line++
				}
				buf.WriteByte(c)
				i++
				if c == '\'' {
					if i < n && src[i] == '\'' {
						buf.WriteByte(src[i])
						i++
						continue
					}
					break
				}
			}
			continue
		}
		// 美元引号 $$...$$ 或 $tag$...$tag$
		if ch == '$' {
			if tag, ok := matchDollarTag(src[i:]); ok {
				if stmtLine == 0 {
					stmtLine = line
				}
				buf.WriteString(tag)
				i += len(tag)
				for i < n {
					if strings.HasPrefix(src[i:], tag) {
						buf.WriteString(tag)
						i += len(tag)
						break
					}
					if src[i] == '\n' {
						line++
					}
					buf.WriteByte(src[i])
					i++
				}
				continue
			}
		}
		if ch == ';' {
			flush()
			i++
			continue
		}
		if stmtLine == 0 && ch != ' ' && ch != '\t' && ch != '\r' {
			stmtLine = line
		}
		buf.WriteByte(ch)
		i++
	}
	flush()
	return stmts
}

func matchDollarTag(s string) (string, bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", false
	}
	if s[1] == '$' {
		return "$$", true
	}
	if !isIdentStart(s[1]) {
		return "", false
	}
	for i := 2; i < len(s); i++ {
		if s[i] == '$' {
			return s[:i+1], true
		}
		if !isIdentPart(s[i]) {
			return "", false
		}
	}
	return "", false
}

func isIdentStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

func isIdentPart(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9')
}

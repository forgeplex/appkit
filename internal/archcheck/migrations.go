package archcheck

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// migNameRe 是迁移文件名规范：四位序号 + 下划线 + 描述 + .sql。
var migNameRe = regexp.MustCompile(`^(\d{4})_.+\.sql$`)

// CheckMigrations 检查 db/migrations 下 .sql 文件名格式，以及序号严格递增且不重复。
// 目录不存在视为无迁移，不报错。非 .sql 文件不参与检查。
func CheckMigrations(dir string) ([]Violation, error) {
	migDir := filepath.Join(dir, "db", "migrations")
	entries, err := os.ReadDir(migDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %v", migDir, err)
	}
	var vs []Violation
	prevNum := -1
	prevName := ""
	for _, e := range entries { // os.ReadDir 已按文件名排序，四位序号下字典序即数值序
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		name := e.Name()
		rel := "db/migrations/" + name
		m := migNameRe.FindStringSubmatch(name)
		if m == nil {
			vs = append(vs, Violation{File: rel, Msg: "迁移文件名必须形如 NNNN_描述.sql（四位序号 + 下划线 + 描述）"})
			continue
		}
		n, _ := strconv.Atoi(m[1])
		switch {
		case prevNum >= 0 && n == prevNum:
			vs = append(vs, Violation{File: rel, Msg: fmt.Sprintf("迁移序号 %s 与 %s 重复", m[1], prevName)})
		case prevNum >= 0 && n < prevNum:
			vs = append(vs, Violation{File: rel, Msg: fmt.Sprintf("迁移序号 %s 未严格递增（前一个为 %s）", m[1], prevName)})
		}
		prevNum, prevName = n, name
	}
	return vs, nil
}

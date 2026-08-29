package archcheck

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"
)

// CheckRequires 检查 go.mod 的 require 清单：github.com/forgeplex/* 中
// 只放行 appkit、appkit/lint、.appkit.yml 的 contracts 与 allowRequires，
// 其余一律违规（域仓库互不依赖铁律，跨域调用只依赖 contracts 生成的类型）。
func CheckRequires(dir string, cfg *Config) ([]Violation, error) {
	p := filepath.Join(dir, "go.mod")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("%s: 读取失败: %v", p, err)
	}
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: 解析失败: %v", p, err)
	}
	allowed := map[string]bool{
		"github.com/forgeplex/appkit":      true,
		"github.com/forgeplex/appkit/lint": true,
	}
	if cfg.Contracts != "" {
		allowed[cfg.Contracts] = true
	}
	for _, m := range cfg.AllowRequires {
		allowed[m] = true
	}
	var vs []Violation
	for _, r := range f.Require {
		mp := r.Mod.Path
		if !strings.HasPrefix(mp, forgeplexPrefix) || allowed[mp] {
			continue
		}
		line := 0
		if r.Syntax != nil {
			line = r.Syntax.Start.Line
		}
		vs = append(vs, Violation{
			File: "go.mod",
			Line: line,
			Msg:  fmt.Sprintf("require %s 违反域仓库互不依赖铁律（跨域调用只依赖 contracts；确需放行请加入 .appkit.yml 的 allowRequires）", mp),
		})
	}
	return vs, nil
}

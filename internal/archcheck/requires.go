package archcheck

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"
)

// isNestedContractModule reports whether mp matches a nested domain contract submodule
// of the form "github.com/forgeplex/<repo>/contract" (excluding appkit itself).
func isNestedContractModule(mp string) bool {
	if !strings.HasPrefix(mp, forgeplexPrefix) {
		return false
	}
	rest := strings.TrimPrefix(mp, forgeplexPrefix)
	if !strings.HasSuffix(rest, "/contract") {
		return false
	}
	repo := strings.TrimSuffix(rest, "/contract")
	return repo != "" && repo != "appkit" && !strings.Contains(repo, "/")
}

// CheckRequires 检查 go.mod 的 require 清单：github.com/forgeplex/* 中
// 只放行 appkit、appkit/lint、.appkit.yml 的 contracts、任意 */contract 嵌套契约子模块与 allowRequires，
// 其余一律违规（域仓库互不依赖铁律，跨域调用只依赖 contracts 生成的类型）。
// 若仓库含 contract/go.mod，同步校验嵌套契约子模块的 module 声明与依赖隔离约束。
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
		if !strings.HasPrefix(mp, forgeplexPrefix) || allowed[mp] || isNestedContractModule(mp) {
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
	nvs, err := checkNestedContractRequires(dir, cfg, allowed)
	if err != nil {
		return nil, err
	}
	vs = append(vs, nvs...)
	return vs, nil
}

func checkNestedContractRequires(dir string, cfg *Config, allowed map[string]bool) ([]Violation, error) {
	rel := "contract/go.mod"
	p := filepath.Join(dir, rel)
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: 读取失败: %v", p, err)
	}
	f, err := modfile.Parse(rel, data, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: 解析失败: %v", p, err)
	}
	var vs []Violation
	wantMod := ""
	if cfg.Module != "" {
		wantMod = cfg.Module + "/contract"
		gotMod := ""
		if f.Module != nil {
			gotMod = f.Module.Mod.Path
		}
		if gotMod != wantMod {
			vs = append(vs, Violation{
				File: rel,
				Line: 1,
				Msg:  fmt.Sprintf("contract 子模块 module 必须为 %s（当前为 %s）", wantMod, gotMod),
			})
		}
	}
	for _, r := range f.Require {
		mp := r.Mod.Path
		line := 0
		if r.Syntax != nil {
			line = r.Syntax.Start.Line
		}
		switch {
		case mp == "github.com/jackc/pgx" || strings.HasPrefix(mp, "github.com/jackc/pgx/"):
			vs = append(vs, Violation{
				File: rel,
				Line: line,
				Msg:  fmt.Sprintf("contract 子模块禁止 require pgx（%s，须零数据库驱动依赖）", mp),
			})
		case strings.HasPrefix(mp, "github.com/gin-gonic/"):
			vs = append(vs, Violation{
				File: rel,
				Line: line,
				Msg:  fmt.Sprintf("contract 子模块禁止 require gin（%s）", mp),
			})
		case cfg.Module != "" && mp == cfg.Module:
			vs = append(vs, Violation{
				File: rel,
				Line: line,
				Msg:  fmt.Sprintf("contract 子模块禁止反向 require 根域模块 %s（避免循环依赖）", mp),
			})
		case strings.HasPrefix(mp, forgeplexPrefix):
			okSelfAllowed := allowed[mp] && mp != cfg.Module && mp != wantMod
			okPeerContract := isNestedContractModule(mp) && mp != wantMod
			if mp == "github.com/forgeplex/appkit" || okSelfAllowed || okPeerContract {
				continue
			}
			vs = append(vs, Violation{
				File: rel,
				Line: line,
				Msg:  fmt.Sprintf("contract 子模块禁止 require %s（仅允许 appkit 或契约模块）", mp),
			})
		}
	}
	return vs, nil
}

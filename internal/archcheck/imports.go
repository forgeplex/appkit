package archcheck

import (
	"fmt"
	"go/parser"
	"go/scanner"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
)

// CheckImports 用 go/parser（ImportsOnly）扫描全部 .go 文件的 import 边界。
// 跳过 _test.go 与 vendor/、点开头目录，其余一律扫描。规则见 importViolation。
func CheckImports(dir string, cfg *Config) ([]Violation, error) {
	var vs []Violation
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != dir && (name == "vendor" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		fvs, err := checkFileImports(path, filepath.ToSlash(rel), cfg)
		if err != nil {
			return err
		}
		vs = append(vs, fvs...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("扫描 %s 下的 Go 源码失败: %v", dir, err)
	}
	return vs, nil
}

// fileLoc 是一个源文件在仓库结构中的位置分类。
type fileLoc struct {
	inDomain    bool // internal/<domain>/**：业务包
	pgxExempt   bool // internal/postgres/**、cmd/**、internal/module/**（wiring 放行）
	inTransport bool // internal/http/**、internal/inbox/**
}

func classify(rel string, cfg *Config) fileLoc {
	var l fileLoc
	if cfg.Kind == KindDomain && cfg.Domain != "" {
		l.inDomain = underDir(rel, "internal/"+cfg.Domain)
	}
	l.pgxExempt = underDir(rel, "internal/postgres") || underDir(rel, "cmd") || underDir(rel, "internal/module")
	l.inTransport = underDir(rel, "internal/http") || underDir(rel, "internal/inbox")
	return l
}

// underDir 判断相对路径 rel 是否位于目录 d 之下。
func underDir(rel, d string) bool { return strings.HasPrefix(rel, d+"/") }

// underPkg 判断 import 路径 imp 是否为包 pkg 本身或其子包。
func underPkg(imp, pkg string) bool { return imp == pkg || strings.HasPrefix(imp, pkg+"/") }

func checkFileImports(path, rel string, cfg *Config) ([]Violation, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if err != nil {
		return []Violation{parseErrViolation(rel, err)}, nil
	}
	loc := classify(rel, cfg)
	var vs []Violation
	for _, spec := range f.Imports {
		imp, uerr := strconv.Unquote(spec.Path.Value)
		if uerr != nil {
			continue
		}
		if msg := importViolation(imp, loc, cfg); msg != "" {
			vs = append(vs, Violation{File: rel, Line: fset.Position(spec.Path.Pos()).Line, Msg: msg})
		}
	}
	return vs, nil
}

// importViolation 按序检查单条 import，命中第一条规则即返回（每条 import 至多报一次）。
func importViolation(imp string, loc fileLoc, cfg *Config) string {
	mod := cfg.Module
	// 规则 1：业务包零 infra，且不得反向依赖 transport / 直连 postgres。
	if loc.inDomain {
		switch {
		case strings.HasPrefix(imp, "github.com/jackc/pgx"),
			strings.HasPrefix(imp, "github.com/gin-gonic/"),
			imp == "net/http", strings.HasPrefix(imp, "net/http/"),
			underPkg(imp, mod+"/internal/postgres"),
			underPkg(imp, mod+"/internal/http"),
			underPkg(imp, mod+"/internal/inbox"):
			return fmt.Sprintf("internal/%s 禁止 import %s（业务包零 infra，且不得反向依赖 transport）", cfg.Domain, imp)
		}
	}
	// 规则 2/3：pgx 与 sqlc 生成物只允许出现在 internal/postgres（cmd/、internal/module 放行 wiring）。
	if !loc.pgxExempt {
		if strings.HasPrefix(imp, "github.com/jackc/pgx") {
			return fmt.Sprintf("import %s：pgx 只允许出现在 internal/postgres（cmd/、internal/module 例外）", imp)
		}
		if mod != "" && underPkg(imp, mod+"/internal/postgres/sqlc") {
			return fmt.Sprintf("import %s：sqlc 生成物只允许被 internal/postgres 使用（cmd/、internal/module 例外）", imp)
		}
	}
	// 规则 4：transport 不得绕过业务包直连 postgres。
	if loc.inTransport && mod != "" && underPkg(imp, mod+"/internal/postgres") {
		return fmt.Sprintf("import %s：internal/http、internal/inbox 禁止 import internal/postgres（transport 必须走业务包接口）", imp)
	}
	// 规则 5：全仓禁 import 其他 forgeplex 域 module（require 铁律的 import 层复查）。
	if cfg.Kind == KindDomain && strings.HasPrefix(imp, forgeplexPrefix) {
		if underPkg(imp, mod) || underPkg(imp, "github.com/forgeplex/appkit") ||
			(cfg.Contracts != "" && underPkg(imp, cfg.Contracts)) {
			return ""
		}
		for _, m := range cfg.AllowRequires {
			if underPkg(imp, m) {
				return ""
			}
		}
		return fmt.Sprintf("禁止 import 其他 forgeplex 域 module（%s）；跨域调用请依赖 contracts", imp)
	}
	return ""
}

// parseErrViolation 把 Go 源码解析错误转成带位置的违规，不中断整体扫描。
func parseErrViolation(rel string, err error) Violation {
	if el, ok := err.(scanner.ErrorList); ok && len(el) > 0 {
		return Violation{File: rel, Line: el[0].Pos.Line, Msg: "Go 源文件解析失败: " + el[0].Msg}
	}
	return Violation{File: rel, Line: 1, Msg: "Go 源文件解析失败: " + err.Error()}
}

package archcheck

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// forbiddenLayerDirs 是禁止在业务包 internal/<domain>/ 下按层拆分的目录名。
// 域代码须按功能成簇（internal/<domain>/<功能>/），不按 model/service/repo
// 水平分层（见 AGENTS.md「文件怎么分」与 docs/DESIGN.md §4/§7）。
var forbiddenLayerDirs = map[string]bool{
	"model":        true,
	"models":       true,
	"entity":       true,
	"entities":     true,
	"service":      true,
	"services":     true,
	"repo":         true,
	"repos":        true,
	"repository":   true,
	"repositories": true,
	"handler":      true,
	"handlers":     true,
	"dto":          true,
	"dtos":         true,
	"controller":   true,
	"controllers":  true,
	"dao":          true,
	"daos":         true,
}

// CheckLayout 检查域仓库的 internal/ 目录拓扑：
//  1. 禁止在 internal/ 下新建与业务包平级的未知目录——它们会静默逃出
//     depguard / go-arch-lint / archcheck 对 internal/<domain>/** 的零 infra 约束；
//  2. 禁止在 internal/<domain>/ 下按层拆子包（model/service/repo/dto 等），
//     子包必须按功能拆分。
func CheckLayout(dir string, cfg *Config) ([]Violation, error) {
	if cfg.Kind == KindSystem {
		return checkSystemLayout(dir), nil
	}
	if cfg.Kind != KindDomain || cfg.Domain == "" {
		return nil, nil
	}

	internalDir := filepath.Join(dir, "internal")
	entries, err := os.ReadDir(internalDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %v", internalDir, err)
	}

	allowedTop := map[string]bool{
		cfg.Domain: true,
		"postgres": true,
		"http":     true,
		"inbox":    true,
		"module":   true,
	}

	var vs []Violation
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !allowedTop[e.Name()] {
			vs = append(vs, Violation{
				File: "internal/" + e.Name(),
				Msg: fmt.Sprintf(
					"禁止在 internal/ 下建平级目录 internal/%s（只允许 %s / postgres / http / inbox / module）：平级包会逃出业务包零 infra 机检，请将业务子包嵌套到 internal/%s/<功能>/",
					e.Name(), cfg.Domain, cfg.Domain,
				),
			})
		}
	}

	domainDir := filepath.Join(internalDir, cfg.Domain)
	if _, err := os.Stat(domainDir); err == nil {
		err = filepath.WalkDir(domainDir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !d.IsDir() || path == domainDir {
				return nil
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "testdata" {
				return filepath.SkipDir
			}
			if forbiddenLayerDirs[strings.ToLower(name)] {
				rel, rerr := filepath.Rel(dir, path)
				if rerr != nil {
					return rerr
				}
				vs = append(vs, Violation{
					File: filepath.ToSlash(rel),
					Msg: fmt.Sprintf(
						"业务包禁止按层拆子包 %q（%s）：请按功能拆成 internal/%s/<功能>/（参见 AGENTS.md「文件怎么分」）",
						name, filepath.ToSlash(rel), cfg.Domain,
					),
				})
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("扫描 %s 失败: %v", domainDir, err)
		}
	}

	return vs, nil
}

// checkSystemLayout 检查组合仓库（kind: system）只做 wiring + 配置 + 部署，
// 零业务逻辑——禁止存在 sqlc.yaml 或 db/queries/。
func checkSystemLayout(dir string) []Violation {
	var vs []Violation
	if _, err := os.Stat(filepath.Join(dir, "sqlc.yaml")); err == nil {
		vs = append(vs, Violation{
			File: "sqlc.yaml",
			Msg:  "组合仓库（kind: system）只做 wiring 与配置，禁止包含 sqlc.yaml；SQL 与存储实现必须放在业务域仓库",
		})
	}
	if info, err := os.Stat(filepath.Join(dir, "db", "queries")); err == nil && info.IsDir() {
		vs = append(vs, Violation{
			File: "db/queries",
			Msg:  "组合仓库（kind: system）只做 wiring 与配置，禁止包含 db/queries/；SQL 查询必须放在业务域仓库",
		})
	}
	return vs
}

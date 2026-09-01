// Package archcheck 实现 appkit check 的四类架构检查：
// go.mod 依赖清单、Go import 边界、SQL 跨 schema 引用、迁移文件序号。
//
// 约束依据见 docs/DESIGN.md §1/§4/§7：域仓库互不依赖、业务包零 infra、
// pgx/sqlc 只出现在 internal/postgres、transport 不抄近路、每域独占 schema。
package archcheck

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"go.yaml.in/yaml/v3"
	"golang.org/x/mod/modfile"
)

// 仓库类型：domain 为业务域仓库（默认），system 为组合仓库（如 psp），
// system 跳过 domain 相关检查（Requires / 域包 import / 跨域 import / SQL schema）。
const (
	KindDomain = "domain"
	KindSystem = "system"
)

const forgeplexPrefix = "github.com/forgeplex/"

// Config 对应仓库根目录的 .appkit.yml。
type Config struct {
	Version       int      `yaml:"version"`
	Kind          string   `yaml:"kind"`
	Domain        string   `yaml:"domain"`
	Module        string   `yaml:"module"`
	Contracts     string   `yaml:"contracts"`
	AllowRequires []string `yaml:"allowRequires"`
	// Partitioned 标记分区域域（appkit new domain -partitioned）：一套代码、
	// N 份数据分区，schema 由调用方经事务级 search_path 路由确定。该形态下
	// SQL 的前缀规则翻转（见 CheckSQL），schema 文档暂不支持（appkit schema 明确报错）。
	Partitioned bool `yaml:"partitioned"`
}

// Violation 是一条违规。File 为相对仓库根目录的斜杠路径；
// Line 为 1 起始行号，文件级违规（如文件名不合规）为 0。
type Violation struct {
	File string
	Line int
	Msg  string
}

// String 输出 "file:line: 消息"；文件级违规行号记为 1，保持格式可被编辑器跳转。
func (v Violation) String() string {
	line := v.Line
	if line < 1 {
		line = 1
	}
	return fmt.Sprintf("%s:%d: %s", v.File, line, v.Msg)
}

var domainRe = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// LoadConfig 读取并校验 dir 下的 .appkit.yml。
// module 缺省时回退到 go.mod 的 module 声明。
func LoadConfig(dir string) (*Config, error) {
	p := filepath.Join(dir, ".appkit.yml")
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: 未找到 .appkit.yml，请先在仓库根目录创建（appkit new 生成，或参照文档手写）", p)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: 读取失败: %v", p, err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: 解析失败: %v", p, err)
	}
	if cfg.Version != 1 {
		return nil, fmt.Errorf("%s: 不支持的 version %d（当前仅支持 1）", p, cfg.Version)
	}
	switch cfg.Kind {
	case "":
		cfg.Kind = KindDomain
	case KindDomain, KindSystem:
	default:
		return nil, fmt.Errorf("%s: kind %q 非法（只允许 domain 或 system）", p, cfg.Kind)
	}
	if cfg.Module == "" {
		cfg.Module = modulePathFromGoMod(dir)
	}
	if cfg.Kind == KindDomain {
		if !domainRe.MatchString(cfg.Domain) {
			return nil, fmt.Errorf("%s: domain %q 非法（须匹配 ^[a-z][a-z0-9]*$）", p, cfg.Domain)
		}
		if cfg.Module == "" {
			return nil, fmt.Errorf("%s: 缺少 module，且无法从 go.mod 推断", p)
		}
	}
	return cfg, nil
}

func modulePathFromGoMod(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	return modfile.ModulePath(data)
}

// Run 读取 .appkit.yml 后依次执行四类检查，返回全部违规。
// kind: system 跳过 Requires 与 SQL 检查，Imports 内部只保留结构性规则。
func Run(dir string) ([]Violation, error) {
	cfg, err := LoadConfig(dir)
	if err != nil {
		return nil, err
	}
	var all []Violation
	if cfg.Kind == KindDomain {
		vs, err := CheckRequires(dir, cfg)
		if err != nil {
			return nil, err
		}
		all = append(all, vs...)
	}
	vs, err := CheckImports(dir, cfg)
	if err != nil {
		return nil, err
	}
	all = append(all, vs...)
	if cfg.Kind == KindDomain {
		vs, err := CheckSQL(dir, cfg)
		if err != nil {
			return nil, err
		}
		all = append(all, vs...)
	}
	vs, err = CheckMigrations(dir)
	if err != nil {
		return nil, err
	}
	all = append(all, vs...)
	return all, nil
}

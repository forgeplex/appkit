package ruleset

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// appConfigName 是各仓库的框架配置文件名。
const appConfigName = ".appkit.yml"

// AppConfig 对应 .appkit.yml（版本恒为 1）。
type AppConfig struct {
	Version       int      `yaml:"version"`
	Kind          string   `yaml:"kind"` // domain（默认）或 system
	Domain        string   `yaml:"domain"`
	Module        string   `yaml:"module"`
	Contracts     string   `yaml:"contracts"`
	AllowRequires []string `yaml:"allowRequires"`
	// Partitioned 是「分区域域」标记（appkit new domain -partitioned 写入）。
	// ruleset 不消费它，这里收下只为与 archcheck.Config 共用一份配置文件。
	Partitioned bool `yaml:"partitioned"`
}

// LoadAppConfig 读取并校验 dir 下的 .appkit.yml。
func LoadAppConfig(dir string) (AppConfig, error) {
	path := filepath.Join(dir, appConfigName)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return AppConfig{}, fmt.Errorf("%s 不存在（appkit sync 需在域仓库根目录运行）", path)
		}
		return AppConfig{}, fmt.Errorf("读取 %s: %w", path, err)
	}
	defer f.Close()

	return parseAppConfig(f, path)
}

// ParseAppConfig 解析并校验已经读入内存的 .appkit.yml，规则与 LoadAppConfig
// 相同。它不读取文件，供需要将校验结果绑定到同一份配置快照的调用方使用。
// 错误中的来源名为 .appkit.yml；文件路径信息由调用方补充。
func ParseAppConfig(data []byte) (AppConfig, error) {
	return parseAppConfig(bytes.NewReader(data), appConfigName)
}

func parseAppConfig(r io.Reader, path string) (AppConfig, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true) // 未知字段直接报错，防手误
	var c AppConfig
	if err := dec.Decode(&c); err != nil {
		return AppConfig{}, fmt.Errorf("解析 %s: %w", path, err)
	}
	if c.Version != 1 {
		return AppConfig{}, fmt.Errorf("%s: version 须为 1，当前为 %d", path, c.Version)
	}
	switch c.Kind {
	case "", "domain":
	case "system":
		return AppConfig{}, fmt.Errorf("%s: kind 为 system（组合仓库），规则集物化仅适用于域仓库", path)
	default:
		return AppConfig{}, fmt.Errorf("%s: kind %q 非法（只允许 domain 或 system）", path, c.Kind)
	}
	cfg := Config{Domain: c.Domain, Module: c.Module, Contracts: c.Contracts}
	if err := cfg.validate(); err != nil {
		return AppConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Sync 按 .appkit.yml 渲染规则集并写入 dir，返回写入的相对路径（有序）。
func Sync(dir, version string) ([]string, error) {
	files, err := renderFor(dir, version)
	if err != nil {
		return nil, err
	}
	paths := sortedKeys(files)
	for _, rel := range paths {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return nil, fmt.Errorf("创建目录 %s: %w", filepath.Dir(abs), err)
		}
		if err := os.WriteFile(abs, files[rel], 0o644); err != nil {
			return nil, fmt.Errorf("写入 %s: %w", abs, err)
		}
	}
	return paths, nil
}

// Check 比对渲染结果与磁盘内容，漂移则报错并列出文件。
func Check(dir, version string) error {
	files, err := renderFor(dir, version)
	if err != nil {
		return err
	}
	var drift []string
	for _, rel := range sortedKeys(files) {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		got, err := os.ReadFile(abs)
		switch {
		case errors.Is(err, os.ErrNotExist):
			drift = append(drift, rel+"（缺失）")
		case err != nil:
			return fmt.Errorf("读取 %s: %w", abs, err)
		case !bytes.Equal(got, files[rel]):
			drift = append(drift, rel+"（内容漂移）")
		}
	}
	if len(drift) > 0 {
		return fmt.Errorf("规则集漂移，请运行 appkit sync 刷新:\n  %s", strings.Join(drift, "\n  "))
	}
	return nil
}

func renderFor(dir, version string) (map[string][]byte, error) {
	ac, err := LoadAppConfig(dir)
	if err != nil {
		return nil, err
	}
	return Render(Config{Domain: ac.Domain, Module: ac.Module, Contracts: ac.Contracts, Version: version})
}

func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

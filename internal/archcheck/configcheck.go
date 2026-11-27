package archcheck

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

type configCheckDoc struct {
	Env   string `yaml:"env"`
	Debug struct {
		Pprof bool `yaml:"pprof"`
	} `yaml:"debug"`
	Security struct {
		Mode string `yaml:"mode"`
		OIDC struct {
			Realms []struct {
				Issuer  string `yaml:"issuer"`
				JWKSURL string `yaml:"jwks_url"`
			} `yaml:"realms"`
		} `yaml:"oidc"`
	} `yaml:"security"`
}

// CheckConfigs 对 config/*.yaml / *.yml 做静态安全基线检查：
//  1. 任意配置文件禁止出现明文 PEM 私钥（-----BEGIN ... PRIVATE KEY-----）；
//  2. 非 dev 配置（env != "dev" 且文件名非 dev.*/local.*）禁止：
//     - security.mode: disabled；
//     - security.mode: user_facing 与 debug.pprof: true 同现；
//     - security.oidc.realms 中使用非回环明文 http:// 作为 issuer 或 jwks_url。
func CheckConfigs(dir string) ([]Violation, error) {
	cfgDir := filepath.Join(dir, "config")
	entries, err := os.ReadDir(cfgDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %v", cfgDir, err)
	}

	var vs []Violation
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		full := filepath.Join(cfgDir, name)
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, fmt.Errorf("读取 %s 失败: %v", full, err)
		}
		rel := "config/" + name
		raw := string(data)
		if strings.Contains(raw, "-----BEGIN ") && strings.Contains(raw, "PRIVATE KEY-----") {
			vs = append(vs, Violation{
				File: rel,
				Msg:  "配置文件禁止提交明文 PEM 私钥，请通过环境变量或 Secret Manager 注入",
			})
		}

		var doc configCheckDoc
		if err := yaml.Unmarshal(data, &doc); err != nil {
			vs = append(vs, Violation{File: rel, Msg: fmt.Sprintf("YAML 解析失败: %v", err)})
			continue
		}
		env := strings.ToLower(strings.TrimSpace(doc.Env))
		isDev := env == "dev" || (env == "" && (strings.HasPrefix(name, "dev.") || strings.HasPrefix(name, "local.")))
		if isDev {
			continue
		}

		mode := strings.ToLower(strings.TrimSpace(doc.Security.Mode))
		if mode == "disabled" {
			vs = append(vs, Violation{
				File: rel,
				Msg:  fmt.Sprintf("非 dev 配置（env=%q）禁止 security.mode: disabled", doc.Env),
			})
		}
		if mode == "user_facing" && doc.Debug.Pprof {
			vs = append(vs, Violation{
				File: rel,
				Msg:  "非 dev 配置禁止在 security.mode=user_facing 下开启 debug.pprof",
			})
		}
		for _, r := range doc.Security.OIDC.Realms {
			if isNonLoopbackHTTP(r.Issuer) {
				vs = append(vs, Violation{
					File: rel,
					Msg:  fmt.Sprintf("非 dev 配置 OIDC issuer %q 禁止明文 http://，请使用 https://", r.Issuer),
				})
			}
			if isNonLoopbackHTTP(r.JWKSURL) {
				vs = append(vs, Violation{
					File: rel,
					Msg:  fmt.Sprintf("非 dev 配置 OIDC jwks_url %q 禁止明文 http://，请使用 https://", r.JWKSURL),
				})
			}
		}
	}
	return vs, nil
}

func isNonLoopbackHTTP(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Scheme, "http") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return false
	}
	return true
}

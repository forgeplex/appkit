package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeOpts 构造全注入的 Options：默认环境健康，测试按需破坏单项。
func fakeOpts(t *testing.T, mutate func(*Options), files map[string]string) Options {
	t.Helper()
	o := Options{
		Prefix: "github.com/forgeplex",
		Home:   "/home/u",
		GoEnv: func(string, ...string) (map[string]string, error) {
			return map[string]string{
				"GOVERSION": "go1.26.6",
				"GOPRIVATE": "github.com/forgeplex/*",
				"GOWORK":    "",
			}, nil
		},
		GitConfig: func() (string, error) {
			return "url.git@github.com:forgeplex/.insteadof https://github.com/forgeplex/\n", nil
		},
		LookPath: func(string) (string, error) { return "/usr/bin/docker", nil },
		ReadFile: func(name string) ([]byte, error) {
			if c, ok := files[name]; ok {
				return []byte(c), nil
			}
			return nil, os.ErrNotExist
		},
	}
	if mutate != nil {
		mutate(&o)
	}
	return o
}

func byName(t *testing.T, cs []Check, name string) Check {
	t.Helper()
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("缺少检查项 %q：%+v", name, cs)
	return Check{}
}

func TestHealthyEnvironment(t *testing.T) {
	cs := Run(fakeOpts(t, nil, nil))
	if HasFailure(cs) {
		t.Fatalf("健康环境不应有 Fail：%+v", cs)
	}
	for _, name := range []string{"Go 工具链", "GOPRIVATE", "git 私有仓库凭据", "docker（集成测试用）"} {
		if c := byName(t, cs, name); c.Status != OK {
			t.Fatalf("%s 应为 OK：%+v", name, c)
		}
	}
}

func TestGoPrivate(t *testing.T) {
	cases := []struct {
		name      string
		goprivate string
		want      Status
	}{
		{"未设置", "", Fail},
		{"覆盖通配", "github.com/forgeplex/*", OK},
		{"覆盖前缀", "github.com/forgeplex", OK},
		{"多值含目标", "example.com/x,github.com/forgeplex/*", OK},
		{"只覆盖别的组织", "github.com/other/*", Fail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := fakeOpts(t, func(o *Options) {
				gp := tc.goprivate
				o.GoEnv = func(string, ...string) (map[string]string, error) {
					return map[string]string{"GOVERSION": "go1.26.6", "GOPRIVATE": gp}, nil
				}
			}, nil)
			c := byName(t, Run(o), "GOPRIVATE")
			if c.Status != tc.want {
				t.Fatalf("GOPRIVATE=%q status=%v want %v（%s）", tc.goprivate, c.Status, tc.want, c.Detail)
			}
			if tc.want == Fail && !strings.Contains(c.Fix, "go env -w GOPRIVATE") {
				t.Fatalf("Fail 项应给出修复命令：%+v", c)
			}
		})
	}
}

func TestGoVersion(t *testing.T) {
	cases := []struct {
		v    string
		want Status
	}{
		{"go1.26.6", OK}, {"go1.27.0", OK}, {"go2.0.0", OK},
		{"go1.25.3", Fail}, {"weird", Warn},
	}
	for _, tc := range cases {
		if c := checkGoVersion(tc.v); c.Status != tc.want {
			t.Fatalf("版本 %q status=%v want %v", tc.v, c.Status, tc.want)
		}
	}
}

func TestGitAuth(t *testing.T) {
	t.Run("无 insteadOf 无 netrc 为 Warn 且提示特征错误", func(t *testing.T) {
		o := fakeOpts(t, func(o *Options) {
			o.GitConfig = func() (string, error) { return "", errors.New("no config") }
		}, nil)
		c := byName(t, Run(o), "git 私有仓库凭据")
		if c.Status != Warn || !strings.Contains(c.Detail, "could not read Username") {
			t.Fatalf("应 Warn 并包含特征错误提示：%+v", c)
		}
		if !strings.Contains(c.Fix, "insteadOf") {
			t.Fatalf("应给出 insteadOf 修复命令：%+v", c)
		}
	})
	t.Run("netrc 凭据视为 OK", func(t *testing.T) {
		o := fakeOpts(t, func(o *Options) {
			o.GitConfig = func() (string, error) { return "", errors.New("no config") }
		}, map[string]string{"/home/u/.netrc": "machine github.com login x password y"})
		if c := byName(t, Run(o), "git 私有仓库凭据"); c.Status != OK {
			t.Fatalf("netrc 应为 OK：%+v", c)
		}
	})
}

func TestWorkspace(t *testing.T) {
	gomodNoRequire := "module github.com/forgeplex/identity\n\ngo 1.26\n"
	gomodWithRequire := gomodNoRequire + "\nrequire github.com/forgeplex/appkit v0.1.0\n"
	dir := t.TempDir()

	cases := []struct {
		name    string
		gomod   string
		appkit  bool // 存在 .appkit.yml
		gowork  string
		want    Status
		present bool
	}{
		{"require 了 appkit", gomodWithRequire, true, "", OK, true},
		{"devel 且在工作区", gomodNoRequire, true, "/x/go.work", OK, true},
		{"devel 且不在工作区", gomodNoRequire, true, "", Fail, true},
		{"无关仓库跳过", gomodNoRequire, false, "", OK, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{filepath.Join(dir, "go.mod"): tc.gomod}
			if tc.appkit {
				files[filepath.Join(dir, ".appkit.yml")] = "version: 1"
			}
			o := fakeOpts(t, func(o *Options) {
				gw := tc.gowork
				o.Dir = dir
				o.GoEnv = func(string, ...string) (map[string]string, error) {
					return map[string]string{
						"GOVERSION": "go1.26.6", "GOPRIVATE": "github.com/forgeplex/*", "GOWORK": gw,
					}, nil
				}
			}, files)
			cs := Run(o)
			var found *Check
			for i := range cs {
				if cs[i].Name == "工作区" {
					found = &cs[i]
				}
			}
			if !tc.present {
				if found != nil {
					t.Fatalf("无关仓库不应有工作区检查：%+v", *found)
				}
				return
			}
			if found == nil {
				t.Fatalf("缺少工作区检查：%+v", cs)
			}
			if found.Status != tc.want {
				t.Fatalf("status=%v want %v（%s）", found.Status, tc.want, found.Detail)
			}
			if tc.want == Fail && !strings.Contains(found.Fix, "appkit dev") {
				t.Fatalf("Fail 项应指引 appkit dev：%+v", *found)
			}
		})
	}
}

func TestDockerMissingIsWarnOnly(t *testing.T) {
	o := fakeOpts(t, func(o *Options) {
		o.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	}, nil)
	cs := Run(o)
	if c := byName(t, cs, "docker（集成测试用）"); c.Status != Warn {
		t.Fatalf("docker 缺失应为 Warn：%+v", c)
	}
	if HasFailure(cs) {
		t.Fatal("docker 缺失不应导致整体失败")
	}
}

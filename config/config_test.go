package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/config"
)

type dbConfig struct {
	URL      string `koanf:"url" validate:"required"`
	PoolSize int    `koanf:"pool_size" validate:"min=1"`
}

type appConfig struct {
	Addr  string   `koanf:"addr" validate:"required"`
	Debug bool     `koanf:"debug"`
	DB    dbConfig `koanf:"db"`
}

const baseYAML = `
addr: ":8080"
debug: false
db:
  url: "postgres://localhost/app"
  pool_size: 4
`

// writeFiles 把 contents 依序写为临时 yaml 文件，返回路径（与 contents 同序）。
func writeFiles(t *testing.T, contents []string) []string {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, len(contents))
	for i, c := range contents {
		paths[i] = filepath.Join(dir, "f"+string(rune('0'+i))+".yaml")
		if err := os.WriteFile(paths[i], []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name string
		// yamls 写入临时文件；missing 追加不存在的路径（相对临时目录）。
		yamls    []string
		missing  []string
		prefix   string
		optional bool
		env      map[string]string

		want         appConfig
		wantCode     string   // 非空表示期望失败且错误码为它
		wantContains []string // 期望出现在 err.Error() 里的片段
	}{
		{
			name:  "单文件加载",
			yamls: []string{baseYAML},
			want: appConfig{
				Addr: ":8080",
				DB:   dbConfig{URL: "postgres://localhost/app", PoolSize: 4},
			},
		},
		{
			name: "多文件按序深合并后者覆盖",
			yamls: []string{baseYAML, `
debug: true
db:
  pool_size: 8
`},
			want: appConfig{
				Addr:  ":8080",
				Debug: true,
				DB:    dbConfig{URL: "postgres://localhost/app", PoolSize: 8},
			},
		},
		{
			name:   "环境变量覆盖文件且双下划线映射层级",
			yamls:  []string{baseYAML},
			prefix: "APP",
			env: map[string]string{
				"APP_ADDR":           ":9090",
				"APP_DB__POOL_SIZE":  "16", // 双下划线分层、单下划线保留在键名内
				"APP_DEBUG":          "true",
				"OTHER_DB__POOLSIZE": "999", // 前缀不匹配，必须被忽略
			},
			want: appConfig{
				Addr:  ":9090",
				Debug: true,
				DB:    dbConfig{URL: "postgres://localhost/app", PoolSize: 16},
			},
		},
		{
			name:   "前缀带尾下划线等价",
			yamls:  []string{baseYAML},
			prefix: "APP_",
			env:    map[string]string{"APP_ADDR": ":7070"},
			want: appConfig{
				Addr: ":7070",
				DB:   dbConfig{URL: "postgres://localhost/app", PoolSize: 4},
			},
		},
		{
			name:     "Optional跳过缺失文件",
			yamls:    []string{baseYAML},
			missing:  []string{"absent.yaml"},
			optional: true,
			want: appConfig{
				Addr: ":8080",
				DB:   dbConfig{URL: "postgres://localhost/app", PoolSize: 4},
			},
		},
		{
			name:         "缺失文件聚合报错",
			yamls:        []string{baseYAML},
			missing:      []string{"a.yaml", "b.yaml"},
			wantCode:     apperr.CodeInvalidArgument,
			wantContains: []string{"a.yaml", "b.yaml"},
		},
		{
			name:         "yaml解析失败",
			yamls:        []string{"addr: [unclosed"},
			wantCode:     apperr.CodeInvalidArgument,
			wantContains: []string{"配置文件"},
		},
		{
			name: "校验失败逐字段聚合",
			yamls: []string{`
debug: true
db:
  pool_size: 0
`},
			wantCode:     apperr.CodeInvalidArgument,
			wantContains: []string{"addr: required", "db.url: required", "db.pool_size: min=1"},
		},
		{
			name:   "环境变量补齐后校验通过",
			yamls:  []string{"db:\n  pool_size: 2\n"},
			prefix: "APP",
			env: map[string]string{
				"APP_ADDR":    ":8080",
				"APP_DB__URL": "postgres://localhost/x",
			},
			want: appConfig{
				Addr: ":8080",
				DB:   dbConfig{URL: "postgres://localhost/x", PoolSize: 2},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			files := writeFiles(t, tt.yamls)
			for _, m := range tt.missing {
				files = append(files, filepath.Join(t.TempDir(), m))
			}

			got, err := config.Load[appConfig](config.Options{
				Files:     files,
				EnvPrefix: tt.prefix,
				Optional:  tt.optional,
			})

			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("Load() 意外失败: %v", err)
				}
				if got != tt.want {
					t.Fatalf("Load() = %+v, want %+v", got, tt.want)
				}
				return
			}

			if err == nil {
				t.Fatalf("Load() = %+v, 期望失败", got)
			}
			if !apperr.Is(err, tt.wantCode) {
				t.Fatalf("错误码 = %v, want %s", err, tt.wantCode)
			}
			if _, ok := errors.AsType[*apperr.Error](err); !ok {
				t.Fatalf("错误类型 = %T, want *apperr.Error", err)
			}
			for _, sub := range tt.wantContains {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("err.Error() 缺少 %q:\n%v", sub, err)
				}
			}
		})
	}
}

// errGraphContains 沿错误对象图（含 Unwrap 链与结构体字段）深扫字符串，
// 检查是否残留 needle。mapstructure 的 ParseError.Value、strconv.NumError.Num
// 等字段即使不出现在 Error() 文本里也持有字段原始值，必须一并覆盖。
func errGraphContains(v reflect.Value, needle string, depth int) bool {
	if depth > 32 || !v.IsValid() {
		return false
	}
	switch v.Kind() {
	case reflect.String:
		return strings.Contains(v.String(), needle)
	case reflect.Pointer, reflect.Interface:
		return errGraphContains(v.Elem(), needle, depth+1)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if errGraphContains(v.Field(i), needle, depth+1) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if errGraphContains(v.Index(i), needle, depth+1) {
				return true
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			if errGraphContains(iter.Key(), needle, depth+1) ||
				errGraphContains(iter.Value(), needle, depth+1) {
				return true
			}
		}
	}
	return false
}

// TestLoad_UnmarshalErrorRedactsValues 复现评审场景：unmarshal 失败的错误链
// 不得携带字段原始值——配置值可能是密钥，错误会进启动日志。
// 覆盖 int 解析失败与 time.Duration 解析失败两类，字段值用形似密钥的字符串。
func TestLoad_UnmarshalErrorRedactsValues(t *testing.T) {
	type timeoutConfig struct {
		Addr     string        `koanf:"addr"`
		Timeout  time.Duration `koanf:"timeout"`
		PoolSize int           `koanf:"pool_size"`
	}

	tests := []struct {
		name    string
		yaml    string
		secret  string   // 不得出现在错误链任何角落的字段原始值
		wantKey []string // 仍应保留的键路径线索
	}{
		{
			name:    "int解析失败",
			yaml:    "addr: \":8080\"\npool_size: \"sk-live-int-53cr3t-AbC\"\n",
			secret:  "sk-live-int-53cr3t-AbC",
			wantKey: []string{"pool_size"},
		},
		{
			name:    "duration解析失败",
			yaml:    "addr: \":8080\"\ntimeout: \"sk-live-dur-53cr3t-XyZ\"\n",
			secret:  "sk-live-dur-53cr3t-XyZ",
			wantKey: []string{"timeout"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := writeFiles(t, []string{tt.yaml})
			_, err := config.Load[timeoutConfig](config.Options{Files: files})
			if err == nil {
				t.Fatal("期望 unmarshal 失败")
			}
			if !apperr.Is(err, apperr.CodeInvalidArgument) {
				t.Fatalf("错误码 = %v, want %s", err, apperr.CodeInvalidArgument)
			}
			text := err.Error()
			if strings.Contains(text, tt.secret) {
				t.Fatalf("错误文本泄漏字段原始值 %q:\n%v", tt.secret, text)
			}
			// 不止文本：错误链上的结构体字段也不得残留原始值
			// （修复前 mapstructure.ParseError.Value 等携带原值）。
			if errGraphContains(reflect.ValueOf(err), tt.secret, 0) {
				t.Fatalf("错误链对象仍携带字段原始值 %q:\n%v", tt.secret, text)
			}
			for _, key := range tt.wantKey {
				if !strings.Contains(text, key) {
					t.Errorf("错误文本应保留键路径线索 %q:\n%v", key, text)
				}
			}
		})
	}
}

// TestLoad_ValidationDetails 校验失败时 Details 必须逐字段聚合（一次性报全）。
func TestLoad_ValidationDetails(t *testing.T) {
	files := writeFiles(t, []string{"debug: true\n"})
	_, err := config.Load[appConfig](config.Options{Files: files})
	if err == nil {
		t.Fatal("期望校验失败")
	}
	e, ok := errors.AsType[*apperr.Error](err)
	if !ok {
		t.Fatalf("错误类型 = %T, want *apperr.Error", err)
	}
	details := e.Details()
	want := map[string]any{
		"addr":         "required",
		"db.url":       "required",
		"db.pool_size": "min=1",
	}
	if len(details) != len(want) {
		t.Fatalf("Details() = %v, want %v", details, want)
	}
	for k, v := range want {
		if details[k] != v {
			t.Errorf("Details()[%q] = %v, want %v", k, details[k], v)
		}
	}
}

// TestLoad_NonStruct 非 struct 的 T 跳过校验但仍正常反序列化。
func TestLoad_NonStruct(t *testing.T) {
	files := writeFiles(t, []string{"foo: 1\nbar: two\n"})
	got, err := config.Load[map[string]any](config.Options{Files: files})
	if err != nil {
		t.Fatalf("Load() 失败: %v", err)
	}
	if got["bar"] != "two" {
		t.Fatalf("got = %v", got)
	}
}

func TestMustLoad(t *testing.T) {
	t.Run("成功返回值", func(t *testing.T) {
		files := writeFiles(t, []string{baseYAML})
		got := config.MustLoad[appConfig](config.Options{Files: files})
		if got.Addr != ":8080" {
			t.Fatalf("got = %+v", got)
		}
	})
	t.Run("失败panic且携带apperr", func(t *testing.T) {
		defer func() {
			p := recover()
			if p == nil {
				t.Fatal("期望 panic")
			}
			err, ok := p.(error)
			if !ok || !apperr.Is(err, apperr.CodeInvalidArgument) {
				t.Fatalf("panic 值 = %v", p)
			}
		}()
		config.MustLoad[appConfig](config.Options{Files: []string{filepath.Join(t.TempDir(), "no.yaml")}})
	})
}

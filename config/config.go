// Package config 实现分层配置加载（DESIGN §2）：yaml 文件按序合并 →
// 环境变量覆盖 → unmarshal 到强类型 → validator 校验。
// 任何一步失败都在启动期报错（fail-fast）；校验失败逐字段聚合后一次性报出，
// 避免"改一个报一个"的多轮启动。
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"regexp"
	"strings"
	"sync"

	"github.com/go-playground/validator/v10"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"

	"github.com/forgeplex/appkit/apperr"
)

// delim 是键路径的层级分隔符（foo.bar）。
const delim = "."

// Options 配置加载来源。
type Options struct {
	// Files 按序加载合并：后面的文件覆盖前面的同名键（深合并）。
	Files []string
	// EnvPrefix 非空时启用环境变量覆盖，优先级高于全部文件。
	// 前缀 "APP"（尾部下划线可省）匹配 APP_ 开头的变量；键名去前缀后
	// 转小写，双下划线映射为层级分隔符：APP_FOO__BAR → foo.bar。
	// 单下划线保留在键名内（APP_DB__POOL_SIZE → db.pool_size）。
	// 留空则完全不读环境变量，避免把无关变量吸进配置。
	EnvPrefix string
	// Optional 为 true 时静默跳过 Files 中不存在的文件；
	// 为 false 时文件缺失报错（缺失项聚合后一次性报出）。
	Optional bool
}

// Load 按 Options 加载配置到 T。返回的错误一律是 *apperr.Error：
// 校验失败时 Details 逐字段聚合违反的规则（键为 koanf 键路径，如 db.pool_size），
// 报错不携带字段值——配置里可能有密钥。
//
// T 通常是 struct（字段用 koanf tag 对应键名、validate tag 声明约束）；
// 非 struct 的 T（如 map）跳过校验。
func Load[T any](opts Options) (T, error) {
	var zero T

	k := koanf.New(delim)

	var loadErrs []error
	for _, path := range opts.Files {
		err := k.Load(file.Provider(path), yaml.Parser())
		if err == nil {
			continue
		}
		if opts.Optional && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		loadErrs = append(loadErrs, fmt.Errorf("配置文件 %s: %w", path, err))
	}
	if len(loadErrs) > 0 {
		return zero, apperr.InvalidArgument("加载配置文件失败").WithCause(errors.Join(loadErrs...))
	}

	if opts.EnvPrefix != "" {
		prefix := strings.TrimSuffix(opts.EnvPrefix, "_") + "_"
		p := env.Provider(prefix, delim, func(s string) string {
			s = strings.ToLower(strings.TrimPrefix(s, prefix))
			return strings.ReplaceAll(s, "__", delim)
		})
		if err := k.Load(p, nil); err != nil {
			return zero, apperr.InvalidArgument("加载环境变量覆盖失败").
				WithCause(fmt.Errorf("环境变量前缀 %s: %w", prefix, err))
		}
	}

	var cfg T
	if err := k.Unmarshal("", &cfg); err != nil {
		return zero, apperr.InvalidArgument("配置反序列化失败").
			WithCause(fmt.Errorf("unmarshal 到 %T: %w", cfg, redactUnmarshalErr(err)))
	}

	if err := validate(cfg); err != nil {
		return zero, err
	}
	return cfg, nil
}

// MustLoad 是 Load 的 panic 版本，供 main 顶层 fail-fast 使用。
func MustLoad[T any](opts Options) T {
	cfg, err := Load[T](opts)
	if err != nil {
		panic(err)
	}
	return cfg
}

// unmarshal 失败的错误链会携带字段原始值：mapstructure 的
// ParseError.Value、strconv.NumError.Num 等结构体字段持有原值，部分错误
// 文本还会以 "..."（strconv/time.ParseDuration）或 value: '...' 形态回显。
// 配置值可能是密钥，错误又会进启动日志，必须脱敏后再入链。
var (
	reDoubleQuoted    = regexp.MustCompile(`"[^"]*"`)
	reSingleQuotedVal = regexp.MustCompile(`value: '[^']*'`)
)

// redactUnmarshalErr 脱敏 unmarshal 错误：只保留键路径与类型信息的文本
// 摘要（引号内容擦除为 [REDACTED]），并返回扁平新错误、切断原错误链——
// 链上的结构体字段即使不出现在 Error() 里也持有原始值。
func redactUnmarshalErr(err error) error {
	s := err.Error()
	s = reDoubleQuoted.ReplaceAllString(s, `"[REDACTED]"`)
	s = reSingleQuotedVal.ReplaceAllString(s, "value: '[REDACTED]'")
	return errors.New(s)
}

// newValidator 全局单例：validator 按类型缓存 tag 解析结果，多实例会丢掉缓存收益。
var newValidator = sync.OnceValue(func() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	// 报错里的字段名用 koanf 键而不是 Go 字段名，让报错直接对应配置键路径。
	v.RegisterTagNameFunc(func(fld reflect.StructField) string {
		tag, _, _ := strings.Cut(fld.Tag.Get("koanf"), ",")
		if tag == "" || tag == "-" {
			return fld.Name
		}
		return tag
	})
	return v
})

func validate(cfg any) error {
	err := newValidator().Struct(cfg)
	if err == nil {
		return nil
	}
	if ferrs, ok := errors.AsType[validator.ValidationErrors](err); ok {
		msgs := make([]string, 0, len(ferrs))
		details := make(map[string]string, len(ferrs))
		for _, fe := range ferrs {
			field := keyPath(fe.Namespace())
			rule := fe.Tag()
			if fe.Param() != "" {
				rule += "=" + fe.Param()
			}
			details[field] = rule
			msgs = append(msgs, field+": "+rule)
		}
		e := apperr.InvalidArgument("配置校验失败（%d 处）：%s", len(ferrs), strings.Join(msgs, "; "))
		for field, rule := range details {
			e = e.WithDetail(field, rule)
		}
		return e.WithCause(fmt.Errorf("配置校验: %w", err))
	}
	if _, ok := errors.AsType[*validator.InvalidValidationError](err); ok {
		// T 不是 struct，没有 validate 语义可言。
		return nil
	}
	return apperr.Internal(fmt.Errorf("配置校验执行失败: %w", err))
}

// keyPath 去掉 Namespace 里的根结构体类型名（Config.db.url → db.url）。
func keyPath(ns string) string {
	if _, rest, ok := strings.Cut(ns, delim); ok {
		return rest
	}
	return ns
}

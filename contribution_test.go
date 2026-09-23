package appkit

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

type testContribution struct {
	Value string
}

type testOtherContribution struct {
	Value string
}

type testContributionDependency struct {
	Value string
}

type testContributionCycleA struct{}
type testContributionCycleB struct{}

func assembleContributionTestApp(t *testing.T, app *App) {
	t.Helper()
	enabled, err := app.enabledModules()
	if err != nil {
		t.Fatal(err)
	}
	if err := app.register(enabled); err != nil {
		t.Fatal(err)
	}
}

func TestResolveContributionsEagerlySortedAndNamespaced(t *testing.T) {
	var alphaCalls, zetaCalls, otherCalls int
	var readDuringConstructor error
	var got []Contribution[testContribution]
	var gotOther []Contribution[testOtherContribution]
	var gotNamed testContribution

	dependency := ModuleFunc("dependency", func(reg *Registry) error {
		Provide[testContributionDependency](reg, func(r *Registry) (testContributionDependency, error) {
			_, readDuringConstructor = ResolveContributions[testContribution](r)
			return testContributionDependency{Value: "dependency"}, nil
		})
		return nil
	})
	catalog := ModuleFunc("catalog", func(reg *Registry) error {
		ProvideValueNamed[testContribution](reg, "alpha", testContribution{Value: "named binding"})
		Contribute[testContribution](reg, "zeta", func(r *Registry) (testContribution, error) {
			zetaCalls++
			dep, err := Resolve[testContributionDependency](r)
			return testContribution{Value: dep.Value + ":zeta"}, err
		})
		Contribute[testContribution](reg, "alpha", func(r *Registry) (testContribution, error) {
			alphaCalls++
			dep, err := Resolve[testContributionDependency](r)
			return testContribution{Value: dep.Value + ":alpha"}, err
		})
		return nil
	})
	other := ModuleFunc("other", func(reg *Registry) error {
		Contribute[testOtherContribution](reg, "alpha", func(*Registry) (testOtherContribution, error) {
			otherCalls++
			return testOtherContribution{Value: "other type"}, nil
		})
		return nil
	})
	consumer := ModuleFunc("consumer", func(reg *Registry) error {
		reg.Setup(func(ctx context.Context) error {
			var err error
			got, err = ResolveContributions[testContribution](reg)
			if err != nil {
				return err
			}
			gotOther, err = ResolveContributions[testOtherContribution](reg)
			if err != nil {
				return err
			}
			gotNamed, err = ResolveNamed[testContribution](reg, "alpha")
			return err
		})
		return nil
	})

	app := newTestApp([]Module{consumer, catalog, other, dependency})
	assembleContributionTestApp(t, app)
	if _, err := ResolveContributions[testContribution](app.reg); err == nil {
		t.Fatal("在依赖解析前读取 Contribution 应返回错误")
	}
	if alphaCalls != 0 || zetaCalls != 0 || otherCalls != 0 {
		t.Fatalf("Register 不应构造 Contribution: alpha=%d zeta=%d other=%d", alphaCalls, zetaCalls, otherCalls)
	}
	if err := app.reg.resolveAll(); err != nil {
		t.Fatal(err)
	}
	if readDuringConstructor == nil || !strings.Contains(readDuringConstructor.Error(), "依赖解析") {
		t.Fatalf("binding constructor 读取未完成的 Contribution 集合应失败，got %v", readDuringConstructor)
	}
	if alphaCalls != 1 || zetaCalls != 1 || otherCalls != 1 {
		t.Fatalf("resolveAll 应各构造一次: alpha=%d zeta=%d other=%d", alphaCalls, zetaCalls, otherCalls)
	}
	if err := app.reg.resolveAll(); err != nil {
		t.Fatal(err)
	}
	if alphaCalls != 1 || zetaCalls != 1 || otherCalls != 1 {
		t.Fatalf("重复 resolveAll 不应重建缓存值: alpha=%d zeta=%d other=%d", alphaCalls, zetaCalls, otherCalls)
	}
	if err := app.reg.runSetups(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "zeta" {
		t.Fatalf("Contribution 未按 name 排序: %+v", got)
	}
	if got[0].Module != "catalog" || got[0].Value.Value != "dependency:alpha" || got[1].Module != "catalog" || got[1].Value.Value != "dependency:zeta" {
		t.Fatalf("Contribution 的来源或依赖解析错误: %+v", got)
	}
	if len(gotOther) != 1 || gotOther[0].Name != "alpha" || gotOther[0].Module != "other" || gotOther[0].Value.Value != "other type" {
		t.Fatalf("不同类型应允许同名 Contribution: %+v", gotOther)
	}
	if gotNamed.Value != "named binding" {
		t.Fatalf("Contribution 与 ProvideNamed 命名空间混用: %+v", gotNamed)
	}

	got[0].Name = "mutated"
	secondRead, err := ResolveContributions[testContribution](app.reg)
	if err != nil {
		t.Fatal(err)
	}
	if secondRead[0].Name != "alpha" {
		t.Fatalf("解析结果应是新切片快照，前次修改泄漏: %+v", secondRead)
	}
}

func TestContributionConstructionOrderDoesNotDependOnModuleOrder(t *testing.T) {
	buildOrder := func(moduleNames []string) []string {
		t.Helper()
		var order []string
		modulesByName := map[string]Module{
			"zeta": ModuleFunc("zeta", func(reg *Registry) error {
				Contribute[testContribution](reg, "zeta", func(*Registry) (testContribution, error) {
					order = append(order, "zeta")
					return testContribution{}, nil
				})
				return nil
			}),
			"alpha": ModuleFunc("alpha", func(reg *Registry) error {
				Contribute[testContribution](reg, "alpha", func(*Registry) (testContribution, error) {
					order = append(order, "alpha")
					return testContribution{}, nil
				})
				return nil
			}),
			"other": ModuleFunc("other", func(reg *Registry) error {
				Contribute[testOtherContribution](reg, "alpha", func(*Registry) (testOtherContribution, error) {
					order = append(order, "other")
					return testOtherContribution{}, nil
				})
				return nil
			}),
		}
		modules := make([]Module, 0, len(moduleNames))
		for _, name := range moduleNames {
			modules = append(modules, modulesByName[name])
		}
		app := newTestApp(modules)
		assembleContributionTestApp(t, app)
		if err := app.reg.resolveAll(); err != nil {
			t.Fatal(err)
		}
		return order
	}

	first := buildOrder([]string{"zeta", "alpha", "other"})
	second := buildOrder([]string{"other", "alpha", "zeta"})
	if !slices.Equal(first, []string{"alpha", "zeta", "other"}) || !slices.Equal(second, first) {
		t.Fatalf("构造顺序应稳定且与 Register 顺序无关: first=%v second=%v", first, second)
	}
}

func TestTargetFiltersContributions(t *testing.T) {
	var excludedRegisterCalls, excludedConstructorCalls int
	excluded := ModuleFunc("provider", func(reg *Registry) error {
		excludedRegisterCalls++
		Contribute[testContribution](reg, "disabled", func(*Registry) (testContribution, error) {
			excludedConstructorCalls++
			return testContribution{Value: "excluded"}, nil
		})
		return nil
	})
	var values []Contribution[testContribution]
	gateway := ModuleFunc("gateway", func(reg *Registry) error {
		reg.Setup(func(context.Context) error {
			var err error
			values, err = ResolveContributions[testContribution](reg)
			return err
		})
		return nil
	})
	app := newTestApp([]Module{excluded, gateway}, Target("gateway"))
	assembleContributionTestApp(t, app)
	if err := app.reg.resolveAll(); err != nil {
		t.Fatal(err)
	}
	if err := app.reg.runSetups(t.Context()); err != nil {
		t.Fatal(err)
	}
	if excludedRegisterCalls != 0 || excludedConstructorCalls != 0 || len(values) != 0 {
		t.Fatalf("target 外 Contribution 被纳入: register=%d constructor=%d values=%+v", excludedRegisterCalls, excludedConstructorCalls, values)
	}
}

func TestDuplicateContributionFailsDuringRegister(t *testing.T) {
	first := ModuleFunc("first", func(reg *Registry) error {
		Contribute[testContribution](reg, "same", func(*Registry) (testContribution, error) {
			return testContribution{}, nil
		})
		return nil
	})
	second := ModuleFunc("second", func(reg *Registry) error {
		Contribute[testContribution](reg, "same", func(*Registry) (testContribution, error) {
			return testContribution{}, nil
		})
		return nil
	})
	app := newTestApp([]Module{first, second})
	enabled, err := app.enabledModules()
	if err != nil {
		t.Fatal(err)
	}
	err = app.register(enabled)
	if err == nil || !strings.Contains(err.Error(), "重复") || !strings.Contains(err.Error(), "first") || !strings.Contains(err.Error(), "second") {
		t.Fatalf("同类型同名 Contribution 应在 Register 阶段报告双方模块: %v", err)
	}
}

func TestContributionRejectsInvalidNameAndNilConstructor(t *testing.T) {
	for _, test := range []struct {
		name string
		call func(*Registry)
	}{
		{
			name: "invalid name",
			call: func(reg *Registry) {
				Contribute[testContribution](reg, "Invalid", func(*Registry) (testContribution, error) {
					return testContribution{}, nil
				})
			},
		},
		{
			name: "nil constructor",
			call: func(reg *Registry) {
				Contribute[testContribution](reg, "valid", nil)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			module := ModuleFunc("invalid", func(reg *Registry) error {
				test.call(reg)
				return nil
			})
			app := newTestApp([]Module{module})
			enabled, err := app.enabledModules()
			if err != nil {
				t.Fatal(err)
			}
			err = app.register(enabled)
			if err == nil {
				t.Fatal("非法声明应转成启动错误")
			}
		})
	}
}

func TestContributionConstructionErrorAndBindingCycleFailStartup(t *testing.T) {
	t.Run("constructor error", func(t *testing.T) {
		wantErr := errors.New("contribution unavailable")
		provider := ModuleFunc("broken-provider", func(reg *Registry) error {
			Contribute[testContribution](reg, "broken", func(*Registry) (testContribution, error) {
				return testContribution{}, wantErr
			})
			return nil
		})
		app := newTestApp([]Module{provider})
		assembleContributionTestApp(t, app)
		err := app.reg.resolveAll()
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "broken-provider") {
			t.Fatalf("构造失败应保留底层错误和来源模块: %v", err)
		}
	})

	t.Run("regular dependency cycle", func(t *testing.T) {
		var contributionCalls int
		provider := ModuleFunc("provider", func(reg *Registry) error {
			Contribute[testContribution](reg, "never", func(*Registry) (testContribution, error) {
				contributionCalls++
				return testContribution{}, nil
			})
			Provide[testContributionCycleA](reg, func(r *Registry) (testContributionCycleA, error) {
				_, err := Resolve[testContributionCycleB](r)
				return testContributionCycleA{}, err
			})
			Provide[testContributionCycleB](reg, func(r *Registry) (testContributionCycleB, error) {
				_, err := Resolve[testContributionCycleA](r)
				return testContributionCycleB{}, err
			})
			return nil
		})
		app := newTestApp([]Module{provider})
		assembleContributionTestApp(t, app)
		err := app.reg.resolveAll()
		if err == nil || !strings.Contains(err.Error(), "循环") || contributionCalls != 0 {
			t.Fatalf("普通绑定循环应在启动期失败且不构造 Contribution: err=%v calls=%d", err, contributionCalls)
		}
	})
}

func TestContributionCanOnlyBeRegisteredDuringModuleRegister(t *testing.T) {
	t.Run("outside Module.Register", func(t *testing.T) {
		defer func() {
			recovered := recover()
			message, ok := recovered.(string)
			if !ok || !strings.Contains(message, "Module.Register") {
				t.Fatalf("Module.Register 之外声明应 panic 并说明阶段，got %v", recovered)
			}
		}()
		Contribute[testContribution](newRegistry(), "outside", func(*Registry) (testContribution, error) {
			return testContribution{}, nil
		})
	})

	module := ModuleFunc("late", func(reg *Registry) error {
		reg.Setup(func(context.Context) error {
			Contribute[testContribution](reg, "late", func(*Registry) (testContribution, error) {
				return testContribution{}, nil
			})
			return nil
		})
		return nil
	})
	app := newTestApp([]Module{module})
	assembleContributionTestApp(t, app)
	if err := app.reg.resolveAll(); err != nil {
		t.Fatal(err)
	}
	err := app.reg.runSetups(t.Context())
	if err == nil || !strings.Contains(err.Error(), "Register") {
		t.Fatalf("Setup 阶段声明 Contribution 应 fail-fast: %v", err)
	}
}

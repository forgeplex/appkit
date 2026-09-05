package appkit

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestNamedBindingsSeparateInstancesAndDefault(t *testing.T) {
	reg := newRegistry()
	ProvideValue[greeter](reg, localGreeter{from: "default"})
	ProvideValueNamed[greeter](reg, "primary", localGreeter{from: "primary"})
	ProvideValueNamed[greeter](reg, "secondary", localGreeter{from: "secondary"})
	// 名字不是全局类型名：不同的 Go 类型可以使用同一个实例名。
	ProvideValueNamed(reg, "primary", 42)
	for _, name := range []string{"primary", "secondary"} {
		if got := MustResolveNamed[greeter](reg, name).Greet(); got != "hello from "+name {
			t.Errorf("%s = %q", name, got)
		}
	}
	if got := MustResolve[greeter](reg).Greet(); got != "hello from default" {
		t.Errorf("default = %q", got)
	}
	if got := MustResolveNamed[int](reg, "primary"); got != 42 {
		t.Errorf("primary int = %d", got)
	}
}

func TestNamedBindingsNeverFallbackAcrossNames(t *testing.T) {
	app := New([]Module{ModuleFunc("providers", func(reg *Registry) error {
		ProvideValue[greeter](reg, localGreeter{from: "default"})
		ProvideValueNamed[greeter](reg, "secondary", localGreeter{from: "secondary"})
		return nil
	})}, Remote(func(*Registry) (greeter, error) { return localGreeter{}, nil }),
		RemoteNamed("tertiary", func(*Registry) (greeter, error) { return localGreeter{}, nil }))
	if err := app.register(app.modules); err != nil {
		t.Fatal(err)
	}
	app.reg.current = "consumer"
	_, err := ResolveNamed[greeter](app.reg, "primary")
	if err == nil || !strings.Contains(err.Error(), `appkit.greeter["primary"]`) || !strings.Contains(err.Error(), "consumer") {
		t.Fatalf("missing exact name should identify instance and consumer: %v", err)
	}

	reg := newRegistry()
	ProvideValueNamed[greeter](reg, "primary", localGreeter{})
	if _, err := Resolve[greeter](reg); err == nil {
		t.Fatal("unnamed Resolve must not use a named binding")
	}
	namedMustPanic(t, func() { MustResolveNamed[greeter](reg, "missing") })
}

func TestNamedBindingLazyMemoizedAndNil(t *testing.T) {
	reg := newRegistry()
	var calls int
	ProvideNamed(reg, "primary", func(*Registry) (*localGreeter, error) {
		calls++
		return &localGreeter{from: "primary"}, nil
	})
	if calls != 0 {
		t.Fatal("ProvideNamed should not construct immediately")
	}
	a := MustResolveNamed[*localGreeter](reg, "primary")
	b := MustResolveNamed[*localGreeter](reg, "primary")
	if a != b || calls != 1 {
		t.Fatalf("instance not memoized: %p, %p; calls %d", a, b, calls)
	}
	ProvideValueNamed[greeter](reg, "optional", nil)
	if got, err := ResolveNamed[greeter](reg, "optional"); got != nil || err != nil {
		t.Fatalf("nil interface value = %v, %v", got, err)
	}
}

func TestNamedBindingLocalRemoteTargetSelection(t *testing.T) {
	for _, target := range []string{"all", "consumer"} {
		t.Run(target, func(t *testing.T) {
			var got greeter
			var remoteCalls int
			provider := ModuleFunc("provider", func(reg *Registry) error {
				ProvideValueNamed[greeter](reg, "primary", localGreeter{from: "local"})
				return nil
			})
			consumer := ModuleFunc("consumer", func(reg *Registry) error {
				Provide(reg, func(r *Registry) (int, error) {
					var err error
					got, err = ResolveNamed[greeter](r, "primary")
					return 1, err
				})
				return nil
			})
			app := New([]Module{provider, consumer}, Target(target),
				RemoteNamed("primary", func(*Registry) (greeter, error) {
					remoteCalls++
					return localGreeter{from: "remote"}, nil
				}),
				RemoteNamed("unused", func(*Registry) (greeter, error) {
					t.Fatal("unused remote must remain lazy")
					return nil, nil
				}),
			)
			enabled, err := app.enabledModules()
			if err != nil {
				t.Fatal(err)
			}
			if err := app.register(enabled); err != nil {
				t.Fatal(err)
			}
			if err := app.reg.resolveAll(); err != nil {
				t.Fatal(err)
			}
			want, wantCalls := "hello from local", 0
			if target == "consumer" {
				want, wantCalls = "hello from remote", 1
			}
			if got.Greet() != want || remoteCalls != wantCalls {
				t.Fatalf("got %q / %d calls; want %q / %d", got.Greet(), remoteCalls, want, wantCalls)
			}
			if again := MustResolveNamed[greeter](app.reg, "primary"); again != got || remoteCalls != wantCalls {
				t.Fatal("resolved named remote should be cached")
			}
		})
	}
}

func TestNamedBindingDuplicateRegistrations(t *testing.T) {
	app := New([]Module{
		ModuleFunc("first", func(reg *Registry) error {
			ProvideValueNamed(reg, "primary", 1)
			return nil
		}),
		ModuleFunc("second", func(reg *Registry) error {
			ProvideValueNamed(reg, "primary", 2)
			return nil
		}),
	})
	if err := app.register(app.modules); err == nil || !strings.Contains(err.Error(), "first") || !strings.Contains(err.Error(), "second") {
		t.Fatalf("duplicate local should identify both modules: %v", err)
	}

	app = New(nil,
		RemoteNamed("primary", func(*Registry) (int, error) { return 1, nil }),
		RemoteNamed("primary", func(*Registry) (int, error) { return 2, nil }),
	)
	if err := app.register(nil); err == nil || !strings.Contains(err.Error(), "RemoteNamed") {
		t.Fatalf("duplicate remote should fail registration: %v", err)
	}

	// 原有 Remote 的后注册覆盖行为不改变。
	app = New(nil,
		Remote(func(*Registry) (int, error) { return 1, nil }),
		Remote(func(*Registry) (int, error) { return 2, nil }),
	)
	if err := app.register(nil); err != nil {
		t.Fatal(err)
	}
	if got := MustResolve[int](app.reg); got != 2 {
		t.Fatalf("default Remote semantics changed: %d", got)
	}
}

func TestNamedBindingNameValidation(t *testing.T) {
	for _, name := range []string{"", " ", "primary ", " primary", "Primary", "1primary", "primary/one", "primary\n", "主实例"} {
		t.Run(name, func(t *testing.T) {
			reg := newRegistry()
			namedMustPanic(t, func() { ProvideValueNamed(reg, name, 1) })
			namedMustPanic(t, func() { RemoteNamed(name, func(*Registry) (int, error) { return 1, nil }) })
			if _, err := ResolveNamed[int](reg, name); err == nil {
				t.Fatal("invalid name should fail resolution")
			}
		})
	}
	for _, name := range []string{"a", "primary", "email.transactional", "primary-1", "primary_2"} {
		t.Run(name, func(t *testing.T) {
			reg := newRegistry()
			ProvideValueNamed(reg, name, 1)
			if got := MustResolveNamed[int](reg, name); got != 1 {
				t.Fatalf("valid name resolved to %d", got)
			}
		})
	}
	namedMustPanic(t, func() { ProvideNamed[int](newRegistry(), "primary", nil) })
	namedMustPanic(t, func() { RemoteNamed[int]("primary", nil) })
}

func TestNamedBindingCyclesIncludeInstanceNames(t *testing.T) {
	reg := newRegistry()
	Provide(reg, func(r *Registry) (int, error) { return ResolveNamed[int](r, "primary") })
	ProvideNamed(reg, "primary", func(r *Registry) (int, error) { return ResolveNamed[int](r, "secondary") })
	ProvideNamed(reg, "secondary", func(r *Registry) (int, error) { return Resolve[int](r) })
	const wantPath = `int → int["primary"] → int["secondary"] → int`
	for range 2 {
		_, err := Resolve[int](reg)
		if err == nil || !strings.Contains(err.Error(), wantPath) {
			t.Fatalf("want cycle path %s, got %v", wantPath, err)
		}
	}
	if len(reg.resolving) != 0 {
		t.Fatalf("resolution stack not restored: %v", reg.resolving)
	}
}

func TestNamedBindingEagerResolutionDeterministic(t *testing.T) {
	for range 20 {
		reg := newRegistry()
		var order []string
		for _, name := range []string{"secondary", "primary"} {
			ProvideNamed(reg, name, func(*Registry) (int, error) {
				order = append(order, name)
				return 1, nil
			})
		}
		Provide(reg, func(*Registry) (int, error) {
			order = append(order, "default")
			return 1, nil
		})
		if err := reg.resolveAll(); err != nil {
			t.Fatal(err)
		}
		if want := []string{"default", "primary", "secondary"}; !reflect.DeepEqual(order, want) {
			t.Fatalf("construction order = %v, want %v", order, want)
		}
	}
}

func TestNamedBindingMissingDependencyFailsBeforeSetup(t *testing.T) {
	setup := false
	app := New([]Module{ModuleFunc("consumer", func(reg *Registry) error {
		ProvideNamed(reg, "primary", func(r *Registry) (int, error) { return ResolveNamed[int](r, "missing") })
		reg.Setup(func(context.Context) error { setup = true; return nil })
		return nil
	})}, HTTPAddr("127.0.0.1:0"))
	if err := app.Run(context.Background()); err == nil || !strings.Contains(err.Error(), `int["missing"]`) {
		t.Fatalf("expected missing named dependency at startup: %v", err)
	}
	if setup {
		t.Fatal("Setup should not run after named resolution failure")
	}
}

func TestProvideContractNamedWrapsOnce(t *testing.T) {
	reg := newRegistry()
	wrapCalls := 0
	ProvideContractNamed(reg, "primary",
		func(*Registry) (greeter, error) { return localGreeter{from: "local"}, nil },
		func(g greeter) greeter { wrapCalls++; return wrappedGreeter{inner: g} },
	)
	for range 2 {
		if got := MustResolveNamed[greeter](reg, "primary").Greet(); got != "wrapped(hello from local)" {
			t.Fatalf("unwrapped contract: %q", got)
		}
	}
	if wrapCalls != 1 {
		t.Fatalf("wrapper applied %d times", wrapCalls)
	}
	namedMustPanic(t, func() {
		ProvideContractNamed(reg, "other", func(*Registry) (greeter, error) { return nil, nil }, nil)
	})
	namedMustPanic(t, func() {
		ProvideContractNamed(reg, "other", nil, func(g greeter) greeter { return g })
	})
}

func TestNamedConstructorFailurePreservesCause(t *testing.T) {
	reg := newRegistry()
	want := errors.New("constructor failed")
	ProvideContractNamed(reg, "primary",
		func(*Registry) (greeter, error) { return nil, want },
		func(g greeter) greeter { t.Fatal("must not wrap a failed constructor"); return g },
	)
	for range 2 {
		_, err := ResolveNamed[greeter](reg, "primary")
		if !errors.Is(err, want) || !strings.Contains(err.Error(), `appkit.greeter["primary"]`) {
			t.Fatalf("constructor error lost: %v", err)
		}
	}
}

func namedMustPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Error("expected panic")
		}
	}()
	fn()
}

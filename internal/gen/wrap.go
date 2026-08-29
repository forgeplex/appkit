package gen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// contractPath 是 wrapper 方法体依赖的契约边界包。
const contractPath = "github.com/forgeplex/appkit/contract"

// shapeDoc 出现在所有签名校验错误里：这不是生成器的技术限制，
// 而是框架对契约粒度的约束（DESIGN §5.3——契约调用是可失败的粗粒度边界）。
const shapeDoc = "契约方法必须是「ctx + 至多一个请求 DTO → 至多一个响应 + error」的粗粒度形态，" +
	"即 (ctx context.Context[, req T]) (resp U, error) 或 (ctx context.Context[, req T]) error"

// method 是一条通过校验的契约方法。
type method struct {
	name string
	req  ast.Expr // 可为 nil
	resp ast.Expr // 可为 nil（仅 error 方法）
}

// Wrap 解析 srcDir 包中的接口 iface，生成同包的契约 wrapper 文件：
// wrapped<Iface> 每个方法体经 contract.Call（事务守卫/ctx 防火墙/超时/错误规范化），
// Wrap<Iface> 供 appkit.ProvideContract 使用——裸实现进不了 registry。
func Wrap(srcDir, iface, system, outPath string) error {
	if srcDir == "" || iface == "" || system == "" || outPath == "" {
		return fmt.Errorf("gen wrap 需要 -src <pkgdir> -iface <Name> -system <name> -out <file.go>")
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9]*$`).MatchString(system) {
		return fmt.Errorf("-system %q 非法（须匹配 ^[a-z][a-z0-9]*$，如 ledger）", system)
	}

	fset := token.NewFileSet()
	file, decl, err := findInterface(fset, srcDir, iface)
	if err != nil {
		return err
	}
	methods, err := checkMethods(fset, iface, decl)
	if err != nil {
		return err
	}
	imports, err := collectImports(fset, file, iface, methods)
	if err != nil {
		return err
	}
	return writeGo(outPath, renderWrap(file.Name.Name, iface, system, methods, imports))
}

// findInterface 逐文件解析 srcDir（跳过 _test.go），定位 iface 的接口声明。
func findInterface(fset *token.FileSet, srcDir, iface string) (*ast.File, *ast.InterfaceType, error) {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return nil, nil, fmt.Errorf("读取 -src 目录: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(srcDir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, nil, fmt.Errorf("解析源码: %w", err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != iface {
					continue
				}
				it, ok := ts.Type.(*ast.InterfaceType)
				if !ok {
					return nil, nil, fmt.Errorf("%s: %s 不是接口类型", fset.Position(ts.Pos()), iface)
				}
				if ts.TypeParams != nil {
					return nil, nil, fmt.Errorf("%s: 不支持泛型接口 %s（契约接口必须是具体类型）", fset.Position(ts.Pos()), iface)
				}
				return f, it, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("在 %s 中未找到接口 %s（检查 -src 与 -iface）", srcDir, iface)
}

// checkMethods 校验每个方法的契约形态并抽取 req/resp 类型。
func checkMethods(fset *token.FileSet, iface string, it *ast.InterfaceType) ([]method, error) {
	if len(it.Methods.List) == 0 {
		return nil, fmt.Errorf("%s: 接口 %s 没有方法", fset.Position(it.Pos()), iface)
	}
	var out []method
	for _, f := range it.Methods.List {
		ft, ok := f.Type.(*ast.FuncType)
		if !ok || len(f.Names) == 0 {
			return nil, fmt.Errorf("%s: 接口 %s 不支持嵌入接口（%s），请平铺方法声明",
				fset.Position(f.Pos()), iface, types.ExprString(f.Type))
		}
		for _, name := range f.Names {
			m, reason := checkSignature(name.Name, ft)
			if reason != "" {
				return nil, fmt.Errorf("%s: 接口 %s 的方法 %s：%s；%s",
					fset.Position(f.Pos()), iface, name.Name, reason, shapeDoc)
			}
			out = append(out, m)
		}
	}
	return out, nil
}

// checkSignature 返回通过校验的方法，或非空的失败原因。
func checkSignature(name string, ft *ast.FuncType) (method, string) {
	var params []ast.Expr
	for _, p := range ft.Params.List {
		if _, ok := p.Type.(*ast.Ellipsis); ok {
			return method{}, "禁止变参"
		}
		n := max(len(p.Names), 1)
		for range n {
			params = append(params, p.Type)
		}
	}
	if len(params) == 0 {
		return method{}, "没有参数（首参必须是 context.Context）"
	}
	if !isContextContext(params[0]) {
		return method{}, "首参必须是 context.Context"
	}
	if len(params) > 2 {
		return method{}, "参数过多（除 ctx 外至多一个请求 DTO）"
	}

	var results []ast.Expr
	if ft.Results != nil {
		for _, r := range ft.Results.List {
			n := max(len(r.Names), 1)
			for range n {
				results = append(results, r.Type)
			}
		}
	}
	if len(results) == 0 {
		return method{}, "没有返回值（最后一个返回值必须是 error）"
	}
	if len(results) > 2 {
		return method{}, "返回值过多（至多一个响应 + error）"
	}
	if id, ok := results[len(results)-1].(*ast.Ident); !ok || id.Name != "error" {
		return method{}, "最后一个返回值必须是 error"
	}

	m := method{name: name}
	if len(params) == 2 {
		m.req = params[1]
	}
	if len(results) == 2 {
		m.resp = results[0]
	}
	return m, ""
}

func isContextContext(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Context" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "context"
}

// impSpec 是一条输出 import；alias 为空表示无别名。
type impSpec struct{ alias, path string }

// collectImports 收集生成文件所需 imports：context/time/contract 固定项 +
// 方法签名里经限定符引用的源文件 imports（保留源文件的别名）。
func collectImports(fset *token.FileSet, file *ast.File, iface string, methods []method) ([]impSpec, error) {
	quals := map[string]bool{}
	for _, m := range methods {
		for _, e := range []ast.Expr{m.req, m.resp} {
			if e == nil {
				continue
			}
			ast.Inspect(e, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok {
						quals[id.Name] = true
					}
				}
				return true
			})
		}
	}

	set := map[impSpec]bool{
		{path: "context"}:    true,
		{path: "time"}:       true,
		{path: contractPath}: true,
	}
	for q := range quals {
		if q == "context" {
			continue
		}
		spec := findImport(file, q)
		if spec == nil {
			return nil, fmt.Errorf("%s: 接口 %s 的方法签名引用了限定符 %q，但本文件没有对应 import",
				fset.Position(file.Pos()), iface, q)
		}
		p, _ := strconv.Unquote(spec.Path.Value)
		s := impSpec{path: p}
		if spec.Name != nil {
			s.alias = spec.Name.Name
		}
		set[s] = true
	}

	out := make([]impSpec, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].path != out[j].path {
			return out[i].path < out[j].path
		}
		return out[i].alias < out[j].alias
	})
	return out, nil
}

// findImport 在 file 的 imports 中按限定符（别名或推断的包名）查找。
func findImport(file *ast.File, qual string) *ast.ImportSpec {
	for _, spec := range file.Imports {
		if spec.Name != nil {
			if spec.Name.Name == qual {
				return spec
			}
			continue
		}
		p, _ := strconv.Unquote(spec.Path.Value)
		if guessPkgName(p) == qual {
			return spec
		}
	}
	return nil
}

var reMajorVer = regexp.MustCompile(`^v[0-9]+$`)

// guessPkgName 从 import path 推断包名（处理 /v5 与 yaml.v3 形式的路径尾）。
func guessPkgName(p string) string {
	segs := strings.Split(p, "/")
	base := segs[len(segs)-1]
	if reMajorVer.MatchString(base) && len(segs) > 1 {
		base = segs[len(segs)-2]
	}
	if i := strings.Index(base, "."); i >= 0 {
		base = base[:i]
	}
	return base
}

func isStdlib(p string) bool {
	return !strings.Contains(strings.SplitN(p, "/", 2)[0], ".")
}

func renderWrap(pkg, iface, system string, methods []method, imports []impSpec) []byte {
	var b bytes.Buffer
	b.WriteString(header)
	fmt.Fprintf(&b, "package %s\n\n", pkg)

	var std, rest []impSpec
	for _, s := range imports {
		if isStdlib(s.path) {
			std = append(std, s)
		} else {
			rest = append(rest, s)
		}
	}
	b.WriteString("import (\n")
	for _, s := range std {
		writeImport(&b, s)
	}
	if len(rest) > 0 {
		b.WriteString("\n")
	}
	for _, s := range rest {
		writeImport(&b, s)
	}
	b.WriteString(")\n\n")

	fmt.Fprintf(&b, "// wrapped%[1]s 是 %[1]s 的契约边界 wrapper：每个方法体经 contract.Call\n", iface)
	b.WriteString("// 获得事务守卫、ctx 防火墙、独立超时与错误规范化（DESIGN §5.3）。\n")
	fmt.Fprintf(&b, "type wrapped%[1]s struct {\n\tinner   %[1]s\n\ttimeout time.Duration\n}\n\n", iface)

	fmt.Fprintf(&b, "// Wrap%[1]s 把实现包进契约边界，供 appkit.ProvideContract 使用；\n", iface)
	b.WriteString("// timeout <= 0 时使用 contract.DefaultTimeout。\n")
	fmt.Fprintf(&b, "func Wrap%[1]s(inner %[1]s, timeout time.Duration) %[1]s {\n", iface)
	b.WriteString("\tif timeout <= 0 {\n\t\ttimeout = contract.DefaultTimeout\n\t}\n")
	fmt.Fprintf(&b, "\treturn wrapped%s{inner: inner, timeout: timeout}\n}\n\n", iface)

	for _, m := range methods {
		reqSig, callArg := "", ""
		if m.req != nil {
			reqSig = ", req " + types.ExprString(m.req)
			callArg = ", req"
		}
		if m.resp != nil {
			resp := types.ExprString(m.resp)
			fmt.Fprintf(&b, "func (w wrapped%s) %s(ctx context.Context%s) (%s, error) {\n", iface, m.name, reqSig, resp)
			fmt.Fprintf(&b, "\treturn contract.Call(ctx, %q, %q, w.timeout, func(ctx context.Context) (%s, error) {\n", system, m.name, resp)
			fmt.Fprintf(&b, "\t\treturn w.inner.%s(ctx%s)\n\t})\n}\n\n", m.name, callArg)
			continue
		}
		fmt.Fprintf(&b, "func (w wrapped%s) %s(ctx context.Context%s) error {\n", iface, m.name, reqSig)
		fmt.Fprintf(&b, "\t_, err := contract.Call(ctx, %q, %q, w.timeout, func(ctx context.Context) (struct{}, error) {\n", system, m.name)
		fmt.Fprintf(&b, "\t\treturn struct{}{}, w.inner.%s(ctx%s)\n\t})\n\treturn err\n}\n\n", m.name, callArg)
	}
	return b.Bytes()
}

func writeImport(b *bytes.Buffer, s impSpec) {
	if s.alias != "" {
		fmt.Fprintf(b, "\t%s %q\n", s.alias, s.path)
		return
	}
	fmt.Fprintf(b, "\t%q\n", s.path)
}

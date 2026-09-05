package gen

import (
	"bytes"
	"fmt"
)

// 契约的 HTTP 形态唯一：POST + JSON body，错误一律 problem+json。
// client.gen.go 与 server.gen.go 由同一份 contractDoc 渲染，编解码只有一份约定。

// renderClient 渲染 client.gen.go：Service 的远程绑定，实现同一接口。
// 方法体经 contract.Call（与进程内 wrapper 共享边界语义），幂等方法
// 对 CodeUnavailable 做有界重试。
func renderClient(doc *contractDoc) []byte {
	needRetry := false
	needBytes := false
	for _, m := range doc.Methods {
		if m.Idempotent {
			needRetry = true
		}
		if len(m.Request) > 0 {
			needBytes = true
		}
	}

	var b bytes.Buffer
	b.WriteString(header)
	fmt.Fprintf(&b, "package %s\n\n", doc.Package)
	b.WriteString("import (\n")
	if needBytes {
		b.WriteString("\t\"bytes\"\n")
	}
	b.WriteString("\t\"context\"\n\t\"encoding/json\"\n\t\"io\"\n\t\"net/http\"\n\t\"strings\"\n")
	if needRetry {
		b.WriteString("\t\"time\"\n")
	}
	b.WriteString(`
	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/callctx"
	"github.com/forgeplex/appkit/contract"
)

`)

	fmt.Fprintf(&b, "// Client 是 %s 契约的远程绑定：与进程内 wrapper（wrap.gen.go）实现\n", doc.System)
	b.WriteString("// 同一个 Service 接口，方法体同样经 contract.Call——事务守卫、ctx 防火墙、\n// 超时与错误规范化在两种部署形态下完全一致（DESIGN §5.3）。\n")
	b.WriteString("type Client struct {\n\tbase string\n\thc   *http.Client\n\tsecure bool\n}\n\n")

	b.WriteString("// NewClient 返回契约 client。base 是服务根地址（如 \"http://ledger:8080\"），\n")
	b.WriteString("// caller 填本服务名（callctx 白名单的归因值）。\n//\n")
	b.WriteString("// 这是兼容旧行为的 legacy/dev 入口，不提供服务认证或 HTTPS 强制；\n// 生产服务调用使用 NewSecureClient。\n//\n")
	b.WriteString("// 白名单传播焊死在装配处：hc 为 nil 走默认 Transport；非 nil 时在其\n")
	b.WriteString("// Transport 外包一层 callctx.Transport（不改调用方的 client）。\n")
	b.WriteString("// 「忘了装 Transport」这个静默失效形态在生成 client 里不存在。\n")
	b.WriteString("func NewClient(base, caller string, hc *http.Client) *Client {\n")
	b.WriteString("\tif hc == nil {\n\t\thc = &http.Client{}\n\t}\n")
	b.WriteString("\tinner := *hc\n\tinner.Transport = callctx.Transport{Base: hc.Transport, Caller: caller}\n")
	b.WriteString("\treturn &Client{base: strings.TrimSuffix(base, \"/\"), hc: &inner}\n}\n\n")
	b.WriteString(`// NewSecureClient 返回显式认证的 HTTPS 契约 client。Audience 与服务凭证
// provider 必填；每次尝试取新凭证，拒绝过期凭证、不安全 transport 和重定向。
// caller 由服务 JWT 的 subject 决定，不接受调用方伪造的 caller 或用户凭证。
// Partition/TenantID 经 contract ctx 防火墙传给 provider，由其明确授权委托。
func NewSecureClient(base string, opts contract.SecureClientOptions) (*Client, error) {
	hc, err := contract.NewSecureHTTPClient(base, opts)
	if err != nil {
		return nil, err
	}
	return &Client{base: strings.TrimSuffix(base, "/"), hc: hc, secure: true}, nil
}

`)

	b.WriteString("// 编译期断言：Client 与 Service 漂移时本包编译失败。\nvar _ Service = (*Client)(nil)\n\n")

	for _, m := range doc.Methods {
		renderClientMethod(&b, doc, m)
	}

	renderDo(&b)
	if needRetry {
		renderRetry(&b)
	}
	return b.Bytes()
}

// renderClientMethod 渲染一个 client 方法。四种形态：req/resp 有无 ×
// 幂等（外包 retryUnavailable）与否。
func renderClientMethod(b *bytes.Buffer, doc *contractDoc, m methodDef) {
	hasReq := len(m.Request) > 0
	hasResp := len(m.Response) > 0
	respType := m.Name + "Reply"
	if !hasResp {
		respType = "struct{}"
	}

	sig := fmt.Sprintf("%s(ctx context.Context", m.Name)
	if hasReq {
		sig += ", req " + m.Name + "Request"
	}
	sig += ")"
	if hasResp {
		sig += fmt.Sprintf(" (%s, error)", respType)
	} else {
		sig += " error"
	}

	fmt.Fprintf(b, "func (c *Client) %s {\n", sig)
	call := fmt.Sprintf("contract.Call(ctx, %q, %q, 0, func(ctx context.Context) (%s, error) {\n", doc.System, m.Name, respType)
	body := ""
	if hasReq {
		body = fmt.Sprintf(`		body, err := json.Marshal(req)
		if err != nil {
			return %s{}, apperr.Internal(err)
		}
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+%q, bytes.NewReader(body))
`, respType, m.Path)
	} else {
		body = fmt.Sprintf(`		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+%q, http.NoBody)
`, m.Path)
	}
	body += fmt.Sprintf(`		if err != nil {
			return %s{}, apperr.Internal(err)
		}
		hreq.Header.Set("Content-Type", "application/json")
		return do[%s](c.hc, hreq, c.secure)
	})`, respType, respType)

	switch {
	case hasResp && m.Idempotent:
		fmt.Fprintf(b, "\treturn retryUnavailable(ctx, func() (%s, error) {\n\t\treturn %s\n\t})\n}\n\n", respType, call+body)
	case hasResp:
		fmt.Fprintf(b, "\treturn %s\n}\n\n", call+body)
	case m.Idempotent:
		fmt.Fprintf(b, "\t_, err := retryUnavailable(ctx, func() (struct{}, error) {\n\t\treturn %s\n\t})\n\treturn err\n}\n\n", call+body)
	default:
		fmt.Fprintf(b, "\t_, err := %s\n\treturn err\n}\n\n", call+body)
	}
}

// renderDo 渲染共享的 HTTP 执行与解码 helper。
func renderDo(b *bytes.Buffer) {
	b.WriteString(`// do 执行一次 HTTP 调用并按契约约定解码：200 解析 JSON 响应体，其余
// 状态一律经 apperr.FromProblem 重建错误——错误身份 = 错误码，跨网络重建后
// apperr.Is 判定与进程内一致。网络层故障折叠为 CodeUnavailable（可重试信号）。
func do[T any](hc *http.Client, hreq *http.Request, secure bool) (T, error) {
	var zero T
	resp, err := hc.Do(hreq)
	if err != nil {
		if secure && (apperr.Is(err, apperr.CodeUnauthenticated) || apperr.Is(err, apperr.CodePermissionDenied) || apperr.Is(err, apperr.CodeInvalidArgument)) {
			return zero, apperr.From(err)
		}
		return zero, apperr.Unavailable(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return zero, apperr.Unavailable(err)
	}
	if resp.StatusCode != http.StatusOK {
		return zero, apperr.FromProblem(resp.StatusCode, raw)
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return zero, apperr.Internal(err)
	}
	return v, nil
}

`)
}

// renderRetry 渲染幂等方法的有界重试 helper（仅当存在幂等方法时生成）。
func renderRetry(b *bytes.Buffer) {
	b.WriteString(`// 重试策略是合约的一部分，不由调用点自由发挥：次数封顶、间隔固定。
const (
	retryMaxAttempts = 3
	retryBackoff     = 100 * time.Millisecond
)

// retryUnavailable 仅包裹声明了 idempotent 的契约方法：只有失败是可重试信号
// （CodeUnavailable——网络错误、502–504、对端超时，契约边界统一折叠）才再试，
// 其余错误码是确定性的业务结果，重试只会放大伤害。非幂等方法不生成此调用——
// 「重复执行是否安全」是方法的语义属性，声明在 contract.yaml 里。
func retryUnavailable[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	v, err := fn()
	for attempt := 1; attempt < retryMaxAttempts && err != nil && apperr.Is(err, apperr.CodeUnavailable); attempt++ {
		timer := time.NewTimer(retryBackoff * time.Duration(attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return v, err
		case <-timer.C:
		}
		v, err = fn()
	}
	return v, err
}
`)
}

// renderServer 渲染 server.gen.go：把 Service 暴露为 HTTP，与 client.gen.go
// 互为镜像。提供方装配：NewHTTPHandler(WrapService(impl, 0))——wrapper 在
// 边界内侧再经一次 contract.Call，两种部署形态语义对齐。
func renderServer(doc *contractDoc) []byte {
	strictRefs := false
	for _, m := range doc.Methods {
		strictRefs = strictRefs || doc.fieldsUseRefs(m.Request)
	}
	var b bytes.Buffer
	b.WriteString(header)
	fmt.Fprintf(&b, "package %s\n\n", doc.Package)
	if strictRefs {
		b.WriteString("import (\n\t\"bytes\"\n\t\"io\"\n\t\"strings\"\n\t\"unicode\"\n")
	} else {
		b.WriteString("import (\n")
	}
	b.WriteString(`	"encoding/json"
	"net/http"

	"github.com/forgeplex/appkit/apperr"
	"github.com/forgeplex/appkit/callctx"
)

`)

	fmt.Fprintf(&b, "// NewHTTPHandler 把 Service 暴露为 HTTP：每方法一条 POST 路由，请求体\n")
	b.WriteString("// JSON decode，错误经 apperr.WriteProblem，成功 200 JSON。与 client.gen.go\n")
	b.WriteString("// 由同一份 contract.yaml 生成，编解码约定只有一份。装配形态：\n//\n")
	fmt.Fprintf(&b, "//\treg.MountInternalService(\"/\", %s.NewHTTPHandler(%s.WrapService(impl, 0)))\n", doc.Package, doc.Package)
	b.WriteString("//\n// 生产必须配置严格 App 服务认证链并分类挂载；本 handler 只做编解码，\n// 不验凭证。裸 handler 的 unsigned tenant/partition/caller 头不构成授权。\n")
	b.WriteString("func NewHTTPHandler(svc Service) http.Handler {\n\tmux := http.NewServeMux()\n")
	for _, m := range doc.Methods {
		fmt.Fprintf(&b, "\tmux.HandleFunc(%q, func(w http.ResponseWriter, r *http.Request) {\n", "POST "+m.Path)
		hasReq := len(m.Request) > 0
		call := "svc." + m.Name + "(r.Context()"
		if hasReq {
			call += ", req"
		}
		call += ")"
		if hasReq {
			fmt.Fprintf(&b, "\t\tvar req %sRequest\n", m.Name)
			if doc.fieldsUseRefs(m.Request) {
				b.WriteString("\t\tif err := decodeRefsRequest(r.Body, &req); err != nil {\n")
			} else {
				b.WriteString("\t\tif err := json.NewDecoder(r.Body).Decode(&req); err != nil {\n")
			}
			b.WriteString("\t\t\tapperr.WriteProblem(w, apperr.InvalidArgument(\"请求体不是合法 JSON\"))\n\t\t\treturn\n\t\t}\n")
		}
		if len(m.Response) > 0 {
			fmt.Fprintf(&b, "\t\treply, err := %s\n", call)
			b.WriteString("\t\tif err != nil {\n\t\t\tapperr.WriteProblem(w, err)\n\t\t\treturn\n\t\t}\n")
			b.WriteString("\t\twriteJSON(w, reply)\n")
		} else {
			fmt.Fprintf(&b, "\t\tif err := %s; err != nil {\n", call)
			b.WriteString("\t\t\tapperr.WriteProblem(w, err)\n\t\t\treturn\n\t\t}\n")
			// 仅 error 方法成功也写 200 + 空对象：client 侧解码形态唯一。
			b.WriteString("\t\twriteJSON(w, struct{}{})\n")
		}
		b.WriteString("\t})\n")
	}
	b.WriteString("\treturn serve(mux)\n}\n\n")
	if strictRefs {
		renderRefsRequestDecoder(&b)
	}
	b.WriteString(`// serve 保留 legacy/dev 的白名单传输行为，不是认证边界。严格 App 在最外层
// 清掉 unsigned 身份头，并由服务验证器重建 ctx；生产须挂 MountInternalService。
// 裸挂时从头提取的 callctx.Meta 仍不可信，不可据此授权。
// request id 的生成与响应回写仍是外层中间件的职责，这里不越权。
func serve(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(callctx.Merge(r.Context(), callctx.Extract(r.Header.Get))))
	})
}

`)
	b.WriteString(`// writeJSON 是成功响应的唯一出口。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
`)
	return b.Bytes()
}

// renderRefsRequestDecoder keeps the opt-in refs wire shape unambiguous even
// when an outer DTO field is repeated. Values.UnmarshalJSON alone cannot detect
// a later duplicate field in its containing object. No resource schema, external
// relationship or authorization is inferred by this transport-level check.
func renderRefsRequestDecoder(b *bytes.Buffer) {
	b.WriteString(`// decodeRefsRequest rejects ambiguous envelopes as well as malformed refs.
// Missing fields still use their zero values; domain Schema validation decides
// required references. Unknown fields remain allowed for contract evolution.
// These opt-in requests are limited to 1 MiB and 64 JSON nesting levels.
const (
	maxRefsRequestBytes = 1 << 20
	maxRefsRequestDepth = 64
)

func decodeRefsRequest(body io.Reader, dst any) error {
	raw, err := io.ReadAll(io.LimitReader(body, maxRefsRequestBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maxRefsRequestBytes {
		return apperr.InvalidArgument("JSON request is too large")
	}
	if raw = bytes.Trim(raw, " \t\r\n"); len(raw) == 0 || raw[0] != '{' {
		return apperr.InvalidArgument("expected a JSON object")
	}
	// The standard decoder validates syntax and the single-document boundary;
	// refs.Values performs its strict flat-value decoding during this call.
	if err := json.Unmarshal(raw, dst); err != nil {
		return err
	}
	tokens := json.NewDecoder(bytes.NewReader(raw))
	tokens.UseNumber()
	return uniqueJSONValue(tokens, 0)
}

func uniqueJSONValue(dec *json.Decoder, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if _, container := tok.(json.Delim); container && depth >= maxRefsRequestDepth {
		return apperr.InvalidArgument("JSON request is too deeply nested")
	}
	switch tok {
	case json.Delim('{'):
		seen := map[string]bool{}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			// encoding/json matches DTO fields using Unicode case folding.
			// Reject case aliases too, instead of silently replacing a field.
			folded := foldJSONKey(key.(string))
			if seen[folded] {
				return apperr.InvalidArgument("duplicate JSON object key")
			}
			seen[folded] = true
			if err := uniqueJSONValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case json.Delim('['):
		for dec.More() {
			if err := uniqueJSONValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	default:
		return nil
	}
}

func foldJSONKey(key string) string {
	var b strings.Builder
	for _, r := range key {
		min := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < min {
				min = next
			}
		}
		b.WriteRune(min)
	}
	return b.String()
}

`)
}

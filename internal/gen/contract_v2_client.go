package gen

import (
	"bytes"
	"fmt"
)

func renderClientV2(doc *contractDocV2) []byte {
	var b bytes.Buffer
	b.WriteString(header)
	fmt.Fprintf(&b, "package %s\n\n", doc.Package)
	b.WriteString("import (\n\t\"context\"\n")
	if hasV2Unary(doc) {
		if hasV2UnaryRequest(doc) {
			b.WriteString("\t\"bytes\"\n")
		}
		b.WriteString("\t\"encoding/json\"\n\t\"io\"\n\t\"net/http\"\n\t\"strings\"\n\t\"time\"\n")
	}
	b.WriteString("\n\t\"github.com/forgeplex/appkit/contract\"\n")
	if hasV2BidiStream(doc) {
		b.WriteString("\t\"github.com/forgeplex/appkit/httpserver\"\n")
	}
	if hasV2Unary(doc) {
		b.WriteString("\t\"github.com/forgeplex/appkit/apperr\"\n\t\"github.com/forgeplex/appkit/callctx\"\n")
	}
	b.WriteString(")\n\n")

	if hasV2Unary(doc) {
		renderV2UnaryWrapper(&b, doc)
		renderV2HTTPClient(&b, doc)
		renderV2HTTPDo(&b)
		if hasV2IdempotentUnary(doc) {
			renderV2Retry(&b)
		}
	}
	if hasV2Streaming(doc) {
		renderV2LocalStreamingClient(&b, doc)
	}
	if hasV2BidiStream(doc) {
		renderV2WebSocketClient(&b, doc)
	}
	return b.Bytes()
}

func renderV2WebSocketClient(b *bytes.Buffer, doc *contractDocV2) {
	for _, m := range doc.Methods {
		if m.Kind != "bidi_stream" {
			continue
		}
		request, response := v2RequestType(m), v2ResponseType(m)
		fmt.Fprintf(b, "// Dial%sWebSocketV2 opens the secure remote Bidi Stream for %s.\nfunc Dial%sWebSocketV2(ctx context.Context, address string, cfg httpserver.WebSocketConfig, secure contract.SecureClientOptions) (contract.ClientStream[%s, %s], error) {\n", m.Name, m.Path, m.Name, request, response)
		fmt.Fprintf(b, "\tcfg.System, cfg.Method = %q, %q\n", doc.System, m.Name)
		fmt.Fprintf(b, "\treturn httpserver.DialSecureWebSocket[%s, %s](ctx, address, cfg, secure)\n}\n\n", request, response)
	}
}

func hasV2UnaryRequest(doc *contractDocV2) bool {
	for _, m := range doc.Methods {
		if m.Kind == "unary" && len(m.Request) > 0 {
			return true
		}
	}
	return false
}

func hasV2IdempotentUnary(doc *contractDocV2) bool {
	for _, m := range doc.Methods {
		if m.Kind == "unary" && m.Idempotent {
			return true
		}
	}
	return false
}

func renderV2UnaryWrapper(b *bytes.Buffer, doc *contractDocV2) {
	b.WriteString("// wrappedServiceV2 applies contract.Call to V2 Unary implementations.\ntype wrappedServiceV2 struct { inner ServiceV2; timeout time.Duration }\n\n")
	b.WriteString("// WrapServiceV2 applies the standard contract boundary; timeout <= 0 uses the framework default.\nfunc WrapServiceV2(inner ServiceV2, timeout time.Duration) ServiceV2 {\n\tif timeout <= 0 { timeout = contract.DefaultTimeout }\n\treturn wrappedServiceV2{inner: inner, timeout: timeout}\n}\n\n")
	for _, m := range doc.Methods {
		if m.Kind != "unary" {
			continue
		}
		request := ""
		callArg := ""
		if len(m.Request) > 0 {
			request = ", req " + v2RequestType(m)
			callArg = ", req"
		}
		if len(m.Response) > 0 {
			resp := v2ResponseType(m)
			fmt.Fprintf(b, "func (w wrappedServiceV2) %s(ctx context.Context%s) (%s, error) {\n", m.Name, request, resp)
			fmt.Fprintf(b, "\treturn contract.Call(ctx, %q, %q, w.timeout, func(ctx context.Context) (%s, error) { return w.inner.%s(ctx%s) })\n}\n\n", doc.System, m.Name, resp, m.Name, callArg)
		} else {
			fmt.Fprintf(b, "func (w wrappedServiceV2) %s(ctx context.Context%s) error {\n", m.Name, request)
			fmt.Fprintf(b, "\t_, err := contract.Call(ctx, %q, %q, w.timeout, func(ctx context.Context) (struct{}, error) { return struct{}{}, w.inner.%s(ctx%s) })\n\treturn err\n}\n\n", doc.System, m.Name, m.Name, callArg)
		}
	}
}

func renderV2HTTPClient(b *bytes.Buffer, doc *contractDocV2) {
	b.WriteString("// ClientV2 is the generated remote binding for V2 Unary methods.\ntype ClientV2 struct { base string; hc *http.Client; secure bool }\n\n")
	b.WriteString("// NewClientV2 is the legacy/dev entry point; production callers should use NewSecureClientV2.\nfunc NewClientV2(base, caller string, hc *http.Client) *ClientV2 {\n\tif hc == nil { hc = &http.Client{} }\n\tinner := *hc\n\tinner.Transport = callctx.Transport{Base: hc.Transport, Caller: caller}\n\treturn &ClientV2{base: strings.TrimSuffix(base, \"/\"), hc: &inner}\n}\n\n")
	b.WriteString("// NewSecureClientV2 creates the authenticated HTTPS client for V2 Unary methods.\nfunc NewSecureClientV2(base string, opts contract.SecureClientOptions) (*ClientV2, error) {\n\thc, err := contract.NewSecureHTTPClient(base, opts)\n\tif err != nil { return nil, err }\n\treturn &ClientV2{base: strings.TrimSuffix(base, \"/\"), hc: hc, secure: true}, nil\n}\n\n")
	b.WriteString("var _ ServiceV2 = (*ClientV2)(nil)\n\n")
	for _, m := range doc.Methods {
		if m.Kind == "unary" {
			renderV2UnaryClientMethod(b, doc, m)
		}
	}
}

func renderV2UnaryClientMethod(b *bytes.Buffer, doc *contractDocV2, m methodDefV2) {
	request := ""
	if len(m.Request) > 0 {
		request = ", req " + v2RequestType(m)
	}
	resultType := v2ResponseType(m)
	if len(m.Response) == 0 {
		resultType = "struct{}"
	}
	call := fmt.Sprintf("contract.Call(ctx, %q, %q, 0, func(ctx context.Context) (%s, error) {\n", doc.System, m.Name, resultType)
	if len(m.Request) > 0 {
		fmt.Fprintf(b, "func (c *ClientV2) %s(ctx context.Context%s) %s {\n", m.Name, request, v2UnaryResult(m))
		fmt.Fprintf(b, "\tcall := func() (%s, error) {\n\t\treturn %s", resultType, call)
		fmt.Fprintf(b, "\t\tbody, err := json.Marshal(req)\n\t\tif err != nil { return %s{}, apperr.Internal(err) }\n\t\threq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+%q, bytes.NewReader(body))\n", resultType, m.Path)
	} else {
		fmt.Fprintf(b, "func (c *ClientV2) %s(ctx context.Context) %s {\n", m.Name, v2UnaryResult(m))
		fmt.Fprintf(b, "\tcall := func() (%s, error) {\n\t\treturn %s", resultType, call)
		fmt.Fprintf(b, "\t\threq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+%q, http.NoBody)\n", m.Path)
	}
	fmt.Fprintf(b, "\t\tif err != nil { return %s{}, apperr.Internal(err) }\n\t\threq.Header.Set(\"Content-Type\", \"application/json\")\n\t\treturn doV2[%s](c.hc, hreq, c.secure)\n\t})\n\t}\n", resultType, resultType)
	if m.Idempotent {
		if len(m.Response) == 0 {
			b.WriteString("\t_, err := retryUnavailableV2(ctx, call)\n\treturn err\n}\n\n")
		} else {
			b.WriteString("\treturn retryUnavailableV2(ctx, call)\n}\n\n")
		}
	} else if len(m.Response) == 0 {
		b.WriteString("\t_, err := call()\n\treturn err\n}\n\n")
	} else {
		b.WriteString("\treturn call()\n}\n\n")
	}
}

func v2UnaryResult(m methodDefV2) string {
	if len(m.Response) == 0 {
		return "error"
	}
	return "(" + v2ResponseType(m) + ", error)"
}

func renderV2HTTPDo(b *bytes.Buffer) {
	b.WriteString(`func doV2[T any](hc *http.Client, hreq *http.Request, secure bool) (T, error) {
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
	if err != nil { return zero, apperr.Unavailable(err) }
	if resp.StatusCode != http.StatusOK { return zero, apperr.FromProblem(resp.StatusCode, raw) }
	var value T
	if err := json.Unmarshal(raw, &value); err != nil { return zero, apperr.Internal(err) }
	return value, nil
}

`)
}

func renderV2Retry(b *bytes.Buffer) {
	b.WriteString(`func retryUnavailableV2[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	value, err := fn()
	for attempt := 1; attempt < 3 && err != nil && apperr.Is(err, apperr.CodeUnavailable); attempt++ {
		timer := time.NewTimer(100 * time.Millisecond * time.Duration(attempt))
		select {
		case <-ctx.Done(): timer.Stop(); return value, err
		case <-timer.C:
		}
		value, err = fn()
	}
	return value, err
}
`)
}

func renderV2LocalStreamingClient(b *bytes.Buffer, doc *contractDocV2) {
	b.WriteString("// StreamReaderV2 is a receive-only view returned by generated Server Stream local openers.\ntype StreamReaderV2[T any] interface { Recv(context.Context) (T, error); Close() error }\n\ntype streamReaderV2[T any] struct { inner contract.ClientStream[struct{}, T] }\nfunc (r streamReaderV2[T]) Recv(ctx context.Context) (T, error) { return r.inner.Recv(ctx) }\nfunc (r streamReaderV2[T]) Close() error { return r.inner.Close() }\n\ntype streamSenderAdapterV2[T any] struct { peer contract.Stream[T, struct{}] }\nfunc (s streamSenderAdapterV2[T]) Send(ctx context.Context, value T) error { return s.peer.Send(ctx, value) }\n\n")
	for _, m := range doc.Methods {
		switch m.Kind {
		case "server_stream":
			request := ""
			arg := ""
			if len(m.Request) > 0 {
				request = ", req " + v2RequestType(m)
				arg = ", req"
			}
			fmt.Fprintf(b, "// Open%sLocalV2 opens a typed local Server Stream; cursor remains application-owned.\nfunc Open%sLocalV2(ctx context.Context, cursor string, cfg contract.StreamConfig, svc StreamingServiceV2%s) (StreamReaderV2[%s], error) {\n", m.Name, m.Name, request, v2ResponseType(m))
			fmt.Fprintf(b, "\tstream, err := contract.OpenLocal[struct{}, %s](ctx, %q, %q, cfg, func(ctx context.Context, peer contract.Stream[%s, struct{}]) error { return svc.%s(ctx, cursor%s, streamSenderAdapterV2[%s]{peer: peer}) })\n", v2ResponseType(m), doc.System, m.Name, v2ResponseType(m), m.Name, arg, v2ResponseType(m))
			fmt.Fprintf(b, "\tif err != nil { return nil, err }\n\treturn streamReaderV2[%s]{inner: stream}, nil\n}\n\n", v2ResponseType(m))
		case "bidi_stream":
			fmt.Fprintf(b, "// Open%sLocalV2 opens a typed local Bidi Stream.\nfunc Open%sLocalV2(ctx context.Context, cfg contract.StreamConfig, svc StreamingServiceV2) (contract.ClientStream[%s, %s], error) {\n", m.Name, m.Name, v2RequestType(m), v2ResponseType(m))
			fmt.Fprintf(b, "\treturn contract.OpenLocal[%s, %s](ctx, %q, %q, cfg, func(ctx context.Context, peer contract.Stream[%s, %s]) error { return svc.%s(ctx, peer) })\n}\n\n", v2RequestType(m), v2ResponseType(m), doc.System, m.Name, v2ResponseType(m), v2RequestType(m), m.Name)
		}
	}
}

package gen

import (
	"bytes"
	"fmt"
	"strings"
)

func renderServerV2(doc *contractDocV2) []byte {
	var b bytes.Buffer
	b.WriteString(header)
	fmt.Fprintf(&b, "package %s\n\n", doc.Package)
	if !hasV2Unary(doc) && !hasV2ServerStream(doc) && !hasV2BidiStream(doc) {
		return b.Bytes()
	}
	b.WriteString("import (\n")
	if hasV2ServerStream(doc) || hasV2BidiStream(doc) {
		b.WriteString("\t\"context\"\n\t\"net/http\"\n")
	}
	if hasV2Unary(doc) {
		b.WriteString("\t\"encoding/json\"\n")
		if !hasV2ServerStream(doc) && !hasV2BidiStream(doc) {
			b.WriteString("\t\"net/http\"\n")
		}
		if v2UnaryUsesRefs(doc) {
			b.WriteString("\t\"bytes\"\n\t\"io\"\n\t\"strings\"\n\t\"unicode\"\n")
		}
	}
	if hasV2Streaming(doc) {
		b.WriteString("\n\t\"github.com/forgeplex/appkit/contract\"\n")
	}
	if hasV2ServerStream(doc) || hasV2BidiStream(doc) {
		b.WriteString("\t\"github.com/forgeplex/appkit/httpserver\"\n")
	}
	if hasV2BidiStream(doc) {
		b.WriteString("\t\"github.com/forgeplex/appkit\"\n")
	}
	if hasV2Unary(doc) {
		if !hasV2BidiStream(doc) {
			b.WriteString("\t\"github.com/forgeplex/appkit/apperr\"\n")
		}
		b.WriteString("\t\"github.com/forgeplex/appkit/callctx\"\n")
	}
	if hasV2BidiStream(doc) {
		b.WriteString("\t\"github.com/forgeplex/appkit/apperr\"\n")
	}
	b.WriteString(")\n\n")

	if hasV2Unary(doc) {
		renderV2HTTPHandler(&b, doc)
	}
	if hasV2ServerStream(doc) {
		renderV2SSEHandlers(&b, doc)
	}
	if hasV2BidiStream(doc) {
		renderV2WebSocketHandlers(&b, doc)
	}
	return b.Bytes()
}

func renderV2WebSocketHandlers(b *bytes.Buffer, doc *contractDocV2) {
	for _, m := range doc.Methods {
		if m.Kind != "bidi_stream" {
			continue
		}
		request, response := v2RequestType(m), v2ResponseType(m)
		fmt.Fprintf(b, "// New%sWebSocketHandlerV2 builds the authenticated WebSocket adapter for %s. Mount it through a classified Registry route.\nfunc New%sWebSocketHandlerV2(cfg httpserver.WebSocketConfig, svc StreamingServiceV2) (http.Handler, error) {\n", m.Name, m.Path, m.Name)
		fmt.Fprintf(b, "\tcfg.System, cfg.Method = %q, %q\n", doc.System, m.Name)
		b.WriteString("\tidentity := cfg.IdentityResolver\n\tif identity == nil {\n\t\tidentity = func(ctx context.Context) (httpserver.WebSocketIdentity, error) {\n")
		b.WriteString("\t\t\tif actor, ok := appkit.ActorFrom(ctx); ok && actor.UserID != \"\" { expiresAt, _ := appkit.IdentityExpiryFrom(ctx); return httpserver.WebSocketIdentity{Subject: actor.UserID, ExpiresAt: expiresAt}, nil }\n")
		b.WriteString("\t\t\tif principal, ok := appkit.ServicePrincipalFrom(ctx); ok && principal.Subject != \"\" { return httpserver.WebSocketIdentity{Subject: principal.Subject, ExpiresAt: principal.ExpiresAt}, nil }\n")
		b.WriteString("\t\t\treturn httpserver.WebSocketIdentity{}, apperr.Unauthenticated(\"authentication required\")\n\t\t}\n\t}\n")
		fmt.Fprintf(b, "\treturn httpserver.NewWebSocketHandler[%s, %s](cfg, identity, func(ctx context.Context, peer contract.Stream[%s, %s]) error { return svc.%s(ctx, peer) })\n}\n\n", request, response, response, request, m.Name)
	}
}

func v2UnaryUsesRefs(doc *contractDocV2) bool {
	legacy := doc.legacyDoc()
	for _, m := range legacy.Methods {
		if m.Idempotent || len(m.Request) > 0 || len(m.Response) > 0 {
			if legacy.fieldsUseRefs(m.Request) {
				return true
			}
		}
	}
	return false
}

func renderV2HTTPHandler(b *bytes.Buffer, doc *contractDocV2) {
	b.WriteString("// NewHTTPHandlerV2 exposes V2 Unary methods. Mount its routes through the Registry's classified security APIs in strict modes.\nfunc NewHTTPHandlerV2(svc ServiceV2) http.Handler {\n\tmux := http.NewServeMux()\n")
	for _, m := range doc.Methods {
		if m.Kind != "unary" {
			continue
		}
		fmt.Fprintf(b, "\tmux.HandleFunc(%q, func(w http.ResponseWriter, r *http.Request) {\n", "POST "+m.Path)
		if len(m.Request) > 0 {
			fmt.Fprintf(b, "\t\tvar req %s\n", v2RequestType(m))
			if v2MethodUsesRefs(doc, m.Request) {
				b.WriteString("\t\tif err := decodeRefsRequestV2(r.Body, &req); err != nil { apperr.WriteProblem(w, apperr.InvalidArgument(\"请求体不是合法 JSON\")); return }\n")
			} else {
				b.WriteString("\t\tif err := json.NewDecoder(r.Body).Decode(&req); err != nil { apperr.WriteProblem(w, apperr.InvalidArgument(\"请求体不是合法 JSON\")); return }\n")
			}
		}
		call := "svc." + m.Name + "(r.Context()"
		if len(m.Request) > 0 {
			call += ", req"
		}
		call += ")"
		if len(m.Response) > 0 {
			fmt.Fprintf(b, "\t\treply, err := %s\n\t\tif err != nil { apperr.WriteProblem(w, err); return }\n\t\twriteJSONV2(w, reply)\n", call)
		} else {
			fmt.Fprintf(b, "\t\tif err := %s; err != nil { apperr.WriteProblem(w, err); return }\n\t\twriteJSONV2(w, struct{}{})\n", call)
		}
		b.WriteString("\t})\n")
	}
	b.WriteString("\treturn serveV2(mux)\n}\n\n")
	b.WriteString("func serveV2(mux *http.ServeMux) http.Handler {\n\treturn http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {\n\t\tmux.ServeHTTP(w, r.WithContext(callctx.Merge(r.Context(), callctx.Extract(r.Header.Get))))\n\t})\n}\n\n")
	b.WriteString("func writeJSONV2(w http.ResponseWriter, value any) { w.Header().Set(\"Content-Type\", \"application/json\"); _ = json.NewEncoder(w).Encode(value) }\n\n")
	if v2UnaryUsesRefs(doc) {
		var helper bytes.Buffer
		renderRefsRequestDecoder(&helper)
		replacer := strings.NewReplacer(
			"decodeRefsRequest", "decodeRefsRequestV2",
			"uniqueJSONValue", "uniqueJSONValueV2",
			"foldJSONKey", "foldJSONKeyV2",
			"maxRefsRequestBytes", "maxRefsRequestBytesV2",
			"maxRefsRequestDepth", "maxRefsRequestDepthV2",
		)
		b.WriteString(replacer.Replace(helper.String()))
	}
}

func v2MethodUsesRefs(doc *contractDocV2, fields []fieldDef) bool {
	named := doc.namedTypes()
	for _, field := range fields {
		base := scalarType(field.Type)
		if base == "refs" {
			return true
		}
		if named[base] {
			for _, typ := range doc.Types {
				if typ.Name == base {
					for _, nested := range typ.Fields {
						if scalarType(nested.Type) == "refs" {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

func renderV2SSEHandlers(b *bytes.Buffer, doc *contractDocV2) {
	b.WriteString("type sseSenderAdapterV2[T, R any] struct { peer contract.Stream[httpserver.SSEEvent[T], R]; cursor func(T) string }\n")
	b.WriteString("func (s sseSenderAdapterV2[T, R]) Send(ctx context.Context, value T) error { id := \"\"; if s.cursor != nil { id = s.cursor(value) }; return s.peer.Send(ctx, httpserver.SSEEvent[T]{ID: id, Data: value}) }\n\n")
	for _, m := range doc.Methods {
		if m.Kind != "server_stream" {
			continue
		}
		reqType := v2RequestType(m)
		respType := v2ResponseType(m)
		fmt.Fprintf(b, "// New%sSSEHandlerV2 builds the POST + SSE adapter for %s. Mount the returned handler through a classified Registry route.\nfunc New%sSSEHandlerV2(cfg httpserver.SSEConfig, svc StreamingServiceV2) (http.Handler, error) {\n", m.Name, m.Path, m.Name)
		fmt.Fprintf(b, "\tcfg.System, cfg.Method = %q, %q\n", doc.System, m.Name)
		fmt.Fprintf(b, "\treturn httpserver.NewSSEHandler[%s, %s](cfg, func(ctx context.Context, cursor string, peer contract.Stream[httpserver.SSEEvent[%s], %s]) error {\n", reqType, respType, respType, reqType)
		if len(m.Request) > 0 {
			b.WriteString("\t\treq, err := peer.Recv(ctx)\n\t\tif err != nil { return err }\n")
		} else {
			b.WriteString("\t\t_, err := peer.Recv(ctx)\n\t\tif err != nil { return err }\n")
		}
		cursorFn := "nil"
		if m.CursorField != "" {
			fmt.Fprintf(b, "\t\tcursorFn := func(event %s) string { return event.%s }\n", respType, camel(m.CursorField))
			cursorFn = "cursorFn"
		}
		callArgs := "ctx, cursor"
		if len(m.Request) > 0 {
			callArgs += ", req"
		}
		fmt.Fprintf(b, "\t\treturn svc.%s(%s, sseSenderAdapterV2[%s, %s]{peer: peer, cursor: %s})\n\t})\n}\n\n", m.Name, callArgs, respType, reqType, cursorFn)
	}
}

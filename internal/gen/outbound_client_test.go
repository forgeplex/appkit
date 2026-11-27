package gen

import (
	"strings"
	"testing"
)

func TestGeneratedUnaryClientsUseSharedOutboundInstrumentation(t *testing.T) {
	for _, tc := range []struct{ input, output, legacyFunc string }{
		{"testdata/contract.yaml", "client.gen.go", "func NewClient("},
		{"testdata/contract_v2.yaml", "client_v2.gen.go", "func NewClientV2("},
	} {
		t.Run(tc.output, func(t *testing.T) {
			files, err := RenderContractSource(tc.input, mustRead(t, tc.input))
			if err != nil {
				t.Fatal(err)
			}
			client := string(files[tc.output])
			if !strings.Contains(client, "contract.NewSecureHTTPClient(base, opts)") {
				t.Fatal("generated unary client must use contract.NewSecureHTTPClient")
			}
			if strings.Contains(client, tc.legacyFunc) {
				t.Fatalf("legacy unauthenticated %s must no longer be generated", tc.legacyFunc)
			}
		})
	}
}

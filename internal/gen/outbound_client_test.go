package gen

import (
	"strings"
	"testing"
)

func TestGeneratedUnaryClientsUseSharedOutboundInstrumentation(t *testing.T) {
	for _, tc := range []struct{ input, output string }{
		{"testdata/contract.yaml", "client.gen.go"},
		{"testdata/contract_v2.yaml", "client_v2.gen.go"},
	} {
		t.Run(tc.output, func(t *testing.T) {
			files, err := RenderContractSource(tc.input, mustRead(t, tc.input))
			if err != nil {
				t.Fatal(err)
			}
			client := string(files[tc.output])
			if !strings.Contains(client, "callctx.Transport{Base: outbound.InstrumentTransport(hc.Transport), Caller: caller}") {
				t.Fatal("legacy unary client must preserve callctx over shared outbound instrumentation")
			}
		})
	}
}

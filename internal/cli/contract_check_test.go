package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContractCheckJSON(t *testing.T) {
	base, err := os.ReadFile("../gen/testdata/contract.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	old := filepath.Join(dir, "old.yaml")
	candidate := filepath.Join(dir, "new.yaml")
	if err := os.WriteFile(old, base, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, breaking := range []bool{false, true} {
		data := base
		if breaking {
			data = []byte(strings.Replace(string(base), "path: /greet", "path: /other", 1))
		}
		if err := os.WriteFile(candidate, data, 0o600); err != nil {
			t.Fatal(err)
		}
		var out, diagnostics bytes.Buffer
		err := contractCheck([]string{"-base", old, "-candidate", candidate}, &out, &diagnostics)
		var result agentResult
		if jsonErr := json.Unmarshal(out.Bytes(), &result); jsonErr != nil {
			t.Fatal(jsonErr)
		}
		if result.OK == breaking || result.Command != "contract-check" || diagnostics.Len() != 0 {
			t.Fatalf("wrong result: %s %v", out.String(), err)
		}
		if breaking {
			var exit *agentExit
			if !errors.As(err, &exit) || exit.code != 3 || result.Error == nil || result.Error.Code != "contract_incompatible" || !strings.Contains(out.String(), `"issues"`) {
				t.Fatalf("wrong failure: %s %v", out.String(), err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
}

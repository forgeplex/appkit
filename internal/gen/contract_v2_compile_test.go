package gen

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Isolated shapes catch unused or missing imports hidden by the mixed fixture.
func TestContractV2GeneratedTransportShapesCompile(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mod := strings.Replace(string(mustRead(t, filepath.Join(root, "go.mod"))), "module github.com/forgeplex/appkit", "module example.com/transportfixture", 1)
	mod += fmt.Sprintf("\nrequire github.com/forgeplex/appkit v0.0.0\nreplace github.com/forgeplex/appkit => %q\n", root)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), mustRead(t, filepath.Join(root, "go.sum")), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, kind string
		request    bool
	}{
		{"unary", "unary", true},
		{"unary_no_request", "unary", false},
		{"sse", "server_stream", true},
		{"sse_no_request", "server_stream", false},
		{"bidi", "bidi_stream", true},
	} {
		request := ""
		if tc.request {
			request = "    request: [{name: text, type: string}]\n"
		}
		source := fmt.Sprintf("version: 2\npackage: fixture\nsystem: fixture\nmethods:\n  - name: M\n    path: /m\n    doc: transport shape\n    kind: %s\n%s    response: [{name: text, type: string}]\n", tc.kind, request)
		files, err := RenderContractSource(tc.name+".yaml", []byte(source))
		if err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dir, tc.name)
		if err := os.Mkdir(out, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(out, name), content, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOENV=off", "GOWORK=off", "GOFLAGS=-mod=readonly", "GOPROXY=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated transport shapes failed to compile: %v\n%s", err, out)
	}
}

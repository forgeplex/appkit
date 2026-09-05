package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIExplicitWorkflowRefIsOffline(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	root := agentDomain(t)
	for _, args := range [][]string{
		{"sync", "-dir", root, "-workflow-ref", agentWorkflowRef},
		{"new", "domain", "sample", "-dir", root, "-target", "generated", "-workflow-ref", agentWorkflowRef},
	} {
		var first, second, diagnostics bytes.Buffer
		if err := runAgent("plan", args, &first, &diagnostics); err != nil {
			t.Fatalf("offline plan %v: %v %s", args, err, first.String())
		}
		if err := runAgent("plan", args, &second, &diagnostics); err != nil || !bytes.Equal(first.Bytes(), second.Bytes()) {
			t.Fatalf("pinned plan not deterministic: %v", err)
		}
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 {
		t.Fatalf("plan wrote target: %v %v", entries, err)
	}
	if err := runSync([]string{"-dir", root, "-workflow-ref", agentWorkflowRef}); err != nil {
		t.Fatalf("direct sync explicit ref: %v", err)
	}
	if err := runSync([]string{"-dir", root, "-workflow-ref", agentWorkflowRef, "-check"}); err != nil {
		t.Fatalf("direct sync check explicit ref: %v", err)
	}
	generated := filepath.Join(t.TempDir(), "sample")
	if err := runNew([]string{"domain", "sample", "-dir", generated, "-workflow-ref", agentWorkflowRef}); err != nil {
		t.Fatalf("direct new explicit ref: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(generated, ".github/workflows/ci.yml"))
	if err != nil || !strings.Contains(string(content), "@"+agentWorkflowRef) {
		t.Fatalf("direct new did not pin workflow: %s %v", content, err)
	}
}

func TestCLIWorkflowRefValidationAndCancellation(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	root := agentDomain(t)
	for _, operation := range [][]string{
		{"sync", "-dir", root},
		{"new", "domain", "sample", "-dir", root, "-target", "generated"},
	} {
		var out, diagnostics bytes.Buffer
		err := runAgent("plan", append(operation, "-workflow-ref", "main"), &out, &diagnostics)
		var exit *agentExit
		if !errors.As(err, &exit) || exit.code != 2 || !strings.Contains(out.String(), "invalid_arguments") {
			t.Fatalf("invalid workflow ref result: %v %s", err, out.String())
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolveCLIWorkflowRef(ctx, "v1.2.3", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("CLI source resolution cancellation: %v", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 {
		t.Fatalf("invalid plan wrote target: %v %v", entries, err)
	}
}

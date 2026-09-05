package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forgeplex/appkit/internal/gen"
)

func TestGenContractCheck(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join("..", "gen", "testdata", "contract.yaml")
	args := []string{"contract", "-in", in, "-dir", dir}
	if err := runGen(args); err != nil {
		t.Fatal(err)
	}
	checkArgs := append(append([]string(nil), args...), "-check")
	if err := runGen(checkArgs); err != nil {
		t.Fatalf("unchanged check: %v", err)
	}
	path := filepath.Join(dir, "service.gen.go")
	if err := os.WriteFile(path, []byte("manually changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runGen(checkArgs)
	if !errors.Is(err, gen.ErrContractDrift) || !strings.Contains(err.Error(), "stale service.gen.go") {
		t.Fatalf("drift check: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "manually changed\n" {
		t.Fatalf("check changed output: %q, %v", got, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	err = runGen(checkArgs)
	if !errors.Is(err, gen.ErrContractDrift) || !strings.Contains(err.Error(), "missing service.gen.go") {
		t.Fatalf("missing check: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check recreated missing file: %v", err)
	}
}

func TestGenContractCheckArguments(t *testing.T) {
	for _, args := range [][]string{
		{"contract", "-check"},
		{"contract", "-check", "-in", "contract.yaml"},
		{"contract", "-check", "-dir", "contract"},
		{"contract", "-check", "-in", "contract.yaml", "-dir", "contract", "unexpected"},
	} {
		if err := runGen(args); err == nil {
			t.Errorf("arguments %q should fail", args)
		}
	}
}

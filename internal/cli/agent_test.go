package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forgeplex/appkit/internal/workspace"
)

func agentDomain(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".appkit.yml"), []byte("version: 1\ndomain: sample\nmodule: example.com/sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestAgentCLIPlanApplyReplay(t *testing.T) {
	root := agentDomain(t)
	var out, diagnostics bytes.Buffer
	if err := runAgent("plan", []string{"sync", "-dir", root}, &out, &diagnostics); err != nil {
		t.Fatalf("plan: %v %s", err, out.String())
	}
	plan, err := workspace.ParsePlan(out.Bytes())
	if err != nil || diagnostics.Len() != 0 {
		t.Fatalf("not clean canonical JSON: %v %s", err, diagnostics.String())
	}
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(planPath, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"committed", "replayed"} {
		out.Reset()
		if err := runAgent("apply", []string{"-dir", root, "-plan", planPath, "-digest", plan.Digest()}, &out, &diagnostics); err != nil {
			t.Fatalf("apply: %v %s", err, out.String())
		}
		var result struct {
			APIVersion string `json:"apiVersion"`
			OK         bool   `json:"ok"`
			Data       struct {
				Disposition string `json:"disposition"`
			} `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.OK || result.Data.Disposition != want || result.APIVersion != agentResultVersion {
			t.Fatalf("unexpected result: %s %v", out.String(), err)
		}
	}
}

func TestAgentJSONFailures(t *testing.T) {
	for _, tc := range []struct {
		command string
		args    []string
		code    string
		exit    int
	}{
		{"plan", nil, "invalid_arguments", 2},
		{"plan", []string{"sync", "-bogus"}, "invalid_arguments", 2},
		{"plan", []string{"contract"}, "invalid_arguments", 2},
		{"plan", []string{"sync", "extra"}, "invalid_arguments", 2},
		{"apply", nil, "invalid_arguments", 2},
		{"apply", []string{"-timeout", "0"}, "invalid_arguments", 2},
		{"plan", []string{"sync", "-dir", t.TempDir()}, "operation_failed", 1},
	} {
		t.Run(strings.Join(append([]string{tc.command}, tc.args...), " "), func(t *testing.T) {
			var out, diagnostics bytes.Buffer
			err := runAgent(tc.command, tc.args, &out, &diagnostics)
			var exit *agentExit
			var result agentResult
			if !errors.As(err, &exit) || exit.code != tc.exit {
				t.Fatalf("exit: %v", err)
			}
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.OK || result.Error == nil || result.Error.Code != tc.code {
				t.Fatalf("error JSON: %s %v", out.String(), err)
			}
		})
	}
}

func TestAgentHelpIsNotAnErrorOrPlan(t *testing.T) {
	for _, tc := range []struct {
		command string
		args    []string
	}{
		{"plan", []string{"-h"}},
		{"plan", []string{"sync", "-h"}},
		{"plan", []string{"contract", "--help"}},
		{"apply", []string{"-h"}},
	} {
		var out, diagnostics bytes.Buffer
		if err := runAgent(tc.command, tc.args, &out, &diagnostics); err != nil || out.Len() != 0 || diagnostics.Len() == 0 {
			t.Fatalf("help: %v stdout=%s stderr=%s", err, out.String(), diagnostics.String())
		}
	}
}

func TestAgentConflictAndTampering(t *testing.T) {
	for _, tc := range []struct {
		mode, code string
		exit       int
	}{
		{"source", "workspace_conflict", 3},
		{"digest", "invalid_plan", 2},
		{"corrupt", "invalid_plan", 2},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			root := agentDomain(t)
			var out, diagnostics bytes.Buffer
			if err := runAgent("plan", []string{"sync", "-dir", root}, &out, &diagnostics); err != nil {
				t.Fatal(err)
			}
			encoded := bytes.Clone(out.Bytes())
			args := []string{"-dir", root}
			switch tc.mode {
			case "source":
				if err := os.WriteFile(filepath.Join(root, ".appkit.yml"), []byte("edited"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "digest":
				args = append(args, "-digest", "sha256:wrong")
			case "corrupt":
				encoded = append(encoded, []byte("{}")...)
			}
			planPath := filepath.Join(t.TempDir(), "plan.json")
			if err := os.WriteFile(planPath, encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			err := runAgent("apply", append(args, "-plan", planPath), &out, &diagnostics)
			var exit *agentExit
			var result agentResult
			if !errors.As(err, &exit) || exit.code != tc.exit {
				t.Fatalf("exit: %v %s", err, out.String())
			}
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Error == nil || result.Error.Code != tc.code {
				t.Fatalf("result: %s %v", out.String(), err)
			}
			if _, err := os.Stat(filepath.Join(root, ".golangci.yml")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("wrote despite rejection: %v", err)
			}
		})
	}
}

func TestAgentFailurePreservesKnownCommitOutcome(t *testing.T) {
	for _, disposition := range []workspace.ApplyDisposition{workspace.ApplyCommitted, workspace.ApplyReplayed, ""} {
		var out, diagnostics bytes.Buffer
		err := &agentApplyError{
			result: workspace.ApplyResult{PlanDigest: "sha256:reviewed-plan", Disposition: disposition},
			err:    errors.New("cleanup or recovery error"),
		}
		if reportAgentError("apply", err, &out, &diagnostics) == nil {
			t.Fatal("failure reported as success")
		}
		var result struct {
			OK    bool           `json:"ok"`
			Data  agentApplyData `json:"data"`
			Error *agentFailure  `json:"error"`
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.OK || result.Error == nil || result.Data.PlanDigest != "sha256:reviewed-plan" || result.Data.Disposition != disposition {
			t.Fatalf("lost apply outcome: %s %v", out.String(), err)
		}
	}
}

func TestPlanFileRejectsSymlinkAndDirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "regular")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{link, dir} {
		if _, err := readPlanFile(name); !errors.Is(err, workspace.ErrInvalidPlanDocument) {
			t.Errorf("nonregular plan accepted %q: %v", name, err)
		}
	}
}

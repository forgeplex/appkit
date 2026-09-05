package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPlanDocumentCanonicalRoundTripAndCrossProcessApply(t *testing.T) {
	root := t.TempDir()
	writeMode(t, filepath.Join(root, "update.txt"), []byte("before"), 0o600)
	writeMode(t, filepath.Join(root, "delete.txt"), []byte("delete"), 0o644)
	plan, err := BuildPlan(root, []Change{
		{Path: "assert.txt", Operation: OperationAssert},
		{Path: "empty.txt", Operation: OperationCreate, Content: []byte{}, Mode: 0o644},
		{Path: "update.txt", Operation: OperationUpdate, Content: []byte("after"), Mode: 0o640},
		{Path: "delete.txt", Operation: OperationDelete},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := MarshalPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParsePlan(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Digest() != plan.Digest() || parsed.SnapshotDigest() != plan.SnapshotDigest() ||
		parsed.FinalDigest() != plan.FinalDigest() || !slices.Equal(parsed.Changes(), plan.Changes()) {
		t.Fatalf("round trip differs: %#v != %#v", parsed.Changes(), plan.Changes())
	}
	encoded[0] ^= 0xff
	second, err := MarshalPlan(parsed)
	if err != nil || second[0] != '{' {
		t.Fatalf("plan retained input bytes: %v", err)
	}
	result, err := Apply(context.Background(), root, parsed)
	if err != nil || result.Disposition != ApplyCommitted {
		t.Fatalf("Apply = %#v, %v", result, err)
	}
	if content, _ := os.ReadFile(filepath.Join(root, "update.txt")); string(content) != "after" {
		t.Fatalf("updated content = %q", content)
	}
	if content, _ := os.ReadFile(filepath.Join(root, "empty.txt")); len(content) != 0 {
		t.Fatalf("empty content = %q", content)
	}
	if _, err := os.Stat(filepath.Join(root, "delete.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted file stat = %v", err)
	}
}

func TestPlanDocumentRejectsNonCanonicalTamperedAndUnknownInput(t *testing.T) {
	root := t.TempDir()
	plan, err := BuildPlan(root, []Change{{Path: "file.txt", Operation: OperationCreate, Content: []byte("x"), Mode: 0o644}})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := MarshalPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		data []byte
		want error
	}{
		{"leading whitespace", append([]byte(" "), canonical...), ErrNonCanonicalPlan},
		{"trailing value", append(slices.Clone(canonical), canonical...), ErrInvalidPlanDocument},
		{"unknown field", bytes.Replace(canonical, []byte(`"kind":`), []byte(`"unknown":true,"kind":`), 1), ErrInvalidPlanDocument},
		{"wrong version", bytes.Replace(canonical, []byte(PlanAPIVersion), []byte("appkit.dev/workspace-plan/v2"), 1), ErrInvalidPlanDocument},
		{"payload tamper", bytes.Replace(canonical, []byte(`"contentBase64":"eA=="`), []byte(`"contentBase64":"eQ=="`), 1), ErrInvalidPlanDocument},
		{"digest tamper", bytes.Replace(canonical, []byte(plan.Digest()), []byte("sha256:"+string(bytes.Repeat([]byte{'0'}, 64))), 1), ErrInvalidPlanDocument},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParsePlan(test.data); !errors.Is(err, test.want) {
				t.Fatalf("ParsePlan error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestPlanDocumentPreconditionModesRoundTripOrFailClosed(t *testing.T) {
	tests := []struct {
		name string
		mode fs.FileMode
		want bool
	}{
		{name: "normal", mode: 0o644, want: true},
		{name: "private", mode: 0o600, want: true},
		{name: "setuid", mode: fs.ModeSetuid | 0o644},
		{name: "setgid", mode: fs.ModeSetgid | 0o644},
		{name: "sticky", mode: fs.ModeSticky | 0o644},
		{name: "group-readable", mode: 0o044},
		{name: "no-permissions", mode: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeMode(t, filepath.Join(root, "input.yaml"), []byte("input"), 0o644)
			plan, err := BuildPlan(root, []Change{{Path: "input.yaml", Operation: OperationAssert}})
			if err != nil {
				t.Fatal(err)
			}
			// Construct valid captured states without relying on host support
			// for special chmod bits or elevated reads of mode-0000 files.
			plan.before.files[0].Mode = test.mode
			plan.before.digest = digestFiles(plan.before.files)
			plan.finalFiles[0].Mode = test.mode
			plan.finalDigest = digestFiles(plan.finalFiles)
			plan.digest = digestPlan(plan)
			if err := plan.Validate(); err != nil {
				t.Fatalf("invalid test plan: %v", err)
			}
			encoded, err := MarshalPlan(plan)
			if !test.want {
				if !errors.Is(err, ErrInvalidPlanDocument) || encoded != nil {
					t.Fatalf("MarshalPlan unsupported mode = %q, %v", encoded, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := ParsePlan(encoded)
			if err != nil || parsed.Digest() != plan.Digest() || !slices.Equal(parsed.Preconditions(), plan.Preconditions()) {
				t.Fatalf("round-trip mode %v = %#v, %v", test.mode, parsed.Preconditions(), err)
			}
		})
	}
}

func TestPlanLimitsFailBeforeWorkspaceCapture(t *testing.T) {
	changes := make([]Change, MaxPlanChanges+1)
	for index := range changes {
		changes[index] = Change{Path: "file", Operation: OperationCreate, Mode: 0o644}
	}
	if _, err := BuildPlan(filepath.Join(t.TempDir(), "missing"), changes); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("change count error = %v", err)
	}
	tooLarge := make([]byte, MaxPlanContentBytes+1)
	if _, err := BuildPlan(t.TempDir(), []Change{{Path: "file", Operation: OperationCreate, Content: tooLarge, Mode: 0o644}}); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("payload limit error = %v", err)
	}
	longPaths := make([]Change, MaxPlanChanges)
	for index := range longPaths {
		longPaths[index] = Change{
			Path:      fmt.Sprintf("%s-%04d", strings.Repeat("a", 256), index),
			Operation: OperationCreate, Mode: 0o644,
		}
	}
	if _, err := BuildPlan(filepath.Join(t.TempDir(), "missing"), longPaths); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("aggregate path limit error = %v", err)
	}
}

func TestPlanDocumentRejectsConflictingTargetsWithValidDigests(t *testing.T) {
	plan, err := BuildPlan(t.TempDir(), []Change{
		{Path: "parent", Operation: OperationCreate, Content: []byte("parent"), Mode: 0o644},
		{Path: "sibling", Operation: OperationCreate, Content: []byte("child"), Mode: 0o644},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Recompute every public digest to prove the parser enforces path-set
	// consistency, not only integrity of the serialized payload.
	plan.before.files[1].Path = "parent/child"
	plan.before.digest = digestFiles(plan.before.files)
	plan.changes[1].public.Path = "parent/child"
	plan.finalFiles[1].Path = "parent/child"
	plan.finalDigest = digestFiles(plan.finalFiles)
	plan.digest = digestPlan(plan)
	if err := plan.Validate(); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("Validate conflicting target set = %v", err)
	}
	document := planDocument{
		APIVersion: PlanAPIVersion, Kind: PlanKind, PlanDigest: plan.digest,
		SnapshotDigest: plan.before.digest, FinalDigest: plan.finalDigest,
		Changes: []planChangeDocument{
			{Path: "parent", Operation: OperationCreate, Before: planFileDocument{},
				ContentDigest: plan.changes[0].public.ContentDigest, ContentBase64: "cGFyZW50", Mode: "0644"},
			{Path: "parent/child", Operation: OperationCreate, Before: planFileDocument{},
				ContentDigest: plan.changes[1].public.ContentDigest, ContentBase64: "Y2hpbGQ=", Mode: "0644"},
		},
	}
	encoded, err := encodePlanDocument(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePlan(encoded); !errors.Is(err, ErrInvalidPlanDocument) {
		t.Fatalf("ParsePlan conflicting target set = %v", err)
	}
}

func TestMaximumWorkspacePayloadFitsCanonicalDocumentBudget(t *testing.T) {
	plan, err := BuildPlan(t.TempDir(), []Change{{
		Path: "maximum.bin", Operation: OperationCreate,
		Content: bytes.Repeat([]byte{'x'}, MaxPlanContentBytes), Mode: 0o600,
	}})
	if err != nil {
		t.Fatal(err)
	}
	document, err := MarshalPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(document) >= MaxPlanDocumentBytes {
		t.Fatalf("maximum payload document = %d bytes, budget %d", len(document), MaxPlanDocumentBytes)
	}
	parsed, err := ParsePlan(document)
	if err != nil || parsed.Digest() != plan.Digest() {
		t.Fatalf("ParsePlan(maximum payload) = (%s, %v)", parsed.Digest(), err)
	}
}

func FuzzParsePlan(f *testing.F) {
	root := f.TempDir()
	plan, err := BuildPlan(root, []Change{{Path: "file.txt", Operation: OperationCreate, Content: []byte("x"), Mode: 0o644}})
	if err != nil {
		f.Fatal(err)
	}
	canonical, err := MarshalPlan(plan)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(canonical)
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, input []byte) {
		plan, err := ParsePlan(input)
		if err != nil {
			return
		}
		encoded, err := MarshalPlan(plan)
		if err != nil || !bytes.Equal(input, encoded) {
			t.Fatalf("accepted non-canonical plan: %v", err)
		}
	})
}

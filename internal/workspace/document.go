package workspace

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
)

const (
	PlanAPIVersion        = "appkit.dev/workspace-plan/v1alpha1"
	GuardedPlanAPIVersion = "appkit.dev/workspace-plan/v1alpha2"
	PlanKind              = "WorkspacePlan"
	MaxPlanDocumentBytes  = 16 << 20
)

var (
	ErrInvalidPlanDocument = errors.New("workspace: invalid plan document")
	ErrNonCanonicalPlan    = errors.New("workspace: non-canonical plan document")
)

type planDocument struct {
	APIVersion      string               `json:"apiVersion"`
	Kind            string               `json:"kind"`
	PlanDigest      string               `json:"planDigest"`
	SnapshotDigest  string               `json:"snapshotDigest"`
	FinalDigest     string               `json:"finalDigest"`
	Changes         []planChangeDocument `json:"changes"`
	DirectoryGuards []DirectoryGuard     `json:"directoryGuards,omitempty"`
}

type planChangeDocument struct {
	Path          string           `json:"path"`
	Operation     Operation        `json:"operation"`
	Before        planFileDocument `json:"before"`
	ContentDigest string           `json:"contentDigest,omitempty"`
	ContentBase64 string           `json:"contentBase64,omitempty"`
	Mode          string           `json:"mode,omitempty"`
}

type planFileDocument struct {
	Exists bool   `json:"exists"`
	Digest string `json:"digest,omitempty"`
	Mode   string `json:"mode,omitempty"`
}

// MarshalPlan returns the unique bounded JSON representation of a Plan. Write
// payloads are included so another process can apply the exact reviewed plan;
// the digest still binds both the before state and desired final state.
// The current wire format requires every existing precondition file to have
// owner-read permission and no special mode bits. Unsupported modes fail closed
// rather than producing a document that cannot represent the state losslessly.
func MarshalPlan(plan Plan) ([]byte, error) {
	if err := plan.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidPlanDocument, err)
	}
	for _, before := range plan.before.files {
		if before.Exists && !validTargetMode(before.Mode) {
			return nil, fmt.Errorf("%w: precondition %q has an unsupported mode %v", ErrInvalidPlanDocument, before.Path, before.Mode)
		}
	}
	document := planDocument{
		APIVersion: PlanAPIVersion, Kind: PlanKind,
		PlanDigest: plan.digest, SnapshotDigest: plan.before.digest, FinalDigest: plan.finalDigest,
		Changes: make([]planChangeDocument, len(plan.changes)),
	}
	if len(plan.guards) > 0 {
		document.APIVersion = GuardedPlanAPIVersion
		document.DirectoryGuards = plan.DirectoryGuards()
	}
	for index, change := range plan.changes {
		before := plan.before.files[index]
		item := planChangeDocument{
			Path: change.public.Path, Operation: change.public.Operation,
			Before: fileDocument(before), ContentDigest: change.public.ContentDigest,
		}
		if change.public.Operation == OperationCreate || change.public.Operation == OperationUpdate {
			item.ContentBase64 = base64.StdEncoding.EncodeToString(change.content)
			item.Mode = formatPlanMode(change.public.Mode)
		}
		document.Changes[index] = item
	}
	encoded, err := encodePlanDocument(document)
	if err != nil {
		return nil, fmt.Errorf("%w: encode: %w", ErrInvalidPlanDocument, err)
	}
	if len(encoded) > MaxPlanDocumentBytes {
		return nil, fmt.Errorf("%w: document exceeds %d bytes", ErrInvalidPlanDocument, MaxPlanDocumentBytes)
	}
	return encoded, nil
}

// ParsePlan accepts only MarshalPlan's canonical representation. It performs
// no filesystem I/O and reconstructs an immutable Plan with detached payloads.
func ParsePlan(data []byte) (Plan, error) {
	if len(data) == 0 || len(data) > MaxPlanDocumentBytes {
		return Plan{}, fmt.Errorf("%w: document size is outside [1,%d]", ErrInvalidPlanDocument, MaxPlanDocumentBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document planDocument
	if err := decoder.Decode(&document); err != nil {
		return Plan{}, fmt.Errorf("%w: decode: %w", ErrInvalidPlanDocument, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Plan{}, fmt.Errorf("%w: multiple JSON values", ErrInvalidPlanDocument)
		}
		return Plan{}, fmt.Errorf("%w: trailing input: %w", ErrInvalidPlanDocument, err)
	}
	legacy := document.APIVersion == PlanAPIVersion && len(document.DirectoryGuards) == 0
	guarded := document.APIVersion == GuardedPlanAPIVersion && len(document.DirectoryGuards) > 0 && len(document.DirectoryGuards) <= MaxDirectoryGuards
	if (!legacy && !guarded) || document.Kind != PlanKind ||
		len(document.Changes) == 0 || len(document.Changes) > MaxPlanChanges {
		return Plan{}, fmt.Errorf("%w: unsupported header or change count", ErrInvalidPlanDocument)
	}
	canonicalDocument, err := encodePlanDocument(document)
	if err != nil {
		return Plan{}, fmt.Errorf("%w: canonicalize: %w", ErrInvalidPlanDocument, err)
	}
	if !bytes.Equal(data, canonicalDocument) {
		return Plan{}, ErrNonCanonicalPlan
	}
	before := make([]File, len(document.Changes))
	changes := make([]preparedChange, len(document.Changes))
	var contentBytes, pathBytes uint64
	for index, item := range document.Changes {
		if len(item.Path) > maxPlanPathBytes || !validRelativePath(item.Path) || reservedWorkspacePath(item.Path) {
			return Plan{}, fmt.Errorf("%w: changes[%d].path", ErrInvalidPlanDocument, index)
		}
		if uint64(len(item.Path)) > MaxPlanPathBytes-pathBytes {
			return Plan{}, fmt.Errorf("%w: aggregate target paths exceed %d bytes", ErrInvalidPlanDocument, MaxPlanPathBytes)
		}
		pathBytes += uint64(len(item.Path))
		state, err := parseFileDocument(item.Path, item.Before)
		if err != nil {
			return Plan{}, fmt.Errorf("%w: changes[%d].before: %w", ErrInvalidPlanDocument, index, err)
		}
		before[index] = state
		public := PlannedChange{Path: item.Path, Operation: item.Operation, ContentDigest: item.ContentDigest}
		switch item.Operation {
		case OperationCreate, OperationUpdate:
			mode, err := parsePlanMode(item.Mode)
			if err != nil {
				return Plan{}, fmt.Errorf("%w: changes[%d].mode: %w", ErrInvalidPlanDocument, index, err)
			}
			remaining := MaxPlanContentBytes - contentBytes
			if len(item.ContentBase64) > base64.StdEncoding.EncodedLen(int(remaining)) ||
				strings.ContainsAny(item.ContentBase64, "\r\n") {
				return Plan{}, fmt.Errorf("%w: changes[%d].contentBase64", ErrInvalidPlanDocument, index)
			}
			content, err := base64.StdEncoding.Strict().DecodeString(item.ContentBase64)
			if err != nil {
				return Plan{}, fmt.Errorf("%w: changes[%d].contentBase64", ErrInvalidPlanDocument, index)
			}
			if uint64(len(content)) > MaxPlanContentBytes-contentBytes {
				return Plan{}, fmt.Errorf("%w: write payload exceeds %d bytes", ErrInvalidPlanDocument, MaxPlanContentBytes)
			}
			contentBytes += uint64(len(content))
			public.Mode = mode
			changes[index] = preparedChange{public: public, content: content}
		case OperationDelete, OperationAssert:
			if item.ContentDigest != "" || item.ContentBase64 != "" || item.Mode != "" {
				return Plan{}, fmt.Errorf("%w: changes[%d] %s contains write fields", ErrInvalidPlanDocument, index, item.Operation)
			}
			changes[index] = preparedChange{public: public}
		default:
			return Plan{}, fmt.Errorf("%w: changes[%d].operation", ErrInvalidPlanDocument, index)
		}
	}
	plan := Plan{
		before:  Snapshot{files: before, digest: document.SnapshotDigest},
		changes: changes, digest: document.PlanDigest, finalDigest: document.FinalDigest,
		guards: document.DirectoryGuards,
	}
	finalFiles, err := desiredFiles(plan.before.files, plan.changes)
	if err != nil {
		return Plan{}, fmt.Errorf("%w: transitions: %w", ErrInvalidPlanDocument, err)
	}
	plan.finalFiles = finalFiles
	if err := plan.Validate(); err != nil {
		return Plan{}, fmt.Errorf("%w: %w", ErrInvalidPlanDocument, err)
	}
	return plan, nil
}

func fileDocument(file File) planFileDocument {
	result := planFileDocument{Exists: file.Exists}
	if file.Exists {
		result.Digest = file.Digest
		result.Mode = formatPlanMode(file.Mode)
	}
	return result
}

func parseFileDocument(name string, document planFileDocument) (File, error) {
	if !document.Exists {
		if document.Digest != "" || document.Mode != "" {
			return File{}, errors.New("missing file has digest or mode")
		}
		return File{Path: name}, nil
	}
	mode, err := parsePlanMode(document.Mode)
	if err != nil || !validDigest(document.Digest) {
		return File{}, errors.New("existing file has invalid digest or mode")
	}
	return File{Path: name, Exists: true, Digest: document.Digest, Mode: mode}, nil
}

func formatPlanMode(mode fs.FileMode) string { return fmt.Sprintf("%04o", uint32(mode.Perm())) }

func parsePlanMode(value string) (fs.FileMode, error) {
	if len(value) != 4 || value[0] != '0' {
		return 0, errors.New("mode must be four canonical octal digits")
	}
	parsed, err := strconv.ParseUint(value, 8, 16)
	mode := fs.FileMode(parsed)
	if err != nil || formatPlanMode(mode) != value || !validTargetMode(mode) {
		return 0, errors.New("mode is invalid")
	}
	return mode, nil
}

func encodePlanDocument(document planDocument) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

package workspace

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
)

var (
	// ErrInvalidChange identifies a malformed or contradictory requested file
	// operation.
	ErrInvalidChange = errors.New("workspace: invalid change")
	// ErrInvalidPlan identifies a zero, corrupted, or internally inconsistent
	// plan. Plans are immutable values produced by BuildPlan.
	ErrInvalidPlan = errors.New("workspace: invalid plan")
	// ErrRollback identifies one or more failures while restoring the captured
	// precondition after a commit error.
	ErrRollback = errors.New("workspace: rollback failed")
)

const planFormat = "appkit.workspace.plan.v1\x00"

const (
	// MaxPlanChanges is the implementation ceiling for one atomic target set.
	MaxPlanChanges = 4_096
	// MaxPlanContentBytes bounds the detached write payload retained by a Plan.
	MaxPlanContentBytes = 8 << 20
	// MaxPlanPathBytes bounds the aggregate target-path bytes retained by one
	// Plan. Combined with the content and change ceilings, this keeps every
	// serialized plan within a practical nested-wire allocation budget.
	MaxPlanPathBytes = 1 << 20
	maxPlanPathBytes = 1_024
)

// Operation is one explicit target-file transition.
type Operation string

const (
	OperationCreate Operation = "create"
	OperationUpdate Operation = "update"
	OperationDelete Operation = "delete"
	// OperationAssert binds a file's captured state to a Plan without ever
	// renaming, writing, chmodding, creating, or deleting that path.
	OperationAssert Operation = "assert"
)

// Change requests one file transition. Create and update content is copied by
// BuildPlan and their modes must permit owner reads so future digest
// verification remains possible. Delete requires nil Content and a zero Mode.
type Change struct {
	Path      string
	Operation Operation
	Content   []byte
	Mode      fs.FileMode
}

// PlannedChange is the payload-free, deterministic representation of a
// change. ContentDigest is populated only for create and update.
type PlannedChange struct {
	Path          string
	Operation     Operation
	ContentDigest string
	Mode          fs.FileMode
}

type preparedChange struct {
	public  PlannedChange
	content []byte
}

// Plan binds an explicit operation set to both the captured current snapshot
// and the desired final snapshot. Its zero value is invalid.
type Plan struct {
	before      Snapshot
	finalFiles  []File
	changes     []preparedChange
	digest      string
	finalDigest string
	guards      []DirectoryGuard
}

// BuildPlan captures the current state of every explicit target and creates a
// deterministic immutable plan. Input order does not affect the result.
func BuildPlan(root string, input []Change) (Plan, error) {
	changes, paths, err := prepareChanges(input)
	if err != nil {
		return Plan{}, err
	}
	before, err := Capture(root, paths)
	if err != nil {
		return Plan{}, err
	}
	finalFiles, err := desiredFiles(before.files, changes)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{
		before:      cloneSnapshot(before),
		finalFiles:  slices.Clone(finalFiles),
		changes:     clonePreparedChanges(changes),
		finalDigest: digestFiles(finalFiles),
	}
	plan.digest = digestPlan(plan)
	return plan, nil
}

// Digest returns the domain-separated digest binding precondition and desired
// state.
func (plan Plan) Digest() string { return plan.digest }

// SnapshotDigest returns the captured precondition digest.
func (plan Plan) SnapshotDigest() string { return plan.before.digest }

// FinalDigest returns the desired target-set digest.
func (plan Plan) FinalDigest() string { return plan.finalDigest }

// Changes returns a payload-free copy in canonical path order.
func (plan Plan) Changes() []PlannedChange {
	result := make([]PlannedChange, len(plan.changes))
	for index := range plan.changes {
		result[index] = plan.changes[index].public
	}
	return result
}

// Preconditions returns the exact file states captured for every change in
// the same canonical path order as Changes.
func (plan Plan) Preconditions() []File {
	return slices.Clone(plan.before.files)
}

func prepareChanges(input []Change) ([]preparedChange, []string, error) {
	if len(input) == 0 {
		return nil, nil, fmt.Errorf("%w: operation set is empty", ErrInvalidChange)
	}
	if len(input) > MaxPlanChanges {
		return nil, nil, fmt.Errorf("%w: operation count exceeds %d", ErrInvalidChange, MaxPlanChanges)
	}
	changes := make([]preparedChange, len(input))
	var contentBytes, pathBytes uint64
	for index, change := range input {
		if len(change.Path) > maxPlanPathBytes || !validRelativePath(change.Path) {
			return nil, nil, fmt.Errorf("%w: change[%d]: %w: %q", ErrInvalidChange, index, ErrInvalidPath, change.Path)
		}
		if reservedWorkspacePath(change.Path) {
			return nil, nil, fmt.Errorf("%w: change[%d] targets reserved workspace state %q", ErrInvalidChange, index, change.Path)
		}
		if uint64(len(change.Path)) > MaxPlanPathBytes-pathBytes {
			return nil, nil, fmt.Errorf("%w: aggregate target paths exceed %d bytes", ErrInvalidChange, MaxPlanPathBytes)
		}
		pathBytes += uint64(len(change.Path))
		planned := PlannedChange{Path: change.Path, Operation: change.Operation, Mode: change.Mode}
		switch change.Operation {
		case OperationCreate, OperationUpdate:
			if !validTargetMode(change.Mode) {
				return nil, nil, fmt.Errorf("%w: change[%d] has invalid target mode %v", ErrInvalidChange, index, change.Mode)
			}
			if uint64(len(change.Content)) > MaxPlanContentBytes-contentBytes {
				return nil, nil, fmt.Errorf("%w: write payload exceeds %d bytes", ErrInvalidChange, MaxPlanContentBytes)
			}
			contentBytes += uint64(len(change.Content))
			changes[index] = preparedChange{
				public:  planned,
				content: slices.Clone(change.Content),
			}
			changes[index].public.ContentDigest = digestBytes(changes[index].content)
		case OperationDelete, OperationAssert:
			if change.Content != nil || change.Mode != 0 {
				return nil, nil, fmt.Errorf("%w: %s change[%d] must not contain content or mode", ErrInvalidChange, change.Operation, index)
			}
			changes[index] = preparedChange{public: planned}
		default:
			return nil, nil, fmt.Errorf("%w: change[%d] has unsupported operation %q", ErrInvalidChange, index, change.Operation)
		}
	}

	slices.SortFunc(changes, func(left, right preparedChange) int {
		return strings.Compare(left.public.Path, right.public.Path)
	})
	paths := make([]string, len(changes))
	seen := make(map[string]struct{}, len(changes))
	for index, change := range changes {
		path := change.public.Path
		if _, duplicate := seen[path]; duplicate {
			return nil, nil, fmt.Errorf("%w: duplicate target %q", ErrInvalidChange, path)
		}
		for offset := strings.IndexByte(path, '/'); offset >= 0; {
			if _, parent := seen[path[:offset]]; parent {
				return nil, nil, fmt.Errorf("%w: target %q conflicts with parent target %q", ErrInvalidChange, path, path[:offset])
			}
			next := strings.IndexByte(path[offset+1:], '/')
			if next < 0 {
				break
			}
			offset += next + 1
		}
		seen[path] = struct{}{}
		paths[index] = path
	}
	return changes, paths, nil
}

func desiredFiles(before []File, changes []preparedChange) ([]File, error) {
	if len(before) != len(changes) {
		return nil, fmt.Errorf("%w: target and snapshot lengths differ", ErrInvalidPlan)
	}
	result := make([]File, len(before))
	for index, change := range changes {
		current := before[index]
		if current.Path != change.public.Path {
			return nil, fmt.Errorf("%w: target and snapshot paths differ", ErrInvalidPlan)
		}
		switch change.public.Operation {
		case OperationCreate:
			if current.Exists {
				return nil, fmt.Errorf("%w: create target %q already exists", ErrInvalidChange, current.Path)
			}
			result[index] = File{Path: current.Path, Exists: true, Mode: change.public.Mode, Digest: change.public.ContentDigest}
		case OperationUpdate:
			if !current.Exists {
				return nil, fmt.Errorf("%w: update target %q does not exist", ErrInvalidChange, current.Path)
			}
			result[index] = File{Path: current.Path, Exists: true, Mode: change.public.Mode, Digest: change.public.ContentDigest}
		case OperationDelete:
			if !current.Exists {
				return nil, fmt.Errorf("%w: delete target %q does not exist", ErrInvalidChange, current.Path)
			}
			result[index] = File{Path: current.Path}
		case OperationAssert:
			result[index] = current
		default:
			return nil, fmt.Errorf("%w: unsupported operation %q", ErrInvalidPlan, change.public.Operation)
		}
	}
	return result, nil
}

// Validate detects zero values, mutation, contradictory file transitions, and
// payload/digest mismatches without consulting a workspace.
func (plan Plan) Validate() error {
	if !validSnapshotFiles(plan.before.files) ||
		!validDigest(plan.before.digest) ||
		digestFiles(plan.before.files) != plan.before.digest ||
		len(plan.changes) == 0 || len(plan.changes) > MaxPlanChanges ||
		len(plan.finalFiles) != len(plan.changes) ||
		!validSnapshotFiles(plan.finalFiles) ||
		!validDigest(plan.finalDigest) ||
		digestFiles(plan.finalFiles) != plan.finalDigest ||
		!validDigest(plan.digest) {
		return fmt.Errorf("%w: malformed plan", ErrInvalidPlan)
	}
	var contentBytes, pathBytes uint64
	seen := make(map[string]struct{}, len(plan.changes))
	for index, change := range plan.changes {
		public := change.public
		if len(public.Path) > maxPlanPathBytes || !validRelativePath(public.Path) || reservedWorkspacePath(public.Path) ||
			public.Path != plan.before.files[index].Path || public.Path != plan.finalFiles[index].Path {
			return fmt.Errorf("%w: malformed target at index %d", ErrInvalidPlan, index)
		}
		if uint64(len(public.Path)) > MaxPlanPathBytes-pathBytes {
			return fmt.Errorf("%w: aggregate target paths exceed %d bytes", ErrInvalidPlan, MaxPlanPathBytes)
		}
		pathBytes += uint64(len(public.Path))
		if index > 0 && public.Path <= plan.changes[index-1].public.Path {
			return fmt.Errorf("%w: targets are not canonical", ErrInvalidPlan)
		}
		for offset := strings.IndexByte(public.Path, '/'); offset >= 0; {
			if _, parent := seen[public.Path[:offset]]; parent {
				return fmt.Errorf("%w: target %q conflicts with parent target %q", ErrInvalidPlan, public.Path, public.Path[:offset])
			}
			next := strings.IndexByte(public.Path[offset+1:], '/')
			if next < 0 {
				break
			}
			offset += next + 1
		}
		seen[public.Path] = struct{}{}
		switch public.Operation {
		case OperationCreate, OperationUpdate:
			if !validTargetMode(public.Mode) || !validDigest(public.ContentDigest) || public.ContentDigest != digestBytes(change.content) {
				return fmt.Errorf("%w: malformed write target %q", ErrInvalidPlan, public.Path)
			}
			if uint64(len(change.content)) > MaxPlanContentBytes-contentBytes {
				return fmt.Errorf("%w: write payload exceeds %d bytes", ErrInvalidPlan, MaxPlanContentBytes)
			}
			contentBytes += uint64(len(change.content))
		case OperationDelete, OperationAssert:
			if public.Mode != 0 || public.ContentDigest != "" || change.content != nil {
				return fmt.Errorf("%w: malformed %s target %q", ErrInvalidPlan, public.Operation, public.Path)
			}
		default:
			return fmt.Errorf("%w: unsupported operation %q", ErrInvalidPlan, public.Operation)
		}
	}
	wantFinal, err := desiredFiles(plan.before.files, plan.changes)
	if err != nil || !slices.Equal(wantFinal, plan.finalFiles) || digestPlan(plan) != plan.digest {
		return fmt.Errorf("%w: modified plan", ErrInvalidPlan)
	}
	if err := validateDirectoryGuards(plan); err != nil {
		return err
	}
	return nil
}

func (plan Plan) validate() error { return plan.Validate() }

func validTargetMode(mode fs.FileMode) bool {
	return mode == mode.Perm() && mode&0o400 != 0
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	return Snapshot{files: slices.Clone(snapshot.files), digest: snapshot.digest}
}

func clonePreparedChanges(source []preparedChange) []preparedChange {
	result := slices.Clone(source)
	for index := range result {
		result[index].content = slices.Clone(source[index].content)
	}
	return result
}

func digestBytes(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func digestPlan(plan Plan) string {
	hasher := sha256.New()
	if len(plan.guards) == 0 {
		_, _ = hasher.Write([]byte(planFormat))
	} else {
		_, _ = hasher.Write([]byte("appkit.workspace.plan.v2\x00"))
	}
	writePlanString := func(value string) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = hasher.Write(size[:])
		_, _ = hasher.Write([]byte(value))
	}
	writePlanString(plan.before.digest)
	writePlanString(plan.finalDigest)
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(plan.changes)))
	_, _ = hasher.Write(count[:])
	for _, change := range plan.changes {
		writePlanString(change.public.Path)
		writePlanString(string(change.public.Operation))
		writePlanString(change.public.ContentDigest)
		binary.BigEndian.PutUint64(count[:], uint64(change.public.Mode))
		_, _ = hasher.Write(count[:])
	}
	if len(plan.guards) > 0 {
		binary.BigEndian.PutUint64(count[:], uint64(len(plan.guards)))
		_, _ = hasher.Write(count[:])
		for _, guard := range plan.guards {
			for _, snapshot := range []DirectorySnapshot{guard.Before, guard.After} {
				writePlanString(snapshot.Path)
				if snapshot.Exists {
					_, _ = hasher.Write([]byte{1})
				} else {
					_, _ = hasher.Write([]byte{0})
				}
				binary.BigEndian.PutUint64(count[:], uint64(len(snapshot.Entries)))
				_, _ = hasher.Write(count[:])
				for _, entry := range snapshot.Entries {
					writePlanString(entry.Path)
					writePlanString(entry.Kind)
				}
			}
		}
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil))
}

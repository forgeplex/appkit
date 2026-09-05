package workspace

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
)

const (
	MaxDirectoryGuards    = 32
	MaxDirectoryEntries   = 8192
	MaxDirectoryPathBytes = 1 << 20
	DirectoryFile         = "file"
	DirectoryDir          = "directory"
)

// DirectoryEntry is a path relative to its guarded directory. Membership
// includes empty directories, but not file contents; assert the relevant files
// separately to bind contents and permissions as well.
type DirectoryEntry struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// DirectorySnapshot records exact recursive membership of a directory. Missing
// directories have Exists=false and no entries. Entries are lexical by Path.
// Path may be "."; reserved workspace transaction directories are excluded only
// at the workspace root, since the engine owns those temporary paths.
type DirectorySnapshot struct {
	Path    string           `json:"path"`
	Exists  bool             `json:"exists"`
	Entries []DirectoryEntry `json:"entries"`
}

// DirectoryGuard binds both the captured tree and the membership derived from
// the plan's explicit file transitions. Callers cannot supply an arbitrary After.
type DirectoryGuard struct {
	Before DirectorySnapshot `json:"before"`
	After  DirectorySnapshot `json:"after"`
}

// CaptureDirectory reads bounded recursive membership through a confined root.
// Symlinks and special files are rejected, including in directory ancestors.
// Two matching walks detect membership changes during capture. Callers composing
// several snapshots should use WithReadLock to exclude cooperative writers.
func CaptureDirectory(root, dir string) (snapshot DirectorySnapshot, resultErr error) {
	if !validDirectoryPath(dir) {
		return DirectorySnapshot{}, fmt.Errorf("%w: guarded directory %q", ErrInvalidPath, dir)
	}
	rooted, err := openWorkspaceRoot(root)
	if err != nil {
		return DirectorySnapshot{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, rooted.Close()) }()
	return captureDirectory(rooted, dir)
}

func captureDirectory(root *os.Root, dir string) (DirectorySnapshot, error) {
	first, err := walkDirectory(root, dir)
	if err != nil {
		return DirectorySnapshot{}, err
	}
	second, err := walkDirectory(root, dir)
	if err != nil {
		return DirectorySnapshot{}, err
	}
	if !equalDirectory(first, second) {
		return DirectorySnapshot{}, fmt.Errorf("%w: directory %q changed during capture", ErrChanged, dir)
	}
	if err := verifyOpenedRootPath(root); err != nil {
		return DirectorySnapshot{}, err
	}
	return first, nil
}

func walkDirectory(root *os.Root, dir string) (DirectorySnapshot, error) {
	result := DirectorySnapshot{Path: dir, Entries: []DirectoryEntry{}}
	info, missing, err := inspectDirectory(root, dir)
	if err != nil || missing {
		return result, err
	}
	result.Exists = true
	pathBytes := len(dir)
	visited := 0
	var walk func(string, fs.FileInfo) error
	walk = func(relative string, before fs.FileInfo) error {
		name := path.Join(dir, relative)
		directory, err := root.Open(name)
		if err != nil {
			return err
		}
		defer directory.Close()
		opened, err := directory.Stat()
		if err != nil {
			return err
		}
		if !opened.IsDir() || !os.SameFile(before, opened) {
			return fmt.Errorf("%w: directory %q changed while opening", ErrChanged, name)
		}
		for {
			entries, readErr := directory.ReadDir(128)
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return readErr
			}
			for _, entry := range entries {
				visited++
				if visited > 2*MaxDirectoryEntries {
					return fmt.Errorf("%w: directory %q exceeds walk limit", ErrInvalidPath, dir)
				}
				child := path.Join(relative, entry.Name())
				full := path.Join(dir, child)
				if dir == "." && reservedWorkspacePath(child) {
					continue
				}
				if !validRelativePath(child) || len(child) > maxPlanPathBytes || len(result.Entries) >= MaxDirectoryEntries {
					return fmt.Errorf("%w: directory %q exceeds entry/path bounds", ErrInvalidPath, dir)
				}
				pathBytes += len(child)
				if pathBytes > MaxDirectoryPathBytes {
					return fmt.Errorf("%w: directory %q exceeds path-byte limit", ErrInvalidPath, dir)
				}
				childInfo, err := root.Lstat(full)
				if err != nil {
					return fmt.Errorf("%w: inspect directory member %q: %w", ErrChanged, full, err)
				}
				kind := DirectoryFile
				switch {
				case childInfo.Mode()&fs.ModeSymlink != 0:
					return fmt.Errorf("%w: %q", ErrSymlink, full)
				case childInfo.IsDir():
					kind = DirectoryDir
				case !childInfo.Mode().IsRegular():
					return fmt.Errorf("%w: directory member %q is not regular", ErrInvalidPath, full)
				}
				result.Entries = append(result.Entries, DirectoryEntry{Path: child, Kind: kind})
				if kind == DirectoryDir {
					if err := walk(child, childInfo); err != nil {
						return err
					}
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
		}
		after, missing, err := inspectDirectory(root, name)
		if err != nil {
			return err
		}
		if missing || !sameFileState(opened, after) {
			return fmt.Errorf("%w: directory %q changed during walk", ErrChanged, name)
		}
		return nil
	}
	if err := walk(".", info); err != nil {
		return DirectorySnapshot{}, err
	}
	slices.SortFunc(result.Entries, compareDirectoryEntry)
	return result, nil
}

func inspectDirectory(root *os.Root, name string) (fs.FileInfo, bool, error) {
	if !validDirectoryPath(name) {
		return nil, false, fmt.Errorf("%w: directory %q", ErrInvalidPath, name)
	}
	prefix := ""
	for _, part := range strings.Split(name, "/") {
		prefix = path.Join(prefix, part)
		info, err := root.Lstat(prefix)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, true, nil
		}
		if err != nil {
			return nil, false, err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, false, fmt.Errorf("%w: directory %q", ErrSymlink, prefix)
		}
		if !info.IsDir() {
			return nil, false, fmt.Errorf("%w: %q is not a directory", ErrInvalidPath, prefix)
		}
		if prefix == name {
			return info, false, nil
		}
	}
	return nil, false, fmt.Errorf("%w: directory %q", ErrInvalidPath, name)
}

func validDirectoryPath(name string) bool {
	return (name == "." || validRelativePath(name)) && len(name) <= maxPlanPathBytes && !reservedWorkspacePath(name)
}

func compareDirectoryEntry(a, b DirectoryEntry) int { return strings.Compare(a.Path, b.Path) }

func cloneDirectory(snapshot DirectorySnapshot) DirectorySnapshot {
	snapshot.Entries = append([]DirectoryEntry{}, snapshot.Entries...)
	return snapshot
}

func equalDirectory(a, b DirectorySnapshot) bool {
	return a.Path == b.Path && a.Exists == b.Exists && slices.Equal(a.Entries, b.Entries)
}

func validateDirectory(snapshot DirectorySnapshot) error {
	if !validDirectoryPath(snapshot.Path) || snapshot.Entries == nil || len(snapshot.Entries) > MaxDirectoryEntries ||
		(!snapshot.Exists && len(snapshot.Entries) != 0) || (snapshot.Path == "." && !snapshot.Exists) {
		return fmt.Errorf("%w: malformed directory snapshot", ErrInvalidPlan)
	}
	parents := make(map[string]string, len(snapshot.Entries))
	pathBytes := len(snapshot.Path)
	for index, entry := range snapshot.Entries {
		if !validRelativePath(entry.Path) || len(entry.Path) > maxPlanPathBytes ||
			(entry.Kind != DirectoryDir && entry.Kind != DirectoryFile) ||
			(index > 0 && entry.Path <= snapshot.Entries[index-1].Path) ||
			(snapshot.Path == "." && reservedWorkspacePath(entry.Path)) {
			return fmt.Errorf("%w: malformed directory entry", ErrInvalidPlan)
		}
		pathBytes += len(entry.Path)
		if pathBytes > MaxDirectoryPathBytes {
			return fmt.Errorf("%w: directory path-byte limit", ErrInvalidPlan)
		}
		if parent := path.Dir(entry.Path); parent != "." && parents[parent] != DirectoryDir {
			return fmt.Errorf("%w: entry %q has no directory parent", ErrInvalidPlan, entry.Path)
		}
		parents[entry.Path] = entry.Kind
	}
	return nil
}

// BuildPlanWithGuards binds captured directory snapshots and derives their final
// membership from changes. Each supplied snapshot is compared with live state;
// additions made after input enumeration therefore cannot disappear from a plan.
// An empty guard list has exactly BuildPlan's legacy digest and wire encoding.
func BuildPlanWithGuards(root string, changes []Change, before []DirectorySnapshot) (result Plan, resultErr error) {
	if len(before) == 0 {
		return BuildPlan(root, changes)
	}
	if len(before) > MaxDirectoryGuards {
		return Plan{}, fmt.Errorf("%w: too many directory guards", ErrInvalidPlan)
	}
	guards := make([]DirectoryGuard, len(before))
	for i, snapshot := range before {
		if err := validateDirectory(snapshot); err != nil {
			return Plan{}, err
		}
		guards[i].Before = cloneDirectory(snapshot)
	}
	slices.SortFunc(guards, func(a, b DirectoryGuard) int { return strings.Compare(a.Before.Path, b.Before.Path) })
	rooted, err := openWorkspaceRoot(root)
	if err != nil {
		return Plan{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, rooted.Close()) }()
	if err := verifyDirectoryGuards(rooted, Plan{guards: guards}, false); err != nil {
		return Plan{}, err
	}
	plan, err := BuildPlan(root, changes)
	if err != nil {
		return Plan{}, err
	}
	plan.guards = guards
	for i := range guards {
		guards[i].After, err = deriveDirectoryAfter(guards[i].Before, plan)
		if err != nil {
			return Plan{}, err
		}
	}
	plan.digest = digestPlan(plan)
	if err := plan.Validate(); err != nil {
		return Plan{}, err
	}
	err = verifyDirectoryGuards(rooted, plan, false)
	if err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// DirectoryGuards returns detached before/final memberships.
func (plan Plan) DirectoryGuards() []DirectoryGuard {
	result := make([]DirectoryGuard, len(plan.guards))
	for i, guard := range plan.guards {
		result[i] = DirectoryGuard{Before: cloneDirectory(guard.Before), After: cloneDirectory(guard.After)}
	}
	return result
}

func directoryRelative(dir, target string) (string, bool) {
	if dir == "." {
		return target, true
	}
	rel, found := strings.CutPrefix(target, dir+"/")
	return rel, found
}

func deriveDirectoryAfter(before DirectorySnapshot, plan Plan) (DirectorySnapshot, error) {
	after := cloneDirectory(before)
	entries := make(map[string]string, len(before.Entries))
	for _, entry := range before.Entries {
		entries[entry.Path] = entry.Kind
	}
	for i, change := range plan.changes {
		target := change.public.Path
		if target == before.Path || pathHasPrefix(before.Path, target) {
			return DirectorySnapshot{}, fmt.Errorf("%w: file target %q conflicts with guarded directory %q", ErrInvalidPlan, target, before.Path)
		}
		rel, inside := directoryRelative(before.Path, target)
		if !inside {
			continue
		}
		current := plan.before.files[i]
		kind := entries[rel]
		if (current.Exists && kind != DirectoryFile) || (!current.Exists && kind != "") {
			return DirectorySnapshot{}, fmt.Errorf("%w: file %q disagrees with directory snapshot", ErrInvalidPlan, target)
		}
		if !plan.finalFiles[i].Exists {
			delete(entries, rel)
			continue
		}
		after.Exists = true
		entries[rel] = DirectoryFile
		for parent := path.Dir(rel); parent != "."; parent = path.Dir(parent) {
			if kind := entries[parent]; kind != "" && kind != DirectoryDir {
				return DirectorySnapshot{}, fmt.Errorf("%w: generated parent is a file", ErrInvalidPlan)
			}
			entries[parent] = DirectoryDir
		}
	}
	after.Entries = make([]DirectoryEntry, 0, len(entries))
	for name, kind := range entries {
		after.Entries = append(after.Entries, DirectoryEntry{Path: name, Kind: kind})
	}
	slices.SortFunc(after.Entries, compareDirectoryEntry)
	return after, validateDirectory(after)
}

func validateDirectoryGuards(plan Plan) error {
	if len(plan.guards) > MaxDirectoryGuards {
		return fmt.Errorf("%w: too many directory guards", ErrInvalidPlan)
	}
	entries, pathBytes := 0, 0
	for i, guard := range plan.guards {
		if err := validateDirectory(guard.Before); err != nil {
			return err
		}
		if err := validateDirectory(guard.After); err != nil {
			return err
		}
		for prior := 0; prior < i; prior++ {
			prev := plan.guards[prior].Before.Path
			if guard.Before.Path <= prev || prev == "." || pathHasPrefix(guard.Before.Path, prev) {
				return fmt.Errorf("%w: directory guards overlap or are not canonical", ErrInvalidPlan)
			}
		}
		for _, snapshot := range []DirectorySnapshot{guard.Before, guard.After} {
			entries += len(snapshot.Entries)
			pathBytes += len(snapshot.Path)
			for _, entry := range snapshot.Entries {
				pathBytes += len(entry.Path)
			}
		}
		if entries > 2*MaxDirectoryEntries || pathBytes > MaxDirectoryPathBytes {
			return fmt.Errorf("%w: aggregate directory guard bounds", ErrInvalidPlan)
		}
		derived, err := deriveDirectoryAfter(guard.Before, plan)
		if err != nil || !equalDirectory(derived, guard.After) {
			return fmt.Errorf("%w: directory after state is not implied by file changes", ErrInvalidPlan)
		}
	}
	return nil
}

func verifyDirectoryGuards(root *os.Root, plan Plan, final bool) error {
	for _, guard := range plan.guards {
		want := guard.Before
		if final {
			want = guard.After
		}
		got, err := captureDirectory(root, want.Path)
		if err != nil {
			return fmt.Errorf("%w: recapture directory %q: %w", ErrChanged, want.Path, err)
		}
		if !equalDirectory(got, want) {
			return fmt.Errorf("%w: directory %q membership changed", ErrChanged, want.Path)
		}
	}
	return nil
}

// verifyRecoverableDirectories accepts only directory membership reachable by
// this plan's interrupted file writes and parent creation. It runs before any
// recovery mutation: an unrelated added file or removed source must not turn a
// valid journal into permission to rewrite a different workspace.
func verifyRecoverableDirectories(root *os.Root, plan Plan, current []File) error {
	for _, guard := range plan.guards {
		got, err := captureDirectory(root, guard.Before.Path)
		if err != nil {
			return err
		}
		if (guard.Before.Exists && !got.Exists) || (!guard.After.Exists && got.Exists) {
			return fmt.Errorf("%w: directory %q existence is not journal-reachable", ErrChanged, got.Path)
		}
		wantFiles, wantDirs, allowedDirs := map[string]bool{}, map[string]bool{}, map[string]bool{}
		for _, entry := range guard.Before.Entries {
			if entry.Kind == DirectoryFile {
				wantFiles[entry.Path] = true
			} else {
				wantDirs[entry.Path] = true
			}
		}
		for _, entry := range guard.After.Entries {
			if entry.Kind == DirectoryDir {
				allowedDirs[entry.Path] = true
			}
		}
		for _, file := range current {
			rel, inside := directoryRelative(guard.Before.Path, file.Path)
			if !inside {
				continue
			}
			delete(wantFiles, rel)
			if file.Exists {
				wantFiles[rel] = true
			}
		}
		for _, entry := range got.Entries {
			if entry.Kind == DirectoryFile {
				if !wantFiles[entry.Path] {
					return fmt.Errorf("%w: unexpected recovery file %q", ErrChanged, path.Join(got.Path, entry.Path))
				}
				delete(wantFiles, entry.Path)
			} else {
				if !allowedDirs[entry.Path] {
					return fmt.Errorf("%w: unexpected recovery directory %q", ErrChanged, path.Join(got.Path, entry.Path))
				}
				delete(wantDirs, entry.Path)
			}
		}
		if len(wantFiles) != 0 || len(wantDirs) != 0 {
			return fmt.Errorf("%w: directory %q is missing recovery members", ErrChanged, got.Path)
		}
	}
	return nil
}

// removeRecoveredDirectories removes only empty parents absent from Before and
// implied by the plan's creates. Existing empty directories survive deletes.
func removeRecoveredDirectories(root *os.Root, plan Plan) error {
	var names []string
	for _, guard := range plan.guards {
		before := make(map[string]bool, len(guard.Before.Entries))
		for _, entry := range guard.Before.Entries {
			if entry.Kind == DirectoryDir {
				before[entry.Path] = true
			}
		}
		for _, entry := range guard.After.Entries {
			if entry.Kind == DirectoryDir && !before[entry.Path] {
				names = append(names, path.Join(guard.Before.Path, entry.Path))
			}
		}
		if !guard.Before.Exists && guard.After.Exists {
			names = append(names, guard.Before.Path)
		}
	}
	slices.Sort(names)
	for i := len(names) - 1; i >= 0; i-- {
		name := names[i]
		_, missing, err := inspectDirectory(root, name)
		if err != nil {
			return err
		}
		if missing {
			continue
		}
		if err := root.Remove(name); err != nil {
			return fmt.Errorf("remove interrupted directory %q: %w", name, err)
		}
		if err := syncDirectory(root, path.Dir(name)); err != nil {
			return err
		}
	}
	return nil
}

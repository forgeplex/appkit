// Package workspace creates deterministic, path-safe plans and applies their
// explicit file transitions with verified snapshot preconditions.
package workspace

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

var (
	ErrInvalidPath  = errors.New("workspace: invalid path")
	ErrSymlink      = errors.New("workspace: symbolic link is not allowed")
	ErrChanged      = errors.New("workspace: precondition changed")
	ErrFileTooLarge = errors.New("workspace: file exceeds read limit")
)

const snapshotFormat = "appkit.workspace.snapshot.v1\x00"

const snapshotModeMask = fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

// File records the content state of one project-relative regular file. A
// missing file is a deliberate state used by plans that intend to create it.
type File struct {
	Path   string
	Exists bool
	Mode   fs.FileMode
	Digest string
}

// Snapshot is an immutable deterministic view of an explicit file set.
type Snapshot struct {
	files  []File
	digest string
}

// Capture snapshots paths below root. Paths must be normalized, slash-
// separated relative file names. Symbolic links are rejected in every path
// component below root so a precondition cannot escape or silently retarget
// its root.
func Capture(root string, paths []string) (Snapshot, error) {
	names, err := normalizePaths(paths)
	if err != nil {
		return Snapshot{}, err
	}

	rooted, err := openWorkspaceRoot(root)
	if err != nil {
		return Snapshot{}, err
	}

	files := make([]File, 0, len(names))
	for _, name := range names {
		file, captureErr := captureFile(rooted, name)
		if captureErr != nil {
			_ = rooted.Close()
			return Snapshot{}, captureErr
		}
		files = append(files, file)
	}
	if err := rooted.Close(); err != nil {
		return Snapshot{}, fmt.Errorf("workspace: close root: %w", err)
	}
	return Snapshot{files: files, digest: digestFiles(files)}, nil
}

// ReadFile reads one bounded regular file through the same symlink-safe root
// confinement and double-read stability checks used by Capture. A missing
// path is returned as an Exists=false File and nil content.
func ReadFile(root, name string, maxBytes int64) (content []byte, state File, resultErr error) {
	if !validRelativePath(name) {
		return nil, File{}, fmt.Errorf("%w: %q", ErrInvalidPath, name)
	}
	if maxBytes < 0 {
		return nil, File{}, fmt.Errorf("%w: negative read limit", ErrInvalidPath)
	}
	rooted, err := openWorkspaceRoot(root)
	if err != nil {
		return nil, File{}, err
	}
	defer func() {
		if closeErr := rooted.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("workspace: close root: %w", closeErr))
		}
	}()
	return readBoundedFile(rooted, name, maxBytes)
}

// Files returns a copy of the canonical file list.
func (snapshot Snapshot) Files() []File {
	return slices.Clone(snapshot.files)
}

// Digest returns the canonical snapshot digest.
func (snapshot Snapshot) Digest() string { return snapshot.digest }

// Verify recaptures the same paths below root and returns ErrChanged when any
// file's existence, permissions, type, or content differs. If recapturing a
// path fails, the error matches both ErrChanged and the underlying cause.
func (snapshot Snapshot) Verify(root string) error {
	if !validSnapshotFiles(snapshot.files) ||
		!validDigest(snapshot.digest) ||
		digestFiles(snapshot.files) != snapshot.digest {
		return fmt.Errorf("%w: invalid snapshot", ErrChanged)
	}
	paths := make([]string, len(snapshot.files))
	for index, file := range snapshot.files {
		paths[index] = file.Path
	}
	current, err := Capture(root, paths)
	if err != nil {
		return fmt.Errorf("%w: recapture: %w", ErrChanged, err)
	}
	if current.digest != snapshot.digest {
		return fmt.Errorf("%w: have %s, want %s", ErrChanged, current.digest, snapshot.digest)
	}
	return nil
}

// openWorkspaceRoot canonicalizes symlinks in ancestors of root and opens a
// directory handle for all subsequent access. The final root itself may not
// be a symlink. Comparing identities before and after opening prevents a
// concurrent rename or link swap from silently changing the selected root.
func openWorkspaceRoot(name string) (*os.Root, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: root is empty", ErrInvalidPath)
	}
	absolute, err := filepath.Abs(name)
	if err != nil {
		return nil, fmt.Errorf("workspace: absolute root: %w", err)
	}

	before, err := os.Lstat(absolute)
	if err != nil {
		return nil, fmt.Errorf("workspace: inspect root: %w", err)
	}
	if before.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: root", ErrSymlink)
	}
	if !before.IsDir() {
		return nil, fmt.Errorf("%w: root is not a directory", ErrInvalidPath)
	}

	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve root: %w", err)
	}
	canonicalInfo, err := os.Lstat(canonical)
	if err != nil {
		return nil, fmt.Errorf("workspace: inspect resolved root: %w", err)
	}
	after, err := os.Lstat(absolute)
	if err != nil {
		return nil, fmt.Errorf("%w: root changed while opening: %w", ErrChanged, err)
	}
	if after.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: root", ErrSymlink)
	}
	if !os.SameFile(before, canonicalInfo) || !os.SameFile(before, after) {
		return nil, fmt.Errorf("%w: root changed while opening", ErrChanged)
	}

	rooted, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, fmt.Errorf("workspace: open root: %w", err)
	}
	opened, err := rooted.Stat(".")
	if err != nil {
		_ = rooted.Close()
		return nil, fmt.Errorf("workspace: inspect opened root: %w", err)
	}
	latest, err := os.Lstat(canonical)
	if err != nil {
		_ = rooted.Close()
		return nil, fmt.Errorf("%w: root changed while opening: %w", ErrChanged, err)
	}
	if latest.Mode()&fs.ModeSymlink != 0 ||
		!os.SameFile(canonicalInfo, opened) ||
		!os.SameFile(canonicalInfo, latest) {
		_ = rooted.Close()
		return nil, fmt.Errorf("%w: root changed while opening", ErrChanged)
	}
	return rooted, nil
}

// verifyOpenedRootPath establishes a linearization point between the caller's
// workspace pathname and the directory inode held by os.Root. Operations use
// the handle for confinement, but Apply must not report success for a path
// that was rebound to another directory during its transaction.
func verifyOpenedRootPath(root *os.Root) error {
	opened, err := root.Stat(".")
	if err != nil {
		return fmt.Errorf("%w: inspect opened workspace root: %w", ErrChanged, err)
	}
	current, err := os.Lstat(root.Name())
	if err != nil {
		return fmt.Errorf("%w: inspect workspace root path: %w", ErrChanged, err)
	}
	if current.Mode()&fs.ModeSymlink != 0 || !current.IsDir() || !opened.IsDir() || !os.SameFile(opened, current) {
		return fmt.Errorf("%w: workspace root path no longer names the opened directory", ErrChanged)
	}
	return nil
}

func normalizePaths(paths []string) ([]string, error) {
	names := append([]string(nil), paths...)
	slices.Sort(names)
	for index, name := range names {
		if !validRelativePath(name) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidPath, name)
		}
		if index > 0 && name == names[index-1] {
			return nil, fmt.Errorf("%w: duplicate %q", ErrInvalidPath, name)
		}
	}
	return names, nil
}

func validRelativePath(name string) bool {
	// fs.ValidPath gives this API one platform-independent path grammar.
	// Backslashes are rejected explicitly because os.Root treats them as path
	// separators on Windows even though io/fs permits them as ordinary bytes.
	return name != "." &&
		fs.ValidPath(name) &&
		!strings.ContainsRune(name, '\x00') &&
		strings.IndexFunc(name, unicode.IsControl) < 0 &&
		!strings.Contains(name, "\\") &&
		filepath.IsLocal(filepath.FromSlash(name))
}

type inspectedComponent struct {
	info fs.FileInfo
}

// inspectPath rejects every symlink and returns missing=true when any
// component is absent. A missing parent and a missing leaf are deliberately
// the same state because in both cases the explicit target does not exist.
func inspectPath(root *os.Root, name string) (components []inspectedComponent, missing bool, err error) {
	parts := strings.Split(name, "/")
	components = make([]inspectedComponent, 0, len(parts))
	prefix := ""
	for index, part := range parts {
		if prefix == "" {
			prefix = part
		} else {
			prefix += "/" + part
		}
		info, statErr := root.Lstat(prefix)
		if errors.Is(statErr, fs.ErrNotExist) {
			return components, true, nil
		}
		if statErr != nil {
			return nil, false, fmt.Errorf("workspace: inspect %q: %w", name, statErr)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, false, fmt.Errorf("%w: %q", ErrSymlink, prefix)
		}
		if index != len(parts)-1 && !info.IsDir() {
			return nil, false, fmt.Errorf("%w: %q has a non-directory parent", ErrInvalidPath, name)
		}
		if index == len(parts)-1 && !info.Mode().IsRegular() {
			return nil, false, fmt.Errorf("%w: %q is not a regular file", ErrInvalidPath, name)
		}
		components = append(components, inspectedComponent{info: info})
	}
	return components, false, nil
}

func captureFile(root *os.Root, name string) (File, error) {
	before, missing, err := inspectPath(root, name)
	if err != nil {
		return File{}, err
	}
	if missing {
		return File{Path: name}, nil
	}

	file, err := root.Open(name)
	if err != nil {
		return File{}, fmt.Errorf("workspace: open %q: %w", name, err)
	}
	openedBefore, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return File{}, fmt.Errorf("workspace: inspect opened %q: %w", name, err)
	}
	if !openedBefore.Mode().IsRegular() || !os.SameFile(before[len(before)-1].info, openedBefore) {
		_ = file.Close()
		return File{}, fmt.Errorf("%w: %q changed while opening", ErrChanged, name)
	}

	firstDigest, err := hashFile(file)
	if err != nil {
		_ = file.Close()
		return File{}, fmt.Errorf("workspace: read %q: %w", name, err)
	}
	openedMiddle, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return File{}, fmt.Errorf("workspace: re-inspect opened %q: %w", name, err)
	}
	if !sameFileState(openedBefore, openedMiddle) {
		_ = file.Close()
		return File{}, fmt.Errorf("%w: %q changed while reading", ErrChanged, name)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return File{}, fmt.Errorf("workspace: rewind %q: %w", name, err)
	}
	secondDigest, err := hashFile(file)
	if err != nil {
		_ = file.Close()
		return File{}, fmt.Errorf("workspace: reread %q: %w", name, err)
	}
	openedAfter, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return File{}, fmt.Errorf("workspace: final inspect of %q: %w", name, err)
	}
	if err := file.Close(); err != nil {
		return File{}, fmt.Errorf("workspace: close %q: %w", name, err)
	}
	if firstDigest != secondDigest || !sameFileState(openedMiddle, openedAfter) {
		return File{}, fmt.Errorf("%w: %q changed while reading", ErrChanged, name)
	}

	after, missing, err := inspectPath(root, name)
	if err != nil {
		return File{}, err
	}
	if missing || len(after) != len(before) {
		return File{}, fmt.Errorf("%w: %q changed while reading", ErrChanged, name)
	}
	for index := range before {
		if !os.SameFile(before[index].info, after[index].info) {
			return File{}, fmt.Errorf("%w: %q changed while reading", ErrChanged, name)
		}
	}
	if !os.SameFile(openedAfter, after[len(after)-1].info) ||
		snapshotMode(openedAfter.Mode()) != snapshotMode(after[len(after)-1].info.Mode()) {
		return File{}, fmt.Errorf("%w: %q changed while reading", ErrChanged, name)
	}

	return File{
		Path:   name,
		Exists: true,
		Mode:   snapshotMode(openedAfter.Mode()),
		Digest: firstDigest,
	}, nil
}

func readBoundedFile(root *os.Root, name string, maxBytes int64) ([]byte, File, error) {
	before, missing, err := inspectPath(root, name)
	if err != nil {
		return nil, File{}, err
	}
	if missing {
		return nil, File{Path: name}, nil
	}

	file, err := root.Open(name)
	if err != nil {
		return nil, File{}, fmt.Errorf("workspace: open %q: %w", name, err)
	}
	openedBefore, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, File{}, fmt.Errorf("workspace: inspect opened %q: %w", name, err)
	}
	if !openedBefore.Mode().IsRegular() || !os.SameFile(before[len(before)-1].info, openedBefore) {
		_ = file.Close()
		return nil, File{}, fmt.Errorf("%w: %q changed while opening", ErrChanged, name)
	}
	if openedBefore.Size() > maxBytes {
		_ = file.Close()
		return nil, File{}, fmt.Errorf("%w: %q", ErrFileTooLarge, name)
	}

	first, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		_ = file.Close()
		return nil, File{}, fmt.Errorf("workspace: read %q: %w", name, err)
	}
	if int64(len(first)) > maxBytes {
		_ = file.Close()
		return nil, File{}, fmt.Errorf("%w: %q", ErrFileTooLarge, name)
	}
	openedMiddle, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, File{}, fmt.Errorf("workspace: re-inspect opened %q: %w", name, err)
	}
	if !sameFileState(openedBefore, openedMiddle) {
		_ = file.Close()
		return nil, File{}, fmt.Errorf("%w: %q changed while reading", ErrChanged, name)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, File{}, fmt.Errorf("workspace: rewind %q: %w", name, err)
	}
	second, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		_ = file.Close()
		return nil, File{}, fmt.Errorf("workspace: reread %q: %w", name, err)
	}
	if int64(len(second)) > maxBytes {
		_ = file.Close()
		return nil, File{}, fmt.Errorf("%w: %q", ErrFileTooLarge, name)
	}
	openedAfter, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, File{}, fmt.Errorf("workspace: final inspect of %q: %w", name, err)
	}
	if err := file.Close(); err != nil {
		return nil, File{}, fmt.Errorf("workspace: close %q: %w", name, err)
	}
	if !bytes.Equal(first, second) || !sameFileState(openedMiddle, openedAfter) {
		return nil, File{}, fmt.Errorf("%w: %q changed while reading", ErrChanged, name)
	}

	after, missing, err := inspectPath(root, name)
	if err != nil {
		return nil, File{}, err
	}
	if missing || len(after) != len(before) {
		return nil, File{}, fmt.Errorf("%w: %q changed while reading", ErrChanged, name)
	}
	for index := range before {
		if !os.SameFile(before[index].info, after[index].info) {
			return nil, File{}, fmt.Errorf("%w: %q changed while reading", ErrChanged, name)
		}
	}
	if !os.SameFile(openedAfter, after[len(after)-1].info) ||
		snapshotMode(openedAfter.Mode()) != snapshotMode(after[len(after)-1].info.Mode()) {
		return nil, File{}, fmt.Errorf("%w: %q changed while reading", ErrChanged, name)
	}

	digest := sha256.Sum256(first)
	return slices.Clone(first), File{
		Path:   name,
		Exists: true,
		Mode:   snapshotMode(openedAfter.Mode()),
		Digest: "sha256:" + hex.EncodeToString(digest[:]),
	}, nil
}

func hashFile(file *os.File) (string, error) {
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

func sameFileState(left, right fs.FileInfo) bool {
	return os.SameFile(left, right) &&
		left.Size() == right.Size() &&
		left.ModTime().Equal(right.ModTime()) &&
		snapshotMode(left.Mode()) == snapshotMode(right.Mode())
}

func snapshotMode(mode fs.FileMode) fs.FileMode {
	return mode & snapshotModeMask
}

func validSnapshotFiles(files []File) bool {
	for index, file := range files {
		if !validRelativePath(file.Path) ||
			(index > 0 && file.Path <= files[index-1].Path) ||
			file.Mode != snapshotMode(file.Mode) {
			return false
		}
		if file.Exists {
			if !validDigest(file.Digest) {
				return false
			}
		} else if file.Mode != 0 || file.Digest != "" {
			return false
		}
	}
	return true
}

func digestFiles(files []File) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(snapshotFormat))
	var size [8]byte
	var mode [4]byte
	for _, file := range files {
		binary.BigEndian.PutUint64(size[:], uint64(len(file.Path)))
		_, _ = hasher.Write(size[:])
		_, _ = hasher.Write([]byte(file.Path))
		if file.Exists {
			_, _ = hasher.Write([]byte{1})
		} else {
			_, _ = hasher.Write([]byte{0})
		}
		binary.BigEndian.PutUint32(mode[:], uint32(snapshotMode(file.Mode)))
		_, _ = hasher.Write(mode[:])
		binary.BigEndian.PutUint64(size[:], uint64(len(file.Digest)))
		_, _ = hasher.Write(size[:])
		_, _ = hasher.Write([]byte(file.Digest))
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil))
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && value == "sha256:"+hex.EncodeToString(decoded)
}

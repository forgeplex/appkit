package agentplan

import (
	"bytes"
	"io"
	"io/fs"
	"path"
	"sort"
	"time"
)

// capturedFS is a detached, read-only filesystem. In particular migration
// execution must never reopen paths after the planner captures their digests.
type capturedFS map[string]memoryNode

type memoryNode struct {
	name string
	data []byte
	dir  bool
}

func newCapturedFS(files map[string][]byte, directories ...string) capturedFS {
	out := capturedFS{".": {name: ".", dir: true}}
	for _, name := range directories {
		for dir := name; dir != "."; dir = path.Dir(dir) {
			out[dir] = memoryNode{name: path.Base(dir), dir: true}
		}
	}
	for name, data := range files {
		out[name] = memoryNode{name: path.Base(name), data: bytes.Clone(data)}
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			out[parent] = memoryNode{name: path.Base(parent), dir: true}
		}
	}
	return out
}

func (m capturedFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	node, ok := m[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	file := &memoryFile{node: node, reader: bytes.NewReader(node.data)}
	if node.dir {
		for child, entry := range m {
			if child != "." && path.Dir(child) == name {
				file.entries = append(file.entries, fs.FileInfoToDirEntry(entry))
			}
		}
		sort.Slice(file.entries, func(i, j int) bool { return file.entries[i].Name() < file.entries[j].Name() })
	}
	return file, nil
}

func (n memoryNode) Name() string       { return n.name }
func (n memoryNode) Size() int64        { return int64(len(n.data)) }
func (n memoryNode) ModTime() time.Time { return time.Time{} }
func (n memoryNode) IsDir() bool        { return n.dir }
func (n memoryNode) Sys() any           { return nil }
func (n memoryNode) Mode() fs.FileMode {
	if n.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

type memoryFile struct {
	node    memoryNode
	reader  *bytes.Reader
	entries []fs.DirEntry
	offset  int
	closed  bool
}

func (f *memoryFile) Close() error { f.closed = true; return nil }
func (f *memoryFile) Stat() (fs.FileInfo, error) {
	if f.closed {
		return nil, fs.ErrClosed
	}
	return f.node, nil
}
func (f *memoryFile) Read(data []byte) (int, error) {
	if f.closed {
		return 0, fs.ErrClosed
	}
	if f.node.dir {
		return 0, fs.ErrInvalid
	}
	return f.reader.Read(data)
}
func (f *memoryFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if f.closed {
		return nil, fs.ErrClosed
	}
	if !f.node.dir {
		return nil, fs.ErrInvalid
	}
	if n > 0 && f.offset == len(f.entries) {
		return nil, io.EOF
	}
	end := len(f.entries)
	if n > 0 && n < end-f.offset {
		end = f.offset + n
	}
	result := append([]fs.DirEntry{}, f.entries[f.offset:end]...)
	f.offset = end
	return result, nil
}

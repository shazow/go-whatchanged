package vfs

import (
	"io"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// GitFS serves a git tree as a read-only FS. It only ever touches the object
// store; the repository's worktree and index are never consulted.
type GitFS struct {
	root *object.Tree
}

// NewGitFS wraps a tree.
func NewGitFS(tree *object.Tree) *GitFS {
	return &GitFS{root: tree}
}

// GitMountPath returns the synthetic path a tree is mounted at.
func GitMountPath(tree *object.Tree) string {
	return SyntheticPrefix + "git/" + tree.Hash.String()
}

func normalize(name string) string {
	name = path.Clean(strings.TrimPrefix(name, "/"))
	if name == "." {
		return ""
	}
	return name
}

// Stat implements FS.
func (g *GitFS) Stat(name string) (fs.FileInfo, error) {
	rel := normalize(name)
	if rel == "" {
		return gitInfo{name: ".", mode: filemode.Dir}, nil
	}
	entry, err := g.root.FindEntry(rel)
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return gitInfo{name: path.Base(rel), mode: entry.Mode, tree: g.root, path: rel}, nil
}

// ReadDir implements FS.
func (g *GitFS) ReadDir(name string) ([]fs.FileInfo, error) {
	rel := normalize(name)
	tree := g.root
	if rel != "" {
		var err error
		tree, err = g.root.Tree(rel)
		if err != nil {
			if err == object.ErrDirectoryNotFound {
				if _, ferr := g.root.FindEntry(rel); ferr == nil {
					return nil, &fs.PathError{Op: "readdir", Path: name, Err: ErrNotDir}
				}
			}
			return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
		}
	}
	infos := make([]fs.FileInfo, 0, len(tree.Entries))
	for _, e := range tree.Entries {
		infos = append(infos, gitInfo{name: e.Name, mode: e.Mode, tree: tree, path: e.Name})
	}
	return infos, nil
}

// Open implements FS.
func (g *GitFS) Open(name string) (io.ReadCloser, error) {
	rel := normalize(name)
	f, err := g.root.File(rel)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return f.Reader()
}

// gitInfo adapts a tree entry to fs.FileInfo. The size is looked up on
// demand: it costs an object-store read per file, and go/build never asks.
type gitInfo struct {
	name string
	mode filemode.FileMode
	tree *object.Tree // holds the entry at path; nil for the root itself
	path string
}

func (i gitInfo) Name() string { return i.name }

func (i gitInfo) Size() int64 {
	if i.tree == nil || !i.mode.IsFile() {
		return 0
	}
	size, _ := i.tree.Size(i.path)
	return size
}

func (i gitInfo) Mode() fs.FileMode {
	m, err := i.mode.ToOSFileMode()
	if err != nil {
		return 0
	}
	return m
}
func (i gitInfo) ModTime() time.Time { return time.Time{} }
func (i gitInfo) IsDir() bool        { return i.mode == filemode.Dir }
func (i gitInfo) Sys() any           { return nil }

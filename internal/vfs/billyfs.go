package vfs

import (
	"io"
	"io/fs"
	"os"

	"github.com/go-git/go-billy/v5"
)

// BillyFS adapts a billy.Filesystem to FS. It is used so tests can point the
// working-tree side at an in-memory filesystem.
type BillyFS struct {
	fs billy.Filesystem
}

// NewBillyFS wraps a billy filesystem.
func NewBillyFS(b billy.Filesystem) *BillyFS {
	return &BillyFS{fs: b}
}

// Stat implements FS.
func (b *BillyFS) Stat(name string) (fs.FileInfo, error) {
	return b.fs.Stat(name)
}

// ReadDir implements FS.
func (b *BillyFS) ReadDir(name string) ([]fs.FileInfo, error) {
	fi, err := b.fs.Stat(name)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: ErrNotDir}
	}
	infos, err := b.fs.ReadDir(name)
	if err != nil {
		return nil, err
	}
	out := make([]fs.FileInfo, len(infos))
	for i, fi := range infos {
		out[i] = fi
	}
	return out, nil
}

// Open implements FS.
func (b *BillyFS) Open(name string) (io.ReadCloser, error) {
	f, err := b.fs.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	return f, nil
}

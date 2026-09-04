// Package vfs builds read-only go/build contexts whose filesystem hooks are
// served from a git tree, any FS implementation, or the real disk.
//
// A Context never writes anywhere: every hook is a pure read. Paths under a
// mount are answered by the mounted FS; every other path falls through to the
// operating system so that GOROOT and the module cache remain reachable.
package vfs

import (
	"errors"
	"go/build"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// FS is the minimal read-only filesystem surface go/build needs.
type FS interface {
	// Stat returns file info for the file at name. Directories report IsDir.
	Stat(name string) (fs.FileInfo, error)
	// ReadDir lists the directory at name.
	ReadDir(name string) ([]fs.FileInfo, error)
	// Open opens the file at name for reading.
	Open(name string) (io.ReadCloser, error)
}

// Mount attaches an FS at an absolute path prefix.
type Mount struct {
	Path string // absolute, slash-separated path prefix (no trailing slash)
	FS   FS     // serves paths relative to Path; "" or "." is its root
}

// Overlay routes paths through a list of mounts and falls back to the host
// operating system for everything else. Mounts may be added while the
// overlay is in use; see Add.
type Overlay struct {
	mu     sync.RWMutex
	mounts []Mount
}

// NewOverlay returns an Overlay with the given mounts. Longer mount paths are
// matched first.
func NewOverlay(mounts ...Mount) *Overlay {
	ms := make([]Mount, 0, len(mounts))
	for _, m := range mounts {
		m.Path = cleanAbs(m.Path)
		ms = append(ms, m)
	}
	slices.SortStableFunc(ms, func(a, b Mount) int { return len(b.Path) - len(a.Path) })
	return &Overlay{mounts: ms}
}

// Add attaches another mount, for a tree that becomes available after the
// overlay was built, such as a module fetched on demand. A mount at a path
// already mounted is ignored. Add is safe to call concurrently with the
// read methods.
func (o *Overlay) Add(m Mount) {
	m.Path = cleanAbs(m.Path)
	o.mu.Lock()
	defer o.mu.Unlock()
	if slices.ContainsFunc(o.mounts, func(e Mount) bool { return e.Path == m.Path }) {
		return
	}
	o.mounts = append(o.mounts, m)
	slices.SortStableFunc(o.mounts, func(a, b Mount) int { return len(b.Path) - len(a.Path) })
}

// cleanAbs normalizes p to a slash-separated cleaned path.
func cleanAbs(p string) string {
	return path.Clean(filepath.ToSlash(p))
}

// lookup returns the mount serving name and the path relative to that mount.
func (o *Overlay) lookup(name string) (FS, string, bool) {
	clean := cleanAbs(name)
	o.mu.RLock()
	defer o.mu.RUnlock()
	for _, m := range o.mounts {
		if clean == m.Path {
			return m.FS, ".", true
		}
		if strings.HasPrefix(clean, m.Path+"/") {
			return m.FS, clean[len(m.Path)+1:], true
		}
	}
	return nil, "", false
}

// Stat implements FS.
func (o *Overlay) Stat(name string) (fs.FileInfo, error) {
	if f, rel, ok := o.lookup(name); ok {
		return f.Stat(rel)
	}
	return os.Stat(name)
}

// ReadDir implements FS.
func (o *Overlay) ReadDir(name string) ([]fs.FileInfo, error) {
	if f, rel, ok := o.lookup(name); ok {
		return f.ReadDir(rel)
	}
	entries, err := os.ReadDir(name)
	if err != nil {
		return nil, err
	}
	infos := make([]fs.FileInfo, 0, len(entries))
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			// The entry vanished between listing and stat; skip it.
			continue
		}
		infos = append(infos, fi)
	}
	return infos, nil
}

// Open implements FS.
func (o *Overlay) Open(name string) (io.ReadCloser, error) {
	if f, rel, ok := o.lookup(name); ok {
		return f.Open(rel)
	}
	return os.Open(name)
}

// ReadFile reads the whole file at name through the overlay.
func (o *Overlay) ReadFile(name string) ([]byte, error) {
	rc, err := o.Open(name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// IsDir reports whether name is a directory.
func (o *Overlay) IsDir(name string) bool {
	fi, err := o.Stat(name)
	return err == nil && fi.IsDir()
}

// Context returns a copy of build.Default whose filesystem hooks are served
// by the overlay. Cgo is disabled because the cgo path shells out and writes
// temporary files. GOPATH is cleared so nothing is ever looked up there.
func Context(o *Overlay, goos, goarch string) build.Context {
	ctxt := build.Default
	if goos != "" {
		ctxt.GOOS = goos
	}
	if goarch != "" {
		ctxt.GOARCH = goarch
	}
	ctxt.CgoEnabled = false
	ctxt.GOPATH = ""
	ctxt.Dir = ""
	ctxt.IsDir = o.IsDir
	ctxt.ReadDir = o.ReadDir
	ctxt.OpenFile = o.Open
	ctxt.HasSubdir = hasSubdir
	ctxt.JoinPath = joinPath
	ctxt.IsAbsPath = isAbsPath
	return ctxt
}

// hasSubdir is a purely lexical replacement for build.Context.HasSubdir; the
// default implementation evaluates symlinks, which is meaningless for
// synthetic mount paths.
func hasSubdir(root, dir string) (rel string, ok bool) {
	root = cleanAbs(root)
	dir = cleanAbs(dir)
	if root == dir {
		return "", true
	}
	if !strings.HasSuffix(root, "/") {
		root += "/"
	}
	after, found := strings.CutPrefix(dir, root)
	if !found {
		return "", false
	}
	return after, true
}

// joinPath joins path elements. Synthetic mount paths are always
// slash-separated; real paths are joined with the host separator.
func joinPath(elem ...string) string {
	if len(elem) > 0 && strings.HasPrefix(elem[0], SyntheticPrefix) {
		return path.Join(elem...)
	}
	return filepath.Join(elem...)
}

func isAbsPath(p string) bool {
	return strings.HasPrefix(p, SyntheticPrefix) || filepath.IsAbs(p)
}

// SyntheticPrefix is the prefix of every synthetic mount path created by this
// package. It cannot collide with a real path on any supported platform.
const SyntheticPrefix = "/@"

// ErrNotDir is returned when a directory operation targets a file.
var ErrNotDir = errors.New("not a directory")

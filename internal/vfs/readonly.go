package vfs

import (
	"errors"
	"io/fs"
	"os"

	"github.com/go-git/go-billy/v5"
)

// ErrReadOnly is the error of every write attempted through a ReadOnly
// filesystem.
var ErrReadOnly = errors.New("read-only filesystem")

// ReadOnly wraps a billy filesystem so that nothing can be written through
// it. Every method that would create, change or remove something fails with
// ErrReadOnly, OpenFile refuses any flag but O_RDONLY, the files it opens
// refuse to write or truncate, and the filesystem advertises no write
// capability, so that go-git takes its read-only paths where it has them.
// Locking is allowed: go-git takes an advisory lock on packed-refs even to
// read it, and a lock changes nothing on disk.
//
// go-whatchanged opens every repository through it (see OpenRepository), so
// that the promise never to touch the repository rests on this type rather
// than on go-git's behaviour.
func ReadOnly(fsys billy.Filesystem) billy.Filesystem {
	if _, ok := fsys.(*readOnlyFS); ok {
		return fsys
	}
	return &readOnlyFS{fsys}
}

type readOnlyFS struct {
	fs billy.Filesystem
}

// denied is the error for a refused write.
func denied(op, name string) error {
	return &fs.PathError{Op: op, Path: name, Err: ErrReadOnly}
}

// Create implements billy.Basic.
func (r *readOnlyFS) Create(name string) (billy.File, error) {
	return nil, denied("create", name)
}

// Open implements billy.Basic.
func (r *readOnlyFS) Open(name string) (billy.File, error) {
	f, err := r.fs.Open(name)
	if err != nil {
		return nil, err
	}
	return readOnlyFile{f}, nil
}

// OpenFile implements billy.Basic, refusing every flag that could write.
func (r *readOnlyFS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0 {
		return nil, denied("open", name)
	}
	f, err := r.fs.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return readOnlyFile{f}, nil
}

// Stat implements billy.Basic.
func (r *readOnlyFS) Stat(name string) (os.FileInfo, error) { return r.fs.Stat(name) }

// Rename implements billy.Basic.
func (r *readOnlyFS) Rename(oldpath, newpath string) error { return denied("rename", oldpath) }

// Remove implements billy.Basic.
func (r *readOnlyFS) Remove(name string) error { return denied("remove", name) }

// Join implements billy.Basic.
func (r *readOnlyFS) Join(elem ...string) string { return r.fs.Join(elem...) }

// TempFile implements billy.TempFile.
func (r *readOnlyFS) TempFile(dir, prefix string) (billy.File, error) {
	return nil, denied("tempfile", r.fs.Join(dir, prefix+"*"))
}

// ReadDir implements billy.Dir.
func (r *readOnlyFS) ReadDir(name string) ([]os.FileInfo, error) { return r.fs.ReadDir(name) }

// MkdirAll implements billy.Dir.
func (r *readOnlyFS) MkdirAll(name string, perm os.FileMode) error { return denied("mkdir", name) }

// Lstat implements billy.Symlink.
func (r *readOnlyFS) Lstat(name string) (os.FileInfo, error) { return r.fs.Lstat(name) }

// Symlink implements billy.Symlink.
func (r *readOnlyFS) Symlink(target, link string) error { return denied("symlink", link) }

// Readlink implements billy.Symlink.
func (r *readOnlyFS) Readlink(link string) (string, error) { return r.fs.Readlink(link) }

// Chroot implements billy.Chroot; the new root is read-only too.
func (r *readOnlyFS) Chroot(name string) (billy.Filesystem, error) {
	sub, err := r.fs.Chroot(name)
	if err != nil {
		return nil, err
	}
	return &readOnlyFS{sub}, nil
}

// Root implements billy.Chroot.
func (r *readOnlyFS) Root() string { return r.fs.Root() }

// Capabilities implements billy.Capable: reading, seeking and locking, and
// nothing that writes.
func (r *readOnlyFS) Capabilities() billy.Capability {
	return billy.ReadCapability | billy.SeekCapability | billy.LockCapability
}

// readOnlyFile is an open file that refuses to write.
type readOnlyFile struct {
	billy.File
}

func (f readOnlyFile) Write(p []byte) (int, error) { return 0, denied("write", f.Name()) }
func (f readOnlyFile) Truncate(size int64) error   { return denied("truncate", f.Name()) }

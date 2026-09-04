package vfs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

func TestReadOnly(t *testing.T) {
	inner := memfs.New()
	if err := inner.MkdirAll("dir", 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := inner.Create("dir/file")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("content")); err != nil {
		t.Fatal(err)
	}
	f.Close()

	fsys := ReadOnly(inner)
	if billy.CapabilityCheck(fsys, billy.WriteCapability) || billy.CapabilityCheck(fsys, billy.ReadAndWriteCapability) || billy.CapabilityCheck(fsys, billy.TruncateCapability) {
		t.Errorf("Capabilities() = %b advertises writing", billy.Capabilities(fsys))
	}
	if !billy.CapabilityCheck(fsys, billy.ReadCapability|billy.SeekCapability|billy.LockCapability) {
		t.Errorf("Capabilities() = %b lacks reading, seeking or locking", billy.Capabilities(fsys))
	}

	// Reads work, and so does the advisory lock go-git takes to read
	// packed-refs.
	rf, err := fsys.Open("dir/file")
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(rf); err != nil || string(b) != "content" {
		t.Errorf("read %q, %v", b, err)
	}
	if err := rf.Lock(); err != nil {
		t.Errorf("Lock: %v", err)
	}
	if err := rf.Unlock(); err != nil {
		t.Errorf("Unlock: %v", err)
	}
	if _, err := rf.Write([]byte("x")); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Write on an open file: %v, want ErrReadOnly", err)
	}
	if err := rf.Truncate(0); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Truncate on an open file: %v, want ErrReadOnly", err)
	}
	rf.Close()
	if fi, err := fsys.Stat("dir/file"); err != nil || fi.Size() != 7 {
		t.Errorf("Stat = %v, %v", fi, err)
	}
	if entries, err := fsys.ReadDir("dir"); err != nil || len(entries) != 1 {
		t.Errorf("ReadDir = %v, %v", entries, err)
	}
	if rf, err := fsys.OpenFile("dir/file", os.O_RDONLY, 0); err != nil {
		t.Errorf("OpenFile(O_RDONLY): %v", err)
	} else {
		rf.Close()
	}

	// Every write is refused, with an error that names the operation.
	writes := map[string]func() error{
		"Create":             func() error { _, err := fsys.Create("new"); return err },
		"OpenFile(O_WRONLY)": func() error { _, err := fsys.OpenFile("dir/file", os.O_WRONLY, 0); return err },
		"OpenFile(O_RDWR)":   func() error { _, err := fsys.OpenFile("dir/file", os.O_RDWR, 0); return err },
		"OpenFile(O_CREATE)": func() error { _, err := fsys.OpenFile("new", os.O_RDONLY|os.O_CREATE, 0o644); return err },
		"OpenFile(O_TRUNC)":  func() error { _, err := fsys.OpenFile("dir/file", os.O_RDONLY|os.O_TRUNC, 0); return err },
		"OpenFile(O_APPEND)": func() error { _, err := fsys.OpenFile("dir/file", os.O_RDONLY|os.O_APPEND, 0); return err },
		"Rename":             func() error { return fsys.Rename("dir/file", "dir/other") },
		"Remove":             func() error { return fsys.Remove("dir/file") },
		"MkdirAll":           func() error { return fsys.MkdirAll("dir/sub", 0o755) },
		"Symlink":            func() error { return fsys.Symlink("dir/file", "link") },
		"TempFile":           func() error { _, err := fsys.TempFile("", "tmp"); return err },
	}
	for name, write := range writes {
		if err := write(); !errors.Is(err, ErrReadOnly) {
			t.Errorf("%s: %v, want ErrReadOnly", name, err)
		}
	}
	if fi, err := inner.Stat("dir/file"); err != nil || fi.Size() != 7 {
		t.Errorf("after the refused writes, Stat = %v, %v", fi, err)
	}
	if _, err := inner.Stat("new"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after the refused writes, new: %v, want not exist", err)
	}

	// A chroot is read-only too.
	sub, err := fsys.Chroot("dir")
	if err != nil {
		t.Fatal(err)
	}
	if sub.Root() != inner.Join(inner.Root(), "dir") {
		t.Errorf("Chroot root = %q", sub.Root())
	}
	if _, err := sub.Create("new"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Create in a chroot: %v, want ErrReadOnly", err)
	}
	if _, err := sub.Stat("file"); err != nil {
		t.Errorf("Stat in a chroot: %v", err)
	}
}

// commitFile makes one commit adding name with content to a repository
// opened for writing.
func commitFile(t *testing.T, repo *git.Repository, name, content string) plumbing.Hash {
	t.Helper()
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	f, err := wt.Filesystem.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := wt.Add(name); err != nil {
		t.Fatal(err)
	}
	h, err := wt.Commit("add "+name, &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestOpenRepository(t *testing.T) {
	dir := t.TempDir()
	writable, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	base := commitFile(t, writable, "a.txt", "a\n")
	head := commitFile(t, writable, "b.txt", "b\n")
	// Packed refs are read through the lock go-git takes on the file, so
	// the branch must live there and not in a loose ref.
	if err := writable.Storer.(*filesystem.Storage).PackRefs(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "packed-refs")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "refs", "heads", "master")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("loose master ref after PackRefs: %v", err)
	}

	repo, err := OpenRepository(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Reading works, packed refs included.
	ref, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if ref.Hash() != head {
		t.Errorf("HEAD = %s, want %s", ref.Hash(), head)
	}
	commit, err := repo.CommitObject(base)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatal(err)
	}
	if f, err := tree.File("a.txt"); err != nil {
		t.Errorf("a.txt at base: %v", err)
	} else if s, _ := f.Contents(); s != "a\n" {
		t.Errorf("a.txt at base = %q", s)
	}

	// Writing is refused by construction, and there is no worktree to
	// touch.
	err = repo.Storer.SetReference(plumbing.NewHashReference("refs/heads/scratch", base))
	if !errors.Is(err, ErrReadOnly) {
		t.Errorf("SetReference: %v, want ErrReadOnly", err)
	}
	if _, err := repo.Worktree(); !errors.Is(err, git.ErrIsBareRepository) {
		t.Errorf("Worktree: %v, want ErrIsBareRepository", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "refs", "heads", "scratch")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("refs/heads/scratch: %v, want not exist", err)
	}

	// A directory without a repository.
	if _, err := OpenRepository(t.TempDir()); !errors.Is(err, git.ErrRepositoryNotExists) {
		t.Errorf("OpenRepository(empty): %v, want ErrRepositoryNotExists", err)
	}

	// A linked worktree, laid out as git worktree add does: a .git file
	// naming the per-worktree directory, which names the main repository
	// in commondir.
	linked := filepath.Join(t.TempDir(), "linked")
	admin := filepath.Join(dir, ".git", "worktrees", "linked")
	for _, d := range []string{admin, linked} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{
		filepath.Join(admin, "HEAD"):      base.String() + "\n",
		filepath.Join(admin, "commondir"): "../..\n",
		filepath.Join(admin, "gitdir"):    filepath.Join(linked, ".git") + "\n",
		filepath.Join(linked, ".git"):     "gitdir: " + admin + "\n",
	} {
		if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	repo, err = OpenRepository(linked)
	if err != nil {
		t.Fatal(err)
	}
	if ref, err := repo.Head(); err != nil || ref.Hash() != base {
		t.Errorf("linked HEAD = %v, %v; want %s", ref, err, base)
	}
	if _, err := repo.CommitObject(head); err != nil {
		t.Errorf("shared object store: %v", err)
	}
	err = repo.Storer.SetReference(plumbing.NewHashReference("refs/heads/scratch", base))
	if !errors.Is(err, ErrReadOnly) {
		t.Errorf("SetReference through a linked worktree: %v, want ErrReadOnly", err)
	}
}

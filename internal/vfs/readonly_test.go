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
	"github.com/go-git/go-billy/v5/util"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// TestReadOnly checks that nothing gets through the wrapper: every write
// fails with ErrReadOnly and leaves the filesystem as it was, while reads,
// and the lock go-git takes to read packed-refs, work.
func TestReadOnly(t *testing.T) {
	inner := memfs.New()
	if err := util.WriteFile(inner, "dir/file", []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	fsys := ReadOnly(inner)
	if billy.CapabilityCheck(fsys, billy.WriteCapability) || billy.CapabilityCheck(fsys, billy.ReadAndWriteCapability) {
		t.Errorf("Capabilities() = %b advertises writing", billy.Capabilities(fsys))
	}

	f, err := fsys.OpenFile("dir/file", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(f); err != nil || string(b) != "content" {
		t.Errorf("read %q, %v", b, err)
	}
	if err := f.Lock(); err != nil {
		t.Errorf("Lock: %v", err)
	}
	if err := f.Unlock(); err != nil {
		t.Errorf("Unlock: %v", err)
	}
	sub, err := fsys.Chroot("dir")
	if err != nil {
		t.Fatal(err)
	}

	writes := map[string]func() error{
		"Write":              func() error { _, err := f.Write([]byte("x")); return err },
		"Truncate":           func() error { return f.Truncate(0) },
		"Create":             func() error { _, err := fsys.Create("new"); return err },
		"Create in a chroot": func() error { _, err := sub.Create("new"); return err },
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
	if fi, err := inner.Stat("dir/file"); err != nil || fi.Size() != int64(len("content")) {
		t.Errorf("after the refused writes, dir/file: %v, %v", fi, err)
	}
	for _, name := range []string{"new", "dir/new", "dir/other", "dir/sub", "link"} {
		if _, err := inner.Stat(name); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("after the refused writes, %s: %v, want not exist", name, err)
		}
	}
}

// TestOpenRepository checks that a repository, and a linked worktree of
// it, can be read but not written: refs and objects come back, and a write
// fails with ErrReadOnly and leaves .git untouched.
func TestOpenRepository(t *testing.T) {
	dir := t.TempDir()
	writable, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	base := commitFile(t, writable, "a.txt", "a\n")
	head := commitFile(t, writable, "b.txt", "b\n")
	// go-git reads packed refs under a lock on the file, the one thing
	// beyond plain reads that the wrapper must let through.
	if err := writable.Storer.(*filesystem.Storage).PackRefs(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "packed-refs")); err != nil {
		t.Fatal(err)
	}
	linked := linkedWorktree(t, dir, base)

	for name, tc := range map[string]struct {
		dir  string
		head plumbing.Hash
	}{
		"repository":      {dir, head},
		"linked worktree": {linked, base},
	} {
		repo, err := OpenRepository(tc.dir)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if ref, err := repo.Head(); err != nil || ref.Hash() != tc.head {
			t.Errorf("%s: HEAD = %v, %v; want %s", name, ref, err, tc.head)
		}
		if _, err := repo.CommitObject(base); err != nil {
			t.Errorf("%s: object store: %v", name, err)
		}
		if _, err := repo.Worktree(); !errors.Is(err, git.ErrIsBareRepository) {
			t.Errorf("%s: Worktree: %v, want ErrIsBareRepository", name, err)
		}
		err = repo.Storer.SetReference(plumbing.NewHashReference("refs/heads/scratch", base))
		if !errors.Is(err, ErrReadOnly) {
			t.Errorf("%s: SetReference: %v, want ErrReadOnly", name, err)
		}
		if _, err := os.Stat(filepath.Join(dir, ".git", "refs", "heads", "scratch")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: refs/heads/scratch on disk: %v, want not exist", name, err)
		}
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
	if err := util.WriteFile(wt.Filesystem, name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
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

// linkedWorktree lays out what "git worktree add" produces for the
// repository in main, at commit head: a .git file naming the per-worktree
// directory, which names the main repository's .git in commondir. It
// returns the worktree directory.
func linkedWorktree(t *testing.T, main string, head plumbing.Hash) string {
	t.Helper()
	linked := filepath.Join(t.TempDir(), "linked")
	admin := filepath.Join(main, ".git", "worktrees", "linked")
	for _, d := range []string{admin, linked} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{
		filepath.Join(admin, "HEAD"):      head.String() + "\n",
		filepath.Join(admin, "commondir"): "../..\n",
		filepath.Join(admin, "gitdir"):    filepath.Join(linked, ".git") + "\n",
		filepath.Join(linked, ".git"):     "gitdir: " + admin + "\n",
	} {
		if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return linked
}

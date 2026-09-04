package vfs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/filesystem/dotgit"
)

// OpenRepository opens the git repository whose .git is in dir, the way
// git.PlainOpen does, except that go-git can only ever read it: the object
// store, refs and configuration are served through a ReadOnly filesystem,
// and the repository has no worktree at all, so no operation on the
// returned repository can write to the checkout, the index or .git. A
// linked worktree (git worktree add) opens too: its .git file names the
// per-worktree directory, whose commondir names the main repository's.
func OpenRepository(dir string) (*git.Repository, error) {
	root := ReadOnly(osfs.New(dir))
	dot, err := dotGit(root, dir)
	if err != nil {
		return nil, err
	}
	common, err := commonDir(dot)
	if err != nil {
		return nil, err
	}
	var repoFS billy.Filesystem = dot
	if common != nil {
		// The composite has no idea of capabilities of its own, so it is
		// wrapped once more to keep go-git on its read-only paths.
		repoFS = ReadOnly(dotgit.NewRepositoryFilesystem(dot, common))
	}
	return git.Open(filesystem.NewStorage(repoFS, cache.NewObjectLRUDefault()), nil)
}

// dotGit returns the repository's .git directory within root, following a
// .git file ("gitdir: path") to the directory it names.
func dotGit(root billy.Filesystem, dir string) (billy.Filesystem, error) {
	fi, err := root.Stat(git.GitDirName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, git.ErrRepositoryNotExists
		}
		return nil, err
	}
	if fi.IsDir() {
		return root.Chroot(git.GitDirName)
	}
	line, err := readLine(root, git.GitDirName)
	if err != nil {
		return nil, err
	}
	const prefix = "gitdir: "
	if !strings.HasPrefix(line, prefix) {
		return nil, fmt.Errorf(".git file has no %s prefix", prefix)
	}
	gitdir := strings.TrimSpace(line[len(prefix):])
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	return ReadOnly(osfs.New(gitdir)), nil
}

// commonDir returns the main repository's .git directory that a linked
// worktree's commondir file names, or nil for a repository of its own.
func commonDir(dot billy.Filesystem) (billy.Filesystem, error) {
	line, err := readLine(dot, "commondir")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	path := strings.TrimSpace(line)
	if path == "" {
		return nil, nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dot.Root(), path)
	}
	common := ReadOnly(osfs.New(path))
	if _, err := common.Stat(""); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, git.ErrRepositoryIncomplete
		}
		return nil, err
	}
	return common, nil
}

// readLine returns the first line of the named file.
func readLine(fsys billy.Filesystem, name string) (string, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return line, nil
}

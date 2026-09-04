package whatchanged

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	"github.com/shazow/go-whatchanged/internal/modfetch"
	"github.com/shazow/go-whatchanged/internal/modres"
	"github.com/shazow/go-whatchanged/internal/render"
	"github.com/shazow/go-whatchanged/internal/vfs"
)

var update = flag.Bool("update", false, "rewrite golden files")

// fixture is an in-memory git repository whose worktree is a memfs. Nothing
// in these tests touches the disk.
type fixture struct {
	t    *testing.T
	repo *git.Repository
	fs   billy.Filesystem
	env  modres.Env
	// modcache, when set, is an in-memory module cache mounted at
	// env.GOMODCACHE on both sides; see useFakeModcache.
	modcache billy.Filesystem
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	fsys := memfs.New()
	repo, err := git.Init(memory.NewStorage(), fsys)
	if err != nil {
		t.Fatal(err)
	}
	env, err := modres.DefaultEnv()
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, repo: repo, fs: fsys, env: env}
	f.write("go.mod", "module example.com/m\n\ngo 1.24\n")
	return f
}

func (f *fixture) write(name, content string) {
	f.t.Helper()
	writeFile(f.t, f.fs, name, content)
}

// writeFile creates name in fsys with content, and its directory if needed.
func writeFile(t *testing.T, fsys billy.Filesystem, name, content string) {
	t.Helper()
	if dir := path.Dir(name); dir != "." {
		if err := fsys.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	file, err := fsys.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// billyFS adapts a billy.Filesystem to vfs.FS, so that a test can point the
// working-tree side at an in-memory filesystem.
type billyFS struct{ fs billy.Filesystem }

func (b *billyFS) Stat(name string) (fs.FileInfo, error) { return b.fs.Stat(name) }

func (b *billyFS) ReadDir(name string) ([]fs.FileInfo, error) {
	fi, err := b.fs.Stat(name)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: vfs.ErrNotDir}
	}
	return b.fs.ReadDir(name)
}

func (b *billyFS) Open(name string) (io.ReadCloser, error) {
	return b.fs.OpenFile(name, os.O_RDONLY, 0)
}

// testSignature is the author and committer of every fixture commit.
func testSignature() *object.Signature {
	return &object.Signature{Name: "test", Email: "test@example.com", When: time.Unix(0, 0)}
}

// useFakeModcache replaces the real module cache with an empty in-memory
// one, populated with writeModule, so tests can define dependencies.
func (f *fixture) useFakeModcache() {
	f.modcache = memfs.New()
	f.env.GOMODCACHE = vfs.SyntheticPrefix + "modcache"
}

// writeModule adds a module version to the fake module cache. files are
// relative to the module root; a go.mod is added unless files has one.
func (f *fixture) writeModule(modPath, version string, files map[string]string) {
	f.t.Helper()
	root := modPath + "@" + version
	if _, ok := files["go.mod"]; !ok {
		files["go.mod"] = "module " + modPath + "\n\ngo 1.24\n"
	}
	for name, content := range files {
		writeFile(f.t, f.modcache, path.Join(root, name), content)
	}
}

func (f *fixture) remove(name string) {
	f.t.Helper()
	if err := f.fs.Remove(name); err != nil {
		f.t.Fatal(err)
	}
}

// commit stages everything and commits. Worktree use is confined to the
// in-memory fixture; the tool itself never touches a worktree.
func (f *fixture) commit(msg string) plumbing.Hash {
	f.t.Helper()
	wt, err := f.repo.Worktree()
	if err != nil {
		f.t.Fatal(err)
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		f.t.Fatal(err)
	}
	sig := testSignature()
	h, err := wt.Commit(msg, &git.CommitOptions{Author: sig, Committer: sig, AllowEmptyCommits: true})
	if err != nil {
		f.t.Fatal(err)
	}
	return h
}

func (f *fixture) tag(name string, h plumbing.Hash) {
	f.t.Helper()
	if _, err := f.repo.CreateTag(name, h, nil); err != nil {
		f.t.Fatal(err)
	}
}

// annotatedTag creates a tag object pointing at h.
func (f *fixture) annotatedTag(name string, h plumbing.Hash) {
	f.t.Helper()
	if _, err := f.repo.CreateTag(name, h, &git.CreateTagOptions{Tagger: testSignature(), Message: name}); err != nil {
		f.t.Fatal(err)
	}
}

// checkout switches the in-memory worktree to branch, creating it at h when
// create is set. The fixture's files are replaced by the branch's tree.
func (f *fixture) checkout(branch string, create bool, h plumbing.Hash) {
	f.t.Helper()
	wt, err := f.repo.Worktree()
	if err != nil {
		f.t.Fatal(err)
	}
	opts := &git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName(branch), Force: true}
	if create {
		opts.Create = true
		opts.Hash = h
	}
	if err := wt.Checkout(opts); err != nil {
		f.t.Fatal(err)
	}
}

// open hands out the in-memory repository. Memory storage is only read
// during a run, so sharing one handle between the sides is safe here.
func (f *fixture) open() (*git.Repository, error) { return f.repo, nil }

type runResult struct {
	stdout, stderr string
	code           int
	err            error
	res            *render.Result
}

// run diffs base against the fixture's in-memory worktree (or head, if set).
// An empty base takes the same default as Run.
func (f *fixture) run(base, head string, opts Options) runResult {
	f.t.Helper()
	var out, errb bytes.Buffer
	opts.Stdout = &out
	opts.Stderr = &errb
	opts.Base = base
	headSpec := sideSpec{fs: &billyFS{fs: f.fs}}
	if head != "" {
		headSpec = sideSpec{rev: head}
	}
	if base == "" {
		base = DefaultBase
	}
	return f.runSpecs(sideSpec{rev: base}, headSpec, opts)
}

// runSpecs diffs two sides given as specs, filling in the fixture's
// repository and module cache.
func (f *fixture) runSpecs(base, head sideSpec, opts Options) runResult {
	f.t.Helper()
	var out, errb bytes.Buffer
	opts.Stdout = &out
	opts.Stderr = &errb
	var mounts []vfs.Mount
	if f.modcache != nil {
		mounts = []vfs.Mount{{Path: f.env.GOMODCACHE, FS: &billyFS{fs: f.modcache}}}
	}
	for _, s := range []*sideSpec{&base, &head} {
		s.open = f.open
		s.mounts = mounts
	}
	res, err := compare(f.t.Context(), base, head, f.env, opts)
	if err != nil {
		return runResult{stderr: errb.String(), code: ExitError, err: err}
	}
	code, err := finish(res, opts)
	return runResult{stdout: out.String(), stderr: errb.String(), code: code, err: err, res: res}
}

// mustRun is run for a diff that is expected to succeed.
func (f *fixture) mustRun(base, head string, opts Options) runResult {
	f.t.Helper()
	r := f.run(base, head, opts)
	if r.err != nil {
		f.t.Fatal(r.err)
	}
	return r
}

func mustContain(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q:\n%s", w, got)
		}
	}
}

func mustNotContain(t *testing.T, got string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(got, u) {
			t.Errorf("output unexpectedly contains %q:\n%s", u, got)
		}
	}
}

func TestUnchanged(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.commit("base")
	r := f.mustRun("HEAD", "", Options{})
	if r.code != ExitClean {
		t.Errorf("exit = %d, want %d", r.code, ExitClean)
	}
	if r.stdout != "no exported API changes\n" {
		t.Errorf("stdout = %q", r.stdout)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q", r.stderr)
	}
}

func TestAddedAndRemovedPackage(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("old/old.go", "package old\n\nfunc Gone() {}\n\ntype T struct{}\n")
	f.commit("base")
	f.remove("old/old.go")
	f.write("fresh/fresh.go", "package fresh\n\nfunc Hello() {}\n")
	r := f.mustRun("HEAD", "", Options{})
	mustContain(t, r.stdout,
		"example.com/m/fresh (new)\n  + func Hello()\n",
		"example.com/m/old (removed)\n  - func Gone()\n  - type T struct{}\n",
		"2 packages changed · 2 incompatible · 1 compatible · would require: MAJOR\n")
	if r.code != ExitIncompatible {
		t.Errorf("exit = %d, want %d", r.code, ExitIncompatible)
	}
}

func TestRemovedFunc(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc Keep() {}\n\nfunc Drop() {}\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nfunc Keep() {}\n")
	r := f.mustRun("HEAD", "", Options{})
	mustContain(t, r.stdout, "example.com/m/a\n  - func Drop()\n", "would require: MAJOR")
	mustNotContain(t, r.stdout, "Keep")
	if r.code != ExitIncompatible {
		t.Errorf("exit = %d", r.code)
	}
}

func TestChangedSignature(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\ntype Client struct{}\n\nfunc (c *Client) Do(n int) {}\n\nfunc Open(name string) error { return nil }\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\ntype Client struct{}\n\nfunc (c *Client) Do(n int, tags ...string) {}\n\ntype Options struct{}\n\nfunc Open(name string, o Options) error { return nil }\n")
	r := f.mustRun("HEAD", "", Options{})
	mustContain(t, r.stdout,
		"  - func (c *Client) Do(n int)\n  + func (c *Client) Do(n int, tags ...string)\n",
		"  - func Open(name string) error\n  + func Open(name string, o Options) error\n",
		"  + type Options struct{}\n",
		"1 package changed · 2 incompatible · 1 compatible · would require: MAJOR\n")
}

func TestAddedStructFieldIsCompatible(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\ntype Point struct{ X, Y int }\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\ntype Point struct{ X, Y, Z int }\n")
	r := f.mustRun("HEAD", "", Options{})
	mustContain(t, r.stdout, "  + field Point.Z int\n", "would require: MINOR")
	if r.code != ExitClean {
		t.Errorf("exit = %d, want %d (compatible change)", r.code, ExitClean)
	}
}

func TestAddedInterfaceMethodIsIncompatibleAddition(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\ntype Sizer interface{ Len() int }\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\ntype Sizer interface {\n\tLen() int\n\tSize() int\n}\n")
	r := f.mustRun("HEAD", "", Options{})
	mustContain(t, r.stdout, "  + func (Sizer) Size() int\n", "would require: MAJOR")
	if r.code != ExitIncompatible {
		t.Errorf("exit = %d", r.code)
	}
}

func TestIgnoredDirectories(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.commit("base")
	f.write("internal/hidden/h.go", "package hidden\n\nfunc Hidden() {}\n")
	f.write("a/internal/deep/d.go", "package deep\n\nfunc Deep() {}\n")
	f.write("cmd/tool/main.go", "package main\n\nfunc Exported() {}\n\nfunc main() {}\n")
	f.write("nested/go.mod", "module example.com/m/nested\n\ngo 1.24\n")
	f.write("nested/n.go", "package nested\n\nfunc Nested() {}\n")
	f.write("testdata/t.go", "package testdata\n\nfunc Fixture() {}\n")
	f.write("vendor/v/v.go", "package v\n\nfunc Vendored() {}\n")
	f.write("_skip/s.go", "package skip\n\nfunc Skipped() {}\n")
	f.write(".hidden/h.go", "package hidden\n\nfunc Dot() {}\n")
	f.write("a/a_test.go", "package a\n\nfunc TestOnly() {}\n")
	r := f.mustRun("HEAD", "", Options{Filter: render.Public})
	if r.stdout != "no exported API changes; add --filter=all to include internal API changes\n" {
		t.Errorf("stdout = %q", r.stdout)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q", r.stderr)
	}
}

func TestCommittedDirectorySymlinkIsIgnored(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	symlinks, ok := f.fs.(billy.Symlink)
	if !ok {
		t.Fatal("fixture filesystem does not support symlinks")
	}
	if err := symlinks.Symlink("a", "alias"); err != nil {
		t.Fatal(err)
	}
	f.commit("base")

	r := f.mustRun("HEAD", "", Options{})
	if r.stdout != "no exported API changes\n" {
		t.Errorf("stdout = %q", r.stdout)
	}
}

func TestGOOSFiltering(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.commit("base")
	f.write("a/a_windows.go", "package a\n\nfunc WindowsOnly() {}\n")
	f.write("a/tagged.go", "//go:build plan9\n\npackage a\n\nfunc Plan9Only() {}\n")

	r := f.mustRun("HEAD", "", Options{GOOS: "linux", GOARCH: "amd64"})
	if r.stdout != "no exported API changes\n" {
		t.Errorf("linux stdout = %q", r.stdout)
	}

	r = f.mustRun("HEAD", "", Options{GOOS: "windows", GOARCH: "amd64"})
	mustContain(t, r.stdout, "  + func WindowsOnly()\n")
	mustNotContain(t, r.stdout, "Plan9Only")

	r = f.mustRun("HEAD", "", Options{GOOS: "plan9", GOARCH: "amd64"})
	mustContain(t, r.stdout, "  + func Plan9Only()\n")
	mustNotContain(t, r.stdout, "WindowsOnly")
}

func TestTypeErrorWarnsButDiffs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n\nvar Broken undefinedType\n")

	r := f.mustRun("HEAD", "", Options{})
	mustContain(t, r.stdout, "  + func B()\n", "  + var Broken invalid type\n")
	if want := "warn: example.com/m/a: a/a.go:7:12: undefined: undefinedType\n"; r.stderr != want {
		t.Errorf("stderr = %q, want %q", r.stderr, want)
	}
	if r.code != ExitClean {
		t.Errorf("exit = %d", r.code)
	}

	// JSON carries the same warning, split into its package and message,
	// and stderr still gets the line.
	r = f.mustRun("HEAD", "", Options{Format: render.JSON})
	var rep struct{ Warnings []render.Warning }
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatal(err)
	}
	if want := []render.Warning{{Package: "example.com/m/a", Message: "a/a.go:7:12: undefined: undefinedType"}}; !slices.Equal(rep.Warnings, want) {
		t.Errorf("json warnings = %+v, want %+v", rep.Warnings, want)
	}
	if want := "warn: example.com/m/a: a/a.go:7:12: undefined: undefinedType\n"; r.stderr != want {
		t.Errorf("json: stderr = %q, want %q", r.stderr, want)
	}

	r = f.run("HEAD", "", Options{Strict: true})
	if r.code != ExitError || r.err == nil {
		t.Errorf("strict: exit = %d, err = %v; want fatal", r.code, r.err)
	}
	if r.stdout != "" {
		t.Errorf("strict: stdout = %q, want none", r.stdout)
	}
	mustContain(t, r.stderr, "warn: example.com/m/a:")
}

func TestTypeErrorOnBaseSideNamesRevision(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nvar Broken undefinedType\n")
	h := f.commit("base")
	f.tag("v0.1.0", h)
	f.write("a/a.go", "package a\n\nvar Broken int\n")
	r := f.mustRun("v0.1.0", "", Options{})
	if want := "warn: example.com/m/a: v0.1.0:a/a.go:3:12: undefined: undefinedType\n"; r.stderr != want {
		t.Errorf("stderr = %q, want %q", r.stderr, want)
	}
	mustContain(t, r.stdout, "  - var Broken invalid type\n  + var Broken int\n")
}

func TestBreakingHidesCompatible(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc Drop() {}\n")
	f.write("b/b.go", "package b\n\nfunc B() {}\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nfunc Added() {}\n")
	f.write("b/b.go", "package b\n\nfunc B() {}\n\nfunc B2() {}\n")
	r := f.mustRun("HEAD", "", Options{Breaking: true})
	want := "example.com/m/a\n  - func Drop()\n\n2 packages changed · 1 incompatible · 2 compatible · would require: MAJOR\n"
	if r.stdout != want {
		t.Errorf("stdout = %q\nwant     %q", r.stdout, want)
	}
}

func TestBreakingWithOnlyCompatibleChanges(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc Added() {}\n")
	r := f.mustRun("HEAD", "", Options{Breaking: true})
	want := "1 package changed · 0 incompatible · 1 compatible · would require: MINOR\n"
	if r.stdout != want {
		t.Errorf("stdout = %q\nwant     %q", r.stdout, want)
	}
}

func TestHeadCommit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.commit("one")
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n")
	f.commit("two")
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n\nfunc Uncommitted() {}\n")

	r := f.mustRun("HEAD~1", "HEAD", Options{})
	mustContain(t, r.stdout, "  + func B()\n")
	mustNotContain(t, r.stdout, "Uncommitted")

	r = f.mustRun("HEAD~1", "", Options{})
	mustContain(t, r.stdout, "  + func B()\n", "  + func Uncommitted()\n")
}

// diskFixture creates a repository on disk with two commits and packs its
// objects, the layout of every clone. go-git indexes packfiles lazily, so
// only this fixture exercises the concurrent object reads a real diff of two
// revisions performs; the in-memory fixture never does. It returns the
// worktree directory and the two commit hashes: base declares func A, head
// adds func B.
func diskFixture(t *testing.T) (dir string, base, head plumbing.Hash) {
	t.Helper()
	dir = t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, repo: repo, fs: wt.Filesystem}
	f.write("go.mod", "module example.com/m\n\ngo 1.24\n")
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	base = f.commit("base")
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n")
	head = f.commit("head")

	if err := repo.RepackObjects(&git.RepackConfig{}); err != nil {
		t.Fatal(err)
	}
	// Repacking leaves the loose copies behind; drop them so every object
	// read goes through the packfile.
	objects := filepath.Join(dir, ".git", "objects")
	entries, err := os.ReadDir(objects)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() && len(e.Name()) == 2 {
			if err := os.RemoveAll(filepath.Join(objects, e.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir, base, head
}

func TestTwoRevisionsOnPackedRepository(t *testing.T) {
	t.Parallel()
	dir, base, head := diskFixture(t)
	// The race this guards against is timing dependent; a handful of runs
	// made it show up reliably before the fix.
	for i := range 10 {
		var out, errb bytes.Buffer
		code, err := Run(Options{
			Base:   dir + "@" + base.String()[:7], // abbreviated, as typed from git log
			Head:   "@" + head.String(),
			Stdout: &out,
			Stderr: &errb,
		})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if code != ExitClean {
			t.Errorf("run %d: exit = %d, stderr = %q", i, code, errb.String())
		}
		mustContain(t, out.String(), "example.com/m/a\n  + func B()\n")
	}
}

func TestLinkedWorktree(t *testing.T) {
	t.Parallel()
	main, base, head := diskFixture(t)

	// Lay out what "git worktree add <linked> <head>" produces: a .git file
	// pointing into the main repository, whose per-worktree directory holds
	// HEAD and points back at the shared object store via commondir.
	linked := filepath.Join(t.TempDir(), "linked")
	admin := filepath.Join(main, ".git", "worktrees", "linked")
	for _, d := range []string{admin, filepath.Join(linked, "a")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(admin, "HEAD"):       head.String() + "\n",
		filepath.Join(admin, "commondir"):  "../..\n",
		filepath.Join(admin, "gitdir"):     filepath.Join(linked, ".git") + "\n",
		filepath.Join(linked, ".git"):      "gitdir: " + admin + "\n",
		filepath.Join(linked, "go.mod"):    "module example.com/m\n\ngo 1.24\n",
		filepath.Join(linked, "a", "a.go"): "package a\n\nfunc A() {}\n\nfunc B() {}\n\nfunc Uncommitted() {}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var out, errb bytes.Buffer
	code, err := Run(Options{Base: linked + "@HEAD~1", Stdout: &out, Stderr: &errb})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != ExitClean {
		t.Errorf("exit = %d, stderr = %q", code, errb.String())
	}
	mustContain(t, out.String(), "  + func B()\n", "  + func Uncommitted()\n")

	out.Reset()
	code, err = Run(Options{Base: linked + "@" + base.String(), Head: "@HEAD", Stdout: &out, Stderr: &errb})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != ExitClean {
		t.Errorf("exit = %d, stderr = %q", code, errb.String())
	}
	mustContain(t, out.String(), "  + func B()\n")
	mustNotContain(t, out.String(), "Uncommitted")
}

func TestNewerGoDirectiveIsClamped(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("go.mod", "module example.com/m\n\ngo 1.99\n")
	f.write("a/a.go", "package a\n\nimport \"fmt\"\n\nfunc A() { fmt.Println() }\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nimport \"fmt\"\n\nfunc A() { fmt.Println() }\n\nfunc B() {}\n")

	r := f.mustRun("HEAD", "", Options{})
	mustContain(t, r.stdout, "  + func B()\n")
	// One module-level warning, deduplicated across the two sides, instead
	// of go/types' per-package "package requires newer Go version" error.
	if n := strings.Count(r.stderr, "\n"); n != 1 {
		t.Errorf("stderr has %d lines, want 1:\n%s", n, r.stderr)
	}
	mustContain(t, r.stderr, "warn: example.com/m: go.mod requires go 1.99 but go-whatchanged was built with go1.", "; type-checking as go1.")
	mustNotContain(t, r.stderr, "requires newer Go version")
	if r.code != ExitClean {
		t.Errorf("exit = %d", r.code)
	}

	r = f.run("HEAD", "", Options{Strict: true})
	if r.code != ExitError || r.err == nil {
		t.Errorf("strict: exit = %d, err = %v; want fatal", r.code, r.err)
	}
}

func TestDefaultBaseIsHeadVersusWorkingTree(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.commit("one")
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc Committed() {}\n")
	f.commit("two")
	f.write("a/a.go", "package a\n\nfunc Committed() {}\n\nfunc Uncommitted() {}\n")

	r := f.mustRun("", "", Options{})
	// Only the dirty state relative to HEAD shows up: the earlier commit's
	// addition is on both sides.
	mustContain(t, r.stdout, "  - func A()\n", "  + func Uncommitted()\n")
	mustNotContain(t, r.stdout, "func Committed()")
	if r.code != ExitIncompatible {
		t.Errorf("exit = %d, want %d", r.code, ExitIncompatible)
	}

	// Clean checkout: nothing changed.
	f.commit("three")
	r = f.mustRun("", "", Options{})
	if r.code != ExitClean {
		t.Errorf("exit = %d, want %d:\n%s", r.code, ExitClean, r.stdout)
	}
	mustNotContain(t, r.stdout, "Uncommitted", "removed")
}

func TestBadRevision(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n")
	f.commit("base")
	r := f.run("does-not-exist", "", Options{})
	if r.code != ExitError || r.err == nil {
		t.Fatalf("exit = %d, err = %v; want error", r.code, r.err)
	}
	mustContain(t, r.err.Error(), "@does-not-exist: no such tag, branch or commit")
}

func TestMissingGoMod(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.remove("go.mod")
	f.write("a/a.go", "package a\n")
	f.commit("base")
	r := f.run("HEAD", "", Options{})
	if r.code != ExitError || r.err == nil {
		t.Fatalf("exit = %d, err = %v; want error", r.code, r.err)
	}
	// Whichever side reports first names itself and says where it looked,
	// in the user's terms rather than at the synthetic mount path.
	if msg := r.err.Error(); msg != "@HEAD: no go.mod at this revision (GOPATH mode is not supported)" &&
		msg != "working tree: no go.mod in the working tree (GOPATH mode is not supported)" {
		t.Errorf("err = %q", msg)
	}
	mustNotContain(t, r.err.Error(), vfs.SyntheticPrefix)

	// A module in a subdirectory, at a revision where the directory did
	// not exist yet: the file is named relative to the repository.
	f.write("go.mod", "module example.com/m\n\ngo 1.24\n")
	f.write("sub/go.mod", "module example.com/m/sub\n\ngo 1.24\n")
	f.write("sub/sub.go", "package sub\n")
	_, err := compare(t.Context(), sideSpec{rev: "HEAD", open: f.open, rel: "sub"}, sideSpec{fs: &billyFS{fs: f.fs}, open: f.open, rel: "sub"}, f.env, Options{})
	if err == nil || err.Error() != "@HEAD: no sub/go.mod at this revision" {
		t.Errorf("err = %v", err)
	}
}

func TestUnresolvableImportIsFatal(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nimport \"example.org/nothere\"\n\nvar X = nothere.X\n")
	r := f.run("HEAD", "", Options{})
	if r.code != ExitError || r.err == nil {
		t.Fatalf("exit = %d, err = %v; want error", r.code, r.err)
	}
	mustContain(t, r.err.Error(), `unresolvable import "example.org/nothere" (required by example.com/m/a)`)

	// A module go.mod requires but the module cache lacks is fatal too when
	// there is nothing to fetch it with, and the error names the flag.
	f.write("go.mod", "module example.com/m\n\ngo 1.24\n\nrequire example.org/nothere v1.2.3\n")
	r = f.run("HEAD", "", Options{})
	if r.code != ExitError || r.err == nil {
		t.Fatalf("exit = %d, err = %v; want error", r.code, r.err)
	}
	mustContain(t, r.err.Error(),
		"working tree: unresolvable import \"example.org/nothere\" (required by example.com/m/a): module example.org/nothere@v1.2.3 not in module cache; remove --fsreadonly to let go-whatchanged download it")
	mustNotContain(t, r.err.Error(), "go.work")

	// In a workspace, the import of a sibling module resolves through
	// go.work for the go command; the error says the file is not read.
	f.write("go.work", "go 1.24\n\nuse (\n\t.\n\t../nothere\n)\n")
	r = f.run("HEAD", "", Options{})
	if r.code != ExitError || r.err == nil {
		t.Fatalf("exit = %d, err = %v; want error", r.code, r.err)
	}
	mustContain(t, r.err.Error(), "not in module cache; remove --fsreadonly to let go-whatchanged download it; go.work is not consulted, so a workspace module must come from the module cache or a replace directive")
}

// fakeSource is a modfetch.Source serving module versions from an in-memory
// filesystem, each at a synthetic directory of its own, the way an
// in-process Source would: nothing it serves is in the module cache.
type fakeSource struct {
	fs         billy.Filesystem
	versions   map[string]string // query → version; anything else resolves to itself
	gomods     map[string]string // version → go.mod, for a tree that has none
	mu         sync.Mutex
	fetched    []module.Version
	prefetched [][]module.Version
}

// Prefetch records the batch; the modules are served by Fetch as usual.
func (s *fakeSource) Prefetch(_ context.Context, mods []module.Version) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prefetched = append(s.prefetched, mods)
	return nil
}

func (s *fakeSource) Resolve(_ context.Context, path, query string) (module.Version, error) {
	if v, ok := s.versions[query]; ok {
		query = v
	}
	return module.Version{Path: path, Version: query}, nil
}

func (s *fakeSource) Fetch(_ context.Context, mod module.Version) (*modfetch.Module, error) {
	root := mod.String()
	if fi, err := s.fs.Stat(root); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s: not found", mod)
	}
	sub, err := s.fs.Chroot(root)
	if err != nil {
		return nil, err
	}
	gomod := []byte(s.gomods[mod.Version])
	if f, err := sub.Open("go.mod"); err == nil {
		gomod, err = io.ReadAll(f)
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	s.fetched = append(s.fetched, mod)
	s.mu.Unlock()
	return &modfetch.Module{Version: mod, Dir: vfs.SyntheticPrefix + "fetched/" + root, FS: &billyFS{fs: sub}, GoMod: gomod}, nil
}

func TestFetchMissingModule(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.useFakeModcache() // empty: every dependency is missing
	remote := memfs.New()
	writeFile(t, remote, "example.org/dep@v1.0.0/go.mod", "module example.org/dep\n\ngo 1.24\n")
	writeFile(t, remote, "example.org/dep@v1.0.0/dep.go", "package dep\n\ntype T struct{}\n")
	src := &fakeSource{fs: remote}

	f.write("go.mod", "module example.com/m\n\ngo 1.24\n\nrequire example.org/dep v1.0.0\n")
	f.write("a/a.go", "package a\n\nimport \"example.org/dep\"\n\nfunc A() dep.T { return dep.T{} }\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nimport \"example.org/dep\"\n\nfunc A() dep.T { return dep.T{} }\n\nfunc B() dep.T { return dep.T{} }\n")

	// With a Source, the missing module is fetched and mounted where the
	// Source says, on both sides, after each side asked for its missing
	// requirements as a batch.
	r := f.mustRun("HEAD", "", Options{Fetch: src, Color: false})
	mustContain(t, r.stdout, "+ func B() dep.T")
	mustNotContain(t, r.stdout, "func A()")
	dep := module.Version{Path: "example.org/dep", Version: "v1.0.0"}
	if len(src.fetched) == 0 || slices.ContainsFunc(src.fetched, func(m module.Version) bool { return m != dep }) {
		t.Errorf("fetched = %v", src.fetched)
	}
	if len(src.prefetched) != 2 || !slices.Equal(src.prefetched[0], []module.Version{dep}) || !slices.Equal(src.prefetched[1], []module.Version{dep}) {
		t.Errorf("prefetched = %v, want [%v] per side", src.prefetched, dep)
	}

	// Without one, the run is read-only and the error says which flag to
	// drop.
	r = f.run("HEAD", "", Options{})
	if r.code != ExitError || r.err == nil {
		t.Fatalf("exit = %d, err = %v; want error", r.code, r.err)
	}
	mustContain(t, r.err.Error(), "module example.org/dep@v1.0.0 not in module cache; remove --fsreadonly to let go-whatchanged download it")

	// A module the Source cannot find is the Source's error, named once.
	f.write("go.mod", "module example.com/m\n\ngo 1.24\n\nrequire example.org/dep v1.0.0\n\nrequire example.org/absent v1.5.0\n")
	f.write("a/a.go", "package a\n\nimport \"example.org/absent\"\n\nvar X = absent.X\n")
	r = f.run("HEAD", "", Options{Fetch: src})
	if r.code != ExitError || r.err == nil {
		t.Fatalf("exit = %d, err = %v; want error", r.code, r.err)
	}
	mustContain(t, r.err.Error(), `working tree: unresolvable import "example.org/absent" (required by example.com/m/a): example.org/absent@v1.5.0: not found`)
	mustNotContain(t, r.err.Error(), "fsreadonly")
}

func TestModuleSides(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.useFakeModcache()
	remote := memfs.New()
	const tip = "v1.1.1-0.20260101000000-abcdef123456"
	for v, body := range map[string]string{
		"v1.0.0": "func A() {}\n",
		"v1.1.0": "func A() {}\n\nfunc B() {}\n",
		tip:      "func A() {}\n\nfunc B() {}\n\nfunc C() {}\n",
	} {
		writeFile(t, remote, "example.org/lib@"+v+"/lib.go", "package lib\n\n"+body)
	}
	// The tree of v1.0.0 has no go.mod; the source knows it anyway.
	for _, v := range []string{"v1.1.0", tip} {
		writeFile(t, remote, "example.org/lib@"+v+"/go.mod", "module example.org/lib\n\ngo 1.24\n\nreplace example.org/other => ../other\n")
	}
	src := &fakeSource{
		fs:       remote,
		versions: map[string]string{"latest": "v1.1.0", "HEAD": tip},
		gomods:   map[string]string{"v1.0.0": "module example.org/lib\n\ngo 1.24\n"},
	}
	lib := func(q string) sideSpec { return sideSpec{mod: module.Version{Path: "example.org/lib", Version: q}} }

	// Latest release against the default branch: the base version is the
	// release, so the summary suggests the next one; replace directives
	// in the fetched go.mod are ignored.
	r := f.runSpecs(lib("latest"), lib("HEAD"), Options{Fetch: src})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "+ func C()", "would require: MINOR (v1.1.0 → v1.2.0)")
	mustNotContain(t, r.stdout, "func B()")
	if r.res.Base != "example.org/lib@v1.1.0" || r.res.Head != "example.org/lib@"+tip {
		t.Errorf("labels = %q, %q", r.res.Base, r.res.Head)
	}

	// Two releases, the older without a go.mod in its tree.
	r = f.runSpecs(lib("v1.0.0"), lib("v1.1.0"), Options{Fetch: src, Positions: true})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "+ func B()", "example.org/lib@v1.1.0:lib.go:5")
	mustNotContain(t, r.stdout, "func C()")

	// A pseudo-version base is no release, so nothing is suggested.
	r = f.runSpecs(lib(tip), lib("v1.1.0"), Options{Fetch: src})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "- func C()", "would require: MAJOR\n")
	mustNotContain(t, r.stdout, "→")

	// Without a source there is nothing to fetch a module version with.
	r = f.runSpecs(lib("latest"), lib("HEAD"), Options{})
	if r.err == nil || !strings.Contains(r.err.Error(), "example.org/lib@latest: diffing a module version needs the go command; remove --fsreadonly") {
		t.Errorf("without a source: %v", r.err)
	}
}

func TestParseTargets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	for _, tc := range []struct {
		base, head string
		wantBase   target
		wantHead   target
	}{
		{"", "", target{dir: ".", query: "HEAD"}, target{dir: "."}},
		{"@v1.4.0", "", target{dir: ".", query: "v1.4.0"}, target{dir: "."}},
		{"@v1.4.0", "@main", target{dir: ".", query: "v1.4.0"}, target{dir: ".", query: "main"}},
		{"@latest", "@HEAD", target{dir: ".", query: LatestRelease}, target{dir: ".", query: "HEAD"}},
		{"@origin/main", "", target{dir: ".", query: "origin/main"}, target{dir: "."}},
		{"@HEAD@{1}", "@main@{upstream}", target{dir: ".", query: "HEAD@{1}"}, target{dir: ".", query: "main@{upstream}"}},
		{"github.com/x/m@latest", "", target{module: "github.com/x/m", query: "latest"}, target{module: "github.com/x/m", query: "HEAD"}},
		{"github.com/x/m@latest", "@main", target{module: "github.com/x/m", query: "latest"}, target{module: "github.com/x/m", query: "main"}},
		{"github.com/x/m@v1.0.0", "@v1.1.0", target{module: "github.com/x/m", query: "v1.0.0"}, target{module: "github.com/x/m", query: "v1.1.0"}},
		{"github.com/x/m@v1.0.0", "@latest", target{module: "github.com/x/m", query: "v1.0.0"}, target{module: "github.com/x/m", query: "latest"}},
		// A local head beside a module base names its checkout.
		{"github.com/x/m@v1.0.0", ".@HEAD", target{module: "github.com/x/m", query: "v1.0.0"}, target{dir: ".", query: "HEAD"}},
		{"github.com/x/m@v1.0.0", "github.com/x/n@v2.0.0", target{module: "github.com/x/m", query: "v1.0.0"}, target{module: "github.com/x/n", query: "v2.0.0"}},
		{dir + "@latest", "", target{dir: dir, query: LatestRelease}, target{dir: dir}},
		{dir + "@v1.0.0", "@main", target{dir: dir, query: "v1.0.0"}, target{dir: dir, query: "main"}},
		{dir + "@v1.0.0", dir + "@main", target{dir: dir, query: "v1.0.0"}, target{dir: dir, query: "main"}},
		{dir, "", target{dir: dir, query: "HEAD"}, target{dir: dir}},
		{"./sub@v1", "", target{dir: "./sub", query: "v1"}, target{dir: "./sub"}},
		{"./sub@v1", "../other", target{dir: "./sub", query: "v1"}, target{dir: "../other"}},
		{"~/src/m@v1", "", target{dir: filepath.Join(home, "src", "m"), query: "v1"}, target{dir: filepath.Join(home, "src", "m")}},
	} {
		base, head, err := parseTargets(tc.base, tc.head)
		if err != nil {
			t.Errorf("parseTargets(%q, %q): %v", tc.base, tc.head, err)
			continue
		}
		if base != tc.wantBase || head != tc.wantHead {
			t.Errorf("parseTargets(%q, %q) = %+v, %+v; want %+v, %+v", tc.base, tc.head, base, head, tc.wantBase, tc.wantHead)
		}
	}
	for _, tc := range []struct{ base, head, want string }{
		{"@", "", "missing a version"},
		{"github.com/x/m@", "", "missing a version"},
		{"github.com/x/m", "", "a module needs a version: github.com/x/m@latest"},
		{"github.com/x/m@v1", "github.com/x/n", "a module needs a version: github.com/x/n@HEAD"},
		{"@v1", "@", "missing a version"},
		{"@v1", "@latest", "can only be the base"},
		// Without an @, an argument is a location: a bare revision is not
		// a module path, and a directory is spelled as a path.
		{"v1.4.0", "", "a tag, branch or commit of the current repository is written with an @: @v1.4.0"},
		{"origin/main", "", "written with an @: @origin/main"},
		{"sub@v1", "", "written with an @: @sub@v1"},
		{"testdata@v1", "", "a directory is written as a path: ./testdata@v1"},
	} {
		_, _, err := parseTargets(tc.base, tc.head)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseTargets(%q, %q) = %v, want %q", tc.base, tc.head, err, tc.want)
		}
	}
}

func TestUnknownRevision(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	h := f.commit("one")

	// With no tags, the error names the branches there are and the forms
	// a revision takes.
	r := f.run("nope", "", Options{})
	if r.err == nil {
		t.Fatal("no error for an unknown revision")
	}
	mustContain(t, r.err.Error(), "@nope: no such tag, branch or commit (branches: master); a revision is written @<tag>, @<branch>, @<commit> or @HEAD~2, and @latest is the newest release tag")
	mustNotContain(t, r.err.Error(), "tags:")

	// Something spelled like a release tag, in a clone with no tags at
	// all, was probably cloned without them.
	r = f.run("v1.4.0", "", Options{})
	if r.err == nil {
		t.Fatal("no error for an unknown revision")
	}
	mustContain(t, r.err.Error(), "@v1.4.0: no such tag, branch or commit (branches: master); the clone has no tags at all: git fetch --tags brings them")

	// Walking past the root commit is not a missing revision.
	r = f.run("HEAD~5", "", Options{})
	if r.err == nil {
		t.Fatal("no error for a revision past the root")
	}
	mustContain(t, r.err.Error(), "@HEAD~5: the history does not reach back that far")
	mustNotContain(t, r.err.Error(), "shallow")

	// With tags, it lists them, newest first, release versions before
	// the rest, and stops after a few.
	for _, name := range []string{"v1.10.0", "v1.9.0", "v1.8.0", "v1.7.0", "v1.6.0", "v1.5.0", "v1.4.0", "build-42"} {
		f.tag(name, h)
	}
	r = f.run("HEAD", "v1.4", Options{})
	if r.err == nil {
		t.Fatal("no error for an unknown revision")
	}
	mustContain(t, r.err.Error(), "@v1.4: no such tag, branch or commit (branches: master; tags: v1.10.0, v1.9.0, v1.8.0, v1.7.0, v1.6.0, v1.5.0, and 2 more); a revision is written")

	// A branch only a remote has, as after a clone or a CI checkout, and
	// a remote-tracking branch the remote has not delivered.
	if _, err := f.repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{"https://example.com/m.git"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "main"), h)); err != nil {
		t.Fatal(err)
	}
	r = f.run("main", "", Options{})
	if r.err == nil {
		t.Fatal("no error for an unknown revision")
	}
	mustContain(t, r.err.Error(), "@main: no such tag, branch or commit (branches: master; tags: v1.10.0, ", "); did you mean @origin/main?")
	r = f.run("origin/dev", "", Options{})
	if r.err == nil {
		t.Fatal("no error for an unknown revision")
	}
	mustContain(t, r.err.Error(), "@origin/dev: no such tag, branch or commit (branches: master; tags: v1.10.0, ", "); fetch it first: git fetch origin dev")
	mustNotContain(t, r.err.Error(), "a revision is written")
}

// TestShallowClone checks that a shallow clone, whose missing history and
// tags are the usual reason a revision or a release cannot be found, is
// named as the reason, with the fix for the environment. It sets an
// environment variable, so it does not run in parallel.
func TestShallowClone(t *testing.T) {
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	one := f.commit("one")
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n")
	two := f.commit("two")
	if err := f.repo.Storer.SetShallow([]plumbing.Hash{one}); err != nil {
		t.Fatal(err)
	}

	const hint = "; the clone is shallow: git fetch --unshallow --tags brings the whole history and its tags"
	r := f.run("v1.0.0", "", Options{})
	if r.err == nil {
		t.Fatal("no error for an unknown revision")
	}
	mustContain(t, r.err.Error(), "@v1.0.0: no such tag, branch or commit (branches: master)"+hint)
	mustNotContain(t, r.err.Error(), "a revision is written")

	r = f.run("HEAD~2", "", Options{})
	if r.err == nil {
		t.Fatal("no error for a revision past the shallow boundary")
	}
	mustContain(t, r.err.Error(), "@HEAD~2: the history does not reach back that far"+hint)

	r = f.run(LatestRelease, "", Options{})
	if r.err == nil {
		t.Fatal("no error for @latest without tags")
	}
	mustContain(t, r.err.Error(), `@latest: no release tags for example.com/m (looking for tags like "v1.2.3")`+hint)

	// Tags on commits the clone does not reach, fetch-tags without the
	// history in CI terms: the walk from HEAD ends at the shallow
	// boundary, whose parent is not in the clone, without an error of its
	// own.
	f.tag("v1.0.0", one)
	f.tag("v0.1.0", plumbing.NewHash("0123456789abcdef0123456789abcdef01234567"))
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n\nfunc C() {}\n")
	f.commit("three")
	if err := f.repo.Storer.SetShallow([]plumbing.Hash{two}); err != nil {
		t.Fatal(err)
	}
	mem := f.repo.Storer.(*memory.Storage)
	delete(mem.Objects, one)
	delete(mem.Commits, one)
	r = f.run(LatestRelease, "", Options{})
	if r.err == nil {
		t.Fatal("no error for @latest without a reachable tag")
	}
	mustContain(t, r.err.Error(), "@latest: none of the 2 release tag(s) for example.com/m (v1.0.0, v0.1.0) is an ancestor of HEAD"+hint)

	// The tag on the reachable side of the boundary is still found.
	f.tag("v1.1.0", two)
	r = f.mustRun(LatestRelease, "", Options{})
	mustContain(t, r.stdout, "+ func C()", "would require: MINOR (v1.1.0 → v1.2.0)")

	// In GitHub Actions, the fix is a checkout option.
	t.Setenv("GITHUB_ACTIONS", "true")
	r = f.run("v2.0.0", "", Options{})
	if r.err == nil {
		t.Fatal("no error for an unknown revision")
	}
	mustContain(t, r.err.Error(), "; the checkout is shallow: check out with fetch-depth: 0 to have the whole history and its tags")
}

// TestEmptyRepository checks the messages for a repository with no
// commits, where HEAD names a branch that does not exist yet.
func TestEmptyRepository(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, base := range []string{"HEAD", LatestRelease} {
		r := f.run(base, "", Options{})
		if r.code != ExitError || r.err == nil {
			t.Fatalf("run(%s): exit = %d, err = %v; want error", base, r.code, r.err)
		}
		if r.err.Error() != "@HEAD: the repository has no commits yet (master is an unborn branch)" {
			t.Errorf("run(%s): err = %q", base, r.err)
		}
	}
	r := f.run("main", "", Options{})
	if r.err == nil {
		t.Fatal("no error for an unknown revision")
	}
	mustContain(t, r.err.Error(), "@main: no such tag, branch or commit; a revision is written")
}

func TestMissingDirectory(t *testing.T) {
	t.Parallel()
	// A directory that does not exist must not fall through to the
	// repository above it.
	nope := filepath.Join(t.TempDir(), "nope")
	var out, errb bytes.Buffer
	_, err := Run(Options{Base: nope + "@HEAD", Stdout: &out, Stderr: &errb})
	if err == nil || !strings.Contains(err.Error(), nope+": no such directory") {
		t.Errorf("Run(%s@HEAD) = %v", nope, err)
	}
}

func TestDirectoryTargets(t *testing.T) {
	t.Parallel()
	dir, base, head := diskFixture(t)
	// The tests run inside this project's repository; a directory target
	// names another one, and "@rev" follows it.
	var out, errb bytes.Buffer
	code, err := Run(Options{Base: dir + "@" + base.String()[:7], Head: "@" + head.String(), Stdout: &out, Stderr: &errb})
	if err != nil || code != ExitClean {
		t.Fatalf("exit = %d, err = %v, stderr = %q", code, err, errb.String())
	}
	mustContain(t, out.String(), "example.com/m/a\n  + func B()\n")

	// Alone, the head is that checkout's working tree.
	out.Reset()
	code, err = Run(Options{Base: dir + "@" + base.String(), Stdout: &out, Stderr: &errb})
	if err != nil || code != ExitClean {
		t.Fatalf("exit = %d, err = %v, stderr = %q", code, err, errb.String())
	}
	mustContain(t, out.String(), "  + func B()\n")
}

// ownDeps reads this project's go.mod (two directories up from this
// package) to find module versions that are guaranteed to be present in the
// module cache.
func ownDeps(t *testing.T, paths ...string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	mf, err := modfile.ParseLax("go.mod", data, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, req := range mf.Require {
		for _, p := range paths {
			if req.Mod.Path == p {
				out[p] = req.Mod.Version
			}
		}
	}
	for _, p := range paths {
		if out[p] == "" {
			t.Fatalf("go.mod does not require %s", p)
		}
	}
	return out
}

func TestDependencies(t *testing.T) {
	t.Parallel()
	deps := ownDeps(t, "golang.org/x/exp", "golang.org/x/tools")
	f := newFixture(t)
	f.write("go.mod", fmt.Sprintf("module example.com/m\n\ngo 1.24\n\nrequire (\n\tgolang.org/x/exp %s\n\tgolang.org/x/tools %s // indirect\n)\n",
		deps["golang.org/x/exp"], deps["golang.org/x/tools"]))
	f.write("a/a.go", `package a

import (
	"fmt"
	"io"

	"golang.org/x/exp/apidiff"
)

type Reporter interface {
	Report() apidiff.Report
}

func Print(w io.Writer, r apidiff.Report) { fmt.Fprint(w, r) }

var _ fmt.Stringer = (*apidiff.Report)(nil)
`)
	f.commit("base")
	f.write("a/a.go", `package a

import (
	"fmt"
	"io"

	"golang.org/x/exp/apidiff"
)

type Reporter interface {
	Report() apidiff.Report
}

func Print(w io.Writer, r apidiff.Report) { fmt.Fprint(w, r) }

func Changes(r apidiff.Report) []apidiff.Change { return r.Changes }

func Describe(s fmt.Stringer) string { return s.String() }
`)
	r := f.mustRun("HEAD", "", Options{})
	if r.stderr != "" {
		t.Errorf("stderr = %q, want none", r.stderr)
	}
	want := "example.com/m/a\n  + func Changes(r apidiff.Report) []apidiff.Change\n  + func Describe(s fmt.Stringer) string\n\n1 package changed · 0 incompatible · 2 compatible · would require: MINOR\n"
	if r.stdout != want {
		t.Errorf("stdout = %q\nwant     %q", r.stdout, want)
	}
}

func TestReplaceDirectory(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("go.mod", "module example.com/m\n\ngo 1.24\n\nrequire example.com/lib v0.0.0\n\nreplace example.com/lib => ./lib\n")
	f.write("lib/go.mod", "module example.com/lib\n\ngo 1.24\n")
	f.write("lib/lib.go", "package lib\n\ntype Conn struct{}\n")
	f.write("a/a.go", "package a\n\nimport \"example.com/lib\"\n\nfunc Open() lib.Conn { return lib.Conn{} }\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nimport \"example.com/lib\"\n\nfunc Open() lib.Conn { return lib.Conn{} }\n\nfunc Close(c lib.Conn) {}\n")

	r := f.mustRun("HEAD", "", Options{})
	if r.stderr != "" {
		t.Errorf("stderr = %q, want none", r.stderr)
	}
	want := "example.com/m/a\n  + func Close(c lib.Conn)\n\n1 package changed · 0 incompatible · 1 compatible · would require: MINOR\n"
	if r.stdout != want {
		t.Errorf("stdout = %q\nwant     %q", r.stdout, want)
	}
}

// A replacement outside the module root but inside the repository, ../lib
// from sub, is read from the tree like the module itself, so a problem in
// it is named by its path in the tree rather than by a synthetic mount
// path.
func TestReplaceDirectoryOutsideModule(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.remove("go.mod")
	f.write("sub/go.mod", "module example.com/m\n\ngo 1.24\n\nrequire example.com/lib v0.0.0\n\nreplace example.com/lib => ../lib\n")
	f.write("lib/go.mod", "module example.com/lib\n\ngo 1.24\n")
	f.write("lib/lib.go", "package lib\n\ntype Conn struct{}\n\nvar Broken undefinedType\n")
	f.write("sub/a/a.go", "package a\n\nimport \"example.com/lib\"\n\nfunc Open() lib.Conn { return lib.Conn{} }\n")
	f.commit("base")

	var out, errb bytes.Buffer
	opts := Options{Stdout: &out, Stderr: &errb}
	res, err := compare(t.Context(), sideSpec{rev: "HEAD", open: f.open, rel: "sub"}, sideSpec{fs: &billyFS{fs: f.fs}, open: f.open, rel: "sub"}, f.env, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := finish(res, opts); err != nil {
		t.Fatal(err)
	}
	if want := "warn: example.com/lib: HEAD:lib/lib.go:5:12: undefined: undefinedType\nwarn: example.com/lib: lib/lib.go:5:12: undefined: undefinedType\n"; errb.String() != want {
		t.Errorf("stderr = %q\nwant     %q", errb.String(), want)
	}
	if want := "no exported API changes\n"; out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
}

func TestReplaceVersion(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.useFakeModcache()
	// Only the replacement is in the cache: resolving the required version
	// would fail.
	f.writeModule("example.com/q", "v1.1.0", map[string]string{"q.go": "package q\n\ntype T struct{}\n"})
	f.write("go.mod", "module example.com/m\n\ngo 1.24\n\nrequire example.com/q v1.0.0\n\nreplace example.com/q v1.0.0 => example.com/q v1.1.0\n")
	f.write("a/a.go", "package a\n\nimport \"example.com/q\"\n\nfunc A() q.T { return q.T{} }\n")
	f.commit("base")

	r := f.mustRun("HEAD", "", Options{})
	if r.stderr != "" || r.stdout != "no exported API changes\n" {
		t.Errorf("stdout = %q, stderr = %q", r.stdout, r.stderr)
	}
}

// A nested module that go.mod requires is served from the module cache, as
// the go command does, not from its directory in the tree.
func TestRequiredNestedModuleComesFromModuleCache(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.useFakeModcache()
	f.writeModule("example.com/m/sub", "v1.0.0", map[string]string{"sub.go": "package sub\n\nconst FromCache = 1\n"})
	f.write("go.mod", "module example.com/m\n\ngo 1.24\n\nrequire example.com/m/sub v1.0.0\n")
	f.write("sub/go.mod", "module example.com/m/sub\n\ngo 1.24\n")
	f.write("sub/sub.go", "package sub\n\nconst FromTree = 1\n")
	f.write("a/a.go", "package a\n\nimport \"example.com/m/sub\"\n\nconst A = sub.FromCache\n")
	f.commit("base")

	r := f.mustRun("HEAD", "", Options{})
	if r.stderr != "" || r.stdout != "no exported API changes\n" {
		t.Errorf("stdout = %q, stderr = %q", r.stdout, r.stderr)
	}
}

// A replaced module may have a path without a dot, which otherwise marks
// the standard library.
func TestReplacedModuleWithoutDot(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("go.mod", "module example.com/m\n\ngo 1.24\n\nrequire foo v0.0.0\n\nreplace foo => ./foo\n")
	f.write("foo/go.mod", "module foo\n\ngo 1.24\n")
	f.write("foo/bar/bar.go", "package bar\n\ntype Baz struct{}\n")
	f.write("a/a.go", "package a\n\nimport \"foo/bar\"\n\nfunc A() bar.Baz { return bar.Baz{} }\n")
	f.commit("base")

	r := f.mustRun("HEAD", "", Options{})
	if r.stderr != "" || r.stdout != "no exported API changes\n" {
		t.Errorf("stdout = %q, stderr = %q", r.stdout, r.stderr)
	}
}

func TestSubdirectoryModule(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.remove("go.mod")
	f.write("README", "not a go module at the root\n")
	f.write("sub/go.mod", "module example.com/sub\n\ngo 1.24\n")
	f.write("sub/a/a.go", "package a\n\nfunc A() {}\n")
	f.commit("base")
	f.write("sub/a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n")

	var out, errb bytes.Buffer
	opts := Options{Stdout: &out, Stderr: &errb}
	res, err := compare(t.Context(), sideSpec{rev: "HEAD", open: f.open, rel: "sub"}, sideSpec{fs: &billyFS{fs: f.fs}, open: f.open, rel: "sub"}, f.env, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := finish(res, opts); err != nil {
		t.Fatal(err)
	}
	mustContain(t, out.String(), "example.com/sub/a\n  + func B()\n")
}

func goldenFixture(t *testing.T) *fixture {
	f := newFixture(t)
	f.write("store/store.go", `package store

import "fmt"

type Client struct{ name string }

func (c *Client) Close() error { return nil }

func Open(path string) (*Client, error) { return &Client{name: path}, nil }

func Describe(c *Client) string { return fmt.Sprint(c.name) }

type Point struct{ X, Y int }

type Config struct{ Timeout int }

const Version = "1"

var Default = Open
`)
	f.write("util/util.go", "package util\n\ntype Sizer interface{ Len() int }\n\ntype Stringer interface{ String() string }\n")
	f.write("gone/gone.go", "package gone\n\nfunc Gone() {}\n")
	f.write("internal/hidden/hidden.go", "package hidden\n\nfunc Hidden() {}\n\nfunc Keep() {}\n")
	h := f.commit("base")
	f.tag("v1.0.0", h)

	f.write("store/store.go", `package store

import "fmt"

type Client struct{ name string }

func (c *Client) Ping() error { return nil }

type Options struct{ Timeout int }

func Open(path string, o Options) (*Client, error) { return &Client{name: path}, nil }

func Describe(c *Client) string { return fmt.Sprint(c.name) }

type Point struct{ X, Y, Z int }

type Config struct{ Timeout int64 }

const Version = 1

var Default func(string) (*Client, error)
`)
	f.write("util/util.go", "package util\n\nimport \"fmt\"\n\ntype Sizer interface {\n\tLen() int\n\tSize() int\n}\n\ntype Stringer = fmt.Stringer\n")
	f.remove("gone/gone.go")
	f.write("fresh/fresh.go", "package fresh\n\nfunc Hello() {}\n")
	f.write("internal/hidden/hidden.go", "package hidden\n\nfunc Keep() {}\n\nfunc Added() {}\n")
	return f
}

func TestGolden(t *testing.T) {
	t.Parallel()
	f := goldenFixture(t)
	for _, tc := range []struct {
		name string
		opts Options
		code int
	}{
		{"nocolor", Options{}, ExitIncompatible},
		{"color", Options{Color: true}, ExitIncompatible},
		{"breaking", Options{Breaking: true}, ExitIncompatible},
		{"markdown", Options{Format: render.Markdown}, ExitIncompatible},
		{"json", Options{Format: render.JSON}, ExitIncompatible},
		{"public", Options{Filter: render.Public}, ExitIncompatible},
		// Internal packages alone: no public API in the selection.
		{"internal", Options{Filter: render.Internal}, ExitClean},
		{"pos", Options{Positions: true}, ExitIncompatible},
		{"json_pos", Options{Format: render.JSON, Positions: true}, ExitIncompatible},
		{"minimal", Options{Signatures: render.MinimalSignatures}, ExitIncompatible},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := f.run("v1.0.0", "", tc.opts)
			if r.err != nil {
				t.Fatal(r.err)
			}
			if r.code != tc.code {
				t.Errorf("exit = %d, want %d", r.code, tc.code)
			}
			got := "# stdout\n" + r.stdout + "# stderr\n" + r.stderr
			golden := filepath.Join("testdata", "golden_"+tc.name+".txt")
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to create)", err)
			}
			if string(want) != got {
				t.Errorf("output differs from %s:\n--- got ---\n%s\n--- want ---\n%s", golden, got, want)
			}
		})
	}
}

// snapshot describes every file under a directory tree.
type snapshot struct {
	digest string // of the names, sizes and mtimes; "" for a missing root
	count  int
}

func snapshots(t *testing.T, roots map[string]string) map[string]snapshot {
	t.Helper()
	out := map[string]snapshot{}
	for name, root := range roots {
		out[name] = snapshotOf(t, root)
	}
	return out
}

func snapshotOf(t *testing.T, root string) snapshot {
	t.Helper()
	if root == "" {
		return snapshot{}
	}
	if _, err := os.Stat(root); err != nil {
		return snapshot{}
	}
	var s snapshot
	h := sha256.New()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are not something we could have written
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		s.count++
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00%d\n", p, info.Mode(), info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.digest = hex.EncodeToString(h.Sum(nil))
	return s
}

func goCache() string {
	if v := os.Getenv("GOCACHE"); v != "" {
		return v
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "go-build")
}

// TestWriteGuard runs the tool against this repository's own history and
// asserts that GOROOT, the module cache, the build cache and the repository
// itself are untouched. It is the one sequential test of the package, so
// nothing else runs in this process meanwhile; the go command, though, may
// still be compiling this module's other test binaries into GOCACHE when
// the test starts, so the baseline is taken once nothing has changed for a
// moment.
func TestWriteGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping filesystem snapshot in -short mode")
	}
	env, err := modres.DefaultEnv()
	if err != nil {
		t.Fatal(err)
	}
	repoDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	roots := map[string]string{
		"GOROOT":     env.GOROOT,
		"GOMODCACHE": env.GOMODCACHE,
		"GOCACHE":    goCache(),
		"repo":       repoDir,
	}
	before := snapshots(t, roots)
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		time.Sleep(500 * time.Millisecond)
		now := snapshots(t, roots)
		if maps.Equal(now, before) {
			break
		}
		t.Logf("waiting: something else is writing to %v", changedRoots(before, now))
		before = now
	}

	var out, errb bytes.Buffer
	code, err := Run(Options{
		Base:   repoDir + "@HEAD",
		GOOS:   runtime.GOOS,
		GOARCH: runtime.GOARCH,
		Stdout: &out,
		Stderr: &errb,
	})
	if err != nil {
		t.Fatalf("Run: %v (stderr: %s)", err, errb.String())
	}
	if code == ExitError {
		t.Fatalf("Run exited %d: %s", code, errb.String())
	}
	t.Logf("diff of HEAD vs working tree:\n%s%s", out.String(), errb.String())

	after := snapshots(t, roots)
	for _, name := range changedRoots(before, after) {
		t.Errorf("%s (%s) changed during the run: %d files before, %d after", name, roots[name], before[name].count, after[name].count)
	}
}

// changedRoots names the roots whose snapshots differ, in order.
func changedRoots(before, after map[string]snapshot) []string {
	var out []string
	for name := range before {
		if before[name] != after[name] {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func TestExitFail(t *testing.T) {
	t.Parallel()
	// One fixture per required level: major (removed func), minor (added
	// func) and patch (no exported API change).
	levels := map[string]func(f *fixture){
		"major": func(f *fixture) { f.write("a/a.go", "package a\n\nfunc Keep() {}\n") },
		"minor": func(f *fixture) {
			f.write("a/a.go", "package a\n\nfunc Keep() {}\n\nfunc Drop() {}\n\nfunc Added() {}\n")
		},
		"patch": func(f *fixture) {
			f.write("a/a.go", "package a\n\n// Comment only.\nfunc Keep() {}\n\nfunc Drop() {}\n")
		},
	}
	tests := []struct {
		level string
		fail  FailOn
		want  int
	}{
		{"major", FailNever, ExitIncompatible},
		{"major", FailMajor, ExitMajor},
		{"major", FailMinor, ExitMajor},
		{"major", FailPatch, ExitMajor},
		{"minor", FailNever, ExitClean},
		{"minor", FailMajor, ExitClean},
		{"minor", FailMinor, ExitMinor},
		{"minor", FailPatch, ExitMinor},
		{"patch", FailNever, ExitClean},
		{"patch", FailMajor, ExitClean},
		{"patch", FailMinor, ExitClean},
		{"patch", FailPatch, ExitPatch},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%s/%d", tc.level, tc.fail), func(t *testing.T) {
			f := newFixture(t)
			f.write("a/a.go", "package a\n\nfunc Keep() {}\n\nfunc Drop() {}\n")
			f.commit("base")
			levels[tc.level](f)
			r := f.run("HEAD", "", Options{ExitFail: tc.fail})
			if r.err != nil {
				t.Fatal(r.err)
			}
			if r.code != tc.want {
				t.Errorf("exit = %d, want %d:\n%s", r.code, tc.want, r.stdout)
			}
		})
	}
}

func TestExitFailStrictErrorWins(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nvar Broken = undefined\n")
	r := f.run("HEAD", "", Options{Strict: true, ExitFail: FailPatch})
	if r.code != ExitError || r.err == nil {
		t.Errorf("exit = %d, err = %v; want %d and an error", r.code, r.err, ExitError)
	}
}

func TestParseFailOn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want FailOn
		ok   bool
	}{
		{"major", FailMajor, true},
		{"MINOR", FailMinor, true},
		{"patch", FailPatch, true},
		{"", FailNever, false},
		{"breaking", FailNever, false},
	}
	for _, tc := range tests {
		got, err := ParseFailOn(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ParseFailOn(%q) = %d, %v; want %d, ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

func TestPackageWithoutExportedAPIRemovedOrAdded(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.write("plugin/plugin.go", "package plugin\n\nfunc init() {}\n")
	f.commit("base")
	f.remove("plugin/plugin.go")
	f.write("other/other.go", "package other\n\nfunc init() {}\n")

	r := f.mustRun("HEAD", "", Options{})
	want := "example.com/m/other (new)\n  + package added\n\n" +
		"example.com/m/plugin (removed)\n  - package removed\n\n" +
		"2 packages changed · 1 incompatible · 1 compatible · would require: MAJOR\n"
	if r.stdout != want {
		t.Errorf("stdout = %q\nwant     %q", r.stdout, want)
	}
	if r.code != ExitIncompatible {
		t.Errorf("exit = %d, want %d", r.code, ExitIncompatible)
	}

	// A nested module is not part of this module's API on either side.
	f.write("plugin/go.mod", "module example.com/m/plugin\n\ngo 1.24\n")
	f.write("plugin/plugin.go", "package plugin\n\nfunc init() {}\n")
	r = f.mustRun("HEAD", "", Options{Breaking: true})
	mustContain(t, r.stdout, "example.com/m/plugin (removed)\n  - package removed\n")
	mustNotContain(t, r.stdout, "package added")
}

func TestConstantValueChange(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nconst Version = \"1.2.0\"\n\nconst Limit int64 = 10\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nconst Version = \"1.3.0-dev\"\n\nconst Limit int64 = 20\n")

	r := f.mustRun("HEAD", "", Options{})
	mustContain(t, r.stdout,
		"  - const Limit int64 = 10\n  + const Limit int64 = 20\n",
		"  - const Version untyped string = \"1.2.0\"\n  + const Version untyped string = \"1.3.0-dev\"\n")

	// go/constant abbreviates a long string value with "..." and no closing
	// quote, in the declarations and in apidiff's message alike; the
	// declarations are shown all the same.
	long := strings.Repeat("a", 80)
	f.write("a/a.go", "package a\n\nconst Long = \""+long+"\"\n")
	f.commit("long")
	f.write("a/a.go", "package a\n\nconst Long = \""+long+"b\"\n")
	r = f.mustRun("HEAD", "", Options{})
	abbreviated := "\"" + strings.Repeat("a", 68) + "..."
	if want := "example.com/m/a\n  - const Long untyped string = " + abbreviated + "\n  + const Long untyped string = " + abbreviated + "\n"; !strings.HasPrefix(r.stdout, want) {
		t.Errorf("stdout = %q\nwant prefix %q", r.stdout, want)
	}
	r = f.mustRun("HEAD", "", Options{Format: render.JSON})
	mustContain(t, r.stdout, `"before": "const Long untyped string = \"`+strings.Repeat("a", 68)+`..."`)
}

// A dependency that imports the main module (grpc-go and go-control-plane
// import each other, for instance) must be linked against each side's own
// main-module packages. Sharing one checked copy between the sides makes
// the other side see two distinct core.Client types and report bogus type
// errors, even when both sides are identical.
func TestDependencyImportingMainModuleIsNotShared(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.useFakeModcache()
	f.writeModule("example.com/dep", "v1.0.0", map[string]string{
		"dep.go": "package dep\n\nimport \"example.com/m/core\"\n\nfunc Use(c core.Client) {}\n",
	})
	f.write("go.mod", "module example.com/m\n\ngo 1.24\n\nrequire example.com/dep v1.0.0\n")
	f.write("core/core.go", "package core\n\ntype Client struct{}\n")
	f.write("a/a.go", "package a\n\nimport (\n\t\"example.com/dep\"\n\t\"example.com/m/core\"\n)\n\nfunc A(c core.Client) { dep.Use(c) }\n")
	f.commit("base")

	for i := range 5 {
		r := f.mustRun("HEAD", "", Options{})
		if r.stderr != "" {
			t.Fatalf("run %d: stderr = %q, want none", i, r.stderr)
		}
		if r.stdout != "no exported API changes\n" {
			t.Errorf("run %d: stdout = %q", i, r.stdout)
		}
	}

	// The same holds for two committed revisions of the same tree.
	r := f.mustRun("HEAD", "HEAD", Options{})
	if r.stderr != "" {
		t.Errorf("stderr = %q, want none", r.stderr)
	}
}

// When the two go.mod files pin a transitive dependency to different
// versions, a package that imports it (same directory on both sides) must be
// checked once per side; otherwise one side links it against the other
// side's version of the dependency.
func TestDependencyPinnedToDifferentVersionsIsNotShared(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.useFakeModcache()
	f.writeModule("example.com/q", "v1.0.0", map[string]string{"q.go": "package q\n\ntype T struct{}\n"})
	f.writeModule("example.com/q", "v1.1.0", map[string]string{"q.go": "package q\n\ntype T struct{}\n\nfunc New() T { return T{} }\n"})
	f.writeModule("example.com/p", "v1.0.0", map[string]string{
		"p.go": "package p\n\nimport \"example.com/q\"\n\nfunc Use(t q.T) {}\n",
	})
	f.write("go.mod", "module example.com/m\n\ngo 1.24\n\nrequire (\n\texample.com/p v1.0.0\n\texample.com/q v1.0.0\n)\n")
	f.write("a/a.go", "package a\n\nimport (\n\t\"example.com/p\"\n\t\"example.com/q\"\n)\n\nfunc A() { p.Use(q.T{}) }\n")
	f.commit("base")
	f.write("go.mod", "module example.com/m\n\ngo 1.24\n\nrequire (\n\texample.com/p v1.0.0\n\texample.com/q v1.1.0\n)\n")

	for i := range 5 {
		r := f.mustRun("HEAD", "", Options{})
		if r.stderr != "" {
			t.Fatalf("run %d: stderr = %q, want none", i, r.stderr)
		}
		if r.stdout != "no exported API changes\n" {
			t.Errorf("run %d: stdout = %q", i, r.stdout)
		}
	}
}

func TestLatestRelease(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	one := f.commit("one")
	f.tag("v0.1.0", one)
	f.tag("release-1", one) // not a semantic version: ignored
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n")
	two := f.commit("two")
	f.annotatedTag("v0.2.0", two)
	f.tag("v0.2", two)   // not canonical: ignored
	f.tag("1.0.0", two)  // no "v": ignored
	f.tag("v2.0.0", two) // wrong major for a module path without /v2: ignored

	// A higher release on a branch that is not an ancestor of HEAD does
	// not count.
	f.checkout("side", true, one)
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc Side() {}\n")
	side := f.commit("side")
	f.tag("v0.9.0", side)
	f.checkout("master", false, plumbing.ZeroHash)

	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n\nfunc C() {}\n")
	three := f.commit("three")
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n\nfunc C() {}\n\nfunc D() {}\n")

	r := f.mustRun(LatestRelease, "", Options{})
	mustContain(t, r.stdout, "  + func C()\n", "  + func D()\n",
		"1 package changed · 0 incompatible · 2 compatible · would require: MINOR (v0.2.0 → v0.3.0)\n")
	mustNotContain(t, r.stdout, "func B()", "Side")
	if r.code != ExitClean {
		t.Errorf("exit = %d, want %d", r.code, ExitClean)
	}

	// A tag on the head commit itself is skipped: on a freshly tagged
	// commit, @latest names the previous release, so the diff describes
	// the new one.
	f.tag("v0.3.0", three)
	r = f.mustRun(LatestRelease, "HEAD", Options{})
	mustContain(t, r.stdout, "  + func C()\n", "would require: MINOR (v0.2.0 → v0.3.0)\n")
	mustNotContain(t, r.stdout, "func D()")

	// Explicit release tags get the same suggestion, however they are spelled.
	for _, base := range []string{"v0.1.0", "tags/v0.1.0", "refs/tags/v0.1.0"} {
		r = f.run(base, "HEAD", Options{})
		if r.err != nil {
			t.Fatalf("%s: %v", base, r.err)
		}
		mustContain(t, r.stdout, "would require: MINOR (v0.1.0 → v0.2.0)\n")
	}

	// A commit with no differences names the release in the empty message.
	r = f.mustRun("v0.3.0", "HEAD", Options{})
	if want := "no exported API changes since v0.3.0\n"; r.stdout != want {
		t.Errorf("stdout = %q, want %q", r.stdout, want)
	}

	// Breaking changes since a v0 release bump the minor version.
	f.write("a/a.go", "package a\n\nfunc B() {}\n\nfunc C() {}\n")
	r = f.mustRun(LatestRelease, "", Options{})
	mustContain(t, r.stdout, "  - func A()\n", "would require: MAJOR (v0.2.0 → v0.3.0)\n")

	// @latest is only meaningful as the base.
	r = f.run("HEAD", LatestRelease, Options{})
	if r.code != ExitError || r.err == nil {
		t.Fatalf("exit = %d, err = %v; want error", r.code, r.err)
	}
	mustContain(t, r.err.Error(), "@latest can only be the base revision")
}

func TestLatestReleasePrerelease(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.tag("v0.9.0", f.commit("one"))
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n")
	f.tag("v1.0.0-rc.1", f.commit("two"))
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n\nfunc C() {}\n")
	f.commit("three")

	r := f.mustRun(LatestRelease, "", Options{})
	mustContain(t, r.stdout, "  + func C()\n", "would require: MINOR (v1.0.0-rc.1 → v1.0.0)\n")
	mustNotContain(t, r.stdout, "func B()")

	f.write("a/a.go", "package a\n\nfunc B() {}\n\nfunc C() {}\n")
	r = f.mustRun(LatestRelease, "", Options{})
	mustContain(t, r.stdout, "  - func A()\n", "would require: MAJOR (v1.0.0-rc.1 → v1.0.0)\n")
}

func TestLatestReleaseMajorSuffix(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("go.mod", "module example.com/m/v2\n\ngo 1.24\n")
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	one := f.commit("one")
	f.tag("v1.9.0", one) // belongs to the v1 module path
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n")
	two := f.commit("two")
	f.tag("v2.0.0", two)
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n\nfunc C() {}\n")
	f.commit("three")
	f.write("a/a.go", "package a\n\nfunc B() {}\n\nfunc C() {}\n")

	r := f.mustRun(LatestRelease, "", Options{})
	mustContain(t, r.stdout, "  - func A()\n", "  + func C()\n", "would require: MAJOR (v2.0.0 → v3.0.0)\n")
	mustNotContain(t, r.stdout, "func B()")
}

func TestLatestReleaseSubdirectoryModule(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.remove("go.mod")
	f.write("sub/go.mod", "module example.com/m/sub\n\ngo 1.24\n")
	f.write("sub/a/a.go", "package a\n\nfunc A() {}\n")
	one := f.commit("one")
	f.tag("sub/v1.0.0", one)
	f.tag("v5.0.0", one) // the root module's release, not sub's
	f.write("sub/a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n")
	two := f.commit("two")
	f.tag("sub/v1.1.0", two)
	f.write("sub/a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n\nfunc C() {}\n")
	f.commit("three")

	var out, errb bytes.Buffer
	opts := Options{Stdout: &out, Stderr: &errb}
	res, err := compare(t.Context(), sideSpec{rev: LatestRelease, open: f.open, rel: "sub"}, sideSpec{fs: &billyFS{fs: f.fs}, open: f.open, rel: "sub"}, f.env, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := finish(res, opts); err != nil {
		t.Fatal(err)
	}
	mustContain(t, out.String(), "example.com/m/sub/a\n  + func C()\n", "would require: MINOR (v1.1.0 → v1.2.0)\n")
	mustNotContain(t, out.String(), "func B()")
	if res.Base != "sub/v1.1.0" || res.Head != "working tree" {
		t.Errorf("labels = %q, %q", res.Base, res.Head)
	}
}

func TestLatestReleaseErrors(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	one := f.commit("one")
	r := f.run(LatestRelease, "", Options{})
	if r.code != ExitError || r.err == nil {
		t.Fatalf("exit = %d, err = %v; want error", r.code, r.err)
	}
	mustContain(t, r.err.Error(), `@latest: no release tags for example.com/m (looking for tags like "v1.2.3"); the clone has no tags at all: git fetch --tags brings them, if the repository has any`)

	// Tags exist, but none fits the module: the wrong layout for a
	// nested module, say. The message shows them.
	f.tag("build-42", one)
	f.tag("v1.0", one)
	r = f.run(LatestRelease, "", Options{})
	if r.code != ExitError || r.err == nil {
		t.Fatalf("exit = %d, err = %v; want error", r.code, r.err)
	}
	mustContain(t, r.err.Error(), `@latest: no release tags for example.com/m (looking for tags like "v1.2.3"; tags: v1.0, build-42)`)

	// A release tag exists, but on a side branch, not among the
	// ancestors of HEAD.
	f.checkout("side", true, one)
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc Side() {}\n")
	f.tag("v0.1.0", f.commit("side"))
	f.checkout("master", false, plumbing.ZeroHash)
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n")
	two := f.commit("two")
	r = f.run(LatestRelease, "", Options{})
	if r.code != ExitError || r.err == nil {
		t.Fatalf("exit = %d, err = %v; want error", r.code, r.err)
	}
	mustContain(t, r.err.Error(), "@latest: none of the 1 release tag(s) for example.com/m (v0.1.0) is an ancestor of HEAD")
	mustNotContain(t, r.err.Error(), "shallow")

	// The tag on HEAD itself, which @latest skips: the usual case right
	// after tagging a release, and the fix is to name the tag.
	f.tag("v0.2.0", two)
	r = f.run(LatestRelease, "", Options{})
	if r.code != ExitError || r.err == nil {
		t.Fatalf("exit = %d, err = %v; want error", r.code, r.err)
	}
	mustContain(t, r.err.Error(), "@latest: v0.2.0 is the newest release tag reachable from HEAD, but it is on HEAD itself, which @latest skips; name it instead: @v0.2.0")
}

func TestJSON(t *testing.T) {
	t.Parallel()
	f := goldenFixture(t)
	r := f.mustRun("v1.0.0", "", Options{Format: render.JSON, Filter: render.Public})
	if r.code != ExitIncompatible {
		t.Errorf("exit = %d", r.code)
	}
	var rep struct {
		Base, Head  string
		BaseVersion string `json:"base_version"`
		NextVersion string `json:"next_version"`
		Packages    []struct {
			Path, Status string
			Changes      []struct {
				Symbol, Kind, Message, Before, After string
				Compatible                           bool
			}
		}
		Warnings []struct{ Package, Message string }
		Summary  struct {
			PackagesChanged int `json:"packages_changed"`
			Incompatible    int
			Compatible      int
			Release         string
		}
	}
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, r.stdout)
	}
	if rep.Base != "v1.0.0" || rep.Head != "working tree" || rep.BaseVersion != "v1.0.0" || rep.NextVersion != "v2.0.0" {
		t.Errorf("sides = %+v", rep)
	}
	if rep.Summary.PackagesChanged != 4 || rep.Summary.Incompatible != 7 || rep.Summary.Compatible != 4 || rep.Summary.Release != "major" {
		t.Errorf("summary = %+v", rep.Summary)
	}
	if rep.Warnings == nil || len(rep.Warnings) != 0 {
		t.Errorf("warnings = %v, want an empty array", rep.Warnings)
	}
	if len(rep.Packages) != 4 || rep.Packages[0].Path != "example.com/m/fresh" || rep.Packages[0].Status != "new" || rep.Packages[1].Status != "removed" || rep.Packages[2].Status != "changed" {
		t.Errorf("packages = %+v", rep.Packages)
	}
	store := rep.Packages[2]
	if len(store.Changes) != 7 {
		t.Fatalf("store changes = %+v", store.Changes)
	}
	open := store.Changes[2]
	if open.Symbol != "Open" || open.Kind != "changed" || open.Compatible ||
		open.Before != "func Open(path string) (*Client, error)" || open.After != "func Open(path string, o Options) (*Client, error)" ||
		!strings.HasPrefix(open.Message, "Open: changed from ") {
		t.Errorf("Open = %+v", open)
	}
	if ping := store.Changes[4]; ping.Symbol != "(*Client).Ping" || ping.Kind != "added" || !ping.Compatible || ping.Before != "" || ping.After != "func (c *Client) Ping() error" {
		t.Errorf("Ping = %+v", ping)
	}
	if timeout := store.Changes[1]; timeout.Symbol != "Config.Timeout" || timeout.Before != "field Config.Timeout int" || timeout.After != "field Config.Timeout int64" {
		t.Errorf("Config.Timeout = %+v", timeout)
	}
	mustNotContain(t, r.stdout, `"pos"`, `"internal"`)
	if closed := store.Changes[0]; closed.Symbol != "(*Client).Close" || closed.Kind != "removed" || closed.Before != "func (c *Client) Close() error" || closed.After != "" {
		t.Errorf("Close = %+v", closed)
	}

	// --breaking drops compatible changes from the lists but not the counts.
	r = f.mustRun("v1.0.0", "", Options{Format: render.JSON, Breaking: true, Filter: render.Public})
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Packages) != 3 || rep.Summary.Compatible != 4 {
		t.Errorf("breaking: packages = %d, compatible = %d", len(rep.Packages), rep.Summary.Compatible)
	}
	for _, p := range rep.Packages {
		for _, c := range p.Changes {
			if c.Compatible {
				t.Errorf("breaking: compatible change %q listed", c.Message)
			}
		}
	}
}

func TestJSONPackageWithoutSymbols(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.write("plugin/plugin.go", "package plugin\n\nfunc init() {}\n")
	f.commit("base")
	f.remove("plugin/plugin.go")
	r := f.mustRun("HEAD", "", Options{Format: render.JSON})
	mustContain(t, r.stdout, `"symbol": "",`, `"kind": "removed",`, `"message": "package removed"`)
}

func TestFormatsWithoutChanges(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.commit("base")
	r := f.mustRun("HEAD", "", Options{Format: render.Markdown})
	if want := "_no exported API changes_\n"; r.stdout != want {
		t.Errorf("markdown = %q, want %q", r.stdout, want)
	}
	r = f.mustRun("HEAD", "", Options{Format: render.JSON})
	mustContain(t, r.stdout, `"base": "HEAD"`, `"packages": [],`, `"release": "patch"`)
	mustNotContain(t, r.stdout, "base_version", "next_version")
}

func TestParseFormat(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]render.Format{"text": render.Text, "markdown": render.Markdown, "md": render.Markdown, "JSON": render.JSON} {
		got, err := ParseFormat(in)
		if err != nil || got != want {
			t.Errorf("ParseFormat(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseFormat("yaml"); err == nil {
		t.Error("ParseFormat accepted yaml")
	}
}

func TestPositions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc Keep() {}\n\nfunc Drop() {}\n\ntype T struct {\n\tX int\n}\n\nfunc (t T) M(n int) {}\n")
	h := f.commit("base")
	f.tag("v0.1.0", h)
	f.write("a/a.go", "package a\n\nfunc Keep() {}\n\ntype T struct {\n\tX int64\n\tY int\n}\n\nfunc (t T) M(n int64) {}\n\nfunc Added() {}\n")
	f.write("b/b.go", "package b\n\nfunc init() {}\n")

	r := f.mustRun("v0.1.0", "", Options{Positions: true})
	// A position sits on the line of the declaration it locates, the new
	// one of a change or an addition and the old one of a removal, two
	// spaces past the widest such line of the package.
	want := "example.com/m/a\n" +
		"  - func Drop()            v0.1.0:a/a.go:5:6\n" +
		"  - func (t T) M(n int)\n" +
		"  + func (t T) M(n int64)  a/a.go:10:12\n" +
		"  - field T.X int\n" +
		"  + field T.X int64        a/a.go:6:2\n" +
		"  + func Added()           a/a.go:12:6\n" +
		"  + field T.Y int          a/a.go:7:2\n" +
		"\n" +
		"example.com/m/b (new)\n" +
		"  + package added\n" +
		"\n" +
		"2 packages changed · 3 incompatible · 3 compatible · would require: MAJOR (v0.1.0 → v0.2.0)\n"
	if r.stdout != want {
		t.Errorf("stdout = \n%s\nwant\n%s", r.stdout, want)
	}

	// Positions are off by default; the layout is then the plain one.
	r = f.mustRun("v0.1.0", "", Options{})
	mustContain(t, r.stdout, "  - func Drop()\n  - func (t T) M(n int)\n  + func (t T) M(n int64)\n")
	mustNotContain(t, r.stdout, "a.go")

	// Two committed revisions: both sides carry a revision prefix.
	f.commit("head")
	r = f.mustRun("v0.1.0", "HEAD", Options{Positions: true})
	mustContain(t, r.stdout, "  - func Drop()            v0.1.0:a/a.go:5:6\n", "  + func Added()           HEAD:a/a.go:12:6\n")

	// With a width limit, the column narrows to the widest row whose
	// position still fits, so that one long declaration does not push
	// every position past the edge of the terminal. The rows too wide
	// for the column get their position after two spaces, unaligned.
	f.write("a/a.go", "package a\n\nfunc Keep() {}\n\ntype T struct {\n\tX int64\n\tY int\n}\n\nfunc (t T) M(n int64) {}\n\nfunc Added() {}\n\nfunc AddedWithAVeryLongSignature(first, second, third string) (fourth, fifth, sixth int) {}\n")
	r = f.mustRun("v0.1.0", "", Options{Positions: true, Width: 50})
	// "  - func Drop()" is 15 columns; at width 50 the "v0.1.0:a/a.go:5:6"
	// position (17) ends at column 34 when aligned at the 23-wide
	// "+ func (t T) M(n int64)".
	want = "  - func Drop()            v0.1.0:a/a.go:5:6\n" +
		"  - func (t T) M(n int)\n" +
		"  + func (t T) M(n int64)  a/a.go:10:12\n" +
		"  - field T.X int\n" +
		"  + field T.X int64        a/a.go:6:2\n" +
		"  + func Added()           a/a.go:12:6\n" +
		"  + func AddedWithAVeryLongSignature(first string, second string, third string) (fourth int, fifth int, sixth int)  a/a.go:14:6\n" +
		"  + field T.Y int          a/a.go:7:2\n"
	mustContain(t, r.stdout, want)
	// Narrower still, and the column drops to the 15-wide "+ field T.Y
	// int", the widest at which "- func Drop()" still fits its position.
	r = f.mustRun("v0.1.0", "", Options{Positions: true, Width: 36})
	mustContain(t, r.stdout,
		"  - func Drop()    v0.1.0:a/a.go:5:6\n",
		"  + func (t T) M(n int64)  a/a.go:10:12\n",
		"  + field T.X int64  a/a.go:6:2\n",
		"  + func Added()   a/a.go:12:6\n",
		"  + field T.Y int  a/a.go:7:2\n")
	// Without a limit, the long row sets the column.
	r = f.mustRun("v0.1.0", "", Options{Positions: true})
	mustContain(t, r.stdout, "  - func Drop()"+strings.Repeat(" ", 101)+"v0.1.0:a/a.go:5:6\n")
}

// A dependency upgrade can change a package's API through promoted fields
// and methods. Those are declared outside the module, so they carry no
// position, and their messages name the dependency's symbol, which cannot
// be looked up, so the renderer falls back to the types quoted in the
// message even when those contain " to " themselves.
func TestPromotedMembersFromDependency(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.useFakeModcache()
	f.writeModule("example.com/dep", "v1.0.0", map[string]string{"dep.go": "package dep\n\ntype Base struct{ X int }\n\nfunc (b *Base) M(fn func(from, to string)) {}\n"})
	f.writeModule("example.com/dep", "v1.1.0", map[string]string{"dep.go": "package dep\n\ntype Base struct{ X, Y int }\n\nfunc (b *Base) M(fn func(from, to string)) error { return nil }\n"})
	f.write("go.mod", "module example.com/m\n\ngo 1.24\n\nrequire example.com/dep v1.0.0\n")
	f.write("a/a.go", "package a\n\nimport \"example.com/dep\"\n\ntype T struct{ dep.Base }\n")
	f.commit("base")
	f.write("go.mod", "module example.com/m\n\ngo 1.24\n\nrequire example.com/dep v1.1.0\n")

	r := f.mustRun("HEAD", "", Options{Positions: true})
	want := "example.com/m/a\n" +
		"  ~ example.com/dep.(*Base).M: changed\n" +
		"      - func(func(from string, to string))\n" +
		"      + func(func(from string, to string)) error\n" +
		"  + field T.Y int\n" +
		"\n1 package changed · 1 incompatible · 1 compatible · would require: MAJOR\n"
	if r.stdout != want {
		t.Errorf("stdout = %q\nwant     %q", r.stdout, want)
	}

	r = f.mustRun("HEAD", "", Options{Positions: true, Format: render.JSON})
	mustContain(t, r.stdout, `"before": "func(func(from string, to string))"`, `"after": "func(func(from string, to string)) error"`)
	mustNotContain(t, r.stdout, `"pos"`)
}

func TestPackageFilter(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.write("store/store.go", "package store\n\nfunc S() {}\n")
	f.write("store/sub/sub.go", "package sub\n\nfunc Sub() {}\n")
	f.write("util/util.go", "package util\n\nfunc U() {}\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc A2() {}\n")
	f.write("store/store.go", "package store\n\nfunc S() {}\n\nfunc S2() {}\n")
	f.write("store/sub/sub.go", "package sub\n\nfunc Sub() {}\n\nfunc Sub2() {}\n")
	f.remove("util/util.go")

	tests := []struct {
		name          string
		pkgs, exclude []string
		want          string
		stderr        string
	}{
		{"all", nil, nil, "example.com/m/a\n  + func A2()\n\nexample.com/m/store\n  + func S2()\n\nexample.com/m/store/sub\n  + func Sub2()\n\nexample.com/m/util (removed)\n  - func U()\n\n4 packages changed · 1 incompatible · 3 compatible · would require: MAJOR\n", ""},
		{"store", []string{"store/..."}, nil, "example.com/m/store\n  + func S2()\n\nexample.com/m/store/sub\n  + func Sub2()\n\n2 packages changed · 0 incompatible · 2 compatible · would require: MINOR\n", ""},
		{"exact", []string{"example.com/m/store"}, nil, "example.com/m/store\n  + func S2()\n\n1 package changed · 0 incompatible · 1 compatible · would require: MINOR\n", ""},
		{"two", []string{"./a", "util"}, nil, "example.com/m/a\n  + func A2()\n\nexample.com/m/util (removed)\n  - func U()\n\n2 packages changed · 1 incompatible · 1 compatible · would require: MAJOR\n", ""},
		{"exclude", nil, []string{"store/...", "util"}, "example.com/m/a\n  + func A2()\n\n1 package changed · 0 incompatible · 1 compatible · would require: MINOR\n", ""},
		{"both", []string{"store/..."}, []string{".../sub"}, "example.com/m/store\n  + func S2()\n\n1 package changed · 0 incompatible · 1 compatible · would require: MINOR\n", ""},
		{"removed only on base", []string{"util"}, nil, "example.com/m/util (removed)\n  - func U()\n\n1 package changed · 1 incompatible · 0 compatible · would require: MAJOR\n", ""},
		{"typo", []string{"stor/...", "a"}, nil, "example.com/m/a\n  + func A2()\n\n1 package changed · 0 incompatible · 1 compatible · would require: MINOR\n", "warn: example.com/m: --pkg \"stor/...\" matched no packages\n"},
		{"nothing", []string{"nothing"}, nil, "no exported API changes\n", "warn: example.com/m: --pkg \"nothing\" matched no packages\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := f.run("HEAD", "", Options{Packages: tc.pkgs, Exclude: tc.exclude})
			if r.err != nil {
				t.Fatal(r.err)
			}
			if r.stdout != tc.want {
				t.Errorf("stdout = %q\nwant     %q", r.stdout, tc.want)
			}
			if r.stderr != tc.stderr {
				t.Errorf("stderr = %q\nwant     %q", r.stderr, tc.stderr)
			}
		})
	}

	// The exit code follows the filtered diff.
	r := f.run("HEAD", "", Options{Packages: []string{"store/..."}})
	if r.code != ExitClean {
		t.Errorf("exit = %d, want %d", r.code, ExitClean)
	}
	r = f.run("HEAD", "", Options{Packages: []string{"nothing"}, Strict: true})
	if r.code != ExitError {
		t.Errorf("strict: exit = %d, want %d", r.code, ExitError)
	}
}

func TestFilterInternalPackages(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.write("internal/hidden/h.go", "package hidden\n\nfunc Hidden() {}\n")
	f.write("a/internal/deep/d.go", "package deep\n\nfunc Deep() {}\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc Added() {}\n")
	f.write("internal/hidden/h.go", "package hidden\n\nfunc Renamed() {}\n")
	f.write("a/internal/deep/d.go", "package deep\n\nfunc Deep() {}\n\nfunc Deeper() {}\n")
	f.write("internal/fresh/f.go", "package fresh\n\nfunc F() {}\n")

	// With --filter=public internal packages do not exist.
	r := f.mustRun("HEAD", "", Options{Filter: render.Public})
	if want := "example.com/m/a\n  + func Added()\n\n1 package changed · 0 incompatible · 1 compatible · would require: MINOR\n"; r.stdout != want {
		t.Errorf("public: stdout = %q\nwant     %q", r.stdout, want)
	}

	// By default (--filter=all) they follow the public API, marked and with
	// a summary line of their own, but the public summary, the release and
	// the exit code still describe the public API only.
	r = f.mustRun("HEAD", "", Options{})
	want := "example.com/m/a\n  + func Added()\n\n" +
		"1 package changed · 0 incompatible · 1 compatible · would require: MINOR\n\n" +
		"example.com/m/a/internal/deep (internal)\n  + func Deeper()\n\n" +
		"example.com/m/internal/fresh (internal, new)\n  + func F()\n\n" +
		"example.com/m/internal/hidden (internal)\n  - func Hidden()\n  + func Renamed()\n\n" +
		"internal: 3 packages changed · 1 incompatible · 3 compatible\n"
	if r.stdout != want {
		t.Errorf("stdout = %q\nwant     %q", r.stdout, want)
	}
	if r.code != ExitClean {
		t.Errorf("exit = %d, want %d", r.code, ExitClean)
	}
	r = f.run("HEAD", "", Options{ExitFail: FailMajor})
	if r.code != ExitClean {
		t.Errorf("--exit-fail=major: exit = %d, want %d", r.code, ExitClean)
	}

	// Only internal changes: the public API is untouched, which the first
	// line says, and the internal packages follow.
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	r = f.mustRun("HEAD", "", Options{})
	want = "no exported API changes\n\n" +
		"example.com/m/a/internal/deep (internal)\n  + func Deeper()\n\n" +
		"example.com/m/internal/fresh (internal, new)\n  + func F()\n\n" +
		"example.com/m/internal/hidden (internal)\n  - func Hidden()\n  + func Renamed()\n\n" +
		"internal: 3 packages changed · 1 incompatible · 3 compatible\n"
	if r.stdout != want {
		t.Errorf("internal only: stdout = %q\nwant     %q", r.stdout, want)
	}
	if r.code != ExitClean {
		t.Errorf("exit = %d, want %d", r.code, ExitClean)
	}
	// With --filter=public the message points at what was left out.
	r = f.mustRun("HEAD", "", Options{Filter: render.Public})
	if want := "no exported API changes; add --filter=all to include internal API changes\n"; r.stdout != want {
		t.Errorf("public: stdout = %q\nwant     %q", r.stdout, want)
	}

	// --breaking and --pkg apply to internal packages too.
	r = f.mustRun("HEAD", "", Options{Breaking: true})
	if want := "no exported API changes\n\nexample.com/m/internal/hidden (internal)\n  - func Hidden()\n\ninternal: 3 packages changed · 1 incompatible · 3 compatible\n"; r.stdout != want {
		t.Errorf("breaking: stdout = %q\nwant     %q", r.stdout, want)
	}
	r = f.mustRun("HEAD", "", Options{Packages: []string{"internal/hidden"}})
	if want := "no exported API changes\n\nexample.com/m/internal/hidden (internal)\n  - func Hidden()\n  + func Renamed()\n\ninternal: 1 package changed · 1 incompatible · 1 compatible\n"; r.stdout != want {
		t.Errorf("pkg: stdout = %q\nwant     %q", r.stdout, want)
	}

	// --filter=internal shows internal packages alone, with only their
	// summary line; there is no public API in the selection, so the exit
	// code is the clean one.
	r = f.mustRun("HEAD", "", Options{Filter: render.Internal, ExitFail: FailMajor})
	want = "example.com/m/a/internal/deep (internal)\n  + func Deeper()\n\n" +
		"example.com/m/internal/fresh (internal, new)\n  + func F()\n\n" +
		"example.com/m/internal/hidden (internal)\n  - func Hidden()\n  + func Renamed()\n\n" +
		"internal: 3 packages changed · 1 incompatible · 3 compatible\n"
	if r.stdout != want {
		t.Errorf("internal: stdout = %q\nwant     %q", r.stdout, want)
	}
	if r.code != ExitClean {
		t.Errorf("internal: exit = %d, want %d", r.code, ExitClean)
	}
	r = f.mustRun("HEAD", "", Options{Filter: render.Internal, Packages: []string{"a"}})
	if want := "warn: example.com/m: --pkg \"a\" matched no packages\n"; r.stderr != want {
		t.Errorf("internal --pkg a: stderr = %q, want %q", r.stderr, want)
	}
	if want := "internal: no changes\n"; r.stdout != want {
		t.Errorf("internal --pkg a: stdout = %q, want %q", r.stdout, want)
	}

	// Nothing changed anywhere: no internal section at all, unless asked
	// for internal packages alone.
	f.commit("all")
	r = f.mustRun("HEAD", "", Options{})
	if want := "no exported API changes\n"; r.stdout != want {
		t.Errorf("clean: stdout = %q\nwant     %q", r.stdout, want)
	}
	r = f.mustRun("HEAD", "", Options{Filter: render.Internal})
	if want := "internal: no changes\n"; r.stdout != want {
		t.Errorf("clean internal: stdout = %q\nwant     %q", r.stdout, want)
	}

	// Machine-readable layouts carry the same split.
	f.write("internal/hidden/h.go", "package hidden\n\nfunc Renamed() {}\n\nfunc More() {}\n")
	r = f.mustRun("HEAD", "", Options{Format: render.JSON})
	mustContain(t, r.stdout, `"internal": true,`, `"release": "patch",`,
		"\"internal\": {\n      \"packages_changed\": 1,\n      \"incompatible\": 0,\n      \"compatible\": 1\n    }")
	r = f.mustRun("HEAD", "", Options{Format: render.JSON, Filter: render.Public})
	mustNotContain(t, r.stdout, `"internal"`)
	r = f.mustRun("HEAD", "", Options{Format: render.Markdown})
	if want := "_no exported API changes_\n\n**example.com/m/internal/hidden (internal)**\n\n```diff\n+ func More()\n```\n\n_internal: 1 package changed · 0 incompatible · 1 compatible_\n"; r.stdout != want {
		t.Errorf("markdown: stdout = %q\nwant     %q", r.stdout, want)
	}
	r = f.mustRun("HEAD", "", Options{Filter: render.Internal, Format: render.Markdown})
	mustContain(t, r.stdout, "```\n\ninternal: 1 package changed · 0 incompatible · 1 compatible\n")
	mustNotContain(t, r.stdout, "exported API")
	r = f.mustRun("HEAD", "", Options{Filter: render.Internal, Format: render.JSON})
	mustContain(t, r.stdout, `"packages_changed": 0,`, `"release": "patch",`, "\"internal\": {\n      \"packages_changed\": 1,")
}

func TestMinimalSignatures(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc Keep() {}\n\nfunc Drop() {}\n\nfunc Open(name string) error { return nil }\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nfunc Keep() {}\n\nfunc Open(name string, n int) error { return nil }\n\nfunc Added() {}\n")

	// One line per change, with apidiff's message as is.
	r := f.mustRun("HEAD", "", Options{Signatures: render.MinimalSignatures})
	want := "example.com/m/a\n" +
		"  - Drop: removed\n" +
		"  ~ Open: changed from func(string) error to func(string, int) error\n" +
		"  + Added: added\n" +
		"\n1 package changed · 2 incompatible · 1 compatible · would require: MAJOR\n"
	if r.stdout != want {
		t.Errorf("stdout = %q\nwant     %q", r.stdout, want)
	}

	// The default shows every declaration.
	r = f.mustRun("HEAD", "", Options{})
	want = "example.com/m/a\n" +
		"  - func Drop()\n" +
		"  - func Open(name string) error\n  + func Open(name string, n int) error\n" +
		"  + func Added()\n" +
		"\n1 package changed · 2 incompatible · 1 compatible · would require: MAJOR\n"
	if r.stdout != want {
		t.Errorf("full: stdout = %q\nwant     %q", r.stdout, want)
	}

	// Markdown and JSON follow the same knob.
	r = f.mustRun("HEAD", "", Options{Signatures: render.MinimalSignatures, Format: render.Markdown})
	mustContain(t, r.stdout, "```diff\n- Drop: removed\n! Open: changed from func(string) error to func(string, int) error\n+ Added: added\n```\n")
	r = f.mustRun("HEAD", "", Options{Signatures: render.MinimalSignatures, Format: render.JSON})
	mustNotContain(t, r.stdout, `"before"`, `"after"`)
	r = f.mustRun("HEAD", "", Options{Format: render.JSON})
	mustContain(t, r.stdout, `"before": "func Drop()"`, `"after": "func Added()"`, `"before": "func Open(name string) error"`)
}

func TestParseFilter(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]render.Visibility{"public": render.Public, "internal": render.Internal, "main": render.Main, "ALL": render.All} {
		got, err := ParseFilter(in)
		if err != nil || got != want {
			t.Errorf("ParseFilter(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseFilter("private"); err == nil {
		t.Error("ParseFilter accepted private")
	}
}

func TestParseSignatures(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]render.Signatures{"full": render.FullSignatures, "Minimal": render.MinimalSignatures} {
		got, err := ParseSignatures(in)
		if err != nil || got != want {
			t.Errorf("ParseSignatures(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseSignatures("short"); err == nil {
		t.Error("ParseSignatures accepted short")
	}
}

func TestFilterMainPackages(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.write("internal/x/x.go", "package x\n\nfunc X() {}\n")
	f.write("cmd/m/main.go", "package main\n\nfunc Version() string { return \"1\" }\n\nfunc main() {}\n")
	f.write("internal/cmd/tool/main.go", "package main\n\nfunc Run() {}\n\nfunc main() {}\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc Added() {}\n")
	f.write("internal/x/x.go", "package x\n\nfunc X() {}\n\nfunc Y() {}\n")
	f.write("cmd/m/main.go", "package main\n\nfunc main() {}\n")
	f.write("internal/cmd/tool/main.go", "package main\n\nfunc Run() {}\n\nfunc Also() {}\n\nfunc main() {}\n")

	// By default (--filter=all) main packages follow the internal section,
	// marked, in a section of their own; a command below an internal
	// directory is a main package first. They never count towards the
	// public summary, the release or the exit code.
	public := "example.com/m/a\n  + func Added()\n\n" +
		"1 package changed · 0 incompatible · 1 compatible · would require: MINOR\n"
	internal := "example.com/m/internal/x (internal)\n  + func Y()\n\n" +
		"internal: 1 package changed · 0 incompatible · 1 compatible\n"
	main := "example.com/m/cmd/m (main)\n  - func Version() string\n\n" +
		"example.com/m/internal/cmd/tool (internal, main)\n  + func Also()\n\n" +
		"main: 2 packages changed · 1 incompatible · 1 compatible\n"
	r := f.mustRun("HEAD", "", Options{ExitFail: FailMajor})
	if want := public + "\n" + internal + "\n" + main; r.stdout != want {
		t.Errorf("default: stdout = %q\nwant     %q", r.stdout, want)
	}
	if r.code != ExitClean {
		t.Errorf("default: exit = %d, want %d", r.code, ExitClean)
	}

	// --filter=public leaves both out; --filter=internal shows the internal
	// packages that are not commands.
	r = f.mustRun("HEAD", "", Options{Filter: render.Public})
	if want := public; r.stdout != want {
		t.Errorf("public: stdout = %q\nwant     %q", r.stdout, want)
	}
	r = f.mustRun("HEAD", "", Options{Filter: render.Internal})
	if want := internal; r.stdout != want {
		t.Errorf("internal: stdout = %q\nwant     %q", r.stdout, want)
	}

	// --filter=main shows the commands alone, with only their summary line.
	r = f.mustRun("HEAD", "", Options{Filter: render.Main, ExitFail: FailMajor})
	if want := main; r.stdout != want {
		t.Errorf("main: stdout = %q\nwant     %q", r.stdout, want)
	}
	if r.code != ExitClean {
		t.Errorf("main: exit = %d, want %d", r.code, ExitClean)
	}

	// --filter=public,main: the importable API, then the commands; the
	// internal packages stay out.
	r = f.mustRun("HEAD", "", Options{Filter: render.Public | render.Main})
	if want := public + "\n" + main; r.stdout != want {
		t.Errorf("public,main: stdout = %q\nwant     %q", r.stdout, want)
	}

	// Without changes to the commands the section disappears below the
	// public API, but stays when asked for on its own.
	f.write("cmd/m/main.go", "package main\n\nfunc Version() string { return \"1\" }\n\nfunc main() {}\n")
	f.write("internal/cmd/tool/main.go", "package main\n\nfunc Run() {}\n\nfunc main() {}\n")
	r = f.mustRun("HEAD", "", Options{})
	if want := public + "\n" + internal; r.stdout != want {
		t.Errorf("unchanged main: stdout = %q\nwant     %q", r.stdout, want)
	}
	r = f.mustRun("HEAD", "", Options{Filter: render.Main})
	if want := "main: no changes\n"; r.stdout != want {
		t.Errorf("unchanged --filter=main: stdout = %q\nwant     %q", r.stdout, want)
	}
	f.write("cmd/m/main.go", "package main\n\nfunc main() {}\n")

	// Markdown italicizes the section below the public API like the
	// internal one; JSON marks the packages and counts them under
	// summary.main, which appears when main packages took part.
	r = f.mustRun("HEAD", "", Options{Filter: render.Public | render.Main, Format: render.Markdown})
	mustContain(t, r.stdout, "**example.com/m/cmd/m (main)**\n\n```diff\n- func Version() string\n```\n\n_main: 1 package changed · 1 incompatible · 0 compatible_\n")
	mustNotContain(t, r.stdout, "internal:")
	r = f.mustRun("HEAD", "", Options{Filter: render.Public | render.Main, Format: render.JSON})
	mustContain(t, r.stdout, `"main": true`, `"path": "example.com/m/cmd/m"`,
		"\"main\": {\n      \"packages_changed\": 1,\n      \"incompatible\": 1,\n      \"compatible\": 0\n    }")
	mustNotContain(t, r.stdout, `"internal": true`, "\"internal\": {\n")
	r = f.mustRun("HEAD", "", Options{Filter: render.Public, Format: render.JSON})
	mustNotContain(t, r.stdout, `"main"`)
}

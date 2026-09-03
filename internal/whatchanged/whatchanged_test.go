package whatchanged

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"golang.org/x/mod/modfile"

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
}

// run diffs base against the fixture's in-memory worktree (or head, if set).
// An empty base takes the same default as Run.
func (f *fixture) run(base, head string, opts Options) runResult {
	f.t.Helper()
	var out, errb bytes.Buffer
	opts.Stdout = &out
	opts.Stderr = &errb
	opts.Base = base
	var mounts []vfs.Mount
	if f.modcache != nil {
		mounts = []vfs.Mount{{Path: f.env.GOMODCACHE, FS: vfs.NewBillyFS(f.modcache)}}
	}
	headSpec := sideSpec{fs: vfs.NewBillyFS(f.fs), mounts: mounts}
	if head != "" {
		headSpec = sideSpec{rev: head, mounts: mounts}
	}
	res, err := runRepo(f.open, sideSpec{rev: opts.baseRev(), mounts: mounts}, headSpec, "", f.env, opts)
	if err != nil {
		return runResult{stderr: errb.String(), code: ExitError, err: err}
	}
	code, err := finish(res, opts)
	return runResult{stdout: out.String(), stderr: errb.String(), code: code, err: err}
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
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.commit("base")
	r := f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
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
	f := newFixture(t)
	f.write("old/old.go", "package old\n\nfunc Gone() {}\n\ntype T struct{}\n")
	f.commit("base")
	f.remove("old/old.go")
	f.write("fresh/fresh.go", "package fresh\n\nfunc Hello() {}\n")
	r := f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout,
		"example.com/m/fresh (new)\n  + func Hello()\n",
		"example.com/m/old (removed)\n  - func Gone()\n  - type T struct{}\n",
		"2 packages changed · 2 incompatible · 1 compatible · would require: MAJOR\n")
	if r.code != ExitIncompatible {
		t.Errorf("exit = %d, want %d", r.code, ExitIncompatible)
	}
}

func TestRemovedFunc(t *testing.T) {
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc Keep() {}\n\nfunc Drop() {}\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nfunc Keep() {}\n")
	r := f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "example.com/m/a\n  - func Drop()\n", "would require: MAJOR")
	mustNotContain(t, r.stdout, "Keep")
	if r.code != ExitIncompatible {
		t.Errorf("exit = %d", r.code)
	}
}

func TestChangedSignature(t *testing.T) {
	f := newFixture(t)
	f.write("a/a.go", "package a\n\ntype Client struct{}\n\nfunc (c *Client) Do(n int) {}\n\nfunc Open(name string) error { return nil }\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\ntype Client struct{}\n\nfunc (c *Client) Do(n int, tags ...string) {}\n\ntype Options struct{}\n\nfunc Open(name string, o Options) error { return nil }\n")
	r := f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout,
		"  - func (c *Client) Do(n int)\n  + func (c *Client) Do(n int, tags ...string)\n",
		"  - func Open(name string) error\n  + func Open(name string, o Options) error\n",
		"  + type Options struct{}\n",
		"1 package changed · 2 incompatible · 1 compatible · would require: MAJOR\n")
}

func TestAddedStructFieldIsCompatible(t *testing.T) {
	f := newFixture(t)
	f.write("a/a.go", "package a\n\ntype Point struct{ X, Y int }\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\ntype Point struct{ X, Y, Z int }\n")
	r := f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "  + field Point.Z int\n", "would require: MINOR")
	if r.code != ExitClean {
		t.Errorf("exit = %d, want %d (compatible change)", r.code, ExitClean)
	}
}

func TestAddedInterfaceMethodIsIncompatibleAddition(t *testing.T) {
	f := newFixture(t)
	f.write("a/a.go", "package a\n\ntype Sizer interface{ Len() int }\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\ntype Sizer interface {\n\tLen() int\n\tSize() int\n}\n")
	r := f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "  + func (Sizer) Size() int\n", "would require: MAJOR")
	if r.code != ExitIncompatible {
		t.Errorf("exit = %d", r.code)
	}
}

func TestIgnoredDirectories(t *testing.T) {
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
	r := f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stdout != "no exported API changes\n" {
		t.Errorf("stdout = %q", r.stdout)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q", r.stderr)
	}
}

func TestCommittedDirectorySymlinkIsIgnored(t *testing.T) {
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

	r := f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stdout != "no exported API changes\n" {
		t.Errorf("stdout = %q", r.stdout)
	}
}

func TestGOOSFiltering(t *testing.T) {
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.commit("base")
	f.write("a/a_windows.go", "package a\n\nfunc WindowsOnly() {}\n")
	f.write("a/tagged.go", "//go:build plan9\n\npackage a\n\nfunc Plan9Only() {}\n")

	r := f.run("HEAD", "", Options{GOOS: "linux", GOARCH: "amd64"})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stdout != "no exported API changes\n" {
		t.Errorf("linux stdout = %q", r.stdout)
	}

	r = f.run("HEAD", "", Options{GOOS: "windows", GOARCH: "amd64"})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "  + func WindowsOnly()\n")
	mustNotContain(t, r.stdout, "Plan9Only")

	r = f.run("HEAD", "", Options{GOOS: "plan9", GOARCH: "amd64"})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "  + func Plan9Only()\n")
	mustNotContain(t, r.stdout, "WindowsOnly")
}

func TestTypeErrorWarnsButDiffs(t *testing.T) {
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n\nvar Broken undefinedType\n")

	r := f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "  + func B()\n", "  + var Broken invalid type\n")
	if want := "warn: example.com/m/a: a/a.go:7:12: undefined: undefinedType\n"; r.stderr != want {
		t.Errorf("stderr = %q, want %q", r.stderr, want)
	}
	if r.code != ExitClean {
		t.Errorf("exit = %d", r.code)
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
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nvar Broken undefinedType\n")
	h := f.commit("base")
	f.tag("v0.1.0", h)
	f.write("a/a.go", "package a\n\nvar Broken int\n")
	r := f.run("v0.1.0", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if want := "warn: example.com/m/a: v0.1.0:a/a.go:3:12: undefined: undefinedType\n"; r.stderr != want {
		t.Errorf("stderr = %q, want %q", r.stderr, want)
	}
	mustContain(t, r.stdout, "  - var Broken invalid type\n  + var Broken int\n")
}

func TestBreakingHidesCompatible(t *testing.T) {
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc Drop() {}\n")
	f.write("b/b.go", "package b\n\nfunc B() {}\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nfunc Added() {}\n")
	f.write("b/b.go", "package b\n\nfunc B() {}\n\nfunc B2() {}\n")
	r := f.run("HEAD", "", Options{Breaking: true})
	if r.err != nil {
		t.Fatal(r.err)
	}
	want := "example.com/m/a\n  - func Drop()\n\n2 packages changed · 1 incompatible · 2 compatible · would require: MAJOR\n"
	if r.stdout != want {
		t.Errorf("stdout = %q\nwant     %q", r.stdout, want)
	}
}

func TestBreakingWithOnlyCompatibleChanges(t *testing.T) {
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc Added() {}\n")
	r := f.run("HEAD", "", Options{Breaking: true})
	if r.err != nil {
		t.Fatal(r.err)
	}
	want := "1 package changed · 0 incompatible · 1 compatible · would require: MINOR\n"
	if r.stdout != want {
		t.Errorf("stdout = %q\nwant     %q", r.stdout, want)
	}
}

func TestHeadCommit(t *testing.T) {
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.commit("one")
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n")
	f.commit("two")
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n\nfunc Uncommitted() {}\n")

	r := f.run("HEAD~1", "HEAD", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "  + func B()\n")
	mustNotContain(t, r.stdout, "Uncommitted")

	r = f.run("HEAD~1", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
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
	write := func(name, content string) {
		t.Helper()
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(msg string) plumbing.Hash {
		t.Helper()
		if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
			t.Fatal(err)
		}
		sig := testSignature()
		h, err := wt.Commit(msg, &git.CommitOptions{Author: sig, Committer: sig})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	write("go.mod", "module example.com/m\n\ngo 1.24\n")
	write("a/a.go", "package a\n\nfunc A() {}\n")
	base = commit("base")
	write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n")
	head = commit("head")

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
	dir, base, head := diskFixture(t)
	// The race this guards against is timing dependent; a handful of runs
	// made it show up reliably before the fix.
	for i := range 10 {
		var out, errb bytes.Buffer
		code, err := Run(Options{
			Repo:   dir,
			Base:   base.String()[:7], // abbreviated, as typed from git log
			Head:   head.String(),
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
	code, err := Run(Options{Repo: linked, Base: "HEAD~1", Stdout: &out, Stderr: &errb})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != ExitClean {
		t.Errorf("exit = %d, stderr = %q", code, errb.String())
	}
	mustContain(t, out.String(), "  + func B()\n", "  + func Uncommitted()\n")

	out.Reset()
	code, err = Run(Options{Repo: linked, Base: base.String(), Head: "HEAD", Stdout: &out, Stderr: &errb})
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
	f := newFixture(t)
	f.write("go.mod", "module example.com/m\n\ngo 1.99\n")
	f.write("a/a.go", "package a\n\nimport \"fmt\"\n\nfunc A() { fmt.Println() }\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nimport \"fmt\"\n\nfunc A() { fmt.Println() }\n\nfunc B() {}\n")

	r := f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
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
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.commit("one")
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc Committed() {}\n")
	f.commit("two")
	f.write("a/a.go", "package a\n\nfunc Committed() {}\n\nfunc Uncommitted() {}\n")

	r := f.run("", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	// Only the dirty state relative to HEAD shows up: the earlier commit's
	// addition is on both sides.
	mustContain(t, r.stdout, "  - func A()\n", "  + func Uncommitted()\n")
	mustNotContain(t, r.stdout, "func Committed()")
	if r.code != ExitIncompatible {
		t.Errorf("exit = %d, want %d", r.code, ExitIncompatible)
	}

	// Clean checkout: nothing changed.
	f.commit("three")
	r = f.run("", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.code != ExitClean {
		t.Errorf("exit = %d, want %d:\n%s", r.code, ExitClean, r.stdout)
	}
	mustNotContain(t, r.stdout, "Uncommitted", "removed")
}

func TestBadRevision(t *testing.T) {
	f := newFixture(t)
	f.write("a/a.go", "package a\n")
	f.commit("base")
	r := f.run("does-not-exist", "", Options{})
	if r.code != ExitError || r.err == nil {
		t.Fatalf("exit = %d, err = %v; want error", r.code, r.err)
	}
	mustContain(t, r.err.Error(), `resolve "does-not-exist"`)
}

func TestMissingGoMod(t *testing.T) {
	f := newFixture(t)
	f.remove("go.mod")
	f.write("a/a.go", "package a\n")
	f.commit("base")
	r := f.run("HEAD", "", Options{})
	if r.code != ExitError || r.err == nil {
		t.Fatalf("exit = %d, err = %v; want error", r.code, r.err)
	}
	mustContain(t, r.err.Error(), "go.mod", "GOPATH mode is not supported")
}

func TestUnresolvableImportIsFatal(t *testing.T) {
	f := newFixture(t)
	f.write("a/a.go", "package a\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nimport \"example.org/nothere\"\n\nvar X = nothere.X\n")
	r := f.run("HEAD", "", Options{})
	if r.code != ExitError || r.err == nil {
		t.Fatalf("exit = %d, err = %v; want error", r.code, r.err)
	}
	mustContain(t, r.err.Error(), `unresolvable import "example.org/nothere" (required by example.com/m/a)`)

	f.write("go.mod", "module example.com/m\n\ngo 1.24\n\nrequire example.org/nothere v1.2.3\n")
	r = f.run("HEAD", "", Options{})
	if r.code != ExitError || r.err == nil {
		t.Fatalf("exit = %d, err = %v; want error", r.code, r.err)
	}
	mustContain(t, r.err.Error(), "module example.org/nothere@v1.2.3 not in module cache (run go mod download)")
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
	r := f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q, want none", r.stderr)
	}
	want := "example.com/m/a\n  + func Changes(r apidiff.Report) []apidiff.Change\n  + func Describe(s fmt.Stringer) string\n\n1 package changed · 0 incompatible · 2 compatible · would require: MINOR\n"
	if r.stdout != want {
		t.Errorf("stdout = %q\nwant     %q", r.stdout, want)
	}
}

func TestReplaceDirectory(t *testing.T) {
	f := newFixture(t)
	f.write("go.mod", "module example.com/m\n\ngo 1.24\n\nrequire example.com/lib v0.0.0\n\nreplace example.com/lib => ./lib\n")
	f.write("lib/go.mod", "module example.com/lib\n\ngo 1.24\n")
	f.write("lib/lib.go", "package lib\n\ntype Conn struct{}\n")
	f.write("a/a.go", "package a\n\nimport \"example.com/lib\"\n\nfunc Open() lib.Conn { return lib.Conn{} }\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nimport \"example.com/lib\"\n\nfunc Open() lib.Conn { return lib.Conn{} }\n\nfunc Close(c lib.Conn) {}\n")

	r := f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q, want none", r.stderr)
	}
	want := "example.com/m/a\n  + func Close(c lib.Conn)\n\n1 package changed · 0 incompatible · 1 compatible · would require: MINOR\n"
	if r.stdout != want {
		t.Errorf("stdout = %q\nwant     %q", r.stdout, want)
	}

	// A type error in the replaced module is reported against the tree it
	// was read from, never as a synthetic mount path.
	f.write("lib/lib.go", "package lib\n\ntype Conn struct{}\n\nvar Broken undefinedType\n")
	f.commit("broken")
	r = f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if want := "warn: example.com/lib: HEAD:lib/lib.go:5:12: undefined: undefinedType\nwarn: example.com/lib: lib/lib.go:5:12: undefined: undefinedType\n"; r.stderr != want {
		t.Errorf("stderr = %q\nwant     %q", r.stderr, want)
	}
}

func TestReplaceVersion(t *testing.T) {
	f := newFixture(t)
	f.useFakeModcache()
	// Only the replacement is in the cache: resolving the required version
	// would fail.
	f.writeModule("example.com/q", "v1.1.0", map[string]string{"q.go": "package q\n\ntype T struct{}\n"})
	f.write("go.mod", "module example.com/m\n\ngo 1.24\n\nrequire example.com/q v1.0.0\n\nreplace example.com/q v1.0.0 => example.com/q v1.1.0\n")
	f.write("a/a.go", "package a\n\nimport \"example.com/q\"\n\nfunc A() q.T { return q.T{} }\n")
	f.commit("base")

	r := f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stderr != "" || r.stdout != "no exported API changes\n" {
		t.Errorf("stdout = %q, stderr = %q", r.stdout, r.stderr)
	}
}

// A nested module that go.mod requires is served from the module cache, as
// the go command does, not from its directory in the tree.
func TestRequiredNestedModuleComesFromModuleCache(t *testing.T) {
	f := newFixture(t)
	f.useFakeModcache()
	f.writeModule("example.com/m/sub", "v1.0.0", map[string]string{"sub.go": "package sub\n\nconst FromCache = 1\n"})
	f.write("go.mod", "module example.com/m\n\ngo 1.24\n\nrequire example.com/m/sub v1.0.0\n")
	f.write("sub/go.mod", "module example.com/m/sub\n\ngo 1.24\n")
	f.write("sub/sub.go", "package sub\n\nconst FromTree = 1\n")
	f.write("a/a.go", "package a\n\nimport \"example.com/m/sub\"\n\nconst A = sub.FromCache\n")
	f.commit("base")

	r := f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stderr != "" || r.stdout != "no exported API changes\n" {
		t.Errorf("stdout = %q, stderr = %q", r.stdout, r.stderr)
	}
}

// A replaced module may have a path without a dot, which otherwise marks
// the standard library.
func TestReplacedModuleWithoutDot(t *testing.T) {
	f := newFixture(t)
	f.write("go.mod", "module example.com/m\n\ngo 1.24\n\nrequire foo v0.0.0\n\nreplace foo => ./foo\n")
	f.write("foo/go.mod", "module foo\n\ngo 1.24\n")
	f.write("foo/bar/bar.go", "package bar\n\ntype Baz struct{}\n")
	f.write("a/a.go", "package a\n\nimport \"foo/bar\"\n\nfunc A() bar.Baz { return bar.Baz{} }\n")
	f.commit("base")

	r := f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stderr != "" || r.stdout != "no exported API changes\n" {
		t.Errorf("stdout = %q, stderr = %q", r.stdout, r.stderr)
	}
}

func TestSubdirectoryModule(t *testing.T) {
	f := newFixture(t)
	f.remove("go.mod")
	f.write("README", "not a go module at the root\n")
	f.write("sub/go.mod", "module example.com/sub\n\ngo 1.24\n")
	f.write("sub/a/a.go", "package a\n\nfunc A() {}\n")
	f.commit("base")
	f.write("sub/a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n")

	var out, errb bytes.Buffer
	opts := Options{Stdout: &out, Stderr: &errb}
	res, err := runRepo(f.open, sideSpec{rev: "HEAD"}, sideSpec{fs: vfs.NewBillyFS(f.fs)}, "sub", f.env, opts)
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
		{"all", Options{Filter: render.All}, ExitIncompatible},
		// Internal packages alone: no public API in the selection.
		{"internal", Options{Filter: render.Internal}, ExitClean},
		{"pos", Options{Positions: true}, ExitIncompatible},
		{"minimal", Options{Signatures: render.MinimalSignatures}, ExitIncompatible},
	} {
		t.Run(tc.name, func(t *testing.T) {
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

// snapshot hashes the names, sizes and mtimes of every file under root.
// Missing roots hash to the empty string.
func snapshot(t *testing.T, root string) (digest string, count int) {
	t.Helper()
	if root == "" {
		return "", 0
	}
	if _, err := os.Stat(root); err != nil {
		return "", 0
	}
	h := sha256.New()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are not something we could have written
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		count++
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00%d\n", p, info.Mode(), info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil)), count
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
// itself are untouched. It must run alone in its test binary: another go
// process compiling concurrently would legitimately write to GOCACHE.
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
	before := map[string][2]any{}
	for name, root := range roots {
		d, n := snapshot(t, root)
		before[name] = [2]any{d, n}
	}

	var out, errb bytes.Buffer
	code, err := Run(Options{
		Repo:   repoDir,
		Base:   "HEAD",
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

	for name, root := range roots {
		d, n := snapshot(t, root)
		if b := before[name]; b[0] != d || b[1] != n {
			t.Errorf("%s (%s) changed during the run: %d files before, %d after", name, root, b[1], n)
		}
	}
}

func TestExitFail(t *testing.T) {
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
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.write("plugin/plugin.go", "package plugin\n\nfunc init() {}\n")
	f.commit("base")
	f.remove("plugin/plugin.go")
	f.write("other/other.go", "package other\n\nfunc init() {}\n")

	r := f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
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
	r = f.run("HEAD", "", Options{Breaking: true})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "example.com/m/plugin (removed)\n  - package removed\n")
	mustNotContain(t, r.stdout, "package added")
}

func TestConstantValueChange(t *testing.T) {
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nconst Version = \"1.2.0\"\n\nconst Limit int64 = 10\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nconst Version = \"1.3.0-dev\"\n\nconst Limit int64 = 20\n")

	r := f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout,
		"  - const Limit int64 = 10\n  + const Limit int64 = 20\n",
		"  - const Version untyped string = \"1.2.0\"\n  + const Version untyped string = \"1.3.0-dev\"\n")
}

// A dependency that imports the main module (grpc-go and go-control-plane
// import each other, for instance) must be linked against each side's own
// main-module packages. Sharing one checked copy between the sides makes
// the other side see two distinct core.Client types and report bogus type
// errors, even when both sides are identical.
func TestDependencyImportingMainModuleIsNotShared(t *testing.T) {
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
		r := f.run("HEAD", "", Options{})
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.stderr != "" {
			t.Fatalf("run %d: stderr = %q, want none", i, r.stderr)
		}
		if r.stdout != "no exported API changes\n" {
			t.Errorf("run %d: stdout = %q", i, r.stdout)
		}
	}

	// The same holds for two committed revisions of the same tree.
	r := f.run("HEAD", "HEAD", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q, want none", r.stderr)
	}
}

// When the two go.mod files pin a transitive dependency to different
// versions, a package that imports it (same directory on both sides) must be
// checked once per side; otherwise one side links it against the other
// side's version of the dependency.
func TestDependencyPinnedToDifferentVersionsIsNotShared(t *testing.T) {
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
		r := f.run("HEAD", "", Options{})
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.stderr != "" {
			t.Fatalf("run %d: stderr = %q, want none", i, r.stderr)
		}
		if r.stdout != "no exported API changes\n" {
			t.Errorf("run %d: stdout = %q", i, r.stdout)
		}
	}
}

func TestLatestRelease(t *testing.T) {
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

	r := f.run(LatestRelease, "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
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
	r = f.run(LatestRelease, "HEAD", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
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
	r = f.run("v0.3.0", "HEAD", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if want := "no exported API changes since v0.3.0\n"; r.stdout != want {
		t.Errorf("stdout = %q, want %q", r.stdout, want)
	}

	// Breaking changes since a v0 release bump the minor version.
	f.write("a/a.go", "package a\n\nfunc B() {}\n\nfunc C() {}\n")
	r = f.run(LatestRelease, "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "  - func A()\n", "would require: MAJOR (v0.2.0 → v0.3.0)\n")

	// @latest is only meaningful as the base.
	r = f.run("HEAD", LatestRelease, Options{})
	if r.code != ExitError || r.err == nil {
		t.Fatalf("exit = %d, err = %v; want error", r.code, r.err)
	}
	mustContain(t, r.err.Error(), "@latest can only be the base revision")
}

func TestLatestReleasePrerelease(t *testing.T) {
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.tag("v0.9.0", f.commit("one"))
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n")
	f.tag("v1.0.0-rc.1", f.commit("two"))
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n\nfunc C() {}\n")
	f.commit("three")

	r := f.run(LatestRelease, "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "  + func C()\n", "would require: MINOR (v1.0.0-rc.1 → v1.0.0)\n")
	mustNotContain(t, r.stdout, "func B()")

	f.write("a/a.go", "package a\n\nfunc B() {}\n\nfunc C() {}\n")
	r = f.run(LatestRelease, "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "  - func A()\n", "would require: MAJOR (v1.0.0-rc.1 → v1.0.0)\n")
}

func TestLatestReleaseMajorSuffix(t *testing.T) {
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

	r := f.run(LatestRelease, "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "  - func A()\n", "  + func C()\n", "would require: MAJOR (v2.0.0 → v3.0.0)\n")
	mustNotContain(t, r.stdout, "func B()")
}

func TestLatestReleaseSubdirectoryModule(t *testing.T) {
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
	res, err := runRepo(f.open, sideSpec{rev: LatestRelease}, sideSpec{fs: vfs.NewBillyFS(f.fs)}, "sub", f.env, opts)
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
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	one := f.commit("one")
	r := f.run(LatestRelease, "", Options{})
	if r.code != ExitError || r.err == nil {
		t.Fatalf("exit = %d, err = %v; want error", r.code, r.err)
	}
	mustContain(t, r.err.Error(), `@latest: no release tags for example.com/m (looking for tags like "v1.2.3")`)

	// Tags exist, but none among the ancestors of HEAD: the release on a
	// side branch, and the tag on HEAD itself.
	f.checkout("side", true, one)
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc Side() {}\n")
	f.tag("v0.1.0", f.commit("side"))
	f.checkout("master", false, plumbing.ZeroHash)
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n")
	f.tag("v0.2.0", f.commit("two"))
	r = f.run(LatestRelease, "", Options{})
	if r.code != ExitError || r.err == nil {
		t.Fatalf("exit = %d, err = %v; want error", r.code, r.err)
	}
	mustContain(t, r.err.Error(), "@latest: none of the 2 release tag(s) for example.com/m is an ancestor of HEAD")
}

func TestJSON(t *testing.T) {
	f := goldenFixture(t)
	r := f.run("v1.0.0", "", Options{Format: render.JSON})
	if r.err != nil {
		t.Fatal(r.err)
	}
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
	r = f.run("v1.0.0", "", Options{Format: render.JSON, Breaking: true})
	if r.err != nil {
		t.Fatal(r.err)
	}
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
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.write("plugin/plugin.go", "package plugin\n\nfunc init() {}\n")
	f.commit("base")
	f.remove("plugin/plugin.go")
	r := f.run("HEAD", "", Options{Format: render.JSON})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, `"symbol": "",`, `"kind": "removed",`, `"message": "package removed"`)
}

func TestFormatsWithoutChanges(t *testing.T) {
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.commit("base")
	r := f.run("HEAD", "", Options{Format: render.Markdown})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if want := "_no exported API changes_\n"; r.stdout != want {
		t.Errorf("markdown = %q, want %q", r.stdout, want)
	}
	r = f.run("HEAD", "", Options{Format: render.JSON})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, `"base": "HEAD"`, `"packages": [],`, `"release": "patch"`)
	mustNotContain(t, r.stdout, "base_version", "next_version")
}

func TestParseFormat(t *testing.T) {
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
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc Keep() {}\n\nfunc Drop() {}\n\ntype T struct {\n\tX int\n}\n\nfunc (t T) M(n int) {}\n")
	h := f.commit("base")
	f.tag("v0.1.0", h)
	f.write("a/a.go", "package a\n\nfunc Keep() {}\n\ntype T struct {\n\tX int64\n\tY int\n}\n\nfunc (t T) M(n int64) {}\n\nfunc Added() {}\n")
	f.write("b/b.go", "package b\n\nfunc init() {}\n")

	r := f.run("v0.1.0", "", Options{Positions: true})
	if r.err != nil {
		t.Fatal(r.err)
	}
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
	r = f.run("v0.1.0", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "  - func Drop()\n  - func (t T) M(n int)\n  + func (t T) M(n int64)\n")
	mustNotContain(t, r.stdout, "a.go")

	// Two committed revisions: both sides carry a revision prefix.
	f.commit("head")
	r = f.run("v0.1.0", "HEAD", Options{Positions: true})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "  - func Drop()            v0.1.0:a/a.go:5:6\n", "  + func Added()           HEAD:a/a.go:12:6\n")
}

func TestPackageFilter(t *testing.T) {
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
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	f.write("internal/hidden/h.go", "package hidden\n\nfunc Hidden() {}\n")
	f.write("a/internal/deep/d.go", "package deep\n\nfunc Deep() {}\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nfunc A() {}\n\nfunc Added() {}\n")
	f.write("internal/hidden/h.go", "package hidden\n\nfunc Renamed() {}\n")
	f.write("a/internal/deep/d.go", "package deep\n\nfunc Deep() {}\n\nfunc Deeper() {}\n")
	f.write("internal/fresh/f.go", "package fresh\n\nfunc F() {}\n")

	// Without the option internal packages do not exist.
	r := f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if want := "example.com/m/a\n  + func Added()\n\n1 package changed · 0 incompatible · 1 compatible · would require: MINOR\n"; r.stdout != want {
		t.Errorf("stdout = %q\nwant     %q", r.stdout, want)
	}

	// With --filter=all they are shown and marked, but the summary, the
	// release and the exit code still describe the public API only.
	r = f.run("HEAD", "", Options{Filter: render.All})
	if r.err != nil {
		t.Fatal(r.err)
	}
	want := "example.com/m/a\n  + func Added()\n\n" +
		"example.com/m/a/internal/deep (internal)\n  + func Deeper()\n\n" +
		"example.com/m/internal/fresh (internal, new)\n  + func F()\n\n" +
		"example.com/m/internal/hidden (internal)\n  - func Hidden()\n  + func Renamed()\n\n" +
		"1 package changed · 0 incompatible · 1 compatible · would require: MINOR\n" +
		"internal: 3 packages changed · 1 incompatible · 3 compatible\n"
	if r.stdout != want {
		t.Errorf("stdout = %q\nwant     %q", r.stdout, want)
	}
	if r.code != ExitClean {
		t.Errorf("exit = %d, want %d", r.code, ExitClean)
	}
	r = f.run("HEAD", "", Options{Filter: render.All, ExitFail: FailMajor})
	if r.code != ExitClean {
		t.Errorf("--exit-fail=major: exit = %d, want %d", r.code, ExitClean)
	}

	// Only internal changes: the public API is untouched.
	f.write("a/a.go", "package a\n\nfunc A() {}\n")
	r = f.run("HEAD", "", Options{Filter: render.All})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "\nno exported API changes\ninternal: 3 packages changed · 1 incompatible · 3 compatible\n")
	if r.code != ExitClean {
		t.Errorf("exit = %d, want %d", r.code, ExitClean)
	}

	// --breaking and --pkg apply to internal packages too.
	r = f.run("HEAD", "", Options{Filter: render.All, Breaking: true})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if want := "example.com/m/internal/hidden (internal)\n  - func Hidden()\n\nno exported API changes\ninternal: 3 packages changed · 1 incompatible · 3 compatible\n"; r.stdout != want {
		t.Errorf("breaking: stdout = %q\nwant     %q", r.stdout, want)
	}
	r = f.run("HEAD", "", Options{Filter: render.All, Packages: []string{"internal/hidden"}})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if want := "example.com/m/internal/hidden (internal)\n  - func Hidden()\n  + func Renamed()\n\nno exported API changes\ninternal: 1 package changed · 1 incompatible · 1 compatible\n"; r.stdout != want {
		t.Errorf("pkg: stdout = %q\nwant     %q", r.stdout, want)
	}

	// --filter=internal shows internal packages alone, with only their
	// summary line; there is no public API in the selection, so the exit
	// code is the clean one.
	r = f.run("HEAD", "", Options{Filter: render.Internal, ExitFail: FailMajor})
	if r.err != nil {
		t.Fatal(r.err)
	}
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
	r = f.run("HEAD", "", Options{Filter: render.Internal, Packages: []string{"a"}})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if want := "warn: example.com/m: --pkg \"a\" matched no packages\n"; r.stderr != want {
		t.Errorf("internal --pkg a: stderr = %q, want %q", r.stderr, want)
	}
	if want := "internal: no changes\n"; r.stdout != want {
		t.Errorf("internal --pkg a: stdout = %q, want %q", r.stdout, want)
	}

	// Nothing changed anywhere.
	f.commit("all")
	r = f.run("HEAD", "", Options{Filter: render.All})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if want := "no exported API changes\ninternal: no changes\n"; r.stdout != want {
		t.Errorf("clean: stdout = %q\nwant     %q", r.stdout, want)
	}
	r = f.run("HEAD", "", Options{Filter: render.Internal})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if want := "internal: no changes\n"; r.stdout != want {
		t.Errorf("clean internal: stdout = %q\nwant     %q", r.stdout, want)
	}

	// Machine-readable layouts carry the same split.
	f.write("internal/hidden/h.go", "package hidden\n\nfunc Renamed() {}\n\nfunc More() {}\n")
	r = f.run("HEAD", "", Options{Filter: render.All, Format: render.JSON})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, `"internal": true,`, `"release": "patch",`,
		"\"internal\": {\n      \"packages_changed\": 1,\n      \"incompatible\": 0,\n      \"compatible\": 1\n    }")
	r = f.run("HEAD", "", Options{Filter: render.All, Format: render.Markdown})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "**example.com/m/internal/hidden (internal)**\n", "_no exported API changes_\n\n_internal: 1 package changed · 0 incompatible · 1 compatible_\n")
	r = f.run("HEAD", "", Options{Filter: render.Internal, Format: render.Markdown})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "```\n\ninternal: 1 package changed · 0 incompatible · 1 compatible\n")
	mustNotContain(t, r.stdout, "exported API")
	r = f.run("HEAD", "", Options{Filter: render.Internal, Format: render.JSON})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, `"packages_changed": 0,`, `"release": "patch",`, "\"internal\": {\n      \"packages_changed\": 1,")
}

func TestMinimalSignatures(t *testing.T) {
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc Keep() {}\n\nfunc Drop() {}\n\nfunc Open(name string) error { return nil }\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\nfunc Keep() {}\n\nfunc Open(name string, n int) error { return nil }\n\nfunc Added() {}\n")

	// One line per change, with apidiff's message as is.
	r := f.run("HEAD", "", Options{Signatures: render.MinimalSignatures})
	if r.err != nil {
		t.Fatal(r.err)
	}
	want := "example.com/m/a\n" +
		"  - Drop: removed\n" +
		"  ~ Open: changed from func(string) error to func(string, int) error\n" +
		"  + Added: added\n" +
		"\n1 package changed · 2 incompatible · 1 compatible · would require: MAJOR\n"
	if r.stdout != want {
		t.Errorf("stdout = %q\nwant     %q", r.stdout, want)
	}

	// The default shows every declaration.
	r = f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	want = "example.com/m/a\n" +
		"  - func Drop()\n" +
		"  - func Open(name string) error\n  + func Open(name string, n int) error\n" +
		"  + func Added()\n" +
		"\n1 package changed · 2 incompatible · 1 compatible · would require: MAJOR\n"
	if r.stdout != want {
		t.Errorf("full: stdout = %q\nwant     %q", r.stdout, want)
	}

	// Markdown and JSON follow the same knob.
	r = f.run("HEAD", "", Options{Signatures: render.MinimalSignatures, Format: render.Markdown})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "```diff\n- Drop: removed\n! Open: changed from func(string) error to func(string, int) error\n+ Added: added\n```\n")
	r = f.run("HEAD", "", Options{Signatures: render.MinimalSignatures, Format: render.JSON})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustNotContain(t, r.stdout, `"before"`, `"after"`)
	r = f.run("HEAD", "", Options{Format: render.JSON})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, `"before": "func Drop()"`, `"after": "func Added()"`, `"before": "func Open(name string) error"`)
}

func TestParseFilter(t *testing.T) {
	for in, want := range map[string]render.Visibility{"public": render.Public, "internal": render.Internal, "ALL": render.All} {
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

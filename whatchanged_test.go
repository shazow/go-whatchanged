package whatchanged

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	if dir := path.Dir(name); dir != "." {
		if err := f.fs.MkdirAll(dir, 0o755); err != nil {
			f.t.Fatal(err)
		}
	}
	file, err := f.fs.Create(name)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := file.Write([]byte(content)); err != nil {
		f.t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		f.t.Fatal(err)
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
	sig := &object.Signature{Name: "test", Email: "test@example.com", When: time.Unix(0, 0)}
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

type runResult struct {
	stdout, stderr string
	code           int
	err            error
}

// run diffs base against the fixture's in-memory worktree (or head, if set).
func (f *fixture) run(base, head string, opts Options) runResult {
	f.t.Helper()
	var out, errb bytes.Buffer
	opts.Stdout = &out
	opts.Stderr = &errb
	opts.Base = base
	headSpec := sideSpec{fs: vfs.NewBillyFS(f.fs)}
	if head != "" {
		headSpec = sideSpec{rev: head}
	}
	res, err := runRepo(f.repo, sideSpec{rev: base}, headSpec, "", f.env, opts)
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
		"example.com/m/fresh (new)\n  + Hello: added\n",
		"example.com/m/old (removed)\n  - Gone: removed\n  - T: removed\n",
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
	mustContain(t, r.stdout, "example.com/m/a\n  - Drop: removed\n", "would require: MAJOR")
	mustNotContain(t, r.stdout, "Keep")
	if r.code != ExitIncompatible {
		t.Errorf("exit = %d", r.code)
	}
}

func TestChangedSignature(t *testing.T) {
	f := newFixture(t)
	f.write("a/a.go", "package a\n\nfunc Open(name string) error { return nil }\n")
	f.commit("base")
	f.write("a/a.go", "package a\n\ntype Options struct{}\n\nfunc Open(name string, o Options) error { return nil }\n")
	r := f.run("HEAD", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout,
		"  ~ Open: changed from func(string) error to func(string, Options) error\n",
		"  + Options: added\n",
		"1 package changed · 1 incompatible · 1 compatible · would require: MAJOR\n")
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
	mustContain(t, r.stdout, "  + Point.Z: added\n", "would require: MINOR")
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
	mustContain(t, r.stdout, "  ! Sizer.Size: added\n", "would require: MAJOR")
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
	mustContain(t, r.stdout, "  + WindowsOnly: added\n")
	mustNotContain(t, r.stdout, "Plan9Only")

	r = f.run("HEAD", "", Options{GOOS: "plan9", GOARCH: "amd64"})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "  + Plan9Only: added\n")
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
	mustContain(t, r.stdout, "  + B: added\n", "  + Broken: added\n")
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
	mustContain(t, r.stdout, "  ~ Broken: changed from invalid type to int\n")
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
	want := "example.com/m/a\n  - Drop: removed\n\n2 packages changed · 1 incompatible · 2 compatible · would require: MAJOR\n"
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
	mustContain(t, r.stdout, "  + B: added\n")
	mustNotContain(t, r.stdout, "Uncommitted")

	r = f.run("HEAD~1", "", Options{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.stdout, "  + B: added\n", "  + Uncommitted: added\n")
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

// ownDeps reads this project's go.mod to find module versions that are
// guaranteed to be present in the module cache.
func ownDeps(t *testing.T, paths ...string) map[string]string {
	t.Helper()
	data, err := os.ReadFile("go.mod")
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
	want := "example.com/m/a\n  + Changes: added\n  + Describe: added\n\n1 package changed · 0 incompatible · 2 compatible · would require: MINOR\n"
	if r.stdout != want {
		t.Errorf("stdout = %q\nwant     %q", r.stdout, want)
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
	res, err := runRepo(f.repo, sideSpec{rev: "HEAD"}, sideSpec{fs: vfs.NewBillyFS(f.fs)}, "sub", f.env, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := finish(res, opts); err != nil {
		t.Fatal(err)
	}
	mustContain(t, out.String(), "example.com/sub/a\n  + B: added\n")
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

const Version = "1"

var Default = Open
`)
	f.write("util/util.go", "package util\n\ntype Sizer interface{ Len() int }\n\ntype Stringer interface{ String() string }\n")
	f.write("gone/gone.go", "package gone\n\nfunc Gone() {}\n")
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

const Version = 1

var Default func(string) (*Client, error)
`)
	f.write("util/util.go", "package util\n\nimport \"fmt\"\n\ntype Sizer interface {\n\tLen() int\n\tSize() int\n}\n\ntype Stringer = fmt.Stringer\n")
	f.remove("gone/gone.go")
	f.write("fresh/fresh.go", "package fresh\n\nfunc Hello() {}\n")
	return f
}

func TestGolden(t *testing.T) {
	f := goldenFixture(t)
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"nocolor", Options{}},
		{"color", Options{Color: true}},
		{"breaking", Options{Breaking: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := f.run("v1.0.0", "", tc.opts)
			if r.err != nil {
				t.Fatal(r.err)
			}
			if r.code != ExitIncompatible {
				t.Errorf("exit = %d", r.code)
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

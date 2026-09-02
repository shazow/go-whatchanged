// Package whatchanged computes a colorized diff of a Go module's exported API
// between a git commit and the working tree (or another commit), without
// writing to disk or invoking the go command.
package whatchanged

import (
	"errors"
	"fmt"
	"go/build"
	"go/token"
	"go/types"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/exp/apidiff"

	"github.com/shazow/go-whatchanged/internal/discover"
	"github.com/shazow/go-whatchanged/internal/loader"
	"github.com/shazow/go-whatchanged/internal/modres"
	"github.com/shazow/go-whatchanged/internal/render"
	"github.com/shazow/go-whatchanged/internal/vfs"
)

// Options configures a run.
type Options struct {
	// Repo is a path inside the git repository. Empty means the current
	// directory. The enclosing .git is found by walking upward.
	Repo string
	// Base is the commit-ish for the old side.
	Base string
	// Head is the commit-ish for the new side. Empty means the working tree.
	Head string
	// GOOS and GOARCH select the build target. Empty means the runtime values.
	GOOS, GOARCH string
	// Breaking hides compatible changes.
	Breaking bool
	// Color enables ANSI escapes.
	Color bool
	// Strict turns type-check warnings into a fatal error.
	Strict bool

	// Stdout receives the diff; Stderr receives warnings.
	Stdout, Stderr io.Writer
}

// Exit codes.
const (
	ExitClean        = 0
	ExitIncompatible = 1
	ExitError        = 2
)

// Run executes a diff and returns the process exit code. Errors are returned
// alongside ExitError; the caller decides how to print them.
func Run(opts Options) (int, error) {
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.Base == "" {
		return ExitError, errors.New("a base commit-ish is required")
	}

	dir := opts.Repo
	if dir == "" {
		dir = "."
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return ExitError, err
	}
	gitRoot, err := findGitRoot(dir)
	if err != nil {
		return ExitError, err
	}
	modRoot, err := findModRoot(dir, gitRoot)
	if err != nil {
		return ExitError, err
	}
	rel, err := filepath.Rel(gitRoot, modRoot)
	if err != nil {
		return ExitError, err
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		rel = ""
	}

	repo, err := git.PlainOpen(gitRoot)
	if err != nil {
		return ExitError, fmt.Errorf("open repository %s: %w", gitRoot, err)
	}

	env, err := modres.DefaultEnv()
	if err != nil {
		return ExitError, err
	}

	var head sideSpec
	if opts.Head != "" {
		head = sideSpec{rev: opts.Head}
	} else {
		head = sideSpec{dir: modRoot}
	}
	res, err := runRepo(repo, sideSpec{rev: opts.Base}, head, rel, env, opts)
	if err != nil {
		return ExitError, err
	}
	return finish(res, opts)
}

// finish prints warnings and the diff, and derives the exit code.
func finish(res *render.Result, opts Options) (int, error) {
	if len(res.Warnings) > 0 {
		if err := render.WriteWarnings(opts.Stderr, res.Warnings, opts.Color); err != nil {
			return ExitError, err
		}
		if opts.Strict {
			return ExitError, fmt.Errorf("%d type-check warning(s) (--strict)", len(res.Warnings))
		}
	}
	if err := render.Write(opts.Stdout, *res, render.Options{Color: opts.Color, BreakingOnly: opts.Breaking}); err != nil {
		return ExitError, err
	}
	if render.Summarize(*res).Incompatible > 0 {
		return ExitIncompatible, nil
	}
	return ExitClean, nil
}

// sideSpec names one side: a git revision, or a directory served either from
// disk (fs == nil) or from an arbitrary read-only filesystem (tests).
type sideSpec struct {
	rev string
	dir string
	fs  vfs.FS
}

// side is a fully loaded side.
type side struct {
	rev     string // git revision, empty for a directory side
	label   string
	prefix  string // path prefix rewritten to label+":" in messages
	overlay *vfs.Overlay
	ctxt    build.Context
	res     *modres.Resolver
	ld      *loader.Loader
	pkgs    map[string]*types.Package
	problem map[string]string
}

// runRepo diffs base against head. rel is the module root relative to the
// repository root (slash-separated, "" for the root).
func runRepo(repo *git.Repository, base, head sideSpec, rel string, env modres.Env, opts Options) (*render.Result, error) {
	fset := token.NewFileSet()
	shared := loader.NewSharedCache()
	goos, goarch := opts.GOOS, opts.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}

	var (
		wg    sync.WaitGroup
		sides [2]*side
		errs  [2]error
	)
	specs := [2]sideSpec{base, head}
	for i := range specs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sides[i], errs[i] = loadSide(repo, specs[i], rel, env, goos, goarch, fset, shared)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return diffSides(sides[0], sides[1])
}

func loadSide(repo *git.Repository, spec sideSpec, rel string, env modres.Env, goos, goarch string, fset *token.FileSet, shared *loader.SharedCache) (*side, error) {
	s := &side{rev: spec.rev}
	var root string
	switch {
	case spec.rev != "":
		tree, err := resolveTree(repo, spec.rev)
		if err != nil {
			return nil, err
		}
		s.label = spec.rev
		mount := vfs.GitMountPath(tree)
		s.overlay = vfs.NewOverlay(vfs.Mount{Path: mount, FS: vfs.NewGitFS(tree)})
		root = path.Join(mount, rel)
		s.prefix = root + "/"
	case spec.fs != nil:
		s.label = "working tree"
		mount := vfs.SyntheticPrefix + "worktree"
		s.overlay = vfs.NewOverlay(vfs.Mount{Path: mount, FS: spec.fs})
		root = path.Join(mount, rel)
		s.prefix = root + "/"
	default:
		s.label = "working tree"
		s.overlay = vfs.NewOverlay()
		root = spec.dir
		s.prefix = root + string(filepath.Separator)
	}
	s.ctxt = vfs.Context(s.overlay, goos, goarch)

	res, err := modres.New(s.overlay, root, env)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.label, err)
	}
	s.res = res
	s.ld = loader.New(s.ctxt, fset, res, shared)

	found, problems, err := discover.Packages(&s.ctxt, s.overlay, root, res.ModPath())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.label, err)
	}
	s.problem = problems
	s.pkgs = make(map[string]*types.Package, len(found))
	paths := make([]string, 0, len(found))
	for p := range found {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		pkg, err := s.ld.Load(p, found[p].Dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.label, err)
		}
		s.pkgs[p] = pkg
	}
	if err := s.ld.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", s.label, err)
	}
	return s, nil
}

func resolveTree(repo *git.Repository, rev string) (*object.Tree, error) {
	hash, err := repo.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", rev, err)
	}
	commit, err := repo.CommitObject(*hash)
	if err != nil {
		// Allow tree-ish objects that are not commits (e.g. a raw tree hash).
		if tree, terr := repo.TreeObject(*hash); terr == nil {
			return tree, nil
		}
		return nil, fmt.Errorf("resolve %q: %s is not a commit: %w", rev, hash, err)
	}
	return commit.Tree()
}

func diffSides(base, head *side) (*render.Result, error) {
	union := map[string]bool{}
	for p := range base.pkgs {
		union[p] = true
	}
	for p := range head.pkgs {
		union[p] = true
	}
	paths := make([]string, 0, len(union))
	for p := range union {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	res := &render.Result{}
	for _, p := range paths {
		old, inBase := base.pkgs[p]
		nw, inHead := head.pkgs[p]
		pkg := render.Package{Path: p}
		switch {
		case inBase && inHead:
			pkg.Status = render.Both
		case inHead:
			pkg.Status = render.New
			old = types.NewPackage(p, nw.Name())
		default:
			pkg.Status = render.Removed
			nw = types.NewPackage(p, old.Name())
		}
		for _, c := range apidiff.Changes(old, nw).Changes {
			rc := render.FromAPIDiff(c)
			rc.Before, rc.After = namedForms(old, nw, c.Message)
			pkg.Changes = append(pkg.Changes, rc)
		}
		res.Packages = append(res.Packages, pkg)
	}

	res.Warnings = collectWarnings(base, head)
	return res, nil
}

// collectWarnings merges both sides' warnings, rewriting synthetic mount
// paths into "<rev>:" prefixes so positions read like git paths.
func collectWarnings(sides ...*side) []render.Warning {
	type key struct{ pkg, msg string }
	seen := map[key]bool{}
	var out []render.Warning
	add := func(pkg, msg string) {
		k := key{pkg, msg}
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, render.Warning{Package: pkg, Message: msg})
	}
	for _, s := range sides {
		rewrite := func(msg string) string {
			if s.rev == "" {
				// Working tree: positions become module-relative, like git diff.
				return strings.ReplaceAll(msg, s.prefix, "")
			}
			return strings.ReplaceAll(msg, s.prefix, s.label+":")
		}
		pkgs := make([]string, 0)
		for p := range s.problem {
			pkgs = append(pkgs, p)
		}
		sort.Strings(pkgs)
		for _, p := range pkgs {
			add(p, rewrite(s.problem[p]))
		}
		warnings := s.ld.Warnings()
		pkgs = pkgs[:0]
		for p := range warnings {
			pkgs = append(pkgs, p)
		}
		sort.Strings(pkgs)
		for _, p := range pkgs {
			for _, m := range warnings[p] {
				add(p, rewrite(m))
			}
		}
	}
	return out
}

// findGitRoot walks upward from dir until it finds a .git entry.
func findGitRoot(dir string) (string, error) {
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("%s is not inside a git repository", dir)
		}
		d = parent
	}
}

// findModRoot walks upward from dir (not past gitRoot) to the nearest go.mod.
func findModRoot(dir, gitRoot string) (string, error) {
	for d := dir; ; {
		if fi, err := os.Stat(filepath.Join(d, "go.mod")); err == nil && !fi.IsDir() {
			return d, nil
		}
		if d == gitRoot {
			return "", fmt.Errorf("no go.mod found between %s and the repository root %s", dir, gitRoot)
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("no go.mod found above %s", dir)
		}
		d = parent
	}
}

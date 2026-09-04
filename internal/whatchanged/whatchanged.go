// Package whatchanged computes a semantic diff of a Go module's exported API
// between two git revisions, or a revision and the working tree, without
// writing to disk or invoking the go command, except to fetch modules the
// module cache lacks through Options.Fetch.
package whatchanged

import (
	"context"
	"errors"
	"fmt"
	"go/token"
	"go/types"
	"go/version"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/exp/apidiff"
	"golang.org/x/mod/module"

	"github.com/shazow/go-whatchanged/internal/discover"
	"github.com/shazow/go-whatchanged/internal/loader"
	"github.com/shazow/go-whatchanged/internal/modfetch"
	"github.com/shazow/go-whatchanged/internal/modres"
	"github.com/shazow/go-whatchanged/internal/release"
	"github.com/shazow/go-whatchanged/internal/render"
	"github.com/shazow/go-whatchanged/internal/vfs"
)

// Options configures a run.
type Options struct {
	// Repo is a path inside the git repository. Empty means the current
	// directory. The enclosing .git is found by walking upward.
	Repo string
	// Base is the commit-ish for the old side. Empty means HEAD, so the
	// zero value diffs the last commit against the working tree.
	Base string
	// Head is the commit-ish for the new side. Empty means the working tree.
	Head string
	// GOOS and GOARCH select the build target. Empty means the runtime values.
	GOOS, GOARCH string
	// Breaking hides compatible changes.
	Breaking bool
	// Packages restricts the diff to packages matching one of these
	// patterns (see discover.Filter); Exclude removes matching packages.
	Packages, Exclude []string
	// Filter selects which packages take part: any combination of the
	// public ones, the internal ones and the main packages (commands); the
	// zero value selects all of them. Internal and main packages are listed
	// and marked, in sections of their own below the public API, but never
	// count towards the public API's summary, the required release or the
	// exit code.
	Filter render.Visibility
	// Positions annotates each change with the position of its declaration.
	Positions bool
	// Width is the number of columns the text layout may use, 0 for no
	// limit; see render.Options.Width.
	Width int
	// Signatures selects whether each change shows the symbol's
	// declaration; the zero value shows it in full.
	Signatures render.Signatures
	// Color enables ANSI escapes.
	Color bool
	// Strict turns type-check warnings into a fatal error.
	Strict bool
	// ExitFail selects the required release levels that make Run exit
	// non-zero, with a code naming the level (see ExitMajor and friends).
	// The zero value, FailNever, keeps the default of ExitIncompatible for
	// incompatible changes.
	ExitFail FailOn
	// Format selects the output layout; the zero value is the colorized
	// text layout.
	Format render.Format
	// Fetch obtains modules that go.mod requires but the module cache
	// lacks. Nil keeps the run read-only: such a module is an error whose
	// message says how to get it.
	Fetch modfetch.Source

	// Stdout receives the diff; Stderr receives warnings.
	Stdout, Stderr io.Writer
}

// Exit codes.
const (
	ExitClean        = 0
	ExitIncompatible = 1
	ExitError        = 2

	// With Options.ExitFail set, the exit code names the release bump the
	// changes require when it reaches the selected threshold.
	ExitMajor = 100
	ExitMinor = 101
	ExitPatch = 102
)

// FailOn is the threshold for Options.ExitFail: the lowest required release
// level that makes Run exit non-zero.
type FailOn int

const (
	// FailNever disables level-based exit codes: Run exits ExitIncompatible
	// on incompatible changes and ExitClean otherwise.
	FailNever FailOn = iota
	// FailMajor exits ExitMajor on incompatible changes.
	FailMajor
	// FailMinor exits ExitMajor on incompatible changes and ExitMinor on
	// compatible ones.
	FailMinor
	// FailPatch always exits non-zero: ExitMajor, ExitMinor or ExitPatch,
	// so the exit code reports the required bump.
	FailPatch
)

// ParseFailOn parses an --exit-fail value: "major", "minor" or "patch".
func ParseFailOn(s string) (FailOn, error) {
	lvl, err := render.ParseLevel(s)
	if err != nil {
		return FailNever, err
	}
	switch lvl {
	case render.Major:
		return FailMajor, nil
	case render.Minor:
		return FailMinor, nil
	default:
		return FailPatch, nil
	}
}

// ParseFormat parses a --format value: "text", "markdown" (or "md") or "json".
func ParseFormat(s string) (render.Format, error) {
	return render.ParseFormat(s)
}

// ParseSignatures parses a --signatures value: "full" or "minimal".
func ParseSignatures(s string) (render.Signatures, error) {
	return render.ParseSignatures(s)
}

// ParseFilter parses one --filter term: "public", "internal", "main" or
// "all".
func ParseFilter(s string) (render.Visibility, error) {
	return render.ParseVisibility(s)
}

// threshold is the lowest level that fails, or ok=false for FailNever.
func (f FailOn) threshold() (floor render.Level, ok bool) {
	switch f {
	case FailMajor:
		return render.Major, true
	case FailMinor:
		return render.Minor, true
	case FailPatch:
		return render.Patch, true
	default:
		return 0, false
	}
}

// Run executes a diff and returns the process exit code. Errors are returned
// alongside ExitError; the caller decides how to print them.
func Run(opts Options) (int, error) {
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	base := opts.baseRev()

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

	open := func() (*git.Repository, error) {
		// EnableDotGitCommonDir makes linked worktrees (git worktree add)
		// work: their .git file points at a per-worktree directory whose
		// objects and refs live in the main repository.
		repo, err := git.PlainOpenWithOptions(gitRoot, &git.PlainOpenOptions{EnableDotGitCommonDir: true})
		if err != nil {
			return nil, fmt.Errorf("open repository %s: %w", gitRoot, err)
		}
		return repo, nil
	}
	// Report a broken repository up front rather than from inside a side.
	if _, err := open(); err != nil {
		return ExitError, err
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
	res, err := runRepo(context.Background(), open, sideSpec{rev: base}, head, rel, env, opts)
	if err != nil {
		return ExitError, err
	}
	return finish(res, opts)
}

// DefaultBase is the revision used for the old side when none is given.
const DefaultBase = "HEAD"

// baseRev returns the old-side revision, applying DefaultBase.
func (o Options) baseRev() string {
	if o.Base == "" {
		return DefaultBase
	}
	return o.Base
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
	ro := render.Options{
		Color:        opts.Color,
		BreakingOnly: opts.Breaking,
		Format:       opts.Format,
		Signatures:   opts.Signatures,
		Positions:    opts.Positions,
		Width:        opts.Width,
		Filter:       opts.Filter,
	}
	if err := render.Write(opts.Stdout, *res, ro); err != nil {
		return ExitError, err
	}
	return exitCode(render.Summarize(*res), opts.ExitFail), nil
}

// exitCode derives the exit code from the summary and the --exit-fail
// threshold.
func exitCode(sum render.Summary, fail FailOn) int {
	floor, ok := fail.threshold()
	if !ok {
		if sum.Incompatible > 0 {
			return ExitIncompatible
		}
		return ExitClean
	}
	lvl := sum.Level()
	if lvl < floor {
		return ExitClean
	}
	switch lvl {
	case render.Major:
		return ExitMajor
	case render.Minor:
		return ExitMinor
	default:
		return ExitPatch
	}
}

// sideSpec names one side: a git revision, or a directory served either from
// disk (fs == nil) or from an arbitrary read-only filesystem (tests).
type sideSpec struct {
	rev    string
	dir    string
	fs     vfs.FS
	mounts []vfs.Mount // extra mounts, such as a test's in-memory module cache
}

// side is one side of the diff: sideSpec.mount serves its tree and loadSide
// type-checks its packages.
type side struct {
	rev       string // git revision, empty for a directory side
	label     string
	mountPath string // synthetic path the side's tree is mounted at, "" on disk
	overlay   *vfs.Overlay
	root      string // the module root within the overlay
	prefix    string // root as a path prefix, rewritten to label+":" in messages
	res       *modres.Resolver
	ld        *loader.Loader
	pkgs      map[string]*types.Package
	internal  map[string]bool // import paths of internal packages
	main      map[string]bool // import paths of main packages
	all       []string        // every import path Options.Filter selects, before Options.Packages and Exclude
	problem   map[string]string
	notes     []string // module-level warnings, reported under the module path
}

// rewrite turns the synthetic mount paths in a message into "<rev>:"
// prefixes, so that positions read like git paths; on the working tree side
// positions become module-relative, like git diff. A file outside the module
// root but inside the mounted tree (a directory replacement) is named
// relative to the tree instead.
func (s *side) rewrite(msg string) string {
	label := ""
	if s.rev != "" {
		label = s.label + ":"
	}
	msg = strings.ReplaceAll(msg, s.prefix, label)
	if s.mountPath != "" {
		msg = strings.ReplaceAll(msg, s.mountPath+"/", label)
	}
	return msg
}

// position converts a token position on this side into a render.Position.
// Positions are relative to the module root, so a declaration outside it
// (a field or method promoted from a dependency) has none.
func (s *side) position(p token.Position) render.Position {
	file, ok := strings.CutPrefix(p.Filename, s.prefix)
	if !p.IsValid() || !ok {
		return render.Position{}
	}
	return render.Position{Rev: s.rev, File: filepath.ToSlash(file), Line: p.Line, Col: p.Column}
}

// openFunc returns a fresh handle on the repository.
type openFunc func() (*git.Repository, error)

// runRepo diffs base against head. rel is the module root relative to the
// repository root (slash-separated, "" for the root).
//
// The two sides load concurrently and each opens its own repository handle:
// go-git's filesystem storage builds its packfile index lazily without
// locking, so a handle shared between the two goroutines races and makes
// revisions spuriously unresolvable ("reference not found"). The first side
// to fail cancels the other's context, which stops its fetches.
func runRepo(ctx context.Context, open openFunc, base, head sideSpec, rel string, env modres.Env, opts Options) (*render.Result, error) {
	if head.rev == LatestRelease {
		return nil, fmt.Errorf("%s can only be the base revision", LatestRelease)
	}
	rev, baseVersion, err := resolveBase(open, base.rev, head, rel, env)
	if err != nil {
		return nil, err
	}
	base.rev = rev

	fset := token.NewFileSet()
	shared := loader.NewSharedCache()
	if opts.GOOS == "" {
		opts.GOOS = runtime.GOOS
	}
	if opts.GOARCH == "" {
		opts.GOARCH = runtime.GOARCH
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		sides [2]*side
		errs  [2]error
	)
	for i, spec := range [2]sideSpec{base, head} {
		wg.Go(func() {
			sides[i], errs[i] = loadSide(ctx, open, spec, rel, env, opts, fset, shared)
			if errs[i] != nil {
				cancel()
			}
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	res := diffSides(sides[0], sides[1], fset)
	res.Base, res.Head = sides[0].label, sides[1].label
	if baseVersion != "" {
		res.BaseVersion = baseVersion
		res.NextVersion = release.Next(baseVersion, render.Summarize(*res).Level())
	}
	res.Warnings = append(res.Warnings, unmatchedPatterns(opts.Packages, sides[0], sides[1])...)
	return res, nil
}

// unmatchedPatterns warns about --pkg patterns that select nothing on
// either side, which is almost always a typo.
func unmatchedPatterns(patterns []string, base, head *side) []render.Warning {
	var out []render.Warning
	for _, pat := range patterns {
		p := discover.Compile(pat)
		matched := slices.ContainsFunc([]*side{base, head}, func(s *side) bool {
			return slices.ContainsFunc(s.all, func(path string) bool { return p.Match(s.res.ModPath(), path) })
		})
		if !matched {
			out = append(out, render.Warning{Package: head.res.ModPath(), Message: fmt.Sprintf("--pkg %q matched no packages", pat)})
		}
	}
	return out
}

// mount serves the tree the spec names, from the git revision, the
// filesystem or the directory on disk, and returns the side with its label,
// its overlay and the module root within it; loadSide does the rest.
func (spec sideSpec) mount(open openFunc, rel string) (*side, error) {
	s := &side{rev: spec.rev}
	switch {
	case spec.rev != "":
		repo, err := open()
		if err != nil {
			return nil, err
		}
		tree, err := resolveTree(repo, spec.rev)
		if err != nil {
			return nil, err
		}
		s.label = spec.rev
		s.mountPath = vfs.GitMountPath(tree)
		s.overlay = vfs.NewOverlay(append([]vfs.Mount{{Path: s.mountPath, FS: vfs.NewGitFS(tree)}}, spec.mounts...)...)
		s.root = path.Join(s.mountPath, rel)
		s.prefix = s.root + "/"
	case spec.fs != nil:
		s.label = "working tree"
		s.mountPath = vfs.SyntheticPrefix + "worktree"
		s.overlay = vfs.NewOverlay(append([]vfs.Mount{{Path: s.mountPath, FS: spec.fs}}, spec.mounts...)...)
		s.root = path.Join(s.mountPath, rel)
		s.prefix = s.root + "/"
	default:
		s.label = "working tree"
		s.overlay = vfs.NewOverlay(spec.mounts...)
		s.root = spec.dir
		s.prefix = s.root + string(filepath.Separator)
	}
	return s, nil
}

func loadSide(ctx context.Context, open openFunc, spec sideSpec, rel string, env modres.Env, opts Options, fset *token.FileSet, shared *loader.SharedCache) (*side, error) {
	s, err := spec.mount(open, rel)
	if err != nil {
		return nil, err
	}
	ctxt := vfs.Context(s.overlay, opts.GOOS, opts.GOARCH)

	res, err := modres.New(s.overlay, s.root, env)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.label, err)
	}
	if opts.Fetch != nil {
		res.Missing = s.fetcher(ctx, opts.Fetch)
		// Fetch the side's missing requirements together, ahead of the
		// imports that need them, when the source can. Best effort: a
		// module that is needed and still missing is fetched, and any
		// problem with it reported, on demand.
		if p, ok := opts.Fetch.(modfetch.Prefetcher); ok {
			if missing := res.MissingModules(); len(missing) > 0 {
				_ = p.Prefetch(ctx, missing)
			}
		}
	}
	s.res = res
	s.ld = loader.New(ctxt, fset, res, shared)
	if limit := s.ld.MaxGoVersion(); limit != "" && version.Compare(version.Lang("go"+res.GoVersion()), limit) > 0 {
		s.notes = append(s.notes, fmt.Sprintf("go.mod requires go %s but go-whatchanged was built with %s; type-checking as %s",
			res.GoVersion(), limit, limit))
	}

	// Main packages can live below internal directories, so those are
	// walked whenever either takes part; Includes sorts out the rest.
	internal := opts.Filter.Has(render.Internal) || opts.Filter.Has(render.Main)
	found, problems, err := discover.Packages(&ctxt, s.overlay, s.root, res.ModPath(), internal, opts.Filter.Has(render.Main))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.label, err)
	}
	s.problem = problems
	s.pkgs = make(map[string]*types.Package, len(found))
	s.internal = make(map[string]bool)
	s.main = make(map[string]bool)
	filter := discover.NewFilter(opts.Packages, opts.Exclude)
	paths := make([]string, 0, len(found))
	for p := range found {
		if !opts.Filter.Includes(found[p].Internal, found[p].Main) {
			continue
		}
		s.all = append(s.all, p)
		if filter.Match(res.ModPath(), p) {
			paths = append(paths, p)
		}
	}
	slices.Sort(s.all)
	slices.Sort(paths)
	for _, p := range paths {
		pkg, err := s.ld.Load(p, found[p].Dir, found[p].Build)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.label, err)
		}
		s.pkgs[p] = pkg
		s.internal[p] = found[p].Internal
		s.main[p] = found[p].Main
	}
	if err := s.ld.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w%s", s.label, err, s.downloadHint(err, rel))
	}
	return s, nil
}

// fetcher returns the resolver hook that fetches a missing module through
// src and makes its tree visible on this side: a tree served by its own
// filesystem is mounted into the side's overlay, one on disk is reachable
// already.
func (s *side) fetcher(ctx context.Context, src modfetch.Source) func(module.Version) (string, error) {
	return func(mod module.Version) (string, error) {
		m, err := src.Fetch(ctx, mod)
		if err != nil {
			return "", err
		}
		if m.FS != nil {
			s.overlay.Add(vfs.Mount{Path: m.Dir, FS: m.FS})
		}
		return m.Dir, nil
	}
}

// downloadHint is the fix to append when err reports a module missing from
// the module cache, which happens only when Options.Fetch is nil, that is
// under --fsreadonly: drop the flag, or run go mod download in the module
// for the working tree and, for a revision, the same from a copy of its
// go.mod in a temporary directory, so that the checkout is not touched. rel
// is the module root relative to the repository root.
func (s *side) downloadHint(err error, rel string) string {
	var missing *modres.MissingModuleError
	if !errors.As(err, &missing) {
		return ""
	}
	if s.rev == "" {
		return "; remove --fsreadonly to let go-whatchanged download it, or download the modules go.mod pins:\n\tgo mod download"
	}
	dir := filepath.Join(os.TempDir(), "go-whatchanged-base")
	return fmt.Sprintf("; remove --fsreadonly to let go-whatchanged download it, or download the modules %s pins:\n\tmkdir -p %s && git show %s:%s > %s && (cd %s && go mod download)",
		s.rev, dir, s.rev, path.Join(rel, "go.mod"), filepath.Join(dir, "go.mod"), dir)
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

func diffSides(base, head *side, fset *token.FileSet) *render.Result {
	union := map[string]bool{}
	for p := range base.pkgs {
		union[p] = true
	}
	for p := range head.pkgs {
		union[p] = true
	}

	res := &render.Result{}
	for _, p := range slices.Sorted(maps.Keys(union)) {
		old, inBase := base.pkgs[p]
		nw, inHead := head.pkgs[p]
		pkg := render.Package{Path: p, Internal: base.internal[p] || head.internal[p], Main: base.main[p] || head.main[p]}
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
			rc := render.Change{Message: c.Message, Compatible: c.Compatible}
			annotate(&rc, fset, base, head, old, nw)
			pkg.Changes = append(pkg.Changes, rc)
		}
		// apidiff only sees symbols, so a package with no exported API
		// that appears or disappears would go unreported. Importers still
		// notice: the import breaks, or its init side effects are gone.
		if len(pkg.Changes) == 0 {
			switch pkg.Status {
			case render.New:
				pkg.Changes = append(pkg.Changes, render.Change{Message: "package added", Compatible: true})
			case render.Removed:
				pkg.Changes = append(pkg.Changes, render.Change{Message: "package removed"})
			}
		}
		res.Packages = append(res.Packages, pkg)
	}

	res.Warnings = collectWarnings(base, head)
	return res
}

// annotate fills in the declarations and the position of the symbol a
// change is about. A removal is described by the base side's object, an
// addition by the head side's, and a "changed from X to Y" message by both
// (the declarations are only set when both can be looked up, so that the
// renderer can fall back to the types quoted in the message). Whole-package
// changes and symbols that cannot be looked up get neither.
func annotate(c *render.Change, fset *token.FileSet, base, head *side, old, nw *types.Package) {
	sym := c.Symbol()
	if sym == "" {
		return
	}
	oldObj, newObj := lookupSymbol(old, sym), lookupSymbol(nw, sym)
	switch c.Kind() {
	case "removed":
		if oldObj != nil {
			c.Before = declString(oldObj, old, sym)
			c.Pos = base.position(fset.Position(oldObj.Pos()))
		}
	case "added":
		if newObj != nil {
			c.After = declString(newObj, nw, sym)
			c.Pos = head.position(fset.Position(newObj.Pos()))
		}
	default:
		if newObj != nil {
			c.Pos = head.position(fset.Position(newObj.Pos()))
		}
		if oldObj != nil && newObj != nil && strings.Contains(c.Message, "changed from ") {
			c.Before, c.After = declString(oldObj, old, sym), declString(newObj, nw, sym)
		}
	}
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
		for _, n := range s.notes {
			add(s.res.ModPath(), n)
		}
		for _, p := range slices.Sorted(maps.Keys(s.problem)) {
			add(p, s.rewrite(s.problem[p]))
		}
		warnings := s.ld.Warnings()
		for _, p := range slices.Sorted(maps.Keys(warnings)) {
			for _, m := range warnings[p] {
				add(p, s.rewrite(m))
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

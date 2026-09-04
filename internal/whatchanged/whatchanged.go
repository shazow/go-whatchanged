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
	// Base names the old side as location@version: "@v1.2.0" for a
	// revision (a commit-ish, or LatestRelease) of the repository of the
	// current directory, "~/src/m@v1.2.0" for one of another checkout, or
	// "github.com/x/m@v1.2.0" or "github.com/x/m@latest" for a module
	// version. Empty means "@HEAD", so the zero value diffs the last
	// commit against the working tree.
	Base string
	// Head names the new side in the same forms; "@query" alone applies
	// the query to the base's repository or module. Empty means the
	// working tree of the base's repository, or "@HEAD", the default
	// branch, for a module base.
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
	// Kinds selects which kinds of change are shown: the API changes,
	// the import changes or, the zero value, both. An import change is a
	// package of another module that a package started or stopped
	// importing, listed before the package's API changes; the standard
	// library and the module's own packages are not tracked. Import
	// changes are not API changes: they never count towards the summary,
	// the required release or the exit code, and a package with import
	// changes alone is listed without counting as changed.
	Kinds render.Kinds
	// Width is the number of columns the text layout may use, 0 for no
	// limit; see render.Options.Width.
	Width int
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
	// lacks, and the module versions Base and Head may name. Nil keeps the
	// run read-only: a missing module is an error whose message says how
	// to get it, and a module version cannot be diffed.
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
	base, head, err := parseTargets(opts.Base, opts.Head)
	if err != nil {
		return ExitError, err
	}
	var specs [2]sideSpec
	for i, t := range [2]target{base, head} {
		if specs[i], err = t.spec(); err != nil {
			return ExitError, err
		}
	}

	env, err := modres.DefaultEnv()
	if err != nil {
		return ExitError, err
	}
	res, err := compare(context.Background(), specs[0], specs[1], env, opts)
	if err != nil {
		return ExitError, err
	}
	return finish(res, opts)
}

// spec opens what the target needs: the repository of a git or working-tree
// target, checked up front so that a broken one is reported once rather
// than from inside a side. A module target is resolved later, in compare.
func (t target) spec() (sideSpec, error) {
	if t.module != "" {
		return sideSpec{mod: module.Version{Path: t.module, Version: t.query}}, nil
	}
	dir, err := filepath.Abs(t.dir)
	if err != nil {
		return sideSpec{}, err
	}
	// Walking up from a directory that does not exist would find the
	// repository above it and silently diff that one.
	if fi, err := os.Stat(dir); err != nil {
		return sideSpec{}, fmt.Errorf("%s: no such directory", t.dir)
	} else if !fi.IsDir() {
		return sideSpec{}, fmt.Errorf("%s is not a directory", t.dir)
	}
	gitRoot, err := findGitRoot(dir)
	if err != nil {
		return sideSpec{}, err
	}
	modRoot, err := findModRoot(dir, gitRoot)
	if err != nil {
		return sideSpec{}, err
	}
	rel, err := filepath.Rel(gitRoot, modRoot)
	if err != nil {
		return sideSpec{}, err
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
	if _, err := open(); err != nil {
		return sideSpec{}, err
	}
	spec := sideSpec{open: open, rel: rel}
	if t.query == "" {
		spec.dir = modRoot
	} else {
		spec.rev = t.query
	}
	return spec, nil
}

// DefaultBase is the revision used for the old side when none is given.
const DefaultBase = "HEAD"

// finish prints warnings and the diff, and derives the exit code.
func finish(res *render.Result, opts Options) (int, error) {
	if len(res.Warnings) > 0 {
		if err := render.WriteWarnings(opts.Stderr, res.Warnings, opts.Color); err != nil {
			return ExitError, err
		}
		if opts.Strict {
			return ExitError, fmt.Errorf("--strict: the %d type-check warning(s) above are fatal", len(res.Warnings))
		}
	}
	ro := render.Options{
		Color:        opts.Color,
		BreakingOnly: opts.Breaking,
		Kinds:        opts.Kinds,
		Format:       opts.Format,
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

// sideSpec names one side: a git revision, a directory served either from
// disk (fs == nil) or from an arbitrary read-only filesystem (tests), or a
// module version fetched through Options.Fetch.
type sideSpec struct {
	rev    string         // git revision; LatestRelease until resolveBase
	dir    string         // working tree directory
	fs     vfs.FS         // filesystem serving the working tree, instead of dir
	open   openFunc       // the repository of a git or working tree side
	rel    string         // module root relative to the repository root, slash-separated
	mod    module.Version // module side; Version is the query until resolveSides
	mounts []vfs.Mount    // extra mounts, such as a test's in-memory module cache
}

// side is one side of the diff: sideSpec.mount serves its tree and loadSide
// type-checks its packages.
type side struct {
	rev       string // git revision, empty for a directory side
	label     string // how the output names the side: the revision, the module version or "working tree"
	name      string // how an error names it: "@rev" for a revision, else label
	rel       string // module root relative to the tree root, slash-separated, "" at the root
	treeRoot  string // the mounted tree, or the repository on disk, holding root
	mountPath string // synthetic path the side's tree is mounted at, "" on disk
	overlay   *vfs.Overlay
	root      string // the module root within the overlay
	prefix    string // root as a path prefix, rewritten to label+":" in messages
	gomod     []byte // the go.mod of a module side, from the fetch
	res       *modres.Resolver
	ld        *loader.Loader
	pkgs      map[string]*types.Package
	internal  map[string]bool     // import paths of internal packages
	main      map[string]bool     // import paths of main packages
	imports   map[string][]string // the packages of other modules each package imports
	all       []string            // every import path Options.Filter selects, before Options.Packages and Exclude
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

// compare diffs base against head.
//
// The two sides load concurrently and each opens its own repository handle:
// go-git's filesystem storage builds its packfile index lazily without
// locking, so a handle shared between the two goroutines races and makes
// revisions spuriously unresolvable ("reference not found"). The first side
// to fail cancels the other's context, which stops its fetches.
func compare(ctx context.Context, base, head sideSpec, env modres.Env, opts Options) (*render.Result, error) {
	if head.rev == LatestRelease {
		return nil, fmt.Errorf("%s can only be the base revision", LatestRelease)
	}
	base, head, baseVersion, err := resolveSides(ctx, base, head, env, opts.Fetch)
	if err != nil {
		return nil, err
	}

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
			sides[i], errs[i] = loadSide(ctx, spec, env, opts, fset, shared)
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
// filesystem, the directory on disk or the fetched module, and returns the
// side with its label, its overlay and the module root within it; loadSide
// does the rest. src fetches a module side.
func (spec sideSpec) mount(ctx context.Context, src modfetch.Source) (*side, error) {
	s := &side{rev: spec.rev}
	switch {
	case spec.mod.Path != "":
		if src == nil {
			return nil, fmt.Errorf("%s@%s: diffing a module version needs the go command; remove --fsreadonly", spec.mod.Path, spec.mod.Version)
		}
		m, err := src.Fetch(ctx, spec.mod)
		if err != nil {
			return nil, err
		}
		s.label = m.Version.String()
		s.name = s.label
		s.rev = s.label // positions read "path@version:file.go:1"
		s.gomod = m.GoMod
		s.treeRoot = m.Dir
		if m.FS != nil {
			s.mountPath = m.Dir
			s.overlay = vfs.NewOverlay(append([]vfs.Mount{{Path: m.Dir, FS: m.FS}}, spec.mounts...)...)
			s.root = m.Dir
			s.prefix = s.root + "/"
		} else {
			s.overlay = vfs.NewOverlay(spec.mounts...)
			s.root = m.Dir
			s.prefix = s.root + string(filepath.Separator)
		}
	case spec.rev != "":
		repo, err := spec.open()
		if err != nil {
			return nil, err
		}
		tree, err := resolveTree(repo, spec.rev)
		if err != nil {
			return nil, err
		}
		s.label = spec.rev
		s.name = "@" + spec.rev
		s.rel = spec.rel
		s.mountPath = vfs.GitMountPath(tree)
		s.treeRoot = s.mountPath
		s.overlay = vfs.NewOverlay(append([]vfs.Mount{{Path: s.mountPath, FS: vfs.NewGitFS(tree)}}, spec.mounts...)...)
		s.root = path.Join(s.mountPath, spec.rel)
		s.prefix = s.root + "/"
	case spec.fs != nil:
		s.label = "working tree"
		s.name = s.label
		s.rel = spec.rel
		s.mountPath = vfs.SyntheticPrefix + "worktree"
		s.treeRoot = s.mountPath
		s.overlay = vfs.NewOverlay(append([]vfs.Mount{{Path: s.mountPath, FS: spec.fs}}, spec.mounts...)...)
		s.root = path.Join(s.mountPath, spec.rel)
		s.prefix = s.root + "/"
	default:
		s.label = "working tree"
		s.name = s.label
		s.rel = spec.rel
		s.overlay = vfs.NewOverlay(spec.mounts...)
		s.root = spec.dir
		s.prefix = s.root + string(filepath.Separator)
		// The repository root is rel levels above the module root.
		s.treeRoot = spec.dir
		if spec.rel != "" {
			for range strings.Count(spec.rel, "/") + 1 {
				s.treeRoot = filepath.Dir(s.treeRoot)
			}
		}
	}
	return s, nil
}

// resolver returns the side's import resolver: from the fetched go.mod of
// a module side, else from the go.mod in its tree. The error names the
// side and, for a tree without a go.mod, where the file was looked for in
// the terms the user named the side in, rather than the synthetic path the
// tree is mounted at.
func (s *side) resolver(env modres.Env) (*modres.Resolver, error) {
	var res *modres.Resolver
	var err error
	if s.gomod != nil {
		res, err = modres.NewModule(s.overlay, s.root, s.gomod, env)
	} else {
		res, err = modres.New(s.overlay, s.root, env)
	}
	var noMod *modres.NoGoModError
	if errors.As(err, &noMod) {
		return nil, fmt.Errorf("%s: %s", s.name, s.noGoMod())
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %s", s.name, s.rewrite(err.Error()))
	}
	return res, nil
}

// noGoMod describes a module root without a go.mod. A revision that
// predates the module, or the directory of a module that did not exist
// yet, is the usual case.
func (s *side) noGoMod() string {
	switch {
	case s.mountPath == "":
		return fmt.Sprintf("no go.mod in %s (GOPATH mode is not supported)", s.root)
	case s.rel != "":
		return fmt.Sprintf("no %s/go.mod at this revision", s.rel)
	case s.rev != "":
		return "no go.mod at this revision (GOPATH mode is not supported)"
	default:
		return "no go.mod in the working tree (GOPATH mode is not supported)"
	}
}

// goWork returns the go.work file between the module root and the root of
// its tree, relative to that root, or "" when there is none. A workspace
// is the usual reason an import of a sibling module cannot be resolved:
// the tool reads go.mod alone.
func (s *side) goWork() string {
	join, parent, sep := path.Join, path.Dir, "/"
	if s.mountPath == "" {
		join, parent, sep = filepath.Join, filepath.Dir, string(filepath.Separator)
	}
	for dir := s.root; ; dir = parent(dir) {
		if fi, err := s.overlay.Stat(join(dir, "go.work")); err == nil && !fi.IsDir() {
			return filepath.ToSlash(strings.TrimPrefix(join(dir, "go.work"), s.treeRoot+sep))
		}
		if dir == s.treeRoot || parent(dir) == dir {
			return ""
		}
	}
}

func loadSide(ctx context.Context, spec sideSpec, env modres.Env, opts Options, fset *token.FileSet, shared *loader.SharedCache) (*side, error) {
	s, err := spec.mount(ctx, opts.Fetch)
	if err != nil {
		return nil, err
	}
	ctxt := vfs.Context(s.overlay, opts.GOOS, opts.GOARCH)

	res, err := s.resolver(env)
	if err != nil {
		return nil, err
	}
	if opts.Fetch != nil {
		// A missing module is fetched and, when its tree comes with a
		// filesystem of its own, mounted into this side's overlay; a tree
		// on disk is reachable already.
		res.Missing = func(mod module.Version) (string, error) {
			m, err := opts.Fetch.Fetch(ctx, mod)
			if err != nil {
				return "", err
			}
			if m.FS != nil {
				s.overlay.Add(vfs.Mount{Path: m.Dir, FS: m.FS})
			}
			return m.Dir, nil
		}
		// The side's missing requirements are fetched together, ahead of
		// the imports that need them. Best effort: a module that is needed
		// and still missing is fetched, and any problem with it reported,
		// on demand.
		if missing := res.MissingModules(); len(missing) > 0 {
			_ = opts.Fetch.Prefetch(ctx, missing)
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
		return nil, fmt.Errorf("%s: %w", s.name, err)
	}
	s.problem = problems
	s.pkgs = make(map[string]*types.Package, len(found))
	s.internal = make(map[string]bool)
	s.main = make(map[string]bool)
	if opts.Kinds.Has(render.Imports) {
		s.imports = make(map[string][]string)
	}
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
			return nil, fmt.Errorf("%s: %w", s.name, err)
		}
		s.pkgs[p] = pkg
		s.internal[p] = found[p].Internal
		s.main[p] = found[p].Main
		if s.imports != nil {
			s.imports[p] = s.dependencies(found[p].Dir, found[p].Build.Imports)
		}
	}
	if err := s.ld.Err(); err != nil {
		// A module the cache lacks is only ever missing without a source,
		// that is under --fsreadonly.
		var missing *modres.MissingModuleError
		if errors.As(err, &missing) {
			err = fmt.Errorf("%w; remove --fsreadonly to let go-whatchanged download it", err)
		}
		// In a workspace, the import of a sibling module resolves through
		// go.work for the go command and not at all here.
		if gowork := s.goWork(); gowork != "" {
			err = fmt.Errorf("%w; %s is not consulted, so a workspace module must come from the module cache or a replace directive", err, gowork)
		}
		return nil, fmt.Errorf("%s: %w", s.name, err)
	}
	return s, nil
}

// resolveTree returns the tree of the commit rev names. go-git resolves
// every revision to a commit, peeling annotated tags on the way.
func resolveTree(repo *git.Repository, rev string) (*object.Tree, error) {
	hash, err := resolveCommit(repo, rev)
	if err != nil {
		return nil, err
	}
	commit, err := repo.CommitObject(hash)
	if err != nil {
		return nil, fmt.Errorf("@%s: %w", rev, err)
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
		pkg.Imports = diffImports(base.imports[p], head.imports[p])
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

// dependencies returns the imports of a package in dir that other modules
// provide: neither the standard library nor this module, as the resolver
// tells them apart, so that a nested module is a dependency and a replaced
// module without a dot in its path is not the standard library. An import
// that resolves nowhere is kept, since it is not the standard library or
// this module either; the loader warns about it separately. The imports
// are those go/build lists: the ones of the package's non-test files for
// the build target, "C" excluded since cgo is disabled.
func (s *side) dependencies(dir string, imports []string) []string {
	var deps []string
	for _, p := range imports {
		if loc, err := s.res.Resolve(p, dir); err == nil && loc.Kind != modres.Dep {
			continue
		}
		deps = append(deps, p)
	}
	return deps
}

// diffImports lists the import paths in old but not in nw as removed and
// those in nw but not in old as added, removals first, each sorted by
// path. The imports of a package on one side only are all removed or all
// added.
func diffImports(old, nw []string) []render.Import {
	var out []render.Import
	for _, p := range slices.Sorted(slices.Values(old)) {
		if !slices.Contains(nw, p) {
			out = append(out, render.Import{Path: p, Removed: true})
		}
	}
	for _, p := range slices.Sorted(slices.Values(nw)) {
		if !slices.Contains(old, p) {
			out = append(out, render.Import{Path: p})
		}
	}
	return out
}

// annotate fills in the declarations, the struct of a field and the
// position of the symbol a change is about. A removal is described by the
// base side's object, an addition by the head side's, and a "changed from
// X to Y" message by both (the declarations are only set when both can be
// looked up, so that the renderer can fall back to the types quoted in the
// message). Whole-package changes and symbols that cannot be looked up get
// neither.
func annotate(c *render.Change, fset *token.FileSet, base, head *side, old, nw *types.Package) {
	sym := c.Symbol()
	if sym == "" {
		return
	}
	oldObj, newObj := lookupSymbol(old, sym), lookupSymbol(nw, sym)
	switch c.Kind() {
	case "removed":
		if oldObj != nil {
			c.Before, c.Struct = declString(oldObj, old), structOf(oldObj, sym)
			c.Pos = base.position(fset.Position(oldObj.Pos()))
		}
	case "added":
		if newObj != nil {
			c.After, c.Struct = declString(newObj, nw), structOf(newObj, sym)
			c.Pos = head.position(fset.Position(newObj.Pos()))
		}
	default:
		if newObj != nil {
			c.Pos = head.position(fset.Position(newObj.Pos()))
		}
		if oldObj != nil && newObj != nil && strings.Contains(c.Message, "changed from ") {
			c.Before, c.After, c.Struct = declString(oldObj, old), declString(newObj, nw), structOf(newObj, sym)
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
			where := dir
			if dir != gitRoot {
				where = fmt.Sprintf("%s or above it, up to the repository root %s", dir, gitRoot)
			}
			return "", fmt.Errorf("no go.mod in %s; go-whatchanged diffs one Go module: run it inside the module, or name its directory, ./sub@HEAD", where)
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("no go.mod found above %s", dir)
		}
		d = parent
	}
}

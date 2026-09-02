// Package loader type-checks packages in-process from a vfs-backed
// go/build context, memoizing results so that a directory is only ever
// checked once.
package loader

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"go/version"
	"io"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/shazow/go-whatchanged/internal/modres"
)

// SharedCache memoizes packages by resolved directory. Standard library and
// module cache directories are immutable, so the same directory always yields
// the same package regardless of which side asked for it.
type SharedCache struct {
	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	done     chan struct{}
	pkg      *types.Package
	err      error
	warnings []string
}

// NewSharedCache returns an empty cache.
func NewSharedCache() *SharedCache {
	return &SharedCache{entries: map[string]*entry{}}
}

// acquire returns the entry for key and whether the caller is responsible
// for populating it.
func (c *SharedCache) acquire(key string) (*entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		return e, false
	}
	e := &entry{done: make(chan struct{})}
	c.entries[key] = e
	return e, true
}

// ResolveError reports an import path that could not be mapped to a
// directory. It is fatal: apidiff cannot reason about an API whose
// dependencies are missing.
type ResolveError struct {
	ImportPath string
	RequiredBy string
	Err        error
}

func (e *ResolveError) Error() string {
	return fmt.Sprintf("unresolvable import %q (required by %s): %v", e.ImportPath, e.RequiredBy, e.Err)
}

func (e *ResolveError) Unwrap() error { return e.Err }

// Loader is a memoizing types.ImporterFrom for one side of a diff.
type Loader struct {
	ctxt     build.Context
	fset     *token.FileSet
	resolver *modres.Resolver
	shared   *SharedCache
	maxGo    string // language version cap, "go1.24" form; "" when unknown

	mu       sync.Mutex
	local    map[string]*types.Package // main-module packages by import path
	localErr map[string]error
	inflight map[string]bool // directories being loaded by this loader (cycle guard)
	warnings map[string][]string
	stack    []string // import paths currently being checked, innermost last
	fatal    error    // first ResolveError encountered
}

// New returns a loader for one side.
func New(ctxt build.Context, fset *token.FileSet, resolver *modres.Resolver, shared *SharedCache) *Loader {
	return &Loader{
		ctxt:     ctxt,
		fset:     fset,
		resolver: resolver,
		shared:   shared,
		maxGo:    toolchainVersion(resolver.StdGoVersion()),
		local:    map[string]*types.Package{},
		localErr: map[string]error{},
		inflight: map[string]bool{},
		warnings: map[string][]string{},
	}
}

// Import implements types.Importer.
func (l *Loader) Import(path string) (*types.Package, error) {
	return l.ImportFrom(path, "", 0)
}

// ImportFrom implements types.ImporterFrom. dir is the directory of the
// importing package, used to resolve GOROOT-vendored imports.
func (l *Loader) ImportFrom(path, dir string, _ types.ImportMode) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	loc, err := l.resolver.Resolve(path, dir)
	if err != nil {
		rerr := &ResolveError{ImportPath: path, RequiredBy: l.current(), Err: err}
		l.mu.Lock()
		if l.fatal == nil {
			l.fatal = rerr
		}
		l.mu.Unlock()
		return nil, rerr
	}
	return l.loadLocation(path, loc)
}

// Err returns the first fatal resolution error encountered, if any.
func (l *Loader) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fatal
}

// current returns the import path of the package currently being checked.
// Loading is synchronous per loader, so the stack is well defined.
func (l *Loader) current() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.stack) == 0 {
		return "?"
	}
	return l.stack[len(l.stack)-1]
}

func (l *Loader) push(importPath string) {
	l.mu.Lock()
	l.stack = append(l.stack, importPath)
	l.mu.Unlock()
}

func (l *Loader) pop() {
	l.mu.Lock()
	l.stack = l.stack[:len(l.stack)-1]
	l.mu.Unlock()
}

// Load type-checks the main-module package at dir with the given import
// path. It is the entry point discover uses for each candidate package.
func (l *Loader) Load(importPath, dir string) (*types.Package, error) {
	return l.loadLocation(importPath, modres.Location{Dir: dir, Kind: modres.Main, GoVersion: l.resolver.GoVersion()})
}

// Warnings returns type-check and parse errors recorded so far, keyed by
// import path, in a deterministic order.
func (l *Loader) Warnings() map[string][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string][]string, len(l.warnings))
	for k, v := range l.warnings {
		out[k] = append([]string(nil), v...)
	}
	return out
}

func (l *Loader) loadLocation(importPath string, loc modres.Location) (*types.Package, error) {
	if loc.Kind == modres.Main {
		return l.loadLocal(importPath, loc)
	}
	return l.loadShared(importPath, loc)
}

func (l *Loader) loadLocal(importPath string, loc modres.Location) (*types.Package, error) {
	l.mu.Lock()
	if pkg, ok := l.local[importPath]; ok {
		l.mu.Unlock()
		return pkg, nil
	}
	if err, ok := l.localErr[importPath]; ok {
		l.mu.Unlock()
		return nil, err
	}
	if l.inflight[loc.Dir] {
		l.mu.Unlock()
		return nil, fmt.Errorf("import cycle through %q", importPath)
	}
	l.inflight[loc.Dir] = true
	l.mu.Unlock()

	pkg, warnings, err := l.check(importPath, loc)

	l.mu.Lock()
	delete(l.inflight, loc.Dir)
	if len(warnings) > 0 {
		l.warnings[importPath] = append(l.warnings[importPath], warnings...)
	}
	if err != nil {
		l.localErr[importPath] = err
	} else {
		l.local[importPath] = pkg
	}
	l.mu.Unlock()
	return pkg, err
}

func (l *Loader) loadShared(importPath string, loc modres.Location) (*types.Package, error) {
	l.mu.Lock()
	if l.inflight[loc.Dir] {
		l.mu.Unlock()
		return nil, fmt.Errorf("import cycle through %q", importPath)
	}
	l.mu.Unlock()

	e, owner := l.shared.acquire(loc.Dir)
	if owner {
		l.mu.Lock()
		l.inflight[loc.Dir] = true
		l.mu.Unlock()

		e.pkg, e.warnings, e.err = l.check(importPath, loc)
		close(e.done)

		l.mu.Lock()
		delete(l.inflight, loc.Dir)
		l.mu.Unlock()
	} else {
		<-e.done
	}
	if len(e.warnings) > 0 {
		l.mu.Lock()
		if _, seen := l.warnings[importPath]; !seen {
			l.warnings[importPath] = append([]string(nil), e.warnings...)
		}
		l.mu.Unlock()
	}
	return e.pkg, e.err
}

// check parses and type-checks the package at loc. Type errors never abort
// the check: apidiff copes with partial packages, so they are returned as
// warnings alongside the (possibly incomplete) package.
func (l *Loader) check(importPath string, loc modres.Location) (*types.Package, []string, error) {
	l.push(importPath)
	defer l.pop()
	bp, err := l.ctxt.ImportDir(loc.Dir, 0)
	if err != nil {
		if _, ok := err.(*build.NoGoError); ok {
			return nil, nil, fmt.Errorf("no buildable Go source files for %q in %s", importPath, loc.Dir)
		}
		return nil, nil, fmt.Errorf("%s: %w", importPath, err)
	}

	var warnings []string
	files := make([]*ast.File, 0, len(bp.GoFiles))
	for _, name := range bp.GoFiles {
		filename := l.ctxt.JoinPath(loc.Dir, name)
		src, err := readFile(l.ctxt, filename)
		if err != nil {
			return nil, warnings, fmt.Errorf("%s: %w", importPath, err)
		}
		f, err := parser.ParseFile(l.fset, filename, src, parser.SkipObjectResolution)
		if err != nil {
			warnings = append(warnings, err.Error())
		}
		if f != nil {
			files = append(files, f)
		}
	}

	conf := types.Config{
		Importer:    l,
		FakeImportC: true,
		GoVersion:   l.langVersion(loc.GoVersion),
		Error: func(err error) {
			warnings = append(warnings, err.Error())
		},
	}
	if sizes := types.SizesFor("gc", l.ctxt.GOARCH); sizes != nil {
		conf.Sizes = sizes
	}
	pkg, _ := conf.Check(importPath, l.fset, files, nil)
	if pkg == nil {
		return nil, warnings, fmt.Errorf("%s: type-checking produced no package", importPath)
	}
	sort.Strings(warnings)
	return pkg, dedupe(warnings), nil
}

func readFile(ctxt build.Context, name string) ([]byte, error) {
	rc, err := ctxt.OpenFile(name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// MaxGoVersion returns the language version every package is type-checked
// at most as, in "go1.24" form, or "" when the toolchain version is unknown.
func (l *Loader) MaxGoVersion() string { return l.maxGo }

// langVersion converts a go.mod language version into the form go/types
// expects, capped at the toolchain's own language version. go/types reports
// an error in every package that declares a version newer than the one it
// was built with, and it cannot know newer features anyway, so checking as
// the newest version it does know loses nothing and keeps the output quiet.
func (l *Loader) langVersion(v string) string {
	gv := goVersion(v)
	if gv == "" || l.maxGo == "" {
		return gv
	}
	if version.Compare(version.Lang(gv), l.maxGo) > 0 {
		return l.maxGo
	}
	return gv
}

// toolchainVersion returns the language version implemented by the running
// binary's go/types, in "go1.24" form: the lower of the version this binary
// was compiled with and the version of the standard library it type-checks
// against (std is the go directive of $GOROOT/src/go.mod). Development
// builds of Go report no usable version and fall back to std alone.
func toolchainVersion(std string) string {
	built := version.Lang(runtime.Version())
	std = version.Lang(goVersion(std))
	switch {
	case built == "":
		return std
	case std == "":
		return built
	case version.Compare(std, built) < 0:
		return std
	default:
		return built
	}
}

// goVersion converts a go.mod language version ("1.21", "1.21.0") into the
// "go1.21" form go/types expects.
func goVersion(v string) string {
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "go") {
		return v
	}
	return "go" + v
}

func dedupe(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i > 0 && s[i-1] == v {
			continue
		}
		out = append(out, v)
	}
	return out
}

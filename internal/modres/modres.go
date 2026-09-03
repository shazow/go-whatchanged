// Package modres resolves import paths to directories for one side of a diff
// using only go.mod, GOROOT, the module cache and the directories that
// replace directives name. It never runs the go command and never touches
// the network.
package modres

import (
	"errors"
	"fmt"
	"go/version"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	"github.com/shazow/go-whatchanged/internal/vfs"
)

// FS is the read-only filesystem surface the resolver needs.
type FS interface {
	IsDir(name string) bool
	ReadFile(name string) ([]byte, error)
}

// Env holds the toolchain locations used for resolution.
type Env struct {
	GOROOT     string
	GOMODCACHE string
}

// DefaultEnv locates GOROOT and GOMODCACHE from the runtime and environment.
func DefaultEnv() (Env, error) {
	// runtime.GOROOT is deprecated in favour of asking the go command, which
	// this tool never runs. It returns $GOROOT when set and the root the
	// binary was built with otherwise, and the check below catches a stale
	// one.
	//lint:ignore SA1019 the go command is off limits; see above.
	goroot := runtime.GOROOT()
	if goroot == "" {
		return Env{}, errors.New("cannot locate GOROOT: set the GOROOT environment variable")
	}
	if fi, err := os.Stat(filepath.Join(goroot, "src", "fmt")); err != nil || !fi.IsDir() {
		return Env{}, fmt.Errorf("GOROOT %q does not contain a Go source tree", goroot)
	}

	modcache := os.Getenv("GOMODCACHE")
	if modcache == "" {
		gopath := os.Getenv("GOPATH")
		if gopath == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return Env{}, fmt.Errorf("cannot locate GOMODCACHE: %w", err)
			}
			gopath = filepath.Join(home, "go")
		}
		if list := filepath.SplitList(gopath); len(list) > 0 {
			gopath = list[0]
		}
		modcache = filepath.Join(gopath, "pkg", "mod")
	}
	return Env{GOROOT: goroot, GOMODCACHE: modcache}, nil
}

// Kind classifies where a package lives.
type Kind int

const (
	// Main is a package of the main module.
	Main Kind = iota
	// Std is a package of the standard library.
	Std
	// Dep is a package from a module in the module cache (or a directory
	// replacement).
	Dep
)

// MissingModuleError reports a module that resolution needs but the module
// cache does not have. The tool never downloads, so the caller says how to.
type MissingModuleError struct {
	Path, Version string
}

func (e *MissingModuleError) Error() string {
	return fmt.Sprintf("module %s@%s not in module cache", e.Path, e.Version)
}

// Location is a resolved import path.
type Location struct {
	Dir       string // directory holding the package's source files
	Kind      Kind
	GoVersion string // language version of the module providing the package
}

// Resolver maps import paths to directories for one side.
type Resolver struct {
	fs        FS
	env       Env
	root      string // main module root
	modPath   string
	goVersion string
	requires  []module.Version
	replaces  map[module.Version]replacement
	stdGo     string

	mu         sync.Mutex
	goVersions map[string]string // module root dir → go version
}

type replacement struct {
	dir string         // non-empty for directory replacements
	mod module.Version // used when dir is empty
}

// New reads <root>/go.mod through fs and returns a resolver for that module.
func New(fs FS, root string, env Env) (*Resolver, error) {
	gomod := joinPath(root, "go.mod")
	data, err := fs.ReadFile(gomod)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w (GOPATH mode is not supported)", gomod, err)
	}
	// This is the main module's go.mod, so parse it in full: the lax parser
	// meant for dependencies drops replace directives.
	mf, err := modfile.Parse(gomod, data, nil)
	if err != nil {
		return nil, err
	}
	if mf.Module == nil || mf.Module.Mod.Path == "" {
		return nil, fmt.Errorf("%s: missing module directive", gomod)
	}
	goVersion := ""
	if mf.Go != nil {
		goVersion = mf.Go.Version
	}
	if goVersion == "" {
		return nil, fmt.Errorf("%s: missing go directive (go >= 1.17 is required)", gomod)
	}
	if version.Compare("go"+goVersion, "go1.17") < 0 {
		return nil, fmt.Errorf("%s: go %s is too old; go >= 1.17 is required for module graph pruning", gomod, goVersion)
	}

	r := &Resolver{
		fs:         fs,
		env:        env,
		root:       root,
		modPath:    mf.Module.Mod.Path,
		goVersion:  goVersion,
		replaces:   map[module.Version]replacement{},
		goVersions: map[string]string{},
	}
	for _, req := range mf.Require {
		r.requires = append(r.requires, req.Mod)
	}
	for _, rep := range mf.Replace {
		if rep.New.Version == "" {
			dir := rep.New.Path
			if !filepath.IsAbs(dir) && !strings.HasPrefix(dir, "/") {
				dir = joinPath(root, dir)
			}
			r.replaces[rep.Old] = replacement{dir: dir}
		} else {
			r.replaces[rep.Old] = replacement{mod: rep.New}
		}
	}

	// The standard library's own go.mod names its language version.
	r.stdGo = version.Lang(runtime.Version())
	if data, err := os.ReadFile(filepath.Join(env.GOROOT, "src", "go.mod")); err == nil {
		if mf, err := modfile.ParseLax("go.mod", data, nil); err == nil && mf.Go != nil {
			r.stdGo = mf.Go.Version
		}
	}
	return r, nil
}

// ModPath returns the main module path.
func (r *Resolver) ModPath() string { return r.modPath }

// GoVersion returns the main module's go directive.
func (r *Resolver) GoVersion() string { return r.goVersion }

// StdGoVersion returns the language version of the standard library in
// GOROOT: the go directive of $GOROOT/src/go.mod, or the running toolchain's
// version when that file cannot be read.
func (r *Resolver) StdGoVersion() string { return r.stdGo }

// Resolve maps importPath to a directory. fromDir is the directory of the
// importing package (or "" when unknown); it is used to honour GOROOT's
// vendor directory when standard library packages import golang.org/x code.
func (r *Resolver) Resolve(importPath, fromDir string) (Location, error) {
	if importPath == "" || importPath == "." || strings.HasPrefix(importPath, "./") || strings.HasPrefix(importPath, "../") {
		return Location{}, fmt.Errorf("relative import %q is not supported", importPath)
	}

	// The module providing a package is the one with the longest matching
	// path among the main module and the require directives, as in the go
	// command: a nested module that go.mod requires serves its packages
	// from the module cache (or its replacement), not from the main
	// module's tree, which discover skips for it anyway.
	var best module.Version
	for _, req := range r.requires {
		if (importPath == req.Path || strings.HasPrefix(importPath, req.Path+"/")) && len(req.Path) > len(best.Path) {
			best = req
		}
	}
	if len(best.Path) <= len(r.modPath) {
		if importPath == r.modPath {
			return Location{Dir: r.root, Kind: Main, GoVersion: r.goVersion}, nil
		}
		if rest, ok := strings.CutPrefix(importPath, r.modPath+"/"); ok {
			return Location{Dir: joinPath(r.root, rest), Kind: Main, GoVersion: r.goVersion}, nil
		}
	}

	// Standard library, including GOROOT/src/vendor for std-internal imports.
	gorootSrc := filepath.Join(r.env.GOROOT, "src")
	if fromDir != "" && isSubdir(gorootSrc, fromDir) {
		vendored := filepath.Join(gorootSrc, "vendor", filepath.FromSlash(importPath))
		if r.fs.IsDir(vendored) {
			return Location{Dir: vendored, Kind: Std, GoVersion: r.stdGo}, nil
		}
	}
	// A path whose first element has no dot is a standard library package
	// when GOROOT has it; otherwise a module may still provide it, which
	// the go command allows for replaced modules.
	if first, _, _ := strings.Cut(importPath, "/"); !strings.Contains(first, ".") {
		dir := filepath.Join(gorootSrc, filepath.FromSlash(importPath))
		if r.fs.IsDir(dir) {
			return Location{Dir: dir, Kind: Std, GoVersion: r.stdGo}, nil
		}
		if best.Path == "" {
			return Location{}, fmt.Errorf("not found in GOROOT (%s)", r.env.GOROOT)
		}
	}
	if best.Path == "" {
		return Location{}, errors.New("no module in go.mod provides it")
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(importPath, best.Path), "/")

	// Apply replace directives: exact version first, then wildcard.
	rep, ok := r.replaces[best]
	if !ok {
		rep, ok = r.replaces[module.Version{Path: best.Path}]
	}
	var modRoot string
	if ok && rep.dir != "" {
		modRoot = rep.dir
		if !r.fs.IsDir(modRoot) {
			return Location{}, fmt.Errorf("replacement directory %s for module %s does not exist", modRoot, best.Path)
		}
	} else {
		mod := best
		if ok {
			mod = rep.mod
		}
		escPath, err := module.EscapePath(mod.Path)
		if err != nil {
			return Location{}, err
		}
		escVer, err := module.EscapeVersion(mod.Version)
		if err != nil {
			return Location{}, err
		}
		modRoot = filepath.Join(r.env.GOMODCACHE, filepath.FromSlash(escPath)+"@"+escVer)
		if !r.fs.IsDir(modRoot) {
			return Location{}, &MissingModuleError{Path: mod.Path, Version: mod.Version}
		}
	}
	dir := modRoot
	if rest != "" {
		dir = joinPath(modRoot, rest)
	}
	return Location{Dir: dir, Kind: Dep, GoVersion: r.moduleGoVersion(modRoot)}, nil
}

// moduleGoVersion reads the go directive of the module rooted at dir,
// falling back to the main module's version.
func (r *Resolver) moduleGoVersion(modRoot string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.goVersions[modRoot]; ok {
		return v
	}
	v := r.goVersion
	if data, err := r.fs.ReadFile(joinPath(modRoot, "go.mod")); err == nil {
		if mf, err := modfile.ParseLax("go.mod", data, nil); err == nil && mf.Go != nil && mf.Go.Version != "" {
			v = mf.Go.Version
		}
	}
	r.goVersions[modRoot] = v
	return v
}

// joinPath joins a root with a slash-separated relative path, preserving the
// root's flavour (synthetic slash paths vs. host paths).
func joinPath(root, rel string) string {
	if strings.HasPrefix(root, vfs.SyntheticPrefix) {
		return path.Join(root, rel)
	}
	return filepath.Join(root, filepath.FromSlash(rel))
}

func isSubdir(root, dir string) bool {
	root = filepath.Clean(root)
	dir = filepath.Clean(dir)
	return dir == root || strings.HasPrefix(dir, root+string(filepath.Separator))
}

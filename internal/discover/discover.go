// Package discover enumerates the diffable packages of a main module.
package discover

import (
	"go/build"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// FS is the read-only filesystem surface needed to walk a module.
type FS interface {
	ReadDir(name string) ([]fs.FileInfo, error)
	Stat(name string) (fs.FileInfo, error)
}

// Package is a candidate package.
type Package struct {
	Dir string
	// Internal is set when an element of the package's path within the
	// module is "internal": the package is importable only from within the
	// module and is not part of its public API.
	Internal bool
}

// Packages walks root and returns every package that contributes to the
// module's exported API, keyed by import path. Directories named testdata
// or vendor, directories starting with "." or "_", and nested modules are
// skipped, as are main packages and directories without buildable Go files.
// Directories named internal are skipped too unless internal is set, in
// which case their packages are returned with Internal marked.
//
// Directories that cannot be imported for reasons other than having no Go
// files (for instance, several package clauses in one directory) are
// reported in problems and otherwise skipped.
func Packages(ctxt *build.Context, fsys FS, root, modPath string, internal bool) (pkgs map[string]Package, problems map[string]string, err error) {
	pkgs = map[string]Package{}
	problems = map[string]string{}
	var walk func(dir, rel string, isInternal bool) error
	walk = func(dir, rel string, isInternal bool) error {
		importPath := modPath
		if rel != "" {
			importPath = modPath + "/" + rel
		}
		bp, ierr := ctxt.ImportDir(dir, 0)
		switch e := ierr.(type) {
		case nil:
			if bp.Name != "main" && len(bp.GoFiles) > 0 {
				pkgs[importPath] = Package{Dir: dir, Internal: isInternal}
			}
		case *build.NoGoError:
			// Nothing to diff here; still descend.
		case *build.MultiplePackageError:
			problems[importPath] = e.Error()
		default:
			problems[importPath] = ierr.Error()
		}

		entries, rerr := fsys.ReadDir(dir)
		if rerr != nil {
			return rerr
		}
		names := make([]string, 0, len(entries))
		byName := make(map[string]fs.FileInfo, len(entries))
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			names = append(names, e.Name())
			byName[e.Name()] = e
		}
		sort.Strings(names)
		for _, name := range names {
			if skipDir(name) || (name == "internal" && !internal) {
				continue
			}
			sub := ctxt.JoinPath(dir, name)
			if fi, serr := fsys.Stat(ctxt.JoinPath(sub, "go.mod")); serr == nil && !fi.IsDir() {
				continue // nested module
			}
			subRel := name
			if rel != "" {
				subRel = path.Join(rel, name)
			}
			if werr := walk(sub, subRel, isInternal || name == "internal"); werr != nil {
				return werr
			}
		}
		return nil
	}
	if err := walk(root, "", false); err != nil {
		return nil, nil, err
	}
	return pkgs, problems, nil
}

func skipDir(name string) bool {
	switch name {
	case "testdata", "vendor":
		return true
	}
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

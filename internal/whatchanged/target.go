package whatchanged

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/mod/module"
)

// target is one side as the command line names it, before anything is
// opened or resolved: a revision in a repository on disk, that repository's
// working tree, or a module version from outside any repository.
//
// The forms follow the go command's version suffixes: a module path with
// one, github.com/x/y@v1.2.0 or @latest or @main; a directory with one,
// ~/src/y@v1.2.0 or ./y@latest, naming a revision in that checkout; and a
// bare revision, v1.2.0 or HEAD~2, in the repository of the default
// directory. A head of "@query" alone applies the query to the base's
// repository or module, so that "v1.4.0 @main" and
// "github.com/x/y@latest @main" both read naturally.
type target struct {
	dir    string // repository directory of a git or working-tree side
	module string // module path of a module side
	query  string // revision, version or query; "" is the working tree
}

// parseTargets parses the two positional arguments. dir is the directory a
// bare revision refers to, "" for the current one. An empty base is HEAD; an
// empty head is the working tree of the base's repository, or @HEAD, the
// default branch, for a module base.
func parseTargets(baseArg, headArg, dir string) (base, head target, err error) {
	if dir == "" {
		dir = "."
	}
	if baseArg == "" {
		baseArg = DefaultBase
	}
	base, err = parseTarget(baseArg, target{dir: dir}, dir)
	if err != nil {
		return base, head, fmt.Errorf("base %q: %w", baseArg, err)
	}
	if base.module == "" && base.query == "" {
		base.query = DefaultBase // a directory alone: its HEAD
	}
	switch {
	case headArg != "":
		head, err = parseTarget(headArg, base, dir)
		if err != nil {
			return base, head, fmt.Errorf("head %q: %w", headArg, err)
		}
	case base.module != "":
		head = target{module: base.module, query: "HEAD"}
	default:
		head = target{dir: base.dir}
	}
	if head.module == "" && head.query == LatestRelease {
		return base, head, fmt.Errorf("%s can only be the base revision", LatestRelease)
	}
	return base, head, nil
}

// parseTarget parses one argument. inherit is the target a bare "@query"
// takes its repository or module from; dir is the repository directory of
// a bare revision.
func parseTarget(arg string, inherit target, dir string) (target, error) {
	if arg == "" {
		return target{}, errors.New("empty")
	}
	if strings.HasPrefix(arg, "@") {
		q := arg[1:]
		if q == "" {
			return target{}, errors.New("missing a version after @")
		}
		if inherit.module != "" {
			return target{module: inherit.module, query: q}, nil
		}
		d := inherit.dir
		if d == "" {
			d = dir
		}
		return target{dir: d, query: gitQuery(q)}, nil
	}
	before, after, hasAt := strings.Cut(arg, "@")
	switch {
	case isLocalPath(before):
		if hasAt && after == "" {
			return target{}, errors.New("missing a version after @")
		}
		if hasAt {
			after = gitQuery(after)
		}
		return target{dir: expandHome(before), query: after}, nil
	case !hasAt && strings.Contains(arg, "/") && module.CheckPath(arg) == nil:
		return target{}, fmt.Errorf("a module path needs a version: %s@latest, %s@v1.2.3", arg, arg)
	case !hasAt:
		return target{dir: dir, query: gitQuery(arg)}, nil
	case after == "":
		return target{}, errors.New("missing a version after @")
	case module.CheckPath(before) == nil:
		return target{module: before, query: after}, nil
	default:
		return target{}, fmt.Errorf("%q is neither a module path nor a directory", before)
	}
}

// gitQuery maps the version suffix of a git target to a revision: "latest"
// is the newest release tag, as the bare @latest is.
func gitQuery(q string) string {
	if q == "latest" {
		return LatestRelease
	}
	return q
}

// isLocalPath reports whether p names a directory rather than a module path
// or a revision: it is spelled as a path (".", "..", "~", or starting with
// "./", "../", "~/" or a root), or it has a path separator and exists as a
// directory. A bare name is never a path, so a branch named like a
// directory is still a branch; "./name" is the directory.
func isLocalPath(p string) bool {
	switch {
	case p == "" || p == "@":
		return false
	case p == "." || p == ".." || p == "~":
		return true
	case strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "/"):
		return true
	case filepath.IsAbs(p) || strings.HasPrefix(p, "."+string(filepath.Separator)) || strings.HasPrefix(p, ".."+string(filepath.Separator)):
		return true
	case strings.ContainsAny(p, `/\`):
		fi, err := os.Stat(expandHome(p))
		return err == nil && fi.IsDir()
	}
	return false
}

// expandHome replaces a leading ~ with the home directory, for a path that
// reached us quoted or through an environment where the shell did not.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

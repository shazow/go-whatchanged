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
// Every side has the one form of the go command's version suffixes,
// location@query. The location is a module path, github.com/x/y@v1.2.0; a
// directory spelled as a path, ~/src/y@v1.2.0 or ./y@latest, naming a
// revision of that checkout; or empty, @v1.2.0, for the base's repository
// or module, which for the base itself is the repository of the default
// directory. A location alone is that checkout's HEAD as base and its
// working tree as head; a module needs a version.
type target struct {
	dir    string // repository directory of a git or working-tree side
	module string // module path of a module side
	query  string // revision, version or query; "" is the working tree
}

// parseTargets parses the two positional arguments. An empty location in
// the base is the current directory. An empty base is HEAD; an empty head is
// the working tree of the base's repository, or @HEAD, the default branch,
// for a module base.
func parseTargets(baseArg, headArg string) (base, head target, err error) {
	base = target{dir: ".", query: DefaultBase}
	if baseArg != "" {
		if base, err = parseTarget(baseArg, target{dir: "."}); err != nil {
			return base, head, fmt.Errorf("base %q: %w", baseArg, err)
		}
	}
	if base.query == "" {
		if base.module != "" {
			return base, head, fmt.Errorf("base %q: a module needs a version: %s@latest, %s@v1.2.3", baseArg, base.module, base.module)
		}
		base.query = DefaultBase
	}
	switch {
	case headArg != "":
		if head, err = parseTarget(headArg, base); err != nil {
			return base, head, fmt.Errorf("head %q: %w", headArg, err)
		}
	case base.module != "":
		head = target{module: base.module, query: "HEAD"}
	default:
		head = target{dir: base.dir}
	}
	switch {
	case head.module != "" && head.query == "":
		return base, head, fmt.Errorf("head %q: a module needs a version: %s@HEAD, %s@v1.2.3", headArg, head.module, head.module)
	case head.module == "" && head.query == LatestRelease:
		return base, head, fmt.Errorf("%s can only be the base revision", LatestRelease)
	}
	return base, head, nil
}

// parseTarget parses one location@query argument. inherit supplies the
// repository or module of an empty location.
func parseTarget(arg string, inherit target) (target, error) {
	if arg == "" {
		return target{}, errors.New("empty")
	}
	// The first @ ends the location: module paths have none, and a
	// revision may, as in main@{upstream}.
	loc, query, hasAt := strings.Cut(arg, "@")
	if hasAt && query == "" {
		return target{}, errors.New("missing a version after @")
	}
	switch {
	case loc == "":
		if inherit.module != "" {
			return target{module: inherit.module, query: query}, nil
		}
		return target{dir: inherit.dir, query: gitQuery(query)}, nil
	case isLocalPath(loc):
		return target{dir: expandHome(loc), query: gitQuery(query)}, nil
	case strings.Contains(loc, "/") && module.CheckPath(loc) == nil:
		return target{module: loc, query: query}, nil
	default:
		// A revision of the current repository lacks its @; a directory
		// that exists lacks its ./ .
		hint := "@" + arg
		if fi, err := os.Stat(loc); err == nil && fi.IsDir() {
			hint = "./" + arg
		}
		return target{}, fmt.Errorf("%q is neither a module path nor a directory (a directory starts with ./, ../, ~/ or /; a revision of the current repository with @); did you mean %s?", loc, hint)
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

// isLocalPath reports whether p is spelled as a directory rather than a
// module path: ".", "..", "~", or starting with "./", "../", "~/" or a
// root, as the go command tells a directory from an import path.
func isLocalPath(p string) bool {
	switch {
	case p == "." || p == ".." || p == "~":
		return true
	case strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "/"):
		return true
	case filepath.IsAbs(p) || strings.HasPrefix(p, "."+string(filepath.Separator)) || strings.HasPrefix(p, ".."+string(filepath.Separator)):
		return true
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

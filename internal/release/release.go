// Package release maps a module's semantic version tags to versions and
// computes the version a set of API changes calls for. It only reasons about
// strings; the git side lives with the caller.
package release

import (
	"fmt"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/shazow/go-whatchanged/internal/render"
)

// Tags describes the release tags of one module inside a repository, as the
// go command reads them: a module in a subdirectory is tagged with the
// directory as a prefix ("sub/v1.2.3") and a module with a major version
// suffix only accepts tags of that major ("v2.x.y" for "example.com/m/v2").
type Tags struct {
	// Prefix precedes the version in every tag name; "" or "dir/".
	Prefix string
	// Major is the module path's major version suffix ("/v2", ".v2") or "".
	Major string
}

// TagsFor returns the tag layout for the module modPath rooted at dir, a
// slash-separated path relative to the repository root ("" for the root).
// A module whose directory is named after its major version, the "major
// subdirectory" layout (example.com/m/v2 in v2/), is tagged without the
// directory prefix, like the go command expects.
func TagsFor(modPath, dir string) (Tags, error) {
	_, major, ok := module.SplitPathVersion(modPath)
	if !ok {
		return Tags{}, fmt.Errorf("invalid module path %q", modPath)
	}
	prefix := strings.Trim(dir, "/")
	if suffix := strings.TrimPrefix(major, "/"); suffix != "" && suffix != major {
		switch {
		case prefix == suffix:
			prefix = ""
		case strings.HasSuffix(prefix, "/"+suffix):
			prefix = strings.TrimSuffix(prefix, "/"+suffix)
		}
	}
	if prefix != "" {
		prefix += "/"
	}
	return Tags{Prefix: prefix, Major: major}, nil
}

// Version returns the version a tag name denotes for the module, or "" when
// the tag is not one of its release tags: wrong prefix, not a canonical
// semantic version ("v1.2", "1.2.3" and "v1.2.3+meta" are ignored, as the
// go command ignores them) or a major version that belongs to another
// module path.
func (t Tags) Version(tag string) string {
	v, ok := strings.CutPrefix(tag, t.Prefix)
	if !ok || !strings.HasPrefix(v, "v") || semver.Canonical(v) != v {
		return ""
	}
	if err := module.CheckPathMajor(v, t.Major); err != nil {
		return ""
	}
	return v
}

// Example returns a sample tag name for error messages.
func (t Tags) Example() string {
	v := "v1.2.3"
	if t.Major != "" {
		v = "v" + strings.TrimLeft(t.Major, "/.v") + ".2.3"
	}
	return t.Prefix + v
}

// Next returns the smallest version after v that changes of the given level
// allow: a new major for Major (the next minor while still at v0, which
// promises nothing), a new minor for Minor and a new patch for Patch. A
// pre-release such as v1.5.0-rc.1 may be finalized with any change, so it
// always yields its release, v1.5.0. v must be canonical.
func Next(v string, lvl render.Level) string {
	major, minor, patch, ok := parse(v)
	if !ok {
		return ""
	}
	if semver.Prerelease(v) != "" {
		return fmt.Sprintf("v%d.%d.%d", major, minor, patch)
	}
	switch {
	case lvl == render.Major && major > 0:
		return fmt.Sprintf("v%d.0.0", major+1)
	case lvl == render.Major || lvl == render.Minor:
		return fmt.Sprintf("v%d.%d.0", major, minor+1)
	default:
		return fmt.Sprintf("v%d.%d.%d", major, minor, patch+1)
	}
}

// parse splits a canonical version into its numeric components.
func parse(v string) (major, minor, patch int, ok bool) {
	if v == "" || semver.Canonical(v) != v {
		return 0, 0, 0, false
	}
	// Scanning stops at the pre-release suffix, if any.
	_, err := fmt.Sscanf(v, "v%d.%d.%d", &major, &minor, &patch)
	return major, minor, patch, err == nil
}

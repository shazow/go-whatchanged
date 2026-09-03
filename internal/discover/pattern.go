package discover

import (
	"regexp"
	"strings"
)

// Filter selects packages by import path pattern. A pattern is a full import
// path ("example.com/m/store"), a path relative to the module root
// ("store", "./store"), or either with "..." standing for any sequence of
// characters, as in the go command: "store/..." matches the store package
// and everything below it, "..." matches every package. With no Include
// patterns every package is selected; Exclude patterns then remove
// packages from the selection.
type Filter struct {
	Include []string
	Exclude []string
}

// IsZero reports whether the filter selects every package.
func (f Filter) IsZero() bool {
	return len(f.Include) == 0 && len(f.Exclude) == 0
}

// Match reports whether the package with importPath in the module modPath
// is selected.
func (f Filter) Match(modPath, importPath string) bool {
	if len(f.Include) > 0 && !MatchAny(f.Include, modPath, importPath) {
		return false
	}
	return !MatchAny(f.Exclude, modPath, importPath)
}

// MatchAny reports whether any pattern matches importPath.
func MatchAny(patterns []string, modPath, importPath string) bool {
	for _, p := range patterns {
		if MatchPattern(p, modPath, importPath) {
			return true
		}
	}
	return false
}

// MatchPattern reports whether pattern matches importPath, a package of the
// module modPath.
func MatchPattern(pattern, modPath, importPath string) bool {
	pattern = strings.TrimSuffix(strings.TrimPrefix(pattern, "./"), "/")
	if pattern == "." || pattern == "" {
		return importPath == modPath
	}
	re := compile(pattern)
	if re.MatchString(importPath) {
		return true
	}
	if rel, ok := strings.CutPrefix(importPath, modPath+"/"); ok {
		return re.MatchString(rel)
	}
	return false
}

// compile turns a pattern into an anchored regular expression: "..."
// matches anything, and a trailing "/..." also matches the path without it,
// so that "a/..." matches "a" itself.
func compile(pattern string) *regexp.Regexp {
	re := strings.ReplaceAll(regexp.QuoteMeta(pattern), `\.\.\.`, `.*`)
	if strings.HasSuffix(re, `/.*`) {
		re = strings.TrimSuffix(re, `/.*`) + `(/.*)?`
	}
	return regexp.MustCompile(`^` + re + `$`)
}

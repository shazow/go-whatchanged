package discover

import (
	"regexp"
	"strings"
)

// Filter selects packages by import path pattern. A pattern is a full import
// path ("example.com/m/store"), a path relative to the module root
// ("store", "./store"), or either with "..." standing for any sequence of
// characters, as in the go command: "store/..." matches the store package
// and everything below it, "..." matches every package. With no include
// patterns every package is selected; exclude patterns then remove
// packages from the selection.
type Filter struct {
	include, exclude []Pattern
}

// NewFilter compiles the include and exclude patterns.
func NewFilter(include, exclude []string) Filter {
	var f Filter
	for _, p := range include {
		f.include = append(f.include, Compile(p))
	}
	for _, p := range exclude {
		f.exclude = append(f.exclude, Compile(p))
	}
	return f
}

// Match reports whether the package with importPath in the module modPath
// is selected.
func (f Filter) Match(modPath, importPath string) bool {
	if len(f.include) > 0 && !matchAny(f.include, modPath, importPath) {
		return false
	}
	return !matchAny(f.exclude, modPath, importPath)
}

func matchAny(patterns []Pattern, modPath, importPath string) bool {
	for _, p := range patterns {
		if p.Match(modPath, importPath) {
			return true
		}
	}
	return false
}

// Pattern is a compiled package pattern (see Filter).
type Pattern struct {
	root bool           // the pattern names the module's root package
	re   *regexp.Regexp // anchored; nil when root is set
}

// Compile compiles a pattern. "..." matches anything, and a trailing "/..."
// also matches the path without it, so that "a/..." matches "a" itself.
func Compile(pattern string) Pattern {
	pattern = strings.TrimSuffix(strings.TrimPrefix(pattern, "./"), "/")
	if pattern == "." || pattern == "" {
		return Pattern{root: true}
	}
	re := strings.ReplaceAll(regexp.QuoteMeta(pattern), `\.\.\.`, `.*`)
	if prefix, ok := strings.CutSuffix(re, `/.*`); ok {
		re = prefix + `(/.*)?`
	}
	return Pattern{re: regexp.MustCompile(`^` + re + `$`)}
}

// Match reports whether the pattern matches importPath, a package of the
// module modPath, either as an import path or as a path relative to the
// module root.
func (p Pattern) Match(modPath, importPath string) bool {
	if p.root {
		return importPath == modPath
	}
	if p.re.MatchString(importPath) {
		return true
	}
	if rel, ok := strings.CutPrefix(importPath, modPath+"/"); ok {
		return p.re.MatchString(rel)
	}
	return false
}

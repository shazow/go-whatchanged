// Package render prints API diffs in a colorized, git-diff-like layout.
package render

import (
	"fmt"
	"io"
	"strings"

	"golang.org/x/exp/apidiff"
)

// Status says on which sides a package exists.
type Status int

const (
	// Both means the package exists on both sides.
	Both Status = iota
	// New means the package exists only on the head side.
	New
	// Removed means the package exists only on the base side.
	Removed
)

// Package is the diff of one package.
type Package struct {
	Path    string
	Status  Status
	Changes []apidiff.Change
}

// Warning is a non-fatal problem encountered while loading one side.
type Warning struct {
	Package string
	Message string
}

// Result is everything Write needs.
type Result struct {
	Packages []Package // sorted by path; packages without changes may be included and are skipped
	Warnings []Warning
}

// Options controls rendering.
type Options struct {
	Color        bool
	BreakingOnly bool
}

// Summary describes the totals of a Result.
type Summary struct {
	PackagesChanged int
	Incompatible    int
	Compatible      int
}

// Summarize counts changes across all packages.
func Summarize(res Result) Summary {
	var s Summary
	for _, p := range res.Packages {
		if len(p.Changes) == 0 {
			continue
		}
		s.PackagesChanged++
		for _, c := range p.Changes {
			if c.Compatible {
				s.Compatible++
			} else {
				s.Incompatible++
			}
		}
	}
	return s
}

// Release returns the semantic version bump the changes require.
func (s Summary) Release() string {
	switch {
	case s.Incompatible > 0:
		return "MAJOR"
	case s.Compatible > 0:
		return "MINOR"
	default:
		return "PATCH"
	}
}

// Write renders the diff to w.
func Write(w io.Writer, res Result, opts Options) error {
	st := Style{Enabled: opts.Color}
	sum := Summarize(res)
	if sum.PackagesChanged == 0 {
		_, err := fmt.Fprintln(w, st.Dim("no exported API changes"))
		return err
	}

	var b strings.Builder
	first := true
	for _, p := range res.Packages {
		lines := make([]string, 0, len(p.Changes))
		for _, c := range p.Changes {
			if opts.BreakingOnly && c.Compatible {
				continue
			}
			lines = append(lines, "  "+formatChange(st, c))
		}
		if len(lines) == 0 {
			continue
		}
		if !first {
			b.WriteString("\n")
		}
		first = false
		header := p.Path
		switch p.Status {
		case New:
			header += " (new)"
		case Removed:
			header += " (removed)"
		}
		b.WriteString(st.Bold(header))
		b.WriteString("\n")
		for _, l := range lines {
			b.WriteString(l)
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(formatSummary(st, sum))
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// formatChange maps an apidiff change to a glyph and color.
func formatChange(st Style, c apidiff.Change) string {
	msg := c.Message
	bold := !c.Compatible
	switch {
	case strings.HasSuffix(msg, ": removed"):
		return st.Red("- "+msg, bold)
	case strings.HasSuffix(msg, ": added"):
		if c.Compatible {
			return st.Green("+ "+msg, false)
		}
		return st.Red("! "+msg, true)
	default:
		if c.Compatible {
			return st.Cyan("~ "+msg, false)
		}
		return st.Yellow("~ "+msg, true)
	}
}

func formatSummary(st Style, sum Summary) string {
	release := sum.Release()
	var colored string
	switch release {
	case "MAJOR":
		colored = st.Red(release, true)
	case "MINOR":
		colored = st.Yellow(release, true)
	default:
		colored = st.Green(release, true)
	}
	return fmt.Sprintf("%s · %d incompatible · %d compatible · would require: %s",
		plural(sum.PackagesChanged, "package"), sum.Incompatible, sum.Compatible, colored)
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s changed", noun)
	}
	return fmt.Sprintf("%d %ss changed", n, noun)
}

// WriteWarnings prints warnings, one per line, in dim yellow.
func WriteWarnings(w io.Writer, warnings []Warning, color bool) error {
	st := Style{Enabled: color}
	for _, wn := range warnings {
		line := "warn: " + wn.Package + ": " + wn.Message
		if _, err := fmt.Fprintln(w, st.DimYellow(line)); err != nil {
			return err
		}
	}
	return nil
}

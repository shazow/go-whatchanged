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

// Change is one API change. Before and After optionally carry the
// declaration-style form of the symbol on each side for "changed from X to
// Y" messages, such as "func Open(path string) (*Client, error)"; when they
// are empty the renderer falls back to the types quoted in the message.
type Change struct {
	Message    string
	Compatible bool
	Before     string
	After      string
}

// FromAPIDiff converts an apidiff change without named forms.
func FromAPIDiff(c apidiff.Change) Change {
	return Change{Message: c.Message, Compatible: c.Compatible}
}

// Package is the diff of one package.
type Package struct {
	Path    string
	Status  Status
	Changes []Change
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

// Level is the semantic version bump a set of changes requires. Levels are
// ordered: Patch < Minor < Major.
type Level int

const (
	// Patch means no exported API changes.
	Patch Level = iota
	// Minor means only compatible changes.
	Minor
	// Major means at least one incompatible change.
	Major
)

// String returns the level in upper case, as printed in the summary line.
func (l Level) String() string {
	switch l {
	case Major:
		return "MAJOR"
	case Minor:
		return "MINOR"
	default:
		return "PATCH"
	}
}

// ParseLevel parses a level name, case-insensitively: "major", "minor" or
// "patch".
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(s) {
	case "major":
		return Major, nil
	case "minor":
		return Minor, nil
	case "patch":
		return Patch, nil
	}
	return 0, fmt.Errorf("invalid level %q (want major, minor or patch)", s)
}

// Level returns the semantic version bump the changes require.
func (s Summary) Level() Level {
	switch {
	case s.Incompatible > 0:
		return Major
	case s.Compatible > 0:
		return Minor
	default:
		return Patch
	}
}

// Release returns the semantic version bump the changes require, as a string.
func (s Summary) Release() string {
	return s.Level().String()
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
			lines = append(lines, formatChange(st, c)...)
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

	if !first {
		b.WriteString("\n")
	}
	b.WriteString(formatSummary(st, sum))
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// formatChange maps an apidiff change to one or more indented lines with a
// glyph and color. "changed from X to Y" messages are split so that the
// before and after values sit on their own "-" and "+" lines, like a small
// patch, which makes long signatures easy to compare.
func formatChange(st Style, c Change) []string {
	bold := !c.Compatible
	paint := func(text string) string {
		switch {
		case strings.HasSuffix(c.Message, " removed"):
			return st.Red(text, bold)
		case strings.HasSuffix(c.Message, " added"):
			if c.Compatible {
				return st.Green(text, false)
			}
			return st.Red(text, true)
		case c.Compatible:
			return st.Cyan(text, false)
		default:
			return st.Yellow(text, true)
		}
	}
	glyph := "~ "
	switch {
	case strings.HasSuffix(c.Message, " removed"):
		glyph = "- "
	case strings.HasSuffix(c.Message, " added"):
		if c.Compatible {
			glyph = "+ "
		} else {
			glyph = "! "
		}
	}

	if head, from, to, ok := splitChangedFromTo(c.Message); ok {
		if c.Before != "" && c.After != "" {
			from, to = c.Before, c.After
		}
		return []string{
			"  " + paint(glyph+head),
			"      " + st.Red("- "+from, bold),
			"      " + st.Green("+ "+to, bold),
		}
	}
	return []string{"  " + paint(glyph+c.Message)}
}

// splitChangedFromTo decomposes "<obj>: [value ]changed from X to Y" into
// "<obj>: [value ]changed", X and Y. Type strings never contain " to "
// themselves, so the first occurrence after "changed from " is the split.
func splitChangedFromTo(msg string) (head, from, to string, ok bool) {
	const marker = "changed from "
	i := strings.Index(msg, marker)
	if i < 0 {
		return "", "", "", false
	}
	rest := msg[i+len(marker):]
	from, to, found := strings.Cut(rest, " to ")
	if !found || from == "" || to == "" {
		return "", "", "", false
	}
	return msg[:i] + "changed", from, to, true
}

func formatSummary(st Style, sum Summary) string {
	release := sum.Release()
	var colored string
	switch sum.Level() {
	case Major:
		colored = st.Red(release, true)
	case Minor:
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

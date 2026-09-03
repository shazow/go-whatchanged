// Package render prints API diffs in a colorized, git-diff-like layout, as
// Markdown for pull requests and job summaries, or as JSON for tools.
package render

import (
	"encoding/json"
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

// String returns the status as printed in JSON output.
func (s Status) String() string {
	switch s {
	case New:
		return "new"
	case Removed:
		return "removed"
	default:
		return "changed"
	}
}

// Change is one API change. Message is an apidiff message, "<symbol>: <what>",
// or "package added" / "package removed" for a package without exported
// symbols. Before and After optionally carry the declaration-style form of
// the symbol on each side for "changed from X to Y" messages, such as "func
// Open(path string) (*Client, error)"; when they are empty the renderer
// falls back to the types quoted in the message.
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

// Symbol returns the object the message is about: "Open", "(*Client).Ping",
// "Point.Z". It is "" for a whole-package change. Symbols never contain
// ": ", and neither do the type strings that follow, so the first
// occurrence is the boundary.
func (c Change) Symbol() string {
	symbol, _, ok := strings.Cut(c.Message, ": ")
	if !ok {
		return ""
	}
	return symbol
}

// Kind classifies the change as "added", "removed" or "changed", from the
// text after the symbol (or the whole message for a whole-package change).
func (c Change) Kind() string {
	what := c.Message
	if _, rest, ok := strings.Cut(c.Message, ": "); ok {
		what = rest
	}
	switch what {
	case "added", "package added":
		return "added"
	case "removed", "package removed":
		return "removed"
	}
	return "changed"
}

// Package is the diff of one package.
type Package struct {
	Path    string
	Status  Status
	Changes []Change
}

// Warning is a non-fatal problem encountered while loading one side.
type Warning struct {
	Package string `json:"package"`
	Message string `json:"message"`
}

// Result is everything Write needs.
type Result struct {
	// Base and Head label the two sides: a revision as the user named it
	// (or the tag @latest resolved to), or "working tree".
	Base, Head string
	// BaseVersion is the semantic version the base side's tag denotes, when
	// the base is a release tag of the module; NextVersion is the version
	// the changes call for. Both are empty otherwise.
	BaseVersion, NextVersion string

	Packages []Package // sorted by path; packages without changes may be included and are skipped
	Warnings []Warning
}

// Format selects an output layout.
type Format int

const (
	// Text is the colorized terminal layout.
	Text Format = iota
	// Markdown renders each package as a fenced diff block, for pull request
	// comments and GitHub job summaries.
	Markdown
	// JSON renders the whole result as one JSON document.
	JSON
)

// ParseFormat parses a --format value: "text", "markdown" or "json".
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(s) {
	case "text":
		return Text, nil
	case "markdown", "md":
		return Markdown, nil
	case "json":
		return JSON, nil
	}
	return 0, fmt.Errorf("invalid format %q (want text, markdown or json)", s)
}

// Options controls rendering.
type Options struct {
	Color        bool
	BreakingOnly bool
	Format       Format
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

// Write renders the diff to w in the layout opts.Format selects.
func Write(w io.Writer, res Result, opts Options) error {
	switch opts.Format {
	case JSON:
		return writeJSON(w, res, opts)
	case Markdown:
		return writeMarkdown(w, res, opts)
	default:
		return writeText(w, res, opts)
	}
}

// line is one change reduced to a glyph, a head line and, for "changed from
// X to Y" messages, the two values to show on their own "-" and "+" lines.
type line struct {
	glyph      string // "-", "!", "~" or "+"
	head       string
	from, to   string // both empty unless the change splits
	compatible bool
}

// describe reduces c to a line. "changed from X to Y" messages are split so
// that the before and after values sit on their own lines, like a small
// patch, which makes long signatures easy to compare.
func describe(c Change) line {
	l := line{glyph: "~", head: c.Message, compatible: c.Compatible}
	switch c.Kind() {
	case "removed":
		l.glyph = "-"
	case "added":
		if c.Compatible {
			l.glyph = "+"
		} else {
			l.glyph = "!"
		}
	}
	if head, from, to, ok := splitChangedFromTo(c.Message); ok {
		if c.Before != "" && c.After != "" {
			from, to = c.Before, c.After
		}
		l.head, l.from, l.to = head, from, to
	}
	return l
}

// packageLines returns the changes of p to show, honoring BreakingOnly.
func packageLines(p Package, opts Options) []line {
	lines := make([]line, 0, len(p.Changes))
	for _, c := range p.Changes {
		if opts.BreakingOnly && c.Compatible {
			continue
		}
		lines = append(lines, describe(c))
	}
	return lines
}

func header(p Package) string {
	switch p.Status {
	case New:
		return p.Path + " (new)"
	case Removed:
		return p.Path + " (removed)"
	}
	return p.Path
}

func writeText(w io.Writer, res Result, opts Options) error {
	st := Style{Enabled: opts.Color}
	sum := Summarize(res)
	if sum.PackagesChanged == 0 {
		_, err := fmt.Fprintln(w, st.Dim(noChanges(res)))
		return err
	}

	var b strings.Builder
	first := true
	for _, p := range res.Packages {
		lines := packageLines(p, opts)
		if len(lines) == 0 {
			continue
		}
		if !first {
			b.WriteString("\n")
		}
		first = false
		b.WriteString(st.Bold(header(p)))
		b.WriteString("\n")
		for _, l := range lines {
			for _, s := range formatLine(st, l) {
				b.WriteString(s)
				b.WriteString("\n")
			}
		}
	}

	if !first {
		b.WriteString("\n")
	}
	b.WriteString(formatSummary(st, sum, res))
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// formatLine maps a line to one or more indented, colored terminal lines.
func formatLine(st Style, l line) []string {
	bold := !l.compatible
	var paint func(string) string
	switch {
	case l.glyph == "-" || l.glyph == "!":
		paint = func(s string) string { return st.Red(s, true) }
	case l.glyph == "+":
		paint = func(s string) string { return st.Green(s, false) }
	case l.compatible:
		paint = func(s string) string { return st.Cyan(s, false) }
	default:
		paint = func(s string) string { return st.Yellow(s, true) }
	}
	out := []string{"  " + paint(l.glyph+" "+l.head)}
	if l.from != "" {
		out = append(out,
			"      "+st.Red("- "+l.from, bold),
			"      "+st.Green("+ "+l.to, bold))
	}
	return out
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

// noChanges is the message for an empty diff, naming the base release when
// there is one so that @latest shows what it resolved to.
func noChanges(res Result) string {
	if res.BaseVersion != "" {
		return "no exported API changes since " + res.BaseVersion
	}
	return "no exported API changes"
}

func formatSummary(st Style, sum Summary, res Result) string {
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
	return fmt.Sprintf("%s · %d incompatible · %d compatible · would require: %s%s",
		plural(sum.PackagesChanged, "package"), sum.Incompatible, sum.Compatible, colored, versions(res))
}

// versions is the " (v1.4.0 → v1.5.0)" suffix of the summary line, or "".
func versions(res Result) string {
	if res.BaseVersion == "" || res.NextVersion == "" {
		return ""
	}
	return fmt.Sprintf(" (%s → %s)", res.BaseVersion, res.NextVersion)
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s changed", noun)
	}
	return fmt.Sprintf("%d %ss changed", n, noun)
}

// writeMarkdown renders each package as a bold path followed by a fenced
// diff block, which GitHub colors: "-" lines red, "+" lines green. An
// incompatible change is marked "!" and a compatible one "~".
func writeMarkdown(w io.Writer, res Result, opts Options) error {
	sum := Summarize(res)
	if sum.PackagesChanged == 0 {
		_, err := fmt.Fprintf(w, "_%s_\n", noChanges(res))
		return err
	}

	var b strings.Builder
	for _, p := range res.Packages {
		lines := packageLines(p, opts)
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(&b, "**%s**\n\n```diff\n", header(p))
		for _, l := range lines {
			glyph := l.glyph
			if glyph == "~" && !l.compatible {
				glyph = "!"
			}
			b.WriteString(glyph + " " + l.head + "\n")
			if l.from != "" {
				b.WriteString("-   " + l.from + "\n")
				b.WriteString("+   " + l.to + "\n")
			}
		}
		b.WriteString("```\n\n")
	}
	fmt.Fprintf(&b, "%s · %d incompatible · %d compatible · would require: **%s**%s\n",
		plural(sum.PackagesChanged, "package"), sum.Incompatible, sum.Compatible, sum.Release(), versions(res))
	_, err := io.WriteString(w, b.String())
	return err
}

// JSON layout. Field names are part of the tool's interface.
type jsonReport struct {
	Base        string        `json:"base"`
	Head        string        `json:"head"`
	BaseVersion string        `json:"base_version,omitempty"`
	NextVersion string        `json:"next_version,omitempty"`
	Packages    []jsonPackage `json:"packages"`
	Warnings    []Warning     `json:"warnings"`
	Summary     jsonSummary   `json:"summary"`
}

type jsonPackage struct {
	Path    string       `json:"path"`
	Status  string       `json:"status"`
	Changes []jsonChange `json:"changes"`
}

type jsonChange struct {
	Symbol     string `json:"symbol"`
	Kind       string `json:"kind"`
	Compatible bool   `json:"compatible"`
	Message    string `json:"message"`
	Before     string `json:"before,omitempty"`
	After      string `json:"after,omitempty"`
}

type jsonSummary struct {
	PackagesChanged int    `json:"packages_changed"`
	Incompatible    int    `json:"incompatible"`
	Compatible      int    `json:"compatible"`
	Release         string `json:"release"`
}

// writeJSON renders the result as one indented JSON document. Only packages
// with changes to show are listed (BreakingOnly filters the changes, and a
// package left without any is dropped); the summary always counts the full
// diff, as in the other layouts.
func writeJSON(w io.Writer, res Result, opts Options) error {
	sum := Summarize(res)
	rep := jsonReport{
		Base:        res.Base,
		Head:        res.Head,
		BaseVersion: res.BaseVersion,
		NextVersion: res.NextVersion,
		Packages:    []jsonPackage{},
		Warnings:    []Warning{},
		Summary: jsonSummary{
			PackagesChanged: sum.PackagesChanged,
			Incompatible:    sum.Incompatible,
			Compatible:      sum.Compatible,
			Release:         strings.ToLower(sum.Release()),
		},
	}
	for _, p := range res.Packages {
		if len(p.Changes) == 0 {
			continue
		}
		jp := jsonPackage{Path: p.Path, Status: p.Status.String(), Changes: []jsonChange{}}
		for _, c := range p.Changes {
			if opts.BreakingOnly && c.Compatible {
				continue
			}
			l := describe(c)
			jp.Changes = append(jp.Changes, jsonChange{
				Symbol:     c.Symbol(),
				Kind:       c.Kind(),
				Compatible: c.Compatible,
				Message:    c.Message,
				Before:     l.from,
				After:      l.to,
			})
		}
		if len(jp.Changes) == 0 {
			continue
		}
		rep.Packages = append(rep.Packages, jp)
	}
	rep.Warnings = append(rep.Warnings, res.Warnings...)

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(rep)
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

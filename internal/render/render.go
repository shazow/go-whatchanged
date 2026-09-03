// Package render prints API diffs in a colorized, git-diff-like layout, as
// Markdown for pull requests and job summaries, or as JSON for tools.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
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

// Position locates a symbol's declaration on one side of the diff.
type Position struct {
	// Rev is the revision the file was read from, "" for the working tree.
	Rev string `json:"rev,omitempty"`
	// File is the path relative to the module root, slash-separated.
	File string `json:"file"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
}

// IsZero reports whether the position is unknown.
func (p Position) IsZero() bool { return p.File == "" }

// String formats the position as "file:line:col", prefixed with "rev:" for
// a file read from a revision, like the positions in warnings.
func (p Position) String() string {
	if p.IsZero() {
		return ""
	}
	s := fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Col)
	if p.Rev != "" {
		s = p.Rev + ":" + s
	}
	return s
}

// Change is one API change. Message is an apidiff message, "<symbol>: <what>",
// or "package added" / "package removed" for a package without exported
// symbols. Before and After carry the declaration-style form of the symbol
// on each side, such as "func Open(path string) (*Client, error)": Before
// for a removal, After for an addition, both for a "changed from X to Y"
// message (where the renderer falls back to the types quoted in the message
// when they are empty). Pos locates the symbol's declaration: on the base
// side for a removal, on the head side otherwise.
type Change struct {
	Message    string
	Compatible bool
	Before     string
	After      string
	Pos        Position
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

// Package is the diff of one package. An Internal package (one below an
// internal directory) is shown but kept out of the public API's counts and
// required release level.
type Package struct {
	Path     string
	Status   Status
	Internal bool
	Changes  []Change
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

// ParseFormat parses a --format value: "text", "markdown" (or "md") or "json".
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

// Signatures selects how much of a symbol's declaration a change shows.
type Signatures int

const (
	// FullSignatures shows each change as the declaration of its symbol,
	// like a small patch: the old one on a "-" line, the new one on a "+"
	// line, both for a changed symbol.
	FullSignatures Signatures = iota
	// MinimalSignatures prints one line per change, the apidiff message,
	// with a changed symbol's old and new types quoted inline.
	MinimalSignatures
)

// ParseSignatures parses a --signatures value: "full" or "minimal".
func ParseSignatures(s string) (Signatures, error) {
	switch strings.ToLower(s) {
	case "full":
		return FullSignatures, nil
	case "minimal":
		return MinimalSignatures, nil
	}
	return 0, fmt.Errorf("invalid signatures %q (want full or minimal)", s)
}

// Visibility selects which packages take part in a diff.
type Visibility int

const (
	// All selects every package: the public API and the internal packages.
	All Visibility = iota
	// Public selects the packages importable from outside the module:
	// everything but internal packages.
	Public
	// Internal selects internal packages only.
	Internal
)

// ParseVisibility parses a --filter value: "all", "public" or "internal".
func ParseVisibility(s string) (Visibility, error) {
	switch strings.ToLower(s) {
	case "all":
		return All, nil
	case "public":
		return Public, nil
	case "internal":
		return Internal, nil
	}
	return 0, fmt.Errorf("invalid filter %q (want all, public or internal)", s)
}

// Includes reports whether a package with the given internal status is
// selected.
func (v Visibility) Includes(internal bool) bool {
	switch v {
	case Public:
		return !internal
	case Internal:
		return internal
	}
	return true
}

// Options controls rendering.
type Options struct {
	Color        bool
	BreakingOnly bool
	Format       Format
	// Signatures selects whether declarations are shown; the zero value
	// shows them in full.
	Signatures Signatures
	// Positions annotates each change with the position of its declaration.
	Positions bool
	// Filter says which packages took part: the public API, its packages
	// and summary line, is printed unless it is Internal, and the internal
	// packages with their own summary line after it unless it is Public
	// (with All, only when some internal package changed). Internal
	// packages never count towards the public API's summary.
	Filter Visibility
}

// Summary describes the totals of a Result.
type Summary struct {
	PackagesChanged int
	Incompatible    int
	Compatible      int
}

// Summarize counts the changes of the public API: every package that is
// not internal.
func Summarize(res Result) Summary {
	return summarize(res, false)
}

// SummarizeInternal counts the changes of internal packages.
func SummarizeInternal(res Result) Summary {
	return summarize(res, true)
}

func summarize(res Result, internal bool) Summary {
	var s Summary
	for _, p := range res.Packages {
		if len(p.Changes) == 0 || p.Internal != internal {
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

// line is one change reduced to a glyph, a message ("Open: changed") and
// the old and new declarations to show on "-" (from) and "+" (to) lines.
// In the text and Markdown layouts the declarations stand in for the
// message; decls says that from and to are declarations, as opposed to the
// bare types quoted in a "changed from X to Y" message whose symbol could
// not be looked up on both sides, which do not name the symbol.
type line struct {
	glyph      string // "-", "!", "~" or "+"
	head       string
	from, to   string // the old and new declarations; either may be empty
	decls      bool
	pos        string // "" when unknown or not wanted
	compatible bool
}

// describe reduces c to a line. With FullSignatures a removed symbol's
// declaration goes on a "-" line, an added one's on a "+" line, and a
// "changed from X to Y" message is split so that the before and after values
// sit on their own lines, like a small patch, which makes long signatures
// easy to compare.
func describe(c Change, opts Options) line {
	l := line{glyph: "~", head: c.Message, compatible: c.Compatible}
	kind := c.Kind()
	switch kind {
	case "removed":
		l.glyph = "-"
	case "added":
		if c.Compatible {
			l.glyph = "+"
		} else {
			l.glyph = "!"
		}
	}
	if opts.Positions {
		l.pos = c.Pos.String()
	}
	if opts.Signatures == MinimalSignatures {
		return l
	}
	switch kind {
	case "removed":
		l.from, l.decls = c.Before, c.Before != ""
	case "added":
		l.to, l.decls = c.After, c.After != ""
	default:
		head, rest, ok := strings.Cut(c.Message, "changed from ")
		if !ok {
			break
		}
		if c.Before != "" && c.After != "" {
			l.head, l.from, l.to, l.decls = head+"changed", c.Before, c.After, true
		} else if from, to, ok := splitFromTo(rest); ok {
			l.head, l.from, l.to = head+"changed", from, to
		}
	}
	return l
}

// role says what a row shows, which decides its color in the text layout.
type role int

const (
	roleMessage role = iota // the apidiff message of a change
	roleRemoved             // the declaration of a removed symbol
	roleAdded               // the declaration of an added symbol
	roleOld                 // the old declaration of a changed symbol
	roleNew                 // the new declaration of a changed symbol
)

// row is one printed line of a change, as the text and Markdown layouts
// share it: a glyph, the text after it and, on the one row that locates
// the change, its position.
type row struct {
	glyph string // "-", "+", "~" or "!"
	text  string // a declaration or a message
	pos   string
	role  role
	// nested marks a bare type quoted under a message row: the X and Y of
	// a "changed from X to Y" message whose symbol could not be looked up
	// on both sides.
	nested bool
	// bold marks the row of an incompatible change that the text layout
	// emphasizes and the Markdown layout, which has no bold inside a code
	// block, marks with "!" instead.
	bold bool
}

// label is the row as printed without color: the glyph and the text.
func (r row) label() string { return r.glyph + " " + r.text }

// rows lays l out as the text and Markdown layouts print it. A change with
// declarations is the declarations alone, like a patch: a removal is its
// old declaration on a "-" row, an addition its new one on a "+" row, and
// a changed symbol is the pair, so that it reads as one edit rather than
// as a removal and an addition. A change without declarations keeps its
// message row ("package added", "T: no longer implements fmt.Stringer"),
// with any bare types quoted in the message nested under it.
func (l line) rows() []row {
	bold := !l.compatible
	if l.decls {
		switch {
		case l.from != "" && l.to != "":
			return []row{
				{glyph: "-", text: l.from, role: roleOld},
				{glyph: "+", text: l.to, pos: l.pos, role: roleNew, bold: bold},
			}
		case l.from != "":
			return []row{{glyph: "-", text: l.from, pos: l.pos, role: roleRemoved, bold: bold}}
		default:
			return []row{{glyph: "+", text: l.to, pos: l.pos, role: roleAdded, bold: bold}}
		}
	}
	out := []row{{glyph: l.glyph, text: l.head, pos: l.pos, role: roleMessage, bold: bold}}
	if l.from != "" {
		out = append(out, row{glyph: "-", text: l.from, role: roleOld, nested: true})
	}
	if l.to != "" {
		out = append(out, row{glyph: "+", text: l.to, role: roleNew, nested: true, bold: bold})
	}
	return out
}

// packageRows returns the rows of the changes of p to show, honoring
// BreakingOnly, and the width of the widest labeled row among those that
// carry a position, so that positions line up in a column.
func packageRows(p Package, opts Options) (rows []row, width int) {
	for _, c := range p.Changes {
		if opts.BreakingOnly && c.Compatible {
			continue
		}
		for _, r := range describe(c, opts).rows() {
			if r.pos != "" {
				width = max(width, utf8.RuneCountInString(r.label()))
			}
			rows = append(rows, r)
		}
	}
	return rows, width
}

// padding returns the spaces that align a position after text at width.
func padding(text string, width int) string {
	return strings.Repeat(" ", max(0, width-utf8.RuneCountInString(text)))
}

func header(p Package) string {
	var notes []string
	if p.Internal {
		notes = append(notes, "internal")
	}
	switch p.Status {
	case New:
		notes = append(notes, "new")
	case Removed:
		notes = append(notes, "removed")
	}
	if len(notes) == 0 {
		return p.Path
	}
	return p.Path + " (" + strings.Join(notes, ", ") + ")"
}

// section is one of the two halves of the text and Markdown layouts: the
// public API or the internal packages, each its packages followed by its
// summary line.
type section struct {
	internal bool
	packages []Package // the ones with rows to show
	summary  string    // the summary line, without markup
	// secondary marks the internal section of an --filter=all diff, which
	// the text layout dims and the Markdown layout italicizes.
	secondary bool
}

// sections splits res into the sections opts.Filter selects. The public
// API comes first, reduced to its summary line when nothing changed, and
// the internal packages after it; with All the internal section appears
// only when some internal package changed.
func sections(res Result, opts Options) []section {
	var out []section
	if opts.Filter != Internal {
		sum := Summarize(res)
		s := section{summary: noChanges(res, opts.Filter)}
		if sum.PackagesChanged > 0 {
			s.summary = ""
		}
		out = append(out, s)
	}
	if isum := SummarizeInternal(res); opts.Filter == Internal || (opts.Filter == All && isum.PackagesChanged > 0) {
		out = append(out, section{internal: true, summary: internalSummary(isum), secondary: opts.Filter == All})
	}
	for i := range out {
		for _, p := range res.Packages {
			if p.Internal != out[i].internal {
				continue
			}
			if rows, _ := packageRows(p, opts); len(rows) > 0 {
				out[i].packages = append(out[i].packages, p)
			}
		}
	}
	return out
}

func writeText(w io.Writer, res Result, opts Options) error {
	st := Style{Enabled: opts.Color}
	var b strings.Builder
	for i, s := range sections(res, opts) {
		if i > 0 {
			b.WriteString("\n")
		}
		for _, p := range s.packages {
			rows, width := packageRows(p, opts)
			b.WriteString(st.Bold(header(p)))
			b.WriteString("\n")
			for _, r := range rows {
				indent := "  "
				if r.nested {
					indent = "      "
				}
				b.WriteString(indent + st.row(r))
				if r.pos != "" {
					b.WriteString(padding(r.label(), width) + "  " + st.Dim(r.pos))
				}
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
		switch {
		case s.internal && s.secondary:
			b.WriteString(st.Dim(s.summary)) // secondary to the public API's line
		case s.internal:
			b.WriteString(s.summary)
		case s.summary != "":
			b.WriteString(st.Dim(s.summary))
		default:
			b.WriteString(formatSummary(st, Summarize(res), res))
		}
		b.WriteString("\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// row colors r for the terminal. A removal is red, an addition green, both
// bold when incompatible; the old declaration of a changed symbol is
// greyed out and the new one orange (bold when incompatible), so that the
// pair reads as one edit. A message row is red for a removal or an
// incompatible addition, green for a compatible addition, and otherwise
// cyan, or yellow when incompatible.
func (st Style) row(r row) string {
	s := r.label()
	switch r.role {
	case roleRemoved:
		return st.Red(s, r.bold)
	case roleAdded:
		return st.Green(s, r.bold)
	case roleOld:
		return st.Grey(s)
	case roleNew:
		return st.Orange(s, r.bold)
	}
	code := codeYellow
	switch {
	case r.glyph == "-" || r.glyph == "!":
		code = codeRed
	case r.glyph == "+":
		code = codeGreen
	case !r.bold:
		code = codeCyan
	}
	return st.color(s, r.bold, code)
}

// splitFromTo splits the "X to Y" that follows "changed from " in a message
// into X and Y. A type string can itself contain " to " (a nested func's
// parameter names, a struct's field names or tags), but only inside
// brackets or a string literal, so the split is the first " to " that
// leaves X complete. When none does, which happens when go/constant has
// abbreviated a long string value with "..." and no closing quote, the
// first " to " is taken.
func splitFromTo(s string) (from, to string, ok bool) {
	const sep = " to "
	first := strings.Index(s, sep)
	if first <= 0 || first+len(sep) >= len(s) {
		return "", "", false // no " to ", or nothing on one side of it
	}
	for i := first; i+len(sep) < len(s); {
		if complete(s[:i]) {
			return s[:i], s[i+len(sep):], true
		}
		j := strings.Index(s[i+len(sep):], sep)
		if j < 0 {
			break
		}
		i += len(sep) + j
	}
	return s[:first], s[first+len(sep):], true
}

// complete reports whether every bracket and string literal in s is closed.
func complete(s string) bool {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch q := s[i]; q {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case '"', '`':
			for i++; i < len(s) && s[i] != q; i++ {
				if q == '"' && s[i] == '\\' {
					i++ // an escaped character, possibly a quote
				}
			}
			if i == len(s) {
				return false
			}
		}
	}
	return depth == 0
}

// noChanges is the message for an empty public diff, naming the base
// release when there is one so that @latest shows what it resolved to, and
// pointing at --filter=all when internal packages were left out.
func noChanges(res Result, filter Visibility) string {
	s := "no exported API changes"
	if res.BaseVersion != "" {
		s += " since " + res.BaseVersion
	}
	if filter == Public {
		s += "; add --filter=all to include internal API changes"
	}
	return s
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

// internalSummary is the extra summary line for internal packages.
func internalSummary(isum Summary) string {
	if isum.PackagesChanged == 0 {
		return "internal: no changes"
	}
	return fmt.Sprintf("internal: %s · %d incompatible · %d compatible",
		plural(isum.PackagesChanged, "package"), isum.Incompatible, isum.Compatible)
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

// writeMarkdown renders the same rows as the text layout, each package as
// a bold path followed by a fenced diff block, which GitHub colors: "-"
// lines red, "+" lines green and "!" lines orange. Where the text layout
// uses bold for an incompatible change, the row's "+" or "~" becomes "!"
// (a removal keeps its "-"), so that a breaking change stands out in a
// pull request comment too.
func writeMarkdown(w io.Writer, res Result, opts Options) error {
	var b strings.Builder
	for i, s := range sections(res, opts) {
		if i > 0 {
			b.WriteString("\n")
		}
		for _, p := range s.packages {
			rows, width := packageRows(p, opts)
			fmt.Fprintf(&b, "**%s**\n\n```diff\n", header(p))
			for _, r := range rows {
				glyph, sep := r.glyph, " "
				if r.bold && glyph != "-" {
					glyph = "!"
				}
				if r.nested {
					sep = "   "
				}
				b.WriteString(glyph + sep + r.text)
				if r.pos != "" {
					b.WriteString(padding(r.label(), width) + "  " + r.pos)
				}
				b.WriteString("\n")
			}
			b.WriteString("```\n\n")
		}
		switch {
		case s.internal && s.secondary, !s.internal && s.summary != "":
			fmt.Fprintf(&b, "_%s_\n", s.summary)
		case s.internal:
			fmt.Fprintf(&b, "%s\n", s.summary)
		default:
			sum := Summarize(res)
			fmt.Fprintf(&b, "%s · %d incompatible · %d compatible · would require: **%s**%s\n",
				plural(sum.PackagesChanged, "package"), sum.Incompatible, sum.Compatible, sum.Release(), versions(res))
		}
	}
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
	Path     string       `json:"path"`
	Status   string       `json:"status"`
	Internal bool         `json:"internal,omitempty"`
	Changes  []jsonChange `json:"changes"`
}

type jsonChange struct {
	Symbol     string    `json:"symbol"`
	Kind       string    `json:"kind"`
	Compatible bool      `json:"compatible"`
	Message    string    `json:"message"`
	Before     string    `json:"before,omitempty"`
	After      string    `json:"after,omitempty"`
	Pos        *Position `json:"pos,omitempty"`
}

type jsonSummary struct {
	PackagesChanged int         `json:"packages_changed"`
	Incompatible    int         `json:"incompatible"`
	Compatible      int         `json:"compatible"`
	Release         string      `json:"release"`
	Internal        *jsonCounts `json:"internal,omitempty"`
}

type jsonCounts struct {
	PackagesChanged int `json:"packages_changed"`
	Incompatible    int `json:"incompatible"`
	Compatible      int `json:"compatible"`
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
	if opts.Filter != Public {
		isum := SummarizeInternal(res)
		rep.Summary.Internal = &jsonCounts{PackagesChanged: isum.PackagesChanged, Incompatible: isum.Incompatible, Compatible: isum.Compatible}
	}
	for _, p := range res.Packages {
		if len(p.Changes) == 0 {
			continue
		}
		jp := jsonPackage{Path: p.Path, Status: p.Status.String(), Internal: p.Internal, Changes: []jsonChange{}}
		for _, c := range p.Changes {
			if opts.BreakingOnly && c.Compatible {
				continue
			}
			l := describe(c, opts)
			jc := jsonChange{
				Symbol:     c.Symbol(),
				Kind:       c.Kind(),
				Compatible: c.Compatible,
				Message:    c.Message,
				Before:     l.from,
				After:      l.to,
			}
			if opts.Positions && !c.Pos.IsZero() {
				pos := c.Pos
				jc.Pos = &pos
			}
			jp.Changes = append(jp.Changes, jc)
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

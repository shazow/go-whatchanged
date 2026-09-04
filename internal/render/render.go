// Package render prints API diffs in a colorized, git-diff-like layout, as
// Markdown for pull requests and job summaries, or as JSON for tools.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
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
// symbols. Before and After carry the declaration of the symbol on each
// side as gofmt would print it, such as "func Open(path string) (*Client,
// error)", on several lines for a struct or interface with several
// members: Before for a removal, After for an addition, both for a
// "changed from X to Y" message (where the renderer falls back to the
// types quoted in the message when they are empty). A struct field's
// declaration is the one inside its struct, "Timeout int", and Struct
// names the struct, "Config"; the layouts show the fields of one struct
// together as a fragment of its declaration. Pos locates the symbol's
// declaration: on the base side for a removal, on the head side otherwise.
type Change struct {
	Message    string
	Compatible bool
	Before     string
	After      string
	Struct     string
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

// Import is a change to the imports of a package: a package of another
// module that the package started importing, or with Removed set, stopped
// importing. An import is not part of the API and never counts towards the
// summary or the required release; the layouts list it before the
// package's changes, as a compatible addition or removal of "import
// \"path\"".
type Import struct {
	Path    string
	Removed bool
}

// Kind classifies the import change as "added" or "removed".
func (i Import) Kind() string {
	if i.Removed {
		return "removed"
	}
	return "added"
}

// Package is the diff of one package. An Internal package (one below an
// internal directory) or a Main package (a command) is shown but kept out
// of the public API's counts and required release level. Imports are the
// changes to the packages of other modules it imports; a package with
// import changes alone is listed but does not count as changed.
type Package struct {
	Path     string
	Status   Status
	Internal bool
	Main     bool
	Changes  []Change
	Imports  []Import
}

// part returns the part of the module the package belongs to: Main for a
// command, whatever its directory, Internal for a package below an internal
// directory, and Public otherwise. Only public packages can be imported
// from outside the module.
func (p Package) part() Visibility {
	switch {
	case p.Main:
		return Main
	case p.Internal:
		return Internal
	}
	return Public
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
	// Markdown renders each package as a fenced Go block, for pull request
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

// Visibility is the set of parts of a module that take part in a diff:
// any combination of Public, Internal and Main, combined with |. The zero
// value selects All.
type Visibility int

const (
	// Public selects the packages importable from outside the module:
	// everything but internal packages and main packages.
	Public Visibility = 1 << iota
	// Internal selects the packages below an internal directory, other
	// than main packages.
	Internal
	// Main selects main packages (commands), wherever they are.
	Main

	// All selects every package: the public API, the internal packages and
	// the main packages.
	All = Public | Internal | Main
)

// ParseVisibility parses one --filter term: "all", "public", "internal" or
// "main".
func ParseVisibility(s string) (Visibility, error) {
	switch strings.ToLower(s) {
	case "all":
		return All, nil
	case "public":
		return Public, nil
	case "internal":
		return Internal, nil
	case "main":
		return Main, nil
	}
	return 0, fmt.Errorf("invalid filter %q (want all, public, internal or main)", s)
}

// String returns the terms of v, comma-separated, as --filter takes them.
func (v Visibility) String() string {
	if v.Has(All) {
		return "all"
	}
	var terms []string
	for _, part := range []struct {
		v    Visibility
		name string
	}{{Public, "public"}, {Internal, "internal"}, {Main, "main"}} {
		if v.Has(part.v) {
			terms = append(terms, part.name)
		}
	}
	return strings.Join(terms, ",")
}

// Has reports whether v selects every part in parts. The zero value
// selects everything.
func (v Visibility) Has(parts Visibility) bool {
	if v == 0 {
		v = All
	}
	return v&parts == parts
}

// Includes reports whether a package with the given internal and main
// status is selected: a main package by Main, and otherwise an internal
// package by Internal and any other by Public.
func (v Visibility) Includes(internal, main bool) bool {
	return v.Has(Package{Internal: internal, Main: main}.part())
}

// Kinds is the set of kinds of change that take part in a diff: API
// changes, import changes or both, combined with |. The zero value selects
// AllKinds.
type Kinds int

const (
	// API selects the changes to the exported API: the symbols apidiff
	// reports on, and packages added or removed.
	API Kinds = 1 << iota
	// Imports selects the changes to the imports of other modules.
	Imports

	// AllKinds selects both.
	AllKinds = API | Imports
)

// ParseKinds parses one --filter term: "api" or "imports".
func ParseKinds(s string) (Kinds, error) {
	switch strings.ToLower(s) {
	case "api":
		return API, nil
	case "imports":
		return Imports, nil
	}
	return 0, fmt.Errorf("invalid filter %q (want api or imports)", s)
}

// Has reports whether k selects every kind in kinds. The zero value
// selects everything.
func (k Kinds) Has(kinds Kinds) bool {
	if k == 0 {
		k = AllKinds
	}
	return k&kinds == kinds
}

// Options controls rendering.
type Options struct {
	Color bool
	// BreakingOnly hides compatible changes, import changes among them.
	BreakingOnly bool
	// Kinds says which kinds of change are shown: the API changes, the
	// import changes or, the default, both. The summary always counts the
	// API changes of the full diff.
	Kinds  Kinds
	Format Format
	// Positions annotates each change with the position of its declaration.
	Positions bool
	// Width is the number of columns the text layout may use, 0 for no
	// limit. Positions line up in a column that fits within it; a row too
	// wide for that column gets its position after two spaces regardless,
	// so that one long declaration does not push the whole column past
	// the edge and wrap every line of the package.
	Width int
	// Filter says which packages took part. The public API, its packages
	// and summary line, is printed when Public is selected; the internal
	// packages follow with a summary line of their own when Internal is,
	// and the main packages likewise when Main is. When the public API is
	// printed the other sections appear only when some package in them
	// changed. Internal and main packages never count towards the public
	// API's summary.
	Filter Visibility
}

// Summary describes the totals of a Result.
type Summary struct {
	PackagesChanged int
	Incompatible    int
	Compatible      int
}

// Summarize counts the changes of the public API: every package that is
// neither internal nor a main package.
func Summarize(res Result) Summary {
	return summarize(res, Public)
}

// SummarizeInternal counts the changes of the internal packages, main
// packages among them excluded.
func SummarizeInternal(res Result) Summary {
	return summarize(res, Internal)
}

// SummarizeMain counts the changes of the main packages.
func SummarizeMain(res Result) Summary {
	return summarize(res, Main)
}

// summarize counts the changes of the packages of one part: Public,
// Internal or Main.
func summarize(res Result, part Visibility) Summary {
	var s Summary
	for _, p := range res.Packages {
		if len(p.Changes) == 0 || p.part() != part {
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
	kind       string // "added", "removed" or "changed"
	head       string
	from, to   string // the old and new declarations; either may be empty
	decls      bool
	strct      string // the struct the declarations are a field of, or ""
	pos        string // "" when unknown or not wanted
	compatible bool
}

// item is one entry of a package's listing: a change, or the field changes
// of one struct, which the layouts show together as a fragment of the
// struct's declaration.
type item struct {
	lines []line
	strct string // the struct, for a fragment
}

// items groups lines into items: the field changes of one struct become
// one item, in the position of the first of them, and every other line an
// item of its own.
func items(lines []line) []item {
	var out []item
	index := map[string]int{}
	for _, l := range lines {
		if l.strct == "" {
			out = append(out, item{lines: []line{l}})
			continue
		}
		i, ok := index[l.strct]
		if !ok {
			i = len(out)
			index[l.strct] = i
			out = append(out, item{strct: l.strct})
		}
		out[i].lines = append(out[i].lines, l)
	}
	return out
}

// describe reduces c to a line. A removed symbol's declaration goes on a
// "-" line, an added one's on a "+" line, and a "changed from X to Y"
// message is split so that the before and after values sit on their own
// lines, like a small patch, which makes long signatures easy to compare.
func describe(c Change, opts Options) line {
	l := line{glyph: "~", head: c.Message, compatible: c.Compatible}
	kind := c.Kind()
	l.kind = kind
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
	if l.decls {
		l.strct = c.Struct
	}
	return l
}

// describeImport reduces an import change to a line: the declaration
// "import \"path\"" on a "-" or a "+" line, compatible either way.
func describeImport(i Import) line {
	decl := "import " + strconv.Quote(i.Path)
	if i.Removed {
		return line{glyph: "-", kind: "removed", head: decl, from: decl, decls: true, compatible: true}
	}
	return line{glyph: "+", kind: "added", head: decl, to: decl, decls: true, compatible: true}
}

// lines reduces the changes of p to show to lines, honoring Kinds and
// BreakingOnly, which hides the import changes along with every other
// compatible one: the imports first, removed before added, then the
// changes in order.
func (p Package) lines(opts Options) []line {
	var lines []line
	if opts.Kinds.Has(Imports) && !opts.BreakingOnly {
		for _, i := range p.Imports {
			lines = append(lines, describeImport(i))
		}
	}
	if opts.Kinds.Has(API) {
		for _, c := range p.Changes {
			if opts.BreakingOnly && c.Compatible {
				continue
			}
			lines = append(lines, describe(c, opts))
		}
	}
	return lines
}

// role says what a row shows, which decides its color in the text layout.
type role int

const (
	roleMessage role = iota // the apidiff message of a change
	roleRemoved             // the declaration of a removed symbol
	roleAdded               // the declaration of an added symbol
	roleOld                 // the old declaration of a changed symbol
	roleNew                 // the new declaration of a changed symbol
	roleFrame               // the "type T struct {" and "}" around the fields of a struct
)

// row is one printed line of the text layout: a glyph, the text after it
// and, on the one row that locates a change, its position. A row without
// a glyph continues the one above: a line of a multi-line declaration, or
// the "}" that closes a struct fragment.
type row struct {
	glyph string // "-", "+", "~", "!" or ""
	text  string // a declaration or a message, tabs expanded
	pos   string
	role  role
	// nested marks a bare type quoted under a message row: the X and Y of
	// a "changed from X to Y" message whose symbol could not be looked up
	// on both sides.
	nested bool
	// bold marks the row of an incompatible change that the text layout
	// emphasizes.
	bold bool
}

// label is the row as printed without color: the glyph and the text, the
// text alone in the glyph's column for a continuation row.
func (r row) label() string {
	if r.glyph == "" {
		return "  " + r.text
	}
	return r.glyph + " " + r.text
}

// tab is what the text layout prints for the tab that indents the lines
// of a multi-line declaration and the fields of a struct fragment.
const tab = "    "

// decl lays out a declaration as rows: one, or for a declaration on
// several lines its first line as given and the others as continuation
// rows, in the same role. The position, when any, goes on the first row.
func decl(r row) []row {
	lines := strings.Split(strings.ReplaceAll(r.text, "\t", tab), "\n")
	r.text = lines[0]
	out := []row{r}
	for _, l := range lines[1:] {
		out = append(out, row{text: l, role: r.role, bold: r.bold})
	}
	return out
}

// rows lays l out as the text layout prints it. A change with declarations
// is the declarations alone, like a patch: a removal is its old
// declaration on a "-" row, an addition its new one on a "+" row, and a
// changed symbol is the pair, so that it reads as one edit rather than as
// a removal and an addition. A change without declarations keeps its
// message row ("package added", "T: no longer implements fmt.Stringer"),
// with any bare types quoted in the message nested under it.
func (l line) rows() []row {
	bold := !l.compatible
	if l.decls {
		switch {
		case l.from != "" && l.to != "":
			return append(decl(row{glyph: "-", text: l.from, role: roleOld}),
				decl(row{glyph: "+", text: l.to, pos: l.pos, role: roleNew, bold: bold})...)
		case l.from != "":
			return decl(row{glyph: "-", text: l.from, pos: l.pos, role: roleRemoved, bold: bold})
		default:
			return decl(row{glyph: "+", text: l.to, pos: l.pos, role: roleAdded, bold: bold})
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

// elision is the last line of a struct fragment, which says that the
// struct has fields the fragment leaves out, as gopls elides them.
const elision = "// ..."

// rows lays it out as the text layout prints it: the rows of its lines,
// wrapped for a struct fragment in the struct's declaration, "~ type T
// struct {" above the fields, indented one tab, and the elision and "}"
// below them.
func (it item) rows() []row {
	if it.strct == "" {
		return it.lines[0].rows()
	}
	out := []row{{glyph: "~", text: "type " + it.strct + " struct {", role: roleFrame}}
	for _, l := range it.lines {
		for _, r := range l.rows() {
			r.text = tab + r.text
			out = append(out, r)
		}
	}
	return append(out, row{text: tab + elision, role: roleFrame}, row{text: "}", role: roleFrame})
}

// packageRows returns the rows of the changes of p to show, honoring
// BreakingOnly, and the column at which their positions line up: the
// width of the widest labeled row among those that carry a position. With
// a width limit, rows whose position would then run past the limit are
// left out of the column, and it narrows to the widest row that fits,
// with indent columns before the label and two between it and the
// position; those rows print their position unaligned instead.
func packageRows(p Package, opts Options, limit, indent int) (rows []row, width int) {
	for _, it := range items(p.lines(opts)) {
		rows = append(rows, it.rows()...)
	}
	return rows, column(rows, limit, indent)
}

// column returns the width of the widest labeled row with a position, or
// with limit > 0, of the widest such row at which every row up to that
// width fits its position within limit.
func column(rows []row, limit, indent int) int {
	var widths []int
	for _, r := range rows {
		if r.pos != "" {
			widths = append(widths, utf8.RuneCountInString(r.label()))
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(widths)))
	for _, w := range widths {
		if limit <= 0 || fits(rows, w, limit, indent) {
			return w
		}
	}
	return 0
}

// fits reports whether every row with a position labeled at most width
// wide fits its position within limit when aligned at width.
func fits(rows []row, width, limit, indent int) bool {
	for _, r := range rows {
		if r.pos == "" || utf8.RuneCountInString(r.label()) > width {
			continue
		}
		if indent+width+2+utf8.RuneCountInString(r.pos) > limit {
			return false
		}
	}
	return true
}

// padding returns the spaces that align a position after text at width.
func padding(text string, width int) string {
	return strings.Repeat(" ", max(0, width-utf8.RuneCountInString(text)))
}

func header(p Package) string {
	return p.Path + notes(p)
}

// notes is the parenthesized suffix of a package's header, " (internal,
// new)", or "" when there is nothing to note.
func notes(p Package) string {
	var notes []string
	if p.Internal {
		notes = append(notes, "internal")
	}
	if p.Main {
		notes = append(notes, "main")
	}
	switch p.Status {
	case New:
		notes = append(notes, "new")
	case Removed:
		notes = append(notes, "removed")
	}
	if len(notes) == 0 {
		return ""
	}
	return " (" + strings.Join(notes, ", ") + ")"
}

// section is one part of the text and Markdown layouts: the public API,
// the internal packages or the main packages, each its packages followed
// by its summary line.
type section struct {
	part     Visibility // Public, Internal or Main
	packages []Package  // the ones with rows to show
	// summary is the summary line, without markup: always set for the
	// internal and main sections, and for the public one only when nothing
	// changed (otherwise the layouts format the full summary themselves).
	summary string
	// secondary marks an internal or main section printed below the public
	// API, which the text layout dims and the Markdown layout italicizes.
	secondary bool
}

// sections splits res into the sections opts.Filter selects. The public
// API comes first, reduced to its summary line when nothing changed, then
// the internal packages and then the main packages (which never are
// public, wherever they live); when the public API is shown, the other
// sections appear only when some package in them changed.
func sections(res Result, opts Options) []section {
	f := opts.Filter
	var out []section
	if f.Has(Public) {
		s := section{part: Public, summary: noChanges(res, f)}
		if Summarize(res).PackagesChanged > 0 {
			s.summary = ""
		}
		out = append(out, s)
	}
	for _, part := range []Visibility{Internal, Main} {
		sum := summarize(res, part)
		if f.Has(part) && (!f.Has(Public) || sum.PackagesChanged > 0) {
			out = append(out, section{part: part, summary: partSummary(part, sum), secondary: f.Has(Public)})
		}
	}
	for i := range out {
		for _, p := range res.Packages {
			if p.part() != out[i].part {
				continue
			}
			if rows, _ := packageRows(p, opts, 0, 0); len(rows) > 0 {
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
			rows, width := packageRows(p, opts, opts.Width, 2)
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
		case s.part != Public && s.secondary:
			b.WriteString(st.Dim(s.summary)) // secondary to the public API's line
		case s.part != Public:
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
// pair reads as one edit. The frame of a struct fragment is dimmed. A
// message row is red for a removal or an incompatible addition, green for
// a compatible addition, and otherwise cyan, or yellow when incompatible.
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
	case roleFrame:
		return st.Dim(s)
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
	if !filter.Has(Internal) {
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

// partSummary is the summary line of the internal or the main section:
// "internal: 2 packages changed · 1 incompatible · 1 compatible".
func partSummary(part Visibility, sum Summary) string {
	if sum.PackagesChanged == 0 {
		return part.String() + ": no changes"
	}
	return fmt.Sprintf("%s: %s · %d incompatible · %d compatible",
		part, plural(sum.PackagesChanged, "package"), sum.Incompatible, sum.Compatible)
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

// writeMarkdown renders each package as a level-3 heading, its path in
// code, followed by a fenced Go block, which GitHub highlights as Go:
// keywords, types, strings and comments each in their own color. A code
// block cannot color a whole line, so the diff's own signal lives in
// comments; see goBlock. The headings nest under the level-2 title the
// action puts above the report.
func writeMarkdown(w io.Writer, res Result, opts Options) error {
	var b strings.Builder
	for i, s := range sections(res, opts) {
		if i > 0 {
			b.WriteString("\n")
		}
		for _, p := range s.packages {
			fmt.Fprintf(&b, "### `%s`%s\n\n", p.Path, notes(p))
			goBlock(&b, p, opts)
			b.WriteString("\n")
		}
		switch {
		case s.part != Public && s.secondary, s.part == Public && s.summary != "":
			fmt.Fprintf(&b, "_%s_\n", s.summary)
		case s.part != Public:
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

// goLine is one line of a Go block: a declaration and the comment that
// trails it, or a comment line alone when code is empty.
type goLine struct {
	code    string
	comment string // without the "//"
}

// goItem is one change as the Go block prints it: one line for a removal
// or an addition, two for a changed symbol, its old declaration on the
// first with "->" trailing and its new one on the second, so that both
// sides are highlighted and read as one edit. A change without
// declarations to show is its message as a comment line. The field
// changes of one struct are one goItem, the fields between "type T struct
// {" and "}" lines, indented one tab.
type goItem []goLine

// goBlock writes the changes of p as a fenced Go block, grouped under
// "// Removed", "// Changed" and "// Added" headers in that order, from
// what breaks to what is merely new. A change whose compatibility is not
// what its group implies says so in a trailing comment: an incompatible
// addition (a method added to an interface), or a compatible change (a
// func that became a var). The items of the Changed group, two lines
// each, are set apart by blank lines, inside a struct fragment too.
// Positions, when wanted, trail each line that locates a change, in a
// column that lines up across the block. A declaration on several lines
// has its comment on the first.
func goBlock(b *strings.Builder, p Package, opts Options) {
	kinds := []string{"removed", "changed", "added"}
	lines := map[string][]line{}
	for _, l := range p.lines(opts) {
		lines[l.kind] = append(lines[l.kind], l)
	}
	groups := make([][]goItem, len(kinds))
	for i, kind := range kinds {
		for _, it := range items(lines[kind]) {
			groups[i] = append(groups[i], it.goItem(kind == "changed"))
		}
	}
	// The column at which trailing comments line up, per indentation as
	// gofmt aligns them, so that the fields of a struct fragment line up
	// among themselves: only with positions, which are long enough to want
	// one; otherwise a single space.
	width := map[string]int{}
	if opts.Positions {
		for _, items := range groups {
			for _, item := range items {
				for _, gl := range item {
					if gl.code != "" && gl.comment != "" {
						indent, code := splitIndent(gl.code)
						width[indent] = max(width[indent], utf8.RuneCountInString(code))
					}
				}
			}
		}
	}
	b.WriteString("```go\n")
	first := true
	for i, kind := range kinds {
		items := groups[i]
		if len(items) == 0 {
			continue
		}
		if !first {
			b.WriteString("\n")
		}
		first = false
		b.WriteString("// " + strings.ToUpper(kind[:1]) + kind[1:] + "\n")
		for i, item := range items {
			if i > 0 && kind == "changed" {
				b.WriteString("\n")
			}
			for _, gl := range item {
				switch {
				case gl.code == "" && gl.comment == "":
					// a blank line
				case gl.code == "":
					b.WriteString("// " + gl.comment)
				case gl.comment == "":
					b.WriteString(gl.code)
				default:
					head, rest, _ := strings.Cut(gl.code, "\n")
					indent, code := splitIndent(head)
					b.WriteString(head + padding(code, width[indent]) + " // " + gl.comment)
					if rest != "" {
						b.WriteString("\n" + rest)
					}
				}
				b.WriteString("\n")
			}
		}
	}
	b.WriteString("```\n")
}

// splitIndent splits the first line of s into the tabs that indent it and
// the rest.
func splitIndent(s string) (indent, rest string) {
	s, _, _ = strings.Cut(s, "\n")
	rest = strings.TrimLeft(s, "\t")
	return s[:len(s)-len(rest)], rest
}

// goItem lays it out for the Go block: the goItem of its line, or for a
// struct fragment the goItems of its fields, indented, between "type T
// struct {" and the elision and "}". A removed field points at itself
// with "<-", since the fragment is otherwise a struct declaration under
// a Removed header. With spaced set, the fields are set apart by blank
// lines, as the items of the Changed group are.
func (it item) goItem(spaced bool) goItem {
	if it.strct == "" {
		return it.lines[0].goItem()
	}
	out := goItem{{code: "type " + it.strct + " struct {"}}
	for i, l := range it.lines {
		if i > 0 && spaced {
			out = append(out, goLine{})
		}
		for _, gl := range l.goItem() {
			gl.code = "\t" + strings.ReplaceAll(gl.code, "\n", "\n\t")
			if l.kind == "removed" {
				gl.comment = strings.TrimSuffix("<- · "+gl.comment, " · ")
			}
			out = append(out, gl)
		}
	}
	return append(out, goLine{code: "\t" + elision}, goLine{code: "}"})
}

// goItem lays l out for the Go block; see goItem.
func (l line) goItem() goItem {
	// The note on the line that locates the change: its compatibility when
	// that is not what its kind implies, then its position.
	var notes []string
	switch {
	case l.kind == "added" && !l.compatible:
		notes = append(notes, "incompatible")
	case l.kind == "changed" && l.compatible:
		notes = append(notes, "compatible")
	}
	if l.pos != "" {
		notes = append(notes, l.pos)
	}
	note := strings.Join(notes, " · ")
	if l.decls {
		switch {
		case l.from != "" && l.to != "":
			return goItem{{code: l.from, comment: "->"}, {code: l.to, comment: note}}
		case l.from != "":
			return goItem{{code: l.from, comment: note}}
		default:
			return goItem{{code: l.to, comment: note}}
		}
	}
	// A message, "package added" or "T: changed from X to Y" with the bare
	// types X and Y the message quoted, as one comment line.
	msg := l.head
	if l.from != "" && l.to != "" {
		msg += " from " + l.from + " to " + l.to
	}
	if note != "" {
		msg += " · " + note
	}
	return goItem{{comment: msg}}
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
	Main     bool         `json:"main,omitempty"`
	Changes  []jsonChange `json:"changes"`
	Imports  []jsonImport `json:"imports,omitempty"`
}

type jsonImport struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

type jsonChange struct {
	Symbol     string    `json:"symbol"`
	Kind       string    `json:"kind"`
	Compatible bool      `json:"compatible"`
	Message    string    `json:"message"`
	Before     string    `json:"before,omitempty"`
	After      string    `json:"after,omitempty"`
	Struct     string    `json:"struct,omitempty"`
	Pos        *Position `json:"pos,omitempty"`
}

type jsonSummary struct {
	PackagesChanged int         `json:"packages_changed"`
	Incompatible    int         `json:"incompatible"`
	Compatible      int         `json:"compatible"`
	Release         string      `json:"release"`
	Internal        *jsonCounts `json:"internal,omitempty"`
	Main            *jsonCounts `json:"main,omitempty"`
}

type jsonCounts struct {
	PackagesChanged int `json:"packages_changed"`
	Incompatible    int `json:"incompatible"`
	Compatible      int `json:"compatible"`
}

// writeJSON renders the result as one indented JSON document. Only packages
// with changes to show are listed (Kinds and BreakingOnly filter the
// changes, and a package left without any is dropped); the summary always
// counts the full diff, as in the other layouts.
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
	// The internal and main counts appear when their packages took part,
	// or when one of them changed anyway (a --pkg pattern can name one).
	if isum := SummarizeInternal(res); opts.Filter.Has(Internal) || isum.PackagesChanged > 0 {
		rep.Summary.Internal = &jsonCounts{PackagesChanged: isum.PackagesChanged, Incompatible: isum.Incompatible, Compatible: isum.Compatible}
	}
	if msum := SummarizeMain(res); opts.Filter.Has(Main) || msum.PackagesChanged > 0 {
		rep.Summary.Main = &jsonCounts{PackagesChanged: msum.PackagesChanged, Incompatible: msum.Incompatible, Compatible: msum.Compatible}
	}
	for _, p := range res.Packages {
		if len(p.Changes) == 0 && len(p.Imports) == 0 {
			continue
		}
		jp := jsonPackage{Path: p.Path, Status: p.Status.String(), Internal: p.Internal, Main: p.Main, Changes: []jsonChange{}}
		if opts.Kinds.Has(Imports) && !opts.BreakingOnly {
			for _, i := range p.Imports {
				jp.Imports = append(jp.Imports, jsonImport{Path: i.Path, Kind: i.Kind()})
			}
		}
		for _, c := range p.Changes {
			if !opts.Kinds.Has(API) || opts.BreakingOnly && c.Compatible {
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
				Struct:     l.strct,
			}
			if opts.Positions && !c.Pos.IsZero() {
				pos := c.Pos
				jc.Pos = &pos
			}
			jp.Changes = append(jp.Changes, jc)
		}
		if len(jp.Changes) == 0 && len(jp.Imports) == 0 {
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

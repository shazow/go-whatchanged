# Go-highlighted Markdown output

`--format=markdown`, and so the job summary and the pull request comment,
used to render each package as a `diff` block. GitHub colors its lines by
their first character, red, green and orange, and that is all the
highlighting a `diff` block gets: a signature is a wall of one color, and
the reader has to parse `func (c *Client) Close() error` themselves.

A `go` block gets the opposite treatment: keywords, type names, strings
and comments each in their own color, but no way to color a whole line.
So the question was where the diff's own signal, removed, changed, added
and incompatible, goes once the lines are Go. The answer rests on one
observation: inside a `go` block, a comment is the one thing rendered in
its own muted color, on any line, at any position. Headers, the pairing
of a changed symbol's two declarations, and notes are all comments.

This document shows the layout as implemented, then the alternatives
that were considered, on the same example: the packages of the golden
test. View it rendered, on GitHub, since the highlighting is the point.

## The layout

One `go` block per package. The changes are grouped under `// Removed`,
`// Changed` and `// Added`, in that order, so the block reads top to
bottom from what breaks to what is merely new. A changed symbol shows its
old declaration with `// ->` trailing and its new one on the line below,
so that both sides are highlighted and read as one edit; the pairs are
set apart by blank lines. A change whose compatibility is not what its
group implies says so in a trailing comment: an incompatible addition,
which is a method added to an interface, or a compatible change, such as
a func that became a var.

**example.com/m/store**

```go
// Removed
func (c *Client) Close() error

// Changed
field Config.Timeout int // ->
field Config.Timeout int64

func Open(path string) (*Client, error) // ->
func Open(path string, o Options) (*Client, error)

const Version untyped string = "1" // ->
const Version untyped int = 1

// Added
func (c *Client) Ping() error
type Options struct{Timeout int}
field Point.Z int
```

**example.com/m/util**

```go
// Changed
type Stringer interface{String() string} // ->
type Stringer = fmt.Stringer

// Added
func (Sizer) Size() int // incompatible
```

2 packages changed · 6 incompatible · 3 compatible · would require: **MAJOR** (v1.0.0 → v2.0.0)

For comparison, the same two packages as the `diff` block rendered them:

**example.com/m/store**

```diff
- func (c *Client) Close() error
- field Config.Timeout int
! field Config.Timeout int64
- func Open(path string) (*Client, error)
! func Open(path string, o Options) (*Client, error)
- const Version untyped string = "1"
! const Version untyped int = 1
+ func (c *Client) Ping() error
+ type Options struct{Timeout int}
+ field Point.Z int
```

**example.com/m/util**

```diff
! func (Sizer) Size() int
- type Stringer interface{String() string}
! type Stringer = fmt.Stringer
```

### Positions

With `--pos`, each line that locates a change carries its position in
the trailing comment, after the compatibility note when there is one.
The comments line up in a column, as gofmt aligns them, set by the lines
of the block that carry a comment:

**example.com/m/store**

```go
// Removed
func (c *Client) Close() error                     // v1.0.0:store/store.go:7:18

// Changed
field Config.Timeout int                           // ->
field Config.Timeout int64                         // store/store.go:17:21

func Open(path string) (*Client, error)            // ->
func Open(path string, o Options) (*Client, error) // store/store.go:11:6

const Version untyped string = "1"                 // ->
const Version untyped int = 1                      // store/store.go:19:7

// Added
func (c *Client) Ping() error                      // store/store.go:7:18
type Options struct{Timeout int}                   // store/store.go:9:6
field Point.Z int                                  // store/store.go:15:26
```

**example.com/m/util**

```go
// Changed
type Stringer interface{String() string} // ->
type Stringer = fmt.Stringer             // util/util.go:10:6

// Added
func (Sizer) Size() int                  // incompatible · util/util.go:7:2
```

### Changes without a declaration

A change with no declaration to show is its message as a comment line in
its group: a package without exported symbols, a type that stopped
satisfying an interface, or a `changed from X to Y` whose symbol could
not be looked up on both sides. A compatible one says so after the
message:

```go
// Changed
// T: no longer implements fmt.Stringer

func V() int // ->
var V int // compatible

// U: changed from int to int64 · compatible
```

### Minimal signatures

`--signatures=minimal` prints apidiff's messages, which are not Go and
gain nothing from a `go` block. It keeps the `diff` block, with `!`
marking the incompatible lines that the terminal shows in bold:

```diff
- Drop: removed
! Open: changed from func(string) error to func(string, int) error
+ Added: added
```

### The pull request comment

The action folds the report under its verdict, as before; nothing in the
action changed for the new layout. Here is the whole golden test as the
comment shows it:

<details>
<summary>🔴 <b>API changes</b>: 4 packages changed · 7 incompatible · 4 compatible · would require: MAJOR (v1.0.0 → v2.0.0) · internal: 1 package changed · 1 incompatible · 1 compatible</summary>

**example.com/m/fresh (new)**

```go
// Added
func Hello()
```

**example.com/m/gone (removed)**

```go
// Removed
func Gone()
```

**example.com/m/store**

```go
// Removed
func (c *Client) Close() error

// Changed
field Config.Timeout int // ->
field Config.Timeout int64

func Open(path string) (*Client, error) // ->
func Open(path string, o Options) (*Client, error)

const Version untyped string = "1" // ->
const Version untyped int = 1

// Added
func (c *Client) Ping() error
type Options struct{Timeout int}
field Point.Z int
```

**example.com/m/util**

```go
// Changed
type Stringer interface{String() string} // ->
type Stringer = fmt.Stringer

// Added
func (Sizer) Size() int // incompatible
```

4 packages changed · 7 incompatible · 4 compatible · would require: **MAJOR** (v1.0.0 → v2.0.0)

**example.com/m/internal/hidden (internal)**

```go
// Removed
func Hidden()

// Added
func Added()
```

_internal: 1 package changed · 1 incompatible · 1 compatible_

<sub>Compared <code>v1.0.0</code> with <code>working tree</code> · <a href="#">job summary</a></sub>
</details>

## Possible refinements

**Go-shaped declarations.** The highlighter does best on lines that parse
as Go. Three of our forms do not, and each has a Go spelling:

| Today | As Go |
|---|---|
| `field Config.Timeout int64` | `type Config struct { Timeout int64 }`, one fragment per struct with the changed fields alone inside |
| `const Version untyped int = 1` | `const Version = 1`, since an untyped constant has no type in source, or `const Version int = 1` |
| `type Options struct{Timeout int}` | `type Options struct{ Timeout int }`, the `go/format` spacing |

The struct fragment reads well and groups a struct's fields, which today
are scattered lines:

```go
// Changed
type Config struct {
	Timeout int // ->
	Timeout int64
}

// Added
type Point struct {
	Z int
}
```

These strings come from `declString`, not the renderer, and feed the
text layout and the JSON `before` and `after` fields too, so a change
here changes all three outputs.

**New declaration first.** The pair could lead with the new declaration,
which is the one a reader will call, and point the `// ->` at the old
one below. The old-first order was kept because an arrow reads as "from
this, to that", and the terminal, the JSON and apidiff's message all run
old to new. It is a swap of two lines in the renderer.

**Linked positions.** A position outside a code block could link to the
file at the head commit, which the action knows. Inside a `go` block it
cannot, so this needs the table layout below, or a position column
outside the block.

## Alternatives considered

Each is shown with the conventions that were adopted, capitalized headers
and `// ->` pairs, so that only the shape differs.

### Grouped by compatibility

The same block, grouped by the question a reviewer asks first: what
breaks? Two groups at most, `// Incompatible` first and `// Compatible`
after, and each line of the first says what happened to it. Additions,
the bulk of the compatible group, carry nothing.

**example.com/m/store**

```go
// Incompatible
func (c *Client) Close() error // removed

field Config.Timeout int // ->
field Config.Timeout int64

func Open(path string) (*Client, error) // ->
func Open(path string, o Options) (*Client, error)

const Version untyped string = "1" // ->
const Version untyped int = 1

// Compatible
func (c *Client) Ping() error
type Options struct{Timeout int}
field Point.Z int
```

**example.com/m/util**

```go
// Incompatible
func (Sizer) Size() int // added

type Stringer interface{String() string} // ->
type Stringer = fmt.Stringer
```

- Pro: mirrors the summary line and the terminal's bold. Everything under
  the first header is what a major release is about; `--filter=breaking`
  is that group alone.
- Con: the kind of each change becomes a trailing note, and a removed
  declaration needs one so as not to read as a live one. The adopted
  layout has nearly the same order, removals and changes before
  additions, with the kind structural and the compatibility the note.
- Note: the two are one renderer with a different grouping key.

### Labelled blocks

The diff's signal leaves the code entirely. Each package is a short list
of labelled `go` blocks, and the label is markdown, so it can be bold,
carry a count or a glyph, and be styled as the verdict line is.

**example.com/m/store**

**Removed** · 1 incompatible

```go
func (c *Client) Close() error
```

**Changed** · 3 incompatible

```go
field Config.Timeout int // ->
field Config.Timeout int64

func Open(path string) (*Client, error) // ->
func Open(path string, o Options) (*Client, error)

const Version untyped string = "1" // ->
const Version untyped int = 1
```

**Added** · 3 compatible

```go
func (c *Client) Ping() error
type Options struct{Timeout int}
field Point.Z int
```

**example.com/m/util**

**Changed** · 1 incompatible

```go
type Stringer interface{String() string} // ->
type Stringer = fmt.Stringer
```

**Added** · 1 incompatible

```go
func (Sizer) Size() int
```

- Pro: the labels are the most visible of any layout, and the counts per
  group come for free.
- Con: three blocks per package where there was one. Each fenced block
  costs a box, padding and margins, so a diff of ten packages is a long
  scroll, and the package heading competes with the group labels for the
  eye.
- Con: more markdown for the action to see through when it extracts the
  summary lines.

### A table per package

The heaviest option: an HTML table with one row per change, so that each
row has a cell for its kind and compatibility beside a `go` block. Fenced
blocks render inside a `<td>` when set off by blank lines, which is what
makes this work at all.

**example.com/m/store**

<table>
<tr><td><sub>removed<br>incompatible</sub></td><td>

```go
func (c *Client) Close() error
```

</td></tr>
<tr><td><sub>changed<br>incompatible</sub></td><td>

```go
func Open(path string) (*Client, error) // ->
func Open(path string, o Options) (*Client, error)
```

</td></tr>
<tr><td><sub>added<br>compatible</sub></td><td>

```go
func (c *Client) Ping() error
type Options struct{Timeout int}
field Point.Z int
```

</td></tr>
</table>

- Pro: the only layout whose labels sit beside the code rather than
  above or inside it, and the only one where a position can be a link.
- Con: verbose markup, roughly three times the bytes of the adopted
  layout, against a comment limit of 65536 characters that the
  truncation already has to respect. A fenced block inside a table cell
  renders with a wide margin, and the table's borders add lines of their
  own.
- Con: nothing else in the output is HTML. The job summary and a comment
  render it, but the action's `markdown` output would be markdown in name
  only.

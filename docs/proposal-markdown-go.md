# Proposal: Go-highlighted Markdown output

`--format=markdown`, and so the job summary and the pull request comment,
renders each package as a `diff` block. GitHub colors its lines by their
first character, red, green and orange, and that is all the highlighting a
`diff` block gets: a signature is a wall of one color, and the reader has
to parse `func (c *Client) Close() error` themselves.

A `go` block gets the opposite treatment: keywords, type names, strings
and comments each in their own color, but no way to color a whole line. So
the question is where the diff's own signal, removed, changed, added and
incompatible, goes once the lines are Go. This proposal shows four
answers on the same example, the store and util packages of the golden
test, and recommends one. View it rendered, on GitHub, since the
highlighting is the point.

## Today, for reference

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

2 packages changed · 6 incompatible · 3 compatible · would require: **MAJOR** (v1.0.0 → v2.0.0)

## What Go comments can carry

Every variant below rests on one observation: inside a `go` block, a
comment is the one thing rendered in its own muted color, on any line, at
any position. That gives three uses for it, and the variants differ in
which they lean on:

- **A header** for a group of declarations: `// removed`.
- **The old form** of a changed symbol, next to the new one: `// was: int`.
  Grey old, highlighted new, which is exactly what the terminal layout does
  with grey and orange, and reads as "this is the signature now; it used
  to be that". The old form is a trailing comment when what differs is
  short (a field's type, a constant's value, an alias target), and a full
  line below otherwise (a signature).
- **A trailing note** on one line: `// incompatible`, `// added`, or the
  `--pos` position, which is a comment already in spirit.

## Variant 1: grouped by kind

One `go` block per package. The changes are grouped under `// removed`,
`// changed` and `// added` headers, in that order, so the block reads
top to bottom from what breaks to what is merely new. A change whose
compatibility is not what its group implies gets a trailing note: the
incompatible addition of a method to an interface, or a compatible change
such as a func becoming a var.

**example.com/m/store**

```go
// removed
func (c *Client) Close() error

// changed
field Config.Timeout int64 // was: int
func Open(path string, o Options) (*Client, error)
// was: func Open(path string) (*Client, error)
const Version untyped int = 1 // was: untyped string = "1"

// added
func (c *Client) Ping() error
type Options struct{Timeout int}
field Point.Z int
```

**example.com/m/util**

```go
// changed
type Stringer = fmt.Stringer // was: interface{String() string}

// added
func (Sizer) Size() int // incompatible
```

2 packages changed · 6 incompatible · 3 compatible · would require: **MAJOR** (v1.0.0 → v2.0.0)

- Pro: the least commentary. Most lines are bare declarations, and a
  package with only additions is a header and a list of Go.
- Pro: the same shape as today, one block per package, so the action's
  summary extraction, truncation and `<details>` folding need no change.
- Con: "incompatible" is not a first-class grouping. The reader infers it
  from the group, and the exceptions are a trailing note they may miss.

## Variant 2: grouped by compatibility

The same block, grouped by the question a reviewer asks first: what
breaks? Two groups at most, `// incompatible` first and `// compatible`
after, and each line says what happened to it. Additions, the bulk of
the compatible group, carry nothing.

**example.com/m/store**

```go
// incompatible
func (c *Client) Close() error // removed
field Config.Timeout int64 // was: int
func Open(path string, o Options) (*Client, error)
// was: func Open(path string) (*Client, error)
const Version untyped int = 1 // was: untyped string = "1"

// compatible
func (c *Client) Ping() error
type Options struct{Timeout int}
field Point.Z int
```

**example.com/m/util**

```go
// incompatible
func (Sizer) Size() int // added
type Stringer = fmt.Stringer // was: interface{String() string}
```

2 packages changed · 6 incompatible · 3 compatible · would require: **MAJOR** (v1.0.0 → v2.0.0)

- Pro: mirrors the summary line and the terminal's bold. Everything under
  the first header is what a major release is about; `--filter=breaking`
  is that group alone.
- Pro: every incompatible line explains itself, and a removed declaration
  cannot be mistaken for a live one.
- Con: more trailing comments than variant 1, and a compatible change
  that is not an addition still needs its `// was:`.
- Note: variants 1 and 2 are one renderer with a different grouping key.
  Picking one does not close the door on the other.

## Variant 3: labelled blocks

The diff's signal leaves the code entirely. Each package is a short list
of labelled `go` blocks, and the label is markdown, so it can be bold,
carry a count or a glyph, and be styled as the verdict line is. The
blocks are pure Go, apart from the `// was:` of a changed symbol.

**example.com/m/store**

**Removed** · 1 incompatible

```go
func (c *Client) Close() error
```

**Changed** · 3 incompatible

```go
field Config.Timeout int64 // was: int
func Open(path string, o Options) (*Client, error)
// was: func Open(path string) (*Client, error)
const Version untyped int = 1 // was: untyped string = "1"
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
type Stringer = fmt.Stringer // was: interface{String() string}
```

**Added** · 1 incompatible

```go
func (Sizer) Size() int
```

2 packages changed · 6 incompatible · 3 compatible · would require: **MAJOR** (v1.0.0 → v2.0.0)

- Pro: the labels are the most visible of any variant, and the counts per
  group come for free.
- Con: three blocks per package where there was one. Each fenced block
  costs a box, padding and margins, so a diff of ten packages is a long
  scroll, and the package heading competes with the group labels for the
  eye. Both are bold; one of them would have to become a `####` heading
  or a `<sub>`.
- Con: more markdown for the action to see through. The summary
  extraction skips bold lines already, but a label that is not bold would
  need its own rule.

## Variant 4: a table per package

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
func Open(path string, o Options) (*Client, error)
// was: func Open(path string) (*Client, error)
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

- Pro: the only variant whose labels sit beside the code rather than
  above or inside it, and the only one where a `--pos` position can be a
  link, since it is outside the code block: the action knows the head
  commit and could point `store/store.go:11` at the file on GitHub.
- Con: verbose markup, roughly three times the bytes of variant 1, against
  a comment limit of 65536 characters that the truncation already has to
  respect. A fenced block inside a table cell also renders with a wide
  margin, and the table's borders add lines of their own.
- Con: nothing else in the output is HTML. The job summary renders it, and
  so does a comment, but the `markdown` output of the action would be
  markdown in name only.

## Refinements that apply to any variant

**Go-shaped declarations.** The highlighter does best on lines that parse
as Go. Three of our forms do not, and each has a Go spelling:

| Today | As Go |
|---|---|
| `field Config.Timeout int64` | `type Config struct { Timeout int64 }`, one fragment per struct, with the changed fields alone inside and `// was:` on each |
| `const Version untyped int = 1` | `const Version = 1`; an untyped constant has no type in source |
| `type Options struct{Timeout int}` | `type Options struct{ Timeout int }`, the `go/format` spacing |

The struct fragment reads well and groups a struct's fields, which today
are scattered lines:

```go
// changed
type Config struct {
	Timeout int64 // was: int
}

// added
type Point struct {
	Z int
}
```

The cost is in `declString` rather than the renderer: the fragment needs
the struct's fields collected per type, and the JSON `before` and `after`
strings, which are part of the interface, would either keep the `field`
form or change with it.

**Positions.** With `--pos`, the position joins the trailing comment,
after the `was:` when there is one, and the column of comments lines up
as it does in the text layout:

```go
// changed
field Config.Timeout int64                          // was: int · store/store.go:17:21
func Open(path string, o Options) (*Client, error)  // store/store.go:11:6
// was: func Open(path string) (*Client, error)
```

**Minimal signatures.** `--signatures=minimal` prints apidiff's messages,
`Open: changed from func(string) (*Client, error) to func(string,
Options) (*Client, error)`, which are not Go and gain nothing from a `go`
block. That mode keeps a plain block, or becomes a list of inline code.

**Message-only changes.** A change without a declaration to show, such as
`package added` or `T: no longer implements fmt.Stringer`, is a comment
line in its group. Today it is a `~` line; a comment is the same weight.

**The `package` line.** A block could open with `package store`, which
the highlighter colors and which makes the block look like a file. It
duplicates the bold heading above it, though, and the heading is what the
eye lands on in a folded comment. Left out.

## Recommendation

Variant 1, grouped by kind, as the new `--format=markdown`, with the
`was:` convention for changed symbols and the Go-shaped declarations as a
follow-up. It keeps the output's shape, so the action's script is
untouched, it puts the least non-code in a code block, and its group order
already runs from breaking to benign. If the incompatible-first reading
turns out to matter more in practice, variant 2 is the same renderer with
a different grouping key.

Replace the `diff` layout rather than adding a second markdown format.
The tool is in beta and the `diff` layout is documented as the layout of
`--format=markdown`, not as an interface; the README's "Output formats"
section changes with it.

The renderer's `line` already carries the old and new declarations, the
compatibility and whether they are declarations or bare types; it needs
the change's kind too, which `describe` has in hand. `writeMarkdown`
groups the lines of a package by kind, prints the headers and turns the
old form into a `was:` comment, trailing or below by the length of what
differs. The golden file and the markdown assertions in the tests are
regenerated with `-update`.

Here is the recommended variant as the pull request comment would show
it, folded as the action folds it, for all five packages of the golden
test:

<details>
<summary>🔴 <b>API changes</b>: 4 packages changed · 7 incompatible · 4 compatible · would require: MAJOR (v1.0.0 → v2.0.0) · internal: 1 package changed · 1 incompatible · 1 compatible</summary>

**example.com/m/fresh (new)**

```go
// added
func Hello()
```

**example.com/m/gone (removed)**

```go
// removed
func Gone()
```

**example.com/m/store**

```go
// removed
func (c *Client) Close() error

// changed
field Config.Timeout int64 // was: int
func Open(path string, o Options) (*Client, error)
// was: func Open(path string) (*Client, error)
const Version untyped int = 1 // was: untyped string = "1"

// added
func (c *Client) Ping() error
type Options struct{Timeout int}
field Point.Z int
```

**example.com/m/util**

```go
// changed
type Stringer = fmt.Stringer // was: interface{String() string}

// added
func (Sizer) Size() int // incompatible
```

4 packages changed · 7 incompatible · 4 compatible · would require: **MAJOR** (v1.0.0 → v2.0.0)

**example.com/m/internal/hidden (internal)**

```go
// removed
func Hidden()

// added
func Added()
```

_internal: 1 package changed · 1 incompatible · 1 compatible_

<sub>Compared <code>v1.0.0</code> with <code>working tree</code> · <a href="#">job summary</a></sub>
</details>

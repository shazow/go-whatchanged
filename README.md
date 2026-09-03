# go-whatchanged

[![CI](https://github.com/shazow/go-whatchanged/actions/workflows/ci.yml/badge.svg)](https://github.com/shazow/go-whatchanged/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/shazow/go-whatchanged.svg)](https://pkg.go.dev/github.com/shazow/go-whatchanged)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

What changed in the public API?

`go-whatchanged` prints a semantic diff of a Go module's exported API between
two git revisions, or between a revision and whatever is in your working
tree, and names the semantic version the changes call for. The comparison
itself is done by [`golang.org/x/exp/apidiff`](https://pkg.go.dev/golang.org/x/exp/apidiff);
this tool makes it work on any git history without touching your checkout.

- **Read-only.** No temporary directories, no clones, no worktrees, no `go`
  command, no network. Both sides are read from the git object store and
  type-checked in memory.
- **Release-aware.** `@latest` finds the last release tag for you, and the
  summary says what the changes would require:
  `would require: MAJOR (v1.4.0 → v2.0.0)`.
- **CI-ready.** Markdown for pull requests, JSON for tools, exit codes for
  gates, and a [GitHub Action](#github-action) that posts the diff to the
  job summary.

**Status:** beta. It was fairly vibecoded, out of a desperate need to review
large pull requests better, but the read-only constraints add a lot of
safety (and efficiency) to how it works. It's quite useful!

## Example

```
$ go-whatchanged @latest
example.com/m/store
  - func (c *Client) Close() error
  - func Open(path string) (*Client, error)
  + func Open(path string, o Options) (*Client, error)
  + func (c *Client) Ping() error
  + type Options struct{Timeout int}

example.com/m/util
  + func (Sizer) Size() int

2 packages changed · 3 incompatible · 2 compatible · would require: MAJOR (v1.4.0 → v2.0.0)
```

Each package lists its changes as a patch of declarations: a removed symbol
on a `-` line, an added one on a `+` line, a changed one as both. In a
terminal the lines are colored, and bold marks the changes that break
importers. The summary counts the changes and names the release they would
require.

## Install

```
go install github.com/shazow/go-whatchanged@latest
```

Building needs Go 1.26 or newer; with the default `GOTOOLCHAIN=auto`, `go
install` fetches that toolchain when yours is older. To run, the tool needs:

- a git repository, in any state, including a linked worktree made with
  `git worktree add`;
- a `go.mod` with a `go` directive of 1.17 or newer;
- the module's dependencies in the module cache, since the tool never
  downloads anything. Run `go mod download` first; see
  [Troubleshooting](#troubleshooting) when the base side pins other
  versions.

## Quick start

| Question | Command |
|---|---|
| What do my uncommitted changes do to the API? | `go-whatchanged` |
| What has changed since the last release? | `go-whatchanged @latest` |
| What did release `v1.4.0` ship? | `go-whatchanged @latest v1.4.0` |
| What does this branch change, compared to `main`? | `go-whatchanged origin/main` |
| Which of these changes break importers? | `go-whatchanged --filter=breaking @latest` |
| Would this pass a compatibility gate? | `go-whatchanged --exit-fail=major @latest` |

## Contents

- [Usage](#usage)
- [Choosing revisions](#choosing-revisions)
- [Choosing packages](#choosing-packages)
- [Reading the output](#reading-the-output)
- [Output formats](#output-formats)
- [Exit codes](#exit-codes)
- [GitHub Action](#github-action)
- [Guarantees](#guarantees)
- [How it works](#how-it-works)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)
- [Development](#development)

## Usage

```
go-whatchanged [options] [<base> [<head>]]
```

| Argument | Meaning | Default |
|---|---|---|
| `base` | commit-ish for the old side: a hash, tag, branch, `HEAD~2`, `origin/main`, ... or `@latest`, the newest release tag among the ancestors of head | `HEAD` |
| `head` | commit-ish for the new side | the working tree, including uncommitted and untracked files |

| Option | Meaning |
|---|---|
| `--repo=DIR` | A path inside the git repository to diff. The module is the nearest `go.mod` at or above it. Default: the current directory. |
| `--pkg=PATTERN` | Diff only packages matching PATTERN. Repeatable, or comma-separated. |
| `--exclude=PATTERN` | Skip packages matching PATTERN. Repeatable, or comma-separated. |
| `--filter=WHICH` | `all` (default), `public` or `internal`: which packages take part. `breaking`: show only incompatible changes. Comma-separated or repeatable. |
| `--signatures=HOW` | `full` (default): show each change as its old and new declarations. `minimal`: one message line per change. |
| `--pos` | Annotate each change with the source position of its declaration. |
| `--format=LAYOUT` | `text` (default), `markdown` (or `md`) or `json`. |
| `--color=WHEN` | `auto` (default), `always` or `never`. `auto` colors when stdout is a terminal, unless `NO_COLOR` is set or `TERM` is `dumb`. |
| `--strict` | Treat warnings as fatal: type-check errors, `--pkg` patterns that match nothing, and the `go` directive note. |
| `--exit-fail=LEVEL` | Exit 100, 101 or 102 when the required bump is `major`, `minor` or `patch`, or higher. See [Exit codes](#exit-codes). |
| `--version` | Print the version and the Go release it was built with, then exit. |
| `-h`, `--help` | Print the full help. |

Options take the GNU form, `--filter=all` or `--filter all`, and `--` ends
them, so a revision that starts with a dash can follow it.

The environment supplies the rest, as it does for the `go` command:

| Variable | Effect |
|---|---|
| `GOOS`, `GOARCH` | The build target, which decides which files' build constraints are satisfied. Default: the running platform. |
| `GOROOT` | The standard library to type-check against. Default: the tree the binary was built with. |
| `GOMODCACHE` | Where dependencies are read from. Default: `$GOPATH/pkg/mod`, or `~/go/pkg/mod`. |
| `NO_COLOR`, `TERM` | Turn color off under `--color=auto`. |

## Choosing revisions

With no arguments the diff is `HEAD` against the working tree: exactly what
the uncommitted changes in your checkout do to the exported API. A side is
named the way git names commits, by hash, tag, branch, `HEAD~2`,
`origin/main` and so on, and a raw tree hash works too. Only the base may
be `@latest`.

### `@latest`, the last release

`@latest` as the base stands for the highest release tag of the module
among the ancestors of the head commit, which is `HEAD` when the head is
the working tree. Release tags are the ones the `go` command would publish:

- canonical semantic versions such as `v1.4.0` or `v1.5.0-rc.1`. Tags like
  `1.4.0`, `v1.4`, `v1.4.0+meta` or `release-1` are ignored;
- of the major version the module path calls for: `v2.x.y` for
  `example.com/m/v2`;
- prefixed with the module's directory when the module lives below the
  repository root (`sub/v1.4.0`), except for a major-version subdirectory
  such as `v2/`, which is tagged like the root;
- lightweight or annotated. Annotated tags are followed to the commit they
  point at.

"Highest" is by semantic version, not by distance from head. A tag on a
branch that was never merged is not an ancestor and does not count.

The head commit's own tags are skipped. On a freshly tagged commit `@latest`
is therefore the *previous* release, and the diff describes the new one.
That is the release-notes case, and what a workflow that runs on a tag push
gets:

```
$ go-whatchanged @latest v1.4.0
example.com/m/util
  + func Pad(s string, n int) string

1 package changed · 0 incompatible · 1 compatible · would require: MINOR (v1.3.0 → v1.4.0)

example.com/m/internal/cache (internal)
  - func Open(path string) error
  + func Get(key string) ([]byte, bool)

internal: 1 package changed · 1 incompatible · 1 compatible
```

The flip side: with uncommitted changes on top of the commit that carries
the latest tag, `@latest` skips that tag and picks the release before it,
or fails when there is none. Name the tag instead: `go-whatchanged v1.4.0`.

### The next version

Whenever the base is a release tag, typed by name or found by `@latest`,
the summary names the version the changes call for:

| Changes | from `v1.4.0` | from `v0.4.0` | from `v1.5.0-rc.1` |
|---|---|---|---|
| any incompatible | `v2.0.0` | `v0.5.0` | `v1.5.0` |
| only compatible | `v1.5.0` | `v0.5.0` | `v1.5.0` |
| none | `v1.4.1` | `v0.4.1` | `v1.5.0` |

At `v0`, which promises nothing, an incompatible change asks for the next
minor. A pre-release always suggests its final release, since anything may
change before it. When nothing changed, the text summary reads `no exported
API changes since v1.4.0`, which shows the tag `@latest` picked; the JSON
report and the action still carry the patch version as `next_version`.

## Choosing packages

Every package of the module takes part, except `main` packages,
directories without buildable Go files for the target platform, `testdata`
and `vendor` directories, directories whose name starts with `.` or `_`,
and nested modules. Packages below an `internal` directory are marked
internal and kept apart, see [below](#public-and-internal-packages).

`--pkg` restricts the diff to the packages matching a pattern and
`--exclude` drops the ones matching another. A pattern is a full import
path (`example.com/m/store`) or a path relative to the module root
(`store`, `./store`), and `...` matches anything, as in the `go` command:
`store/...` is the store package and everything below it, `...` is every
package. Both flags may be repeated or take comma-separated lists.

```
$ go-whatchanged --pkg store/... --exclude .../experimental v1.4.0
```

The summary, the required release and the exit code then describe the
selected packages only. A `--pkg` pattern that matches nothing on either
side prints a warning, since it is almost always a typo (fatal with
`--strict`):

```
warn: example.com/m: --pkg "stroe" matched no packages
```

### Public and internal packages

`--filter` says which packages take part. The default, `all`, is every
package: the public API, everything outside `internal` directories, comes
first with its summary line, and the internal packages follow, marked
`(internal)` and summarized on a line of their own. Internal packages never
count towards the public API's totals, the required release, the next
version or the exit code, so the default still works as a compatibility
gate, and it makes the tool useful for reviewing an application, not only a
library.

When nothing in the public API changed, the first line says so and the
internal packages still follow. When no internal package changed, the
internal section is left out:

```
$ go-whatchanged v1.3.0 v1.4.0
example.com/m/util
  + func Pad(s string, n int) string

1 package changed · 0 incompatible · 1 compatible · would require: MINOR (v1.3.0 → v1.4.0)

example.com/m/internal/cache (internal)
  - func Open(path string) error
  + func Get(key string) ([]byte, bool)

internal: 1 package changed · 1 incompatible · 1 compatible
```

`--filter=public` leaves the internal packages out entirely; an empty diff
then reads `no exported API changes; add --filter=all to include internal
API changes`. `--filter=internal` shows them alone, with only their summary
line, so it never exits 1.

`breaking` narrows the diff to incompatible changes and combines with the
others, comma-separated or repeated: `--filter=public,breaking`, or
`--filter internal --filter breaking`. The counts in the summary always
describe the full diff.

## Reading the output

Packages are sorted by import path. Within a package, incompatible changes
come first and compatible ones after, each group sorted by symbol. Each
change is shown as a patch of declarations, the old one on a `-` line and
the new one on a `+` line:

| Lines | Meaning | Color |
|---|---|---|
| `-` | symbol removed | red, bold |
| `+` | compatible addition | green |
| `+` | incompatible addition (a method on an interface, for example) | green, bold |
| `-` then `+` | symbol changed (signature, type, constant value): the old declaration, then the new one | grey, then orange (bold when incompatible) |

Bold marks an incompatible change. A changed symbol is greyed out and
orange rather than red and green so that it reads as one edit, and so that
two long signatures can be compared column by column.

Declarations are printed as source would print them, with parameter names,
and with foreign types qualified by package name:

| Kind | Example |
|---|---|
| function | `func Open(path string) (*Client, error)` |
| method | `func (c *Client) Ping() error` |
| interface method | `func (Sizer) Size() int` |
| type | `type Options struct{Timeout int}` |
| alias | `type Stringer = fmt.Stringer` |
| struct field | `field Config.Timeout int` |
| constant, with its value | `const Version untyped string = "1.4.0"` |

A constant whose value changed shows the old value on the `-` line and the
new one on the `+` line.

### Packages and the summary

A package header carries `(new)` when the package exists only on the head
side, `(removed)` when it exists only on the base side, and `(internal)`
for an internal package; they combine, as in `(internal, new)`. A package
without any exported API, one that only registers itself in `init`, say,
still gets a `+ package added` (compatible) or `- package removed`
(incompatible) line when it appears or disappears, since importers notice
either way. A directory that becomes a nested module counts as removed.

The summary line counts the packages and changes of the public API and ends
with the semantic version bump they would require: `MAJOR` (red) if
anything is incompatible, `MINOR` (yellow) if only compatible changes were
made, `PATCH` (green) otherwise. The counts always describe the full diff,
even with `--filter=breaking`. When the base is a release tag the summary
also names the next version, see [The next version](#the-next-version).

### Positions

With `--pos`, each declaration ends with its position, dimmed and aligned
per package: on the `+` line for an addition or a change, where the new
declaration is, and on the `-` line for a removal, on the base side,
prefixed with the revision:

```
$ go-whatchanged --pos @latest
example.com/m/store
  - func (c *Client) Close() error                      v1.4.0:store/store.go:11:18
  - func Open(path string) (*Client, error)
  + func Open(path string, o Options) (*Client, error)  store/store.go:11:6
  + func (c *Client) Ping() error                       store/store.go:14:18
  + type Options struct{Timeout int}                    store/store.go:8:6

example.com/m/util
  + func (Sizer) Size() int  util/util.go:7:2

2 packages changed · 3 incompatible · 2 compatible · would require: MAJOR (v1.4.0 → v2.0.0)
```

Working tree positions are relative to the module root, so terminals and
editors can open them. A field or method promoted from a dependency is
declared outside the module and has no position.

### One line per change

`--signatures=minimal` prints every change as apidiff's message, one line
marked with a glyph, with a changed symbol's old and new types quoted
inline:

```
$ go-whatchanged --signatures=minimal @latest
example.com/m/store
  - (*Client).Close: removed
  ~ Open: changed from func(string) (*Client, error) to func(string, Options) (*Client, error)
  + (*Client).Ping: added
  + Options: added

example.com/m/util
  ! Sizer.Size: added

2 packages changed · 3 incompatible · 2 compatible · would require: MAJOR (v1.4.0 → v2.0.0)
```

| Glyph | Meaning | Color |
|---|---|---|
| `-` | symbol removed | red, bold |
| `!` | incompatible addition (a method on an interface, for example) | red, bold |
| `~` | incompatible change (signature, type, constant value) | yellow, bold |
| `~` | compatible change | cyan |
| `+` | compatible addition | green |

The full layout falls back to such a line for a change with no declaration
to show: `+ package added` and `- package removed`, `~ T: no longer
implements fmt.Stringer` and the like for changes apidiff describes in
words rather than types, and `~ Open: changed` for a changed symbol that
cannot be looked up on both sides, which then shows the bare types quoted
in the message on `-` and `+` lines under it.

### Warnings

Problems that do not stop the diff are reported on stderr, one
`warn: <package>: <message>` line each, in dim yellow:

```
warn: example.com/m/util: util/broken.go:3:15: undefined: Missing
```

Type-check errors are the usual ones. They never abort the diff, since
apidiff copes with a partial package; a declaration whose type could not be
resolved shows `invalid type`. Positions on the base side are prefixed with
the revision (`v1.4.0:store/store.go:12:3`) and positions in the working
tree are relative to the module root. A module whose `go` directive is
newer than the Go that built `go-whatchanged` is type-checked as the newest
version the binary knows, with a single warning such as `warn: example.com/m:
go.mod requires go 1.27 but go-whatchanged was built with go1.26;
type-checking as go1.26`. With `--strict`, any warning is fatal and the
tool exits 2.

## Output formats

### Markdown

`--format=markdown` renders the same lines as the text layout, each package
as a bold path and a fenced `diff` block, which GitHub colors, followed by
the same summary line: ready for a pull request comment or a job summary.
A code block has no bold, so where the text layout emphasizes an
incompatible change, the Markdown layout marks the line `!` instead of `+`
(or `~`), which GitHub colors orange; a removal keeps its `-`. The `no
exported API changes` line and the internal packages' summary line are set
in italics.

````
$ go-whatchanged --format=markdown @latest
**example.com/m/store**

```diff
- func (c *Client) Close() error
- func Open(path string) (*Client, error)
! func Open(path string, o Options) (*Client, error)
+ func (c *Client) Ping() error
+ type Options struct{Timeout int}
```

**example.com/m/util**

```diff
! func (Sizer) Size() int
```

2 packages changed · 3 incompatible · 2 compatible · would require: **MAJOR** (v1.4.0 → v2.0.0)
````

### JSON

`--format=json` prints one document for tools and bots. Its field names are
part of the tool's interface.

```
$ go-whatchanged --format=json @latest
```

```json
{
  "base": "v1.4.0",
  "head": "working tree",
  "base_version": "v1.4.0",
  "next_version": "v2.0.0",
  "packages": [
    {
      "path": "example.com/m/store",
      "status": "changed",
      "changes": [
        {
          "symbol": "(*Client).Close",
          "kind": "removed",
          "compatible": false,
          "message": "(*Client).Close: removed",
          "before": "func (c *Client) Close() error"
        },
        {
          "symbol": "Open",
          "kind": "changed",
          "compatible": false,
          "message": "Open: changed from func(string) (*Client, error) to func(string, Options) (*Client, error)",
          "before": "func Open(path string) (*Client, error)",
          "after": "func Open(path string, o Options) (*Client, error)"
        },
        {
          "symbol": "(*Client).Ping",
          "kind": "added",
          "compatible": true,
          "message": "(*Client).Ping: added",
          "after": "func (c *Client) Ping() error"
        },
        {
          "symbol": "Options",
          "kind": "added",
          "compatible": true,
          "message": "Options: added",
          "after": "type Options struct{Timeout int}"
        }
      ]
    },
    {
      "path": "example.com/m/util",
      "status": "changed",
      "changes": [
        {
          "symbol": "Sizer.Size",
          "kind": "added",
          "compatible": false,
          "message": "Sizer.Size: added",
          "after": "func (Sizer) Size() int"
        }
      ]
    }
  ],
  "warnings": [],
  "summary": {
    "packages_changed": 2,
    "incompatible": 3,
    "compatible": 2,
    "release": "major",
    "internal": {
      "packages_changed": 0,
      "incompatible": 0,
      "compatible": 0
    }
  }
}
```

| Field | Meaning |
|---|---|
| `base`, `head` | The two sides as diffed: the revision given, the tag `@latest` resolved to, or the literal `working tree`. |
| `base_version`, `next_version` | Present when the base is a release tag: the version it denotes and the one the changes call for (the next patch when nothing changed). |
| `packages[].path` | Import path. Only packages with changes to show are listed. |
| `packages[].status` | `changed`, `new` (head side only) or `removed` (base side only). |
| `packages[].internal` | `true` for an internal package; absent otherwise. |
| `changes[].symbol` | The symbol as apidiff names it: `Open`, `(*Client).Ping`, `Config.Timeout`. Empty for a whole-package change. |
| `changes[].kind` | `added`, `removed` or `changed`. |
| `changes[].compatible` | `false` for a change that breaks importers. |
| `changes[].message` | apidiff's message, or `package added` / `package removed`. |
| `changes[].before`, `changes[].after` | What the text layout prints on its `-` and `+` lines: the old declaration of a removed symbol, the new one of an added symbol, both for a changed one. Absent with `--signatures=minimal`. |
| `changes[].pos` | With `--pos`: `{"rev": "v1.4.0", "file": "store/store.go", "line": 11, "col": 18}`, where `rev` is absent for the working tree. Absent for a declaration outside the module. |
| `warnings[]` | The warnings stderr shows, one `{"package": ..., "message": ...}` object each. |
| `summary` | `packages_changed`, `incompatible` and `compatible` count the public packages in the selection; `release` is `major`, `minor` or `patch`. |
| `summary.internal` | The same counts for the internal packages. Absent under `--filter=public`. |

`--filter=breaking` filters the change lists in every format; the summary
always counts the full diff.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | no incompatible changes |
| 1 | incompatible changes |
| 2 | error, including warnings under `--strict` |

For CI, `--exit-fail=LEVEL` turns the semantic version bump the changes
would require into the exit code, whenever that bump is `LEVEL` or higher:

| Required bump | `--exit-fail=major` | `--exit-fail=minor` | `--exit-fail=patch` |
|---|---:|---:|---:|
| MAJOR | 100 | 100 | 100 |
| MINOR | 0 | 101 | 101 |
| PATCH | 0 | 0 | 102 |

`--exit-fail=major` fails the build on incompatible changes, like the
default exit code 1 but with a distinct code. `--exit-fail=minor` also
fails on compatible additions, which suits a branch that must not grow the
API. `--exit-fail=patch` is always non-zero unless there is an error, so a
script can read the required bump straight from `$?`:

```
$ go-whatchanged --exit-fail=patch @latest >/dev/null
$ echo $?
100
```

Errors still exit 2 regardless of `--exit-fail`. Internal packages never
count towards the exit code.

## GitHub Action

The repository is also an action. It builds the tool from the ref that pins
it, runs it on the checkout and appends the diff to the job summary, where
GitHub colors the `diff` blocks:

```yaml
on: pull_request

permissions:
  contents: read

jobs:
  api:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0 # history and tags, for the merge-base and @latest
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - uses: shazow/go-whatchanged@main
```

On a `pull_request` event the base defaults to the merge-base of the pull
request and its base branch, so the diff is exactly what the pull request
does to the API. When the checkout is too shallow to find the merge-base,
the tip of the base branch stands in, with a warning. On any other event
the base defaults to `@latest`: a workflow that runs on a tag push gets the
release notes of the tag.

The step needs a Go toolchain on `PATH` (`actions/setup-go` above; the
action's own `go.mod` makes `go` fetch a newer toolchain if the runner's is
too old) and `jq`, which GitHub-hosted runners have. Since the tool never
fetches anything, the action runs `go mod download` in the checkout for the
head side and, for the base side, downloads what the base revision's
`go.mod` pins from a copy in the runner's temp directory, so nothing in the
checkout is written. The dependencies of the tag `@latest` resolves to are
downloaded once the tool has named it.

### Inputs

Every flag that applies in CI has an input. The action picks the format
itself: JSON for the outputs and Markdown for the summary.

| Input | Default | Meaning |
|---|---|---|
| `base` | see above | Commit-ish or `@latest` for the old side. |
| `head` | `HEAD` | Commit-ish for the new side. Empty means the working tree. |
| `working-directory` | `.` | Directory of the module to diff, for repositories with several. |
| `pkg` | | Patterns of packages to diff, comma- or newline-separated. |
| `exclude` | | Patterns of packages to skip. |
| `filter` | `all` | `all`, `public` or `internal`. |
| `breaking` | `false` | Show only incompatible changes. |
| `signatures` | `full` | `full` or `minimal`. |
| `pos` | `false` | Annotate changes with source positions. |
| `strict` | `false` | Treat warnings as fatal. |
| `goos`, `goarch` | the runner's | Build target. |
| `fail-on` | | `major`, `minor` or `patch`: fail the step at that level or above, like `--exit-fail`. The summary and the outputs are written either way. |
| `summary` | `true` | Append the diff to the job summary. |
| `title` | `API changes` | Heading above the diff in the summary. Empty for none. |

### Outputs

The summary line of the report becomes an annotation on the run, a notice
when every change is compatible, a warning when some are not, and an error
when `fail-on` fails the step. The report and its numbers are outputs for
later steps:

| Output | Value |
|---|---|
| `release` | `major`, `minor` or `patch` (no changes) |
| `packages-changed`, `incompatible`, `compatible` | the counts of the summary line |
| `base`, `head` | the revisions as diffed; `base` is the tag `@latest` picked |
| `base-version`, `next-version` | when the base is a release tag |
| `summary` | the summary line(s) as plain text |
| `markdown`, `json` | the whole report in either format |

Internal packages never count in the outputs or towards `fail-on`, as on
the command line. A pull request comment is one more step:

```yaml
      - uses: shazow/go-whatchanged@main
        id: api
      - if: steps.api.outputs.packages-changed != '0'
        env:
          GH_TOKEN: ${{ github.token }} # with permissions: pull-requests: write
          BODY: ${{ steps.api.outputs.markdown }}
        run: gh pr comment "${{ github.event.pull_request.number }}" --body "$BODY"
```

### Without the action

The same job in plain steps:

```yaml
- uses: actions/checkout@v4
  with:
    fetch-depth: 0 # tags and history for @latest and the base branch
- uses: actions/setup-go@v5
  with:
    go-version-file: go.mod
- run: go mod download
- run: go install github.com/shazow/go-whatchanged@latest
- run: |
    go-whatchanged --format=markdown origin/${{ github.base_ref }} HEAD \
      >> "$GITHUB_STEP_SUMMARY"
- run: go-whatchanged --exit-fail=major origin/${{ github.base_ref }} HEAD
```

`go mod download` is the one write: the tool itself never fetches, and a
dependency that is not in the module cache is an error, so a base revision
that pins other versions than the checkout needs them downloaded too (see
[Troubleshooting](#troubleshooting), or the action's script for one way).

## Guarantees

The tool is safe to run at any time, in any state of your checkout,
including a linked worktree created with `git worktree add`:

- **No disk writes.** No temporary directories, no checkouts, no build
  cache, no `go.sum` edits. It only reads the repository, `.git`,
  `$GOROOT/src`, `$GOMODCACHE` and the directories that `replace`
  directives point at.
- **No `go` command.** Both sides are parsed and type-checked in-process
  with `go/build`, `go/parser` and `go/types`. Cgo is disabled: files that
  `import "C"` are skipped, so an API declared only in them is not diffed.
- **No network.** A dependency missing from the module cache is a clear
  error: run `go mod download` and try again.
- **No repository mutation.** Only go-git's object store is used. The
  worktree, index and refs are never touched.

## How it works

The repository is the nearest `.git` at or above `--repo`, and the module
the nearest `go.mod` between there and the repository root. Each side gets
its own `go/build` context whose filesystem hooks are served either from
the git tree, mounted at a synthetic path, or from the working directory;
everything outside the module falls through to the real filesystem, so
`$GOROOT` and the module cache stay reachable.

Import paths are resolved from `go.mod` alone: the longest matching
`require`, `replace` directives (directories and modules alike), `$GOROOT`
and the module cache, without consulting the `go` command. This is sound
for modules at `go 1.17` or newer, because graph pruning guarantees that
every module providing an imported package is listed.

The two sides load concurrently. Function bodies are type-checked only in
the main module, since the diff reads package-level declarations alone.
Dependency packages are type-checked once and shared between the sides
when their imports resolve identically on both; a dependency that imports
the main module (grpc-go and go-control-plane import each other), or one
whose transitive imports the two `go.mod` files pin to different versions,
is checked once per side instead, so that neither side is ever linked
against the other side's packages.

Each package in the union of the two sides is then handed to apidiff, and
every change it reports is annotated with the declaration and the position
of the symbol it names.

## Limitations

- **Not a library.** Everything lives under `internal/`; the command line
  and the JSON layout are the interface.
- **One platform per run.** `GOOS` and `GOARCH` pick it. There is no union
  diff across platforms.
- **No cgo.** Files that `import "C"` are skipped on both sides.
- **No `main` packages.** They have no importable API and are not diffed.
- **No `go.work`, vendor mode or GOPATH mode.** Dependencies come from the
  module cache and `replace` directories, and `go.mod` must say `go 1.17`
  or newer.
- **`go.mod` is trusted.** Imports are resolved from it alone, so a stale
  `go.mod`, one that `go mod tidy` would change, can leave an import
  unresolvable.
- **Nothing is fetched.** Ever. The module cache has to be warm.
- **Only the main module is compared.** The APIs of dependencies are not.

## Troubleshooting

| Message | What to do |
|---|---|
| `module X@vY not in module cache (run go mod download)` | Run `go mod download` in the checkout. For a base revision that pins other versions, download them from a copy of its `go.mod`: `mkdir -p /tmp/base && git show v1.4.0:go.mod > /tmp/base/go.mod && (cd /tmp/base && go mod download)`. |
| `@latest: no release tags for M (looking for tags like "v1.2.3")` | The module has no tag the `go` command would publish. See [`@latest`](#latest-the-last-release) for the rules, and check that the clone has tags (`git fetch --tags`, or `fetch-depth: 0` in CI). |
| `@latest: none of the N release tag(s) for M is an ancestor of HEAD` | The tags are on unmerged branches, the clone is too shallow (`git fetch --unshallow`), or `HEAD` itself is the tagged commit and there is no earlier release. Name the tag instead. |
| `X is not inside a git repository` or `no go.mod found` | Point `--repo` at a directory inside the repository and the module. |
| `GOROOT "..." does not contain a Go source tree` | The binary was built against a Go tree that has since moved. Set `GOROOT="$(go env GOROOT)"`, or reinstall. |
| `unresolvable import "x" (required by y): no module in go.mod provides it` | That side's `go.mod` is out of date. Run `go mod tidy` there and commit. |
| `warn: M: go.mod requires go 1.27 but go-whatchanged was built with go1.26` | Reinstall with a newer Go. Until then the module is type-checked as the older version, which may misreport code using newer features. |
| `warn: pkg: file:line:col: ...` | A type-check error in the package. The diff continues with what could be resolved; fix the code, or fail on it with `--strict`. |
| `go X is too old; go >= 1.17 is required for module graph pruning` | Bump the `go` directive on that side. |

## Development

```
go test ./...
go test ./internal/whatchanged -update   # refresh the golden files in testdata
```

The tests build their repositories in memory and never touch the disk. CI
runs `gofmt`, `go vet`, `staticcheck`, `shellcheck` on the action's script
and `go test -race`, and a second workflow dogfoods the action on every
pull request.

## License

[MIT](LICENSE).

# go-whatchanged

What changed in the public API?

Pretty semantic diff of a Go module's exported API, powered by 
[`golang.org/x/exp/apidiff`](https://pkg.go.dev/golang.org/x/exp/apidiff).

Read-only: No mutations to your filesystem, no git clones, no git worktrees.

## Usage

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

## Install

```
go install github.com/shazow/go-whatchanged@latest
```

## Usage

```
go-whatchanged [flags] [<base> [<head>]]

  base   optional commit-ish for the old side: hash, tag, branch, HEAD~2, ...
         or @latest, the newest release tag among the ancestors of head.
         Default: HEAD.
  head   optional commit-ish for the new side. Default: the working tree.
```

With no arguments the diff is `HEAD` against the working tree, so it shows
exactly what the uncommitted changes in your checkout do to the exported API.
`go-whatchanged @latest` answers the other everyday question: what has
changed since the last release?

```
Flags:
  --repo string      path inside a git repository (default: current directory)
  --goos, --goarch   build target (default: the running platform)
  --pkg PATTERN      diff only packages matching PATTERN (repeatable)
  --exclude PATTERN  skip packages matching PATTERN (repeatable)
  --filter string    public | internal | all: which packages take part
                     (default public; see below)
  --breaking         show only incompatible changes
  --signatures       full | minimal: show each change as its old and new
                     declarations, or as one message line (default full)
  --pos              annotate changes with source positions (see below)
  --format string    text | markdown | json (default text; see below)
  --color string     auto | always | never (default auto; honors NO_COLOR)
  --strict           type-check errors are fatal (default: warn)
  --exit-fail LEVEL  exit 100/101/102 when the required bump is major, minor
                     or patch, or higher (see below)
  --version          print the version and exit

Exit codes: 0 no incompatible changes · 1 incompatible changes · 2 error
```

### Choosing packages

`--pkg` restricts the diff to the packages matching a pattern and
`--exclude` drops the ones matching another; both take a full import path
(`example.com/m/store`) or a path relative to the module root (`store`,
`./store`), and `...` matches anything, as in the `go` command: `store/...`
is the store package and everything below it, `...` is every package. The
flags may be repeated or take comma-separated lists. The summary, the
required release and the exit code then describe the selected packages
only, and a `--pkg` pattern that matches nothing on either side prints a
warning (fatal with `--strict`), since it is almost always a typo.

```
$ go-whatchanged --pkg store/... --exclude .../experimental v1.4.0
```

`--filter` says which packages take part. The default, `public`, is the
importable API: everything outside `internal` directories. `all` adds the
internal packages, which makes the tool useful for reviewing an
application, not only a library; `internal` shows them alone. Internal
packages are listed with an `(internal)` mark and summarized on a line of
their own, and they never count towards the public API's totals, the
required release, the next version or the exit code, so `--filter=all`
combines with `--exit-fail`, and `--filter=internal` never fails a build:

```
$ go-whatchanged --filter=all
example.com/m/internal/store (internal)
  - func Open(path string) (*Client, error)

example.com/m/util
  + func Pad(s string, n int) string

1 package changed · 0 incompatible · 1 compatible · would require: MINOR
internal: 1 package changed · 1 incompatible · 0 compatible
```

With `--filter=internal` only the last line and the internal packages
remain.

### Since the last release

`@latest` as the base stands for the highest release tag of the module
among the ancestors of the head commit (`HEAD` when the head is the working
tree). Release tags are the ones the `go` command would publish: canonical
semantic versions such as `v1.4.0` or `v1.5.0-rc.1`, prefixed with the
module's directory for a module below the repository root (`sub/v1.4.0`),
and of the major version the module path calls for (`v2.x.y` for
`example.com/m/v2`). Tags like `1.4.0`, `v1.4` or `release-1` are ignored.
A tag on a branch that was never merged is not an ancestor and does not
count.

The head commit's own tags are skipped, so on a freshly tagged commit
`@latest` is the *previous* release and the diff describes the new one.
That is the release-notes case: in a workflow that runs on a tag push,
`go-whatchanged @latest` lists what the tag ships.

Whenever the base is a release tag (typed by name or found by `@latest`),
the summary names the version the changes call for:

```
$ go-whatchanged @latest
...
1 package changed · 0 incompatible · 2 compatible · would require: MINOR (v1.4.0 → v1.5.0)
```

An incompatible change asks for the next major (`v1.4.0 → v2.0.0`), or the
next minor while the module is still at `v0`, which promises nothing. A
pre-release base always suggests its final release (`v1.5.0-rc.1 →
v1.5.0`), since anything may change before it. When there are no changes
the message reads `no exported API changes since v1.4.0`, so you can see
which tag `@latest` picked.

### Output formats

`--format=markdown` renders each package as a fenced `diff` block, which
GitHub colors, followed by the same summary line, for a pull request
comment or a job summary:

````
**example.com/m/store**

```diff
- (*Client).Close: removed
-   func (c *Client) Close() error
! Open: changed
-   func Open(path string) (*Client, error)
+   func Open(path string, o Options) (*Client, error)
+ (*Client).Ping: added
+   func (c *Client) Ping() error
```

1 package changed · 2 incompatible · 1 compatible · would require: **MAJOR** (v1.4.0 → v2.0.0)
````

`--format=json` prints one document for tools and bots. `--breaking`
filters the change lists in every format; the summary always counts the
full diff.

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
        }
      ]
    }
  ],
  "warnings": [],
  "summary": {
    "packages_changed": 1,
    "incompatible": 2,
    "compatible": 1,
    "release": "major"
  }
}
```

`head` is the revision given or the literal `working tree`. `base_version`
and `next_version` appear when the base is a release tag. A package's
`status` is `changed`, `new` or `removed`, and `"internal": true` marks an
internal package under `--filter=all` or `internal`, which the summary then
also counts separately under `summary.internal` (the public counts and
`release` describe the public packages in the selection, none under
`internal`). A change's `kind` is `added`,
`removed` or `changed`, and `symbol` is empty for a whole-package change
(`package added`). `before` and `after` hold what the text layout prints on
its `-` and `+` lines: the old declaration of a removed symbol, the new one
of an added symbol, both for a `changed from X to Y` message; neither is
present with `--signatures=minimal`.
With `--pos`, each change also carries a `pos` object locating the
declaration, `{"rev": "v1.4.0", "file": "store/store.go", "line": 9,
"col": 18}`, where `rev` is absent for the working tree. `release` is
`major`, `minor` or `patch`.

### In GitHub Actions

A job that posts the API changes of a pull request to its summary, and
fails when they are incompatible:

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
dependency that is not in the module cache is an error.

### Failing on a release level

For CI, `--exit-fail=LEVEL` turns the semantic version bump the changes
would require into the exit code, whenever that bump is `LEVEL` or higher:

| Required bump | `--exit-fail=major` | `--exit-fail=minor` | `--exit-fail=patch` |
|---------------|--------------------:|--------------------:|--------------------:|
| MAJOR         | 100                 | 100                 | 100                 |
| MINOR         | 0                   | 101                 | 101                 |
| PATCH         | 0                   | 0                   | 102                 |

`--exit-fail=major` fails the build on incompatible changes, like the default
exit code 1 but with a distinct code. `--exit-fail=minor` also fails on
compatible additions, which is useful for a branch that must not grow the
API. `--exit-fail=patch` is always non-zero unless there is an error, so a
script can read the required bump straight from `$?`:

```
$ go-whatchanged --exit-fail=patch v1.4.0 >/dev/null
$ echo $?
100
```

Errors still exit 2 regardless of `--exit-fail`, including `--strict`
type-check warnings.

## Reading the output

Changes are grouped by package and shown as a patch of declarations: the
old declaration on a `-` line, the new one on a `+` line.

| Lines | Meaning | Color |
|-------|---------|-------|
| `-` | symbol removed | red, bold |
| `+` | compatible addition | green |
| `+` | incompatible addition (a method on an interface, for example) | green, bold |
| `-` then `+` | symbol changed (signature, type, constant value): the old declaration, then the new one | grey, then orange (bold when incompatible) |

Bold marks an incompatible change. A removed symbol shows what went away, an
added one what arrived, and a `changed from X to Y` message becomes a small
patch of both, greyed out and orange rather than red and green so that it
reads as one edit, and so that two long signatures can be compared column
by column. Declarations are printed as source would, with parameter names
and foreign types qualified by package name (`func Open(path string)
(*Client, error)`, `func (c *Client) Ping() error`, `type Options
struct{Timeout int}`, `const Version untyped int = 1`, `field
Config.Timeout int` for a struct field, `func (Sizer) Size() int` for an
interface method). A constant whose value changed shows both values:
`const Version untyped string = "1.4.0"` on the `-` line and the new value
on the `+` line.

With `--pos`, each declaration ends with its position, dimmed and aligned
per package: on the `+` line for an addition or a change, where the new
declaration is, and on the `-` line for a removal, on the base side,
prefixed with the revision like the positions in warnings:

```
$ go-whatchanged --pos @latest
example.com/m/store
  - func (c *Client) Close() error                      v1.4.0:store/store.go:9:18
  - func Open(path string) (*Client, error)
  + func Open(path string, o Options) (*Client, error)  store/store.go:14:6
  + func (c *Client) Ping() error                       store/store.go:11:18
```

Working tree positions are relative to the module root, so terminals and
editors can open them.

A change with no declaration to show keeps apidiff's message instead, one
line marked with a glyph, and `--signatures=minimal` prints every change
that way, with the message as is: `~ Open: changed from func(string)
(*Client, error) to func(string, Options) (*Client, error)`.

| Glyph | Meaning | Color |
|-------|---------|-------|
| `-`   | symbol removed | red, bold |
| `!`   | incompatible addition (a method on an interface, for example) | red, bold |
| `~`   | incompatible change (signature, type, constant value) | yellow, bold |
| `~`   | compatible change | cyan |
| `+`   | compatible addition | green |

Such lines are `+ package added` and `- package removed` for a package
without exported API (see below), `~ T: no longer implements fmt.Stringer`
and the like for changes apidiff describes in words rather than types, and
`~ Open: changed` for a changed symbol that cannot be looked up on both
sides, which then shows the bare types quoted in the message on `-` and
`+` lines under it.

A package header carries `(new)` when it exists only on the head side and
`(removed)` when it exists only on the base side. A package without any
exported API, one that only registers itself in `init`, say, still gets a
`package added` (compatible) or `package removed` (incompatible) line when
it appears or disappears, since importers notice either way. A directory
that becomes a nested module counts as removed. The summary line ends with
the semantic version bump the changes would require: `MAJOR` if anything is
incompatible, `MINOR` if only compatible changes were made, `PATCH` otherwise.
The counts always describe the full diff, even with `--breaking`. When the
base is a release tag the summary also names the next version, see
[Since the last release](#since-the-last-release).

Type-check problems are reported on stderr as `warn: <package>: <error>` and
never abort the diff unless `--strict` is set. Positions on the base side are
prefixed with the revision (`v1.4.0:store/store.go:12:3`); positions in the
working tree are relative to the module root. A module whose `go` directive
is newer than the Go that built `go-whatchanged` is type-checked as the
newest version the binary knows, with a single `warn: <module>: go.mod
requires go 1.25 but go-whatchanged was built with go1.24; ...` line, which
`--strict` also treats as fatal.

## Guarantees

The tool is safe to run at any time, in any state of your checkout,
including a linked worktree created with `git worktree add`:

- **No disk writes.** No temporary directories, no checkouts, no build cache,
  no `go.sum` edits. It only reads the repository, `.git`, `$GOROOT/src` and
  `$GOMODCACHE`.
- **No `go` command.** Both sides are parsed and type-checked in-process with
  `go/build`, `go/parser` and `go/types`. Cgo is disabled; `import "C"` is
  faked.
- **No network.** A dependency missing from the module cache is a clear
  error: run `go mod download` and try again.
- **No repository mutation.** Only go-git's object store is used. The
  worktree, index and refs are never touched.

## How it works

Each side gets its own `go/build` context whose filesystem hooks are served
either from the git tree (mounted at a synthetic path) or from the working
directory; everything outside the module falls through to the real
filesystem so `$GOROOT` and the module cache stay reachable. Import paths are
resolved from `go.mod` alone, which is sound for modules at `go 1.17` or
newer because graph pruning guarantees every module providing an imported
package is listed. Dependency packages are type-checked once and shared
between the two sides when their imports resolve identically on both: a
dependency that imports the main module (grpc-go and go-control-plane import
each other), or one whose transitive imports the two `go.mod` files pin to
different versions, is checked once per side instead, so that neither side
is ever linked against the other side's packages.

## Non-goals (for now)

A stable library API, multi-platform union diffs, `go.work`, vendor mode,
GOPATH mode, cgo-only APIs, module graph resolution beyond `go.mod`, proxy
fetching, and comparing the APIs of dependencies.

## License

[MIT](LICENSE).

# go-whatchanged

What changed in the public API?

Pretty semantic diff of a Go module's exported API, powered by 
[`golang.org/x/exp/apidiff`](https://pkg.go.dev/golang.org/x/exp/apidiff).

Read-only: No mutations to your filesystem, no git clones, no git worktrees.

## Usage

```
$ go-whatchanged @latest
example.com/m/store
  - (*Client).Close: removed
  ~ Open: changed
      - func Open(path string) (*Client, error)
      + func Open(path string, o Options) (*Client, error)
  + (*Client).Ping: added
  + Options: added

example.com/m/util
  ! Sizer.Size: added

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
  --breaking         show only incompatible changes
  --format string    text | markdown | json (default text; see below)
  --color string     auto | always | never (default auto; honors NO_COLOR)
  --strict           type-check errors are fatal (default: warn)
  --exit-fail LEVEL  exit 100/101/102 when the required bump is major, minor
                     or patch, or higher (see below)
  --version          print the version and exit

Exit codes: 0 no incompatible changes · 1 incompatible changes · 2 error
```

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
! Open: changed
-   func Open(path string) (*Client, error)
+   func Open(path string, o Options) (*Client, error)
+ (*Client).Ping: added
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
          "message": "(*Client).Close: removed"
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
`status` is `changed`, `new` or `removed`; a change's `kind` is `added`,
`removed` or `changed`, and `symbol` is empty for a whole-package change
(`package added`). `before` and `after` are present for `changed from X to
Y` messages and hold what the text layout prints on its `-` and `+` lines.
`release` is `major`, `minor` or `patch`.

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

Changes are grouped by package, one line per change:

| Glyph | Meaning | Color |
|-------|---------|-------|
| `-`   | symbol removed | red, bold |
| `!`   | incompatible addition (a method on an interface, for example) | red, bold |
| `~`   | incompatible change (signature, type, constant value) | yellow, bold |
| `~`   | compatible change | cyan |
| `+`   | compatible addition | green |

A `changed from X to Y` message is split into a small patch: the old
declaration on a red `-` line and the new one on a green `+` line, so two
long signatures can be compared column by column. The lines show the full
declaration with parameter names (`func Open(path string) ...`,
`func (c *Client) Ping() ...`, `const Version untyped int`) when the change
is to a whole symbol; a change to a struct field falls back to the bare
types.

A constant whose value changed shows both values: `const Version untyped
string = "1.4.0"` on the `-` line and the new value on the `+` line.

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

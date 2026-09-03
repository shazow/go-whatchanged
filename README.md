# go-whatchanged

[![CI](https://github.com/shazow/go-whatchanged/actions/workflows/ci.yml/badge.svg)](https://github.com/shazow/go-whatchanged/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/shazow/go-whatchanged.svg)](https://pkg.go.dev/github.com/shazow/go-whatchanged)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

What changed in the public API?

`go-whatchanged` prints a semantic diff of a Go module's exported API between
two git revisions, or between a revision and your uncommitted work, and
names the semantic version the changes call for. The comparison itself is
done by [`golang.org/x/exp/apidiff`](https://pkg.go.dev/golang.org/x/exp/apidiff);
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

## Install

```
go install github.com/shazow/go-whatchanged@latest
```

Needs Go 1.26 or newer to build. Nothing is ever downloaded at run time,
so the module's dependencies have to be in the module cache already: `go
mod download` in the checkout covers what its `go.mod` pins. If the base
revision pins versions your checkout does not have, the error names the
missing module, and the quickest fix is to download from a copy of that
revision's `go.mod`:

```
mkdir -p /tmp/base && git show v1.4.0:go.mod > /tmp/base/go.mod && (cd /tmp/base && go mod download)
```

## Usage

| Question | Command |
|---|---|
| What do my uncommitted changes do to the API? | `go-whatchanged` |
| What has changed since the last release? | `go-whatchanged @latest` |
| What did release `v1.4.0` ship? | `go-whatchanged @latest v1.4.0` |
| What does this branch change, compared to `main`? | `go-whatchanged origin/main` |
| Which of these changes break importers? | `go-whatchanged --filter=breaking @latest` |
| Would this pass a compatibility gate? | `go-whatchanged --exit-fail=major @latest` |

```
go-whatchanged [options] [<base> [<head>]]

  base   commit-ish for the old side: hash, tag, branch, HEAD~2, ...
         or @latest, the newest release tag among the ancestors of head.
         Default: HEAD.
  head   commit-ish for the new side. Default: the working tree.

Options:
  --repo=DIR         path inside a git repository (default: current directory)
  --pkg=PATTERN      diff only packages matching PATTERN (repeatable)
  --exclude=PATTERN  skip packages matching PATTERN (repeatable)
  --filter=WHICH     all | public | internal: which packages take part, and
                     breaking: only incompatible changes (default all)
  --signatures=HOW   full | minimal: declarations, or one line per change
                     (default full)
  --pos              annotate changes with source positions
  --format=LAYOUT    text | markdown (or md) | json (default text)
  --color=WHEN       auto | always | never (default auto; honors NO_COLOR)
  --strict           type-check errors are fatal (default: warn)
  --exit-fail=LEVEL  exit 100/101/102 when the required bump is major, minor
                     or patch, or higher
  --version          print the version and exit
  -h, --help         print the full help
```

`GOOS` and `GOARCH` in the environment select the build target, as for the
`go` command.

### Cutting a release

Before tagging, `go-whatchanged @latest` shows everything since the last
release and the version it calls for. After tagging, `go-whatchanged
@latest v1.4.0` lists what the tag ships, for the release notes:

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

`@latest` is the highest release tag among the ancestors of the head
commit (`HEAD` when the head is the working tree), skipping the head
commit's own tags, which is why the second command reports `v1.3.0 →
v1.4.0` rather than an empty diff. Release tags are the ones the `go`
command would publish: `v1.4.0`, `sub/v1.4.0` for a module below the
repository root, `v2.x.y` for a `/v2` module. Tags like `1.4.0` or `v1.4`
are ignored, and a tag on an unmerged branch is not an ancestor.

One gotcha: with uncommitted changes on the tagged commit itself, `@latest`
skips that tag and picks the release before it, or fails when there is
none. Name the tag: `go-whatchanged v1.4.0`.

The version the summary suggests is the next major for an incompatible
change, or the next minor while the module is still at `v0`; a
pre-release always suggests its final release.

### Large modules and applications

In a large module, `--pkg` and `--exclude` narrow the diff. They take
import paths or module-relative paths, and `...` matches anything, as in
the `go` command: `store/...` is the store package and everything below
it. The summary and the exit code then describe the selection only. `main`
packages are never diffed.

When you are reviewing an application rather than a library, the internal
packages matter too. By default they are listed after the public API with
a summary line of their own, as in the example above, and they never count
towards the public API's totals, the required release or the exit code.
`--filter=public` leaves them out, `--filter=internal` shows only them, and
`breaking` narrows any of these to incompatible changes:
`--filter=public,breaking`. The counts in the summary always describe the
full diff.

## Reading the output

Bold marks an incompatible change: a removal, a changed signature, a
method added to an interface. A changed symbol shows its old declaration
greyed out above the new one in orange, rather than red and green, so that
the pair reads as one edit and long signatures line up for comparison.

`--pos` appends the position of each declaration: prefixed with the
revision on the base side (`v1.4.0:store/store.go:11:18`), and relative to
the module root in the working tree, so that your editor can open it.

`--signatures=minimal` prints apidiff's one-line message per change
instead, such as `~ Open: changed from func(string) (*Client, error) to
func(string, Options) (*Client, error)`. The full layout falls back to the
same line when there is no declaration to show, as in `+ package added`.

| Glyph | Meaning |
|---|---|
| `-` | removed |
| `+` | compatible addition |
| `!` | incompatible addition |
| `~` | changed (bold when incompatible) |

## Output formats

For a pull request comment or a job summary, `--format=markdown` renders
each package as a fenced `diff` block, which GitHub colors. A code block
has no bold, so an incompatible `+` or `~` line is marked `!` instead,
which GitHub colors orange; a removal keeps its `-`:

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

For scripts and bots, `--format=json` prints one document whose field
names are part of the tool's interface:

```json
{
  "base": "v1.4.0",
  "head": "working tree",
  "base_version": "v1.4.0",
  "next_version": "v2.0.0",
  "packages": [
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
    "packages_changed": 1,
    "incompatible": 1,
    "compatible": 0,
    "release": "major",
    "internal": {
      "packages_changed": 0,
      "incompatible": 0,
      "compatible": 0
    }
  }
}
```

`base_version` and `next_version` appear when the base is a release tag.
A package's `status` is `changed`, `new` or `removed`, and `"internal":
true` marks an internal package. A change's `before` and `after` hold the
old and new declarations, whichever apply; with `--pos` it also carries
`{"rev": "v1.4.0", "file": "store/store.go", "line": 11, "col": 18}`,
where `rev` is absent for the working tree. `warnings` holds the `warn:`
lines stderr would show.

## Exit codes

In a script, the exit code is 0 when no change is incompatible, 1 when
one is, and 2 on an error. To gate on a release level instead,
`--exit-fail=LEVEL` exits with a code naming the bump the changes would
require, whenever it is `LEVEL` or higher; `--exit-fail=patch` therefore
always reports the bump:

| Required bump | `--exit-fail=major` | `--exit-fail=minor` | `--exit-fail=patch` |
|---|---:|---:|---:|
| MAJOR | 100 | 100 | 100 |
| MINOR | 0 | 101 | 101 |
| PATCH | 0 | 0 | 102 |

## GitHub Action

To get the diff on every pull request, add the action to a workflow. It
builds the tool from the ref that pins it, runs it on the checkout and
appends the diff to the job summary:

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
does to the API; when the checkout is too shallow to find it, the tip of
the base branch stands in, with a warning. On any other event the base
defaults to `@latest`, so a workflow that runs on a tag push gets the
release notes of the tag. The step needs a Go toolchain on `PATH` and
`jq`, and downloads the dependencies of both sides itself, without writing
to the checkout.

| Input | Default | Meaning |
|---|---|---|
| `base` | see above | Commit-ish or `@latest` for the old side. |
| `head` | `HEAD` | Commit-ish for the new side. Empty means the working tree. |
| `working-directory` | `.` | The module to diff, for repositories with several. |
| `pkg`, `exclude` | | Package patterns, comma- or newline-separated. |
| `filter` | `all` | `all`, `public` or `internal`. |
| `breaking` | `false` | Show only incompatible changes. |
| `signatures` | `full` | `full` or `minimal`. |
| `pos` | `false` | Annotate changes with source positions. |
| `strict` | `false` | Treat type-check errors as fatal. |
| `goos`, `goarch` | the runner's | Build target. |
| `fail-on` | | `major`, `minor` or `patch`: fail the step at that level or above. |
| `summary` | `true` | Append the diff to the job summary. |
| `title` | `API changes` | Heading above the diff in the summary. Empty for none. |

To use the result in later steps: the summary line becomes an annotation
on the run, a warning when there are incompatible changes, and the report
and its numbers are outputs:

| Output | Value |
|---|---|
| `release` | `major`, `minor` or `patch` (no changes) |
| `packages-changed`, `incompatible`, `compatible` | the counts of the summary line |
| `base`, `head` | the revisions as diffed; `base` is the tag `@latest` picked |
| `base-version`, `next-version` | when the base is a release tag |
| `summary` | the summary line(s) as plain text |
| `markdown`, `json` | the whole report in either format |

To post it as a pull request comment, add one step:

```yaml
      - uses: shazow/go-whatchanged@main
        id: api
      - if: steps.api.outputs.packages-changed != '0'
        env:
          GH_TOKEN: ${{ github.token }} # with permissions: pull-requests: write
          BODY: ${{ steps.api.outputs.markdown }}
        run: gh pr comment "${{ github.event.pull_request.number }}" --body "$BODY"
```

On another CI system, or without the action, the same job is a few plain
steps:

```yaml
- uses: actions/checkout@v4
  with:
    fetch-depth: 0
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

## Guarantees

Run it on a dirty checkout, on a shared CI runner, in a linked worktree
from `git worktree add`: nothing is changed.

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

For the curious, and for anyone debugging an import that resolves
unexpectedly: each side gets its own `go/build` context whose filesystem
hooks are served either from the git tree, mounted at a synthetic path, or
from the working directory; everything outside the module falls through
to the real filesystem, so `$GOROOT` and the module cache stay reachable.
Import paths are resolved from `go.mod` alone, which is sound for modules
at `go 1.17` or newer because graph pruning guarantees that every module
providing an imported package is listed. The two sides load concurrently,
and dependency packages are type-checked once and shared between them when
their imports resolve identically on both; otherwise, as when a dependency
imports the main module, they are checked once per side, so that neither
side is ever linked against the other side's packages.

## Limitations

- One platform per run, picked by `GOOS` and `GOARCH`; no union diff
  across platforms.
- No `go.work`, vendor mode or GOPATH mode. Dependencies come from the
  module cache and `replace` directories, and `go.mod` must say `go 1.17`
  or newer.
- Only the main module's API is compared, not its dependencies'.
- Not a library: everything lives under `internal/`, and the command line
  and the JSON layout are the interface.

## License

[MIT](LICENSE).

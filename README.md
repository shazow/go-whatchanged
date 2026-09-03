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
- **Release-aware.** `@latest` stands for the last release tag, and the
  summary names the version the changes call for.
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

## Usage

| Question | Command |
|---|---|
| What do my uncommitted changes do to the API? | `go-whatchanged` |
| What has changed since the last release? | `go-whatchanged @latest` |
| What did release `v1.4.0` ship? | `go-whatchanged @latest v1.4.0` |
| What does this branch change, compared to `main`? | `go-whatchanged origin/main` |
| Which of these changes break importers? | `go-whatchanged --filter=breaking @latest` |
| What changed in the commands, the `main` packages? | `go-whatchanged --filter=main @latest` |
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
  --filter=WHICH     all | public | internal: which packages take part;
                     main: include main packages; breaking: only
                     incompatible changes (default all)
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

`@latest` is the last release tag before the head commit, where release
tags are the ones the `go` command would publish: `v1.4.0`, `sub/v1.4.0`
for a module below the repository root, `v2.x.y` for a `/v2` module. With
uncommitted changes on the tagged commit itself, `@latest` therefore skips
that tag; name it instead: `go-whatchanged v1.4.0`.

At `v0` an incompatible change suggests the next minor, and a pre-release
always suggests its final release.

### Large modules and applications

`--pkg` and `--exclude` take package patterns as the `go` command does,
`store/...` for the store package and everything below it.

Internal packages are listed after the public API with a summary line of
their own, as above, and never count towards the public API's totals or
the exit code. `main` packages are left out unless `--filter=main` asks
for them; nothing can import a command, so they join the internal
section. With `--filter=breaking`, the counts in the summary still
describe the full diff.

## Reading the output

Bold marks an incompatible change: a removal, a changed signature, a
method added to an interface. A line with no declaration to show, and
every line under `--signatures=minimal`, carries apidiff's message behind
a glyph:

| Glyph | Meaning |
|---|---|
| `-` | removed |
| `+` | compatible addition |
| `!` | incompatible addition |
| `~` | changed (bold when incompatible) |

## Output formats

`--format=markdown` renders each package as a `diff` block for a pull
request comment or a job summary, with `!` marking the incompatible lines
that the terminal shows in bold:

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

`--format=json` prints one document for scripts and bots; its field names
are part of the tool's interface. `base_version` and `next_version` are
present when the base is a release tag, `pos` with `--pos`.

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

## Exit codes

The exit code is 0, 1 for incompatible changes, or 2 for an error.
`--exit-fail=LEVEL` fails at that release level or above, with a code that
names the bump:

| Required bump | `--exit-fail=major` | `--exit-fail=minor` | `--exit-fail=patch` |
|---|---:|---:|---:|
| MAJOR | 100 | 100 | 100 |
| MINOR | 0 | 101 | 101 |
| PATCH | 0 | 0 | 102 |

## GitHub Action

Add the action to a workflow to get the diff on every pull request. It
builds the tool from the ref that pins it and appends the diff to the job
summary; on a tag push, it lists what the tag ships:

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

| Input | Default | Meaning |
|---|---|---|
| `base` | the pull request's merge-base, else `@latest` | Commit-ish or `@latest` for the old side. |
| `head` | `HEAD` | Commit-ish for the new side. Empty means the working tree. |
| `working-directory` | `.` | The module to diff, for repositories with several. |
| `pkg`, `exclude` | | Package patterns, comma- or newline-separated. |
| `filter` | `all` | `all`, `public` or `internal`, plus `main` for the commands: `all,main`. |
| `breaking` | `false` | Show only incompatible changes. |
| `signatures` | `full` | `full` or `minimal`. |
| `pos` | `false` | Annotate changes with source positions. |
| `strict` | `false` | Treat type-check errors as fatal. |
| `goos`, `goarch` | the runner's | Build target. |
| `fail-on` | | `major`, `minor` or `patch`: fail the step at that level or above. |
| `summary` | `true` | Append the diff to the job summary. |
| `title` | `API changes` | Heading above the diff in the summary. Empty for none. |

Outputs for later steps:

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
- **No network.** Dependencies come from the module cache; nothing is
  fetched.
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
  module cache and `replace` directories.
- Only the main module's API is compared, not its dependencies'.
- Not a library, for now: everything lives under `internal/`, and the
  command line and the JSON layout are the interface.

## License

[MIT](LICENSE).

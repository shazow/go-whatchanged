# go-whatchanged

[![CI](https://github.com/shazow/go-whatchanged/actions/workflows/ci.yml/badge.svg)](https://github.com/shazow/go-whatchanged/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/shazow/go-whatchanged.svg)](https://pkg.go.dev/github.com/shazow/go-whatchanged)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

What changed in the public API?

`go-whatchanged` prints an API diff of a Go module's API between
two versions.

- **Read-only:** Happy path is optimized to avoid writing to the filesystem (no temporary directories, git clones, or worktrees). Only `go` commands are used for comparing remote packages that are not already cached, use `--fsreadonly` to disable any features that rely on writing the filesystem.
- **Release-aware:** All standard Go version tags supported (`@latest` for latest tagged release; `@v1.2.3` for a specific tag; `@HEAD` for a ref like HEAD, etc).
- **CI-ready.** Markdown for pull requests, JSON for tools, exit codes for
  gates, and a [GitHub Action](#github-action) that posts the diff to the
  job summary and as a pull request comment.

**Status:** beta. It was fairly vibecoded, out of a desperate need to review
large pull requests better, but the readonly constraints add a lot of
safety (and efficiency) to how it works. It's quite useful!

## Example

```
$ go-whatchanged @latest
example.com/m/store
  - func (c *Client) Close() error
  - func Open(path string) (*Client, error)
  + func Open(path string, o Options) (*Client, error)
  + func (c *Client) Ping() error
  + type Options struct{ Timeout int }

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
| What did release `v1.4.0` ship? | `go-whatchanged @latest @v1.4.0` |
| What does this branch change, compared to `main`? | `go-whatchanged @origin/main` |
| What is unreleased on `main` of a module I don't have checked out? | `go-whatchanged github.com/stretchr/testify@latest` |
| What did a published release change? | `go-whatchanged github.com/stretchr/testify@v1.9.0 @v1.10.0` |
| What has changed since the last release in another checkout? | `go-whatchanged ~/src/m@latest` |
| Which of these changes break importers? | `go-whatchanged --filter=breaking @latest` |
| What changed in the commands, the `main` packages? | `go-whatchanged --filter=main @latest` |
| Would this pass a compatibility gate? | `go-whatchanged --exit-fail=major @latest` |

```
go-whatchanged [options] [<base> [<head>]]

  base   the old side, as location@version: @v1.4.0, @HEAD~2 or
         @origin/main in the current repository, @latest for its newest
         release tag among the ancestors of head; github.com/x/m@v1.2.0 or
         github.com/x/m@latest for a published module; ~/src/m@v1.2.0 for
         another checkout. Default: @HEAD.
  head   the new side, in the same forms. @main alone means main in the
         base's repository or module. Default: the working tree, or @HEAD
         for a module.

Options:
  --pkg=PATTERN      diff only packages matching PATTERN (repeatable)
  --exclude=PATTERN  skip packages matching PATTERN (repeatable)
  --filter=WHICH     all, or any of public | internal | main: which
                     packages take part; breaking: only incompatible
                     changes (default all)
  --pos              annotate changes with source positions
  --format=LAYOUT    text | markdown (or md) | json (default text)
  --color=WHEN       auto | always | never (default auto; honors NO_COLOR)
  --strict           type-check errors are fatal (default: warn)
  --fsreadonly       never write to the filesystem or run the go command
  --exit-fail=LEVEL  exit 100/101/102 when the required bump is major, minor
                     or patch, or higher
  --version          print the version and exit
  -h, --help         print the full help
```

### Cutting a release

Before tagging, `go-whatchanged @latest` shows everything since the last
release and the version it calls for. After tagging, `go-whatchanged
@latest @v1.4.0` lists what the tag ships, for the release notes:

```
$ go-whatchanged @latest @v1.4.0
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
that tag; name it instead: `go-whatchanged @v1.4.0`.

At `v0` an incompatible change suggests the next minor, and a pre-release
always suggests its final release.

### Naming the sides

Each side is `location@version`, the way the go command names versions.
The location is a module path, a directory, or nothing:

| Location | `@version` means | Alone |
|---|---|---|
| none: `@v1.4.0`, `@HEAD~2`, `@origin/main` | a revision of the current repository, or for the head, of the base's repository or module | the default: `@HEAD` as base, the working tree as head |
| `@latest` | the newest release tag among the ancestors of the head | |
| a module path: `github.com/x/m@v1.2.0` | a published module version, fetched into the module cache; `@latest` its newest release, `@main` a branch, `@HEAD` the default branch, `@abc1234` a commit | an error: a module needs a version |
| a directory: `~/src/m@v1.2.0`, `./m@latest`, `../m@main` | a revision of that checkout | that checkout's `HEAD` as base, its working tree as head |

A directory is anything spelled as a path, starting with `./`, `../`, `~/`
or `/`, as the go command tells a directory from an import path. Module
versions come through the proxy the go command is configured for, and a
branch or `@HEAD` may lag the repository by the proxy's cache. A version
whose go.mod declares go 1.16 or older cannot be diffed; see
[Limitations](#limitations).

### Examples

A library you maintain, from its checkout:

```sh
go-whatchanged                          # what my uncommitted changes do to the API
go-whatchanged @latest                  # everything since the last release, and the version it calls for
go-whatchanged @latest @v1.4.0          # what v1.4.0 shipped, for the release notes
go-whatchanged @origin/main             # what this branch changes, compared to main
go-whatchanged @v1.3.0 @v1.4.0          # between any two revisions
go-whatchanged --filter=breaking @latest        # only the changes that break importers
go-whatchanged --exit-fail=major @latest        # a compatibility gate: exit 100 on a required major
```

A module you depend on, without a checkout, before and after an upgrade:

```sh
go-whatchanged github.com/gorilla/mux@v1.8.0 @v1.8.1          # what the upgrade changes
go-whatchanged --filter=breaking github.com/spf13/viper@v1.18.0 @v1.19.0
go-whatchanged github.com/stretchr/testify@latest             # what is unreleased on its default branch
go-whatchanged github.com/stretchr/testify@latest @master     # or on a named branch
go-whatchanged github.com/stretchr/testify@v1.10.0 @abc1234   # up to a commit
```

A monorepo, with a module below the root and tags like `services/api/v1.2.0`:

```sh
cd services/api && go-whatchanged @latest         # the module of the current directory
go-whatchanged ./services/api@latest              # the same, from the root
go-whatchanged ./services/api@v1.1.0 @v1.2.0
```

An application, where the API that matters is internal or the commands:

```sh
go-whatchanged --filter=internal @latest          # the internal packages alone
go-whatchanged --filter=main @latest              # the main packages: changed flags, say
go-whatchanged --pkg store/... --exclude cmd/... @latest
```

Other checkouts, a fork against its upstream, another platform, a script:

```sh
go-whatchanged ~/src/m@latest                     # another checkout, from anywhere
go-whatchanged ~/src/upstream@v1.4.0 ~/src/fork@main
GOOS=windows go-whatchanged @latest               # the API as built for another platform
go-whatchanged --format=json @latest | jq .summary
```

### Large modules and applications

`--pkg` and `--exclude` take package patterns as the `go` command does,
`store/...` for the store package and everything below it.

Internal packages are listed after the public API with a summary line of
their own, as above, and `main` packages, which nothing can import, after
them in a section of theirs; neither counts towards the public API's
totals or the exit code. `--filter` picks the sections: `--filter=public`
for the importable API alone, `--filter=main` for the commands alone,
`--filter=public,main` for both. With `--filter=breaking`, the counts in
the summary still describe the full diff.

## Reading the output

Each change shows the declaration of its symbol, formatted as gofmt
formats it; the fields of a struct are shown together, as a fragment of
the struct with the changed fields alone inside and `// ...` for the
rest. Bold marks an
incompatible change: a removal, a changed signature, a method added to an
interface. A line with no declaration to show carries apidiff's message
behind a glyph:

| Glyph | Meaning |
|---|---|
| `-` | removed |
| `+` | compatible addition |
| `!` | incompatible addition |
| `~` | changed (bold when incompatible) |

## Output formats

`--format=markdown` renders each package as a `go` block, which GitHub
highlights as Go, for a pull request comment or a job summary. The
changes are grouped under `// Removed`, `// Changed` and `// Added`, from
what breaks to what is merely new; a changed symbol shows its old
declaration with `// ->` trailing and its new one below it, so that both
sides are highlighted and read as one edit. A change whose compatibility
is not what its group implies says so in a trailing comment, such as a
method added to an interface, and `--pos` positions trail each line in a
column.

````
$ go-whatchanged --format=markdown @latest
**example.com/m/store**

```go
// Removed
func (c *Client) Close() error

// Changed
func Open(path string) (*Client, error) // ->
func Open(path string, o Options) (*Client, error)

// Added
func (c *Client) Ping() error
type Options struct{ Timeout int }
```

**example.com/m/util**

```go
// Added
func (Sizer) Size() int // incompatible
```

2 packages changed · 3 incompatible · 2 compatible · would require: **MAJOR** (v1.4.0 → v2.0.0)
````

`--format=json` prints one document for scripts and bots; its field names
are part of the tool's interface. `base_version` and `next_version` are
present when the base is a release tag, `pos` with `--pos`, and `struct`
on a struct field's change, whose `before` and `after` are the field's
declaration inside it.

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
    },
    "main": {
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
builds the tool from the ref that pins it, appends the diff to the job
summary under a heading whose glyph names the release the changes call
for, 🟢 none, 🟡 minor, 🔴 major, and posts it as a pull request comment
folded under its summary line. On a tag push, it lists what the tag
ships in the job summary:

```yaml
on: pull_request

permissions:
  contents: read
  pull-requests: write # for the comment

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

The comment is updated on every later push instead of adding another,
and the first one waits until there is something to show, so a pull
request that never touches the API gets none. It needs `pull-requests:
write`, which the default token lacks on a pull request from a fork;
there, the action says so in a notice and the job summary still has the
diff. `comment: false` keeps the diff to the job summary:

```yaml
permissions:
  contents: read

    steps:
      # ...
      - uses: shazow/go-whatchanged@main
        with:
          comment: false
```

| Input | Default | Meaning |
|---|---|---|
| `base` | the pull request's merge-base, else `@latest` | The old side, `@rev` or `@latest`. |
| `head` | `@HEAD` | The new side, `@rev`. Empty means the working tree. |
| `working-directory` | `.` | The module to diff, for repositories with several. |
| `pkg`, `exclude` | | Package patterns, comma- or newline-separated. |
| `filter` | `all` | `all`, or any of `public`, `internal` and `main`: `public,main`. |
| `breaking` | `false` | Show only incompatible changes. |
| `pos` | `false` | Annotate changes with source positions. |
| `strict` | `false` | Treat type-check errors as fatal. |
| `goos`, `goarch` | the runner's | Build target. |
| `fail-on` | | `major`, `minor` or `patch`: fail the step at that level or above. |
| `summary` | `true` | Append the diff to the job summary. |
| `title` | `API changes` | Heading above the diff in the summary and the comment. Empty for none. |
| `comment` | `true` | Post the diff as a pull request comment and keep it updated. `false` for the job summary alone. |
| `token` | `github.token` | The token for the comment. |

Outputs for later steps:

| Output | Value |
|---|---|
| `release` | `major`, `minor` or `patch` (no changes) |
| `packages-changed`, `incompatible`, `compatible` | the counts of the summary line |
| `base`, `head` | the revisions as diffed; `base` is the tag `@latest` picked |
| `base-version`, `next-version` | when the base is a release tag |
| `summary` | the summary line(s) as plain text |
| `markdown`, `json` | the whole report in either format |

On another CI system, or without the action, the same job is a few plain
steps:

```yaml
- uses: actions/checkout@v4
  with:
    fetch-depth: 0
- uses: actions/setup-go@v5
  with:
    go-version-file: go.mod
- run: go install github.com/shazow/go-whatchanged@latest
- run: |
    go-whatchanged --format=markdown @origin/${{ github.base_ref }} @HEAD \
      >> "$GITHUB_STEP_SUMMARY"
- run: go-whatchanged --exit-fail=major @origin/${{ github.base_ref }} @HEAD
```

## Guarantees

Run it on a dirty checkout, on a shared CI runner, in a linked worktree
from `git worktree add`: nothing in the repository is changed.

- **No repository writes.** No temporary directories, no checkouts, no
  build cache, no `go.sum` edits. It reads the repository, `.git`,
  `$GOROOT/src`, `$GOMODCACHE` and the directories that `replace`
  directives point at.
- **The module cache is the one exception.** The modules that a side's
  `go.mod` pins and the cache lacks are fetched together with one
  `go mod download`, run from outside any module with `GOWORK=off`, so
  that the go command never sees, let alone edits, the checkout's
  `go.mod`, `go.sum` or `go.work`. That download is the only time the tool
  runs the go command or reaches the network, through the usual `GOPROXY`,
  `GOPRIVATE` and `GONOSUMDB`.
- **`--fsreadonly` removes the exception.** With it the tool never writes
  anywhere and never runs the go command; a module the cache lacks is an
  error that says to drop the flag.
- **In-process type-checking.** Both sides are parsed and type-checked
  with `go/build`, `go/parser` and `go/types`. Cgo is disabled: files that
  `import "C"` are skipped, so an API declared only in them is not diffed.
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

A module the cache lacks is fetched through one small interface,
`internal/modfetch.Source`: resolve a query to a version, fetch a version
to a readable tree, or several at once. Before a side is type-checked,
every requirement of its `go.mod` that the cache lacks is fetched as one
batch, in parallel, and anything that still turns out to be missing is
fetched on demand. The interface's only implementation runs the go
command; a client for the module proxy protocol could replace it without
the rest of the tool noticing, and `--fsreadonly` simply leaves it out.

## Limitations

- One platform per run, picked by `GOOS` and `GOARCH`; no union diff
  across platforms.
- No `go.work`, vendor mode or GOPATH mode. Dependencies come from the
  module cache and `replace` directories.
- Modules at `go 1.16` or older in their go.mod cannot be diffed, whether a
  revision or a published version: without module graph pruning, go.mod
  alone does not say where every import comes from.
- Only the main module's API is compared, not its dependencies'.
- Not a library, for now: everything lives under `internal/`, and the
  command line and the JSON layout are the interface.

## License

[MIT](LICENSE).

# The GitHub Action

Back to the [README](../README.md).

Add the action to a workflow and every pull request that touches the API
gets a comment like this one, folded under its verdict and updated on
every later push:

<details>
<summary>🔴 <b>API changes</b>: 2 packages changed · 3 incompatible · 2 compatible · would require: MAJOR</summary>

### `example.com/m/store`

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

### `example.com/m/util`

```go
// Added
func (Sizer) Size() int // incompatible
```

2 packages changed · 3 incompatible · 2 compatible · would require: **MAJOR**

<sub>Compared <code>a1b2c3d</code> with <code>3f2a9c1</code> · <a href="#">job summary</a> · powered by <a href="https://github.com/shazow/go-whatchanged">go-whatchanged</a></sub>
</details>

The same report goes to the job summary under a heading whose glyph
names the release the changes call for, 🟢 none, 🟡 minor, 🔴 major. A
pull request that never touches the API gets no comment at all.

## Adding it

```yaml
name: API changes
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
      - uses: actions/setup-go@v5 # optional; its cache makes the build instant
        with:
          go-version-file: go.mod
      - uses: shazow/go-whatchanged@main
```

That is all of it. The diff is of the pull request's merge-base against
its head, which is what the pull request itself changes, so `fetch-depth:
0` matters: with the default shallow checkout the action diffs against
the tip of the base branch instead, and says so in a warning.

The action builds the tool from the ref that pins it, so one ref names
the version of both; pin it by SHA if your policy asks for that. It
builds with whatever `go` is on the runner's path, which GitHub's runners
provide, and `actions/setup-go` is there for its cache: with it, the
build takes a few seconds on every run after the first.

The comment needs `pull-requests: write`. A pull request from a fork runs
with a read-only token, so its comment cannot be posted; the action says
so in a notice and the job summary still has the diff (see [Pull requests
from forks](#pull-requests-from-forks)). `comment: false` keeps the diff
to the job summary and needs no permission beyond `contents: read`:

```yaml
      - uses: shazow/go-whatchanged@main
        with:
          comment: false
```

## Recipes

**A compatibility gate.** `fail-on` fails the step at that release level
or above, after the summary and the comment are written, with the exit
code the command line would use (100 for a major, 101 for a minor, 102
for a patch):

```yaml
      - uses: shazow/go-whatchanged@main
        with:
          fail-on: major
```

The verdict is also an output, for a step that decides for itself:

```yaml
      - uses: shazow/go-whatchanged@main
        id: api
      - if: steps.api.outputs.release == 'major'
        run: echo "::warning::this pull request calls for a ${{ steps.api.outputs.release }} release"
```

**The public API alone**, without the internal and `main` packages that
the default lists after it, or only the incompatible changes:

```yaml
      - uses: shazow/go-whatchanged@main
        with:
          filter: public
          breaking: true
```

**A repository with several modules.** `working-directory` names the
module, and each one gets a comment of its own:

```yaml
jobs:
  api:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        module: [., services/api]
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: actions/setup-go@v5
        with:
          go-version-file: ${{ matrix.module }}/go.mod
      - uses: shazow/go-whatchanged@main
        with:
          working-directory: ${{ matrix.module }}
```

**Release notes on a tag push.** Without a pull request the base is
`@latest`, the newest release tag before the head commit, so on a tag
push the job summary lists what the tag ships and the `markdown` output
carries it, ready for the release notes. The very first tag has no
release before it and the step fails saying so:

```yaml
name: Release notes
on:
  push:
    tags: ['v*']

permissions:
  contents: read

jobs:
  api:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0 # the tags
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - uses: shazow/go-whatchanged@main
```

**Another platform.** The API is diffed as built for the runner's
platform; `goos` and `goarch` pick another, and a matrix over them diffs
each:

```yaml
      - uses: shazow/go-whatchanged@main
        with:
          goos: windows
```

## Pull requests from forks

On `pull_request`, a fork's token cannot comment, and the job summary is
where the diff is. A workflow on `pull_request_target` runs with the base
repository's token and can, at a price: the event is designed for
workflows that never run the pull request's code, and this one checks it
out. The tool only parses and type-checks that code, and never runs it,
but `go mod download` follows the pull request's `go.mod` and
`actions/setup-go` reads it for the Go version, so use this recipe only
if you have weighed that:

```yaml
name: API changes
on: pull_request_target

permissions:
  contents: read
  pull-requests: write

jobs:
  api:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          ref: refs/pull/${{ github.event.pull_request.number }}/merge
          fetch-depth: 0
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - uses: shazow/go-whatchanged@main
```

## Inputs and outputs

| Input | Default | Meaning |
|---|---|---|
| `base` | the pull request's merge-base, else `@latest` | The old side, `@rev` or `@latest`. |
| `head` | `@HEAD` | The new side, `@rev`. Empty means the working tree. |
| `working-directory` | `.` | The module to diff, for repositories with several. |
| `pkg`, `exclude` | | Package patterns, comma- or newline-separated. |
| `filter` | `all` | `all`, or any of `public`, `internal` and `main` for the packages and `api` and `imports` for the kinds of change: `public,main`, `public,api`. |
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

## Without the action

On another CI system, or without the action, the same job is a few plain
steps: check out with history, install the tool, and diff the merge-base
of the base branch against the head. On GitHub that is:

```yaml
- uses: actions/checkout@v4
  with:
    fetch-depth: 0
- uses: actions/setup-go@v5
  with:
    go-version-file: go.mod
- run: go install github.com/shazow/go-whatchanged@latest
- run: |
    base=$(git merge-base "origin/$GITHUB_BASE_REF" HEAD)
    go-whatchanged --format=markdown "@$base" @HEAD >> "$GITHUB_STEP_SUMMARY"
    go-whatchanged --exit-fail=major "@$base" @HEAD
```


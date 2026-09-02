# go-whatchanged

Colorized, read-only diff of a Go module's exported API, built on
[`golang.org/x/exp/apidiff`](https://pkg.go.dev/golang.org/x/exp/apidiff).

Answers "what did I change in the public API since `main` / `v1.4.0` /
`HEAD~3`?" while you work, before you commit. The old side is read straight
from git objects and the new side from the working tree (uncommitted and
untracked files included). Everything is type-checked in-process.

```
$ go-whatchanged v1.4.0
example.com/m/store
  - (*Client).Close: removed
  ~ Open: changed
      from func(string) (*Client, error)
        to func(string, Options) (*Client, error)
  + (*Client).Ping: added
  + Options: added

example.com/m/util
  ! Sizer.Size: added

2 packages changed · 3 incompatible · 2 compatible · would require: MAJOR
```

## Install

```
go install github.com/shazow/go-whatchanged/cmd/go-whatchanged@latest
```

Requires Go 1.24 or newer.

## Usage

```
go-whatchanged [flags] <base> [<head>]

  base   commit-ish for the old side: hash, tag, branch, HEAD~2, ...
  head   optional commit-ish for the new side. Default: the working tree.

Flags:
  --repo string      path inside a git repository (default: current directory)
  --goos, --goarch   build target (default: the running platform)
  --breaking         show only incompatible changes
  --color string     auto | always | never (default auto; honors NO_COLOR)
  --strict           type-check errors are fatal (default: warn)

Exit codes: 0 no incompatible changes · 1 incompatible changes · 2 error
```

## Reading the output

Changes are grouped by package, one line per change:

| Glyph | Meaning | Color |
|-------|---------|-------|
| `-`   | symbol removed | red, bold |
| `!`   | incompatible addition (a method on an interface, for example) | red, bold |
| `~`   | incompatible change (signature, type, constant value) | yellow, bold |
| `~`   | compatible change | cyan |
| `+`   | compatible addition | green |

A `changed from X to Y` message is split onto aligned `from` and `to` lines
so that two long signatures can be compared column by column.

A package header carries `(new)` when it exists only on the head side and
`(removed)` when it exists only on the base side. The summary line ends with
the semantic version bump the changes would require: `MAJOR` if anything is
incompatible, `MINOR` if only compatible changes were made, `PATCH` otherwise.
The counts always describe the full diff, even with `--breaking`.

Type-check problems are reported on stderr as `warn: <package>: <error>` and
never abort the diff unless `--strict` is set. Positions on the base side are
prefixed with the revision (`v1.4.0:store/store.go:12:3`); positions in the
working tree are relative to the module root.

## Guarantees

The tool is safe to run at any time, in any state of your checkout:

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
between the two sides since their directories are immutable.

## Non-goals (for now)

JSON output, a stable library API, multi-platform union diffs, `go.work`,
vendor mode, GOPATH mode, cgo-only APIs, module graph resolution beyond
`go.mod`, proxy fetching, and comparing the APIs of dependencies.

## License

MIT, see [LICENSE](LICENSE).

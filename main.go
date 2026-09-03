// Command go-whatchanged prints a colorized diff of a Go module's exported API
// between a commit and the working tree. With no arguments it compares HEAD
// against the current, possibly uncommitted, state of the checkout.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strings"

	"golang.org/x/term"

	"github.com/shazow/go-whatchanged/internal/whatchanged"
)

const usage = `usage: go-whatchanged [flags] [<base> [<head>]]

Show how the exported API of the Go module differs between <base> and <head>.
With no arguments, compare HEAD against the working tree: what your
uncommitted changes do to the API.

  base   optional commit-ish for the old side (hash, tag, branch, HEAD~2, ...),
         or @latest for the newest release tag (v1.2.3) among the ancestors
         of the head commit; default: HEAD
  head   optional commit-ish for the new side; default: the working tree,
         including uncommitted and untracked files

When the base is a release tag, the summary also names the version the
changes call for: "would require: MINOR (v1.4.0 → v1.5.0)".

--pkg and --exclude take import paths or module-relative paths, with "..."
matching anything: "store/..." is the store package and everything below
it. Both may be repeated or given comma-separated lists.

The tool never writes to disk and never runs the go command.

Exit codes: 0 no incompatible changes · 1 incompatible changes · 2 error

With --exit-fail=LEVEL the exit code names the semantic version bump the
changes would require, when it is LEVEL or higher:
  100 major · 101 minor · 102 patch
(--exit-fail=patch is therefore always non-zero, unless there is an error.)

Flags:
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("go-whatchanged", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		fs.PrintDefaults()
	}
	var opts whatchanged.Options
	var color, exitFail, format, signatures, filter string
	var showVersion bool
	fs.StringVar(&opts.Repo, "repo", "", "path inside a git repository (default: current directory)")
	fs.StringVar(&opts.GOOS, "goos", runtime.GOOS, "build target OS")
	fs.StringVar(&opts.GOARCH, "goarch", runtime.GOARCH, "build target architecture")
	fs.BoolVar(&opts.Breaking, "breaking", false, "show only incompatible changes")
	fs.Var((*patterns)(&opts.Packages), "pkg", "diff only packages matching `pattern` (repeatable)")
	fs.Var((*patterns)(&opts.Exclude), "exclude", "skip packages matching `pattern` (repeatable)")
	fs.StringVar(&filter, "filter", "public", "packages to diff: public, internal or all (internal ones never count in the summary or exit code)")
	fs.BoolVar(&opts.Positions, "pos", false, "annotate each change with its source position")
	fs.StringVar(&signatures, "signatures", "full", "show declarations under each change: full or minimal")
	fs.StringVar(&color, "color", "auto", "colorize output: auto, always or never (auto honors NO_COLOR)")
	fs.BoolVar(&opts.Strict, "strict", false, "treat type-check errors as fatal")
	fs.StringVar(&exitFail, "exit-fail", "", "exit 100/101/102 when the required bump is major, minor or patch, or higher")
	fs.StringVar(&format, "format", "text", "output layout: text, markdown or json")
	fs.BoolVar(&showVersion, "version", false, "print the version of go-whatchanged and exit")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return whatchanged.ExitClean
		}
		return whatchanged.ExitError
	}
	if showVersion {
		fmt.Println(version())
		return whatchanged.ExitClean
	}

	switch color {
	case "always":
		opts.Color = true
	case "never":
		opts.Color = false
	case "auto":
		opts.Color = autoColor()
	default:
		fmt.Fprintf(os.Stderr, "go-whatchanged: invalid --color value %q (want auto, always or never)\n", color)
		return whatchanged.ExitError
	}

	if exitFail != "" {
		fail, err := whatchanged.ParseFailOn(exitFail)
		if err != nil {
			fmt.Fprintf(os.Stderr, "go-whatchanged: --exit-fail: %v\n", err)
			return whatchanged.ExitError
		}
		opts.ExitFail = fail
	}

	f, err := whatchanged.ParseFormat(format)
	if err != nil {
		fmt.Fprintf(os.Stderr, "go-whatchanged: --format: %v\n", err)
		return whatchanged.ExitError
	}
	opts.Format = f

	sig, err := whatchanged.ParseSignatures(signatures)
	if err != nil {
		fmt.Fprintf(os.Stderr, "go-whatchanged: --signatures: %v\n", err)
		return whatchanged.ExitError
	}
	opts.Signatures = sig

	vis, err := whatchanged.ParseFilter(filter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "go-whatchanged: --filter: %v\n", err)
		return whatchanged.ExitError
	}
	opts.Filter = vis

	switch fs.NArg() {
	case 0:
		// Base defaults to HEAD inside whatchanged.Run.
	case 1:
		opts.Base = fs.Arg(0)
	case 2:
		opts.Base = fs.Arg(0)
		opts.Head = fs.Arg(1)
	default:
		fs.Usage()
		return whatchanged.ExitError
	}

	opts.Stdout = os.Stdout
	opts.Stderr = os.Stderr
	code, err := whatchanged.Run(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "go-whatchanged: %v\n", err)
	}
	return code
}

// patterns collects a repeatable, comma-separated pattern flag.
type patterns []string

func (p *patterns) String() string { return strings.Join(*p, ",") }

func (p *patterns) Set(s string) error {
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			*p = append(*p, v)
		}
	}
	return nil
}

// version describes this build: the module version go install recorded (or
// the VCS-derived version of a go build), and the Go release it was built
// with, which caps the language version it can type-check.
func version() string {
	v := "(devel)"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		v = info.Main.Version
	}
	return fmt.Sprintf("go-whatchanged %s (built with %s)", v, runtime.Version())
}

func autoColor() bool {
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

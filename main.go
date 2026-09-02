// Command go-whatchanged prints a colorized diff of a Go module's exported API
// between a commit and the working tree.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/term"

	"github.com/shazow/go-whatchanged/internal/whatchanged"
)

const usage = `usage: go-whatchanged [flags] <base> [<head>]

Show how the exported API of the Go module differs between <base> and <head>.

  base   commit-ish for the old side (hash, tag, branch, HEAD~2, ...)
  head   optional commit-ish for the new side; default: the working tree,
         including uncommitted and untracked files

The tool never writes to disk and never runs the go command.

Exit codes: 0 no incompatible changes · 1 incompatible changes · 2 error

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
	var color string
	fs.StringVar(&opts.Repo, "repo", "", "path inside a git repository (default: current directory)")
	fs.StringVar(&opts.GOOS, "goos", runtime.GOOS, "build target OS")
	fs.StringVar(&opts.GOARCH, "goarch", runtime.GOARCH, "build target architecture")
	fs.BoolVar(&opts.Breaking, "breaking", false, "show only incompatible changes")
	fs.StringVar(&color, "color", "auto", "colorize output: auto, always or never (auto honors NO_COLOR)")
	fs.BoolVar(&opts.Strict, "strict", false, "treat type-check errors as fatal")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return whatchanged.ExitClean
		}
		return whatchanged.ExitError
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

	switch fs.NArg() {
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

func autoColor() bool {
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

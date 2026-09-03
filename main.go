// Command go-whatchanged prints a semantic diff of a Go module's exported API
// between two git revisions, or a revision and the working tree, as colorized
// text, Markdown or JSON. With no arguments it compares HEAD against the
// current, possibly uncommitted, state of the checkout.
package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/jessevdk/go-flags"
	"golang.org/x/term"

	"github.com/shazow/go-whatchanged/internal/render"
	"github.com/shazow/go-whatchanged/internal/whatchanged"
)

const description = `Show how the exported API of the Go module differs between <base> and <head>.
With no arguments, compare HEAD against the working tree: what your
uncommitted changes do to the API.

When the base is a release tag, the summary also names the version the
changes call for: "would require: MINOR (v1.4.0 → v1.5.0)".

--pkg and --exclude take import paths or module-relative paths, with "..."
matching anything: "store/..." is the store package and everything below
it. Both may be repeated or given comma-separated lists.

--filter=all, the default, lists the public API first and the internal
packages after it; internal packages never count towards the summary, the
required release or the exit code. --filter=breaking narrows the diff to
incompatible changes and combines with the others: --filter=public,breaking.

GOOS and GOARCH in the environment select the build target, as for the go
command; the default is the running platform.

The tool never writes to disk and never runs the go command.

Exit codes: 0 no incompatible changes · 1 incompatible changes · 2 error

With --exit-fail=LEVEL the exit code names the semantic version bump the
changes would require, when it is LEVEL or higher: 100 for major, 101 for
minor and 102 for patch (--exit-fail=patch is therefore always non-zero,
unless there is an error).`

// options is the command line, as go-flags parses it.
type options struct {
	Repo       string   `long:"repo" value-name:"DIR" description:"path inside a git repository (default: current directory)"`
	Pkg        patterns `long:"pkg" value-name:"PATTERN" description:"diff only packages matching PATTERN (example: --pkg store/... --pkg util)"`
	Exclude    patterns `long:"exclude" value-name:"PATTERN" description:"skip packages matching PATTERN (example: --exclude cmd/...,experimental)"`
	Filter     filter   `long:"filter" value-name:"WHICH" default:"all" description:"packages to diff: all, public or internal, and breaking to show only incompatible changes; comma-separated or repeatable (example: --filter public,breaking)"`
	Signatures string   `long:"signatures" choice:"full" choice:"minimal" default:"full" description:"show each change as its declarations (full) or as one message line (minimal)"`
	Pos        bool     `long:"pos" description:"annotate each change with its source position"`
	Format     string   `long:"format" choice:"text" choice:"markdown" choice:"md" choice:"json" default:"text" description:"output type"`
	Color      string   `long:"color" choice:"auto" choice:"always" choice:"never" default:"auto" description:"colorize output (auto honors NO_COLOR)"`
	Strict     bool     `long:"strict" description:"treat type-check errors as fatal"`
	ExitFail   string   `long:"exit-fail" choice:"major" choice:"minor" choice:"patch" description:"exit 100/101/102 when the required bump is major, minor or patch, or higher"`
	Version    bool     `long:"version" description:"print the version of go-whatchanged and exit"`

	Args struct {
		Base string `positional-arg-name:"base" description:"commit-ish for the old side (hash, tag, branch, HEAD~2, ...), or @latest for the newest release tag (v1.2.3) among the ancestors of the head commit (default: HEAD)"`
		Head string `positional-arg-name:"head" description:"commit-ish for the new side (default: the working tree, including uncommitted and untracked files)"`
	} `positional-args:"yes"`
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	o, err := parseArgs(args)
	if err != nil {
		var ferr *flags.Error
		if errors.As(err, &ferr) && ferr.Type == flags.ErrHelp {
			fmt.Fprintln(os.Stdout, ferr.Message)
			return whatchanged.ExitClean
		}
		fmt.Fprintf(os.Stderr, "go-whatchanged: %v\n", err)
		return whatchanged.ExitError
	}
	if o.Version {
		fmt.Println(version())
		return whatchanged.ExitClean
	}
	opts, err := o.whatchanged()
	if err != nil {
		fmt.Fprintf(os.Stderr, "go-whatchanged: %v\n", err)
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

// parseArgs parses the command line. A --help request comes back as a
// flags.Error of type flags.ErrHelp whose message is the help text.
func parseArgs(args []string) (*options, error) {
	o := &options{}
	// PrintErrors is deliberately left out: run is the single place parse
	// errors get printed, so they never appear twice.
	p := flags.NewParser(o, flags.HelpFlag|flags.PassDoubleDash)
	p.Name = "go-whatchanged"
	p.Usage = "[OPTIONS]"
	p.LongDescription = description
	rest, err := p.ParseArgs(args)
	if err != nil {
		return nil, err
	}
	if len(rest) > 0 {
		return nil, fmt.Errorf("too many arguments: %s (want [<base> [<head>]])", strings.Join(rest, " "))
	}
	return o, nil
}

// whatchanged converts the parsed command line into run options.
func (o *options) whatchanged() (whatchanged.Options, error) {
	opts := whatchanged.Options{
		Repo:      o.Repo,
		GOOS:      os.Getenv("GOOS"),
		GOARCH:    os.Getenv("GOARCH"),
		Packages:  o.Pkg,
		Exclude:   o.Exclude,
		Filter:    o.Filter.visibility(),
		Breaking:  o.Filter.breaking(),
		Positions: o.Pos,
		Strict:    o.Strict,
		Base:      o.Args.Base,
		Head:      o.Args.Head,
	}
	switch o.Color {
	case "always":
		opts.Color = true
	case "never":
		opts.Color = false
	default:
		opts.Color = autoColor()
	}
	if o.ExitFail != "" {
		fail, err := whatchanged.ParseFailOn(o.ExitFail)
		if err != nil {
			return opts, fmt.Errorf("--exit-fail: %w", err)
		}
		opts.ExitFail = fail
	}
	var err error
	if opts.Format, err = whatchanged.ParseFormat(o.Format); err != nil {
		return opts, fmt.Errorf("--format: %w", err)
	}
	if opts.Signatures, err = whatchanged.ParseSignatures(o.Signatures); err != nil {
		return opts, fmt.Errorf("--signatures: %w", err)
	}
	return opts, nil
}

// filter collects the terms of a repeatable, comma-separated --filter flag:
// "all", "public" or "internal" say which packages take part (the last
// two add up to all) and "breaking" narrows the diff to incompatible
// changes. It is a slice so that go-flags drops the default when the flag
// is given.
type filter []string

// UnmarshalFlag adds the terms of one flag occurrence, implementing
// flags.Unmarshaler.
func (f *filter) UnmarshalFlag(s string) error {
	for term := range strings.SplitSeq(s, ",") {
		term = strings.TrimSpace(term)
		switch term {
		case "":
			continue
		case "all", "public", "internal", "breaking":
			*f = append(*f, term)
		default:
			return fmt.Errorf("invalid filter %q (want all, public, internal or breaking)", term)
		}
	}
	return nil
}

// MarshalFlag implements flags.Marshaler.
func (f filter) MarshalFlag() (string, error) { return strings.Join(f, ","), nil }

// visibility returns the packages the terms select: all unless only public
// or only internal was named.
func (f filter) visibility() render.Visibility {
	public, internal := slices.Contains(f, "public"), slices.Contains(f, "internal")
	switch {
	case slices.Contains(f, "all"), public == internal:
		return render.All
	case public:
		return render.Public
	default:
		return render.Internal
	}
}

// breaking reports whether only incompatible changes are wanted.
func (f filter) breaking() bool { return slices.Contains(f, "breaking") }

// patterns collects a repeatable, comma-separated pattern flag.
type patterns []string

// UnmarshalFlag adds the patterns of one flag occurrence, implementing
// flags.Unmarshaler.
func (p *patterns) UnmarshalFlag(s string) error {
	for v := range strings.SplitSeq(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			*p = append(*p, v)
		}
	}
	return nil
}

// MarshalFlag implements flags.Marshaler.
func (p patterns) MarshalFlag() (string, error) { return strings.Join(p, ","), nil }

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

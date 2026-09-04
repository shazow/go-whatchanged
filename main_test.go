package main

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/shazow/go-whatchanged/internal/modfetch"
	"github.com/shazow/go-whatchanged/internal/render"
	"github.com/shazow/go-whatchanged/internal/whatchanged"
)

func TestPatterns(t *testing.T) {
	var p patterns
	// Repeated flags and comma-separated lists add up; blanks, such as the
	// trailing comma the action's newline-to-comma translation leaves, are
	// dropped.
	for _, s := range []string{"store/..., util", "", "a,"} {
		if err := p.UnmarshalFlag(s); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"store/...", "util", "a"}; !slices.Equal([]string(p), want) {
		t.Errorf("patterns = %q, want %q", []string(p), want)
	}
	if got, _ := p.MarshalFlag(); got != "store/...,util,a" {
		t.Errorf("MarshalFlag() = %q, want %q", got, "store/...,util,a")
	}
}

// TestWithFlagHint checks that what a read-only run could not do is
// reported with the flag to drop, and nothing else is touched.
func TestWithFlagHint(t *testing.T) {
	err := fmt.Errorf("example.org/m@latest: %w", modfetch.ErrReadOnly)
	if got := withFlagHint(err); !errors.Is(got, modfetch.ErrReadOnly) || !strings.HasSuffix(got.Error(), "; remove --fsreadonly to let go-whatchanged run it") {
		t.Errorf("withFlagHint(%v) = %v", err, got)
	}
	other := errors.New("other")
	if got := withFlagHint(other); got != other {
		t.Errorf("withFlagHint(%v) = %v", other, got)
	}
}

func TestParseArgs(t *testing.T) {
	// The defaults: everything, in full, as text, HEAD against the working tree.
	o, err := parseArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	opts, err := o.whatchanged()
	if err != nil {
		t.Fatal(err)
	}
	if opts.Filter != render.All || opts.Format != render.Text || opts.Base != "" || opts.Head != "" {
		t.Errorf("defaults = %+v", opts)
	}
	// Missing modules are downloaded with the go command unless
	// --fsreadonly forbids it.
	if _, ok := opts.Fetch.(*modfetch.GoCommand); !ok {
		t.Errorf("default Fetch = %T, want *modfetch.GoCommand", opts.Fetch)
	}
	if o, err := parseArgs([]string{"--fsreadonly"}); err != nil {
		t.Fatal(err)
	} else if opts, err := o.whatchanged(); err != nil {
		t.Fatal(err)
	} else if opts.Fetch != nil {
		t.Errorf("--fsreadonly: Fetch = %T, want nil", opts.Fetch)
	}

	o, err = parseArgs([]string{"--filter=internal", "--pkg", "store/...,util", "--pkg=a", "--exclude", "b", "--format", "md", "--exit-fail=minor", "--filter", "breaking", "--pos", "--filter=api", "--color=never", "v1.4.0", "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	opts, err = o.whatchanged()
	if err != nil {
		t.Fatal(err)
	}
	want := whatchanged.Options{
		Base: "v1.4.0", Head: "HEAD",
		Packages: []string{"store/...", "util", "a"}, Exclude: []string{"b"},
		Filter: render.Internal, Format: render.Markdown,
		ExitFail: whatchanged.FailMinor, Breaking: true, Positions: true, Kinds: render.API,
		Fetch: &modfetch.GoCommand{Stderr: os.Stderr},
	}
	if !reflect.DeepEqual(opts, want) {
		t.Errorf("opts = %+v\nwant   %+v", opts, want)
	}

	// --filter terms add up across repeats and comma-separated lists, and
	// giving the flag drops the default.
	for _, tc := range []struct {
		args     []string
		packages render.Visibility
		kinds    render.Kinds
		breaking bool
	}{
		{nil, render.All, render.AllKinds, false},
		{[]string{"--filter=public"}, render.Public, render.AllKinds, false},
		{[]string{"--filter=breaking"}, render.All, render.AllKinds, true},
		{[]string{"--filter=public,breaking"}, render.Public, render.AllKinds, true},
		{[]string{"--filter", "internal", "--filter", "breaking"}, render.Internal, render.AllKinds, true},
		{[]string{"--filter=public,internal"}, render.Public | render.Internal, render.AllKinds, false},
		{[]string{"--filter=public,internal,main"}, render.All, render.AllKinds, false},
		{[]string{"--filter=main"}, render.Main, render.AllKinds, false},
		{[]string{"--filter=public,main,breaking"}, render.Public | render.Main, render.AllKinds, true},
		{[]string{"--filter=public", "--filter=all"}, render.All, render.AllKinds, false},
		// The kinds of change are a dimension of their own: naming one
		// leaves the packages alone, and both add up to all of them.
		{[]string{"--filter=imports"}, render.All, render.Imports, false},
		{[]string{"--filter=api"}, render.All, render.API, false},
		{[]string{"--filter=public,imports"}, render.Public, render.Imports, false},
		{[]string{"--filter=api,imports"}, render.All, render.AllKinds, false},
		{[]string{"--filter", "api", "--filter", "breaking"}, render.All, render.API, true},
		{[]string{"--filter=imports", "--filter=all"}, render.All, render.AllKinds, false},
	} {
		o, err := parseArgs(tc.args)
		if err != nil {
			t.Errorf("parseArgs(%q): %v", tc.args, err)
			continue
		}
		if o.Filter.visibility() != tc.packages || o.Filter.kinds() != tc.kinds || o.Filter.breaking() != tc.breaking {
			t.Errorf("parseArgs(%q): filter = %v, kinds = %v, breaking = %v; want %v, %v, %v", tc.args, o.Filter.visibility(), o.Filter.kinds(), o.Filter.breaking(), tc.packages, tc.kinds, tc.breaking)
		}
	}

	// Bad values and extra arguments are errors; --help is not.
	for _, args := range [][]string{{"--filter=private"}, {"--format", "yaml"}, {"-filter", "all"}, {"--breaking"}, {"--goos=linux"}, {"--repo=."}, {"a", "b", "c"}, {"--bogus"}} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs(%q) succeeded", args)
		}
	}
	_, err = parseArgs([]string{"--help"})
	if err == nil || !strings.Contains(err.Error(), "--filter=WHICH") {
		t.Errorf("--help: %v", err)
	}
}

package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"

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
	if opts.Filter != render.All || opts.Signatures != render.FullSignatures || opts.Format != render.Text || opts.Base != "" || opts.Head != "" {
		t.Errorf("defaults = %+v", opts)
	}

	o, err = parseArgs([]string{"--filter=internal", "--pkg", "store/...,util", "--pkg=a", "--exclude", "b", "--format", "md", "--signatures=minimal", "--exit-fail=minor", "--breaking", "--pos", "--color=never", "v1.4.0", "HEAD"})
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
		Filter: render.Internal, Format: render.Markdown, Signatures: render.MinimalSignatures,
		ExitFail: whatchanged.FailMinor, Breaking: true, Positions: true,
	}
	if !reflect.DeepEqual(opts, want) {
		t.Errorf("opts = %+v\nwant   %+v", opts, want)
	}

	// Bad values and extra arguments are errors; --help is not.
	for _, args := range [][]string{{"--filter=private"}, {"--format", "yaml"}, {"-filter", "all"}, {"a", "b", "c"}, {"--bogus"}} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs(%q) succeeded", args)
		}
	}
	_, err = parseArgs([]string{"--help"})
	if err == nil || !strings.Contains(err.Error(), "--filter=[all|public|internal]") {
		t.Errorf("--help: %v", err)
	}
}

package main

import (
	"slices"
	"testing"
)

func TestPatterns(t *testing.T) {
	var p patterns
	// Repeated flags and comma-separated lists add up; blanks, such as the
	// trailing comma the action's newline-to-comma translation leaves, are
	// dropped.
	for _, s := range []string{"store/..., util", "", "a,"} {
		if err := p.Set(s); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"store/...", "util", "a"}; !slices.Equal([]string(p), want) {
		t.Errorf("patterns = %q, want %q", []string(p), want)
	}
	if got, want := p.String(), "store/...,util,a"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

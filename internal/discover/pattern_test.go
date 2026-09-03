package discover

import (
	"slices"
	"testing"
)

func TestPatternMatch(t *testing.T) {
	const mod = "example.com/m"
	tests := []struct {
		pattern, importPath string
		want                bool
	}{
		{"...", "example.com/m", true},
		{"...", "example.com/m/store", true},
		{"./...", "example.com/m/store/sub", true},
		{".", "example.com/m", true},
		{".", "example.com/m/store", false},
		{"store", "example.com/m/store", true},
		{"./store", "example.com/m/store", true},
		{"store/", "example.com/m/store", true},
		{"store", "example.com/m/store/sub", false},
		{"store", "example.com/m/storefront", false},
		{"store/...", "example.com/m/store", true},
		{"store/...", "example.com/m/store/sub/deep", true},
		{"store/...", "example.com/m/storefront", false},
		{"example.com/m/store", "example.com/m/store", true},
		{"example.com/m/store/...", "example.com/m/store/sub", true},
		{"example.com/other/...", "example.com/m/store", false},
		{".../sub", "example.com/m/store/sub", true},
		{"st...e", "example.com/m/store", true},
		{"st...e", "example.com/m/stable/x", false},
		{"s.ore", "example.com/m/store", false}, // "." is literal
	}
	for _, tc := range tests {
		if got := Compile(tc.pattern).Match(mod, tc.importPath); got != tc.want {
			t.Errorf("Compile(%q).Match(%q, %q) = %v, want %v", tc.pattern, mod, tc.importPath, got, tc.want)
		}
	}
}

func TestFilter(t *testing.T) {
	const mod = "example.com/m"
	pkgs := []string{"example.com/m", "example.com/m/store", "example.com/m/store/sub", "example.com/m/util", "example.com/m/x/experimental"}
	tests := []struct {
		include, exclude []string
		want             []string
	}{
		{nil, nil, pkgs},
		{[]string{"store/..."}, nil, []string{"example.com/m/store", "example.com/m/store/sub"}},
		{[]string{"store", "util"}, nil, []string{"example.com/m/store", "example.com/m/util"}},
		{nil, []string{".../experimental"}, pkgs[:4]},
		{[]string{"..."}, []string{"store/..."}, []string{"example.com/m", "example.com/m/util", "example.com/m/x/experimental"}},
		{[]string{"store/..."}, []string{"store"}, []string{"example.com/m/store/sub"}},
	}
	for _, tc := range tests {
		f := NewFilter(tc.include, tc.exclude)
		var got []string
		for _, p := range pkgs {
			if f.Match(mod, p) {
				got = append(got, p)
			}
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("NewFilter(%v, %v): got %v, want %v", tc.include, tc.exclude, got, tc.want)
		}
	}
}

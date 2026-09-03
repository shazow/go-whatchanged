package discover

import "testing"

func TestMatchPattern(t *testing.T) {
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
		if got := MatchPattern(tc.pattern, mod, tc.importPath); got != tc.want {
			t.Errorf("MatchPattern(%q, %q, %q) = %v, want %v", tc.pattern, mod, tc.importPath, got, tc.want)
		}
	}
}

func TestFilter(t *testing.T) {
	const mod = "example.com/m"
	pkgs := []string{"example.com/m", "example.com/m/store", "example.com/m/store/sub", "example.com/m/util", "example.com/m/x/experimental"}
	tests := []struct {
		f    Filter
		want []string
	}{
		{Filter{}, pkgs},
		{Filter{Include: []string{"store/..."}}, []string{"example.com/m/store", "example.com/m/store/sub"}},
		{Filter{Include: []string{"store", "util"}}, []string{"example.com/m/store", "example.com/m/util"}},
		{Filter{Exclude: []string{".../experimental"}}, pkgs[:4]},
		{Filter{Include: []string{"..."}, Exclude: []string{"store/..."}}, []string{"example.com/m", "example.com/m/util", "example.com/m/x/experimental"}},
		{Filter{Include: []string{"store/..."}, Exclude: []string{"store"}}, []string{"example.com/m/store/sub"}},
	}
	for _, tc := range tests {
		var got []string
		for _, p := range pkgs {
			if tc.f.Match(mod, p) {
				got = append(got, p)
			}
		}
		if len(got) != len(tc.want) {
			t.Errorf("%+v: got %v, want %v", tc.f, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%+v: got %v, want %v", tc.f, got, tc.want)
				break
			}
		}
	}
}

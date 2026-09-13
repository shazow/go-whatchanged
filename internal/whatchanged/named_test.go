package whatchanged

import "testing"

// TestGofmtRawTags checks that struct tags, which go/types prints as
// interpreted strings, come out as the raw strings source writes them.
func TestGofmtRawTags(t *testing.T) {
	for _, tc := range []struct {
		decl string
		want string
	}{
		{
			`type T struct{ID string "json:\"id\""}`,
			"type T struct {\n\tID string `json:\"id\"`\n}",
		},
		{
			`type T struct{Addr string "json:\"addr,omitempty\" xml:\"addr\""}`,
			"type T struct {\n\tAddr string `json:\"addr,omitempty\" xml:\"addr\"`\n}",
		},
		{
			// Fields line up on the raw tags, not the escaped ones.
			`type T struct{ID string "json:\"id\""; Attached bool "json:\"attached\""}`,
			"type T struct {\n\tID       string `json:\"id\"`\n\tAttached bool   `json:\"attached\"`\n}",
		},
		{
			// A tag a raw string cannot hold stays quoted.
			"type T struct{ID string \"json:\\\"a`b\\\"\"}",
			"type T struct {\n\tID string \"json:\\\"a`b\\\"\"\n}",
		},
		{
			`type T struct{ID string "json:\"a\nb\""}`,
			"type T struct {\n\tID string \"json:\\\"a\\nb\\\"\"\n}",
		},
		{
			`var _ struct{ID string "json:\"id\""}`,
			"var _ struct {\n\tID string `json:\"id\"`\n}",
		},
		{
			// Nothing to rewrite.
			"type T struct{ Timeout int }",
			"type T struct{ Timeout int }",
		},
		{
			"func Open(path string) (*Client, error)",
			"func Open(path string) (*Client, error)",
		},
		{
			// The parser rejects it; it comes back as it is.
			"type T struct{",
			"type T struct{",
		},
	} {
		if got := gofmt(tc.decl); got != tc.want {
			t.Errorf("gofmt(%q) =\n%q\nwant\n%q", tc.decl, got, tc.want)
		}
	}
}

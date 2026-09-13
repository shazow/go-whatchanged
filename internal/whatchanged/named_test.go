package whatchanged

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

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
			// A tab a raw string can hold does not break the alignment.
			"type T struct{ID string \"json:\\\"a\\tb\\\"\"; N int \"j:\\\"n\\\"\"}",
			"type T struct {\n\tID string `json:\"a\tb\"`\n\tN  int    `j:\"n\"`\n}",
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
			// A tag already backquoted is left alone: the old gofmt,
			// format.Source, preserved it too, and the pipeline never
			// sees one, see TestGofmtRawTagsSource.
			"type T struct{ID string `json:\"id\"`}",
			"type T struct {\n\tID string `json:\"id\"`\n}",
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

// TestGofmtRawTagsSource checks where the backquotes of a tag written in
// source go: the type checker decodes the tag literal (go/types.Checker.tag
// calls strconv.Unquote), so types.Struct.Tag holds the tag alone and the
// quoting form is gone by the time declString runs; types.ObjectString then
// re-encodes it with strconv.Quote. gofmt only ever sees the escaped form,
// and turns it back into the backquotes source wrote.
func TestGofmtRawTagsSource(t *testing.T) {
	const src = "package p\n\ntype T struct {\n\tID string `json:\"id\"`\n}\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := (&types.Config{}).Check("p", fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	obj := pkg.Scope().Lookup("T")
	if got, want := obj.Type().Underlying().(*types.Struct).Tag(0), `json:"id"`; got != want {
		t.Errorf("Struct.Tag(0) = %q, want %q", got, want)
	}
	decl := types.ObjectString(obj, func(*types.Package) string { return "" })
	if want := `type T struct{ID string "json:\"id\""}`; decl != want {
		t.Errorf("ObjectString = %q, want %q", decl, want)
	}
	if got, want := gofmt(decl), "type T struct {\n\tID string `json:\"id\"`\n}"; got != want {
		t.Errorf("gofmt = %q, want %q", got, want)
	}
}

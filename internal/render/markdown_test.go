package render

import (
	"strings"
	"testing"
)

// TestGoBlock covers what the golden fixture lacks: a change without a
// declaration to show, a compatible change, an incompatible addition,
// positions and BreakingOnly.
func TestGoBlock(t *testing.T) {
	t.Parallel()
	pkg := Package{Path: "example.com/m/p", Changes: []Change{
		{Message: "T: no longer implements fmt.Stringer"},
		{Message: "V: changed from func to var", Compatible: true, Before: "func V() int", After: "var V int",
			Pos: Position{File: "p/p.go", Line: 9, Col: 5}},
		{Message: "U: changed from int to int64", Compatible: true},
		{Message: "I.M: added", After: "func (I) M()", Pos: Position{File: "p/p.go", Line: 3, Col: 2}},
		{Message: "F: added", Compatible: true, After: "func F(long string, signature int) error"},
		{Message: "Gone: removed", Before: "func Gone()", Pos: Position{Rev: "v1.0.0", File: "p/p.go", Line: 12, Col: 6}},
	}}
	res := Result{Base: "v1.0.0", Head: "working tree", Packages: []Package{pkg}}
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"plain", Options{Format: Markdown}, "**example.com/m/p**\n\n```go\n" +
			"// Removed\n" +
			"func Gone()\n" +
			"\n// Changed\n" +
			"// T: no longer implements fmt.Stringer\n" +
			"\n" +
			"func V() int // ->\n" +
			"var V int // compatible\n" +
			"\n" +
			"// U: changed from int to int64 · compatible\n" +
			"\n// Added\n" +
			"func (I) M() // incompatible\n" +
			"func F(long string, signature int) error\n" +
			"```\n\n1 package changed · 3 incompatible · 3 compatible · would require: **MAJOR**\n"},
		// The comment column is set by the lines that carry a comment; a
		// longer line without one does not widen it.
		{"positions", Options{Format: Markdown, Positions: true}, "```go\n" +
			"// Removed\n" +
			"func Gone()  // v1.0.0:p/p.go:12:6\n" +
			"\n// Changed\n" +
			"// T: no longer implements fmt.Stringer\n" +
			"\n" +
			"func V() int // ->\n" +
			"var V int    // compatible · p/p.go:9:5\n" +
			"\n" +
			"// U: changed from int to int64 · compatible\n" +
			"\n// Added\n" +
			"func (I) M() // incompatible · p/p.go:3:2\n" +
			"func F(long string, signature int) error\n" +
			"```\n"},
		{"breaking", Options{Format: Markdown, BreakingOnly: true}, "```go\n" +
			"// Removed\n" +
			"func Gone()\n" +
			"\n// Changed\n" +
			"// T: no longer implements fmt.Stringer\n" +
			"\n// Added\n" +
			"func (I) M() // incompatible\n" +
			"```\n"},
	} {
		var b strings.Builder
		if err := Write(&b, res, tc.opts); err != nil {
			t.Fatal(err)
		}
		if got := b.String(); !strings.Contains(got, tc.want) {
			t.Errorf("%s: got\n%s\nwant to contain\n%s", tc.name, got, tc.want)
		}
	}
}

// TestStructFragment covers the fields of one struct across groups: the
// Go block shows one fragment per group, each closed by an elision, with a
// removed field pointing at itself, the text layout one for the struct,
// and the comment column of a fragment is its own.
func TestStructFragment(t *testing.T) {
	t.Parallel()
	pkg := Package{Path: "example.com/m/p", Changes: []Change{
		{Message: "Config.Timeout: changed from int to int64", Before: "Timeout int", After: "Timeout int64", Struct: "Config",
			Pos: Position{File: "p/p.go", Line: 5, Col: 2}},
		{Message: "Config.Retries: changed from int to uint", Before: "Retries int", After: "Retries uint", Struct: "Config"},
		{Message: "Config.Name: removed", Before: "Name string", Struct: "Config"},
		{Message: "Config.Logger: added", Compatible: true, After: "*log.Logger", Struct: "Config",
			Pos: Position{File: "p/p.go", Line: 8, Col: 2}},
		{Message: "F: added", Compatible: true, After: "func F(a, b, c, d, e int) (err error)"},
	}}
	res := Result{Base: "v1.0.0", Head: "working tree", Packages: []Package{pkg}}
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"markdown", Options{Format: Markdown, Positions: true}, "```go\n" +
			"// Removed\n" +
			"type Config struct {\n" +
			"\tName string   // <-\n" +
			"\t// ...\n" +
			"}\n" +
			"\n// Changed\n" +
			"type Config struct {\n" +
			"\tTimeout int   // ->\n" +
			"\tTimeout int64 // p/p.go:5:2\n" +
			"\n" +
			"\tRetries int   // ->\n" +
			"\tRetries uint\n" +
			"\t// ...\n" +
			"}\n" +
			"\n// Added\n" +
			"type Config struct {\n" +
			"\t*log.Logger   // p/p.go:8:2\n" +
			"\t// ...\n" +
			"}\n" +
			"func F(a, b, c, d, e int) (err error)\n" +
			"```\n"},
		{"text", Options{Positions: true}, "example.com/m/p\n" +
			"  ~ type Config struct {\n" +
			"  -     Timeout int\n" +
			"  +     Timeout int64  p/p.go:5:2\n" +
			"  -     Retries int\n" +
			"  +     Retries uint\n" +
			"  -     Name string\n" +
			"  +     *log.Logger    p/p.go:8:2\n" +
			"        // ...\n" +
			"    }\n" +
			"  + func F(a, b, c, d, e int) (err error)\n" +
			"\n1 package changed · 3 incompatible · 2 compatible · would require: MAJOR\n"},
	} {
		var b strings.Builder
		if err := Write(&b, res, tc.opts); err != nil {
			t.Fatal(err)
		}
		if got := b.String(); !strings.Contains(got, tc.want) {
			t.Errorf("%s: got\n%s\nwant to contain\n%s", tc.name, got, tc.want)
		}
	}
}

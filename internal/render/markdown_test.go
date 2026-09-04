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

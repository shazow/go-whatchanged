package render

import "testing"

func TestSplitFromTo(t *testing.T) {
	tests := []struct {
		s, from, to string
		ok          bool
	}{
		{"func(string) error to func(string, int) error", "func(string) error", "func(string, int) error", true},
		{"1 to 2", "1", "2", true},
		{"func(func(from string, to string)) to func(func(from string, to string)) error",
			"func(func(from string, to string))", "func(func(from string, to string)) error", true},
		{"struct{to int} to struct{to int64}", "struct{to int}", "struct{to int64}", true},
		{`struct{A int "json:\"a to b\""} to struct{A int}`, `struct{A int "json:\"a to b\""}`, "struct{A int}", true},
		{"func(a to b) to int", "func(a to b)", "int", true},
		{"func(a to func(b) to int", "func(a", "func(b) to int", true}, // never complete: the first " to "
		{`"aaaa... to "aaab...`, `"aaaa...`, `"aaab...`, true},         // an abbreviated string value
		{"int", "", "", false},
		{" to int", "", "", false},
		{"int to ", "", "", false},
	}
	for _, tc := range tests {
		from, to, ok := splitFromTo(tc.s)
		if from != tc.from || to != tc.to || ok != tc.ok {
			t.Errorf("splitFromTo(%q) = %q, %q, %v; want %q, %q, %v", tc.s, from, to, ok, tc.from, tc.to, tc.ok)
		}
	}
}

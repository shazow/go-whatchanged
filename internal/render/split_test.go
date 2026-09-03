package render

import "testing"

func TestSplitChangedFromTo(t *testing.T) {
	tests := []struct {
		msg            string
		head, from, to string
		ok             bool
	}{
		{"Open: changed from func(string) error to func(string, int) error", "Open: changed", "func(string) error", "func(string, int) error", true},
		{"Version: value changed from 1 to 2", "Version: value changed", "1", "2", true},
		{"M: changed from func(func(from string, to string)) to func(func(from string, to string)) error",
			"M: changed", "func(func(from string, to string))", "func(func(from string, to string)) error", true},
		{"F: changed from struct{to int} to struct{to int64}", "F: changed", "struct{to int}", "struct{to int64}", true},
		{`F: changed from struct{A int "json:\"a to b\""} to struct{A int}`, "F: changed", `struct{A int "json:\"a to b\""}`, "struct{A int}", true},
		{"F: changed from func(a to b) to int", "F: changed", "func(a to b)", "int", true},
		{"F: changed from func(a to func(b) to int", "", "", "", false}, // never balanced
		{`F: changed from "a to b`, "", "", "", false},                  // unterminated literal
		{"T: no longer implements fmt.Stringer", "", "", "", false},
		{"F: changed from  to int", "", "", "", false},
	}
	for _, tc := range tests {
		head, from, to, ok := splitChangedFromTo(tc.msg)
		if head != tc.head || from != tc.from || to != tc.to || ok != tc.ok {
			t.Errorf("splitChangedFromTo(%q) = %q, %q, %q, %v; want %q, %q, %q, %v", tc.msg, head, from, to, ok, tc.head, tc.from, tc.to, tc.ok)
		}
	}
}

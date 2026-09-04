package render

import "testing"

func TestParseFormat(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]Format{"text": Text, "markdown": Markdown, "md": Markdown, "JSON": JSON} {
		got, err := ParseFormat(in)
		if err != nil || got != want {
			t.Errorf("ParseFormat(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseFormat("yaml"); err == nil {
		t.Error("ParseFormat accepted yaml")
	}
}

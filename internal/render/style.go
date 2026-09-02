package render

import "strconv"

// ANSI SGR codes.
const (
	codeReset  = 0
	codeBold   = 1
	codeDim    = 2
	codeRed    = 31
	codeGreen  = 32
	codeYellow = 33
	codeCyan   = 36
)

// Style wraps text in ANSI escapes when enabled and is a no-op otherwise.
type Style struct {
	Enabled bool
}

func (s Style) wrap(text string, codes ...int) string {
	if !s.Enabled || len(codes) == 0 || text == "" {
		return text
	}
	seq := "\x1b["
	for i, c := range codes {
		if i > 0 {
			seq += ";"
		}
		seq += strconv.Itoa(c)
	}
	return seq + "m" + text + "\x1b[" + strconv.Itoa(codeReset) + "m"
}

// Bold renders text in bold.
func (s Style) Bold(text string) string { return s.wrap(text, codeBold) }

// Dim renders text dimmed.
func (s Style) Dim(text string) string { return s.wrap(text, codeDim) }

// Red renders text in red, bold when bold is set.
func (s Style) Red(text string, bold bool) string { return s.color(text, codeRed, bold) }

// Green renders text in green, bold when bold is set.
func (s Style) Green(text string, bold bool) string { return s.color(text, codeGreen, bold) }

// Yellow renders text in yellow, bold when bold is set.
func (s Style) Yellow(text string, bold bool) string { return s.color(text, codeYellow, bold) }

// Cyan renders text in cyan, bold when bold is set.
func (s Style) Cyan(text string, bold bool) string { return s.color(text, codeCyan, bold) }

// DimYellow renders text in dim yellow.
func (s Style) DimYellow(text string) string { return s.wrap(text, codeDim, codeYellow) }

func (s Style) color(text string, code int, bold bool) string {
	if bold {
		return s.wrap(text, codeBold, code)
	}
	return s.wrap(text, code)
}

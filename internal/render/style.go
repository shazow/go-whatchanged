package render

import (
	"strconv"
	"strings"
)

// ANSI SGR codes.
const (
	codeReset  = 0
	codeBold   = 1
	codeDim    = 2
	codeRed    = 31
	codeGreen  = 32
	codeYellow = 33
	codeCyan   = 36
	// codeFG256 selects a foreground from the 256-color palette; it is
	// followed by 5 and the palette index.
	codeFG256 = 38
)

// Palette indexes for shades the 16 basic colors lack: 242 is a mid grey
// that reads on dark and light backgrounds alike, 208 is orange.
const (
	paletteGrey   = 242
	paletteOrange = 208
)

// Style wraps text in ANSI escapes when enabled and is a no-op otherwise.
type Style struct {
	Enabled bool
}

func (s Style) wrap(text string, codes ...int) string {
	if !s.Enabled || len(codes) == 0 || text == "" {
		return text
	}
	var b strings.Builder
	b.WriteString("\x1b[")
	for i, c := range codes {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(strconv.Itoa(c))
	}
	b.WriteString("m")
	b.WriteString(text)
	b.WriteString("\x1b[" + strconv.Itoa(codeReset) + "m")
	return b.String()
}

// Bold renders text in bold.
func (s Style) Bold(text string) string { return s.wrap(text, codeBold) }

// Dim renders text dimmed.
func (s Style) Dim(text string) string { return s.wrap(text, codeDim) }

// Red renders text in red, bold when bold is set.
func (s Style) Red(text string, bold bool) string { return s.color(text, bold, codeRed) }

// Green renders text in green, bold when bold is set.
func (s Style) Green(text string, bold bool) string { return s.color(text, bold, codeGreen) }

// Yellow renders text in yellow, bold when bold is set.
func (s Style) Yellow(text string, bold bool) string { return s.color(text, bold, codeYellow) }

// Cyan renders text in cyan, bold when bold is set.
func (s Style) Cyan(text string, bold bool) string { return s.color(text, bold, codeCyan) }

// DimYellow renders text in dim yellow.
func (s Style) DimYellow(text string) string { return s.wrap(text, codeDim, codeYellow) }

// Grey renders text greyed out, for the old declaration of a changed symbol.
func (s Style) Grey(text string) string { return s.wrap(text, codeFG256, 5, paletteGrey) }

// Orange renders text in orange, bold when bold is set, for the new
// declaration of a changed symbol.
func (s Style) Orange(text string, bold bool) string {
	return s.color(text, bold, codeFG256, 5, paletteOrange)
}

// color renders text in the color codes select, bold when bold is set.
func (s Style) color(text string, bold bool, codes ...int) string {
	if bold {
		codes = append([]int{codeBold}, codes...)
	}
	return s.wrap(text, codes...)
}

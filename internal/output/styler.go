package output

import (
	"github.com/charmbracelet/lipgloss"
)

// Styler decorates the text renderer's tokens. Plain output uses identity
// hooks; a terminal gets lipgloss styles. The layout lives in WriteText
// either way — a styler never adds, removes, or reorders characters beyond
// the escape codes themselves.
type Styler struct {
	target    func(string) string
	status    func(string) string
	label     func(string) string
	signature func(string) string
	dim       func(string) string
	warn      func(string) string
}

func plainStyler() *Styler {
	id := func(s string) string { return s }
	return &Styler{
		target:    id,
		status:    id,
		label:     id,
		signature: id,
		dim:       id,
		warn:      id,
	}
}

// NewStyler builds the terminal styler from a lipgloss renderer, which
// carries the color profile detected from (or forced onto) the output.
// Bright ANSI base colors only: they render on light and dark backgrounds
// without an adaptive palette.
// one adapts a variadic lipgloss render to the single-token hook shape.
func one(st lipgloss.Style) func(string) string {
	return func(s string) string { return st.Render(s) }
}

func NewStyler(r *lipgloss.Renderer) *Styler {
	var (
		bold   = r.NewStyle().Bold(true)
		green  = r.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
		red    = r.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
		yellow = r.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)
		faint  = r.NewStyle().Faint(true)
	)
	return &Styler{
		target: one(bold),
		status: func(s string) string {
			switch s {
			case StatusSettled:
				return green.Render(s)
			case StatusFailed, StatusPermissionDenied:
				return red.Render(s)
			default:
				return yellow.Render(s)
			}
		},
		label:     one(faint),
		signature: one(red),
		dim:       one(faint),
		warn:      one(yellow),
	}
}

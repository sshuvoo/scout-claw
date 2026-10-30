package cli

import (
	"strings"

	"github.com/charmbracelet/glamour"
)

// renderMarkdown renders src as styled, wrapped terminal output. On any
// failure it falls back to the raw source so a rendering bug never loses text.
func renderMarkdown(src string, width int) string {
	if width <= 0 {
		width = 80
	}
	r, err := glamour.NewTermRenderer(glamour.WithStandardStyle("dark"), glamour.WithWordWrap(width))
	if err != nil {
		return src
	}
	out, err := r.Render(src)
	if err != nil {
		return src
	}
	return strings.TrimRight(out, "\n") + "\n"
}

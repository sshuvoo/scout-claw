package cli

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"

	"github.com/alecthomas/chroma/v2/quick"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
)

// fencedCodeRe matches a fenced code block and captures its language tag (if
// any) and body, so it can be pulled out and rendered as its own bordered
// card instead of going through glamour's plain code-block styling.
var fencedCodeRe = regexp.MustCompile("(?s)```[ \t]*([a-zA-Z0-9_+-]*)[ \t]*\r?\n(.*?)\r?\n?```")

// renderMarkdown renders src as styled, wrapped terminal output: headings,
// lists, quotes, and emphasis through a glamour style tuned to the app's
// accent color, and fenced code blocks as bordered, syntax-highlighted cards.
// On any failure it falls back to the raw source so a rendering bug never
// loses text.
func renderMarkdown(src string, width int) string {
	if width <= 0 {
		width = 80
	}

	var cards []string
	withPlaceholders := fencedCodeRe.ReplaceAllStringFunc(src, func(m string) string {
		sub := fencedCodeRe.FindStringSubmatch(m)
		cards = append(cards, codeCard(sub[2], sub[1], width))
		return "\x02CODEBLOCK" + strconv.Itoa(len(cards)-1) + "\x02"
	})

	r, err := glamour.NewTermRenderer(glamour.WithStyles(markdownStyle()), glamour.WithWordWrap(width))
	if err != nil {
		return src
	}
	out, err := r.Render(withPlaceholders)
	if err != nil {
		return src
	}
	for i, card := range cards {
		out = strings.Replace(out, "\x02CODEBLOCK"+strconv.Itoa(i)+"\x02", "\n"+card, 1)
	}
	return strings.TrimRight(out, "\n") + "\n"
}

// codeCard syntax-highlights a fenced code block's body and wraps it in a
// rounded, accent-bordered box with a small language caption above it, so
// code reads as a distinct block rather than blending into the prose.
func codeCard(code, lang string, width int) string {
	var buf bytes.Buffer
	if err := quick.Highlight(&buf, strings.TrimRight(code, "\n"), lang, "terminal16m", "catppuccin-mocha"); err != nil {
		buf.Reset()
		buf.WriteString(code)
	}
	boxW := width - 6 // document margin (2) + border (2) + a little breathing room
	if boxW < 20 {
		boxW = 20
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accent).
		Padding(0, 1).
		Width(boxW).
		Render(strings.TrimRight(buf.String(), "\n"))

	label := lang
	if label == "" {
		label = "code"
	}
	caption := lipgloss.NewStyle().Foreground(accent).Render("▍ " + label)
	return indentLines(caption+"\n"+box, "  ")
}

func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }

// markdownStyle is glamour's dark style, retuned to the app's accent color
// and with the literal "##"/"###" markup hidden in favor of bold, colored
// headings so sections read as sections instead of raw markdown source.
func markdownStyle() ansi.StyleConfig {
	s := styles.DarkStyleConfig
	accentHex := string(accent)

	s.H1 = ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{
		Prefix: " ", Suffix: " ", Color: strp("255"), BackgroundColor: strp(accentHex), Bold: boolp(true),
	}}
	s.H2 = ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{
		Prefix: "▎ ", Color: strp(accentHex), Bold: boolp(true), BlockSuffix: "\n",
	}}
	s.H3 = ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{
		Prefix: "· ", Color: strp(accentHex), Bold: boolp(true), BlockSuffix: "\n",
	}}
	s.H4 = ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{
		Color: strp("250"), Bold: boolp(true), Italic: boolp(true), BlockSuffix: "\n",
	}}

	s.Item = ansi.StylePrimitive{BlockPrefix: "", Prefix: "• ", Color: strp(accentHex)}
	s.Enumeration = ansi.StylePrimitive{BlockPrefix: "", Prefix: ". ", Color: strp(accentHex)}

	s.BlockQuote.Color = strp(accentHex)
	s.BlockQuote.IndentToken = strp("▏ ")

	s.Link = ansi.StylePrimitive{Color: strp(accentHex), Underline: boolp(true)}
	s.LinkText = ansi.StylePrimitive{Color: strp(accentHex), Bold: boolp(true)}

	s.HorizontalRule = ansi.StylePrimitive{Color: strp("240"), Format: "\n──────────\n"}

	s.Code = ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{
		Prefix: " ", Suffix: " ", Color: strp("216"), BackgroundColor: strp("236"),
	}}

	return s
}

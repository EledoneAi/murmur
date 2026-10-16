package console

import (
	"image/color"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
)

// theme is every colour the console uses. Blue-grey neutrals carry the structure; one colour, teal, asks for the
// person's attention and is used for nothing else; thorn red marks what is out of tune or failed; the rest of the
// colour on screen belongs to people. The key to press in a label is underlined, bold and a shade brighter than the
// text: it reads in monochrome too, and doesn't pull the eye from the text around it. The console never paints the terminal's background: only the selected row and the
// current scope tab get a tint.
type theme struct {
	name                   string
	fg, dim, faint, bright color.Color // text; secondary text; decorative strokes; agent and people names
	teal, thorn            color.Color // needs you; out of tune or failed
	link, sib              color.Color // links between people; links within one person's agents
	sel, tab               color.Color // the selected row's tint; the current scope tab's
	hotkey                 color.Color // the key to press, inside a label or before a word: never the teal
	run, web, lilac        color.Color // texture: run and web calls; subagents, second checkouts and stacked branches
	people                 [8]color.Color
}

func hex(s string) color.Color { return lipgloss.Color(s) }

func hexes(s ...string) (out [8]color.Color) {
	for i := range out {
		out[i] = hex(s[i])
	}
	return out
}

// The eight people colours stay apart under deuteranopia and protanopia, and none comes near the teal.
var (
	dark = theme{name: "dark", fg: hex("#d3d6e2"), dim: hex("#8b90a9"), faint: hex("#3a3e55"), bright: hex("#eef0f8"),
		teal: hex("#4fd6c4"), thorn: hex("#f47383"), link: hex("#dbe9ff"), sib: hex("#59617f"),
		sel: hex("#1b1e2d"), tab: hex("#262a40"), hotkey: hex("#f7f8ff"), run: hex("#9fb4ff"), web: hex("#9fb4ff"), lilac: hex("#c9a6ff"),
		people: hexes("#7aa7ff", "#b4e06c", "#b99cff", "#ff8fc8", "#e6c46a", "#ffb27a", "#7cc65e", "#d9a6ff")}
	light = theme{name: "light", fg: hex("#2a2d36"), dim: hex("#626777"), faint: hex("#c8cbd6"), bright: hex("#15171e"),
		teal: hex("#08766a"), thorn: hex("#c42d3d"), link: hex("#1d2540"), sib: hex("#a5abbf"),
		sel: hex("#eceef5"), tab: hex("#e6e7ee"), hotkey: hex("#05060a"), run: hex("#3651b8"), web: hex("#3651b8"), lilac: hex("#8a45b8"),
		people: hexes("#2f62d0", "#4d7d10", "#6f45c9", "#c2357f", "#8a6a00", "#b35a12", "#2f7a1c", "#9046b8")}
	// ansi uses only the terminal's own 16 colours, so a person's palette, high contrast included, carries through.
	ansi16 = theme{name: "ansi", fg: hex("7"), dim: hex("8"), faint: hex("8"), bright: hex("15"),
		teal: hex("6"), thorn: hex("1"), link: hex("15"), sib: hex("8"),
		sel: hex("0"), tab: hex("8"), hotkey: hex("15"), run: hex("4"), web: hex("4"), lilac: hex("5"),
		people: hexes("12", "10", "13", "9", "11", "3", "2", "5")}
)

// contrast is a theme with every dimmed tone dropped: secondary text and strokes take the text colour.
func contrast(t theme) theme {
	t.name = "contrast"
	t.dim, t.faint, t.sib = t.fg, t.fg, t.fg
	return t
}

// pickTheme reads MURMUR_THEME (dark, light, ansi or contrast); otherwise the terminal's background decides.
func pickTheme(isDark bool) theme {
	base := light
	if isDark {
		base = dark
	}
	switch strings.ToLower(os.Getenv("MURMUR_THEME")) {
	case "dark":
		return dark
	case "light":
		return light
	case "ansi":
		return ansi16
	case "contrast":
		return contrast(base)
	}
	return base
}

// styles are the theme turned into lipgloss styles, once per theme.
type styles struct {
	t                                    theme
	fg, dim, faint, name, bold, wordmark lipgloss.Style
	teal, tealBold, thorn, link, sib     lipgloss.Style
	hotkey, tab, label, italic           lipgloss.Style
	run, web, lilac                      lipgloss.Style
	clash                                lipgloss.Style // a file both sides of a dissonance change
}

func newStyles(t theme) styles {
	s := lipgloss.NewStyle()
	return styles{t: t,
		fg: s.Foreground(t.fg), dim: s.Foreground(t.dim), faint: s.Foreground(t.faint), name: s.Foreground(t.bright),
		bold: s.Foreground(t.bright).Bold(true), wordmark: s.Foreground(t.bright).Bold(true),
		teal: s.Foreground(t.teal), tealBold: s.Foreground(t.teal).Bold(true), thorn: s.Foreground(t.thorn),
		link: s.Foreground(t.link), sib: s.Foreground(t.sib),
		hotkey: s.Foreground(t.hotkey).Bold(true).Underline(true), tab: s.Foreground(t.dim),
		label: s.Foreground(t.dim).Bold(true), italic: s.Foreground(t.fg).Italic(true),
		run: s.Foreground(t.run), web: s.Foreground(t.web), lilac: s.Foreground(t.lilac),
		// ponytail: no curly or coloured underline: Terminal.app misreads both and paints the rest of the row
		clash: s.Foreground(t.thorn),
	}
}

// person is the style of someone's initial: their colour, bold.
func (s styles) person(i int) lipgloss.Style {
	n := len(s.t.people)
	return lipgloss.NewStyle().Foreground(s.t.people[(i%n+n)%n]).Bold(true)
}

// hot draws a label with the key to press inside it: the letter at i in the hotkey style, the rest in base. A label
// that starts a phrase is capitalised; its key is the letter in lower case.
func (s styles) hot(label string, i int, base lipgloss.Style) string {
	r := []rune(label)
	if i < 0 || i >= len(r) {
		return base.Render(label)
	}
	var out strings.Builder
	if i > 0 {
		out.WriteString(base.Render(string(r[:i])))
	}
	out.WriteString(s.hotkey.Render(string(r[i])))
	if i+1 < len(r) {
		out.WriteString(base.Render(string(r[i+1:])))
	}
	return out.String()
}

// keyWord is a key, then a word for what it does: the footer's way, and a label's when the letter isn't in its word.
func (s styles) keyWord(key, word string) string {
	return s.hotkey.Render(key) + " " + s.dim.Render(word)
}

package console

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

// The vocabulary: a small set of signs, each with one meaning, used on every screen. Every state has a shape, a word
// and a colour, so the console reads the same in monochrome, for colour-blind readers and under NO_COLOR.

// glyphs is the set of signs a terminal can draw. Most terminals draw box lines, blocks and these symbols; a
// terminal or font that can't gets plain ASCII instead of missing-glyph boxes.
type glyphs struct {
	working, needs, waits, idle, shell, gone string // lamps
	read, edit, run, web, subagent, message  string // texture
	fuse, fuseHalf, fuseRest                 string
	inTune, outOfTune, delivered, failed     string
	second, stacked, detached, folder, also  string // places
	h, hDash, v, vDash                       string // links
	tl, bl, tee, bottomTee, cross, leftTee   string
	logoLine, logoOff, sel, ellipsis, arrow  string
	sendArrow, heldArrow                     string
}

var unicodeGlyphs = glyphs{
	working: "●", needs: "◆", waits: "◇", idle: "○", shell: "◌", gone: "⊘",
	read: "·", edit: "▪", run: "›", web: "∿", subagent: "⋔", message: "⇢",
	fuse: "━", fuseHalf: "╸", fuseRest: "─",
	inTune: "≈", outOfTune: "×", delivered: "✓", failed: "×",
	second: "²", stacked: "⟵", detached: "∅", folder: "⌂", also: "⊕",
	h: "─", hDash: "┄", v: "│", vDash: "┆", tl: "╭", bl: "╰", tee: "┬", bottomTee: "┴", cross: "┼", leftTee: "├",
	logoLine: "╭─┬─╮", logoOff: "╭┄┬┄╮", sel: "▌", ellipsis: "…", arrow: "─▸", sendArrow: "─▸", heldArrow: "┄⇢",
}

var asciiGlyphs = glyphs{
	working: "*", needs: "!", waits: "?", idle: "o", shell: ".", gone: "x",
	read: ".", edit: "#", run: ">", web: "~", subagent: "+", message: "=",
	fuse: "=", fuseHalf: "-", fuseRest: ".",
	inTune: "~", outOfTune: "x", delivered: "ok", failed: "x",
	second: "'", stacked: "<-", detached: "@", folder: "~", also: "+",
	h: "-", hDash: ".", v: "|", vDash: ":", tl: ",", bl: "`", tee: "+", bottomTee: "+", cross: "+", leftTee: "+",
	logoLine: ",-+-.", logoOff: ",.+..", sel: ">", ellipsis: "~", arrow: "->", sendArrow: "->", heldArrow: "..>",
}

// pickGlyphs reads MURMUR_GLYPHS (unicode or ascii); otherwise a terminal without UTF-8 gets ASCII.
func pickGlyphs() glyphs {
	switch strings.ToLower(os.Getenv("MURMUR_GLYPHS")) {
	case "ascii":
		return asciiGlyphs
	case "unicode":
		return unicodeGlyphs
	}
	if os.Getenv("TERM") == "dumb" {
		return asciiGlyphs
	}
	for _, v := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if l := os.Getenv(v); l != "" {
			if u := strings.ToUpper(l); strings.Contains(u, "UTF-8") || strings.Contains(u, "UTF8") {
				return unicodeGlyphs
			}
			return asciiGlyphs
		}
	}
	return unicodeGlyphs // macOS terminals often set none of them, and draw UTF-8
}

// state is what an agent is doing, as the console tells it: its lamp, its word, and whether it waits.
type state int

const (
	working    state = iota
	needsYou         // one of your agents waits for you
	waitsOwner       // someone else's agent waits for its person
	idle
	shell
	gone
)

func agentState(a protocol.Agent, mine bool) state {
	switch {
	case a.Needs != "" || a.Status == protocol.StatusWaiting:
		if mine {
			return needsYou
		}
		return waitsOwner
	case a.Status == protocol.StatusBusy:
		return working
	case a.Status == protocol.StatusShell:
		return shell
	}
	return idle
}

func (g glyphs) lamp(s state) string {
	return [...]string{g.working, g.needs, g.waits, g.idle, g.shell, g.gone}[s]
}

func (s state) word() string {
	return [...]string{"working", "needs you", "waits on its owner", "idle", "shell", "gone"}[s]
}

func (s state) waiting() bool { return s == needsYou || s == waitsOwner }

// texture is an agent's last n tool calls, newest on the right, one sign per kind: ··∿··▪ is exploring, ▪▪·▪▪▪ is
// writing, ›››››› is running the same command over and over. Calls missing at the start pad with blanks.
func (st styles) texture(g glyphs, kinds string, n int) string {
	runes := []rune(kinds)
	if len(runes) > n {
		runes = runes[len(runes)-n:]
	}
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", n-len(runes)))
	for _, r := range runes {
		switch r {
		case protocol.KindRead:
			b.WriteString(st.dim.Render(g.read))
		case protocol.KindEdit:
			b.WriteString(st.name.Render(g.edit))
		case protocol.KindRun:
			b.WriteString(st.run.Render(g.run))
		case protocol.KindWeb:
			b.WriteString(st.web.Render(g.web))
		case protocol.KindSubagent:
			b.WriteString(st.lilac.Render(g.subagent))
		case protocol.KindMessage:
			b.WriteString(st.name.Render(g.message))
		default:
			b.WriteString(" ")
		}
	}
	return b.String()
}

// fuseFull is how long a wait takes to burn the whole fuse.
const fuseFull = 10 * time.Minute

// fuse burns along a light line while an agent waits, half a cell at a time, full at ten minutes: teal when it
// waits on you, muted when it waits on someone else. The clock beside it says the same in words.
func (st styles) fuse(g glyphs, waited time.Duration, mine bool, width int) string {
	halves := min(2*width, int((float64(waited)/float64(fuseFull))*float64(2*width)+0.5))
	full, half := halves/2, halves%2
	burnt := strings.Repeat(g.fuse, full)
	if half == 1 {
		burnt += g.fuseHalf
	}
	rest := strings.Repeat(g.fuseRest, width-full-half)
	burn := st.dim
	if mine {
		burn = st.teal
	}
	return burn.Render(burnt) + st.faint.Render(rest)
}

// clock is a wait in minutes and seconds, 3:12, or hours past an hour, 1:05:00.
func clock(d time.Duration) string {
	d = max(d, 0).Truncate(time.Second)
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// ago is how long ago, in a word: now, 4m, 2h, 3d.
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// shares is how an agent spreads its last calls over reading, editing, running and talking, for the inspector.
func shares(kinds string) string {
	if kinds == "" {
		return ""
	}
	n := map[rune]int{}
	for _, r := range kinds {
		n[r]++
	}
	total := len([]rune(kinds))
	var parts []string
	for _, k := range []struct {
		word  string
		kinds []rune
	}{{"read", []rune{protocol.KindRead}}, {"edit", []rune{protocol.KindEdit}}, {"run", []rune{protocol.KindRun}},
		{"web", []rune{protocol.KindWeb}}, {"subagents", []rune{protocol.KindSubagent}}, {"talk", []rune{protocol.KindMessage}}} {
		c := 0
		for _, r := range k.kinds {
			c += n[r]
		}
		if c > 0 {
			parts = append(parts, fmt.Sprintf("%s %d%%", k.word, c*100/total))
		}
	}
	return strings.Join(parts, " · ")
}

// shortBranch fits a branch in w cells by keeping its end, where the name differs: …/sp-ocr.
func shortBranch(b string, w int, g glyphs) string {
	r := []rune(b)
	if len(r) <= w || w < 2 {
		return b
	}
	if i := strings.LastIndex(b, "/"); i >= 0 && len([]rune(b[i:]))+1 <= w {
		return g.ellipsis + b[i:]
	}
	return g.ellipsis + string(r[len(r)-w+1:])
}

// waitReason says why one of your agents waits, from what Claude Code reports: the permission it asks for, or a
// question for you.
func waitReason(a protocol.Agent) string {
	if a.Needs == "permission" {
		if a.Doing != "" {
			return "approve " + strings.ToLower(a.Doing[:1]) + a.Doing[1:]
		}
		return "approve a tool call"
	}
	return "waits for your answer"
}

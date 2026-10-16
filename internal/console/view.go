package console

import (
	"fmt"
	"image/color"
	"path"
	"slices"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// line is one line of the screen, exactly as wide as the screen, and the item it selects, if any. A selected item
// can span two lines: its row, and the detail unfolded under it.
type line struct {
	text string
	key  string
	hot  []string // the keys its labels show, which the footer then leaves out
}

func cellWidth(s string) int { return ansi.StringWidth(s) }

func truncate(s string, w int, tail string) string { return ansi.Truncate(s, w, tail) }

// fit cuts plain text to w cells, with an ellipsis where it cuts.
func fit(s string, w int, g glyphs) string {
	if w <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, g.ellipsis)
}

// pad cuts or fills a styled string to exactly w cells.
func pad(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if n := ansi.StringWidth(s); n < w {
		return s + strings.Repeat(" ", w-n)
	} else if n > w {
		return ansi.Truncate(s, w, "")
	}
	return s
}

// spread puts left and right on one line of w cells, cutting left when they don't fit.
func spread(left, right string, w int) string {
	rw := ansi.StringWidth(right)
	return pad(left, max(0, w-rw)) + right
}

// tinted lays a background under a styled line, through the resets inside it: the selection. on is the code that
// sets the background, from tint.
func tinted(s, on string) string {
	return on + strings.ReplaceAll(s, "\x1b[m", "\x1b[m"+on) + "\x1b[m"
}

// tint is the code that sets a background colour.
func tint(c color.Color) string {
	on, _, _ := strings.Cut(lipgloss.NewStyle().Background(c).Render("x"), "x")
	return on
}

// layout is where the columns of a row go at the screen's width. Columns drop in a fixed order as the screen
// narrows, the title first and then the place; the lamp, the name and the fuse never drop.
type layout struct {
	w       int
	initial int // the owner's initial and a space; 0 alone on the hub
	name    int
	place   int
	title   int
	right   int // where the activity or the fuse starts
}

const rightW = 14 // a fuse, a space and a clock up to 10:00:00

func (m *model) layout(b *board) layout {
	lay := layout{w: m.w}
	if !b.solo {
		for _, in := range b.initials {
			lay.initial = max(lay.initial, cellWidth(in)+1)
		}
	}
	longest := 0
	for _, r := range b.all() {
		longest = max(longest, cellWidth(r.a.Name))
	}
	lay.name = min(max(longest+2, 10), 18)
	fixed := 1 + gutterW + 2 + lay.initial + lay.name + rightW + 2
	if fixed > m.w {
		lay.name = max(6, lay.name-(fixed-m.w))
		fixed = 1 + gutterW + 2 + lay.initial + lay.name + rightW + 2
	}
	switch rest := m.w - fixed; {
	case m.w < 64: // narrow: places and titles drop; the lamp, the name and the fuse stay
	case rest >= 36:
		lay.place = min(24, rest/2)
		lay.title = rest - lay.place
	case rest >= 10:
		lay.place = min(rest, 26)
	}
	lay.right = m.w - rightW - 2
	return lay
}

func (b *board) all() []*agentRef {
	var out []*agentRef
	for _, g := range append(slices.Clone(b.groups), b.elsewhere...) {
		out = append(out, g.members...)
	}
	return append(append(out, b.lone...), b.quiet...)
}

// ---------------------------------------------------------------- header and footer

// header is the m and the wordmark, the scope tabs, you, the counts and the connection. The m is the console's own
// link line: one thought, ╭─┬─╮, with three agents hanging from it. Its middle lamp turns teal when something needs
// you; offline, its line turns dashed and its lamps hollow.
func (m *model) header(b *board) []string {
	st, g := m.st, m.g
	live := m.live()
	lamp := st.fg.Render(g.working)
	middle := lamp
	if len(b.needs) > 0 {
		middle = st.teal.Render(g.working)
	}
	logo, lamps := " "+st.fg.Render(g.logoLine), m.logoLamps(b)
	if !live {
		logo = " " + st.dim.Render(g.logoOff)
	}
	logo, lamps = m.animatedLogo(b, logo, lamps)
	conn := st.fg.Render(g.working) + " " + st.dim.Render("live")
	if !live {
		word := m.snap.State
		if word == "" {
			word = "connecting"
		}
		conn = st.dim.Render(g.idle + " " + word)
	}
	you := "" // your initial in your colour
	if in := b.initials[b.me]; in != "" {
		you = st.person(b.colours[b.me]).Render(in)
	}
	if m.w < 64 { // one row: the m lies down
		right := conn
		if you != "" {
			right = you + " " + right
		}
		if n := len(b.needs); n > 0 {
			right = st.teal.Render(fmt.Sprintf("%s %d", g.needs, n)) + "   " + right
		}
		left := " " + lamp + st.fg.Render(g.h) + middle + st.fg.Render(g.h) + lamp + "  " + st.wordmark.Render("murmur")
		return []string{spread(left, right+" ", m.w)}
	}
	tabs := ""
	for _, sc := range m.tabs(b) {
		if sc == m.currentScope() {
			tabs += tinted(" "+st.hot(scopeNames[sc], scopeHot[sc], st.name)+" ", st.tabBg) + "  "
		} else {
			tabs += " " + st.hot(scopeNames[sc], scopeHot[sc], st.tab) + "   "
		}
	}
	left := lamps + "  " + st.wordmark.Render("murmur") + "   " + tabs
	// you and the counts, as much as fits beside the tabs: the people online drop out first, then the agents, then
	// your handle
	counts := []string{fmt.Sprintf("%d agents", b.agents)}
	if !b.solo {
		counts = []string{fmt.Sprintf("%d online · %d agents", b.people, b.agents), counts[0]}
	}
	var rights []string
	for _, c := range counts {
		if you == "" {
			rights = append(rights, st.dim.Render(c))
		} else {
			rights = append(rights, you+" "+st.fg.Render(b.me)+st.dim.Render(" · "+c))
		}
	}
	if you != "" {
		rights = append(rights, you+" "+st.fg.Render(b.me), you)
	}
	right := ""
	for _, r := range rights {
		if right = r + "   " + conn + "  "; cellWidth(left)+cellWidth(right) <= m.w {
			break
		}
	}
	return []string{pad(logo, m.w), spread(left, right, m.w)}
}

// tabs are the scopes the header offers as tabs: none alone on the hub, nor on a narrow screen.
func (m *model) tabs(b *board) []scope {
	if b.solo || m.w < 64 {
		return nil
	}
	return scopes(len(b.colours))
}

func (m *model) live() bool { return m.snap.State == "online" || m.demo }

// logoLamps are the three agents hanging from the m: the middle one teal when something needs you, all hollow
// while offline.
func (m *model) logoLamps(b *board) string {
	st, g := m.st, m.g
	if !m.live() {
		return " " + st.dim.Render(g.idle+" "+g.idle+" "+g.idle)
	}
	lamp, middle := st.fg.Render(g.working), st.fg.Render(g.working)
	if len(b.needs) > 0 {
		middle = st.teal.Render(g.working)
	}
	return " " + lamp + " " + middle + " " + lamp
}

// footer explains the keys that matter where the focus is, in pairs of a key and a word, leaving out those the
// screen shows already: as many as fit, the first first, and on the right how to see them all.
func (m *model) footer(keys []string, shown map[string]bool) string {
	right := m.st.keyWord("?", "keys") + "  "
	room := m.w - 1 - cellWidth(right) - 2
	var parts []string
	used := 0
	for i := 0; i+1 < len(keys); i += 2 {
		if shown[keys[i]] {
			continue
		}
		part := m.st.keyWord(keys[i], keys[i+1])
		w := cellWidth(part)
		if len(parts) > 0 {
			w += 2
		}
		if used+w > room {
			break
		}
		parts, used = append(parts, part), used+w
	}
	return spread(" "+strings.Join(parts, "  "), right, m.w)
}

// hotIn is the keys the labels on these lines show.
func hotIn(lines []line) map[string]bool {
	shown := map[string]bool{}
	for _, l := range lines {
		for _, k := range l.hot {
			shown[k] = true
		}
	}
	return shown
}

// ---------------------------------------------------------------- the board

func (m *model) homeLines(b *board) (pinned, body []line) {
	lay := m.layout(b)
	st, g := m.st, m.g
	if len(b.needs) > 0 {
		pinned = append(pinned, line{text: pad(" "+st.teal.Render(g.needs)+" "+st.hot("Needs you", 0, st.bold)+"  "+st.dim.Render(fmt.Sprint(len(b.needs))), m.w),
			hot: []string{"n"}})
		shown := b.needs
		if most := max(1, (m.h-8)/2); len(shown) > most {
			shown = shown[:most]
		}
		for _, n := range shown {
			person, _, _ := strings.Cut(n.who, "/")
			pinned = append(pinned, line{text: m.glowing(n.key, m.needLine(b, lay, n), b.colours[person]), key: n.key})
		}
		if more := len(b.needs) - len(shown); more > 0 {
			pinned = append(pinned, line{text: pad("   "+st.dim.Render(fmt.Sprintf("+ %d more: ", more))+st.keyWord("n", "goes through them"), m.w),
				hot: []string{"n"}})
		}
		pinned = append(pinned, line{text: pad("", m.w)})
	}
	blank := line{text: pad("", m.w)}
	groups := slices.Clone(b.groups)
	slices.SortStableFunc(groups, func(x, y *group) int { // pinned constellations first
		return boolRank(m.pinned[y.key]) - boolRank(m.pinned[x.key])
	})
	for i, gr := range groups {
		folded := m.folded[gr.key]
		body = append(body, line{text: m.groupHeader(b, lay, gr, folded), key: "group:" + gr.key})
		if !folded {
			rows, gaps := st.gutter(g, gr.links, len(gr.members))
			for i, r := range gr.members {
				body = append(body, m.agentLines(b, lay, r, rows[i], gaps[i])...)
			}
		}
		if !m.compact || i == len(groups)-1 {
			body = append(body, blank)
		}
	}
	if len(b.lone) > 0 || len(b.dissonant) > 0 {
		right := ""
		if n := len(b.dissonant); n > 0 {
			right = st.thorn.Render(fmt.Sprintf("%s %d out of tune", g.outOfTune, n))
			if m.w < 64 {
				right = st.thorn.Render(fmt.Sprintf("%s %d", g.outOfTune, n))
			}
		}
		title := "Not in touch"
		if len(b.groups) == 0 {
			title = "Agents"
		}
		body = append(body, line{text: spread(" "+st.bold.Render(title), right+"  ", m.w)})
		for _, r := range b.lone {
			body = append(body, m.agentLines(b, lay, r, strings.Repeat(" ", gutterW), strings.Repeat(" ", gutterW))...)
		}
		for _, d := range b.dissonant {
			body = append(body, line{text: m.dissonanceLine(lay, d), key: "dis:" + d.mine.addr + "|" + d.other, hot: []string{"i"}})
		}
		body = append(body, blank)
	}
	if len(b.elsewhere) > 0 || b.elseLone+b.elseQuiet > 0 {
		body = append(body, m.elsewhereLines(b)...)
	}
	if len(b.quiet) > 0 {
		word := "agents"
		if len(b.quiet) == 1 {
			word = "agent"
		}
		if !m.unfolded["quiet"] {
			body = append(body, line{text: pad(" "+st.dim.Render(fmt.Sprintf("%s %d quiet %s, folded", g.read, len(b.quiet), word)), m.w), key: "fold:quiet"})
		} else {
			body = append(body, line{text: pad(" "+st.dim.Render(fmt.Sprintf("Quiet, idle for over %s", ago(quietAfter))), m.w), key: "fold:quiet"})
			for _, r := range b.quiet {
				body = append(body, m.agentLines(b, lay, r, strings.Repeat(" ", gutterW), strings.Repeat(" ", gutterW))...)
			}
		}
		body = append(body, blank)
	}
	if b.agents == 0 {
		body = append(body, m.empty()...)
	}
	return pinned, body
}

// elsewhereLines are the constellations far from you, folded to chords, what needs attention first: one lamp per
// agent, its shape for the state and its colour for the owner. → unfolds one in place.
func (m *model) elsewhereLines(b *board) []line {
	st, g := m.st, m.g
	agents := 0
	for _, gr := range b.elsewhere {
		agents += len(gr.members)
	}
	head := " " + st.label.Render("ELSEWHERE") + "  " + st.dim.Render(fmt.Sprintf("%d %s · %d agents · what needs attention first",
		len(b.elsewhere), plural(len(b.elsewhere), "constellation", "constellations"), agents))
	out := []line{{text: pad(head, m.w)}}
	shown := 0
	for _, gr := range b.elsewhere {
		if m.unfolded[gr.key] {
			lay := m.layout(b)
			out = append(out, line{text: m.groupHeader(b, lay, gr, false), key: "group:" + gr.key})
			rows, gaps := st.gutter(g, gr.links, len(gr.members))
			for i, r := range gr.members {
				out = append(out, m.agentLines(b, lay, r, rows[i], gaps[i])...)
			}
			continue
		}
		if shown >= 6 {
			continue
		}
		shown++
		var chord strings.Builder
		for _, r := range gr.members {
			chord.WriteString(st.person(b.colours[r.person]).Render(g.lamp(r.st)))
		}
		var right string
		waiting := 0
		for _, r := range gr.members {
			if r.st.waiting() {
				waiting++
			}
		}
		switch {
		case gr.tune.off:
			right = st.thorn.Render(g.outOfTune+" ") + st.clash.Render(path.Base(gr.tune.file))
		case waiting > 0:
			right = st.dim.Render(fmt.Sprintf("%s %d waiting", g.waits, waiting))
		default:
			right = st.fg.Render(g.inTune)
		}
		left := "   " + pad(chord.String(), 9) + pad(st.fg.Render(fit(gr.name, 26, g)), 28) + st.dim.Render(strings.Join(gr.repos, " "+g.also+" "))
		out = append(out, line{text: spread(left, right+"  ", m.w), key: "group:" + gr.key})
	}
	var rest []string
	if more := len(b.elsewhere) - shown - len(m.unfoldedIn(b.elsewhere)); more > 0 {
		rest = append(rest, fmt.Sprintf("+ %d more", more))
	}
	if b.elseLone > 0 {
		rest = append(rest, fmt.Sprintf("%d lone %s", b.elseLone, plural(b.elseLone, "agent", "agents")))
	}
	if b.elseQuiet > 0 {
		rest = append(rest, fmt.Sprintf("%d quiet", b.elseQuiet))
	}
	if len(rest) > 0 {
		out = append(out, line{text: pad("   "+st.dim.Render(strings.Join(rest, " · ")+" · ")+st.hot("All", 0, st.dim)+st.dim.Render(" unfolds them"), m.w),
			hot: []string{"a"}})
	}
	return append(out, line{text: pad("", m.w)})
}

func (m *model) unfoldedIn(gs []*group) []*group {
	var out []*group
	for _, g := range gs {
		if m.unfolded[g.key] {
			out = append(out, g)
		}
	}
	return out
}

func (m *model) empty() []line {
	st := m.st
	text := []string{
		st.bold.Render("No agents yet."),
		st.dim.Render("Start Claude Code in a repo, and its session appears here: what it works on, its branch, and"),
		st.dim.Render("when it needs you. Teammates' agents appear too, once they install murmur."),
	}
	var out []line
	for _, t := range text {
		out = append(out, line{text: pad("   "+t, m.w)})
	}
	return out
}

// needLine is one item of Needs you: who, what they need, and the fuse with its clock.
func (m *model) needLine(b *board, lay layout, n need) string {
	st, g := m.st, m.g
	who := n.who
	person, agent, _ := strings.Cut(n.who, "/")
	initial := ""
	if !b.solo {
		initial = pad(st.person(b.colours[person]).Render(b.initials[person]), lay.initial)
	}
	if agent != "" {
		who = agent
	}
	if n.kind == needHeld {
		_, to, _ := strings.Cut(n.to, "/")
		who += " " + g.heldArrow + " " + shortName(to)
	}
	whoW := min(24, max(16, lay.name+8))
	room := lay.right - 1 - 3 - cellWidth(initial) - whoW // for what they need
	quote := func(word string, style func(...string) string) string {
		return style(word) + st.fg.Render(fit("“"+n.text, room-cellWidth(word)-1, g)+"”")
	}
	var text string
	switch n.kind {
	case needApprove:
		text = st.teal.Render(fit(n.text, room, g))
	case needQuestion:
		text = quote("asks you ", st.dim.Render)
	case needHeld:
		text = quote("held ", st.dim.Render)
	case needFailed:
		text = quote("not delivered ", st.thorn.Render)
	}
	waited := m.now().Sub(n.since)
	right := st.fuse(g, waited, true, 8) + " " + st.dim.Render(clock(waited))
	if m.w < 64 {
		text = st.teal.Render([...]string{"approve", "asks", "held", "failed"}[n.kind])
	}
	if !m.motion && m.progress(animGlow, n.key) >= 0 {
		text = st.tealBold.Render("new ") + text
	}
	left := "   " + initial + st.name.Render(fit(who, whoW-1, g))
	left = pad(left, 3+cellWidth(initial)+whoW) + text
	return pad(pad(left, lay.right-1)+" "+right, m.w)
}

// shortName is the end of an agent's name, where names differ: 9b for api-9b.
func shortName(s string) string {
	if i := strings.LastIndexAny(s, "-_"); i >= 0 && i < len(s)-1 {
		return s[i+1:]
	}
	return s
}

// groupHeader names a constellation, the repos it spans, its people, and its harmony on the right. Folded, it shows
// a chord instead: one lamp per agent, its shape for the state and its colour for the owner.
func (m *model) groupHeader(b *board, lay layout, gr *group, folded bool) string {
	st, g := m.st, m.g
	meta := strings.Join(gr.repos, " "+g.also+" ")
	if gr.people > 1 {
		meta += fmt.Sprintf(" · %d people", gr.people)
	}
	left := " " + st.bold.Render(gr.name) + "   "
	if m.pinned[gr.key] {
		left = " " + st.dim.Render("* ") + st.bold.Render(gr.name) + "   "
	}
	if folded {
		var chord strings.Builder
		for _, r := range gr.members {
			chord.WriteString(st.person(b.colours[r.person]).Render(g.lamp(r.st)))
		}
		left += chord.String() + "  "
		meta += " · folded"
	}
	left += st.dim.Render(meta)
	var right string
	switch {
	case gr.tune.off:
		right = st.thorn.Render(g.outOfTune+" out of tune") + st.dim.Render(" on ") + st.clash.Render(path.Base(gr.tune.file))
	case gr.tune.file != "":
		right = st.fg.Render(g.inTune) + st.dim.Render(" in tune on "+path.Base(gr.tune.file))
	default:
		right = st.fg.Render(g.inTune)
	}
	if m.w < 64 {
		right = st.fg.Render(g.inTune)
		if gr.tune.off {
			right = st.thorn.Render(g.outOfTune)
		}
	}
	return spread(left, right+"  ", m.w)
}

// agentLines is an agent's row, and when it is selected the detail unfolded under it: what it does now, the size
// of its change, its pull request and how full its context is.
func (m *model) agentLines(b *board, lay layout, r *agentRef, gutter, gap string) []line {
	st, g := m.st, m.g
	key := "agent:" + r.addr
	lamp := m.lampStyle(r.st).Render(g.lamp(r.st))
	if m.progress(animBold, r.addr) >= 0 {
		lamp = m.lampStyle(r.st).Bold(true).Reverse(!m.motion).Render(g.lamp(r.st))
	}
	initial := ""
	if lay.initial > 0 {
		initial = pad(st.person(b.colours[r.person]).Render(b.initials[r.person]), lay.initial)
	}
	row := " " + gutter + lamp + " " + initial + pad(st.name.Render(fit(r.a.Name, lay.name-1, g)), lay.name)
	if lay.place > 0 {
		row += pad(st.place(g, r, lay.place-2), lay.place)
	}
	if lay.title > 0 {
		row += pad(m.titleOf(r, lay.title-2), lay.title)
	}
	row = pad(row, lay.right) + m.activity(r)
	out := []line{{text: pad(row, m.w), key: key}}
	if m.selected != key {
		return out
	}
	var parts []string
	if r.a.Doing != "" {
		parts = append(parts, st.fg.Render(r.a.Doing))
	}
	if r.a.Changed > 0 {
		parts = append(parts, st.fg.Render(fmt.Sprintf("+%d", r.a.Added))+" "+st.thorn.Render(fmt.Sprintf("−%d", r.a.Deleted))+
			st.dim.Render(fmt.Sprintf(" in %d %s", r.a.Changed, plural(r.a.Changed, "file", "files"))))
	}
	if r.a.PR > 0 {
		pr := st.fg.Render(fmt.Sprintf("#%d", r.a.PR))
		if r.a.PRURL != "" {
			pr = st.fg.Hyperlink(r.a.PRURL).Render(fmt.Sprintf("#%d", r.a.PR))
		}
		parts = append(parts, pr)
	}
	if r.a.Context > 0 {
		parts = append(parts, st.dim.Render(fmt.Sprintf("context %d%%", r.a.Context)))
	}
	if len(parts) == 0 {
		return out
	}
	detail := " " + gap + strings.Repeat(" ", 2+lay.initial+2) + st.dim.Render("↳ ") + strings.Join(parts, st.dim.Render(" · "))
	return append(out, line{text: pad(ansi.Truncate(detail, m.w-2, g.ellipsis), m.w), key: key})
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func (m *model) lampStyle(s state) ink {
	switch s {
	case working:
		return m.st.fg
	case needsYou:
		return m.st.teal
	case shell, gone:
		return m.st.faint
	}
	return m.st.dim
}

// titleOf is what an agent works on, or why it waits: a waiting agent's title says so, in teal when it waits for
// you. A loop gets a question mark, because spotting one is a guess.
func (m *model) titleOf(r *agentRef, w int) string {
	st, g := m.st, m.g
	switch {
	case r.st == needsYou:
		return st.teal.Render(fit(waitReason(r.a), w, g))
	case r.st == waitsOwner:
		return st.dim.Render(fit("waiting for "+r.person, w, g))
	case r.a.Loop:
		return st.bold.Render(fit("looping? the same command again and again", w, g))
	case r.a.Title == "":
		return st.dim.Render(fit(r.a.Repo, w, g))
	}
	return st.fg.Render(fit(r.a.Title, w, g))
}

// activity is the texture of a working agent, the fuse of a waiting one, or how long it has been idle.
func (m *model) activity(r *agentRef) string {
	st, g := m.st, m.g
	switch r.st {
	case needsYou, waitsOwner:
		waited := r.idleFor(m.now())
		return st.fuse(g, waited, r.st == needsYou, 8) + " " + st.dim.Render(clock(waited))
	case idle:
		return st.dim.Render("idle " + ago(r.idleFor(m.now())))
	case shell:
		return st.faint.Render("shell")
	case gone:
		return st.faint.Render("gone")
	}
	if r.a.Kinds == "" {
		return st.dim.Render("working")
	}
	return st.texture(g, r.a.Kinds, 12)
}

// dissonanceLine is two agents changing the same file without ever talking, ending with what i does: introduce them.
// The file and the words give way before the label does.
func (m *model) dissonanceLine(lay layout, d dissonance) string {
	st, g := m.st, m.g
	file := st.clash.Render(d.file)
	if d.more > 0 {
		file += st.dim.Render(fmt.Sprintf(" and %d more", d.more))
	}
	_, other, _ := strings.Cut(d.other, "/")
	text := "  " + st.dim.Render(d.mine.a.Name+" and "+other+" both change it, never talked")
	if lay.title == 0 {
		text = ""
	}
	act := "  " + st.hot("introduce", 0, st.dim)
	left := " " + strings.Repeat(" ", gutterW) + "    " + st.thorn.Render(g.outOfTune) + " " + file + text
	return pad(ansiFit(left, m.w-cellWidth(act)-2, g)+act, m.w)
}

// outcomeLine is the result of the last action: it stays ten seconds, an error until the next action.
func (m *model) outcomeLine() string {
	o := m.outcome
	if o.text == "" || (!o.err && m.now().Sub(o.at) > outcomeFor) {
		return pad("", m.w)
	}
	if o.err {
		return pad(" "+m.st.thorn.Render(m.g.failed+" "+o.text), m.w)
	}
	return pad(" "+m.st.fg.Render(m.g.delivered+" "+o.text)+m.st.dim.Render(" · "+o.at.Format("15:04")), m.w)
}

const outcomeFor = 10 * time.Second

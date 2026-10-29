package console

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
)

// Sheets open over the board: the palette, one box for everything; the help, every key for where you are; the
// people; and the choice of what to mute. Each is a list you can search as you type.

type sheetKind int

const (
	sheetPalette sheetKind = iota
	sheetHelp
	sheetPeople
	sheetMute
)

type sheet struct {
	kind  sheetKind
	query string
	sel   int
	all   []item
}

// item is one line of a sheet: under a section, plain text that the query matches, a hint or its key on the right,
// and what enter does.
type item struct {
	section string
	text    string
	lead    string // drawn before the text, not searched: a lamp, an initial
	hint    string
	key     string // the key that does the same from the board, drawn in the hotkey style
	run     func(m *model) tea.Cmd
}

func (s *sheet) items() []item {
	if s.query == "" {
		return s.all
	}
	var out []item
	for _, it := range s.all {
		if match(it.text+" "+it.hint+" "+it.key, s.query) != nil {
			out = append(out, it)
		}
	}
	return out
}

// match finds the letters of query in order in text, ignoring case, and says where.
func match(text, query string) []int {
	t, q := []rune(strings.ToLower(text)), []rune(strings.ToLower(query))
	var at []int
	for i, j := 0, 0; j < len(q); i++ {
		if i >= len(t) {
			return nil
		}
		if t[i] == q[j] {
			at = append(at, i)
			j++
		}
	}
	return at
}

func (m *model) openSheet(kind sheetKind, b *board) {
	s := &sheet{kind: kind}
	switch kind {
	case sheetPalette:
		s.all = m.paletteItems(b)
	case sheetHelp:
		s.all = m.helpItems()
	case sheetPeople:
		s.all = m.peopleItems(b)
	case sheetMute:
		s.all = m.muteItems(b)
		if len(s.all) == 0 {
			m.outcome = outcome{text: "Select an agent, a constellation or a message to mute where it comes from", err: true, at: m.now()}
			return
		}
	}
	m.sheet = s
}

func (m *model) sheetKey(k tea.KeyPressMsg) tea.Cmd {
	s := m.sheet
	items := s.items()
	switch k.String() {
	case "esc":
		m.sheet = nil
	case "ctrl+c":
		return tea.Quit
	case "up", "ctrl+p":
		s.sel = max(0, s.sel-1)
	case "down", "ctrl+n":
		s.sel = max(0, min(len(items)-1, s.sel+1)) // nothing matching leaves it on the first line
	case "enter":
		if s.sel < len(items) && items[s.sel].run != nil {
			m.sheet = nil
			return items[s.sel].run(m)
		}
		m.sheet = nil
	case "backspace":
		if r := []rune(s.query); len(r) > 0 {
			s.query, s.sel = string(r[:len(r)-1]), 0
		}
	default:
		if s.kind == sheetMute { // d and f pick how long, on the selected target
			if (k.String() == "d" || k.String() == "f") && s.sel < len(items) {
				m.sheet = nil
				return m.mute(items[s.sel].hint, items[s.sel].text, map[string]string{"d": "24h", "f": ""}[k.String()])
			}
		}
		if k.Text != "" && k.Mod&^tea.ModShift == 0 {
			s.query, s.sel = s.query+k.Text, 0
		}
	}
	return nil
}

// sheetBox draws the sheet as a box to lay over the screen.
func (m *model) sheetBox(maxH int) (string, int) {
	st, g, s := m.st, m.g, m.sheet
	w := max(10, min(72, m.w-6)) // on a terminal too narrow for it, the box is cut at the edge
	inner := w - 4
	title := [...]string{"agents · people · files · actions", "keys", "people", "mute"}[s.kind]
	var rows []string
	cursor := st.teal.Render("▏")
	rows = append(rows, spread(st.dim.Render("› ")+st.fg.Render(s.query)+cursor, st.dim.Render(title), inner))
	rows = append(rows, st.faint.Render(strings.Repeat(g.h, inner)))
	items := s.items()
	section := ""
	visible := maxH - 6
	first := max(0, s.sel-visible/2)
	shown := 0
	for i, it := range items {
		if i < first || shown >= visible {
			continue
		}
		if it.section != section {
			section = it.section
			if len(rows) > 2 {
				rows = append(rows, "")
			}
			rows = append(rows, "  "+st.label.Render(section))
			shown++
		}
		text := highlight(st, it.text, s.query)
		hint := st.dim.Render(it.hint)
		if it.key != "" {
			hint = st.key(it.key)
		}
		if it.lead != "" {
			text = it.lead + " " + text
		}
		row := "  " + spread(text, hint, inner-2)
		if i == s.sel {
			row = tinted(st.bold.Render(g.sel)+" "+spread(text, hint, inner-2), st.selBg)
		}
		rows = append(rows, row)
		shown++
	}
	if len(items) == 0 {
		rows = append(rows, st.dim.Render("Nothing matches."))
	}
	keys := st.keyWord("⏎", "go") + "   " + st.keyWord("esc", "close")
	switch s.kind {
	case sheetMute:
		keys = st.keyWord("⏎", "an hour") + "   " + st.keyWord("d", "a day") + "   " + st.keyWord("f", "until unmuted") + "   " +
			st.keyWord("esc", "close")
	case sheetHelp:
		keys = st.dim.Render("type to search") + "   " + st.keyWord("esc", "close")
	}
	rows = append(rows, "", keys)
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(st.t.dim).Padding(0, 1).Width(w)
	return box.Render(strings.Join(rows, "\n")), w
}

// highlight underlines the letters of the query in text, so matches read without colour too.
func highlight(st styles, text, query string) string {
	at := match(text, query)
	if len(at) == 0 {
		return st.fg.Render(text)
	}
	var b strings.Builder
	hit := st.hotkey // matches read without colour too: underlined, never teal
	for i, r := range []rune(text) {
		if slices.Contains(at, i) {
			b.WriteString(hit.Render(string(r)))
		} else {
			b.WriteString(st.fg.Render(string(r)))
		}
	}
	return b.String()
}

// overlay lays the open sheet over the screen, near its top.
func (m *model) overlay(base string) string {
	box, w := m.sheetBox(m.h - 4)
	x, y := max(0, (m.w-w)/2), 3
	return lipgloss.NewCompositor(lipgloss.NewLayer(base), lipgloss.NewLayer(box).X(x).Y(y).Z(1)).Render()
}

// ---------------------------------------------------------------- what each sheet lists

func (m *model) paletteItems(b *board) []item {
	st, g := m.st, m.g
	var out []item
	selectKey := func(key string) func(*model) tea.Cmd {
		return func(m *model) tea.Cmd { m.selected, m.thread = key, nil; return nil }
	}
	for _, r := range b.all() {
		lead := m.lampStyle(r.st).Render(g.lamp(r.st))
		if !b.solo {
			lead += " " + st.person(b.colours[r.person]).Render(b.initials[r.person])
		}
		out = append(out, item{section: "AGENTS", lead: lead, text: r.a.Name, hint: r.a.Branch, run: func(m *model) tea.Cmd {
			if gr := b.groupOf(r); gr != nil { // a folded agent has no row to select: its fold opens
				delete(m.folded, gr.key)
				m.unfolded[gr.key] = true
			}
			if slices.Contains(b.quiet, r) {
				m.unfolded["quiet"] = true
			}
			return selectKey("agent:" + r.addr)(m)
		}})
	}
	if !b.solo {
		for _, p := range m.snap.People {
			out = append(out, item{section: "PEOPLE", lead: st.person(b.colours[p.Handle]).Render(b.initials[p.Handle]), text: p.Handle,
				hint: fmt.Sprintf("%d %s", len(p.Agents), plural(len(p.Agents), "agent", "agents")),
				run:  func(m *model) tea.Cmd { m.filter = p.Handle; return nil }})
		}
	}
	repos := map[string]int{}
	for _, r := range b.all() {
		if r.a.Repo != "" {
			repos[r.a.Repo]++
		}
	}
	for _, repo := range sortedKeys(repos) {
		out = append(out, item{section: "REPOS", text: repo, hint: fmt.Sprintf("%d %s", repos[repo], plural(repos[repo], "agent", "agents")),
			run: func(m *model) tea.Cmd { m.filter = repo; return nil }})
	}
	for _, gr := range b.groups {
		if gr.tune.file != "" {
			out = append(out, item{section: "FILES", lead: st.fg.Render(g.inTune), text: gr.tune.file, hint: "in tune, in " + gr.name,
				run: selectKey("group:" + gr.key)})
		}
	}
	for _, d := range b.dissonant {
		out = append(out, item{section: "FILES", lead: st.thorn.Render(g.outOfTune), text: d.file, hint: "out of tune",
			run: selectKey("dis:" + d.mine.addr + "|" + d.other)})
	}
	act := func(text, k string) item { // each action with the key that does it from the board
		shown := map[string]string{" ": "␣"}[k]
		if shown == "" {
			shown = k
		}
		return item{section: "ACTIONS", text: text, key: shown, run: func(m *model) tea.Cmd {
			_, cmd := m.key(tea.KeyPressMsg{Code: []rune(k)[0], Text: k})
			return cmd
		}}
	}
	if r := m.selectedAgent(b); r != nil {
		out = append(out, act("Write to "+r.a.Name, "w"))
		if b.askable(r) {
			out = append(out, act("Ask "+r.a.Name+" by the way; it won't see the question", "b"))
		}
		out = append(out, act("Copy "+r.addr, "c"), act("Mute where "+r.a.Name+"'s messages come from", "z"))
		if r.a.PRURL != "" {
			out = append(out, act(fmt.Sprintf("Open #%d", r.a.PR), "o"))
		}
		if r.mine {
			out = append(out, act("Go to "+r.a.Name+"'s terminal", "g"))
		}
		if r.st == waitsOwner {
			out = append(out, act("Nudge "+r.person+": "+r.a.Name+" waits for them", "N"))
		}
	}
	if gr := m.groupAround(b); gr != nil {
		out = append(out, act("Pin "+gr.name+" to the top", "*"))
	}
	out = append(out, item{section: "ACTIONS", text: "Density: comfortable or compact", run: func(m *model) tea.Cmd {
		m.compact = !m.compact
		return nil
	}})
	if m.snap.Update != "" && !m.nix {
		out = append(out, act("Update murmur to "+m.snap.Update+" now", "U"))
	}
	out = append(out, act("Next thing that needs you", "n"))
	if !b.solo {
		for _, sc := range scopes(len(b.colours)) {
			out = append(out, act("Show "+scopeWords[sc], scopeKeys[sc]))
		}
		out = append(out, act("People", "p"))
	}
	out = append(out, act("Filter the board", "/"), act("Inspect the selection", " "), act("Every key", "?"), act("Quit", "q"))
	if !b.solo {
		out = append(out, m.btwItems()...)
	}
	return out
}

// scopeWords say what each scope shows, for the palette.
var scopeWords = [...]string{"the team", "your agents and their constellations", "all, every fold open",
	"what is near you: your agents, what they talk to, your repos"}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// helpItems are the keys that work where the help was opened: in a thread, its own; on a laptop alone, none of the
// team's.
func (m *model) helpItems() []item {
	b := m.board()
	composer := [][3]string{{"COMPOSER", "⏎", "send"}, {"COMPOSER", "⇧⏎ ⌥⏎", "new line"}, {"COMPOSER", "⇥", "change the recipient"},
		{"COMPOSER", "esc", "stop writing, or asking"}}
	var keys [][3]string
	if m.thread != nil {
		keys = [][3]string{
			{"THREAD", "w ⏎", "write"}, {"THREAD", "b", "ask by the way; the agent won't see it"}, {"THREAD", "⇥", "change the recipient"},
			{"THREAD", "↑↓ j k", "scroll"}, {"THREAD", "y x", "let a held message or a question by the way through, or drop it"},
			{"THREAD", "x", "mark the conversation done"}, {"THREAD", "?", "these keys"}, {"THREAD", "esc q", "back to the board"},
			{"THREAD", "⌃c", "quit"},
		}
		if len(b.groups) > 0 {
			keys = append(keys, [3]string{"THREAD", "[ ]", "previous or next constellation's thread"})
		}
	} else {
		keys = [][3]string{
			{"MOVE", "↑↓ j k", "previous or next row"}, {"MOVE", "← → h l", "fold or unfold"}, {"MOVE", "[ ]", "previous or next constellation"},
			{"MOVE", "home end", "top, bottom"}, {"MOVE", "n", "next thing that needs you, from anywhere"},
			{"ACT", "⏎", "open the thread"}, {"ACT", "␣", "inspector on or off"}, {"ACT", "w", "write to the selected agent"},
			{"ACT", "r", "reply to the selected question"}, {"ACT", "b", "ask the selected agent by the way; it won't see it"},
			{"ACT", "y x", "let a held message through, or drop it"},
			{"ACT", "y x", "share an answer by the way once you read it whole, or drop it"}, {"ACT", "y x", "let a cold question by the way run, or decline it"},
			{"ACT", "x", "mark a question or a failed message done"}, {"ACT", "i", "introduce two agents that are out of tune"},
			{"ACT", "o", "open the pull request"}, {"ACT", "c", "copy the address"}, {"ACT", "z", "mute for an hour, a day, until unmuted"},
			{"ACT", "g", "go to your agent's terminal (tmux, iTerm2, WezTerm, kitty)"}, {"ACT", "*", "pin a constellation to the top"},
			{"ACT", "N", "nudge a teammate whose agent waits for them"},
			{"ACT", "U", "update murmur to your team's version, when the header says one is available"},
			{"VIEWS", "t", "Team: everyone's agents"}, {"VIEWS", "m", "Mine: your agents and their constellations; Near me past ten people"},
			{"VIEWS", "a", "All: every fold open"}, {"VIEWS", ": ⌃k", "palette"}, {"VIEWS", "/", "filter in place"}, {"VIEWS", "p", "people"},
			{"VIEWS", "?", "these keys"}, {"VIEWS", "esc", "back one level, never quits"}, {"VIEWS", "q ⌃c", "quit"},
		}
		if b.solo { // no teammates, no scopes
			keys = slices.DeleteFunc(keys, func(k [3]string) bool { return slices.Contains([]string{"t", "m", "a", "p", "N"}, k[1]) })
		}
	}
	var out []item
	for _, k := range append(keys, composer...) {
		out = append(out, item{section: k[0], text: k[2], key: k[1]})
	}
	return out
}

func (m *model) peopleItems(b *board) []item {
	st := m.st
	var out []item
	for _, p := range m.snap.People {
		state := "offline"
		if p.Online {
			state = "online"
		}
		waiting := 0
		for _, a := range p.Agents {
			if agentState(a, false).waiting() {
				waiting++
			}
		}
		hint := fmt.Sprintf("%s · %d %s", state, len(p.Agents), plural(len(p.Agents), "agent", "agents"))
		if waiting > 0 {
			hint += fmt.Sprintf(" · %d waiting", waiting)
		}
		out = append(out, item{section: "PEOPLE", lead: st.person(b.colours[p.Handle]).Render(fmt.Sprintf("%-2s", b.initials[p.Handle])),
			text: p.Handle, hint: hint, run: func(m *model) tea.Cmd { m.filter = p.Handle; return nil }})
	}
	for _, mu := range m.snap.Mutes {
		out = append(out, item{section: "MUTED", text: mu, hint: "murmur unmute"})
	}
	return out
}

// muteItems are what the selection lets you mute: the agent, its person, its constellation, its repo, its branch; on a
// constellation's header, the constellation.
func (m *model) muteItems(b *board) []item {
	r := m.selectedAgent(b)
	if r == nil {
		if gr := m.selectedGroup(b); gr != nil {
			return m.muteTargets([][2]string{{"Constellation " + gr.name, "group:" + gr.members[0].addr}})
		}
		return nil
	}
	targets := [][2]string{{"Agent " + r.a.Name, r.addr}}
	if !r.mine {
		targets = append(targets, [2]string{"Everyone of " + r.person + "'s", r.person})
	}
	if gr := b.groupOf(r); gr != nil {
		targets = append(targets, [2]string{"Constellation " + gr.name, "group:" + r.addr})
	}
	if r.a.Repo != "" && r.a.Tree != "" {
		targets = append(targets, [2]string{"Repo " + r.a.Repo, "repo:" + r.a.Repo})
		if r.a.Branch != "" {
			targets = append(targets, [2]string{"Branch " + r.a.Branch, "branch:" + r.a.Repo + "@" + r.a.Branch})
		}
	}
	return m.muteTargets(targets)
}

// muteTargets are the mute sheet's lines: what it mutes in words, and as murmur mute takes it.
func (m *model) muteTargets(targets [][2]string) []item {
	var out []item
	for _, t := range targets {
		out = append(out, item{section: "HOLD MESSAGES TO YOUR AGENTS FROM", text: t[0], hint: t[1],
			run: func(m *model) tea.Cmd { return m.mute(t[1], t[0], "1h") }})
	}
	return out
}

// mute mutes who, which the outcome names as label says it: "Muted constellation Billing for an hour".
func (m *model) mute(who, label, howLong string) tea.Cmd {
	how := map[string]string{"1h": "for an hour", "24h": "for a day", "": "until you unmute"}[howLong]
	return m.do(ipc.Request{Op: "mute", To: who, Text: howLong}, "Muted "+strings.ToLower(label[:1])+label[1:]+" "+how)
}

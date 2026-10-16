package console

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// The inspector shows everything known about the selected agent, ending with the actions on it. It is a side panel
// on terminals inspectorMin columns wide or more, and the whole screen below that. It follows the selection, so the
// board can be walked with it open.

const (
	inspectorMin = 110
	sideBoard    = 44 // the board's width beside the panel
)

func (m *model) inspectorLines(b *board, w int) []line {
	st, g := m.st, m.g
	r := m.selectedAgent(b)
	if r == nil {
		if gr := m.selectedGroup(b); gr != nil {
			return m.groupInspector(b, gr, w)
		}
		return []line{{text: pad("", w)}, {text: pad("  "+st.dim.Render("Select an agent to see everything murmur knows about it."), w)}}
	}
	a := r.a
	label := func(s string) string { return "  " + st.label.Render(fmt.Sprintf("%-10s", s)) + " " }
	indent := strings.Repeat(" ", 13)
	var out []line
	add := func(s string) { out = append(out, line{text: pad(ansiFit(s, w, g), w)}) }

	initial := ""
	if !b.solo {
		initial = st.person(b.colours[r.person]).Render(b.initials[r.person]) + " "
	}
	since := ""
	if a.Since > 0 {
		since = " " + ago(r.idleFor(m.now()))
	}
	add(spread("  "+initial+st.bold.Render(a.Name), m.lampStyle(r.st).Render(g.lamp(r.st)+" "+r.st.word()+since)+"  ", w))
	if a.Title != "" {
		add("  " + st.fg.Render(a.Title))
	}
	add("")

	where := a.Repo
	if a.Branch != "" {
		where += " " + a.Branch
	}
	if a.Detached != "" {
		where += " " + g.detached + " " + a.Detached
	}
	if r.checkout > 1 {
		where += superscript(r.checkout)
	}
	if a.Base != "" {
		where += " " + g.stacked + " " + a.Base
	}
	add(label("PLACE") + st.fg.Render(where))
	var more []string
	if a.Op != "" {
		more = append(more, a.Op)
	}
	if a.Dir != "" {
		more = append(more, "in "+a.Dir+"/")
	}
	if r.checkout > 1 {
		more = append(more, fmt.Sprintf("checkout %d of this branch", r.checkout))
	}
	if a.App != "" {
		more = append(more, "in "+a.App)
	}
	if len(more) > 0 {
		add(indent + st.dim.Render(strings.Join(more, " · ")))
	}
	if len(a.Also) > 0 {
		add(indent + st.dim.Render("also in "+strings.Join(shortRepos(a.Also), " "+g.also+" ")))
	}
	add("")

	if a.Doing != "" || a.Kinds != "" {
		add(label("NOW") + st.fg.Render(a.Doing))
		if a.Kinds != "" {
			add(indent + st.texture(g, a.Kinds, 30))
			add(indent + st.dim.Render(shares(a.Kinds)))
		}
		if a.Loop {
			add(indent + st.bold.Render("looping?") + st.dim.Render(" the same command in most of its last calls"))
		}
		add("")
	}
	if a.Changed > 0 || a.PR > 0 {
		var parts []string
		if a.Changed > 0 {
			parts = append(parts, st.fg.Render(fmt.Sprintf("+%d", a.Added))+" "+st.thorn.Render(fmt.Sprintf("−%d", a.Deleted))+
				st.dim.Render(fmt.Sprintf(" in %d %s", a.Changed, plural(a.Changed, "file", "files"))))
		}
		if a.PR > 0 {
			pr := fmt.Sprintf("#%d", a.PR)
			if a.PRURL != "" {
				pr = st.fg.Hyperlink(a.PRURL).Render(pr)
			}
			parts = append(parts, pr)
		}
		add(label("CHANGES") + strings.Join(parts, st.dim.Render(" · ")))
	}
	if a.Context > 0 {
		note := ""
		if a.Context >= 75 {
			note = st.dim.Render(" · compacts soon")
		}
		add(label("CONTEXT") + st.fg.Render(contextBar(a.Context)+fmt.Sprintf(" %d%%", a.Context)) + note)
	}
	if a.Changed > 0 || a.PR > 0 || a.Context > 0 {
		add("")
	}

	if touch := m.inTouch(b, r); len(touch) > 0 {
		for i, t := range touch {
			if i == 0 {
				add(label("IN TOUCH") + t)
			} else {
				add(indent + t)
			}
		}
		add("")
	}
	for i, o := range m.harmony(b, r) {
		if i == 0 {
			add(label("HARMONY") + o)
		} else {
			add(indent + o)
		}
		if i == len(m.harmony(b, r))-1 {
			add("")
		}
	}

	// the actions, each a word with its key in it, as many as fit; mute's z isn't in its word, so it stays in the footer
	acts := []string{"write", "copy"}
	if a.PRURL != "" {
		acts = append(acts, fmt.Sprintf("open #%d", a.PR))
	}
	if r.mine {
		acts = append(acts, "go to terminal")
	}
	row := " "
	var hot []string
	for _, act := range acts {
		if cellWidth(row)+2+cellWidth(act) > w {
			break
		}
		row += " " + st.hot(act, 0, st.fg) + " "
		hot = append(hot, act[:1])
	}
	out = append(out, line{text: pad(row, w), hot: hot})
	return out
}

// contextBar is one block, as full as the context window.
func contextBar(pct int) string {
	blocks := []rune(" ▁▂▃▄▅▆▇█")
	return string(blocks[min(len(blocks)-1, pct*(len(blocks)-1)/100)])
}

// inTouch lists the agents this one talked with lately, the busiest first.
func (m *model) inTouch(b *board, r *agentRef) []string {
	st, g := m.st, m.g
	gr := b.groupOf(r)
	if gr == nil {
		return nil
	}
	me := slices.Index(gr.members, r)
	type peer struct {
		r     *agentRef
		l     link
		count int
	}
	var peers []peer
	for _, l := range gr.links {
		switch me {
		case l.a:
			peers = append(peers, peer{gr.members[l.b], l, l.count})
		case l.b:
			peers = append(peers, peer{gr.members[l.a], l, l.count})
		}
	}
	slices.SortFunc(peers, func(x, y peer) int { return y.count - x.count })
	var out []string
	for _, p := range peers {
		mark, style := g.h, st.linkStyle(p.l)
		word := fmt.Sprintf(" %d", p.count)
		if p.l.held {
			mark, word = g.hDash, " held for you"
		}
		who := st.name.Render(p.r.a.Name)
		if !b.solo {
			who = st.person(b.colours[p.r.person]).Render(b.initials[p.r.person]) + " " + who
		}
		out = append(out, style.Render(mark)+" "+who+st.dim.Render(word))
	}
	return out
}

// harmony lists the files this agent changes that another changes too, and whether they talk about it.
func (m *model) harmony(b *board, r *agentRef) []string {
	st, g := m.st, m.g
	var out []string
	for _, o := range m.snap.Pairs {
		if !r.mine || o.Mine != r.a.Name || len(o.Files) == 0 {
			continue
		}
		_, other, _ := strings.Cut(o.Other, "/")
		talked := false
		if gr := b.groupOf(r); gr != nil {
			talked = slices.ContainsFunc(gr.members, func(x *agentRef) bool { return x.addr == o.Other })
		}
		if talked {
			out = append(out, st.fg.Render(g.inTune+" ")+st.fg.Render(path.Base(o.Files[0]))+st.dim.Render(" · talking with "+other))
		} else {
			out = append(out, st.thorn.Render(g.outOfTune+" ")+st.clash.Render(path.Base(o.Files[0]))+st.dim.Render(" · "+other+" changes it too; never talked"))
		}
	}
	return out
}

func (b *board) groupOf(r *agentRef) *group {
	for _, g := range append(slices.Clone(b.groups), b.elsewhere...) {
		if slices.Contains(g.members, r) {
			return g
		}
	}
	return nil
}

func (m *model) selectedGroup(b *board) *group {
	for _, g := range append(slices.Clone(b.groups), b.elsewhere...) {
		if m.selected == "group:"+g.key {
			return g
		}
	}
	return nil
}

// groupInspector sums a constellation up: its agents and what they need.
func (m *model) groupInspector(b *board, gr *group, w int) []line {
	st, g := m.st, m.g
	out := []line{{text: pad("  "+st.bold.Render(gr.name)+"   "+st.dim.Render(strings.Join(gr.repos, " "+g.also+" ")), w)}, {text: pad("", w)}}
	for _, r := range gr.members {
		who := st.name.Render(r.a.Name)
		if !b.solo {
			who = st.person(b.colours[r.person]).Render(b.initials[r.person]) + " " + who
		}
		out = append(out, line{text: pad("  "+m.lampStyle(r.st).Render(g.lamp(r.st))+" "+who+"  "+st.dim.Render(r.st.word()), w)})
	}
	return out
}

// ansiFit cuts a styled line to w cells, with an ellipsis.
func ansiFit(s string, w int, g glyphs) string {
	if cellWidth(s) <= w {
		return s
	}
	return truncate(s, w, g.ellipsis)
}

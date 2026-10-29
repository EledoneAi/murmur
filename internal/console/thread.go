package console

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/charmbracelet/x/ansi"
)

// A thread is one conversation in order: a constellation's, or the one with an agent or a person. Agents' words are
// set in italics, the voice of the collective; people's are upright. Each message says whether it is sending,
// delivered or failed, and a message a mute holds sits in place with its keys. Questions asked by the way of its
// agents, and by them, sit among the messages, set apart (see aside.go). The composer at the bottom knows whether its recipient is
// busy or waiting, and says when it will read the message; asking by the way, it says how the answer comes.

type thread struct {
	name    string          // what the header calls it
	group   string          // the constellation's key, for [ and ]
	members map[string]bool // the addresses whose messages belong here
	to      string          // the composer's recipient
	writing bool            // the composer has the keys; else the thread does
	aside   bool            // the composer asks its recipient by the way: the agent won't see it
	input   textarea.Model
	scroll  int // lines scrolled up from the newest
	// focus is the aside that waits for you which the thread was opened on from Needs you: y and x settle it while it
	// waits, and only its lines carry their keys. jump scrolls to it on the next draw: to its first line you haven't
	// had on screen, when it is taller than the screen. read says which of its lines you had, readOf of which aside.
	focus  string
	jump   bool
	read   []bool
	readOf string
}

func newInput() textarea.Model {
	in := textarea.New()
	in.ShowLineNumbers, in.Prompt, in.CharLimit = false, "", protocol.MaxText
	in.Placeholder = "write to send"
	in.SetHeight(1)
	in.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "alt+enter", "ctrl+j"))
	return in
}

// openThread opens the conversation of what is selected: its constellation, the agent alone, or whoever asked.
func (m *model) openThread(b *board, writing bool) tea.Cmd {
	t := &thread{members: map[string]bool{}, input: newInput(), writing: writing}
	if gr := m.selectedGroup(b); gr != nil { // a constellation's header: its conversation, to its first teammate's agent
		t.name, t.group, t.to = gr.name, gr.key, gr.members[0].addr
		for _, x := range gr.members {
			t.members[x.addr] = true
			if !x.mine && t.to == gr.members[0].addr && gr.members[0].mine {
				t.to = x.addr
			}
		}
		m.thread = t
		m.resizeInput()
		if writing {
			return t.input.Focus()
		}
		return nil
	}
	switch r := m.selectedAgent(b); {
	case r != nil && b.groupOf(r) != nil:
		gr := b.groupOf(r)
		t.name, t.group = gr.name, gr.key
		for _, x := range gr.members {
			t.members[x.addr] = true
		}
		t.to = r.addr
		if n, ok := m.selectedNeed(b); ok && n.kind == needQuestion {
			t.to = n.who
		}
	case r != nil:
		t.name, t.to = r.a.Name, r.addr
		t.members[r.addr] = true
	default:
		n, ok := m.selectedNeed(b)
		if !ok {
			return nil
		}
		t.name, t.to = n.who, n.who
		t.members[n.who] = true
	}
	if n, ok := m.selectedNeed(b); ok && (n.kind == needAsideHeld || n.kind == needAsideCold) { // opened on it
		t.focus, t.jump, t.members[n.btw.To] = n.btw.ID, true, true
	}
	m.thread = t
	m.resizeInput()
	if writing {
		return t.input.Focus()
	}
	return nil
}

func (m *model) resizeInput() {
	if m.thread == nil {
		return
	}
	lines := strings.Count(m.thread.input.Value(), "\n") + 1
	m.thread.input.SetWidth(max(10, m.w-30))
	m.thread.input.SetHeight(min(lines, 4))
}

// threadMsgs are the messages of the thread, oldest first, with the ones a mute holds. An answer by the way delivered
// as a message is the aside's, shown once.
func (m *model) threadMsgs() (msgs []protocol.Msg, held map[string]bool) {
	t, me, delivered := m.thread, m.snap.Me.Handle, m.board().delivered
	in := func(addr string) bool {
		return t.members[addr] || addr == me || protocol.PersonOf(addr) == me && !strings.Contains(addr, "/")
	}
	belongs := func(x protocol.Msg) bool {
		return (t.members[x.From] || t.members[x.To]) && in(x.From) && in(x.To) && x.From != protocol.System && !delivered[x.ID]
	}
	held = map[string]bool{}
	for _, x := range append(slices.Clone(m.snap.Msgs), m.snap.Outbox...) {
		if belongs(x) {
			msgs = append(msgs, x)
		}
	}
	for _, x := range m.snap.Asks {
		if belongs(x) {
			msgs, held[x.ID] = append(msgs, x), true
		}
	}
	slices.SortStableFunc(msgs, func(a, b protocol.Msg) int { return int(a.At - b.At) })
	return msgs, held
}

// who is an address as a thread shows it: the person's initial in their colour, then the agent's or the person's
// name in the given ink.
func (m *model) who(b *board, addr string, name ink) string {
	person, agent, _ := strings.Cut(addr, "/")
	if agent == "" {
		agent = person
	}
	s := name.Render(agent)
	if !b.solo {
		s = m.st.person(b.colours[person]).Render(b.initials[person]) + " " + s
	}
	return s
}

// threadScreen draws the thread under the header: the messages and the asides, newest at the bottom, then the
// composer. It notes which lines of the aside y would settle you have had on screen.
func (m *model) threadScreen(b *board, room int) []line {
	st, g, t := m.st, m.g, m.thread
	msgs, held := m.threadMsgs()
	asides := m.threadAsides()
	target, targetMsg := m.waiting(msgs, held, asides)
	who := func(addr string) string { return m.who(b, addr, st.name) }
	textW := max(20, m.w-14)
	lines := func(x protocol.Msg) []line { // a message as the thread shows it
		var out []line
		add := func(s string) { out = append(out, line{text: pad(s, m.w)}) }
		at := time.UnixMilli(x.At).Format("15:04")
		arrow := st.dim.Render(g.sendArrow)
		status := st.fg.Render(g.delivered)
		switch {
		case held[x.ID]:
			arrow, status = st.teal.Render(g.heldArrow), st.teal.Render("held for you")
		case slices.Contains(m.snap.Sending, x.ID):
			status = st.dim.Italic(true).Render("sending…")
		case m.snap.Failed[x.ID] != "":
			status = st.thorn.Render(g.failed + " " + m.snap.Failed[x.ID])
		}
		head := " " + st.dim.Render(at) + "  " + who(x.From) + " " + arrow + " " + who(x.To)
		if x.Topic != "" {
			head += st.dim.Render("  on " + fit(x.Topic, 30, g))
		}
		if x.Last { // its sender expects no answer: the two agents are no longer in touch
			status = st.dim.Render("last message  ") + status
		}
		add(spread(head, status+"  ", m.w))
		voice := st.italic
		if !strings.Contains(x.From, "/") {
			voice = st.fg
		}
		for _, l := range strings.Split(wrap(x.Text, textW), "\n") {
			add("          " + voice.Render(l))
		}
		if targetMsg != nil && x.ID == targetMsg.ID {
			out = append(out, line{text: pad("          "+st.keyWord("y", "let through")+"   "+st.keyWord("x", "drop"), m.w), hot: []string{"y", "x"}})
		}
		add("")
		return out
	}
	// the composer: its recipient, the text, and when the recipient will read it; asking by the way, its rule says
	// the agent won't see the question, and the line under it how the answer comes
	rule, verb, note := "", "to ", m.readsWhen(b, t.to)
	if t.aside {
		rule = st.dim.Render(g.aside+" by the way: "+agentName(t.to)+" won't see this") + " "
		verb, note = "ask ", m.answersHow(b, t.to)
	}
	compose := []line{{text: pad(" "+rule+st.faint.Render(strings.Repeat(g.h, max(0, m.w-2-cellWidth(rule)))), m.w)}}
	inputLines := strings.Split(t.input.View(), "\n")
	for i, l := range inputLines {
		left := " " + st.dim.Render(verb) + who(t.to) + "  "
		if i > 0 {
			left = strings.Repeat(" ", cellWidth(left))
		}
		right := ""
		if i == 0 && t.writing {
			right = st.keyWord("⇧⏎", "new line") + "  "
		}
		compose = append(compose, line{text: spread(left+l, right, m.w)})
	}
	compose = append(compose, line{text: pad(" "+st.dim.Render(note), m.w)})

	// the messages and the asides, newest at the bottom: only as many as the screen and the scroll show are drawn,
	// all of them to jump to the aside y settles
	room = max(0, room-len(compose)) // a terminal too short for both keeps the composer
	var body []line
	below, tall := -1, 0 // the lines under the aside y settles, and its own
	for i, j := len(msgs)-1, len(asides)-1; (i >= 0 || j >= 0) && (len(body) < room+t.scroll || t.jump); {
		if j >= 0 && (i < 0 || asides[j].At >= msgs[i].At) {
			a := m.asideLines(b, asides[j], textW, target != nil && asides[j].ID == target.ID)
			if target != nil && asides[j].ID == target.ID {
				below, tall = len(body), len(a)
			}
			body = append(a, body...)
			j--
		} else {
			body = append(lines(msgs[i]), body...)
			i--
		}
	}
	if len(msgs) == 0 && len(asides) == 0 {
		body = []line{{text: pad("   "+st.dim.Render("No messages yet. Write below to start the conversation."), m.w)}}
	}
	if t.jump && below >= 0 {
		t.scroll = scrollTo(t, target.ID, below, tall, room)
	}
	t.jump = false
	t.scroll = max(0, min(t.scroll, len(body)-room))
	if below >= 0 && target.State == ipc.AsideHeld {
		m.sawLines(target.ID, below, tall, room)
	}
	start := max(0, len(body)-room-t.scroll)
	shown := body[start:min(len(body), start+room)]
	for len(shown) < room { // the newest messages sit just above the composer
		shown = append([]line{{text: pad("", m.w)}}, shown...)
	}
	return append(shown, compose...)
}

// waiting is what y and x settle in the thread: the aside it was opened on from Needs you while that waits, else the
// newest of what waits for you, a held message or an aside. Only its lines carry the keys.
func (m *model) waiting(msgs []protocol.Msg, held map[string]bool, asides []ipc.Aside) (*ipc.Aside, *protocol.Msg) {
	if i := slices.IndexFunc(asides, func(a ipc.Aside) bool { return a.ID == m.thread.focus && waits(a) }); i >= 0 {
		return &asides[i], nil
	}
	var msg *protocol.Msg
	for i := len(msgs) - 1; i >= 0; i-- {
		if held[msgs[i].ID] {
			msg = &msgs[i]
			break
		}
	}
	if a := lastWaiting(asides); a != nil && (msg == nil || a.At >= msg.At) {
		return a, nil
	}
	return nil, msg
}

// scrollTo is the thread's scroll that shows aside id, with below lines under it and tall of its own: where it is,
// when it is on screen already; else just far enough for all of it; or, taller than the screen, from its first line
// you haven't had on screen.
func scrollTo(t *thread, id string, below, tall, room int) int {
	switch {
	case tall <= room && below >= t.scroll && below+tall <= t.scroll+room:
		return t.scroll
	case tall <= room && below < t.scroll:
		return below
	case tall <= room:
		return below + tall - room
	}
	first := 0
	if t.readOf == id && len(t.read) == tall {
		for first < tall-1 && t.read[first] {
			first++
		}
	}
	return max(below, below+tall-first-room)
}

// sawLines notes the lines of held answer id, below lines from the bottom of the thread and tall of its own, that are
// on screen now: once you have had all of them, y shares it.
func (m *model) sawLines(id string, below, tall, room int) {
	t := m.thread
	if t.readOf != id || len(t.read) != tall { // another aside, or the screen's width changed how it wraps
		t.read, t.readOf = make([]bool, tall), id
	}
	all := true
	for i := range tall {
		d := below + tall - 1 - i // its distance from the bottom
		t.read[i] = t.read[i] || d >= t.scroll && d < t.scroll+room
		all = all && t.read[i]
	}
	if all {
		m.read[id] = true
	}
}

// readsWhen says when the recipient will read a message: between two tool calls, at the end of its turn, or now.
func (m *model) readsWhen(b *board, addr string) string {
	if !strings.Contains(addr, "/") {
		return addr + " sees it in their console."
	}
	for _, r := range b.all() {
		if r.addr != addr {
			continue
		}
		switch r.st {
		case working:
			return r.a.Name + " is working; it reads this between two tool calls."
		case needsYou:
			return r.a.Name + " waits for you; it reads this when its turn ends."
		case waitsOwner:
			return r.a.Name + " waits on " + r.person + "; it reads this when its turn ends."
		case gone:
			return r.a.Name + "'s laptop is offline; it gets this when it reconnects."
		}
		return r.a.Name + " is idle; this wakes it."
	}
	return addr + " isn't running; the message waits for it."
}

// threadKey handles a key in the thread: the composer's when it has them.
func (m *model) threadKey(b *board, k tea.KeyPressMsg) tea.Cmd {
	t := m.thread
	if t.writing {
		switch k.String() {
		case "ctrl+c":
			return tea.Quit
		case "esc":
			t.writing = false
			t.input.Blur()
			t.setAside(false)
			return nil
		case "enter":
			text := strings.TrimSpace(t.input.Value())
			if text == "" {
				return nil
			}
			if t.aside && len([]rune(text)) > protocol.MaxBtwQuestion {
				m.outcome = outcome{text: fmt.Sprintf("A question by the way holds %d characters at most", protocol.MaxBtwQuestion), err: true, at: m.now()}
				return nil
			}
			t.input.Reset()
			m.resizeInput()
			t.scroll = 0
			if t.aside {
				return m.askBtw(t.to, text)
			}
			return m.send(t.to, text)
		case "tab":
			t.to = m.nextRecipient(b, t)
			return nil
		case "pgup":
			t.scroll += 5
			return nil
		case "pgdown":
			t.scroll = max(0, t.scroll-5)
			return nil
		}
		var cmd tea.Cmd
		t.input, cmd = t.input.Update(k)
		m.resizeInput()
		return cmd
	}
	msgs, held := m.threadMsgs()
	// y and x settle what waits for you: the aside the thread was opened on, else the newest of what waits, a held
	// message or an aside. y shares an answer only once all of it has been on screen: until then, it scrolls to it.
	a, heldMsg := m.waiting(msgs, held, m.threadAsides())
	if a != nil && (k.String() == "y" || k.String() == "x") {
		if k.String() == "y" && a.State == ipc.AsideHeld && !m.read[a.ID] {
			t.focus, t.jump = a.ID, true
			m.outcome = outcome{text: "Read on: y shares " + agentName(a.To) + "'s answer once all of it has been on screen", at: m.now()}
			return nil
		}
		return m.settle(*a, k.String() == "y")
	}
	switch k.String() {
	case "esc", "q":
		m.thread = nil
	case "w", "r", "enter":
		t.writing = true
		t.setAside(false)
		return t.input.Focus()
	case "b":
		return m.askHere(b)
	case "tab":
		t.to = m.nextRecipient(b, t)
	case "up", "k", "pgup":
		t.scroll++
	case "down", "j", "pgdown":
		t.scroll = max(0, t.scroll-1)
	case "y":
		if heldMsg != nil {
			return m.do(ipc.Request{Op: "approve", ID: heldMsg.ID}, "Let through to "+heldMsg.To)
		}
	case "x":
		if heldMsg != nil {
			return m.do(ipc.Request{Op: "decline", ID: heldMsg.ID}, "Dropped; "+heldMsg.From+" is told")
		}
		var ids []string // mark the conversation done: it leaves Needs you until someone writes again
		for _, x := range msgs {
			ids = append(ids, x.ID)
		}
		m.thread = nil
		if len(ids) > 0 {
			return m.do(ipc.Request{Op: "done", IDs: ids}, "Marked done")
		}
	case "[", "]":
		m.switchThread(b, k.String() == "]")
	case "?":
		m.openSheet(sheetHelp, b)
	case "ctrl+c":
		return tea.Quit
	}
	return nil
}

// nextRecipient goes round the thread's members, other than you; asking by the way, round its agents that can be
// asked.
func (m *model) nextRecipient(b *board, t *thread) string {
	var all []string
	for a := range t.members {
		if (protocol.PersonOf(a) != m.snap.Me.Handle || strings.Contains(a, "/")) && (!t.aside || b.askable(b.find(a))) {
			all = append(all, a)
		}
	}
	slices.Sort(all)
	if len(all) == 0 {
		return t.to
	}
	i := slices.Index(all, t.to)
	return all[(i+1)%len(all)]
}

// switchThread moves to the previous or next constellation's thread.
func (m *model) switchThread(b *board, forward bool) {
	if len(b.groups) == 0 {
		return
	}
	i := slices.IndexFunc(b.groups, func(g *group) bool { return g.key == m.thread.group })
	if i < 0 && !forward { // a lone agent's thread: ] goes to the first constellation, [ to the last
		i = 0
	}
	if forward {
		i = (i + 1) % len(b.groups)
	} else {
		i = (i - 1 + len(b.groups)) % len(b.groups)
	}
	m.selected = "agent:" + b.groups[i].members[0].addr
	writing := m.thread.writing
	m.openThread(b, writing)
}

// send sends a message, and says so on the outcome line once the daemon took it.
func (m *model) send(to, text string) tea.Cmd {
	return func() tea.Msg {
		r, err := m.call(ipc.Request{Op: "send", To: to, Text: text}, 10*time.Second)
		switch {
		case err != nil:
			return doneMsg{text: err.Error(), err: true}
		case !r.OK:
			return doneMsg{text: r.Err, err: true}
		}
		word := "Sent to "
		if r.Note == "delivered" {
			word = "Delivered to "
		}
		return doneMsg{text: word + to}
	}
}

// wrap breaks text into lines of at most w cells, at spaces where it can: a link, or Chinese, breaks anywhere.
func wrap(text string, w int) string { return ansi.Wrap(strings.TrimRight(text, "\n"), w, "") }

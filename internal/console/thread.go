package console

import (
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
// delivered or failed, and a message a mute holds sits in place with its keys. The composer at the bottom knows
// whether its recipient is busy or waiting, and says when it will read the message.

type thread struct {
	name    string          // what the header calls it
	group   string          // the constellation's key, for [ and ]
	members map[string]bool // the addresses whose messages belong here
	to      string          // the composer's recipient
	writing bool            // the composer has the keys; else the thread does
	input   textarea.Model
	scroll  int // lines scrolled up from the newest
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

// threadMsgs are the messages of the thread, oldest first, with the ones a mute holds.
func (m *model) threadMsgs() (msgs []protocol.Msg, held map[string]bool) {
	t, me := m.thread, m.snap.Me.Handle
	in := func(addr string) bool {
		return t.members[addr] || addr == me || protocol.PersonOf(addr) == me && !strings.Contains(addr, "/")
	}
	belongs := func(x protocol.Msg) bool {
		return (t.members[x.From] || t.members[x.To]) && in(x.From) && in(x.To) && x.From != protocol.System
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

// threadScreen draws the thread under the header: the messages, newest at the bottom, then the composer.
func (m *model) threadScreen(b *board, room int) []line {
	st, g, t := m.st, m.g, m.thread
	msgs, held := m.threadMsgs()
	who := func(addr string) string {
		person, agent, _ := strings.Cut(addr, "/")
		name := agent
		if agent == "" {
			name = person
		}
		s := st.name.Render(name)
		if !b.solo {
			s = st.person(b.colours[person]).Render(b.initials[person]) + " " + s
		}
		return s
	}
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
		add(spread(" "+st.dim.Render(at)+"  "+who(x.From)+" "+arrow+" "+who(x.To), status+"  ", m.w))
		voice := st.italic
		if !strings.Contains(x.From, "/") {
			voice = st.fg
		}
		for _, l := range strings.Split(wrap(x.Text, textW), "\n") {
			add("          " + voice.Render(l))
		}
		if held[x.ID] {
			out = append(out, line{text: pad("          "+st.keyWord("y", "let through")+"   "+st.keyWord("x", "drop"), m.w), hot: []string{"y", "x"}})
		}
		add("")
		return out
	}
	// the composer: its recipient, the text, and when the recipient will read it
	compose := []line{{text: pad(" "+st.faint.Render(strings.Repeat(g.h, max(0, m.w-2))), m.w)}}
	inputLines := strings.Split(t.input.View(), "\n")
	for i, l := range inputLines {
		left := " " + st.dim.Render("to ") + who(t.to) + "  "
		if i > 0 {
			left = strings.Repeat(" ", cellWidth(left))
		}
		right := ""
		if i == 0 && t.writing {
			right = st.keyWord("⇧⏎", "new line") + "  "
		}
		compose = append(compose, line{text: spread(left+l, right, m.w)})
	}
	compose = append(compose, line{text: pad(" "+st.dim.Render(m.readsWhen(b, t.to)), m.w)})

	// the messages, newest at the bottom: only as many as the screen and the scroll show are drawn
	room = max(0, room-len(compose)) // a terminal too short for both keeps the composer
	var body []line
	for i := len(msgs) - 1; i >= 0 && len(body) < room+t.scroll; i-- {
		body = append(lines(msgs[i]), body...)
	}
	if len(msgs) == 0 {
		body = []line{{text: pad("   "+st.dim.Render("No messages yet. Write below to start the conversation."), m.w)}}
	}
	t.scroll = max(0, min(t.scroll, len(body)-room))
	start := max(0, len(body)-room-t.scroll)
	shown := body[start:min(len(body), start+room)]
	for len(shown) < room { // the newest messages sit just above the composer
		shown = append([]line{{text: pad("", m.w)}}, shown...)
	}
	return append(shown, compose...)
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
			return nil
		case "enter":
			text := strings.TrimSpace(t.input.Value())
			if text == "" {
				return nil
			}
			t.input.Reset()
			m.resizeInput()
			t.scroll = 0
			return m.send(t.to, text)
		case "tab":
			t.to = m.nextRecipient(t)
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
	var heldMsg *protocol.Msg
	for i := len(msgs) - 1; i >= 0; i-- {
		if held[msgs[i].ID] {
			heldMsg = &msgs[i]
			break
		}
	}
	switch k.String() {
	case "esc", "q":
		m.thread = nil
	case "w", "r", "enter":
		t.writing = true
		return t.input.Focus()
	case "tab":
		t.to = m.nextRecipient(t)
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
	case "ctrl+c":
		return tea.Quit
	}
	return nil
}

// nextRecipient goes round the thread's members, other than you.
func (m *model) nextRecipient(t *thread) string {
	var all []string
	for a := range t.members {
		if protocol.PersonOf(a) != m.snap.Me.Handle || strings.Contains(a, "/") {
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

// wrap breaks text into lines of at most w cells, at spaces where it can.
func wrap(text string, w int) string { return ansi.Wordwrap(strings.TrimRight(text, "\n"), w, "") }

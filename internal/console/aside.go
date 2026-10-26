package console

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

// Questions by the way: you ask an agent something without interrupting it. A throwaway copy of its conversation, on
// its person's laptop, answers; the agent itself never sees the question. b asks the selected agent. A question and
// its answer sit in the thread of the agent asked, dimmer than the messages and behind the aside sign: they are never
// messages to or from the agent. A teammate's answer that waits for you to share it, and a teammate's question to an
// agent idle so long that its copy would read the whole conversation again, wait in Needs you.
//
// Agents ask too, with murmur's btw tool: yours, and teammates' agents asking yours. An agent's question sits in the
// asking agent's thread as well, in its voice, and links the two agents on the board as a message would: the asker
// learned from the other's conversation, so they are in touch. When the answer comes after the tool stopped waiting,
// the daemon hands it to the asking agent as a message; the console shows it once, as the aside's answer.

// coldAfter is how long an agent idles before a question to it is cold: its copy reads the whole conversation again,
// at full price, so a teammate's question waits for its person's go. The daemon decides; the console only says so
// before you ask.
const coldAfter = time.Hour

// find is the agent at an address, shown or not, or nil.
func (b *board) find(addr string) *agentRef { return b.byAddr[addr] }

// askable says whether you can ask an agent by the way: a teammate's when its person lets teammates ask; one of yours
// likewise, or when you turned only teammates' questions off (protocol.BtwMine), from any of your laptops; and one of
// yours on this laptop when the daemon lists it in Said, whatever it tells the team: this laptop can always tell. One
// of yours that the daemon lists in CantAsk can't be asked, whatever else says it can.
func (b *board) askable(r *agentRef) bool {
	if r == nil || r.st == gone || b.cantAskHere(r) != "" {
		return false
	}
	switch r.a.Btw {
	case protocol.BtwApprove, protocol.BtwAuto:
		return true
	case protocol.BtwMine:
		return r.mine
	}
	_, here := b.said[r.a.Name] // this laptop reads what its own agents say
	return r.mine && here
}

// cantAskHere is why nobody can ask one of your agents on this laptop by the way, in the daemon's words, which its
// person acts on: "api-9a can't be asked by the way: its Claude Code is older than 2.1.292; update Claude Code", or
// "codex-1 runs in Codex, which can't be asked by the way". It is "" for the others: yours on another laptop, and a
// teammate's, though it shares a name with one here.
func (b *board) cantAskHere(r *agentRef) string {
	if r == nil || !r.mine {
		return ""
	}
	return b.cantAsk[r.a.Name]
}

// whyNot says why an agent can't be asked by the way. For one of yours on this laptop, the daemon says why. For the
// others: its laptop is offline, its person turned teammates' questions off, or nobody can ask it. Nobody can when its
// coding tool can't copy a conversation (managed settings may keep Claude Code from it), its Claude Code or the murmur
// on its laptop is too old, or the hub doesn't carry questions; the hub does once any agent says how it answers. A
// person's other agents tell nothing about this one: they may run on another laptop.
func (b *board) whyNot(r *agentRef) string {
	switch why := b.cantAskHere(r); {
	case r == nil || b.askable(r):
		return ""
	case why != "":
		return oneLine(why)
	case r.st == gone:
		return r.person + "'s laptop is offline; ask " + r.a.Name + " by the way once it's back"
	case r.a.Btw == protocol.BtwMine: // a teammate's: yours can be asked
		return r.person + " turned questions by the way off"
	case !b.hubBtw:
		return r.a.Name + " can't be asked by the way: the hub doesn't carry questions, its coding tool can't copy a " +
			"conversation, or its Claude Code or murmur is too old"
	}
	return r.a.Name + " can't be asked by the way: its coding tool can't copy a conversation, or its Claude Code or murmur " +
		"is too old"
}

// briefly says why something can't be done in w cells, for the outcome line: all of it when it fits, else its first
// clauses, up to a colon or a semicolon, else as many whole words as fit. "api-9a can't be asked by the way: its Claude
// Code is older than 2.1.292; update Claude Code" says less as the line narrows: "api-9a can't be asked by the way:
// its Claude Code is older than 2.1.292", then "api-9a can't be asked by the way". The inspector says all of it.
func briefly(why string, w int, g glyphs) string {
	why = oneLine(why)
	if cellWidth(why) <= w {
		return why
	}
	for i := len(why); i > 0; {
		if i = max(strings.LastIndex(why[:i], ": "), strings.LastIndex(why[:i], "; ")); i > 0 && cellWidth(why[:i]) <= w {
			return why[:i]
		}
	}
	words := strings.TrimRight(firstLine(wrap(why, w-cellWidth(g.ellipsis))), " ,;:") + g.ellipsis
	if w <= cellWidth(g.ellipsis) || cellWidth(words) > w { // a first word too long for the line
		return fit(why, w, g)
	}
	return words
}

// cannot says on the outcome line why something can't be done, as much of it as fits.
func (m *model) cannot(why string) {
	m.outcome = outcome{text: briefly(why, m.w-2-cellWidth(m.g.failed), m.g), err: true, at: m.now()}
}

// ask opens the composer on the selected agent's thread, to ask it by the way, or says why it can't be asked.
func (m *model) ask(b *board) tea.Cmd {
	r := m.selectedAgent(b)
	switch {
	case r == nil:
		m.outcome = outcome{text: "Select an agent to ask it by the way", err: true, at: m.now()}
		return nil
	case !b.askable(r):
		m.cannot(b.whyNot(r))
		return nil
	}
	cmd := m.openThread(b, true)
	m.thread.to = r.addr
	m.thread.setAside(true)
	return cmd
}

// askHere turns the open thread's composer to asking by the way: its recipient when it can be asked, else the first
// of the thread's agents that can.
func (m *model) askHere(b *board) tea.Cmd {
	t := m.thread
	r := b.find(t.to)
	if !b.askable(r) {
		for _, addr := range slices.Sorted(maps.Keys(t.members)) {
			if x := b.find(addr); b.askable(x) {
				r = x
				break
			}
		}
	}
	if !b.askable(r) {
		why := "No agent in this thread can be asked by the way"
		if x := b.find(t.to); x != nil {
			why = b.whyNot(x)
		}
		m.cannot(why)
		return nil
	}
	t.to, t.writing = r.addr, true
	t.setAside(true)
	return t.input.Focus()
}

// canAskHere says whether one of the open thread's agents can be asked by the way: its recipient first.
func (m *model) canAskHere(b *board) bool {
	t := m.thread
	if b.askable(b.find(t.to)) {
		return true
	}
	for a := range t.members {
		if strings.Contains(a, "/") && b.askable(b.find(a)) {
			return true
		}
	}
	return false
}

// setAside turns the composer to asking by the way, or back to writing a message.
func (t *thread) setAside(on bool) {
	t.aside = on
	t.input.Placeholder, t.input.CharLimit = "write to send", protocol.MaxText
	if on {
		t.input.Placeholder, t.input.CharLimit = "ask by the way", protocol.MaxBtwQuestion
	}
}

// askBtw asks an agent a question by the way, under an ID chosen here; the answer comes in the agent's thread.
func (m *model) askBtw(to, text string) tea.Cmd {
	return m.do(ipc.Request{Op: "btw", ID: protocol.RandomHex(8), To: to, Text: text}, "Asked "+to+" by the way; the answer comes in its thread")
}

// answersHow says, under the composer, how the answer to a question by the way comes: a copy answers at once, its
// person reads it first, or the agent idled so long that the question waits for its person's go.
func (m *model) answersHow(b *board, addr string) string {
	const copyAnswers = "A throwaway copy of its conversation answers"
	r := b.find(addr)
	if r == nil {
		return copyAnswers + "."
	}
	idleFor := r.idleFor(m.now())
	cold := r.st == idle && idleFor > coldAfter
	switch {
	case cold && r.mine:
		return "Idle " + ago(idleFor) + ": its copy reads the whole conversation again before it answers."
	case r.mine || (r.a.Btw == protocol.BtwAuto && !cold):
		return copyAnswers + ", in a few seconds."
	case cold:
		return "Idle " + ago(idleFor) + ": the question waits for " + r.person + "'s go, then its copy reads the whole conversation again."
	}
	return copyAnswers + "; " + r.person + " reads the answer before it reaches you."
}

// threadAsides are the questions asked by the way of the thread's agents, and by them, oldest first.
func (m *model) threadAsides() []ipc.Aside {
	var out []ipc.Aside
	for _, a := range m.snap.Asides {
		if m.thread.members[a.To] || byAgent(a) && m.thread.members[a.From] {
			out = append(out, a)
		}
	}
	slices.SortStableFunc(out, func(x, y ipc.Aside) int { return cmp.Compare(x.At, y.At) })
	return out
}

// waits says an aside waits for you: an answer to share, or a cold question to let run.
func waits(a ipc.Aside) bool { return a.State == ipc.AsideHeld || a.State == ipc.AsideCold }

// lastWaiting is the newest of the asides that wait for you, or nil.
func lastWaiting(asides []ipc.Aside) *ipc.Aside {
	for i := len(asides) - 1; i >= 0; i-- {
		if waits(asides[i]) {
			return &asides[i]
		}
	}
	return nil
}

// settle answers an aside that waits for you: y shares the answer, or lets the cold question's copy run; x drops the
// answer, or declines the question. The teammate who asked is told either way.
func (m *model) settle(a ipc.Aside, yes bool) tea.Cmd { return m.do(settleRequest(a, yes)) }

// share shares the answer of a need held for you, once all of it has been on screen: in Needs you, where it fits on
// the line under the need, or in the thread. Until then, y opens the thread on it, where all of it shows.
func (m *model) share(b *board, n need) tea.Cmd {
	if m.read[n.btw.ID] {
		return m.settle(n.btw, true)
	}
	cmd := m.openThread(b, false)
	m.outcome = outcome{text: "Read " + agentName(n.btw.To) + "'s answer to " + askedBy(n.btw, "") + ": y shares it, x drops it", at: m.now()}
	return cmd
}

// shareWord is what y does to a held answer: shares it once you have had all of it on screen, else reads it first.
func (m *model) shareWord(a ipc.Aside) string {
	if m.read[a.ID] {
		return "share"
	}
	return "read"
}

// settleRequest is what settles an aside, and what to say once it's done. What waits for you was asked by a teammate
// or a teammate's agent, never by you or yours.
func settleRequest(a ipc.Aside, yes bool) (ipc.Request, string) {
	name, who := agentName(a.To), askedBy(a, "")
	switch {
	case a.State == ipc.AsideHeld && yes:
		return ipc.Request{Op: "btw-approve", ID: a.ID}, "Shared " + name + "'s answer with " + who
	case a.State == ipc.AsideHeld:
		return ipc.Request{Op: "btw-decline", ID: a.ID}, "Dropped " + name + "'s answer; " + who + " is told"
	case yes:
		return ipc.Request{Op: "btw-approve", ID: a.ID}, name + "'s copy is answering " + who
	}
	return ipc.Request{Op: "btw-decline", ID: a.ID}, "Declined; " + who + " is told"
}

// agentName is the agent's part of an address: main-1 for bob/main-1.
func agentName(addr string) string {
	if _, name, ok := strings.Cut(addr, "/"); ok {
		return name
	}
	return addr
}

// byAgent says an agent asked the aside, with murmur's btw tool, rather than a person.
func byAgent(a ipc.Aside) bool { return strings.Contains(a.From, "/") }

// askedBy names who asked an aside, as a sentence says it to you, the person me: you, carol, your main-1, carol's agent
// web-2.
func askedBy(a ipc.Aside, me string) string {
	person, name, agent := strings.Cut(a.From, "/")
	switch {
	case !agent && (a.Mine || person == me):
		return "you"
	case !agent:
		return person
	case person == me:
		return "your " + name
	}
	return person + "'s agent " + name
}

// whose is the possessive of who askedBy names: your, carol's, your main-1's.
func whose(who string) string {
	if who == "you" {
		return "your"
	}
	return who + "'s"
}

// capital starts a sentence: You, Your, Your main-1. Handles stay as people chose them, youssef too.
func capital(s string) string {
	if first, _, _ := strings.Cut(s, " "); first == "you" || first == "your" {
		return "Y" + s[1:]
	}
	return s
}

// deliveries are the messages that carry an answer by the way to the agent that asked, once its tool stopped waiting:
// the daemon hands the final answer to the agent as a message from the agent asked (murmur's note when there is
// none), under the id bw-<the aside's id>, but the console shows it once, as the aside's answer. They are left out of
// threads, links, needs and the plain mode's messages. Only the id tells one: a message that says what an answer
// said is a message all the same, and the hub takes no message under such an id.
func deliveries(s ipc.Snapshot) map[string]bool {
	ids := map[string]bool{}
	for _, a := range s.Asides {
		if byAgent(a) && a.Final() {
			ids["bw-"+a.ID] = true
		}
	}
	var out map[string]bool
	for _, x := range s.Msgs {
		if ids[x.ID] {
			if out == nil {
				out = map[string]bool{}
			}
			out[x.ID] = true
		}
	}
	return out
}

// asideLines draw a question asked by the way in a thread, with its answer: dimmer than the messages, behind the
// aside sign, the answer behind its hook, and where it stands on the right. An agent's question is in its voice, in
// italics. An answer or a cold question that waits for you carries its keys when they settle it.
func (m *model) asideLines(b *board, a ipc.Aside, textW int, keys bool) []line {
	st, g := m.st, m.g
	var out []line
	add := func(s string) { out = append(out, line{text: pad(s, m.w)}) }
	const indent = "          "
	hook := strings.Repeat(" ", max(0, len(indent)-1-cellWidth(g.answer))) + st.dim.Render(g.answer) + " "
	head := " " + st.dim.Render(time.UnixMilli(a.At).Format("15:04")) + "  " + m.who(b, a.From, st.dim) + " " +
		st.dim.Render(g.aside) + " " + m.who(b, a.To, st.dim) + "  " + st.dim.Render("by the way")
	add(spread(head, m.asideStatus(a)+"  ", m.w))
	voice := st.dim
	if byAgent(a) {
		voice = st.aside
	}
	for _, l := range strings.Split(wrap(a.Question, textW), "\n") {
		add(indent + voice.Render(l))
	}
	under := func(text string, ink ink) {
		for i, l := range strings.Split(wrap(text, textW), "\n") {
			if i == 0 {
				add(hook + ink.Render(l))
			} else {
				add(indent + ink.Render(l))
			}
		}
	}
	switch a.State {
	case ipc.AsideHeld, ipc.AsideAnswered:
		under(a.Answer, st.aside)
	case ipc.AsideFailed:
		under(a.Err, st.thorn)
	case ipc.AsideCold:
		what := "its copy would read " + tokens(a.Cold) + " tokens, the whole conversation"
		if r := b.find(a.To); r != nil && r.st == idle && r.a.Since > 0 {
			what = "idle " + ago(r.idleFor(m.now())) + "; " + what
		}
		under(what, st.dim)
	}
	switch {
	case keys && a.State == ipc.AsideHeld:
		out = append(out, line{text: pad(indent+st.keyWord("y", "share")+"   "+st.keyWord("x", "drop"), m.w), hot: []string{"y", "x"}})
	case keys && a.State == ipc.AsideCold:
		out = append(out, line{text: pad(indent+st.keyWord("y", "go")+"   "+st.keyWord("x", "decline"), m.w), hot: []string{"y", "x"}})
	}
	add("")
	return out
}

// asideStatus is where an aside stands, for the right of its line: on its way, answering, waiting for you, answered
// with the tokens its copy read, or no answer.
func (m *model) asideStatus(a ipc.Aside) string {
	st, g := m.st, m.g
	read := ""
	if a.Tokens > 0 {
		read = st.dim.Render(tokens(a.Tokens) + " tokens read  ")
	}
	switch a.State {
	case ipc.AsideAsking:
		if a.Note != "" {
			return st.aside.Render(a.Note + "…")
		}
		return st.aside.Render("asking…")
	case ipc.AsideRunning:
		return st.aside.Render("its copy is answering…")
	case ipc.AsideHeld:
		return read + st.teal.Render("answer held for you")
	case ipc.AsideCold:
		return st.teal.Render("waits for your go")
	case ipc.AsideFailed:
		return read + st.thorn.Render(g.failed+" no answer")
	}
	return read + st.fg.Render(g.delivered)
}

// asideWord is where an aside stands, in a few words, for the inspector.
func asideWord(a ipc.Aside) string {
	switch a.State {
	case ipc.AsideAsking:
		return cmp.Or(a.Note, "asking")
	case ipc.AsideRunning:
		return "answering"
	case ipc.AsideHeld:
		return "answer held for you"
	case ipc.AsideCold:
		return "waits for your go"
	case ipc.AsideFailed:
		return "no answer"
	}
	return "answered"
}

// needDetail unfolds a question by the way under its line in Needs you, when it is selected: the answer that waits
// for you to share it, or what a cold question's copy would read, and the keys that settle it. An answer that fits
// there whole has been on screen: y shares it at once.
func (m *model) needDetail(b *board, lay layout, n need) (line, bool) {
	st, g := m.st, m.g
	var keys string
	switch n.kind {
	case needAsideHeld:
		keys = st.keyWord("y", "share") + "  " + st.keyWord("x", "drop")
	case needAsideCold:
		keys = st.keyWord("y", "go") + "  " + st.keyWord("x", "decline")
	default:
		return line{}, false
	}
	initial := 0
	if !b.solo {
		initial = lay.initial
	}
	left := strings.Repeat(" ", 3+initial+min(24, max(16, lay.name+8))) + st.dim.Render(g.answer+" ")
	room := func() int { return lay.right - 1 - cellWidth(keys) - 2 - cellWidth(left) } // for what it says
	answer := strings.TrimSpace(n.btw.Answer)
	if n.kind == needAsideHeld && room() >= 8 && !strings.Contains(answer, "\n") && cellWidth("“"+answer) <= room()-1 {
		m.read[n.btw.ID] = true // all of it shows here
	}
	if n.kind == needAsideHeld && !m.read[n.btw.ID] {
		keys = st.keyWord("y", "read") + "  " + st.keyWord("x", "drop")
	}
	if room := room(); room >= 8 {
		if n.kind == needAsideHeld {
			left += st.aside.Render(fit("“"+firstLine(answer), room-1, g) + "”")
		} else {
			what := "its copy would read " + tokens(n.btw.Cold) + " tokens"
			if n.ref != nil && n.ref.st == idle && n.ref.a.Since > 0 {
				what = "idle " + ago(n.ref.idleFor(m.now())) + "; " + what
			}
			left += st.dim.Render(fit(what, room, g))
		}
	}
	body := pad(left, max(0, lay.right-1-cellWidth(keys))) + keys
	return line{text: pad(body, m.w), key: n.key, hot: []string{"y", "x"}}, true
}

// said is what one of your agents on this laptop last said in its own words, or nothing: the daemon says it only of the
// agents here that can be asked by the way.
func (m *model) said(r *agentRef) string {
	if !r.mine {
		return ""
	}
	return m.snap.Said[r.a.Name]
}

// btwLines say, for the inspector, who can ask the agent by the way and how its answers come, or why it can't be asked,
// wrapped to w cells, then the latest three questions asked of it, and by it, newest first: ↝ carol for one carol
// asked it, ↝ asked B main-1 for one it asked.
func (m *model) btwLines(b *board, r *agentRef, w int) []string {
	st, g := m.st, m.g
	var how string
	switch {
	case !b.askable(r):
		how = b.whyNot(r)
	case r.mine && r.a.Btw == protocol.BtwApprove:
		how = "teammates and agents can ask it; you share answers"
	case r.mine && r.a.Btw == protocol.BtwAuto && m.snap.BtwAnswers == ipc.BtwAll:
		how = "teammates and agents can ask it; every answer goes out on its own"
	case r.mine && r.a.Btw == protocol.BtwAuto:
		how = "teammates and agents can ask it; answers go out on their own"
	case r.mine && r.a.Btw == protocol.BtwMine:
		how = "only you and your agents can ask it; teammates' questions are off"
	case r.mine: // this laptop asks it without the hub
		how = "only you and your agents on this laptop can ask it"
	case r.a.Btw == protocol.BtwApprove:
		how = "you and your agents can ask it; " + r.person + " reads answers first"
	default:
		how = "you and your agents can ask it; answers come on their own"
	}
	var out []string
	for _, l := range strings.Split(wrap(how, w), "\n") {
		out = append(out, st.fg.Render(l))
	}
	asked := 0
	for i := len(m.snap.Asides) - 1; i >= 0 && asked < 3; i-- {
		a := m.snap.Asides[i]
		var who string
		switch {
		case a.To == r.addr && byAgent(a):
			who = m.who(b, a.From, st.name)
		case a.To == r.addr:
			who = st.name.Render(askedBy(a, m.snap.Me.Handle))
		case a.From == r.addr:
			who = st.dim.Render("asked ") + m.who(b, a.To, st.name)
		default:
			continue
		}
		asked++
		out = append(out, st.dim.Render(g.aside)+" "+who+st.dim.Render(" "+ago(m.now().Sub(time.UnixMilli(a.At)))+" · "+asideWord(a)+" ")+
			st.aside.Render("“"+firstLine(a.Question)+"”"))
	}
	return out
}

// btwItems are this laptop's setting for teammates' questions to your agents, the one in force marked now; enter
// chooses one. Each names the command that does the same, murmur btw-answers.
func (m *model) btwItems() []item {
	now := cmp.Or(m.snap.BtwAnswers, ipc.BtwApprove)
	var out []item
	for _, o := range []struct{ v, text, done string }{
		{ipc.BtwApprove, "Each answer waits for you to share it", "Answers to teammates now wait for you to share them"},
		{ipc.BtwAuto, "Answers go out on their own", "Answers to teammates now go out on their own, but for cold questions"},
		{ipc.BtwAll, "Every answer goes out, idle agents' too", "Every answer to teammates now goes out on its own"},
		{ipc.BtwOff, "Teammates can't ask; you still can", "Teammates can no longer ask your agents by the way"},
	} {
		hint := "btw-answers " + o.v
		if o.v == now {
			hint = "now · " + hint
		}
		out = append(out, item{section: "TEAMMATES ASKING YOUR AGENTS BY THE WAY", text: o.text, hint: hint,
			run: func(m *model) tea.Cmd { return m.do(ipc.Request{Op: "btw-answers", Text: o.v}, o.done) }})
	}
	return out
}

// noticeAnswers says on the outcome line when an answer to your question by the way comes in, or won't; in the
// background it also asks the terminal for a notification.
func (m *model) noticeAnswers(old ipc.Snapshot) tea.Cmd {
	if len(m.snap.Asides) == 0 {
		return nil
	}
	final := map[string]bool{}
	for _, a := range old.Asides {
		if a.Final() {
			final[a.ID] = true
		}
	}
	var cmds []tea.Cmd
	for _, a := range m.snap.Asides { // your agents' answers go to them: the thread shows those
		if !a.Mine || byAgent(a) || !a.Final() || final[a.ID] {
			continue
		}
		if a.Answer != "" {
			m.outcome = outcome{text: a.To + " answered your question by the way", at: m.now()}
		} else {
			m.outcome = outcome{text: a.To + " couldn't answer by the way: " + a.Err, err: true, at: m.now()}
		}
		if m.blurred {
			cmds = append(cmds, notify(answerSentence(a)))
		}
	}
	return tea.Batch(cmds...)
}

// answerSentence says the answer to your question by the way, or to your agent's, or why there is none, for
// notifications and the plain mode: "bob's agent main-1 answered: …", "bob's agent main-1 answered your api-9b by the
// way: …".
func answerSentence(a ipc.Aside) string {
	me, asker, _ := strings.Cut(a.From, "/")
	who := a.To
	if person, name, ok := strings.Cut(a.To, "/"); ok {
		who = person + "'s agent " + name
		if person == me {
			who = "Your agent " + name
		}
	}
	switch {
	case a.Answer == "" && asker != "":
		return fmt.Sprintf("%s couldn't answer your %s's question by the way: %s", who, asker, a.Err)
	case a.Answer == "":
		return fmt.Sprintf("%s couldn't answer your question by the way: %s", who, a.Err)
	}
	s := who + " answered: " + oneLine(a.Answer)
	if asker != "" {
		s = who + " answered your " + asker + " by the way: " + oneLine(a.Answer)
	}
	if a.Tokens > 0 {
		s += fmt.Sprintf(" (%s tokens read)", tokens(a.Tokens))
	}
	return s
}

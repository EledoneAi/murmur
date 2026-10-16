// Package console is murmur's terminal console: the team's agents as a board, what needs you at the top, and the
// conversations between agents. It reads everything from the daemon, and asks it to send, mute and archive.
package console

import (
	"bufio"
	"encoding/json"
	"fmt"
	"image/color"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/release"
	"github.com/charmbracelet/x/ansi"
)

type (
	snapMsg ipc.Snapshot
	lostMsg struct{}
	tickMsg time.Time
	// doneMsg is the result of an action, for the outcome line.
	doneMsg struct {
		text string
		err  bool
	}
)

type outcome struct {
	text string
	err  bool
	at   time.Time
}

type model struct {
	snap ipc.Snapshot
	have bool
	w, h int
	st   styles
	g    glyphs
	demo bool // a made-up team, no daemon: murmur demo, the README's recording and the golden tests

	scope     scope
	scopeSet  bool            // t, m or a chose the scope; else the team's size does
	selected  string          // the selected item's key, which stays put while the board changes
	folded    map[string]bool // constellations folded by hand
	unfolded  map[string]bool // folds opened by hand: the quiet agents
	offset    int             // the first board line shown, when the board is taller than the screen
	outcome   outcome
	keys      []string        // selectable items, top to bottom, as last drawn
	inspect   bool            // the inspector is open
	thread    *thread         // the open conversation, over the board
	sheet     *sheet          // the open sheet, over everything
	filter    string          // what the board is filtered by
	pinned    map[string]bool // constellations pinned to the top
	compact   bool            // no blank line between constellations
	lastClick struct {
		at  time.Time
		key string
	}
	typing bool // keys go to the filter

	motion  bool        // animations play; off over SSH, with MURMUR_MOTION=0 or TERM=dumb
	anims   []anim      // the animations playing
	framing bool        // a frame is on its way
	blurred bool        // the terminal says the console is in the background
	bg      color.Color // the terminal's background, once it told us

	themeFixed bool // MURMUR_THEME chose the theme: the terminal's background doesn't
	clock      func() time.Time
	sub        chan tea.Msg
	exe        string      // the binary this console started from: an update replacing it restarts the console
	bin        os.FileInfo // that binary, to notice the replacement
	restart    bool
}

func newModel() *model {
	m := &model{st: newStyles(pickTheme(true)), g: pickGlyphs(), folded: map[string]bool{}, unfolded: map[string]bool{}, pinned: map[string]bool{},
		clock: time.Now, sub: make(chan tea.Msg, 16), themeFixed: os.Getenv("MURMUR_THEME") != "", motion: motionOn()}
	m.exe = config.Exe()
	m.bin, _ = os.Stat(m.exe)
	if from := os.Getenv("MURMUR_UPDATED_FROM"); from != "" {
		os.Unsetenv("MURMUR_UPDATED_FROM")
		m.outcome = outcome{text: "Updated " + from + " → " + release.Version, at: time.Now()}
	}
	return m
}

func (m *model) now() time.Time { return m.clock() }

// Run opens the console on this laptop's daemon, starting it when it isn't running.
func Run() error {
	if err := ipc.EnsureDaemon(); err != nil {
		return err
	}
	m := newModel()
	go m.listen()
	if _, err := tea.NewProgram(m).Run(); err != nil || !m.restart {
		return err
	}
	os.Setenv("MURMUR_UPDATED_FROM", release.Version)
	return syscall.Exec(m.exe, os.Args, os.Environ())
}

// listen keeps a subscription to the daemon open, restarting the daemon if it went away (an update, a crash), from
// the binary at this console's path.
func (m *model) listen() {
	for {
		_ = ipc.EnsureDaemon()
		if c, err := ipc.Dial(); err == nil {
			_ = json.NewEncoder(c).Encode(ipc.Request{Op: "sub"})
			r := bufio.NewReaderSize(c, 1<<20)
			for {
				data, err := r.ReadBytes('\n')
				if err != nil {
					break
				}
				var s ipc.Snapshot
				if json.Unmarshal(data, &s) == nil {
					m.sub <- snapMsg(s)
				}
			}
			c.Close()
		}
		m.sub <- lostMsg{}
		time.Sleep(500 * time.Millisecond)
	}
}

func (m *model) wait() tea.Cmd { return func() tea.Msg { return <-m.sub } }

// tick redraws every second while a clock runs on screen, else twice a minute for the idle times.
func (m *model) tick() tea.Cmd {
	every := 30 * time.Second
	if b := m.board(); len(b.needs) > 0 || slices.ContainsFunc(b.all(), func(r *agentRef) bool { return r.st.waiting() }) {
		every = time.Second
	}
	return tea.Tick(every, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.wait(), m.tick(), tea.RequestBackgroundColor, m.play(animDraw, ""))
}

func (m *model) board() *board {
	b := buildBoard(m.snap, m.now(), m.currentScope())
	b.filterBy(m.filter)
	return b
}

// currentScope is the scope chosen, when the team's size offers it, or the default for that size.
func (m *model) currentScope() scope {
	people := map[string]bool{m.snap.Me.Handle: true}
	for _, p := range m.snap.People {
		if len(p.Agents) > 0 {
			people[p.Handle] = true
		}
	}
	offered := scopes(len(people))
	if m.scopeSet && slices.Contains(offered, m.scope) {
		return m.scope
	}
	return offered[0]
}

// call talks to the daemon, or pretends to in the demo.
func (m *model) call(req ipc.Request, timeout time.Duration) (ipc.Reply, error) {
	var r ipc.Reply
	if m.demo {
		return ipc.Reply{OK: true, Note: "delivered"}, nil
	}
	err := ipc.Call(req, &r, timeout)
	return r, err
}

// do runs an action against the daemon in the background, and reports its outcome.
func (m *model) do(req ipc.Request, ok string) tea.Cmd {
	return func() tea.Msg {
		r, err := m.call(req, 10*time.Second)
		switch {
		case err != nil:
			return doneMsg{text: err.Error(), err: true}
		case !r.OK:
			return doneMsg{text: r.Err, err: true}
		}
		return doneMsg{text: ok}
	}
}

// ---------------------------------------------------------------- update

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.resizeInput()
	case tea.FocusMsg:
		m.blurred = false
	case tea.BlurMsg:
		m.blurred = true
	case frameMsg:
		return m, m.onFrame()
	case tea.BackgroundColorMsg:
		m.bg = msg.Color
		if !m.themeFixed {
			m.st = newStyles(pickTheme(msg.IsDark()))
		}
	case snapMsg:
		changed := m.noticeChanges(m.snap, ipc.Snapshot(msg))
		m.snap, m.have = ipc.Snapshot(msg), true
		if m.replaced() {
			m.restart = true
			return m, tea.Quit
		}
		return m, tea.Batch(m.wait(), changed)
	case lostMsg:
		m.snap.State = "reconnecting"
		return m, m.wait()
	case tickMsg:
		return m, m.tick()
	case doneMsg:
		m.outcome = outcome{text: msg.text, err: msg.err, at: m.now()}
	case tea.KeyPressMsg:
		b := m.board()
		switch {
		case m.sheet != nil:
			return m, m.sheetKey(msg)
		case m.typing:
			return m, m.filterKey(msg)
		case m.thread != nil:
			return m, m.threadKey(b, msg)
		}
		return m.key(msg)
	case tea.PasteMsg:
		if m.thread != nil && m.thread.writing {
			var cmd tea.Cmd
			m.thread.input, cmd = m.thread.input.Update(msg)
			m.resizeInput()
			return m, cmd
		}
	case tea.MouseClickMsg:
		return m, m.click(msg.Mouse())
	case tea.MouseWheelMsg:
		if msg.Mouse().Button == tea.MouseWheelUp {
			m.move(-1)
		} else {
			m.move(1)
		}
	}
	return m, nil
}

// replaced says whether an update replaced this console's binary while the daemon moved to another version: the
// console restarts into it. Comparing files, not versions, never loops when the console is the newer one.
func (m *model) replaced() bool {
	if m.snap.Version == release.Version || m.snap.Version == "" || m.bin == nil {
		return false
	}
	st, err := os.Stat(m.exe)
	return err == nil && !os.SameFile(st, m.bin)
}

func (m *model) key(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.outcome.err { // an error stays until the next action
		m.outcome = outcome{}
	}
	b := m.board()
	switch k.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "esc":
		switch {
		case m.inspect:
			m.inspect = false
		case m.filter != "":
			m.filter = ""
		}
	case "space":
		m.inspect = !m.inspect
	case "enter":
		if n, ok := m.selectedNeed(b); ok && n.kind == needFailed {
			return m, m.do(ipc.Request{Op: "send", To: n.msg.To, Text: n.msg.Text}, "Sent again to "+n.msg.To)
		}
		if m.selected == "fold:quiet" {
			m.unfolded["quiet"] = !m.unfolded["quiet"]
			return m, nil
		}
		return m, m.openThread(b, false)
	case "w", "r":
		return m, m.openThread(b, true)
	case "t", "m", "a": // the scope tabs; in a large team m is Near me
		for _, sc := range scopes(len(b.colours)) {
			if scopeKeys[sc] == k.String() {
				m.scope, m.scopeSet = sc, true
			}
		}
	case ":", "ctrl+k":
		m.openSheet(sheetPalette, b)
	case "?":
		m.openSheet(sheetHelp, b)
	case "p":
		if !b.solo {
			m.openSheet(sheetPeople, b)
		}
	case "z":
		m.openSheet(sheetMute, b)
	case "/":
		m.typing = true
	case "g":
		if r := m.selectedAgent(b); r != nil && r.mine {
			return m, m.goTo(r)
		}
	case "N":
		if r := m.selectedAgent(b); r != nil && r.st == waitsOwner {
			return m, m.nudge(r)
		}
	case "*":
		if gr := m.groupAround(b); gr != nil {
			m.pinned[gr.key] = !m.pinned[gr.key]
			word := "Unpinned "
			if m.pinned[gr.key] {
				word = "Pinned "
			}
			m.outcome = outcome{text: word + gr.name, at: m.now()}
		}
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "home":
		m.jump(0)
	case "end":
		m.jump(len(m.keys) - 1)
	case "n":
		m.nextNeed(b)
	case "[", "]":
		m.nextGroup(k.String() == "]")
	case "left", "h":
		m.fold(b, true)
	case "right", "l":
		m.fold(b, false)
	case "c":
		if addr := m.selectedAddr(b); addr != "" {
			m.outcome = outcome{text: "Copied " + addr, at: m.now()}
			return m, tea.SetClipboard(addr)
		}
	case "o":
		if r := m.selectedAgent(b); r != nil && r.a.PRURL != "" {
			return m, openURL(r.a.PRURL, fmt.Sprintf("Opened #%d", r.a.PR))
		}
	case "i":
		if d, ok := m.selectedDissonance(b); ok {
			return m, m.introduce(d)
		}
	case "y":
		if n, ok := m.selectedNeed(b); ok && n.kind == needHeld {
			return m, m.do(ipc.Request{Op: "approve", ID: n.msg.ID}, "Let through to "+n.to)
		}
	case "x":
		if n, ok := m.selectedNeed(b); ok {
			switch n.kind {
			case needHeld:
				return m, m.do(ipc.Request{Op: "decline", ID: n.msg.ID}, "Dropped; "+n.who+" is told")
			case needQuestion, needFailed:
				return m, m.do(ipc.Request{Op: "done", ID: n.msg.ID}, "Marked done")
			}
		}
	}
	return m, nil
}

// filterKey takes the keys while the filter is typed: the board narrows as you type, keeping its structure.
func (m *model) filterKey(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "enter":
		m.typing = false
	case "esc":
		m.typing, m.filter = false, ""
	case "backspace":
		if r := []rune(m.filter); len(r) > 0 {
			m.filter = string(r[:len(r)-1])
		}
	case "ctrl+c":
		return tea.Quit
	default:
		if k.Text != "" {
			m.filter += k.Text
		}
	}
	return nil
}

// ---------------------------------------------------------------- selection

func (m *model) index() int { return slices.Index(m.keys, m.selected) }

func (m *model) move(d int) {
	if len(m.keys) == 0 {
		return
	}
	i := m.index()
	if i < 0 {
		m.jump(0)
		return
	}
	m.jump(max(0, min(len(m.keys)-1, i+d)))
}

func (m *model) jump(i int) {
	if i >= 0 && i < len(m.keys) {
		m.selected = m.keys[i]
	}
}

// nextNeed goes to the next thing that needs you, from anywhere, round to the first.
func (m *model) nextNeed(b *board) {
	if len(b.needs) == 0 {
		return
	}
	i := slices.IndexFunc(b.needs, func(n need) bool { return n.key == m.selected })
	m.selected = b.needs[(i+1)%len(b.needs)].key
}

func (m *model) nextGroup(forward bool) {
	var heads []int
	for i, k := range m.keys {
		if strings.HasPrefix(k, "group:") {
			heads = append(heads, i)
		}
	}
	if len(heads) == 0 {
		return
	}
	i := m.index()
	if forward {
		for _, h := range heads {
			if h > i {
				m.jump(h)
				return
			}
		}
		m.jump(heads[0])
		return
	}
	for j := len(heads) - 1; j >= 0; j-- {
		if heads[j] < i {
			m.jump(heads[j])
			return
		}
	}
	m.jump(heads[len(heads)-1])
}

// fold folds or unfolds what the selection is in: its constellation, or the quiet agents.
func (m *model) fold(b *board, fold bool) {
	if m.selected == "fold:quiet" || (strings.HasPrefix(m.selected, "agent:") && slices.ContainsFunc(b.quiet, func(r *agentRef) bool { return "agent:"+r.addr == m.selected })) {
		m.unfolded["quiet"] = !fold
		m.selected = "fold:quiet"
		return
	}
	in := func(g *group) bool {
		return m.selected == "group:"+g.key || slices.ContainsFunc(g.members, func(r *agentRef) bool { return "agent:"+r.addr == m.selected })
	}
	for _, g := range b.elsewhere { // folded unless opened by hand
		if in(g) {
			m.unfolded[g.key] = !fold
			m.selected = "group:" + g.key
			return
		}
	}
	for _, g := range b.groups {
		if in(g) {
			m.folded[g.key] = fold
			if fold {
				m.selected = "group:" + g.key
			}
			return
		}
	}
}

func (m *model) selectedAgent(b *board) *agentRef {
	addr, ok := strings.CutPrefix(m.selected, "agent:")
	if !ok {
		if n, ok := m.selectedNeed(b); ok {
			return n.ref
		}
		return nil
	}
	for _, r := range b.all() {
		if r.addr == addr {
			return r
		}
	}
	return nil
}

func (m *model) selectedNeed(b *board) (need, bool) {
	for _, n := range b.needs {
		if n.key == m.selected {
			return n, true
		}
	}
	return need{}, false
}

func (m *model) selectedDissonance(b *board) (dissonance, bool) {
	for _, d := range b.dissonant {
		if m.selected == "dis:"+d.mine.addr+"|"+d.other {
			return d, true
		}
	}
	return dissonance{}, false
}

// selectedAddr is the address of what is selected, to paste into a prompt.
func (m *model) selectedAddr(b *board) string {
	if r := m.selectedAgent(b); r != nil {
		return r.addr
	}
	if n, ok := m.selectedNeed(b); ok {
		return n.who
	}
	return ""
}

// introduce tells two agents changing the same file, who never talked, about each other: one message to each.
func (m *model) introduce(d dissonance) tea.Cmd {
	file := d.file
	if d.more > 0 {
		file += fmt.Sprintf(" and %d more files", d.more)
	}
	toMine := fmt.Sprintf("%s also changes %s. Talk with it (send, to %s) before your changes collide.", d.other, file, d.other)
	toOther := fmt.Sprintf("%s also changes %s. Talk with it (send, to %s) before your changes collide.", d.mine.addr, file, d.mine.addr)
	return func() tea.Msg {
		for _, req := range []ipc.Request{{Op: "send", To: d.mine.addr, Text: toMine}, {Op: "send", To: d.other, Text: toOther}} {
			r, err := m.call(req, 10*time.Second)
			if err != nil {
				return doneMsg{text: err.Error(), err: true}
			}
			if !r.OK {
				return doneMsg{text: r.Err, err: true}
			}
		}
		return doneMsg{text: "Introduced " + d.mine.a.Name + " and " + d.other}
	}
}

func openURL(url, ok string) tea.Cmd {
	return func() tea.Msg {
		name := "xdg-open"
		if runtime.GOOS == "darwin" {
			name = "open"
		}
		if err := exec.Command(name, url).Start(); err != nil {
			return doneMsg{text: "Couldn't open " + url + ": " + err.Error(), err: true}
		}
		return doneMsg{text: ok}
	}
}

// click selects the row under the pointer; on an owner's initial it filters the board to that person, and a
// second click on the same row opens it.
func (m *model) click(at tea.Mouse) tea.Cmd {
	key := m.keyAt(at.Y)
	if key == "" {
		return nil
	}
	b := m.board()
	double := m.lastClick.key == key && m.now().Sub(m.lastClick.at) < 400*time.Millisecond
	m.lastClick.at, m.lastClick.key = m.now(), key
	m.selected = key
	if r := m.selectedAgent(b); r != nil && !b.solo && strings.HasPrefix(key, "agent:") {
		if x := 1 + gutterW + 2; at.X >= x && at.X < x+cellWidth(b.initials[r.person]) {
			m.filter = r.person
			return nil
		}
	}
	if double {
		return m.openThread(b, false)
	}
	return nil
}

// nudge tells a teammate that one of their agents has been waiting for them.
func (m *model) nudge(r *agentRef) tea.Cmd {
	text := fmt.Sprintf("Your agent %s has been waiting for you for %s.", r.addr, spoken(r.idleFor(m.now())))
	return m.do(ipc.Request{Op: "send", To: r.person, Text: text}, "Nudged "+r.person)
}

// groupAround is the constellation the selection is in, or is.
func (m *model) groupAround(b *board) *group {
	if gr := m.selectedGroup(b); gr != nil {
		return gr
	}
	if r := m.selectedAgent(b); r != nil {
		return b.groupOf(r)
	}
	return nil
}

// ---------------------------------------------------------------- view

// keyAt is the item under line y of the screen, for the mouse.
func (m *model) keyAt(y int) string {
	_, lines := m.screen()
	if y >= 0 && y < len(lines) {
		return lines[y].key
	}
	return ""
}

func (m *model) View() tea.View {
	text, lines := m.screen()
	if m.sheet != nil && m.w > 0 {
		text = m.overlay(text)
	}
	v := tea.NewView(text)
	v.AltScreen, v.ReportFocus = true, true
	v.MouseMode = tea.MouseModeCellMotion
	b := m.board()
	v.WindowTitle = "murmur"
	if n := len(b.needs); n > 0 {
		v.WindowTitle = fmt.Sprintf("murmur · %d %s you", n, plural(n, "needs", "need"))
		v.ProgramStatus = &tea.ProgramStatus{State: tea.ProgramStateBlocked}
	}
	// the terminal's own cursor sits on the selected item, so magnifiers and screen readers follow the selection
	if y := slices.IndexFunc(lines, func(l line) bool { return l.key != "" && l.key == m.selected }); y >= 0 {
		c := tea.NewCursor(0, y)
		c.Shape, c.Blink = tea.CursorBar, false
		v.Cursor = c
	}
	return v
}

// screen draws the whole screen: exactly h lines of w cells.
func (m *model) screen() (string, []line) {
	if m.w == 0 || m.h == 0 {
		return "", nil
	}
	b := m.board()
	head := m.header(b)
	if m.thread != nil {
		return m.threadView(b, head)
	}
	pinned, body := m.homeLines(b)

	// the selection survives a new snapshot when its item is still there; else it moves to the first item
	m.keys = m.keys[:0]
	for _, l := range append(slices.Clone(pinned), body...) {
		if l.key != "" && (len(m.keys) == 0 || m.keys[len(m.keys)-1] != l.key) {
			m.keys = append(m.keys, l.key)
		}
	}
	if !slices.Contains(m.keys, m.selected) && len(m.keys) > 0 {
		m.selected = m.keys[0]
		pinned, body = m.homeLines(b) // draw the new selection's detail line
	}

	room := m.h - len(head) - 1 - len(pinned) - 3 // a blank line under the header; the outcome, a blank, the footer
	if room < 1 {
		pinned = pinned[:max(0, min(len(pinned), m.h-len(head)-4))]
		room = max(0, m.h-len(head)-1-len(pinned)-3)
	}
	if i := slices.IndexFunc(body, func(l line) bool { return l.key == m.selected }); i >= 0 {
		last := i
		for last+1 < len(body) && body[last+1].key == m.selected {
			last++
		}
		if i < m.offset {
			m.offset = i
		}
		if last >= m.offset+room {
			m.offset = last - room + 1
		}
	}
	m.offset = max(0, min(m.offset, len(body)-room))
	shown := body[m.offset:min(len(body), m.offset+room)]
	if m.inspect {
		shown = m.withInspector(b, pinned, body, room)
		if m.w < inspectorMin {
			pinned = nil
		}
	}

	var lines []line
	for _, h := range head {
		lines = append(lines, line{text: h})
	}
	for _, sc := range m.tabs(b) { // the tabs carry their keys
		lines[len(lines)-1].hot = append(lines[len(lines)-1].hot, scopeKeys[sc])
	}
	lines = append(lines, line{text: pad("", m.w)})
	lines = append(lines, pinned...)
	lines = append(lines, shown...)
	for len(lines) < m.h-3 {
		lines = append(lines, line{text: pad("", m.w)})
	}
	footer := m.footer(m.footerKeys(b), hotIn(lines[:max(0, min(len(lines), m.h-3))]))
	lines = append(lines, line{text: m.outcomeLine()}, line{text: pad("", m.w)}, line{text: footer})
	lines = lines[:min(len(lines), m.h)]

	var out strings.Builder
	for i, l := range lines {
		text := l.text
		if l.key != "" && l.key == m.selected {
			text = m.st.bold.Render(m.g.sel) + ansi.TruncateLeft(text, 1, "")
			text = tinted(text, m.st.selBg)
		}
		out.WriteString(text)
		if i < len(lines)-1 {
			out.WriteByte('\n')
		}
	}
	return out.String(), lines
}

// threadView is the open thread under the header, with its keys at the bottom.
func (m *model) threadView(b *board, head []string) (string, []line) {
	st := m.st
	head = head[:len(head)-1]
	title := " " + st.dim.Render(scopeNames[m.currentScope()]+" › ") + st.bold.Render(m.thread.name)
	if len(head) == 0 { // narrow: one header row
		head = []string{spread(title, "", m.w)}
	} else {
		head = append(head, spread(m.logoLamps(b)+"  "+st.wordmark.Render("murmur")+"  "+title, st.dim.Render("seen only by the people on each end")+"  ", m.w))
	}
	var lines []line
	for _, h := range head {
		lines = append(lines, line{text: h})
	}
	lines = append(lines, line{text: pad("", m.w)})
	room := m.h - len(lines) - 3
	lines = append(lines, m.threadScreen(b, room)...)
	keys := []string{"w", "write", "⇥", "recipient", "[ ]", "constellation", "x", "done", "esc", "back"}
	if m.thread.writing {
		keys = []string{"⏎", "send", "⇥", "recipient", "esc", "stop writing"}
	} else if _, held := m.threadMsgs(); len(held) > 0 { // x drops the held message; its line shows y and x
		keys = []string{"y", "let through", "x", "drop", "w", "write", "⇥", "recipient", "[ ]", "constellation", "esc", "back"}
	}
	footer := m.footer(keys, hotIn(lines))
	lines = append(lines, line{text: m.outcomeLine()}, line{text: pad("", m.w)}, line{text: footer})
	lines = lines[:min(len(lines), m.h)]
	var out []string
	for _, l := range lines {
		out = append(out, l.text)
	}
	return strings.Join(out, "\n"), lines
}

// withInspector lays the inspector beside a narrowed board, or over the whole body on a narrow terminal.
func (m *model) withInspector(b *board, pinned, body []line, room int) []line {
	if m.w < inspectorMin {
		panel := m.inspectorLines(b, m.w)
		var out []line
		for i := range room + len(pinned) {
			l := line{text: pad("", m.w)}
			if i < len(panel) {
				l = panel[i]
			}
			out = append(out, l)
		}
		return out
	}
	w := m.w
	m.w = sideBoard
	_, left := m.homeLines(b)
	m.w = w
	i := slices.IndexFunc(left, func(l line) bool { return l.key == m.selected })
	start := max(0, min(i-room/2, len(left)-room))
	panel := m.inspectorLines(b, m.w-sideBoard-3)
	var out []line
	for r := range room {
		l := line{text: pad("", sideBoard)}
		if start+r < len(left) && start+r >= 0 {
			l = left[start+r]
		}
		right := line{text: pad("", m.w-sideBoard-3)}
		if r < len(panel) {
			right = panel[r]
		}
		out = append(out, line{text: pad(l.text, sideBoard) + " " + m.st.faint.Render(m.g.v) + " " + right.text, key: l.key,
			hot: append(slices.Clone(l.hot), right.hot...)})
	}
	return out
}

// footerKeys are the keys that act where the focus is, in pairs of a key and a word: what only this selection does
// first, then what any agent's row does. The footer leaves out those the screen shows already: the scope tabs, n
// beside Needs you, i on a dissonance, the inspector's actions, a held message's y and x.
func (m *model) footerKeys(b *board) []string {
	if m.typing {
		return []string{"/", m.filter + "▏", "⏎", "keep", "esc", "clear"}
	}
	var keys []string
	if m.filter != "" {
		keys = append(keys, "/", "filter: "+m.filter, "esc", "clear")
	}
	if len(b.needs) > 0 {
		keys = append(keys, "n", "next need")
	}
	if _, ok := m.selectedDissonance(b); ok {
		return append(keys, "i", "introduce")
	}
	inspect := "inspect"
	if m.inspect {
		inspect = "close"
	}
	if gr := m.selectedGroup(b); gr != nil {
		fold, word := "←", "fold"
		if m.folded[gr.key] || (slices.Contains(b.elsewhere, gr) && !m.unfolded[gr.key]) {
			fold, word = "→", "unfold"
		}
		pin := "pin"
		if m.pinned[gr.key] {
			pin = "unpin"
		}
		return append(keys, "⏎", "thread", "␣", inspect, fold, word, "*", pin)
	}
	if m.selected == "fold:quiet" {
		if m.unfolded["quiet"] {
			return append(keys, "⏎", "fold")
		}
		return append(keys, "⏎", "unfold")
	}
	n, isNeed := m.selectedNeed(b)
	r := m.selectedAgent(b)
	switch {
	case isNeed && n.kind == needFailed:
		return append(keys, "⏎", "send again", "x", "done", "c", "copy")
	case isNeed && n.kind == needHeld:
		keys = append(keys, "y", "let through", "x", "drop")
	case isNeed && n.kind == needQuestion:
		keys = append(keys, "r", "reply", "x", "done")
	case r != nil && r.mine && r.st == needsYou:
		keys = append(keys, "g", "terminal")
	case r != nil && r.st == waitsOwner:
		keys = append(keys, "N", "nudge "+r.person)
	}
	if r == nil {
		if isNeed { // a person's question
			keys = append(keys, "⏎", "thread", "c", "copy")
		}
		return keys
	}
	keys = append(keys, "⏎", "thread", "␣", inspect)
	if !isNeed || n.kind != needQuestion {
		keys = append(keys, "w", "write")
	}
	keys = append(keys, "c", "copy", "z", "mute")
	if r.mine && r.st != needsYou {
		keys = append(keys, "g", "terminal")
	}
	if r.a.PRURL != "" {
		keys = append(keys, "o", fmt.Sprintf("open #%d", r.a.PR))
	}
	return keys
}

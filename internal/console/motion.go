package console

import (
	"image/color"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
)

// Motion answers "what just changed, and where?": the m draws itself when murmur starts, a light crosses it when a
// thought is sent anywhere in the team, a new need glows in its owner's colour as it arrives, and a lamp that
// receives a message brightens. Animations render at up to 30 frames a second while they play; at rest the console
// redraws only when something changes. Nothing loops and nothing flashes. Motion is off with MURMUR_MOTION=0, over
// SSH and with TERM=dumb; then every change has a still version: the m appears, the lamp is bold for two seconds,
// the new need is marked new.

type animKind int

const (
	animDraw  animKind = iota // the m draws itself, from the left foot to the right
	animLight                 // a light crosses the m
	animGlow                  // a new need glows, then fades
	animBold                  // a lamp that received a message
)

var animFor = [...]time.Duration{animDraw: 900 * time.Millisecond, animLight: 350 * time.Millisecond,
	animGlow: 1400 * time.Millisecond, animBold: 2 * time.Second}

type anim struct {
	kind  animKind
	key   string // the need or the agent it plays on
	start time.Time
}

type frameMsg time.Time

func motionOn() bool {
	return os.Getenv("MURMUR_MOTION") != "0" && os.Getenv("SSH_CONNECTION") == "" && os.Getenv("TERM") != "dumb"
}

// progress is how far an animation has played, from 0 to 1, or -1 when it is over.
func (m *model) progress(kind animKind, key string) float64 {
	now := m.now()
	for _, a := range m.anims {
		if a.kind == kind && a.key == key {
			p := float64(now.Sub(a.start)) / float64(animFor[a.kind])
			if p >= 0 && p < 1 {
				return p
			}
		}
	}
	return -1
}

func (m *model) play(kind animKind, key string) tea.Cmd {
	if !m.motion && kind != animBold && kind != animGlow {
		return nil
	}
	m.anims = append(m.anims, anim{kind: kind, key: key, start: m.now()})
	return m.frame()
}

// frame asks for the next frame while an animation plays.
func (m *model) frame() tea.Cmd {
	if m.framing {
		return nil
	}
	m.framing = true
	every := 33 * time.Millisecond
	if !m.motion {
		every = 500 * time.Millisecond // only to end the still versions on time
	}
	return tea.Tick(every, func(t time.Time) tea.Msg { return frameMsg(t) })
}

// onFrame drops the animations that ended, and asks for another frame if any still plays.
func (m *model) onFrame() tea.Cmd {
	m.framing = false
	now := m.now()
	live := m.anims[:0]
	for _, a := range m.anims {
		if now.Sub(a.start) < animFor[a.kind] {
			live = append(live, a)
		}
	}
	m.anims = live
	if len(live) == 0 {
		return nil
	}
	return m.frame()
}

// noticeChanges plays what the new snapshot changed since old, whose needs were before: messages sent, messages
// received, new needs. In the background it also tells the terminal, which can show a notification.
func (m *model) noticeChanges(old ipc.Snapshot, before []need) tea.Cmd {
	if !m.have {
		return nil
	}
	now := m.snap
	seen := map[string]bool{}
	for _, x := range old.Msgs {
		seen[x.ID] = true
	}
	for _, x := range old.Asks {
		seen[x.ID] = true
	}
	var cmds []tea.Cmd
	sent := false
	for _, x := range append(now.Msgs, now.Asks...) {
		if seen[x.ID] {
			continue
		}
		sent = true
		cmds = append(cmds, m.play(animBold, x.To))
	}
	if sent {
		cmds = append(cmds, m.play(animLight, ""))
	}
	had := map[string]bool{}
	for _, n := range before {
		had[n.key] = true
	}
	for _, n := range m.board().needs {
		if had[n.key] {
			continue
		}
		cmds = append(cmds, m.play(animGlow, n.key))
		if n.kind == needApprove && os.Getenv("MURMUR_BELL") == "1" { // one of your agents starts waiting for you
			cmds = append(cmds, tea.Raw("\a"))
		}
		if m.blurred {
			cmds = append(cmds, notify(needSentence(n)))
		}
	}
	return tea.Batch(cmds...)
}

// notify asks the terminal for a desktop notification (OSC 9), which iTerm2, kitty, Ghostty and WezTerm show.
func notify(text string) tea.Cmd {
	text = strings.NewReplacer("\x1b", "", "\x07", "", "\n", " ").Replace(text)
	return tea.Raw("\x1b]9;murmur: " + text + "\x07")
}

// needSentence says a need in words, for notifications and the plain mode.
func needSentence(n need) string {
	switch n.kind {
	case needApprove:
		return n.who + " needs you: " + n.text
	case needQuestion:
		return n.who + " asks you: " + n.text
	case needHeld:
		return "a mute holds a message from " + n.who + " to " + n.to
	}
	return "not delivered to " + n.who + ": " + n.why
}

// ---------------------------------------------------------------- what motion draws

// logo is the m's line and lamps as an animation leaves them.
func (m *model) animatedLogo(b *board, line, lamps string) (string, string) {
	st, g := m.st, m.g
	if !m.live() {
		return line, lamps
	}
	cells := []rune(g.logoLine)
	lit := func(i int) string { return st.fg.Render(string(cells[i])) }
	lampAt := map[int]int{0: 0, 2: 1, 4: 2}
	lamp := func(i int, on bool) string {
		c := st.fg
		if i == 1 && len(b.needs) > 0 {
			c = st.teal
		}
		if !on {
			return st.faint.Render(g.idle)
		}
		return c.Render(g.working)
	}
	if p := m.progress(animDraw, ""); p >= 0 {
		shown := int(p * float64(len(cells)+1))
		var l, ls strings.Builder
		for i := range cells {
			if i < shown {
				l.WriteString(lit(i))
			} else {
				l.WriteString(" ")
			}
		}
		for i := range 3 {
			if i > 0 {
				ls.WriteString(" ")
			}
			ls.WriteString(lamp(i, shown > 2*i))
		}
		return " " + l.String(), " " + ls.String()
	}
	if p := m.progress(animLight, ""); p >= 0 {
		at := int(p * float64(len(cells)))
		var l, ls strings.Builder
		for i := range cells {
			if i == at {
				l.WriteString(st.bold.Render(string(cells[i])))
			} else {
				l.WriteString(lit(i))
			}
		}
		for i := range 3 {
			if i > 0 {
				ls.WriteString(" ")
			}
			s := lamp(i, true)
			if j, ok := lampAt[at]; ok && j == i {
				s = st.bold.Render(g.working)
			}
			ls.WriteString(s)
		}
		return " " + l.String(), " " + ls.String()
	}
	return line, lamps
}

// glowing tints a new need's row in its owner's colour, fading in four steps; without motion it says "new".
func (m *model) glowing(key string, text string, owner int) string {
	p := m.progress(animGlow, key)
	if p < 0 {
		return text
	}
	if !m.motion {
		return text // the row carries "new" instead; see needLine
	}
	steps := lipgloss.Blend1D(5, m.st.t.people[owner%len(m.st.t.people)], m.background())
	return tinted(text, tint(steps[min(4, 1+int(p*4))]))
}

// background is the terminal's background as it told us, or the theme's own guess.
func (m *model) background() color.Color {
	if m.bg != nil {
		return m.bg
	}
	if m.st.t.name == "light" {
		return hex("#fafaf8")
	}
	return hex("#0f1017")
}

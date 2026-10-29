package console

import (
	"cmp"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/charmbracelet/x/ansi"
)

var rewrite = flag.Bool("update", false, "rewrite the golden files in testdata")

// demo is the console on the demo team at demoNow, w by h, in a theme.
func demo(w, h int, t theme) *model {
	m := newModel()
	m.demo, m.have, m.w, m.h = true, true, w, h
	m.setSnap(demoSnapshot(demoNow))
	m.clock = func() time.Time { return demoNow }
	m.st, m.g = newStyles(t), unicodeGlyphs
	m.outcome, m.motion = outcome{}, false
	return m
}

func press(m *model, keys ...string) {
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "up":
			msg = tea.KeyPressMsg{Code: tea.KeyUp}
		case "left":
			msg = tea.KeyPressMsg{Code: tea.KeyLeft}
		case "right":
			msg = tea.KeyPressMsg{Code: tea.KeyRight}
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		default:
			msg = tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
		}
		m.Update(msg)
		m.screen()
	}
}

// The home screen at four widths, in both themes, compared with testdata: the plain text in .txt files, the colours
// in .ansi files. After a change on purpose, run go test ./internal/console -update and read the diff.
func TestGolden(t *testing.T) {
	for _, w := range []int{60, 84, 110, 160} {
		for _, th := range []theme{dark, light} {
			m := demo(w, 32, th)
			press(m, "down", "down", "down", "down", "down", "down") // the first board row, with its detail line
			text, _ := m.screen()
			golden(t, fmt.Sprintf("home-%d-%s.ansi", w, th.name), text)
			if th.name == "dark" {
				golden(t, fmt.Sprintf("home-%d.txt", w), ansi.Strip(text))
			}
		}
	}
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *rewrite {
		_ = os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v: run go test ./internal/console -update", err)
	}
	if got != string(want) {
		t.Errorf("%s changed:\n%s", path, ansi.Strip(got))
	}
}

// Every screen fills the terminal exactly, at any size, whatever is selected, a thread too.
func TestScreenFits(t *testing.T) {
	for _, size := range [][2]int{{40, 10}, {60, 24}, {84, 30}, {110, 12}, {160, 50}, {200, 8}, {84, 5}} {
		w, h := size[0], size[1]
		m := demo(w, h, dark)
		fits := func(where string) {
			t.Helper()
			text, _ := m.screen()
			lines := strings.Split(text, "\n")
			if len(lines) != h {
				t.Fatalf("%dx%d, %s: %d lines", w, h, where, len(lines))
			}
			for _, l := range lines {
				if ansi.StringWidth(l) != w {
					t.Fatalf("%dx%d, %s: a line is %d wide: %q", w, h, where, ansi.StringWidth(l), ansi.Strip(l))
				}
			}
		}
		for range 12 {
			fits("on " + m.selected)
			press(m, "down")
		}
		m.selected = "agent:alice/api-9b"
		press(m, "enter")
		fits("in a thread")
	}
}

// What needs you comes first, oldest first, and n goes through it from anywhere.
func TestNeeds(t *testing.T) {
	b := buildBoard(demoSnapshot(demoNow), demoNow, scopeTeam)
	var got []string
	for _, n := range b.needs {
		got = append(got, n.key)
	}
	want := []string{"need:agent:alice/api-1a", "need:btw:w2", "need:msg:m6", "need:held:h1"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("needs %v, want %v", got, want)
	}
	m := demo(110, 30, dark)
	m.screen()
	press(m, "end", "n")
	if m.selected != want[0] {
		t.Fatalf("n went to %q", m.selected)
	}
	press(m, "n", "n", "n", "n")
	if m.selected != want[0] {
		t.Fatalf("n doesn't go round: %q", m.selected)
	}
}

// A message the hub refused is one need, with murmur's note about it in the history, and marking it done clears it.
func TestFailedMessageIsOneNeed(t *testing.T) {
	s := ipc.Snapshot{Me: protocol.Person{Handle: "alice"},
		Msgs:   []protocol.Msg{{ID: "nd-m1", From: protocol.System, To: "alice", Text: "not delivered to bob/b1: offline", At: 2}},
		Outbox: []protocol.Msg{{ID: "m1", From: "alice", To: "bob/b1", Text: "hi", At: 1}},
		Failed: map[string]string{"m1": "bob/b1 is not running"}}
	if n := buildNeeds(s, nil, nil, demoNow); len(n) != 1 || n[0].kind != needFailed {
		t.Fatalf("needs: %+v", n)
	}
	s.Done = map[string]bool{"m1": true}
	if n := buildNeeds(s, nil, nil, demoNow); len(n) != 0 {
		t.Fatalf("marked done, still needs: %+v", n)
	}
}

// Agents in touch form constellations; an overlap inside one is in tune, and two agents changing the same file
// without talking are out of tune.
func TestConstellations(t *testing.T) {
	b := buildBoard(demoSnapshot(demoNow), demoNow, scopeTeam)
	if len(b.groups) != 2 {
		t.Fatalf("%d constellations, want 2", len(b.groups))
	}
	billing := b.groups[0]
	var members []string
	for _, r := range billing.members {
		members = append(members, r.addr)
	}
	if got := strings.Join(members, " "); got != "alice/api-1a alice/api-9b bob/main-1 carol/billing-2" {
		t.Fatalf("billing has %s", got)
	}
	if billing.tune.file != "billing/certifier.go" || billing.tune.off || billing.needs != 1 {
		t.Fatalf("billing's harmony: %+v, needs %d", billing.tune, billing.needs)
	}
	held := false
	for _, l := range billing.links {
		held = held || l.held
	}
	if !held {
		t.Fatal("the held message from bob/main-1 draws no dashed link")
	}
	if len(b.dissonant) != 1 || b.dissonant[0].other != "dave/infra-1" || b.dissonant[0].file != "main.tf" {
		t.Fatalf("dissonances: %+v", b.dissonant)
	}
	if len(b.quiet) != 1 || b.quiet[0].addr != "alice/docs-2" {
		t.Fatalf("quiet: %v", b.quiet)
	}
	mine := buildBoard(demoSnapshot(demoNow), demoNow, scopeMine)
	for _, r := range mine.lone {
		if !r.mine {
			t.Fatalf("Mine shows %s", r.addr)
		}
	}
}

// A style draws exactly what lipgloss draws, whatever the text.
func TestInkIsLipgloss(t *testing.T) {
	for _, th := range []theme{dark, light, ansi16, contrast(dark)} {
		st := newStyles(th)
		inks := []ink{st.fg, st.dim, st.bold, st.hotkey, st.italic, st.label, st.person(3), st.tealBold}
		for _, k := range inks {
			for _, text := range []string{"", " ", "x", "esc", "a b", "é", "⏎", "x\ty", "one\ntwo", "\xff", " ", "r\r\nn"} {
				if got, want := k.Render(text), k.Style.Render(text); got != want {
					t.Errorf("%s: %q drew %q, lipgloss %q", th.name, text, got, want)
				}
			}
			if got, want := k.Render("a", "b"), k.Style.Render("a", "b"); got != want {
				t.Errorf("%s: two strings drew %q, lipgloss %q", th.name, got, want)
			}
		}
	}
}

// The fuse burns half a cell at a time and is full at ten minutes; the clock says the same.
func TestFuse(t *testing.T) {
	st := newStyles(dark)
	for _, c := range []struct {
		waited time.Duration
		want   string
	}{{0, "────────"}, {40 * time.Second, "╸───────"}, {65 * time.Second, "━───────"}, {192 * time.Second, "━━╸─────"},
		{520 * time.Second, "━━━━━━━─"}, {time.Hour, "━━━━━━━━"}} {
		if got := ansi.Strip(st.fuse(unicodeGlyphs, c.waited, true, 8)); got != c.want {
			t.Errorf("after %v: %q, want %q", c.waited, got, c.want)
		}
	}
	if clock(192*time.Second) != "3:12" || clock(time.Hour+5*time.Minute) != "1:05:00" {
		t.Fatal("clock")
	}
}

// The gutter joins linked rows with lines in lanes, long links outside.
func TestGutter(t *testing.T) {
	st := newStyles(dark)
	rows, gaps := st.gutter(unicodeGlyphs, []link{{a: 0, b: 1, between: true}, {a: 1, b: 3}, {a: 1, b: 2}}, 4)
	got := ansi.Strip(strings.Join(rows, "\n"))
	want := "  ╭────\n╭─┴─┬──\n│   ╰──\n╰──────"
	if got != want {
		t.Fatalf("gutter:\n%s\nwant:\n%s", got, want)
	}
	if ansi.Strip(gaps[1]) != "│   │  " {
		t.Fatalf("gap under row 1: %q", ansi.Strip(gaps[1]))
	}
}

// The other screens, at the width where each matters, compared with testdata like the home screen. A screen can start
// from another team than the demo's, on a selection, and be taller than 32 rows.
func TestGoldenScreens(t *testing.T) {
	for _, c := range []struct {
		name string
		w, h int
		snap func() ipc.Snapshot
		sel  string
		keys []string
	}{
		{"inspector-side", 140, 0, nil, "", []string{"down", "down", "down", "down", "down", "down", "down", "space"}},
		{"inspector-full", 84, 0, nil, "", []string{"down", "down", "down", "down", "down", "down", "down", "space"}},
		{"thread", 110, 0, nil, "", []string{"down", "down", "down", "down", "down", "down", "enter"}},
		{"composer", 110, 0, nil, "", []string{"down", "down", "down", "down", "down", "down", "down", "w", "O", "k"}},
		{"palette", 110, 0, nil, "", []string{":", "w", "e", "b"}},
		{"palette-actions", 110, 0, nil, "", []string{"down", "down", "down", "down", "down", "down", "down", ":", "a", "p", "i", "9", "b"}},
		{"help", 110, 0, nil, "", []string{"?"}},
		{"mute", 110, 0, nil, "", []string{"down", "down", "down", "down", "down", "down", "down", "down", "z"}},
		{"filter", 110, 0, nil, "", []string{"/", "w", "e", "b"}},
		{"composer-aside", 110, 0, nil, "agent:bob/main-1", []string{"b", "S", "t", "u", "c", "k", "?"}},
		{"thread-asides", 110, 48, asideTeam, "agent:alice/api-9b", []string{"enter"}},
		{"needs-asides", 110, 0, asideTeam, "need:btw:w2", nil},
		{"needs-asides-cold", 84, 0, asideTeam, "need:btw:w6", nil},
		{"inspector-teammate", 84, 40, nil, "agent:bob/main-1", []string{"space"}},
		{"palette-btw", 110, 0, nil, "agent:bob/main-1", []string{":", "b", "t", "w"}},
		{"needs-agent-aside", 110, 0, agentAsideTeam, "need:btw:a2", nil},
		{"thread-agent-aside", 110, 48, agentAsideTeam, "agent:alice/api-9b", []string{"enter"}},
		{"review-aside", 110, 0, asideTeam, "need:btw:w2", []string{"y"}},
		{"review-long", 84, 24, longAnswerTeam, "need:btw:w2", []string{"y"}},
		{"review-long-rest", 84, 24, longAnswerTeam, "need:btw:w2", []string{"y", "y"}},
	} {
		m := demo(c.w, cmp.Or(c.h, 32), dark)
		if c.snap != nil {
			m.setSnap(c.snap())
		}
		if c.sel != "" {
			m.screen()
			m.selected = c.sel
		}
		press(m, c.keys...)
		text, _ := m.screen()
		if m.sheet != nil {
			text = m.overlay(text)
		}
		golden(t, "screen-"+c.name+".txt", ansi.Strip(text))
		golden(t, "screen-"+c.name+".ansi", text)
	}
}

// A held message is let through or dropped from Needs you; the daemon hears which.
func TestHeldMessage(t *testing.T) {
	m := demo(110, 30, dark)
	m.screen()
	m.selected = "need:held:h1"
	_, cmd := m.key(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if cmd == nil {
		t.Fatal("y does nothing on a held message")
	}
	if got := cmd().(doneMsg); got.err || !strings.Contains(got.text, "Let through to alice/api-9b") {
		t.Fatalf("y: %+v", got)
	}
}

// The m draws itself at start, a light crosses it when a message is sent, a new need glows and the receiving lamp
// brightens; each plays once and ends on time. Without motion, the still versions say the same.
func TestMotion(t *testing.T) {
	now := demoNow
	m := demo(110, 30, dark)
	m.motion, m.clock = true, func() time.Time { return now }
	m.play(animDraw, "")
	now = now.Add(300 * time.Millisecond)
	if logo := ansi.Strip(m.header(m.board())[0]); logo != " ╭─  "+strings.Repeat(" ", 105) {
		t.Fatalf("a third of the way, the m is %q", strings.TrimRight(logo, " "))
	}
	now = now.Add(time.Second)
	if m.onFrame(); len(m.anims) != 0 {
		t.Fatal("the m kept drawing")
	}

	next := demoSnapshot(demoNow)
	next.Msgs = append(next.Msgs, protocol.Msg{ID: "new", From: "carol/billing-2", To: "alice/api-9b", Text: "pushed", At: now.UnixMilli()})
	next.Asks = append(next.Asks, protocol.Msg{ID: "h2", From: "bob/main-1", To: "alice/api-1a", Text: "rebase?", At: now.UnixMilli()})
	m.Update(snapMsg(next))
	if m.progress(animLight, "") < 0 || m.progress(animBold, "alice/api-9b") < 0 || m.progress(animGlow, "need:held:h2") < 0 {
		t.Fatalf("a message and a new need played nothing: %+v", m.anims)
	}

	m = demo(110, 30, dark) // motion off: the new need says so
	m.clock = func() time.Time { return now }
	m.have = true
	m.Update(snapMsg(next))
	if text, _ := m.screen(); !strings.Contains(ansi.Strip(text), "new held “rebase?”") {
		t.Fatal("without motion, a new need isn't marked new")
	}
}

// bigTeam is the demo team grown to two dozen people, each with a few agents in a constellation of their own.
func bigTeam() ipc.Snapshot {
	s := demoSnapshot(demoNow)
	names := []string{"hugo", "ines", "jules", "karim", "lea", "malo", "maya", "nina", "omar", "paul", "quinn", "rosa", "sami",
		"theo", "uma", "victor", "wen", "yanis", "zoe"}
	at := demoNow.Add(-time.Hour).UnixMilli()
	for i, n := range names {
		p := protocol.Person{Handle: n, Online: true, Colour: (5 + i) % protocol.Colours} // after the demo team, as a hub numbers them
		for j := range 3 {
			a := protocol.Agent{Name: fmt.Sprintf("%s-%d", n[:2], j), Repo: fmt.Sprintf("acme/svc-%d", i%7), Branch: fmt.Sprintf("feat/topic-%d", i),
				Tree: fmt.Sprintf("t-%s-%d", n, j), Title: "Work on topic " + fmt.Sprint(i), Status: protocol.StatusBusy, Since: at,
				Kinds: "rreexxrree"}
			if j == 2 && i%4 == 0 {
				a.Status, a.Needs = protocol.StatusWaiting, "input"
			}
			p.Agents = append(p.Agents, a)
		}
		s.People = append(s.People, p)
		s.Msgs = append(s.Msgs, protocol.Msg{ID: "b" + n, From: n + "/" + n[:2] + "-0", To: n + "/" + n[:2] + "-1", Text: "hi", At: at},
			protocol.Msg{ID: "c" + n, From: n + "/" + n[:2] + "-1", To: n + "/" + n[:2] + "-2", Text: "hi", At: at})
	}
	return s
}

// A large team folds what is far from you into Elsewhere, near you first; All unfolds it.
func TestLargeTeam(t *testing.T) {
	m := demo(110, 40, dark)
	m.setSnap(bigTeam())
	b := m.board()
	if !b.large || m.currentScope() != scopeNear {
		t.Fatalf("large %v, scope %v", b.large, m.currentScope())
	}
	if len(b.elsewhere) != 19 || len(b.groups) != 2 {
		t.Fatalf("%d near, %d elsewhere", len(b.groups), len(b.elsewhere))
	}
	if in := b.initials["maya"]; in != "Ma" {
		t.Fatalf("maya's initial is %q next to malo", in)
	}
	press(m, "a")
	if b := m.board(); len(b.elsewhere) != 0 || len(b.groups) != 21 {
		t.Fatalf("All folds %d away", len(b.elsewhere))
	}
	m = demo(110, 40, dark)
	m.setSnap(bigTeam())
	text, _ := m.screen()
	golden(t, "screen-large.txt", ansi.Strip(text))
}

// The plain mode says the team in sentences, then what changes, and takes commands by need number.
func TestPlain(t *testing.T) {
	s := demoSnapshot(demoNow)
	text := strings.Join(summary(s, demoNow), "\n")
	for _, want := range []string{"murmur, as alice. Connected. 4 people online, 12 agents.", "4 things need you:",
		"1. alice/api-1a needs you: approve running go test ./ocr/..., waiting 3 minutes.",
		"alice/api-9b is working, in acme/api on feat/billing-v2: Rerun billing on staging.",
		"Out of tune: alice/infra-3 and dave/infra-1 both change main.tf and never talked."} {
		if !strings.Contains(text, want) {
			t.Errorf("the summary doesn't say %q:\n%s", want, text)
		}
	}
	next := demoSnapshot(demoNow)
	next.Msgs = append(next.Msgs, protocol.Msg{ID: "n1", From: "carol/billing-2", To: "alice/api-9b", Text: "pushed\nmore", At: demoNow.UnixMilli()})
	next.People[0].Agents[1].Status, next.People[0].Agents[1].Needs = protocol.StatusBusy, ""
	got := strings.Join(changes(s, next, demoNow), "\n")
	for _, want := range []string{"Message from carol/billing-2 to alice/api-9b: pushed", "alice/api-1a no longer needs you."} {
		if !strings.Contains(got, want) {
			t.Errorf("the changes don't say %q:\n%s", want, got)
		}
	}
	var asked ipc.Request
	fake := func(r ipc.Request) (ipc.Reply, error) { asked = r; return ipc.Reply{OK: true}, nil }
	if reply, _ := command(s, "y 4", demoNow, fake); reply != "Let through to alice/api-9b." || asked.Op != "approve" || asked.ID != "h1" {
		t.Fatalf("y 4: %q, asked %+v", reply, asked)
	}
	if reply, _ := command(s, "y 1", demoNow, fake); !strings.Contains(reply, "held message") {
		t.Fatalf("y on an approval: %q", reply)
	}
	if _, quit := command(s, "quit", demoNow, fake); !quit {
		t.Fatal("quit doesn't quit")
	}
}

// * pins a constellation to the top; a click on an owner's initial filters to that person, and a double click opens.
func TestPinAndClick(t *testing.T) {
	m := demo(110, 40, dark)
	m.screen()
	m.selected = "agent:alice/web-4c"
	press(m, "*")
	if _, body := m.homeLines(m.board()); body[0].key != "group:alice/web-4c" {
		t.Fatalf("the pinned constellation isn't first: %q", body[0].key)
	}
	_, lines := m.screen()
	y := slices.IndexFunc(lines, func(l line) bool { return l.key == "agent:carol/web-2" })
	m.click(tea.Mouse{X: 1 + gutterW + 2, Y: y})
	if m.filter != "carol" {
		t.Fatalf("a click on carol's initial filters to %q", m.filter)
	}
	m.filter = ""
	_, lines = m.screen()
	y = slices.IndexFunc(lines, func(l line) bool { return l.key == "agent:carol/web-2" })
	m.click(tea.Mouse{X: 30, Y: y})
	m.click(tea.Mouse{X: 30, Y: y})
	if m.thread == nil || m.thread.name != "Search" {
		t.Fatal("a double click doesn't open the thread")
	}
}

// N nudges a teammate whose agent waits for them.
func TestNudge(t *testing.T) {
	m := demo(110, 30, dark)
	m.screen()
	m.selected = "agent:carol/billing-2"
	_, cmd := m.key(tea.KeyPressMsg{Code: 'N', Text: "N"})
	if cmd == nil || cmd().(doneMsg).text != "Nudged carol" {
		t.Fatal("N doesn't nudge carol")
	}
	m.selected = "agent:bob/main-1"
	if _, cmd := m.key(tea.KeyPressMsg{Code: 'N', Text: "N"}); cmd != nil {
		t.Fatal("N nudges about an agent that isn't waiting")
	}
}

// g switches to the terminal an agent runs in, as its hook saw it: tmux first, then iTerm2, WezTerm and kitty; a
// terminal that doesn't allow it is named instead, and nothing odd reaches a command.
func TestJumpCmd(t *testing.T) {
	cmd, err := jumpCmd(map[string]string{"TMUX": "/tmp/tmux-501/default,123,0", "TMUX_PANE": "%12", "ITERM_SESSION_ID": "w0t1p0:x"})
	if err != nil || strings.Join(cmd.Args, " ") != "tmux -S /tmp/tmux-501/default select-window -t %12 ; select-pane -t %12 ; switch-client -t %12" {
		t.Fatalf("tmux: %v %v", cmd, err)
	}
	cmd, _ = jumpCmd(map[string]string{"ITERM_SESSION_ID": "w0t1p0:0B1C2D3E-4F50-6172-8394-A5B6C7D8E9F0"})
	if cmd == nil || cmd.Args[0] != "osascript" || !strings.Contains(cmd.Args[2], `unique id of s is "0B1C2D3E-4F50-6172-8394-A5B6C7D8E9F0"`) {
		t.Fatalf("iTerm2: %v", cmd)
	}
	if cmd, _ := jumpCmd(map[string]string{"WEZTERM_PANE": "7"}); cmd == nil || strings.Join(cmd.Args, " ") != "wezterm cli activate-pane --pane-id 7" {
		t.Fatalf("WezTerm: %v", cmd)
	}
	if cmd, _ := jumpCmd(map[string]string{"KITTY_WINDOW_ID": "3", "KITTY_LISTEN_ON": "unix:/tmp/k"}); cmd == nil ||
		strings.Join(cmd.Args, " ") != "kitty @ --to unix:/tmp/k focus-window --match id:3" {
		t.Fatalf("kitty: %v", cmd)
	}
	if _, err := jumpCmd(map[string]string{"TERM_PROGRAM": "vscode"}); err == nil || !strings.Contains(err.Error(), "VS Code") {
		t.Fatalf("VS Code: %v", err)
	}
	for _, bad := range []map[string]string{{"TMUX_PANE": "%1; rm -rf ~"}, {"ITERM_SESSION_ID": `w0:" & do shell script "x`}, {"WEZTERM_PANE": "1 2"}} {
		if cmd, err := jumpCmd(bad); cmd != nil || err == nil {
			t.Fatalf("%v gave %v", bad, cmd)
		}
	}
}

// A long conversation opens on its newest messages and scrolls a line at a time, back to its first.
func TestThreadScrolls(t *testing.T) {
	m := demo(110, 30, dark)
	s := demoSnapshot(demoNow)
	for i := range 60 {
		s.Msgs = append(s.Msgs, protocol.Msg{ID: fmt.Sprint("long-", i), From: "carol/billing-2", To: "alice/api-9b",
			Text: fmt.Sprintf("message %d, long enough to wrap: %s", i, strings.Repeat("words ", 30)), At: demoNow.Add(time.Duration(i-60) * time.Second).UnixMilli()})
	}
	m.setSnap(s)
	m.screen()
	m.selected = "agent:alice/api-9b"
	press(m, "enter")
	body := func() []string { // the lines between the header and the composer's rule
		text, _ := m.screen()
		lines := strings.Split(ansi.Strip(text), "\n")
		end := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, " ───") })
		return lines[3:end]
	}
	newest := body()
	if text := strings.Join(newest, "\n"); !strings.Contains(text, "message 59") || strings.Contains(text, "message 0,") {
		t.Fatalf("the thread opens on:\n%s", text)
	}
	m.thread.scroll = 1
	if up := body(); strings.Join(up[1:], "\n") != strings.Join(newest[:len(newest)-1], "\n") {
		t.Fatalf("one line up shows:\n%s", strings.Join(up, "\n"))
	}
	m.thread.scroll = 100_000
	if oldest := body(); !strings.Contains(strings.Join(oldest, "\n"), "message 0,") || m.thread.scroll == 100_000 {
		t.Fatalf("scrolled to the top, scroll %d:\n%s", m.thread.scroll, strings.Join(oldest, "\n"))
	}
}

// Enter on a constellation's header opens its conversation, written to a teammate's agent in it.
func TestHeaderOpensThread(t *testing.T) {
	m := demo(110, 30, dark)
	m.screen()
	m.selected = "group:" + m.board().groups[0].key
	press(m, "enter")
	if m.thread == nil || m.thread.name != "Billing" || protocol.PersonOf(m.thread.to) == "alice" {
		t.Fatalf("thread %+v", m.thread)
	}
}

// Ctrl+C quits from anywhere, the composer and the sheets included.
func TestCtrlCQuits(t *testing.T) {
	for _, keys := range [][]string{{}, {"down", "down", "down", "down", "down", "w"}, {":"}, {"/"}, {"?"}} {
		m := demo(110, 30, dark)
		press(m, keys...)
		_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
		if cmd == nil || cmd() != tea.Quit() {
			t.Errorf("after %v, ctrl+c doesn't quit", keys)
		}
	}
}

// t, m and a press the scope tabs, whose letters are drawn as keys; in a large team m is Near me.
func TestScopeKeys(t *testing.T) {
	m := demo(110, 30, dark)
	if m.currentScope() != scopeTeam {
		t.Fatalf("a team of four starts on %s", scopeNames[m.currentScope()])
	}
	for _, letter := range []string{"T", "M", "A"} {
		if !strings.Contains(m.header(m.board())[1], m.st.hotkey.Render(letter)) {
			t.Fatalf("the tabs don't draw %s as a key", letter)
		}
	}
	for _, c := range []struct {
		key  string
		want scope
	}{{"m", scopeMine}, {"a", scopeAll}, {"t", scopeTeam}} {
		press(m, c.key)
		if m.currentScope() != c.want {
			t.Fatalf("%s chose %s", c.key, scopeNames[m.currentScope()])
		}
	}
	m.setSnap(bigTeam())
	if head := m.header(m.board())[1]; !strings.Contains(ansi.Strip(head), " Near me ") || !strings.Contains(head, m.st.hotkey.Render("m")) {
		t.Fatal("Near me doesn't draw the m of me as its key")
	}
	for _, c := range []struct {
		key  string
		want scope
	}{{"m", scopeNear}, {"t", scopeTeam}, {"a", scopeAll}, {"m", scopeNear}} {
		press(m, c.key)
		if m.currentScope() != c.want {
			t.Fatalf("in a large team, %s chose %s", c.key, scopeNames[m.currentScope()])
		}
	}
}

// The header shows you, your initial in the colour the hub gave you; everyone's colour is the hub's.
func TestHeaderShowsYou(t *testing.T) {
	m := demo(110, 30, dark)
	m.snap.Me.Colour, m.snap.People[0].Colour = 6, 6
	m.snap.People[2].Colour = 0 // carol joined before anyone
	b := m.board()
	if b.colours["alice"] != 6 || b.colours["bob"] != 1 || b.colours["carol"] != 0 || b.colours["dave"] != 3 {
		t.Fatalf("colours %v", b.colours)
	}
	head := m.header(b)
	if got := ansi.Strip(head[1]); !strings.HasSuffix(got, "A alice · 4 online · 12 agents   ● live  ") {
		t.Fatalf("the header says %q", got)
	}
	if !strings.Contains(head[1], m.st.person(6).Render("A")) || strings.Contains(head[1], m.st.person(0).Render("A")) {
		t.Fatal("your initial isn't in your colour")
	}
	m.w = 60 // one row: your initial before the lamp
	head = m.header(m.board())
	if got := ansi.Strip(head[0]); len(head) != 1 || !strings.HasSuffix(got, "◆ 4   A ● live ") || !strings.Contains(head[0], m.st.person(6).Render("A")) {
		t.Fatalf("the narrow header says %q", got)
	}
	m = demo(110, 30, dark) // alone on the hub
	m.snap.People = m.snap.People[:1]
	if got := ansi.Strip(m.header(m.board())[1]); !strings.HasSuffix(got, "A alice · 7 agents   ● live  ") {
		t.Fatalf("alone, the header says %q", got)
	}
	for _, snap := range []ipc.Snapshot{demoSnapshot(demoNow), bigTeam()} { // the tabs and you fit at every width
		m.setSnap(snap)
		for m.w = 64; m.w <= 140; m.w++ {
			if got := ansi.Strip(m.header(m.board())[1]); !strings.Contains(got, " All ") || !strings.HasSuffix(got, "A   ● live  ") &&
				!strings.HasSuffix(got, "alice   ● live  ") && !strings.HasSuffix(got, "agents   ● live  ") {
				t.Fatalf("at %d, the header says %q", m.w, got)
			}
		}
	}
}

// The footer explains only the keys the screen doesn't show: never a scope tab's, nor n beside Needs you, nor i
// beside a dissonance, nor the inspector's actions, nor a held message's y and x.
func TestFooterLeavesOutShownKeys(t *testing.T) {
	check := func(m *model, where string) string {
		t.Helper()
		text, _ := m.screen()
		lines := strings.Split(text, "\n")
		shown := map[string]bool{}
		for _, k := range hotkeys(m, strings.Join(lines[:len(lines)-1], "\n")) {
			shown[strings.ToLower(k)] = true // a label's letter is the key in lower case
		}
		footer := lines[len(lines)-1]
		for _, k := range hotkeys(m, footer) {
			if shown[k] {
				t.Errorf("%s: the footer repeats %s, on screen already: %q", where, k, ansi.Strip(footer))
			}
		}
		return ansi.Strip(footer)
	}
	for _, size := range [][2]int{{60, 24}, {84, 30}, {110, 30}, {140, 40}} {
		for _, inspect := range []bool{false, true} {
			for _, big := range []bool{false, true} {
				m := demo(size[0], size[1], dark)
				if big {
					m.setSnap(bigTeam())
				}
				m.screen()
				m.inspect = inspect
				for i := range len(m.keys) {
					m.jump(i)
					check(m, fmt.Sprintf("%dx%d, inspector %v, big team %v, on %s", size[0], size[1], inspect, big, m.selected))
				}
			}
		}
	}
	m := demo(110, 30, dark)
	m.screen()
	m.selected = "agent:alice/api-9b"
	if got := check(m, "api-9b"); !strings.HasPrefix(got, " ⏎ thread  ␣ inspect  w write  b by the way  c copy  z mute  g terminal  o open #1822 ") ||
		!strings.HasSuffix(got, "? keys  ") {
		t.Fatalf("on api-9b, the footer says %q", got)
	}
	m.selected = "agent:carol/billing-2"
	if got := check(m, "billing-2"); !strings.HasPrefix(got, " N nudge carol  ⏎ thread") {
		t.Fatalf("on carol's waiting agent, the footer says %q", got)
	}
	m.selected = "need:held:h1"
	press(m, "enter")
	if got := check(m, "a thread with a held message"); !strings.HasPrefix(got, " w write  b by the way  ⇥ recipient") {
		t.Fatalf("in a thread with a held message in view, the footer says %q", got)
	}
}

// hotkeys are the keys drawn in the hotkey style in text, in order.
func hotkeys(m *model, text string) []string {
	on, off, _ := strings.Cut(m.st.hotkey.Render("x"), "x")
	var out []string
	for {
		_, rest, ok := strings.Cut(text, on)
		if !ok {
			return out
		}
		k, after, _ := strings.Cut(rest, off)
		out, text = append(out, k), after
	}
}

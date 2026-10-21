package console

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/charmbracelet/x/ansi"
)

// fakeDaemon answers the console on a socket of its own, as the daemon would, and keeps what it was asked.
func fakeDaemon(t *testing.T, answer func(ipc.Request) ipc.Reply) func() []ipc.Request {
	t.Helper()
	dir, err := os.MkdirTemp("", "mm") // short: unix socket paths are capped near 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("MURMUR_HOME", dir)
	ln, err := net.Listen("unix", config.SockPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	var asked []ipc.Request
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadBytes('\n')
			var req ipc.Request
			_ = json.Unmarshal(line, &req)
			reply := ipc.Reply{OK: true} // a status: the daemon runs
			if req.Op != "status" {
				mu.Lock()
				asked = append(asked, req)
				mu.Unlock()
				reply = answer(req)
			}
			_ = json.NewEncoder(c).Encode(reply)
			c.Close()
		}
	}()
	return func() []ipc.Request {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

// failedTo is the demo team with a message to bob that didn't get through, which only the outbox has.
func failedTo(state string) ipc.Snapshot {
	s := demoSnapshot(demoNow)
	s.State = state
	s.Outbox = []protocol.Msg{{ID: "f1", From: "alice", To: "bob", Text: "ok to ship?", At: demoNow.Add(-time.Minute).UnixMilli()}}
	s.Failed = map[string]string{"f1": "bob is away"}
	return s
}

func enter() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyEnter} }

// ⏎ on a message that didn't get through sends it again, once however often it is pressed, and its failure leaves
// Needs you once the daemon took the new one: should that fail too, the new failure says so, and only it. Signed
// out, the daemon refuses the message, and the failure stays.
func TestResendReplacesTheFailure(t *testing.T) {
	for _, c := range []struct {
		name, state string
		reply       ipc.Reply
		done        bool
	}{
		{"sent", "online", ipc.Reply{OK: true, Note: "delivered"}, true},
		{"failed again", "online", ipc.Reply{Err: `nobody called "bob" is on murmur; ` + "`who`" + ` lists the team`}, true},
		{"signed out", "signed out: run murmur join", ipc.Reply{Err: "Murmur is signed out: run murmur join"}, false},
	} {
		asked := fakeDaemon(t, func(r ipc.Request) ipc.Reply {
			if r.Op == "send" {
				return c.reply
			}
			return ipc.Reply{OK: true}
		})
		m := demo(110, 30, dark)
		m.demo = false
		m.setSnap(failedTo(c.state))
		m.screen()
		m.selected = "need:failed:f1"
		_, cmd := m.Update(enter())
		if _, again := m.Update(enter()); cmd == nil || again != nil {
			t.Fatalf("%s: ⏎ twice gave %v then %v", c.name, cmd != nil, again != nil)
		}
		m.Update(cmd())
		var ops []string
		for _, r := range asked() {
			ops = append(ops, r.Op+" "+r.To+r.ID)
		}
		want := []string{"send bob"}
		if c.done {
			want = append(want, "done f1")
		}
		if !slices.Equal(ops, want) {
			t.Errorf("%s: the daemon was asked %v, want %v", c.name, ops, want)
		}
		if got := m.outcome; got.err != (c.reply.Err != "") || c.reply.Err != "" && got.text != c.reply.Err {
			t.Errorf("%s: the outcome says %+v", c.name, got)
		}
		if _, cmd := m.Update(enter()); (cmd != nil) != !c.done { // what the daemon didn't take can go again
			t.Errorf("%s: ⏎ after the answer sends again: %v", c.name, cmd != nil)
		}
	}
}

// A thread wraps a long word, a link or a line of Chinese without spaces, instead of cutting it at the edge.
func TestThreadWrapsLongWords(t *testing.T) {
	link := "https://example.com/" + strings.Repeat("a1b2c3", 30)
	zh := strings.Repeat("数据处理代理", 25)
	s := demoSnapshot(demoNow)
	s.Msgs = append(s.Msgs, protocol.Msg{ID: "l1", From: "carol/billing-2", To: "alice/api-9b", Text: link + "\n" + zh, At: demoNow.UnixMilli()})
	for _, w := range []int{63, 84, 109, 160} {
		m := demo(w, 60, dark)
		m.setSnap(s)
		m.screen()
		m.selected = "agent:alice/api-9b"
		press(m, "enter")
		text, _ := m.screen()
		var joined strings.Builder
		for _, l := range strings.Split(ansi.Strip(text), "\n") {
			if ansi.StringWidth(l) != w {
				t.Fatalf("at %d, a line is %d wide: %q", w, ansi.StringWidth(l), l)
			}
			joined.WriteString(strings.TrimSpace(l))
		}
		if !strings.Contains(joined.String(), link) || !strings.Contains(joined.String(), zh) {
			t.Errorf("at %d, the thread cuts the message:\n%s", w, ansi.Strip(text))
		}
	}
}

// keyPress is a key as Bubble Tea reports it.
func keyPress(k string) tea.KeyPressMsg {
	switch k {
	case "enter":
		return enter()
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "home":
		return tea.KeyPressMsg{Code: tea.KeyHome}
	case "end":
		return tea.KeyPressMsg{Code: tea.KeyEnd}
	}
	return tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
}

// everyKey is every key the board and the thread take.
var everyKey = []string{"x", "y", "enter", "r", "w", "g", "N", "i", "o", "c", "space", "z", "p", ":", "?", "/", "*", "n", "[", "]",
	"h", "l", "home", "end", "up", "down", "t", "m", "a", "esc"}

// Every key, then every key again, on an empty board, before sign-in with nobody, on a need that went away between
// two snapshots, in a thread and in the inspector, at any size: nothing breaks, and the screen fills the terminal
// wherever it has room for the board. A sheet open on a terminal narrower than it is cut at the edge.
func TestEveryKeyEverywhere(t *testing.T) {
	gone := failedTo("online")
	gone.Outbox, gone.Failed = nil, nil
	snaps := []struct {
		name     string
		snap     ipc.Snapshot
		selected string
		opens    []string // what is open when the keys come: the board, the inspector, a thread
	}{
		{"an empty board", ipc.Snapshot{Me: protocol.Person{Handle: "alice"}, State: "online"}, "", []string{""}},
		{"nobody, before sign-in", ipc.Snapshot{State: "not signed in: run murmur join"}, "", []string{""}},
		{"a need gone", gone, "need:failed:f1", []string{""}},
		{"the demo", demoSnapshot(demoNow), "agent:alice/api-9b", []string{"", "space", "enter"}},
	}
	for _, c := range snaps {
		for _, size := range [][2]int{{0, 0}, {1, 1}, {9, 40}, {40, 10}, {110, 30}} {
			for _, open := range c.opens {
				for _, k := range everyKey {
					where := c.name + ", " + open + " then " + k
					func() {
						defer func() {
							if r := recover(); r != nil {
								t.Fatalf("%s %dx%d: %v", where, size[0], size[1], r)
							}
						}()
						m := demo(size[0], size[1], dark)
						m.setSnap(failedTo("online"))
						m.screen()
						m.selected = "need:failed:f1"
						m.setSnap(c.snap) // the need goes between two snapshots
						m.selected = c.selected
						for _, k := range []string{open, k, k} {
							if k == "" {
								continue
							}
							m.Update(keyPress(k))
							m.View()
						}
						if m.sheet != nil || size[0] < 40 || size[1] < 5 {
							return
						}
						text, _ := m.screen()
						lines := strings.Split(text, "\n")
						if len(lines) != size[1] {
							t.Fatalf("%s %dx%d: %d lines", where, size[0], size[1], len(lines))
						}
						for _, l := range lines {
							if ansi.StringWidth(l) != size[0] {
								t.Fatalf("%s %dx%d: a line is %d wide: %q", where, size[0], size[1], ansi.StringWidth(l), ansi.Strip(l))
							}
						}
					}()
				}
			}
		}
	}
}

// The filter matches what is typed as it is, regex signs included, ignoring case in any script; what needs you stays.
func TestFilterTakesAnyText(t *testing.T) {
	s := demoSnapshot(demoNow)
	s.People[3].Agents[0].Title = "Écrire le guide 数据 (v2.*)" // dave/infra-1
	for filter, want := range map[string]int{"(v2.*)": 1, "écrire": 1, "数据": 1, ".+": 0, "[": 0, "   ": 12, "́": 0} {
		m := demo(110, 30, dark)
		m.setSnap(s)
		press(m, "/")
		for _, r := range filter {
			press(m, string(r))
		}
		b := m.board()
		if got := len(b.lone) + len(b.quiet) + len(b.groups); want != 12 && got != want || want == 12 && b.agents != 12 {
			t.Errorf("%q: %d rows, want %d", filter, got, want)
		}
		if len(b.needs) != 4 {
			t.Errorf("%q: %d needs", filter, len(b.needs))
		}
	}
}

// A search in a sheet that matches nothing, then ↓ and ⏎, closes it and does nothing; in the mute sheet, d and f
// pick nothing and go to the search.
func TestSheetWithNothingMatching(t *testing.T) {
	for _, keys := range [][]string{{":", "q", "q", "q", "q", "down", "enter"}, {"?", "§", "down", "down", "enter"},
		{"z", "q", "q", "q", "q", "down", "d", "f", "enter"}} {
		m := demo(110, 30, dark)
		m.screen()
		m.selected = "agent:carol/billing-2"
		var cmd tea.Cmd
		for _, k := range keys {
			_, cmd = m.Update(keyPress(k))
			m.View()
			if cmd != nil {
				t.Errorf("%v: %s does something", keys, k)
			}
		}
		if m.sheet != nil {
			t.Errorf("%v: the sheet stays open", keys)
		}
	}
}

// A teammate's laptop whose clock runs ahead says its agent waits since a moment still to come, and the hub's clock
// stamps a question: the fuse hasn't burnt yet, and the clock says 0:00.
func TestWaitsFromAClockAhead(t *testing.T) {
	s := demoSnapshot(demoNow)
	s.People[2].Agents[0].Since = demoNow.Add(5 * time.Minute).UnixMilli() // carol/billing-2, waiting on carol
	s.Msgs[5].At = demoNow.Add(2 * time.Minute).UnixMilli()                // dave's question
	m := demo(110, 30, dark)
	m.setSnap(s)
	text, _ := m.screen()
	for _, who := range []string{"billing-2", "dave"} {
		found := false
		for _, l := range strings.Split(ansi.Strip(text), "\n") {
			if strings.Contains(l, who) && strings.Contains(l, "──────── 0:00") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s's fuse:\n%s", who, ansi.Strip(text))
		}
	}
}

// Why one of your agents waits keeps the first letter of what it does whole, whatever the letter.
func TestWaitReasonKeepsLetters(t *testing.T) {
	for doing, want := range map[string]string{"Écrire notes.md": "approve écrire notes.md", "数据 · query": "approve 数据 · query",
		"Running make": "approve running make"} {
		if got := waitReason(protocol.Agent{Needs: "permission", Doing: doing}); got != want {
			t.Errorf("%q: %q, want %q", doing, got, want)
		}
	}
}

// A constellation on two branches that start alike is named after what they share, in whole letters.
func TestConstellationNameInWholeLetters(t *testing.T) {
	for want, branches := range map[string][2]string{"数据": {"数据处理", "数据库"}, "Cach": {"caché", "cachè"}, "Billing": {"billing-ocr", "billing-v2"}} {
		ms := []*agentRef{{a: protocol.Agent{Name: "a", Branch: branches[0]}}, {a: protocol.Agent{Name: "b", Branch: branches[1]}}}
		if got := groupName(ms); got != want {
			t.Errorf("%v: %q, want %q", branches, got, want)
		}
	}
}

// evil is what a terminal would act on: a clipboard write, clearing the screen, a right-to-left override, an 8-bit
// CSI, a link to a file, a bell.
const evil = "\x1b]52;c;ZXZpbA==\x07\x1b[2J\u202e\u009b31m\x1b]8;;file:///etc/passwd\x1b\\"

func noControls(t *testing.T, where, text string) {
	t.Helper()
	for _, bad := range []string{"]52;", "[2J", "\x07", "\u202e", "\u009b", "file:///"} {
		if strings.Contains(text, bad) {
			t.Errorf("%s: %q reaches the terminal", where, bad)
		}
	}
}

// What this laptop's own agents send isn't cleaned by the daemon: a message waiting in the outbox, or failed, and
// the note saying so, which quotes its address. None of it reaches the terminal raw: not on the board, in the
// thread, the sheets, the plain mode's sentences, nor a notification.
func TestOwnAgentsTextIsCleaned(t *testing.T) {
	s := demoSnapshot(demoNow)
	at := demoNow.Add(-time.Minute).UnixMilli()
	s.Outbox = []protocol.Msg{{ID: "o1", From: "alice/api-9b", To: "bob" + evil, Text: "hi" + evil, At: at},
		{ID: "o2", From: "alice/api-9b", To: "carol/billing-2", Text: "sending" + evil, At: at}}
	s.Sending, s.Failed = []string{"o2"}, map[string]string{"o1": "nobody called bob" + evil}
	s.Msgs = append(s.Msgs, protocol.Msg{ID: "nd-o1", From: protocol.System, To: "alice", Text: "not delivered to bob" + evil, At: at})
	s.Mutes = append(s.Mutes, "agent bob/main-1"+evil)
	m := demo(160, 50, dark)
	m.setSnap(s)
	text, _ := m.screen()
	noControls(t, "the board", text)
	m.selected = "need:failed:o1"
	m.Update(keyPress("space"))
	text, _ = m.screen()
	noControls(t, "the inspector", text)
	m.Update(keyPress("w"))
	text, _ = m.screen()
	noControls(t, "the thread", text)
	m.thread = nil
	m.selected = "agent:carol/billing-2"
	m.Update(keyPress("enter"))
	text, _ = m.screen()
	noControls(t, "the thread of a message sending", text)
	m.thread, m.inspect = nil, false
	for _, k := range []string{"p", ":"} {
		m.Update(keyPress(k))
		text, _ = m.screen()
		noControls(t, "the sheet "+k, m.overlay(text))
		m.sheet = nil
	}
	for _, n := range m.board().needs {
		noControls(t, "the notification for "+n.key, needSentence(n))
	}
	before := demoSnapshot(demoNow)
	noControls(t, "the plain mode", strings.Join(append(summary(cleanSnap(s), demoNow), changes(before, cleanSnap(s), demoNow)...), "\n"))
}

// o opens a pull request's page, never what a teammate's laptop names as one: a file, an app, a share, an argument.
func TestPullRequestLinksAreWebPages(t *testing.T) {
	for _, link := range []string{"file:///Applications/Calculator.app", "-a Calculator", "smb://evil.example/share",
		"x-apple.systempreferences:com.apple.preference.security", "javascript:alert(1)"} {
		s := demoSnapshot(demoNow)
		s.People[2].Agents[1].PR, s.People[2].Agents[1].PRURL = 7, link // carol/web-2
		m := demo(110, 30, dark)
		m.setSnap(s)
		m.screen()
		m.selected = "agent:carol/web-2"
		text, _ := m.screen()
		if _, cmd := m.key(keyPress("o")); cmd != nil || strings.Contains(text, link) || slices.Contains(m.footerKeys(m.board()), "o") {
			t.Errorf("%q: o opens it %v, the screen links it %v", link, cmd != nil, strings.Contains(text, link))
		}
	}
	m := demo(110, 30, dark)
	m.screen()
	m.selected = "agent:alice/api-9b"
	if _, cmd := m.key(keyPress("o")); cmd == nil {
		t.Error("o doesn't open a pull request on GitHub")
	}
}

// The plain mode's commands take extra spaces, and refuse a missing argument, a number that names no need of the
// kind (0, negative, too big, not ASCII digits) and an empty answer, without asking the daemon. done marks a question
// or a failed message, never an approval or a held message.
func TestPlainCommands(t *testing.T) {
	// 1 an approval, 2 carol's question by the way, 3 dave's question, 4 the failed message, 5 the held one
	s := failedTo("online")
	var asked []ipc.Request
	fake := func(r ipc.Request) (ipc.Reply, error) { asked = append(asked, r); return ipc.Reply{OK: true}, nil }
	for line, want := range map[string]string{
		"y   5": "approve h1", "x 5": "decline h1", "reply  3   ok, merge it": "send dave ok, merge it", "done 3": "done m6",
		"done 4": "done f1", "send  bob   hi": "send bob hi", "mute bob/main-1 1h": "mute bob/main-1 1h",
	} {
		asked = nil
		if reply, _ := command(s, line, demoNow, fake); len(asked) != 1 || strings.HasPrefix(reply, "Say") {
			t.Errorf("%q: %q, asked %+v", line, reply, asked)
			continue
		}
		if got := strings.Join(strings.Fields(asked[0].Op+" "+asked[0].To+asked[0].ID+" "+asked[0].Text), " "); got != want {
			t.Errorf("%q asked %q, want %q", line, got, want)
		}
	}
	for _, line := range []string{"y", "x", "y 0", "y -5", "y 6", "y 99999999999999999999", "y ５", "y ٥", "y 5.0", "y 0x5", "y 1",
		"x 4", "done", "done 1", "done 5", "done 9", "reply", "reply 3", "reply 3    ", "reply two ok", "send", "send bob",
		"send bob  \t ", "mute"} {
		asked = nil
		if reply, _ := command(s, line, demoNow, fake); len(asked) != 0 || reply == "" {
			t.Errorf("%q: %q, asked %+v", line, reply, asked)
		}
	}
}

// x in a thread marks its messages done, the failed one only the outbox has too; a thread with no messages, or only
// murmur's notes, asks nothing. A held message decided on another screen says what the daemon says.
func TestThreadDoneAndHeldDecidedElsewhere(t *testing.T) {
	asked := fakeDaemon(t, func(r ipc.Request) ipc.Reply {
		if r.Op == "approve" {
			return ipc.Reply{Err: "that message was let through or dropped already"}
		}
		return ipc.Reply{OK: true}
	})
	s := failedTo("online")
	s.Msgs = append(s.Msgs, protocol.Msg{ID: "nd-x", From: protocol.System, To: "alice/notes", Text: "not delivered", At: demoNow.UnixMilli()})
	m := demo(110, 30, dark)
	m.demo = false
	m.setSnap(s)
	m.screen()
	for _, sel := range []string{"need:failed:f1", "agent:alice/notes", "agent:alice/docs-2"} {
		m.selected = sel
		m.Update(keyPress("w")) // ⏎ on a failed message sends it again
		m.Update(keyPress("esc"))
		if m.thread == nil {
			t.Fatalf("w on %s opens no thread", sel)
		}
		if _, cmd := m.Update(keyPress("x")); cmd != nil {
			m.Update(cmd())
		}
		if m.thread != nil {
			t.Fatalf("x leaves the thread of %s open", sel)
		}
	}
	m.selected = "need:held:h1"
	_, cmd := m.Update(keyPress("y"))
	m.Update(cmd())
	var ops []string
	for _, r := range asked() {
		ops = append(ops, r.Op+" "+strings.Join(r.IDs, ",")+r.ID)
	}
	if want := []string{"done f1", "approve h1"}; !slices.Equal(ops, want) {
		t.Errorf("the daemon was asked %v, want %v", ops, want)
	}
	if !m.outcome.err || m.outcome.text != "that message was let through or dropped already" {
		t.Errorf("the outcome says %+v", m.outcome)
	}
}

// A need's number in the plain mode is its place in the list as it was last printed: a question arriving since, which
// sorts before it, doesn't turn "x 4" onto another message.
func TestPlainNumbersAreThoseListed(t *testing.T) {
	listed := demoSnapshot(demoNow)
	last := demoSnapshot(demoNow)
	last.Msgs = append(last.Msgs, protocol.Msg{ID: "q0", From: "bob", To: "alice", Text: "lunch?", At: demoNow.Add(-3 * time.Hour).UnixMilli()})
	var asked ipc.Request
	fake := func(r ipc.Request) (ipc.Reply, error) { asked = r; return ipc.Reply{OK: true}, nil }
	snap, _ := numbered("x 4", last, listed)
	if _, _ = command(snap, "x 4", demoNow, fake); asked.Op != "decline" || asked.ID != "h1" {
		t.Fatalf("x 4 asked %+v", asked)
	}
	if _, now := numbered("needs", last, listed); len(now.Msgs) != len(last.Msgs) {
		t.Fatal("needs doesn't list the needs as they are now")
	}
}

package console

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/EledoneAi/murmur/v2/internal/harness"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/charmbracelet/x/ansi"
)

// asideTeam is the demo team with a question by the way in every state: alice's own on their way to bob's reviewer
// (bob reads its answer first), being answered by her api-9b, declined by carol; and dave's to alice's api-1a, idle two
// hours, which waits for her go.
func asideTeam() ipc.Snapshot {
	s := demoSnapshot(demoNow)
	ago := func(d time.Duration) int64 { return demoNow.Add(-d).UnixMilli() }
	a := &s.People[0].Agents[1] // api-1a
	a.Status, a.Needs, a.Since = protocol.StatusIdle, "", ago(2*time.Hour)
	s.Asides = append(s.Asides,
		ipc.Aside{Btw: protocol.Btw{ID: "w3", From: "alice", To: "bob/main-1", Question: "Is the retry limit read anywhere?",
			Note: "answered; waits for bob to share it", At: ago(3 * time.Minute)}, Mine: true, State: ipc.AsideAsking},
		ipc.Aside{Btw: protocol.Btw{ID: "w5", From: "alice", To: "carol/billing-2", Question: "Did the split land?",
			Err: "carol declined the question", At: ago(4 * time.Minute)}, Mine: true, State: ipc.AsideFailed},
		ipc.Aside{Btw: protocol.Btw{ID: "w4", From: "alice", To: "alice/api-9b", Question: "Which test fails?", At: ago(30 * time.Second)},
			Mine: true, State: ipc.AsideRunning},
		ipc.Aside{Btw: protocol.Btw{ID: "w6", From: "dave", To: "alice/api-1a", Question: "Are the OCR fixtures checked in?",
			At: ago(20 * time.Second)}, State: ipc.AsideCold, Cold: 184_000},
	)
	return s
}

// b opens the composer to ask the selected agent by the way, enter asks it under a fresh ID, and esc goes back to
// writing messages. An agent that can't be asked says why on the outcome line.
func TestAsk(t *testing.T) {
	m := demo(110, 30, dark)
	var asked ipc.Request
	m.heard = func(r ipc.Request) { asked = r }
	m.screen()
	m.selected = "agent:bob/main-1"
	if text, _ := m.screen(); !strings.Contains(ansi.Strip(text), "b by the way") {
		t.Fatal("the footer doesn't offer b on bob's reviewer")
	}
	press(m, "b", "S", "t", "u", "c", "k", "?")
	if m.thread == nil || !m.thread.aside || !m.thread.writing || m.thread.to != "bob/main-1" {
		t.Fatalf("b opened %+v", m.thread)
	}
	text, _ := m.screen()
	for _, want := range []string{"↝ by the way: main-1 won't see this", "ask B main-1  Stuck?", "bob reads the answer before it reaches you",
		"⏎ ask  ⇥ recipient  esc stop asking"} {
		if !strings.Contains(ansi.Strip(text), want) {
			t.Errorf("the composer doesn't say %q", want)
		}
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter asks nothing")
	}
	if done := cmd().(doneMsg); done.err || asked.Op != "btw" || asked.To != "bob/main-1" || asked.Text != "Stuck?" ||
		!regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(asked.ID) {
		t.Fatalf("asked %+v: %+v", asked, done)
	}
	press(m, "esc")
	if m.thread.aside || m.thread.input.Placeholder != "write to send" {
		t.Fatal("esc doesn't go back to writing messages")
	}

	for _, c := range []struct{ addr, why string }{
		{"dave/infra-1", "dave turned questions by the way off"},
		{"carol/web-2", "web-2 can't be asked by the way"}, // the inspector says the rest, which doesn't fit
		{"alice/notes", codex},
	} {
		m := demo(110, 30, dark)
		m.screen()
		m.selected = "agent:" + c.addr
		if text, _ := m.screen(); strings.Contains(ansi.Strip(text), "b by the way") {
			t.Errorf("the footer offers b on %s", c.addr)
		}
		press(m, "b")
		if m.thread != nil || !m.outcome.err || m.outcome.text != c.why {
			t.Errorf("b on %s: %+v", c.addr, m.outcome)
		}
	}

	// your own agents on this laptop can always be asked, even with teammates' questions off, and before they said
	// anything: the daemon lists every one it can ask in Said
	s := demoSnapshot(demoNow)
	for i := range s.People[0].Agents {
		s.People[0].Agents[i].Btw = ""
	}
	s.BtwAnswers, s.Said["api-9b"] = ipc.BtwOff, ""
	m = demo(110, 30, dark)
	m.setSnap(s)
	m.screen()
	m.selected = "agent:alice/api-9b"
	press(m, "b")
	if m.thread == nil || !m.thread.aside || m.thread.to != "alice/api-9b" {
		t.Fatalf("b on your own agent, teammates' questions off: %+v %+v", m.thread, m.outcome)
	}
	if text, _ := m.screen(); !strings.Contains(ansi.Strip(text), "A throwaway copy of its conversation answers, in a few seconds.") {
		t.Fatal("the composer doesn't say how your own agent answers")
	}
}

// cantAsk is why an agent that tells the team nothing about questions by the way can't be asked, when other agents
// show the hub carries them.
const cantAsk = " can't be asked by the way: its coding tool can't copy a conversation, or its Claude Code or murmur is too old"

// What this laptop's daemon says of the agents here that nobody can ask by the way: alice's notes runs in Codex, and
// an agent of an older Claude Code.
const (
	codex    = "notes runs in Codex, which can't be asked by the way"
	olderCC  = " can't be asked by the way: its Claude Code is older than 2.1.292; update Claude Code"
	longName = "billing-rerun-shadow-checks"
)

// btwTeam is the demo team with every way an agent tells the team about questions by the way, on this laptop or not:
// alice's api-2f runs on her other laptop, where she turned teammates' questions off, and her docs-2 on it can't be
// asked at all; her infra-3, here, tells the team nothing, and api-1a only she and her agents can ask; carol's web-2
// shares its name with an agent here; erin is offline.
func btwTeam() ipc.Snapshot {
	s := demoSnapshot(demoNow)
	for addr, btw := range map[string]string{"alice/api-2f": protocol.BtwMine, "alice/docs-2": "", "alice/infra-3": "",
		"alice/web-4c": protocol.BtwAuto, "alice/api-1a": protocol.BtwMine} {
		agentOf(&s, addr).Btw = btw
	}
	delete(s.Said, "api-2f")
	delete(s.Said, "docs-2")
	s.Said["web-2"] = ""
	s.People[4].Agents = []protocol.Agent{{Name: "web-9", Repo: "acme/web", Status: protocol.StatusIdle, Btw: protocol.BtwApprove}}
	return s
}

// agentOf is the agent at addr in s.
func agentOf(s *ipc.Snapshot, addr string) *protocol.Agent {
	person, name, _ := strings.Cut(addr, "/")
	for i := range s.People {
		for j := range s.People[i].Agents {
			if s.People[i].Handle == person && s.People[i].Agents[j].Name == name {
				return &s.People[i].Agents[j]
			}
		}
	}
	panic("no agent " + addr)
}

// Who can ask an agent by the way, by what it tells the team and whose it is, and why the others can't be asked. Your
// own agent on another laptop that only you may ask can be asked; a teammate's can't, and an agent that tells nothing
// can be asked only on its own laptop, by its person.
func TestAskable(t *testing.T) {
	cases := []struct{ addr, why string }{
		{"alice/api-9b", ""},  // approve, here
		{"alice/web-4c", ""},  // auto, here
		{"alice/api-1a", ""},  // mine, here
		{"alice/infra-3", ""}, // nothing, here: this laptop asks it without the hub
		{"alice/api-2f", ""},  // mine, on another laptop
		{"alice/docs-2", "docs-2" + cantAsk},
		{"alice/notes", codex},  // here: this laptop says why
		{"bob/main-1", ""},      // approve
		{"carol/billing-2", ""}, // auto
		{"carol/web-2", "web-2" + cantAsk},
		{"dave/infra-1", "dave turned questions by the way off"},
		{"dave/ingest-1", "dave turned questions by the way off"},
		{"erin/web-9", "erin's laptop is offline; ask web-9 by the way once it's back"},
	}
	b := buildBoard(btwTeam(), demoNow, scopeAll)
	for _, c := range cases {
		r := b.find(c.addr)
		if got := b.askable(r); got != (c.why == "") {
			t.Errorf("askable(%s) = %v", c.addr, got)
		}
		if got := b.whyNot(r); got != c.why {
			t.Errorf("whyNot(%s) = %q, want %q", c.addr, got, c.why)
		}
	}

	// a hub that doesn't carry questions leaves every agent saying nothing: this laptop still asks its own
	s := btwTeam()
	for i := range s.People {
		for j := range s.People[i].Agents {
			s.People[i].Agents[j].Btw = ""
		}
	}
	b = buildBoard(s, demoNow, scopeAll)
	const noHub = " can't be asked by the way: the hub doesn't carry questions, its coding tool can't copy a conversation, or " +
		"its Claude Code or murmur is too old"
	for addr, why := range map[string]string{"alice/infra-3": "", "alice/api-2f": "api-2f" + noHub, "dave/infra-1": "infra-1" + noHub} {
		if got := b.whyNot(b.find(addr)); got != why || b.askable(b.find(addr)) != (why == "") {
			t.Errorf("without the hub, whyNot(%s) = %q, want %q", addr, got, why)
		}
	}
}

// The inspector says who can ask an agent by the way, and how its answers come, or why it can't be asked, whole; the
// plain mode's btw asks what the inspector says can be asked, and says why not otherwise.
func TestWhoCanAsk(t *testing.T) {
	m := demo(84, 40, dark)
	m.setSnap(btwTeam())
	m.unfolded["quiet"] = true // docs-2 idles
	m.screen()
	for addr, want := range map[string]string{
		"alice/api-9b":    "teammates and agents can ask it; you share answers",
		"alice/web-4c":    "teammates and agents can ask it; answers go out on their own",
		"alice/api-1a":    "only you and your agents can ask it; teammates' questions are off",
		"alice/api-2f":    "only you and your agents can ask it; teammates' questions are off",
		"alice/infra-3":   "only you and your agents on this laptop can ask it",
		"alice/docs-2":    "docs-2" + cantAsk,
		"bob/main-1":      "you and your agents can ask it; bob reads answers first",
		"carol/billing-2": "you and your agents can ask it; answers come on their own",
		"carol/web-2":     "web-2" + cantAsk,
		"dave/infra-1":    "dave turned questions by the way off",
	} {
		if got := inspectorSays(m, addr); got != want {
			t.Errorf("the inspector on %s says %q, want %q", addr, got, want)
		}
	}

	s := btwTeam()
	var asked ipc.Request
	fake := func(r ipc.Request) (ipc.Reply, error) { asked = r; return ipc.Reply{OK: true}, nil }
	for line, want := range map[string]string{
		"btw api-2f Which fixtures?":      "Asked alice/api-2f by the way; the answer comes here.",
		"btw infra-3 Which plan?":         "Asked alice/infra-3 by the way; the answer comes here.",
		"btw dave/infra-1 Which version?": "Not asked: dave turned questions by the way off.",
		"btw docs-2 Which examples?":      "Not asked: docs-2" + cantAsk + ".",
	} {
		asked = ipc.Request{}
		reply, _ := command(s, line, demoNow, fake)
		if reply != want || (asked.Op == "btw") != strings.HasPrefix(want, "Asked") {
			t.Errorf("%s: %q, asked %+v", line, reply, asked)
		}
	}
}

// inspectorSays is what the inspector on the agent at addr says of asking it by the way, its lines joined again.
func inspectorSays(m *model, addr string) string {
	m.selected, m.inspect = "agent:"+addr, true
	text, _ := m.screen()
	said := ""
	in := false
	for _, l := range strings.Split(ansi.Strip(text), "\n") {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "BY THE WAY"):
			in, l = true, strings.TrimSpace(strings.TrimPrefix(l, "BY THE WAY"))
		case !in:
			continue
		case l == "" || strings.HasPrefix(l, "↝"):
			in = false
			continue
		}
		if said != "" && !strings.HasSuffix(said, "-") { // wrapped at a space, or after a hyphen
			said += " "
		}
		said += l
	}
	return said
}

// cantAskTeam is btwTeam where alice's laptop says why nobody can ask three of her agents here: notes runs in Codex,
// and web-4c and infra-3, under a long name, run a Claude Code too old; web-4c still tells the team it answers, and
// this laptop still has what it last said. It names main-1 and web-2 too: bob's and carol's keep what they tell.
func cantAskTeam() ipc.Snapshot {
	s := btwTeam()
	agentOf(&s, "alice/infra-3").Name = longName
	delete(s.Said, "infra-3")
	s.CantAsk = map[string]string{"notes": codex, "web-4c": "web-4c" + olderCC, longName: longName + olderCC,
		"main-1": "main-1" + olderCC, "web-2": "web-2" + olderCC}
	return s
}

// One of your agents on this laptop that nobody can ask by the way says why in this laptop's words, wherever the
// console says so: b's outcome, less of it where all of it doesn't fit but never a word cut; the inspector, all of it;
// the plain mode's btw and its sentence about the agent. What the daemon says wins over what the agent tells the team,
// and over Said. Teammates' agents, and yours on another laptop, keep their own reasons.
func TestCantAsk(t *testing.T) {
	s := cantAskTeam()
	b := buildBoard(s, demoNow, scopeAll)
	for addr, why := range map[string]string{
		"alice/notes":       codex,
		"alice/web-4c":      "web-4c" + olderCC,
		"alice/" + longName: longName + olderCC,
		"alice/api-9b":      "",
		"alice/api-2f":      "", // mine, on another laptop
		"alice/docs-2":      "docs-2" + cantAsk,
		"bob/main-1":        "",
		"carol/web-2":       "web-2" + cantAsk,
		"dave/infra-1":      "dave turned questions by the way off",
	} {
		r := b.find(addr)
		if got := b.whyNot(r); b.askable(r) != (why == "") || got != why {
			t.Errorf("askable(%s) = %v, whyNot %q, want %q", addr, b.askable(r), got, why)
		}
	}

	for _, c := range []struct {
		w         int
		addr, why string
	}{
		{110, "alice/notes", codex},
		{110, "alice/web-4c", "web-4c" + olderCC},
		{110, "alice/" + longName, longName + " can't be asked by the way: its Claude Code is older than 2.1.292"},
		{84, "alice/" + longName, longName + " can't be asked by the way"},
		{40, "alice/" + longName, longName + " can't be…"},
	} {
		m := demo(c.w, 30, dark)
		m.setSnap(s)
		m.screen()
		m.selected = "agent:" + c.addr
		if text, _ := m.screen(); strings.Contains(ansi.Strip(text), "b by the way") {
			t.Errorf("the footer offers b on %s", c.addr)
		}
		press(m, "b")
		text, _ := m.screen()
		shown := slices.ContainsFunc(strings.Split(ansi.Strip(text), "\n"), func(l string) bool { return strings.TrimSpace(l) == "× "+c.why })
		if m.thread != nil || !m.outcome.err || m.outcome.text != c.why || !shown {
			t.Errorf("b on %s at %d columns says %q, want %q", c.addr, c.w, m.outcome.text, c.why)
		}
	}

	m := demo(84, 40, dark)
	m.setSnap(s)
	m.unfolded["quiet"] = true
	m.screen()
	for addr, want := range map[string]string{
		"alice/notes":       codex,
		"alice/web-4c":      "web-4c" + olderCC,
		"alice/" + longName: longName + olderCC,
		"bob/main-1":        "you and your agents can ask it; bob reads answers first",
		"carol/web-2":       "web-2" + cantAsk,
	} {
		if got := inspectorSays(m, addr); got != want {
			t.Errorf("the inspector on %s says %q, want %q", addr, got, want)
		}
		fits(t, m, 84, 40, "inspecting "+addr)
	}

	var asked ipc.Request
	fake := func(r ipc.Request) (ipc.Reply, error) { asked = r; return ipc.Reply{OK: true}, nil }
	for line, want := range map[string]string{
		"btw notes Which plan?":       "Not asked: " + codex + ".",
		"btw web-4c Which filters?":   "Not asked: web-4c" + olderCC + ".",
		"btw " + longName + " Which?": "Not asked: " + longName + olderCC + ".",
		"btw bob/main-1 What now?":    "Asked bob/main-1 by the way; the answer comes here.",
		"btw carol/web-2 Which page?": "Not asked: web-2" + cantAsk + ".",
	} {
		asked = ipc.Request{}
		reply, _ := command(s, line, demoNow, fake)
		if reply != want || (asked.Op == "btw") != strings.HasPrefix(want, "Asked") {
			t.Errorf("%s: %q, asked %+v", line, reply, asked)
		}
	}
	who, _ := command(s, "who", demoNow, fake)
	for _, want := range []string{
		"alice/notes is working, in notes: Draft the weekly plan. " + codex + ".",
		"alice/" + longName + " is working, in acme/infra on chore/tf-providers: Upgrade the Terraform providers. " + longName + olderCC + ".",
		"bob/main-1 is working, in acme/api on main: Review the billing config.\n",
		"carol/web-2 is working, in acme/web on feat/search-api: Search endpoint pagination.\n",
	} {
		if !strings.Contains(who+"\n", want) {
			t.Errorf("who doesn't say %q:\n%s", want, who)
		}
	}
	if got := strings.Join(changes(btwTeam(), s, demoNow), "\n"); !strings.Contains(got, "New: alice/"+longName+" is working") ||
		!strings.Contains(got, longName+olderCC+".") {
		t.Errorf("the changes say:\n%s", got)
	}
}

// In a thread, b asks by the way the first of its agents that can be asked, and ⇥ goes round only those.
func TestAskInThread(t *testing.T) {
	m := demo(110, 30, dark)
	m.screen()
	m.selected = "agent:carol/web-2" // Search: web-4c can be asked, carol's web-2 can't
	press(m, "enter", "b")
	if m.thread == nil || !m.thread.aside || m.thread.to != "alice/web-4c" {
		t.Fatalf("b in Search asks %+v", m.thread)
	}
	press(m, "tab")
	if m.thread.to != "alice/web-4c" {
		t.Fatalf("⇥ went to %s, which can't be asked", m.thread.to)
	}
}

// A teammate's answer held for you and a cold question join Needs you in its order; n reaches them, y and x settle
// them, and the window's title counts them.
func TestAsideNeeds(t *testing.T) {
	m := demo(110, 32, dark)
	m.setSnap(asideTeam())
	var asked ipc.Request
	m.heard = func(r ipc.Request) { asked = r }
	m.screen()
	var keys []string
	for _, n := range m.board().needs {
		keys = append(keys, n.key)
	}
	if want := "need:btw:w2 need:msg:m6 need:held:h1 need:btw:w6"; strings.Join(keys, " ") != want {
		t.Fatalf("needs %v, want %s", keys, want)
	}
	if title := m.View().WindowTitle; title != "murmur · 4 need you" {
		t.Fatalf("the title says %q", title)
	}
	press(m, "end", "n")
	if m.selected != "need:btw:w2" {
		t.Fatalf("n went to %s", m.selected)
	}
	press(m, "y") // the answer doesn't fit under the need: y opens it whole, and the next y shares it
	if m.thread == nil || m.thread.focus != "w2" || asked.Op != "" {
		t.Fatalf("y on a held answer not read: %+v, asked %+v", m.thread, asked)
	}
	m.screen()
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if done := cmd().(doneMsg); asked.Op != "btw-approve" || asked.ID != "w2" || done.text != "Shared api-9b's answer with carol" {
		t.Fatalf("y on a held answer: %+v, %+v", asked, done)
	}
	press(m, "esc", "n", "n", "n")
	if m.selected != "need:btw:w6" {
		t.Fatalf("n went to %s", m.selected)
	}
	text, _ := m.screen()
	if !strings.Contains(ansi.Strip(text), "↳ idle 2h; its copy would read 184k tokens") {
		t.Fatalf("the cold question doesn't unfold what it would cost:\n%s", ansi.Strip(text))
	}
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if done := cmd().(doneMsg); asked.Op != "btw-decline" || asked.ID != "w6" || done.text != "Declined; dave is told" {
		t.Fatalf("x on a cold question: %+v, %+v", asked, done)
	}

	// in the thread, y settles the newest that waits: the cold question
	m.selected = "agent:alice/api-9b"
	press(m, "enter")
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if cmd(); asked.Op != "btw-approve" || asked.ID != "w6" {
		t.Fatalf("y in the thread: %+v", asked)
	}
}

// A person's asides are never messages: they link no agents, sit in no message list, and leave the board as it was.
func TestAsidesAreNotMessages(t *testing.T) {
	s := demoSnapshot(demoNow)
	s.Asides = append(s.Asides, ipc.Aside{Btw: protocol.Btw{ID: "w9", From: "alice", To: "dave/infra-1", Question: "Which provider version?",
		Answer: "5.2", At: demoNow.UnixMilli()}, Mine: true, State: ipc.AsideAnswered})
	b := buildBoard(s, demoNow, scopeTeam)
	if len(b.groups) != 2 || b.groupOf(b.find("dave/infra-1")) != nil {
		t.Fatal("a question by the way linked dave/infra-1")
	}
	m := demo(110, 30, dark)
	m.setSnap(s)
	m.screen()
	m.selected = "agent:dave/infra-1"
	press(m, "enter")
	if msgs, _ := m.threadMsgs(); len(msgs) != 0 {
		t.Fatalf("the thread counts asides as messages: %v", msgs)
	}
	if text, _ := m.screen(); !strings.Contains(ansi.Strip(text), "A alice ↝ D infra-1  by the way") || strings.Contains(ansi.Strip(text), "No messages yet") {
		t.Fatal("infra-1's thread doesn't show the aside")
	}
}

// agentAsideTeam is the demo team with agents asking agents by the way: alice's api-9b asked bob's reviewer, and has
// its answer; bob's reviewer asked alice's api-9b, whose answer waits for alice to share it; dave's ingest-1 asked
// alice's docs-2, idle three hours, and the question waits for her go.
func agentAsideTeam() ipc.Snapshot {
	s := demoSnapshot(demoNow)
	ago := func(d time.Duration) int64 { return demoNow.Add(-d).UnixMilli() }
	s.Asides = append(s.Asides,
		ipc.Aside{Btw: protocol.Btw{ID: "a1", From: "alice/api-9b", To: "bob/main-1", Question: "Did your review find anything that blocks the rerun?",
			Answer: "No blockers. I'd only flip the shadow flag's default to off before the rerun.", Tokens: 39_400, At: ago(2 * time.Minute)},
			Mine: true, State: ipc.AsideAnswered},
		ipc.Aside{Btw: protocol.Btw{ID: "a2", From: "bob/main-1", To: "alice/api-9b", Question: "Which config keys does the rerun read? I'm checking each has a default.",
			Answer: "billing.rerun.enabled, billing.retry_limit and billing.certifier; only retry_limit has no default.", Tokens: 48_100,
			At: ago(50 * time.Second)}, State: ipc.AsideHeld},
		ipc.Aside{Btw: protocol.Btw{ID: "a3", From: "dave/ingest-1", To: "alice/docs-2", Question: "Does the API guide document the rerun queue's endpoints?",
			At: ago(15 * time.Second)}, State: ipc.AsideCold, Cold: 162_000},
	)
	return s
}

// An agent's question by the way reads as that agent asking: in the asking agent's thread, the asked agent's and their
// constellation's, in the asker's voice. It links the two on the board as a message would, dashed while it waits for
// you; Needs you names the agent and its person, and the inspector lists what an agent asked as well as what it was
// asked.
func TestAgentAsides(t *testing.T) {
	m := demo(110, 40, dark)
	m.setSnap(agentAsideTeam())
	var asked ipc.Request
	m.heard = func(r ipc.Request) { asked = r }
	m.screen()
	b := m.board()
	ingest, docs := b.find("dave/ingest-1"), b.find("alice/docs-2")
	gr := b.groupOf(ingest)
	if gr == nil || gr != b.groupOf(docs) {
		t.Fatal("dave's ingest-1 asked alice's docs-2 by the way, and isn't in touch with it")
	}
	for _, l := range gr.links {
		if x, y := gr.members[l.a], gr.members[l.b]; (x == ingest || y == ingest) && !l.held {
			t.Fatalf("the question that waits for alice's go draws a solid link: %+v", l)
		}
	}
	n := b.needs[slices.IndexFunc(b.needs, func(n need) bool { return n.key == "need:btw:a2" })]
	if got := needSentence(n); got != "bob's agent main-1 asked your api-9b by the way; the answer waits for you" {
		t.Fatalf("the need says %q", got)
	}
	m.selected = "need:btw:a2"
	text, _ := m.screen()
	if !strings.Contains(ansi.Strip(text), "B main-1 ↝ 9b        by the way “Which config keys") {
		t.Fatalf("Needs you doesn't name the asking agent and its person:\n%s", ansi.Strip(text))
	}
	press(m, "y")
	m.screen()
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if done := cmd().(doneMsg); asked.Op != "btw-approve" || asked.ID != "a2" || done.text != "Shared api-9b's answer with bob's agent main-1" {
		t.Fatalf("y on an agent's held answer: %+v, %+v", asked, done)
	}
	m.thread = nil

	// the asker's thread has it, in the asker's voice; the asked agent's too
	m.selected = "agent:dave/ingest-1"
	press(m, "enter")
	if as := m.threadAsides(); !slices.ContainsFunc(as, func(a ipc.Aside) bool { return a.ID == "a3" }) {
		t.Fatalf("ingest-1's thread leaves out the question it asked: %v", as)
	}
	m.thread = nil
	m.selected = "agent:bob/main-1"
	press(m, "space")
	text, _ = m.screen()
	for _, want := range []string{"↝ asked A api-9b now · answer held for you “", "↝ A api-9b 2m · answered “", "↝ you 6m · answered “"} {
		if !strings.Contains(ansi.Strip(text), want) {
			t.Errorf("bob's reviewer's inspector doesn't say %q:\n%s", want, ansi.Strip(text))
		}
	}

	// the demo's: carol's web-2 asked alice's web-4c, in Search
	m = demo(110, 40, dark)
	m.screen()
	m.selected = "agent:alice/web-4c"
	press(m, "enter")
	if text, _ := m.screen(); !strings.Contains(ansi.Strip(text), "C web-2 ↝ A web-4c  by the way") {
		t.Fatalf("Search's thread doesn't show web-2 asking web-4c:\n%s", ansi.Strip(text))
	}
	press(m, "esc")
	m.selected = "agent:carol/web-2"
	press(m, "space")
	if text, _ := m.screen(); !strings.Contains(ansi.Strip(text), "↝ asked A web-4c 10m · answered “Which query") {
		t.Fatalf("web-2's inspector doesn't list what it asked:\n%s", ansi.Strip(text))
	}
}

// An answer that came after the btw tool stopped waiting reaches the asking agent as a message from the agent asked.
// The console shows it once, as the aside's answer: not in the thread, the links or the plain mode's messages.
func TestAgentAsideDelivered(t *testing.T) {
	s := agentAsideTeam()
	links := func(s ipc.Snapshot) int {
		b := buildBoard(s, demoNow, scopeTeam)
		gr := b.groupOf(b.find("alice/api-9b"))
		n := 0
		for _, l := range gr.links {
			n += l.count
		}
		return n
	}
	before := links(s)
	next := agentAsideTeam()
	next.Msgs = append(next.Msgs, protocol.Msg{ID: "bw-a1", From: "bob/main-1", To: "alice/api-9b", At: demoNow.UnixMilli(),
		Text: "Answer to your question by the way (\"Did your review find anything that blocks the rerun?\"), from a copy of " +
			"bob/main-1's conversation; bob/main-1 itself never saw the question:\nNo blockers. I'd only flip the shadow flag's default to off before the rerun."})
	if got := links(next); got != before {
		t.Fatalf("the delivered answer links again: %d links, %d before", got, before)
	}
	m := demo(110, 40, dark)
	m.setSnap(next)
	m.screen()
	m.selected = "agent:alice/api-9b"
	press(m, "enter")
	if msgs, _ := m.threadMsgs(); slices.ContainsFunc(msgs, func(x protocol.Msg) bool { return x.ID == "bw-a1" }) {
		t.Fatal("the thread shows the delivered answer as a message")
	}
	if got := strings.Join(changes(s, next, demoNow), "\n"); strings.Contains(got, "Message from bob/main-1") {
		t.Fatalf("the plain mode says the delivered answer as a message:\n%s", got)
	}
}

// A message that says what an answer by the way said is a message all the same: only a delivery's id tells it apart.
// With an answer "Yes.", "Yes. I pushed the fix…" stays in the thread, links the agents, is said in the plain mode,
// and one of yours that failed waits in Needs you.
func TestAsideAnswerHidesNoMessage(t *testing.T) {
	before := agentAsideTeam()
	n := len(demoSnapshot(demoNow).Asides)
	before.Asides[n].Answer = "Yes." // a1: alice's api-9b asked bob's reviewer
	before.Asides = append(before.Asides, ipc.Aside{Btw: protocol.Btw{ID: "a9", From: "alice/web-4c", To: "alice/api-9b",
		Question: "Is the index rebuilt?", Answer: "Yes.", At: demoNow.Add(-time.Minute).UnixMilli()}, Mine: true, State: ipc.AsideAnswered})
	after := before
	after.Msgs = append(slices.Clone(before.Msgs),
		protocol.Msg{ID: "m-yes", From: "bob/main-1", To: "alice/api-9b", Text: "Yes. I pushed the fix to feat/billing-v2; rebase on it.",
			At: demoNow.UnixMilli()},
		protocol.Msg{ID: "m-yes2", From: "alice/api-9b", To: "alice/web-4c", Text: "Yes. And the search index too.", At: demoNow.UnixMilli()})
	after.Failed = map[string]string{"m-yes2": "web-4c closed before it read it"}
	if d := deliveries(after); len(d) > 0 {
		t.Fatalf("messages taken for deliveries: %v", d)
	}
	m := demo(110, 40, dark)
	m.setSnap(after)
	m.screen()
	m.selected = "agent:alice/api-9b"
	press(m, "enter")
	if msgs, _ := m.threadMsgs(); !slices.ContainsFunc(msgs, func(x protocol.Msg) bool { return x.ID == "m-yes" }) {
		t.Fatal("the thread leaves out bob's reviewer's message")
	}
	if !slices.ContainsFunc(m.board().needs, func(n need) bool { return n.key == "need:failed:m-yes2" }) {
		t.Fatal("Needs you leaves out the message that failed")
	}
	if got := strings.Join(changes(before, after, demoNow), "\n"); !strings.Contains(got, "Message from bob/main-1 to alice/api-9b: Yes. I pushed the fix") {
		t.Fatalf("the plain mode doesn't say the message:\n%s", got)
	}
}

// The plain mode says agents asking: yours, and teammates' agents asking yours, with the need numbers y and x take.
func TestPlainAgentAsides(t *testing.T) {
	old := demoSnapshot(demoNow)
	next := agentAsideTeam()
	next.Asides[len(old.Asides)].Answer, next.Asides[len(old.Asides)].State = "", ipc.AsideAsking // a1, on its way
	got := strings.Join(changes(old, next, demoNow), "\n")
	b := buildBoard(next, demoNow, scopeAll)
	nth := func(key string) string {
		return strconv.Itoa(slices.IndexFunc(b.needs, func(n need) bool { return n.key == key }) + 1)
	}
	for _, want := range []string{
		"Your api-9b asked bob/main-1 by the way: Did your review find anything that blocks the rerun?",
		"bob's agent main-1 asked your api-9b by the way; the answer waits for you: y " + nth("need:btw:a2") + " shares it, x " +
			nth("need:btw:a2") + " drops it.",
		"   Answer: billing.rerun.enabled, billing.retry_limit and billing.certifier; only retry_limit has no default.",
		"dave's agent ingest-1 asked your docs-2 by the way, idle long; its copy would read 162k tokens: y " + nth("need:btw:a3") +
			" lets it run, x " + nth("need:btw:a3") + " declines it.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the changes don't say %q:\n%s", want, got)
		}
	}
	if text := strings.Join(summary(next, demoNow), "\n"); !strings.Contains(text,
		"Your api-9b's question by the way to bob/main-1 (on its way): Did your review find anything that blocks the rerun?") {
		t.Errorf("the summary says:\n%s", text)
	}
	got = strings.Join(changes(next, agentAsideTeam(), demoNow), "\n")
	if want := "bob's agent main-1 answered your api-9b by the way: No blockers. I'd only flip the shadow flag's default to off before the rerun. (39k tokens read)."; !strings.Contains(got, want) {
		t.Errorf("the answer to your agent isn't said:\n%s", got)
	}

	fake := func(ipc.Request) (ipc.Reply, error) { return ipc.Reply{OK: true}, nil }
	if reply, _ := command(next, "needs", demoNow, fake); !strings.Contains(reply, nth("need:btw:a2")+". bob's agent main-1 asked your api-9b by the way; the answer waits for you, waiting") {
		t.Errorf("needs says:\n%s", reply)
	}
	if reply, _ := command(next, "x "+nth("need:btw:a3"), demoNow, fake); reply != "Declined; dave's agent ingest-1 is told." {
		t.Errorf("x on an agent's cold question: %q", reply)
	}
	failed := agentAsideTeam()
	failed.Asides[len(old.Asides)+2].State, failed.Asides[len(old.Asides)+2].Err = ipc.AsideFailed, "alice declined the question"
	if got := strings.Join(changes(agentAsideTeam(), failed, demoNow), "\n"); got != "dave's agent ingest-1 asked your docs-2 by the way and got no answer: alice declined the question." {
		t.Errorf("a declined agent's question: %q", got)
	}
}

// murmur's btw tool call is talk in an agent's texture, like send: ⇢ on its row and in the inspector.
func TestBtwCallIsTalk(t *testing.T) {
	call, _ := harness.CallOf("/repo", "mcp__murmur__btw", json.RawMessage(`{"to":"bob/main-1","question":"stuck?"}`))
	if call.Kind != protocol.KindMessage {
		t.Fatalf("the btw tool call is kind %q, not a message", call.Kind)
	}
	s := demoSnapshot(demoNow)
	a := &s.People[0].Agents[0] // api-9b
	a.Kinds, a.Doing = "rrrr"+string(rune(call.Kind)), call.Doing
	m := demo(140, 40, dark)
	m.setSnap(s)
	m.screen()
	m.selected = "agent:alice/api-9b"
	press(m, "space")
	text, _ := m.screen()
	for _, want := range []string{"····⇢", "talk 20%", "murmur · btw"} {
		if !strings.Contains(ansi.Strip(text), want) {
			t.Errorf("the btw call doesn't show %q:\n%s", want, ansi.Strip(text))
		}
	}
}

// Asides fit the screen at every size, in ASCII too: a selected one unfolded in Needs you, the composer asking, the
// thread with every state, agents asking too.
func TestAsidesFit(t *testing.T) {
	for _, g := range []glyphs{unicodeGlyphs, asciiGlyphs} {
		for _, size := range [][2]int{{40, 10}, {60, 24}, {84, 30}, {110, 12}, {160, 50}} {
			w, h := size[0], size[1]
			m := demo(w, h, dark)
			m.g = g
			s := asideTeam()
			s.Asides = append(s.Asides, agentAsideTeam().Asides[len(demoSnapshot(demoNow).Asides):]...)
			m.setSnap(s)
			m.screen()
			for _, sel := range []string{"need:btw:w2", "need:btw:w6", "need:btw:a2", "need:btw:a3"} {
				m.selected = sel
				fits(t, m, w, h, sel)
			}
			for _, sel := range []string{"need:btw:w2", "need:btw:a2"} { // its answer, whole, in the thread
				m.selected = sel
				press(m, "y")
				fits(t, m, w, h, "reading "+sel)
				press(m, "y")
				fits(t, m, w, h, "reading on "+sel)
				m.thread = nil
			}
			m.selected = "agent:bob/main-1"
			press(m, "b")
			fits(t, m, w, h, "asking bob/main-1")
			m.thread.scroll = 100_000
			fits(t, m, w, h, "the thread's oldest")
		}
	}
}

func fits(t *testing.T, m *model, w, h int, where string) {
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

// The plain mode says asides as they change, numbers the ones that wait for you, and asks with btw.
func TestPlainAsides(t *testing.T) {
	old := demoSnapshot(demoNow)
	old.Asides = []ipc.Aside{{Btw: protocol.Btw{ID: "w1", From: "alice", To: "bob/main-1", Question: "What's left?", At: demoNow.UnixMilli()},
		Mine: true, State: ipc.AsideAsking}}
	next := demoSnapshot(demoNow)
	next.Asides = append(next.Asides, asideTeam().Asides[len(next.Asides):]...)
	got := strings.Join(changes(old, next, demoNow), "\n")
	for _, want := range []string{
		"bob's agent main-1 answered: Two things: the shadow flag still defaults to on, and nothing reads the retry limit. I'm checking certifier.go next. (41k tokens read).",
		"carol asked your api-9b by the way; the answer waits for you: y 2 shares it, x 2 drops it.",
		"   Question: Does rerun.go still call the old certifier?",
		"   Answer: Yes, once: billing/rerun.go:88 calls Rerun() through the alias. It stays until #1831 merges.",
		"You asked bob/main-1 by the way: Is the retry limit read anywhere?\nYour question by the way to bob/main-1: answered; waits for bob to share it.",
		"carol's agent billing-2 couldn't answer your question by the way: carol declined the question.",
		"You asked alice/api-9b by the way: Which test fails?\nYour question by the way to alice/api-9b: being answered by a copy of its conversation.",
		"dave asked your api-1a by the way, idle long; its copy would read 184k tokens: y 5 lets it run, x 5 declines it.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the changes don't say %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Needs you: carol asked") {
		t.Errorf("a held answer is said twice:\n%s", got)
	}

	s := asideTeam()
	var asked ipc.Request
	fake := func(r ipc.Request) (ipc.Reply, error) { asked = r; return ipc.Reply{OK: true}, nil }
	if reply, _ := command(s, "btw bob/main-1 What now?", demoNow, fake); reply != "Asked bob/main-1 by the way; the answer comes here." ||
		asked.Op != "btw" || asked.To != "bob/main-1" || asked.Text != "What now?" || len(asked.ID) != 16 {
		t.Fatalf("btw: %q, asked %+v", reply, asked)
	}
	if command(s, "btw api-9b Which test?", demoNow, fake); asked.To != "alice/api-9b" {
		t.Fatalf("btw by your agent's name asked %s", asked.To)
	}
	asked = ipc.Request{}
	if reply, _ := command(s, "btw dave/infra-1 hi", demoNow, fake); reply != "Not asked: dave turned questions by the way off." || asked.Op != "" {
		t.Fatalf("btw to an agent that can't be asked: %q", reply)
	}
	b := buildBoard(s, demoNow, scopeAll)
	nth := func(key string) string { // the need's number, as needs says it
		return strconv.Itoa(slices.IndexFunc(b.needs, func(n need) bool { return n.key == key }) + 1)
	}
	if reply, _ := command(s, "y "+nth("need:btw:w2"), demoNow, fake); reply != "Shared api-9b's answer with carol." || asked.Op != "btw-approve" || asked.ID != "w2" {
		t.Fatalf("y on a held answer: %q, asked %+v", reply, asked)
	}
	if reply, _ := command(s, "x "+nth("need:btw:w6"), demoNow, fake); reply != "Declined; dave is told." || asked.Op != "btw-decline" || asked.ID != "w6" {
		t.Fatalf("x on a cold question: %q, asked %+v", reply, asked)
	}
	if reply, _ := command(s, "needs", demoNow, fake); !strings.Contains(reply, "carol asked your api-9b by the way; the answer waits for you, waiting") ||
		!strings.Contains(reply, "   Answer: Yes, once") {
		t.Fatalf("needs says:\n%s", reply)
	}
	if reply, _ := command(s, "help", demoNow, fake); !strings.Contains(reply, "btw AGENT QUESTION") {
		t.Fatal("help doesn't list btw")
	}
	if text := strings.Join(summary(s, demoNow), "\n"); !strings.Contains(text, "Your question by the way to bob/main-1 (answered; waits for bob to share it): Is the retry limit read anywhere?") ||
		!strings.Contains(text, "It last said: The rerun passes locally") {
		t.Fatalf("the summary says:\n%s", text)
	}
}

// Tokens read as people say them.
func TestTokens(t *testing.T) {
	for n, want := range map[int]string{0: "0", 900: "900", 41_200: "41k", 184_000: "184k", 999_499: "999k", 999_500: "1.0M", 1_260_000: "1.3M"} {
		if got := tokens(n); got != want {
			t.Errorf("tokens(%d) = %q, want %q", n, got, want)
		}
	}
}

// longAnswerTeam is asideTeam with an answer held for alice that is taller than a small screen.
func longAnswerTeam() ipc.Snapshot {
	s := asideTeam()
	i := slices.IndexFunc(s.Asides, func(a ipc.Aside) bool { return a.ID == "w2" })
	var parts []string
	for n := 1; n <= 12; n++ {
		parts = append(parts, fmt.Sprintf("Step %d: billing/rerun.go:%d still calls Rerun() through the alias, and certifier.go reads it once more.", n, 80+n))
	}
	s.Asides[i].Answer = strings.Join(parts, "\n") + "\nLast: the deploy key is in the vault, not here."
	return s
}

// y shares an answer only once all of it has been on screen. In Needs you, an answer that fits whole under the need
// is shared at once; a longer one opens in its thread, scrolled to it, and the next y shares it. One taller than the
// screen opens at its first line: y scrolls on to what you haven't had on screen, and shares once you have had all of
// it. Cold questions run at once, as before.
func TestReadBeforeShare(t *testing.T) {
	var asked ipc.Request
	heard := func(m *model) { m.heard = func(r ipc.Request) { asked = r } }

	s := asideTeam()
	i := slices.IndexFunc(s.Asides, func(a ipc.Aside) bool { return a.ID == "w2" })
	s.Asides[i].Answer = "Yes, once, through the alias."
	m := demo(110, 32, dark)
	heard(m)
	m.setSnap(s)
	m.screen()
	m.selected = "need:btw:w2"
	if text, _ := m.screen(); !strings.Contains(ansi.Strip(text), "↳ “Yes, once, through the alias.”") || !strings.Contains(ansi.Strip(text), "y share  x drop") {
		t.Fatalf("a short answer doesn't show whole with y share:\n%s", ansi.Strip(text))
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"}); cmd == nil || m.thread != nil {
		t.Fatal("y doesn't share an answer shown whole")
	} else if cmd(); asked.Op != "btw-approve" || asked.ID != "w2" {
		t.Fatalf("y asked %+v", asked)
	}

	asked = ipc.Request{}
	m = demo(84, 24, dark)
	heard(m)
	m.setSnap(longAnswerTeam())
	m.screen()
	m.selected = "need:btw:w2"
	if text, _ := m.screen(); !strings.Contains(ansi.Strip(text), "y read  x drop") {
		t.Fatalf("Needs you offers to share an answer it cuts:\n%s", ansi.Strip(text))
	}
	press(m, "y")
	seen, _ := m.screen()
	if m.thread == nil || !strings.Contains(ansi.Strip(seen), "↳ Step 1: billing/rerun.go:81") || asked.Op != "" {
		t.Fatalf("y doesn't open the answer at its start:\n%s", ansi.Strip(seen))
	}
	for n := 0; asked.Op == "" && n < 10; n++ {
		_, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
		if cmd != nil {
			cmd()
			break
		}
		text, _ := m.screen()
		seen += ansi.Strip(text)
	}
	if asked.Op != "btw-approve" || asked.ID != "w2" {
		t.Fatalf("y never shared: %+v", asked)
	}
	if !strings.Contains(seen, "Last: the deploy key is in the vault") || !strings.Contains(seen, "Step 12") {
		t.Fatal("shared before the end of the answer was on screen")
	}
}

// Keys show only under what y and x settle: in a thread opened from the board, the newest of what waits for you,
// never the others.
func TestThreadKeysOnlyWhereTheyAct(t *testing.T) {
	m := demo(110, 48, dark)
	m.setSnap(asideTeam())
	var asked ipc.Request
	m.heard = func(r ipc.Request) { asked = r }
	m.screen()
	m.selected = "agent:alice/api-9b"
	press(m, "enter")
	text := ansi.Strip(must(m.screen()))
	if strings.Count(text, "y go   x decline") != 1 || strings.Contains(text, "y share") || strings.Contains(text, "y let through") {
		t.Fatalf("keys under what they don't settle:\n%s", text)
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if cmd(); asked.Op != "btw-approve" || asked.ID != "w6" {
		t.Fatalf("y settled %+v", asked)
	}
}

func must(text string, _ []line) string { return text }

// The plain mode says a held answer whole wherever it numbers it for y: as it comes, in needs, and in what it says
// first.
func TestPlainSaysHeldAnswersWhole(t *testing.T) {
	s := longAnswerTeam()
	i := slices.IndexFunc(s.Asides, func(a ipc.Aside) bool { return a.ID == "w2" })
	whole := "   Answer: " + oneLine(s.Asides[i].Answer)
	if got := strings.Join(summary(s, demoNow), "\n"); !strings.Contains(got, whole) {
		t.Fatalf("the summary doesn't say the answer whole:\n%s", got)
	}
	fake := func(ipc.Request) (ipc.Reply, error) { return ipc.Reply{OK: true}, nil }
	if reply, _ := command(s, "needs", demoNow, fake); !strings.Contains(reply, whole) {
		t.Fatalf("needs doesn't say the answer whole:\n%s", reply)
	}
	before := longAnswerTeam()
	before.Asides = slices.DeleteFunc(before.Asides, func(a ipc.Aside) bool { return a.ID == "w2" })
	if got := strings.Join(changes(before, s, demoNow), "\n"); !strings.Contains(got, whole) {
		t.Fatalf("the changes don't say the answer whole:\n%s", got)
	}
}

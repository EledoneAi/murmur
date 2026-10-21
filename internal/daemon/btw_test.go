package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/harness"
	"github.com/EledoneAi/murmur/v2/internal/harness/claude"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// fakeAsker answers questions by the way as a copy of a Claude Code session would, without Claude Code.
type fakeAsker struct {
	mu         sync.Mutex
	asked      []harness.Question
	sessions   []harness.Session // the sessions copied, as Ask got them
	running    map[string]int    // copies running, by session id
	now, most  int               // copies running now, and at most, on the laptop
	mostOneRan int               // copies of one session at once, at most
	gate       chan struct{}     // when set, each copy waits for a value from it
	err        error
	during     func(q harness.Question) // runs while the copy runs
}

func (f *fakeAsker) Ask(ctx context.Context, s harness.Session, q harness.Question) (harness.Answer, error) {
	f.mu.Lock()
	f.asked, f.sessions = append(f.asked, q), append(f.sessions, s)
	if f.running == nil {
		f.running = map[string]int{}
	}
	f.now++
	f.running[s.ID]++
	f.most, f.mostOneRan = max(f.most, f.now), max(f.mostOneRan, f.running[s.ID])
	gate, err, during := f.gate, f.err, f.during
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.now--
		f.running[s.ID]--
		f.mu.Unlock()
	}()
	if during != nil {
		during(q)
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return harness.Answer{}, ctx.Err()
		}
	}
	if err != nil {
		return harness.Answer{}, err
	}
	return harness.Answer{Text: s.Name + ": " + q.Text, Tokens: 1200, Took: time.Second}, nil
}

func (f *fakeAsker) questions() []harness.Question {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

// fakeAskers makes a fake Claude Code answer the questions by the way d's agents are asked, and returns it.
func fakeAskers(d *Daemon) *fakeAsker {
	f := &fakeAsker{}
	d.askers = func(name string) harness.Asker {
		if name == claude.Name {
			return f
		}
		return nil
	}
	return f
}

// btwLaptop is alice's daemon, in MURMUR_HOME as the test set it, online with a hub that carries questions by the
// way, with these sessions and a fake Claude Code that answers questions.
func btwLaptop(t *testing.T, sessions ...*session) (*Daemon, *fakeAsker) {
	t.Helper()
	d := newDaemon("alice")
	f := fakeAskers(d)
	for _, s := range sessions {
		d.add(s)
	}
	d.out, d.state, d.hubBtw, d.welcomed = make(chan protocol.Frame, 64), "online", true, true
	d.btwOut = d.out
	d.loadHistory()
	t.Cleanup(func() { d.hist.Close() })
	return d, f
}

// call sends the daemon one request, as a client of its socket does, and reads the reply.
func call(d *Daemon, req ipc.Request) ipc.Reply {
	c, them := net.Pipe()
	defer them.Close()
	go d.serve(c)
	var r ipc.Reply
	_ = json.NewEncoder(them).Encode(req)
	_ = json.NewDecoder(them).Decode(&r)
	return r
}

// shown records the desktop notifications a daemon shows.
func shown(d *Daemon) chan string {
	ch := make(chan string, 64)
	d.notify = func(title, text string) { ch <- title + " | " + text }
	return ch
}

func expectShown(t *testing.T, ch chan string, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("notification %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("no notification, want %q", want)
	}
}

// nextFrame is the next frame of this kind the laptop sends on out.
func nextFrame(t *testing.T, out chan protocol.Frame, kind string) protocol.Frame {
	t.Helper()
	for {
		select {
		case f := <-out:
			if f.T == kind {
				return f
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("no %s frame", kind)
		}
	}
}

// noFrame fails when the laptop sends a frame on out within wait.
func noFrame(t *testing.T, out chan protocol.Frame, wait time.Duration) {
	t.Helper()
	select {
	case f := <-out:
		t.Fatalf("sent %+v", f)
	case <-time.After(wait):
	}
}

// asideOf is the aside under id: this person's own when mine, else one asked of their agents.
func asideOf(d *Daemon, id string, mine bool) aside {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range d.asides {
		if a.ID == id && a.Mine == mine {
			return *a
		}
	}
	return aside{}
}

// ask is a teammate's question, as the hub brings it.
func ask(d *Daemon, id, from, to, question string) {
	d.onFrame(protocol.Frame{T: protocol.FrameBtw, Btw: &protocol.Btw{ID: id, From: from, To: to, Question: question, At: 1}})
}

var uuid4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// This person's question for one of this laptop's agents runs a copy here, with no hub: the copy hears it from "your
// person", what the agent is doing, and runs under a session id of its own. An agent whose coding tool can't be
// asked, a question without a hub that carries it, and a question too long are refused at once.
func TestBtwAsksOwnAgentHere(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	a1 := &session{id: "s1", name: "a1", status: protocol.StatusBusy}
	a1.act.Doing, a1.act.Tokens = "Running go test", 52000
	d, f := btwLaptop(t, a1, &session{harness: "codex", id: "t1", name: "codex-1"})
	d.hubBtw = false
	seen := shown(d)

	f.gate = make(chan struct{}) // the copy answers once murmur btw waits for it, as a real one takes seconds
	if r := call(d, ipc.Request{Op: "btw", ID: "q1", To: "a1", Text: "what are you stuck on?"}); !r.OK {
		t.Fatalf("btw: %+v", r)
	}
	waited := make(chan ipc.Reply, 1)
	go func() { waited <- call(d, ipc.Request{Op: "btw-wait", ID: "q1"}) }()
	eventually(t, "btw-wait waits", func() bool { d.mu.Lock(); defer d.mu.Unlock(); return d.waiters["q1"] == 1 })
	f.gate <- struct{}{}
	if r := <-waited; !r.OK || r.Text != "a1: what are you stuck on?" {
		t.Fatalf("btw-wait: %+v", r)
	}
	f.mu.Lock()
	f.gate = nil
	f.mu.Unlock()
	q := f.questions()
	if len(q) != 1 || q[0].From != "your person" || q[0].State != "in the middle of Running go test" || !uuid4.MatchString(q[0].Fork) ||
		q[0].Tokens != 52000 {
		t.Fatalf("the copy was asked %+v", q)
	}
	if a := asideOf(d, "q1", true); a.State != ipc.AsideAnswered || a.To != "alice/a1" || a.Tokens != 1200 || !a.Local {
		t.Fatalf("aside %+v", a)
	}
	if len(d.out) != 0 {
		t.Fatalf("a question for this laptop's agent went to the hub: %+v", <-d.out)
	}

	// asked from the console, with nobody waiting on it: a notification brings the answer
	if r := call(d, ipc.Request{Op: "btw", ID: "q2", To: "alice/a1", Text: "and the tests?\nall of them"}); !r.OK {
		t.Fatalf("btw: %+v", r)
	}
	expectShown(t, seen, "Murmur: alice/a1 answered by the way | a1: and the tests?")
	d.mu.Lock()
	forks := len(d.forks)
	d.mu.Unlock()
	if forks != 0 {
		t.Fatalf("%d copies still left out of the sessions", forks)
	}

	for _, c := range []struct {
		req  ipc.Request
		want string
	}{
		{ipc.Request{ID: "q3", To: "codex-1", Text: "hi"}, "codex-1 runs in Codex, which can't be asked by the way"},
		{ipc.Request{ID: "q4", To: "bob/b1", Text: "hi"}, noHubBtw},
		{ipc.Request{ID: "q5", To: "bob", Text: "hi"}, "ask one agent, like bob/main-1"},
		{ipc.Request{ID: "q 6", To: "a1", Text: "hi"}, "needs an id"},
		{ipc.Request{ID: "q7", To: "a1", Text: strings.Repeat("é", protocol.MaxBtwQuestion+1)}, "the question is 2001 characters"},
		{ipc.Request{ID: "q1", To: "a1", Text: "another question"}, "asked under this id already"},
	} {
		c.req.Op = "btw"
		if r := call(d, c.req); r.OK || !strings.Contains(r.Err, c.want) {
			t.Errorf("%+v: %+v, want %q", c.req, r, c.want)
		}
	}
	d.mu.Lock()
	d.welcomed, d.state = false, "reconnecting"
	d.mu.Unlock()
	if r := call(d, ipc.Request{Op: "btw", ID: "q8", To: "bob/b1", Text: "hi"}); r.Err != "Murmur is reconnecting; ask again once it is online" {
		t.Fatalf("before any hub: %+v", r)
	}
	if r := call(d, ipc.Request{Op: "btw", ID: "q1", To: "a1", Text: "what are you stuck on?"}); !r.OK || len(f.questions()) != 2 {
		t.Fatalf("the same call again: %+v, %d copies", r, len(f.questions()))
	}
}

// What a session is doing, as its copy hears it: where its turn stands, as its transcript says, or else its last tool
// call.
func TestCopyState(t *testing.T) {
	ago := time.Now().Add(-3*time.Hour - time.Minute).UnixMilli()
	busy := func(turn, call string) session {
		return session{status: protocol.StatusBusy, act: harness.Activity{Doing: "Editing api.go", Turn: turn, TurnCall: call}}
	}
	for _, c := range []struct {
		s    session
		want string
	}{
		{busy(harness.TurnTool, "Running go"), "in the middle of Running go, waiting for its result"},
		{busy(harness.TurnReply, ""), "writing its reply to the last message in this conversation"},
		{busy(harness.TurnResult, "Reading api.go"), "working on the result of Reading api.go"},
		{busy(harness.TurnResult, ""), "working"}, // the result of a call too long to read
		{busy(harness.TurnEnded, ""), "working"},
		{busy(harness.TurnUnknown, ""), "in the middle of Editing api.go"},
		{session{status: protocol.StatusBusy}, "working"},
		{session{status: protocol.StatusWaiting, act: harness.Activity{Turn: harness.TurnTool, TurnCall: "Running rm"}}, "waiting for its person"},
		{session{status: protocol.StatusIdle, since: ago, act: harness.Activity{Turn: harness.TurnReply}}, "idle for 3h"},
		{session{status: protocol.StatusIdle}, "idle"},
	} {
		if got := stateOf(&c.s); got != c.want {
			t.Errorf("%s, turn %q: %q, want %q", c.s.status, c.s.act.Turn, got, c.want)
		}
	}
}

// who's last words point to asking by the way: an agent, which asks through its MCP server, with its btw tool; its
// person with the command.
func TestWhoHintsAtBtw(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d, _ := btwLaptop(t)
	for _, c := range []struct {
		req  ipc.Request
		want string
	}{
		{ipc.Request{Op: "who"}, "or ask an agent by the way with murmur btw, without interrupting it."},
		{ipc.Request{Op: "who", Harness: "claude", Session: "s1"}, "or ask an agent by the way with the btw tool, without interrupting it."},
	} {
		if r := call(d, c.req); !r.OK || !strings.Contains(r.Text, "to a handle (bob) to reach the person; "+c.want) {
			t.Errorf("%+v: %+v", c.req, r)
		}
	}
}

// A question for a teammate's agent goes through the hub; its notes keep it asking, its answer settles it, and an
// answer for another agent under its id is no answer. A question without a final answer goes again under its id on
// each new connection, once; one asked while offline waits for the connection; a day without an answer fails it.
func TestBtwThroughTheHub(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d, _ := btwLaptop(t)
	seen := shown(d)
	connect := func() {
		d.mu.Lock()
		d.out = make(chan protocol.Frame, 64)
		d.mu.Unlock()
	}
	answer := func(b protocol.Btw) { d.onFrame(protocol.Frame{T: protocol.FrameBtwAnswer, Btw: &b}) }

	if r := call(d, ipc.Request{Op: "btw", ID: "q1", To: "Bob/b1", Text: "which branch has the fix?"}); !r.OK {
		t.Fatalf("btw: %+v", r)
	}
	if f := nextFrame(t, d.out, protocol.FrameBtw); f.Btw.ID != "q1" || f.Btw.To != "bob/b1" || f.Btw.Question != "which branch has the fix?" {
		t.Fatalf("to the hub: %+v", f.Btw)
	}
	answer(protocol.Btw{ID: "q1", To: "carol/c1", Answer: "not from bob"})
	answer(protocol.Btw{ID: "q1", From: "alice", To: "bob/b1", Note: "answered; waits for bob to share it"})
	if a := asideOf(d, "q1", true); a.State != ipc.AsideAsking || a.Note != "answered; waits for bob to share it" {
		t.Fatalf("after a note: %+v", a)
	}
	waited := make(chan ipc.Reply, 1)
	go func() { waited <- call(d, ipc.Request{Op: "btw-wait", ID: "q1"}) }()
	eventually(t, "btw-wait waits", func() bool { d.mu.Lock(); defer d.mu.Unlock(); return d.waiters["q1"] == 1 })
	answer(protocol.Btw{ID: "q1", From: "alice", To: "bob/b1", Answer: "feat/x, since this morning", Tokens: 900})
	if r := <-waited; !r.OK || r.Text != "feat/x, since this morning" {
		t.Fatalf("btw-wait: %+v", r)
	}
	if a := asideOf(d, "q1", true); a.State != ipc.AsideAnswered || a.Tokens != 900 {
		t.Fatalf("answered: %+v", a)
	}
	answer(protocol.Btw{ID: "q1", To: "bob/b1", Err: "too late"})
	if a := asideOf(d, "q1", true); a.State != ipc.AsideAnswered {
		t.Fatalf("a final answer changed: %+v", a)
	}

	// a new connection: the open question goes again, once
	call(d, ipc.Request{Op: "btw", ID: "q2", To: "bob/b1", Text: "and the tests?"})
	nextFrame(t, d.out, protocol.FrameBtw)
	welcome := func() {
		d.onFrame(protocol.Frame{T: protocol.FrameWelcome, Me: &protocol.Person{Handle: "alice"}, Can: []string{protocol.CanBtw}})
	}
	connect()
	welcome()
	if f := nextFrame(t, d.out, protocol.FrameBtw); f.Btw.ID != "q2" {
		t.Fatalf("asked again: %+v", f.Btw)
	}
	welcome() // the team's settings changed: the same connection
	noFrame(t, d.out, 50*time.Millisecond)

	// offline, it waits for the connection; whoever waits for it may stop waiting
	d.mu.Lock()
	d.out = nil
	d.mu.Unlock()
	if r := call(d, ipc.Request{Op: "btw", ID: "q3", To: "bob/b1", Text: "lunch?"}); !r.OK || r.Note != "waits for murmur to reconnect" {
		t.Fatalf("offline: %+v", r)
	}
	c, them := net.Pipe()
	go d.serve(c)
	_ = json.NewEncoder(them).Encode(ipc.Request{Op: "btw-wait", ID: "q3"})
	waiters := func() int { d.mu.Lock(); defer d.mu.Unlock(); return d.waiters["q3"] }
	eventually(t, "btw-wait waits", func() bool { return waiters() == 1 })
	them.Close()
	eventually(t, "btw-wait stops waiting", func() bool { return waiters() == 0 })
	connect()
	welcome()
	got := map[string]bool{}
	for range 2 {
		got[nextFrame(t, d.out, protocol.FrameBtw).Btw.ID] = true
	}
	if !got["q2"] || !got["q3"] || asideOf(d, "q3", true).Note != "" {
		t.Fatalf("on reconnecting: %v, %+v", got, asideOf(d, "q3", true))
	}

	// a refusal is final; a day without an answer fails the question
	answer(protocol.Btw{ID: "q3", To: "bob/b1", Err: "bob turned questions by the way off"})
	expectShown(t, seen, "Murmur: no answer from bob/b1 | bob turned questions by the way off")
	d.mu.Lock()
	d.asides["q2"].At = time.Now().Add(-asideTTL - time.Minute).UnixMilli()
	d.asides["q2"].Last = d.asides["q2"].At
	d.pruneLocked() // what still asks fails before it goes
	d.mu.Unlock()
	d.btwTick(askAgain)
	if a := asideOf(d, "q2", true); a.State != ipc.AsideFailed || a.Err != "no answer within a day" {
		t.Fatalf("after a day: %+v", a)
	}
	expectShown(t, seen, "Murmur: no answer from bob/b1 | no answer within a day")

	// a hub that no longer carries questions fails those waiting
	call(d, ipc.Request{Op: "btw", ID: "q4", To: "bob/b1", Text: "still there?"})
	d.onFrame(protocol.Frame{T: protocol.FrameWelcome, Me: &protocol.Person{Handle: "alice"}})
	if a := asideOf(d, "q4", true); a.State != ipc.AsideFailed {
		t.Fatalf("on a hub without btw: %+v", a)
	}
	if r := call(d, ipc.Request{Op: "btw", ID: "q5", To: "bob/b1", Text: "hi"}); r.Err != noHubBtw {
		t.Fatalf("on a hub without btw: %+v", r)
	}
}

// A question without its answer goes to the hub again every askAgain while it is open, connected or not before: the
// laptop holding the agent may have restarted before its answer left, and answers a question it has seen from its
// history. Asked again, it says so, for a hub that restarted since; one that never went doesn't. One that went out a
// moment ago, and one answered, don't go again.
func TestBtwAsksAgain(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d, _ := btwLaptop(t)
	call(d, ipc.Request{Op: "btw", ID: "q1", To: "bob/b1", Text: "which branch?"})
	if f := nextFrame(t, d.out, protocol.FrameBtw); f.Btw.Again || f.Btw.From != "alice" {
		t.Fatalf("asked: %+v", f.Btw)
	}
	d.btwTick(askAgain)
	noFrame(t, d.out, 50*time.Millisecond)
	d.btwTick(0)
	if f := nextFrame(t, d.out, protocol.FrameBtw); f.Btw.ID != "q1" || f.Btw.To != "bob/b1" || f.Btw.Question != "which branch?" || !f.Btw.Again {
		t.Fatalf("asked again: %+v", f.Btw)
	}

	out := d.out
	d.mu.Lock()
	d.out = nil
	d.mu.Unlock()
	if r := call(d, ipc.Request{Op: "btw", ID: "q2", To: "bob/b1", Text: "lunch?"}); r.Note != offline {
		t.Fatalf("offline: %+v", r)
	}
	d.btwTick(0)
	d.mu.Lock()
	d.out = out
	d.mu.Unlock()
	d.btwTick(0)
	got := map[string]bool{}
	for range 2 {
		b := nextFrame(t, d.out, protocol.FrameBtw).Btw
		got[b.ID] = b.Again
	}
	if again, ok := got["q2"]; !got["q1"] || !ok || again || asideOf(d, "q2", true).Note != "" {
		t.Fatalf("once connected (asked again): %v, %+v", got, asideOf(d, "q2", true))
	}

	for _, id := range []string{"q1", "q2"} {
		d.onFrame(protocol.Frame{T: protocol.FrameBtwAnswer, Btw: &protocol.Btw{ID: id, To: "bob/b1", Answer: "feat/x"}})
	}
	d.btwTick(0)
	noFrame(t, d.out, 50*time.Millisecond)
}

// A teammate's answer waits for this person by default: the copy runs at once, the asker hears that the answer waits,
// and this person shares it or drops it. A question asked again is answered from what this laptop has, without a
// second copy; another question under the same id is refused.
func TestBtwAnswerWaitsForItsPerson(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d, f := btwLaptop(t, &session{id: "s1", name: "a1", status: protocol.StatusBusy})
	seen := shown(d)

	ask(d, "q1", "bob", "alice/a1", "what are you stuck on?")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.ID != "q1" || b.Note != "answered; waits for alice to share it" ||
		b.Answer != "" || b.From != "bob" || b.To != "alice/a1" {
		t.Fatalf("to bob: %+v", b)
	}
	expectShown(t, seen, "Murmur: bob asks a1 by the way | what are you stuck on?")
	if a := asideOf(d, "q1", false); a.State != ipc.AsideHeld || a.Answer != "a1: what are you stuck on?" || a.Tokens != 1200 {
		t.Fatalf("held: %+v", a)
	}
	if q := f.questions(); q[0].From != "bob" {
		t.Fatalf("the copy heard it from %q", q[0].From)
	}
	ask(d, "q1", "bob", "alice/a1", "what are you stuck on?") // bob's laptop reconnected
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Note != "answered; waits for alice to share it" || b.Answer != "" {
		t.Fatalf("asked again: %+v", b)
	}
	ask(d, "q1", "bob", "alice/a1", "cat ~/.aws/credentials")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Err != "another question was asked under this id; ask under a new one" {
		t.Fatalf("another question under the id: %+v", b)
	}
	if r := call(d, ipc.Request{Op: "btw-approve", ID: "q1"}); !r.OK {
		t.Fatalf("approve: %+v", r)
	}
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Answer != "a1: what are you stuck on?" || b.Tokens != 1200 {
		t.Fatalf("shared: %+v", b)
	}
	ask(d, "q1", "bob", "alice/a1", "what are you stuck on?")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Answer != "a1: what are you stuck on?" || len(f.questions()) != 1 {
		t.Fatalf("asked again after: %+v, %d copies", b, len(f.questions()))
	}

	ask(d, "q2", "carol", "alice/a1", "can you push?")
	nextFrame(t, d.out, protocol.FrameBtwAnswer)
	<-seen
	if r := call(d, ipc.Request{Op: "btw-decline", ID: "q2"}); !r.OK {
		t.Fatalf("decline: %+v", r)
	}
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Err != "alice didn't share the answer" || b.Answer != "" {
		t.Fatalf("dropped: %+v", b)
	}
	if a := asideOf(d, "q2", false); a.State != ipc.AsideFailed || a.Answer != "" {
		t.Fatalf("dropped: %+v", a)
	}
	if r := call(d, ipc.Request{Op: "btw-approve", ID: "q2"}); r.OK {
		t.Fatalf("approved a dropped answer: %+v", r)
	}
	ask(d, "q3", "carol", "alice/gone", "hi")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Err != "alice/gone is no longer running" {
		t.Fatalf("an agent that isn't running: %+v", b)
	}

	// the console sees them all, oldest first, and this laptop's setting
	d.mu.Lock()
	snap := d.snapshotLocked()
	d.mu.Unlock()
	var ids []string
	for _, a := range snap.Asides {
		ids = append(ids, a.ID)
	}
	if strings.Join(ids, ",") != "q1,q1,q2,q3" || snap.BtwAnswers != ipc.BtwApprove {
		t.Fatalf("snapshot: %v, %q", ids, snap.BtwAnswers)
	}
}

// With auto, teammates' answers go on their own. With off, teammates can't ask, and the agents say so to the team: only
// their person and their agents ask them (mine); this person's own questions from their other laptop still run, and
// never wait.
func TestBtwAutoAndOff(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d, f := btwLaptop(t, &session{id: "s1", name: "a1", status: protocol.StatusBusy}, &session{harness: "codex", id: "t1", name: "codex-1"})
	seen := shown(d)
	btwOf := func() map[string]string {
		d.mu.Lock()
		defer d.mu.Unlock()
		out := map[string]string{}
		for _, a := range d.localAgentsLocked(true) {
			out[a.Name] = a.Btw
		}
		return out
	}
	if got := btwOf(); got["a1"] != protocol.BtwApprove || got["codex-1"] != "" {
		t.Fatalf("by default: %v", got)
	}
	d.hubBtw = false
	if got := btwOf(); got["a1"] != "" {
		t.Fatalf("on a hub without btw: %v", got)
	}
	d.hubBtw = true

	if r := call(d, ipc.Request{Op: "btw-answers", Text: "auto"}); !r.OK || config.Load().BtwAnswers != "auto" || btwOf()["a1"] != protocol.BtwAuto {
		t.Fatalf("auto: %+v, saved %q, published %v", r, config.Load().BtwAnswers, btwOf())
	}
	ask(d, "q1", "bob", "alice/a1", "which port?")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Answer != "a1: which port?" {
		t.Fatalf("auto: %+v", b)
	}
	ask(d, "q2", "bob", "alice/codex-1", "which port?")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Err != "codex-1 runs in Codex, which can't be asked by the way" {
		t.Fatalf("codex: %+v", b)
	}

	if r := call(d, ipc.Request{Op: "btw-answers", Text: "off"}); !r.OK || btwOf()["a1"] != protocol.BtwMine || btwOf()["codex-1"] != "" {
		t.Fatalf("off: %+v, published %v", r, btwOf())
	}
	ask(d, "q3", "bob", "alice/a1", "which port?")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Err != "alice turned questions by the way off" {
		t.Fatalf("off: %+v", b)
	}
	ask(d, "q4", "alice", "alice/a1", "which port?")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Answer != "a1: which port?" {
		t.Fatalf("alice's own question: %+v", b)
	}
	if q := f.questions(); len(q) != 2 || q[1].From != "your person" {
		t.Fatalf("copies: %+v", q)
	}
	if r := call(d, ipc.Request{Op: "btw-answers", Text: "sometimes"}); r.OK {
		t.Fatalf("an unknown setting: %+v", r)
	}
	if r := call(d, ipc.Request{Op: "btw-answers", Text: "approve"}); !r.OK || config.Load().BtwAnswers != "" {
		t.Fatalf("approve: %+v, saved %q", r, config.Load().BtwAnswers)
	}
	select {
	case n := <-seen:
		t.Fatalf("notified %q", n)
	default:
	}
}

// A teammate's question for an agent idle for over an hour waits for this person's go before the copy runs, showing
// what the copy would read; with auto too. Once they say go, the answer waits for them like any other. This person's
// own question runs at once.
func TestBtwColdQuestionWaits(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	transcript := filepath.Join(t.TempDir(), "s1.jsonl")
	_ = os.WriteFile(transcript, []byte("{}\n"), 0o600)
	three := time.Now().Add(-3*time.Hour - time.Minute)
	_ = os.Chtimes(transcript, three, three)
	a1 := &session{id: "s1", name: "a1", status: protocol.StatusIdle, since: three.UnixMilli(), transcript: transcript}
	a1.act.Tokens = 180000
	d, f := btwLaptop(t, a1)
	seen := shown(d)

	ask(d, "q1", "bob", "alice/a1", "where did you leave it?")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Note != "a1 has been idle for 3h; the question waits for alice's go" {
		t.Fatalf("cold: %+v", b)
	}
	expectShown(t, seen, "Murmur: bob asks a1 by the way | where did you leave it?")
	if a := asideOf(d, "q1", false); a.State != ipc.AsideCold || a.Cold != 180000 || len(f.questions()) != 0 {
		t.Fatalf("cold: %+v, %d copies", a, len(f.questions()))
	}
	ask(d, "q1", "bob", "alice/a1", "where did you leave it?") // bob's laptop asks again, every two minutes
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Note != "a1 has been idle for 3h; the question waits for alice's go" ||
		len(f.questions()) != 0 || asideOf(d, "q1", false).State != ipc.AsideCold {
		t.Fatalf("cold, asked again: %+v, %d copies", b, len(f.questions()))
	}
	if r := call(d, ipc.Request{Op: "btw-approve", ID: "q1"}); !r.OK {
		t.Fatalf("go: %+v", r)
	}
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Note != "being answered" {
		t.Fatalf("on go: %+v", b)
	}
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Note != "answered; waits for alice to share it" {
		t.Fatalf("answered: %+v", b)
	}
	if q := f.questions(); len(q) != 1 || q[0].State != "idle for 3h" {
		t.Fatalf("the copy: %+v", q)
	}

	_ = d.setBtwAnswers(ipc.BtwAuto)
	ask(d, "q2", "carol", "alice/a1", "anything left?")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; !strings.Contains(b.Note, "the question waits for alice") {
		t.Fatalf("cold with auto: %+v", b)
	}
	if r := call(d, ipc.Request{Op: "btw-decline", ID: "q2"}); !r.OK {
		t.Fatalf("decline: %+v", r)
	}
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Err != "alice declined the question" {
		t.Fatalf("declined: %+v", b)
	}
	ask(d, "q3", "alice", "alice/a1", "anything left?")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Answer != "a1: anything left?" {
		t.Fatalf("alice's own: %+v", b)
	}
}

// A laptop answers at most btwPerHour teammates' questions an hour; its person's own go on.
func TestBtwCap(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d, _ := btwLaptop(t, &session{id: "s1", name: "a1", status: protocol.StatusBusy})
	_ = d.setBtwAnswers(ipc.BtwAuto)
	for i := range btwPerHour {
		ask(d, fmt.Sprint("q", i), "bob", "alice/a1", "hi")
		if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Answer == "" {
			t.Fatalf("question %d: %+v", i, b)
		}
	}
	ask(d, "over", "carol", "alice/a1", "hi")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Err != "alice's agents answer at most 20 questions by the way an hour; ask again later" {
		t.Fatalf("over the cap: %+v", b)
	}
	ask(d, "mine", "alice", "alice/a1", "hi")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Answer == "" {
		t.Fatalf("alice's own: %+v", b)
	}
	d.mu.Lock()
	for _, a := range d.asides {
		a.At = time.Now().Add(-time.Hour - time.Minute).UnixMilli()
	}
	d.mu.Unlock()
	ask(d, "later", "carol", "alice/a1", "hi")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Answer == "" {
		t.Fatalf("an hour later: %+v", b)
	}
}

// Two copies at most run on a laptop, one per session; a session's second question waits for its first, and one
// more than maxLine waiting is refused.
func TestBtwCopiesTakeTurns(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d, f := btwLaptop(t, &session{id: "s1", name: "a1"}, &session{id: "s2", name: "a2"}, &session{id: "s3", name: "a3"})
	f.gate = make(chan struct{})
	asks := map[string]string{"q1": "a1", "q2": "a2", "q3": "a3", "q4": "a1", "q5": "a1", "q6": "a1"}
	for _, id := range slices.Sorted(maps.Keys(asks)) {
		if r := call(d, ipc.Request{Op: "btw", ID: id, To: asks[id], Text: "status?"}); !r.OK {
			t.Fatalf("%s: %+v", id, r)
		}
	}
	refused := func() (n int) {
		for _, id := range []string{"q1", "q4", "q5", "q6"} {
			if strings.Contains(asideOf(d, id, true).Err, "has 2 questions by the way waiting already") {
				n++
			}
		}
		return n
	}
	eventually(t, "a1's line refuses one", func() bool { return refused() == 1 })
	eventually(t, "two copies", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.now == 2 })
	for range 5 {
		f.gate <- struct{}{}
	}
	eventually(t, "every answer", func() bool {
		n := 0
		for id := range asks {
			if asideOf(d, id, true).State == ipc.AsideAnswered {
				n++
			}
		}
		return n == 5
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.most != 2 || f.mostOneRan != 1 {
		t.Fatalf("at most %d copies at once, %d of one session", f.most, f.mostOneRan)
	}
}

// A copy registers as a session while it runs, under the id murmur chose: it never becomes one of this laptop's
// agents, and its hooks tell it nothing. The harness gets the session's process start time as murmur read it, not as
// Claude Code records it, to tell a reused pid apart.
func TestBtwCopyIsNoSession(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	ccDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", ccDir)
	_ = os.MkdirAll(filepath.Join(ccDir, "sessions"), 0o700)
	register := func(pid int, id string) string {
		path := filepath.Join(ccDir, "sessions", fmt.Sprintf("%d.json", pid))
		entry := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"cwd":%q,"name":"a1","status":"busy","procStart":"1"}`, pid, id, ccDir)
		_ = os.WriteFile(path, []byte(entry), 0o600)
		return path
	}
	register(os.Getpid(), "s1")
	copyProc := exec.Command("sleep", "30")
	if err := copyProc.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = copyProc.Process.Kill() }()
	d, f := btwLaptop(t)
	d.team.Agreements = "Keep pull requests small."
	d.scan(claude.Name, claude.Claude{})
	var during []string
	f.during = func(q harness.Question) {
		register(copyProc.Process.Pid, q.Fork)
		d.scan(claude.Name, claude.Claude{})
		d.mu.Lock()
		for _, s := range d.sessions {
			during = append(during, s.id)
		}
		d.mu.Unlock()
		if text, _ := d.onHook(harness.Event{Kind: harness.Start, Harness: claude.Name, SessionID: q.Fork, PID: copyProc.Process.Pid}); text != "" {
			during = append(during, "told the copy "+text)
		}
	}
	if text, _ := d.onHook(harness.Event{Kind: harness.Start, Harness: claude.Name, SessionID: "s1", PID: os.Getpid()}); text == "" {
		t.Fatal("a real session's start hook told it nothing")
	}
	call(d, ipc.Request{Op: "btw", ID: "q1", To: "a1", Text: "status?"})
	if r := call(d, ipc.Request{Op: "btw-wait", ID: "q1"}); !r.OK || r.Text == "" {
		t.Fatalf("btw-wait: %+v", r)
	}
	if strings.Join(during, ",") != "s1" {
		t.Fatalf("while the copy ran: %v", during)
	}
	if s := f.sessions[0]; s.PID != os.Getpid() || s.Started == "" || s.Started != harness.StartTime(os.Getpid()) {
		t.Fatalf("the copy's session %+v", s)
	}
}

// A session whose process ended without Claude Code noticing leaves its PID to another process: when the session's
// record says its process started at another time than the one with the PID, a copy takes nothing from that process.
func TestBtwProcessStartedElsewhen(t *testing.T) {
	ccDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", ccDir)
	_ = os.MkdirAll(filepath.Join(ccDir, "sessions"), 0o700)
	at := harness.StartedAt(os.Getpid()).UTC()
	for procStart, want := range map[string]string{
		at.Format("Mon Jan 2 15:04:05 2006"):                      harness.StartTime(os.Getpid()),
		at.Add(-48 * time.Hour).Format("Mon Jan 2 15:04:05 2006"): "",
	} {
		entry := fmt.Sprintf(`{"pid":%d,"sessionId":"s1","cwd":%q,"name":"a1","status":"busy","procStart":%q}`, os.Getpid(), ccDir, procStart)
		_ = os.WriteFile(filepath.Join(ccDir, "sessions", fmt.Sprintf("%d.json", os.Getpid())), []byte(entry), 0o600)
		t.Setenv("MURMUR_HOME", t.TempDir())
		d, f := btwLaptop(t)
		d.scan(claude.Name, claude.Claude{})
		call(d, ipc.Request{Op: "btw", ID: "q1", To: "a1", Text: "status?"})
		if r := call(d, ipc.Request{Op: "btw-wait", ID: "q1"}); !r.OK {
			t.Fatalf("btw-wait: %+v", r)
		}
		if s := f.sessions[0]; s.PID != os.Getpid() || s.Started != want {
			t.Errorf("procStart %q: the copy's session started %q, want %q", procStart, s.Started, want)
		}
		d.hist.Close()
	}
}

// Questions and answers outlast a restart: a held answer still waits for its person and a question asked again is
// answered from the history, without a copy. A copy that ran when the daemon stopped stopped with it.
func TestBtwAcrossARestart(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d, f := btwLaptop(t, &session{id: "s1", name: "a1", status: protocol.StatusBusy})
	ask(d, "q1", "bob", "alice/a1", "what are you stuck on?")
	nextFrame(t, d.out, protocol.FrameBtwAnswer)
	_ = d.setBtwAnswers(ipc.BtwAuto)
	ask(d, "q2", "bob", "alice/a1", "and the tests?")
	nextFrame(t, d.out, protocol.FrameBtwAnswer)
	call(d, ipc.Request{Op: "btw", ID: "q3", To: "bob/b1", Text: "lunch?"})
	f.gate = make(chan struct{})
	defer close(f.gate)
	call(d, ipc.Request{Op: "btw", ID: "q4", To: "a1", Text: "still there?"})
	eventually(t, "the copy runs", func() bool { return len(f.questions()) == 3 })
	d.hist.Close()

	d, f = btwLaptop(t, &session{id: "s1", name: "a1", status: protocol.StatusBusy})
	if a := asideOf(d, "q1", false); a.State != ipc.AsideHeld || a.Answer != "a1: what are you stuck on?" {
		t.Fatalf("held, after a restart: %+v", a)
	}
	if a := asideOf(d, "q4", true); a.State != ipc.AsideFailed || !strings.Contains(a.Err, "restarted") {
		t.Fatalf("running, after a restart: %+v", a)
	}
	ask(d, "q2", "bob", "alice/a1", "and the tests?")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Answer != "a1: and the tests?" || len(f.questions()) != 0 {
		t.Fatalf("answered before the restart: %+v", b)
	}
	if r := call(d, ipc.Request{Op: "btw-approve", ID: "q1"}); !r.OK {
		t.Fatalf("approve after a restart: %+v", r)
	}
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Answer != "a1: what are you stuck on?" {
		t.Fatalf("shared after a restart: %+v", b)
	}
	d.btwOut = nil // as a new connection
	d.onFrame(protocol.Frame{T: protocol.FrameWelcome, Me: &protocol.Person{Handle: "alice"}, Can: []string{protocol.CanBtw}})
	if b := nextFrame(t, d.out, protocol.FrameBtw).Btw; b.ID != "q3" {
		t.Fatalf("asked again after a restart: %+v", b)
	}
}

// A copy that can't sign in: its asker hears only that, this person sees why and what to do.
func TestBtwSignInFailure(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d, f := btwLaptop(t, &session{id: "s1", name: "a1", status: protocol.StatusBusy})
	f.err = fmt.Errorf("%w: run `aws sso login` (or refresh your AWS credentials)", harness.ErrSignIn)
	ask(d, "q1", "bob", "alice/a1", "hi")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Err != "the copy couldn't sign in on alice's laptop; alice sees why" {
		t.Fatalf("to bob: %+v", b)
	}
	if a := asideOf(d, "q1", false); !strings.Contains(a.Err, "aws sso login") {
		t.Fatalf("for alice: %+v", a)
	}
	call(d, ipc.Request{Op: "btw", ID: "q2", To: "a1", Text: "hi"})
	if r := call(d, ipc.Request{Op: "btw-wait", ID: "q2"}); !strings.Contains(r.Err, "aws sso login") {
		t.Fatalf("alice's own: %+v", r)
	}
}

// The hello says this laptop carries questions by the way.
func TestHelloSaysBtw(t *testing.T) {
	can := make(chan []string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		var f protocol.Frame
		_ = wsjson.Read(r.Context(), c, &f)
		can <- f.Can
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()
	d := newDaemon("alice")
	_ = d.connect(config.Config{Hub: srv.URL})
	if got := <-can; !slices.Contains(got, protocol.CanBtw) {
		t.Fatalf("hello can %v", got)
	}
}

// checkedAsker is a fake Claude Code that tells whether its copies can run on this laptop.
type checkedAsker struct {
	*fakeAsker
	cant error
}

func (c *checkedAsker) CanAsk() error { return c.cant }

// An agent whose copies can't run on this laptop, its Claude Code under managed settings that turn hooks off, isn't
// offered to the team: teammates' consoles and agents don't ask it.
func TestBtwNotOfferedWhereCopiesCantRun(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d, f := btwLaptop(t, &session{id: "s1", name: "a1", status: protocol.StatusBusy})
	c := &checkedAsker{fakeAsker: f, cant: errors.New("your organisation's Claude Code settings turn hooks off")}
	d.askers = func(name string) harness.Asker {
		if name == claude.Name {
			return c
		}
		return nil
	}
	btw := func() string {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.localAgentsLocked(true)[0].Btw
	}
	if got := btw(); got != "" {
		t.Fatalf("offered with hooks off: %q", got)
	}
	c.cant = nil
	if got := btw(); got != protocol.BtwApprove {
		t.Fatalf("with hooks on: %q", got)
	}
}

// What this laptop's agents last said reaches its console, by agent name, and nothing else. Every agent here that can
// be asked by the way is in it, with nothing said yet too: the console asks it here. The others come with why not.
func TestSnapshotSaid(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	a1 := &session{id: "s1", name: "a1"}
	a1.act.Said = "Fixed the flaky test; \x1b[31mpushing now."
	d, _ := btwLaptop(t, a1, &session{id: "s2", name: "a2"}, &session{harness: "codex", id: "t1", name: "codex-1"},
		&session{id: "s3"})
	d.mu.Lock()
	snap := d.snapshotLocked()
	d.mu.Unlock()
	if !maps.Equal(snap.Said, map[string]string{"a1": "Fixed the flaky test; pushing now.", "a2": ""}) ||
		!maps.Equal(snap.CantAsk, map[string]string{"codex-1": "codex-1 runs in Codex, which can't be asked by the way"}) {
		t.Fatalf("said %q, can't ask %q", snap.Said, snap.CantAsk)
	}
}

// An answer this person shares while the laptop is offline goes once the hub welcomes it again.
func TestBtwAnswerSharedOffline(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d, _ := btwLaptop(t, &session{id: "s1", name: "a1", status: protocol.StatusBusy})
	ask(d, "q1", "bob", "alice/a1", "what are you stuck on?")
	nextFrame(t, d.out, protocol.FrameBtwAnswer)
	d.mu.Lock()
	d.out = nil
	d.mu.Unlock()
	if r := call(d, ipc.Request{Op: "btw-approve", ID: "q1"}); !r.OK {
		t.Fatalf("approve offline: %+v", r)
	}
	d.mu.Lock()
	d.out = make(chan protocol.Frame, 64)
	d.mu.Unlock()
	d.onFrame(protocol.Frame{T: protocol.FrameWelcome, Me: &protocol.Person{Handle: "alice"}, Can: []string{protocol.CanBtw}})
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.ID != "q1" || b.Answer != "a1: what are you stuck on?" {
		t.Fatalf("once back: %+v", b)
	}
	d.onFrame(protocol.Frame{T: protocol.FrameWelcome, Me: &protocol.Person{Handle: "alice"}, Can: []string{protocol.CanBtw}})
	noFrame(t, d.out, 50*time.Millisecond)
}

// ---------------------------------------------------------------- agents asking

// agentAsks asks something by the way as the agent of session s, as its btw tool does: the request names its session.
func agentAsks(d *Daemon, s *session, id, to, question string) ipc.Reply {
	return call(d, ipc.Request{Op: "btw", ID: id, To: to, Text: question, Harness: claude.Name, PID: s.pid, Session: s.id})
}

// agentWaits waits for the answer to id as the agent of session s, as its btw tool does.
func agentWaits(d *Daemon, s *session, id string) ipc.Reply {
	return call(d, ipc.Request{Op: "btw-wait", ID: id, Harness: claude.Name, PID: s.pid, Session: s.id})
}

// waitsThenGoes waits for the answer to id as the agent of session s does, and stops waiting before it comes, as its
// btw tool does after 50 seconds.
func waitsThenGoes(t *testing.T, d *Daemon, s *session, id string) {
	t.Helper()
	c, them := net.Pipe()
	go d.serve(c)
	_ = json.NewEncoder(them).Encode(ipc.Request{Op: "btw-wait", ID: id, Harness: claude.Name, PID: s.pid, Session: s.id})
	waiters := func() int { d.mu.Lock(); defer d.mu.Unlock(); return d.waiters[id] }
	eventually(t, "btw-wait waits", func() bool { return waiters() == 1 })
	them.Close()
	eventually(t, "btw-wait stops waiting", func() bool { return waiters() == 0 })
}

// next is the next message the inbox gets, as its agent reads it.
func (in *inbox) next(t *testing.T) string {
	t.Helper()
	select {
	case line := <-in.got:
		var m struct{ Message struct{ Content string } }
		_ = json.Unmarshal([]byte(line), &m)
		return m.Message.Content
	case <-time.After(5 * time.Second):
		t.Fatal("the inbox got nothing")
	}
	return ""
}

// lateAnswer is a final answer an agent's btw tool didn't wait for, as the agent reads it: it follows up with btw, not
// with a message that would interrupt the agent asked, which never saw the question.
func lateAnswer(to, question, answer string) string {
	return fmt.Sprintf("[murmur] %s: Answer to your question by the way (%q), from a copy of %s's conversation; %s itself never "+
		"saw the question:\n%s\n\n(From a copy of %s's conversation, through murmur: %s itself never saw your question. For a "+
		"follow-up, use the murmur `btw` tool again; use `send` only if %s itself must act or reply.)", to, question, to, to, answer, to, to, to)
}

// lateError is why an agent's question got no answer, once its btw tool stopped waiting, as the agent reads it.
func lateError(to, question, why string) string {
	return fmt.Sprintf("[murmur] No answer to your question by the way to %s (%q), relayed by murmur: %q\n\n(From murmur, on your "+
		"question to a copy of %s's conversation: %s itself never saw it. To ask again, use the murmur `btw` tool; use `send` only "+
		"if %s itself must act or reply.)", to, question, why, to, to, to)
}

// An agent asks another agent on this laptop: a copy runs here, hearing it from its person's agent. An answer within
// its tool's wait goes back to the tool only; once the tool stopped waiting, the answer reaches the agent as a message
// from the agent asked, and an error as murmur's, quoting why, once each. Its person gets no notification.
func TestBtwAgentAsksHere(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	in := newInbox(t)
	a1, a2 := agentSession("a1", "s1", in), agentSession("a2", "s2", newInbox(t))
	a1.pid, a2.pid = 101, 102
	d, f := btwLaptop(t, a1, a2)
	seen := shown(d)

	if r := agentAsks(d, a1, "q1", "a2", "which port does the API use?"); !r.OK {
		t.Fatalf("btw: %+v", r)
	}
	if r := agentWaits(d, a1, "q1"); !r.OK || r.Text != "a2: which port does the API use?" {
		t.Fatalf("btw-wait: %+v", r)
	}
	if q := f.questions(); len(q) != 1 || q[0].From != "your person's agent a1" {
		t.Fatalf("the copy was asked %+v", q)
	}
	if a := asideOf(d, "q1", true); a.From != "alice/a1" || a.To != "alice/a2" || !a.Local || a.By != a1.key() || !a.Handed {
		t.Fatalf("aside %+v", a)
	}

	f.hold()
	agentAsks(d, a1, "q2", "alice/a2", "and the tests?")
	waitsThenGoes(t, d, a1, "q2")
	f.gate <- struct{}{}
	if got := in.next(t); got != lateAnswer("alice/a2", "and the tests?", "a2: and the tests?") {
		t.Fatalf("late answer:\n%s", got)
	}
	f.mu.Lock()
	f.err = errors.New("the copy of a2 gave no answer")
	f.mu.Unlock()
	agentAsks(d, a1, "q3", "a2", "lunch?\nat noon")
	waitsThenGoes(t, d, a1, "q3")
	f.gate <- struct{}{}
	if got := in.next(t); got != lateError("alice/a2", "lunch?…", "the copy of a2 gave no answer") {
		t.Fatalf("late error:\n%s", got)
	}
	d.btwTick(0)
	in.quiet(t, 100*time.Millisecond)

	for _, c := range []struct {
		req  ipc.Request
		want string
	}{
		{ipc.Request{ID: "q4", To: "a2", Text: "hi", Harness: claude.Name, PID: 999, Session: "nope"}, "this session is not on murmur yet"},
		{ipc.Request{ID: "q5", To: "a1", Text: "hi", Harness: claude.Name, PID: a1.pid, Session: a1.id}, "an agent can't ask itself by the way"},
		{ipc.Request{ID: "q1", To: "a2", Text: "which port does the API use?"}, "asked under this id already"}, // its person, under a1's id
	} {
		c.req.Op = "btw"
		if r := call(d, c.req); r.OK || !strings.Contains(r.Err, c.want) {
			t.Errorf("%+v: %+v, want %q", c.req, r, c.want)
		}
	}
	d.mu.Lock()
	snap := d.snapshotLocked()
	d.mu.Unlock()
	if len(snap.Asides) != 3 || !snap.Asides[1].Mine || snap.Asides[1].From != "alice/a1" || len(snap.Msgs) != 0 {
		t.Fatalf("the console sees %+v, and messages %+v", snap.Asides, snap.Msgs)
	}
	select {
	case n := <-seen:
		t.Fatalf("notified %q", n)
	default:
	}
}

// An agent asks a teammate's agent, through the hub, which hears which agent asks. A final answer reaches the agent
// once: through its tool, when it comes while the tool waits or before; as a message when it comes after the tool
// stopped waiting, or when the tool never waited; never once the agent has closed. Why there is no answer, which the
// hub or the other laptop wrote, comes quoted, never in murmur's own words. A question asked again says so, and stops
// once its agent has closed.
func TestBtwAgentAsksThroughTheHub(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	in := newInbox(t)
	a1 := agentSession("a1", "s1", in)
	a1.pid = 101
	d, _ := btwLaptop(t, a1)
	seen := shown(d)
	const question = "which branch has the fix?"
	ask := func(id string) {
		t.Helper()
		if r := agentAsks(d, a1, id, "bob/b1", question); !r.OK {
			t.Fatalf("btw %s: %+v", id, r)
		}
		if b := nextFrame(t, d.out, protocol.FrameBtw).Btw; b.ID != id || b.From != "alice/a1" || b.Again {
			t.Fatalf("to the hub: %+v", b)
		}
	}
	answer := func(b protocol.Btw) {
		b.From, b.To = "alice/a1", "bob/b1"
		d.onFrame(protocol.Frame{T: protocol.FrameBtwAnswer, Btw: &b})
	}

	ask("q1")
	waited := make(chan ipc.Reply, 1)
	go func() { waited <- agentWaits(d, a1, "q1") }()
	eventually(t, "btw-wait waits", func() bool { d.mu.Lock(); defer d.mu.Unlock(); return d.waiters["q1"] == 1 })
	answer(protocol.Btw{ID: "q1", Note: "answered; waits for bob to share it"})
	answer(protocol.Btw{ID: "q1", Answer: "feat/x"})
	if r := <-waited; !r.OK || r.Text != "feat/x" {
		t.Fatalf("within the wait: %+v", r)
	}
	ask("q2")
	answer(protocol.Btw{ID: "q2", Answer: "feat/y"}) // before its tool waits
	if r := agentWaits(d, a1, "q2"); !r.OK || r.Text != "feat/y" {
		t.Fatalf("before the wait: %+v", r)
	}
	d.btwTick(0)
	in.quiet(t, 100*time.Millisecond)

	ask("q3")
	waitsThenGoes(t, d, a1, "q3")
	answer(protocol.Btw{ID: "q3", Answer: "feat/z", Tokens: 900})
	answer(protocol.Btw{ID: "q3", Answer: "feat/z", Tokens: 900}) // again: from bob's history, say
	if got := in.next(t); got != lateAnswer("bob/b1", question, "feat/z") {
		t.Fatalf("after the wait:\n%s", got)
	}
	ask("q4")
	answer(protocol.Btw{ID: "q4", Err: "bob didn't share the answer"})
	d.btwTick(askAgain) // its tool may still come
	in.quiet(t, 50*time.Millisecond)
	d.btwTick(0) // it never did
	if got := in.next(t); got != lateError("bob/b1", question, "bob didn't share the answer") {
		t.Fatalf("no wait:\n%s", got)
	}
	d.btwTick(0)
	in.quiet(t, 100*time.Millisecond)
	if a := asideOf(d, "q3", true); a.State != ipc.AsideAnswered || !a.Handed || a.Tokens != 900 {
		t.Fatalf("q3: %+v", a)
	}

	ask("q5")
	d.btwTick(0)
	if b := nextFrame(t, d.out, protocol.FrameBtw).Btw; b.ID != "q5" || !b.Again || b.From != "alice/a1" {
		t.Fatalf("asked again: %+v", b)
	}
	ask("q6")
	waitsThenGoes(t, d, a1, "q6")
	d.mu.Lock()
	d.removeLocked(a1.key())
	d.mu.Unlock()
	answer(protocol.Btw{ID: "q6", Answer: "feat/w"})
	if a := asideOf(d, "q6", true); a.State != ipc.AsideAnswered || !a.Handed {
		t.Fatalf("for a closed agent: %+v", a)
	}
	d.btwTick(0)
	if a := asideOf(d, "q5", true); a.State != ipc.AsideFailed || a.Err != "a1 closed before the answer came" {
		t.Fatalf("asked again for a closed agent: %+v", a)
	}
	noFrame(t, d.out, 50*time.Millisecond)
	in.quiet(t, 100*time.Millisecond)
	select {
	case n := <-seen:
		t.Fatalf("notified %q", n)
	default:
	}
}

// A question from a teammate's agent waits for this person as a teammate's does, and the copy and the notification
// name the agent. One from this person's agent on another laptop never waits, cold or with teammates' questions off.
func TestBtwAskedByAnAgent(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	transcript := filepath.Join(t.TempDir(), "s2.jsonl")
	_ = os.WriteFile(transcript, []byte("{}\n"), 0o600)
	three := time.Now().Add(-3*time.Hour - time.Minute)
	_ = os.Chtimes(transcript, three, three)
	a2 := &session{id: "s2", name: "a2", status: protocol.StatusIdle, since: three.UnixMilli(), transcript: transcript}
	d, f := btwLaptop(t, &session{id: "s1", name: "a1", status: protocol.StatusBusy}, a2)
	seen := shown(d)

	ask(d, "q1", "bob/b1", "alice/a1", "what are you stuck on?")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Note != "answered; waits for alice to share it" || b.From != "bob/b1" {
		t.Fatalf("to bob/b1: %+v", b)
	}
	expectShown(t, seen, "Murmur: bob's agent b1 asks a1 by the way | what are you stuck on?")
	if q := f.questions(); q[0].From != "bob's agent b1" {
		t.Fatalf("the copy heard it from %q", q[0].From)
	}
	_ = d.setBtwAnswers(ipc.BtwOff)
	ask(d, "q2", "alice/a9", "alice/a2", "where did you leave it?")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Answer != "a2: where did you leave it?" {
		t.Fatalf("alice's own agent: %+v", b)
	}
	if q := f.questions(); len(q) != 2 || q[1].From != "your person's agent a9" {
		t.Fatalf("the copies: %+v", q)
	}
}

// A late answer whose message doesn't reach its agent (an inbox that refuses it: a Claude Code too old to take
// messages, a socket gone) goes again on the next btwTicks, without writing the question down again, and no longer
// once it failed lateTries times. One that goes through on a retry reaches the agent once.
func TestBtwLateAnswerGivesUp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MURMUR_HOME", home)
	in := newInbox(t)
	a1, a2 := agentSession("a1", "s1", in), agentSession("a2", "s2", newInbox(t))
	a1.pid, a2.pid = 101, 102
	a1.sock = filepath.Join(home, "no-such-socket")
	d, f := btwLaptop(t, a1, a2)
	f.hold()
	agentAsks(d, a1, "q1", "a2", strings.Repeat("why? ", 300))
	waitsThenGoes(t, d, a1, "q1")
	f.gate <- struct{}{}
	eventually(t, "a first try", func() bool { return asideOf(d, "q1", true).tries == 1 })
	hist := filepath.Join(home, "history.jsonl")
	before, _ := os.Stat(hist)
	last := asideOf(d, "q1", true).Last
	for range lateTries + 10 {
		d.btwTick(0)
	}
	after, _ := os.Stat(hist)
	if a := asideOf(d, "q1", true); after.Size() != before.Size() || a.Last != last || !a.Handed || a.retry || a.tries != lateTries {
		t.Fatalf("history grew %d bytes; the question: handed %v, retry %v, %d tries, last changed %d ms later", after.Size()-before.Size(),
			a.Handed, a.retry, a.tries, a.Last-last)
	}

	agentAsks(d, a1, "q2", "a2", "and now?")
	waitsThenGoes(t, d, a1, "q2")
	f.gate <- struct{}{}
	eventually(t, "a first try", func() bool { return asideOf(d, "q2", true).tries == 1 })
	d.mu.Lock()
	a1.sock = in.sock // the agent's inbox is back
	d.mu.Unlock()
	d.btwTick(0)
	if got := in.next(t); got != lateAnswer("alice/a2", "and now?", "a2: and now?") {
		t.Fatalf("on a retry:\n%s", got)
	}
	d.btwTick(0)
	in.quiet(t, 100*time.Millisecond)
}

// A late answer waits in the queue of an agent whose coding tool takes messages only with its hooks' answers (Cursor,
// Codex). When that agent closes before it reads the answer, nobody hears of it: not the agent asked, which the answer
// names as its sender and which never saw the question, whether the session ends, its hook gives the answer back once
// it ended, or murmur restarts after its process ended.
func TestBtwLateAnswerUnreadTellsNobody(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MURMUR_HOME", home)
	in := newInbox(t)
	a2 := agentSession("a2", "s2", in)
	a2.pid = 102
	d, f := btwLaptop(t, a2)
	d.running = true
	f.hold()
	hook := func(kind, id string, pid int) (string, string) {
		return d.onHook(harness.Event{Kind: kind, Harness: "cursor", SessionID: id, PID: pid, Cwd: home, Carries: true})
	}
	// session id, of process pid, asks a2 under q; its tool doesn't wait, and the answer waits for its next hook
	late := func(id string, pid int, q string) {
		t.Helper()
		hook(harness.Start, id, pid)
		r := call(d, ipc.Request{Op: "btw", ID: q, To: "a2", Text: "what is left of the migration?", Harness: "cursor", PID: pid, Session: id})
		if !r.OK {
			t.Fatalf("btw %s: %+v", q, r)
		}
		f.gate <- struct{}{}
		eventually(t, "the answer to "+q, func() bool { a := asideOf(d, q, true); return a.final() })
		d.btwTick(0)
		d.mu.Lock()
		defer d.mu.Unlock()
		if w := d.queued[harness.Key("cursor", id)]; len(w) != 1 || !strings.Contains(w[0].Msg.Text, "what is left of the migration?") {
			t.Fatalf("what waits for %s: %+v", id, w)
		}
	}
	nothingSent := func(how string) {
		t.Helper()
		end := time.After(300 * time.Millisecond)
		for waiting := true; waiting; {
			select {
			case fr := <-d.out:
				if fr.T == protocol.FrameSend {
					t.Fatalf("%s: %s heard %q", how, fr.Msg.To, fr.Msg.Text)
				}
			case <-end:
				waiting = false
			}
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if len(d.outbox) > 0 || len(d.notices) > 0 {
			t.Fatalf("%s: the outbox %+v, notices %+v", how, d.outbox, d.notices)
		}
	}

	late("c1", os.Getpid(), "q1")
	hook(harness.End, "c1", os.Getpid())
	nothingSent("the session ended")

	late("c2", os.Getpid(), "q2")
	text, token := hook(harness.AfterTool, "c2", os.Getpid())
	if !strings.Contains(text, "what is left of the migration?") {
		t.Fatalf("the hook carries %q", text)
	}
	hook(harness.End, "c2", os.Getpid())
	d.settle(token, false) // the hook never printed it
	nothingSent("the hook gave it back")

	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	late("c3", sleeper.Process.Pid, "q3")
	_ = sleeper.Process.Kill()
	_ = sleeper.Wait()
	d.hist.Close()
	d = newDaemon("alice")
	d.loadHistory()
	t.Cleanup(func() { d.hist.Close() })
	d.mu.Lock()
	notices, waiting := d.notices, len(d.queued)
	d.mu.Unlock()
	if len(notices) > 0 || waiting > 0 {
		t.Fatalf("after a restart: notices %+v, %d queues", notices, waiting)
	}
	in.quiet(t, 50*time.Millisecond)
}

// A copy that fails: its asker, a teammate or their agent, hears only that it failed, and this person sees why. What
// the copy said (its paths, its output, a provider's error) never leaves the laptop; what murmur says of a question it
// doesn't take still goes, for the asker to act on.
func TestBtwCopyFailureStaysHere(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d, f := btwLaptop(t, &session{id: "s1", name: "a1", status: protocol.StatusBusy})
	const why = "claude exited with status 1: Error: ENOENT /Users/alice/.local/share/claude/versions/2.1.292, " +
		"ANTHROPIC_BASE_URL https://gateway.internal"
	f.err = errors.New(why)
	ask(d, "q1", "bob/b1", "alice/a1", "hi")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Err != "the copy failed on alice's laptop; alice sees why" {
		t.Fatalf("to bob/b1: %+v", b)
	}
	if a := asideOf(d, "q1", false); a.Err != why {
		t.Fatalf("for alice: %+v", a)
	}
	ask(d, "q1", "bob/b1", "alice/a1", "hi") // asked again: from the history
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Err != "the copy failed on alice's laptop; alice sees why" {
		t.Fatalf("asked again: %+v", b)
	}
	ask(d, "q2", "bob", "alice/a7", "hi")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Err != "alice/a7 is no longer running" {
		t.Fatalf("for an agent gone: %+v", b)
	}
	call(d, ipc.Request{Op: "btw", ID: "q3", To: "a1", Text: "hi"})
	if r := call(d, ipc.Request{Op: "btw-wait", ID: "q3"}); r.Err != why {
		t.Fatalf("alice's own: %+v", r)
	}
}

// A hub that lost track of a question asks it again of every laptop of this person: one that has seen it answers from
// its history, even once the agent is gone; one that hasn't, and doesn't run the agent, says nothing, for another
// laptop of this person's to answer; one that runs it answers it.
func TestBtwAskedAgainOfEveryLaptop(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	a1 := &session{id: "s1", name: "a1", status: protocol.StatusBusy}
	d, f := btwLaptop(t, a1)
	_ = d.setBtwAnswers(ipc.BtwAuto)
	again := func(id string) {
		d.onFrame(protocol.Frame{T: protocol.FrameBtw, Btw: &protocol.Btw{ID: id, From: "bob", To: "alice/a1", Question: "status?", At: 1,
			Again: true}})
	}
	again("q1")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Answer != "a1: status?" {
		t.Fatalf("for an agent here: %+v", b)
	}
	d.mu.Lock()
	d.removeLocked(a1.key())
	d.mu.Unlock()
	again("q1")
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Answer != "a1: status?" || len(f.questions()) != 1 {
		t.Fatalf("from the history: %+v", b)
	}
	again("q2")
	noFrame(t, d.out, 50*time.Millisecond)
	if a := asideOf(d, "q2", false); a.ID != "" {
		t.Fatalf("kept: %+v", a)
	}
}

// The daemon stays while a question asked from this laptop waits for its answer, as it does for a session or a
// console: it alone would hear the answer, and tell whoever asked. Answered, or a day old, the question no longer
// keeps it.
func TestBtwKeepsTheDaemon(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d, _ := btwLaptop(t)
	idle := func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.lastUse = time.Now().Add(-idleExit - time.Minute)
		return d.idleLocked()
	}
	if !idle() {
		t.Fatal("kept with nothing to do")
	}
	call(d, ipc.Request{Op: "btw", ID: "q1", To: "bob/b1", Text: "which branch?"})
	if idle() {
		t.Fatal("idle while a question waits for its answer")
	}
	d.mu.Lock()
	d.asides["q1"].At = time.Now().Add(-asideTTL - time.Minute).UnixMilli()
	d.mu.Unlock()
	if !idle() {
		t.Fatal("kept by a question asked over a day ago")
	}
	d.mu.Lock()
	d.asides["q1"].At = time.Now().UnixMilli()
	d.mu.Unlock()
	d.onFrame(protocol.Frame{T: protocol.FrameBtwAnswer, Btw: &protocol.Btw{ID: "q1", To: "bob/b1", Answer: "feat/x"}})
	if !idle() {
		t.Fatal("kept by a question answered")
	}
}

// A daemon that stops (replaced by a newer murmur, uninstalled, stopped by a signal) stops the copies running first,
// with what they started: they would answer nobody, at this person's cost. Their questions stay as they were, for the
// next daemon to say the copy stopped with murmur, and no copy starts after.
func TestBtwCopiesStopWithTheDaemon(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d, f := btwLaptop(t, &session{id: "s1", name: "a1", status: protocol.StatusBusy})
	f.hold()
	call(d, ipc.Request{Op: "btw", ID: "q1", To: "a1", Text: "still there?"})
	forks := func() int { d.mu.Lock(); defer d.mu.Unlock(); return len(d.forks) }
	eventually(t, "the copy runs", func() bool { return forks() == 1 })
	start := time.Now()
	d.haltCopies(5 * time.Second)
	if n := forks(); n != 0 || time.Since(start) > 2*time.Second {
		t.Fatalf("%d copies run %s after the daemon stopped them", n, time.Since(start))
	}
	ask(d, "q2", "bob", "alice/a1", "and you?")
	time.Sleep(100 * time.Millisecond) // what the copies' questions would become
	if q := f.questions(); len(q) != 1 {
		t.Fatalf("copies after the stop: %+v", q)
	}
	for id, mine := range map[string]bool{"q1": true, "q2": false} {
		if a := asideOf(d, id, mine); a.State != ipc.AsideRunning || a.Err != "" {
			t.Fatalf("%s: %+v", id, a)
		}
	}
	d.hist.Close()
	d, _ = btwLaptop(t, &session{id: "s1", name: "a1", status: protocol.StatusBusy})
	if a := asideOf(d, "q1", true); a.Err != "murmur restarted before the copy answered; ask again" {
		t.Fatalf("after a restart: %+v", a)
	}
}

// A daemon that starts removes what the copies of one that stopped left behind: their settings hold sign-in.
func TestBtwSweepsCopiesAtStart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MURMUR_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	_ = os.WriteFile(filepath.Join(home, "btw-7e1d2c3b-4a59-4687-b7c6-d5e4f3a2b1c0.json"), []byte(`{"apiKeyHelper":"~/bin/key.sh"}`), 0o600)
	sweepCopies()
	if left, _ := filepath.Glob(filepath.Join(home, "btw-*")); len(left) > 0 {
		t.Fatalf("left %q", left)
	}
}

// An agent's btw tool whose call is cancelled as its answer comes never got it: the answer reaches the agent as a
// message all the same.
func TestBtwAnswerToAWaiterGone(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	in := newInbox(t)
	a1 := agentSession("a1", "s1", in)
	a1.pid = 101
	d, _ := btwLaptop(t, a1)
	agentAsks(d, a1, "q1", "bob/b1", "which branch?")
	nextFrame(t, d.out, protocol.FrameBtw)
	d.onFrame(protocol.Frame{T: protocol.FrameBtwAnswer, Btw: &protocol.Btw{ID: "q1", From: "alice/a1", To: "bob/b1", Answer: "feat/x"}})
	c, them := net.Pipe()
	go d.serve(c)
	_ = json.NewEncoder(them).Encode(ipc.Request{Op: "btw-wait", ID: "q1", Harness: claude.Name, PID: a1.pid, Session: a1.id})
	them.Close() // cancelled: the tool reads nothing
	if got := in.next(t); got != lateAnswer("bob/b1", "which branch?", "feat/x") {
		t.Fatalf("for the waiter gone:\n%s", got)
	}
	if a := asideOf(d, "q1", true); !a.Handed {
		t.Fatalf("q1: %+v", a)
	}
	d.btwTick(0)
	in.quiet(t, 100*time.Millisecond)
}

// copyChecked is a fake Claude Code that copies only the sessions Claude Code does.
type copyChecked struct{ *fakeAsker }

func (copyChecked) CanCopy(s harness.Session) error { return claude.Claude{}.CanCopy(s) }

// A session of a Claude Code older than the oldest copies were checked with can't be asked by the way: the team isn't
// offered it, and its person hears why, when they ask it as in their console; a question from the hub is refused
// saying so, its person's own too. No copy runs.
func TestBtwOlderClaudeCode(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d, f := btwLaptop(t, &session{id: "s1", name: "a1", status: protocol.StatusBusy, version: "2.1.200"},
		&session{id: "s2", name: "a2", status: protocol.StatusBusy, version: claude.AskVersion})
	d.askers = func(name string) harness.Asker {
		if name == claude.Name {
			return copyChecked{f}
		}
		return nil
	}
	const why = "a1 can't be asked by the way: its Claude Code is older than 2.1.292; update Claude Code"
	d.mu.Lock()
	agents, snap := d.localAgentsLocked(true), d.snapshotLocked()
	d.mu.Unlock()
	if agents[0].Btw != "" || agents[1].Btw != protocol.BtwApprove {
		t.Fatalf("offered %+v", agents)
	}
	if _, ok := snap.Said["a1"]; ok || !maps.Equal(snap.CantAsk, map[string]string{"a1": why}) {
		t.Fatalf("the console sees said %q, can't ask %q", snap.Said, snap.CantAsk)
	}
	if r := call(d, ipc.Request{Op: "btw", ID: "q1", To: "a1", Text: "hi"}); r.OK || r.Err != why {
		t.Fatalf("its person: %+v", r)
	}
	for _, from := range []string{"bob", "alice/a9"} {
		ask(d, "q-"+from, from, "alice/a1", "hi")
		if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.Err != why {
			t.Fatalf("from %s: %+v", from, b)
		}
	}
	if q := f.questions(); len(q) != 0 {
		t.Fatalf("copies: %+v", q)
	}
}

// An answer this person shares while the hub doesn't carry questions by the way (an older one, or its admins turned
// them off) waits for one that does: that hub would drop it.
func TestBtwAnswerWaitsForAHubThatCarriesIt(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d, _ := btwLaptop(t, &session{id: "s1", name: "a1", status: protocol.StatusBusy})
	ask(d, "q1", "bob", "alice/a1", "what are you stuck on?")
	nextFrame(t, d.out, protocol.FrameBtwAnswer)
	d.onFrame(protocol.Frame{T: protocol.FrameWelcome, Me: &protocol.Person{Handle: "alice"}})
	if r := call(d, ipc.Request{Op: "btw-approve", ID: "q1"}); !r.OK {
		t.Fatalf("approve: %+v", r)
	}
	noFrame(t, d.out, 50*time.Millisecond)
	d.onFrame(protocol.Frame{T: protocol.FrameWelcome, Me: &protocol.Person{Handle: "alice"}, Can: []string{protocol.CanBtw}})
	if b := nextFrame(t, d.out, protocol.FrameBtwAnswer).Btw; b.ID != "q1" || b.Answer != "a1: what are you stuck on?" {
		t.Fatalf("once the hub carries it: %+v", b)
	}
}

// btwTick comes every askAgain: a question that went on the last tick, a moment after it, goes again on this one, not
// on the next.
func TestBtwAsksAgainEveryTick(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d, _ := btwLaptop(t)
	call(d, ipc.Request{Op: "btw", ID: "q1", To: "bob/b1", Text: "which branch?"})
	nextFrame(t, d.out, protocol.FrameBtw)
	d.mu.Lock()
	d.asides["q1"].sent = time.Now().Add(-askAgain + time.Second)
	d.mu.Unlock()
	d.btwTick(askAgain)
	if f := nextFrame(t, d.out, protocol.FrameBtw); f.Btw.ID != "q1" || !f.Btw.Again {
		t.Fatalf("asked again: %+v", f.Btw)
	}
}

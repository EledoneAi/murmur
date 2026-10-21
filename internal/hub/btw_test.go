package hub

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/bus"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/store"
)

// btwHub starts a hub on one instance where alice, bob and carol are on the team, each with a laptop, bob with a
// second one, and dave was removed. f changes the team's settings; logs, when set, gets the hub's log as JSON.
func btwHub(t *testing.T, f func(*Settings), logs io.Writer) (*Hub, string) {
	t.Helper()
	return btwHubOn(t, f, logs, bus.NewLocal())
}

// btwHubOn is btwHub on bus b.
func btwHubOn(t *testing.T, f func(*Settings), logs io.Writer, b bus.Bus) (*Hub, string) {
	t.Helper()
	ctx := context.Background()
	st := store.NewMemory()
	for _, u := range []store.User{{Email: "alice@x.com", Handle: "alice"}, {Email: "bob@x.com", Handle: "bob"},
		{Email: "carol@x.com", Handle: "carol"}, {Email: "dave@x.com", Handle: "dave", Disabled: true}} {
		_ = st.PutUser(ctx, u)
		_ = st.PutDevice(ctx, store.Device{ID: "d-" + u.Handle, Email: u.Email, TokenHash: hashToken("tok-" + u.Handle), Created: time.Now()})
	}
	_ = st.PutDevice(ctx, store.Device{ID: "d-bob2", Email: "bob@x.com", TokenHash: hashToken("tok-bob2"), Created: time.Now()})
	cfg := Config{Secret: []byte("0123456789abcdef"), Seed: seed(f), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if logs != nil {
		cfg.Log = slog.New(slog.NewJSONHandler(logs, nil))
	}
	h, err := New(ctx, cfg, st, b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	return h, srv.URL
}

// carrier connects a laptop that carries questions by the way.
func carrier(t *testing.T, url, token string, agents ...protocol.Agent) *laptop {
	t.Helper()
	return connectWith(t, url, token, protocol.Frame{T: protocol.FrameHello, Protocol: protocol.Version, Host: "test", Agents: agents,
		Can: []string{protocol.CanBtw}})
}

// runs waits until hub h sees an agent running on a laptop that is online.
func runs(t *testing.T, h *Hub, handle, agent string) {
	t.Helper()
	eventually(t, handle+"/"+agent+" never ran", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		x := h.hostLocked(handle, agent)
		return x != nil && x.online
	})
}

func (l *laptop) ask(id, to, question string) {
	l.send(protocol.Frame{T: protocol.FrameBtw, Btw: &protocol.Btw{ID: id, To: to, Question: question}})
}

func (l *laptop) reply(b protocol.Btw) {
	l.send(protocol.Frame{T: protocol.FrameBtwAnswer, Btw: &b})
}

// question is the next question by the way the laptop gets under id.
func (l *laptop) question(id string) protocol.Btw {
	l.t.Helper()
	return *l.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameBtw && f.Btw.ID == id }).Btw
}

// answer is the next answer by the way the laptop gets under id.
func (l *laptop) answer(id string) protocol.Btw {
	l.t.Helper()
	return *l.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameBtwAnswer && f.Btw.ID == id }).Btw
}

// askable waits for a roster that says how agents answer questions by the way, as pairs of an address and how: ""
// when it can't be asked.
func (l *laptop) askable(pairs ...string) {
	l.t.Helper()
	l.expect(func(f protocol.Frame) bool {
		said := map[string]string{}
		for _, p := range f.People {
			for _, a := range p.Agents {
				said[p.Handle+"/"+a.Name] = a.Btw
			}
		}
		for i := 0; i < len(pairs); i += 2 {
			if btw, ok := said[pairs[i]]; !ok || btw != pairs[i+1] {
				return false
			}
		}
		return f.T == protocol.FrameRoster
	})
}

func isBtw(f protocol.Frame) bool { return f.T == protocol.FrameBtw || f.T == protocol.FrameBtwAnswer }

func asides(h *Hub) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.asides)
}

// final says whether hub h passed on the final answer to question id, and keeps the question for the asking laptop to
// ask again.
func final(h *Hub, id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.asides[id]
	return a != nil && a.final
}

// minuteOn is a minute on for the questions laptop dev asked on its connection: asked again, they go on.
func minuteOn(h *Hub, dev string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, w := range h.links[dev].went {
		w.at = w.at.Add(-askAgainEvery)
		h.links[dev].went[id] = w
	}
}

// gone waits until hub h sees laptop dev go.
func gone(t *testing.T, h *Hub, dev string) {
	t.Helper()
	eventually(t, dev+" never went", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.links[dev] != nil && !h.links[dev].gone.IsZero()
	})
}

// A question by the way goes to the laptop holding the agent, saying who asks whatever the asking laptop says, and
// that laptop's answers come back to the asker, notes first, then the final one. The hub keeps the question then, for
// the asking laptop to ask again, but passes no other answer until it does. Its logs never say what the question or
// the answer said.
func TestBtw(t *testing.T) {
	var logs lockedBuffer
	h, url := btwHub(t, nil, &logs)
	alice := carrier(t, url, "tok-alice", protocol.Agent{Name: "a1", Status: protocol.StatusBusy})
	bob := carrier(t, url, "tok-bob", protocol.Agent{Name: "b1", Status: protocol.StatusBusy, Btw: protocol.BtwApprove})
	if !slices.Equal(alice.welcome.Can, []string{protocol.CanBtw}) {
		t.Fatalf("the welcome doesn't say the hub carries questions by the way: %q", alice.welcome.Can)
	}
	alice.askable("bob/b1", protocol.BtwApprove)

	alice.send(protocol.Frame{T: protocol.FrameBtw, Btw: &protocol.Btw{ID: "q1", From: "bob", To: " Bob/b1 ",
		Question: "what are you stuck on?", Answer: "made up"}})
	if q := bob.question("q1"); q.From != "alice" || q.To != "bob/b1" || q.Question != "what are you stuck on?" || q.Answer != "" || q.At == 0 {
		t.Fatalf("question: %+v", q)
	}
	bob.reply(protocol.Btw{ID: "q1", Note: "waits for bob"})
	if a := alice.answer("q1"); a.Note != "waits for bob" || a.Final() || a.From != "alice" || a.To != "bob/b1" {
		t.Fatalf("note: %+v", a)
	}
	if asides(h) != 1 {
		t.Fatal("a note ended the question")
	}
	bob.reply(protocol.Btw{ID: "q1", From: "mallory", To: "carol/c1", Answer: "the flaky migration test", Tokens: 51200})
	if a := alice.answer("q1"); a.Answer != "the flaky migration test" || a.Tokens != 51200 || a.From != "alice" || a.To != "bob/b1" {
		t.Fatalf("answer: %+v", a)
	}
	if asides(h) != 1 || !final(h, "q1") {
		t.Fatal("the hub doesn't keep an answered question")
	}
	bob.reply(protocol.Btw{ID: "q1", Answer: "once more"}) // nobody waits for it now
	alice.quiet(isBtw)

	for _, text := range []string{"stuck on", "migration"} {
		if strings.Contains(logs.String(), text) {
			t.Fatalf("%q reached the logs", text)
		}
	}
	for _, event := range []string{"question by the way sent", "question by the way answered"} {
		if !strings.Contains(logs.String(), event) {
			t.Errorf("no %q in the logs", event)
		}
	}
}

// Questions the hub can't carry are refused at once, in a final answer that says why. A laptop that didn't say it
// carries questions by the way never gets one, nor a refusal. A question for an agent whose laptop is reconnecting
// isn't refused: a note says so, and it goes once the laptop is back and the asking laptop asks again.
func TestBtwRefused(t *testing.T) {
	h, url := btwHub(t, nil, nil)
	alice := carrier(t, url, "tok-alice", protocol.Agent{Name: "a1"})
	bob := carrier(t, url, "tok-bob", protocol.Agent{Name: "b1", Btw: protocol.BtwAuto}, protocol.Agent{Name: "b2"})
	bob2 := carrier(t, url, "tok-bob2", protocol.Agent{Name: "b3", Btw: protocol.BtwApprove})
	carol := connect(t, url, "tok-carol", protocol.Agent{Name: "c1", Btw: protocol.BtwApprove}) // an older murmur
	runs(t, h, "bob", "b2")
	runs(t, h, "bob", "b3")
	runs(t, h, "carol", "c1")
	alice.askable("bob/b1", protocol.BtwAuto, "carol/c1", "") // whatever carol's laptop says
	for _, c := range []struct{ id, to, question, want string }{
		{"a/b", "bob/b1", "hi", "1 to 64 letters, digits and dashes"},
		{strings.Repeat("f", 65), "bob/b1", "hi", "1 to 64 letters, digits and dashes"},
		{"q1", "bob/b1", " \n ", "the question is empty"},
		{"q2", "bob/b1", strings.Repeat("é", protocol.MaxBtwQuestion+1), "the question is 2001 characters; the limit is 2000"},
		{"q3", "bob", "hi", "like bob/main-1"},
		{"q4", " Repo:org/app", "hi", "like bob/main-1"},
		{"q5", "erin/e1", "hi", `nobody called "erin"`},
		{"q6", "dave/d1", "hi", `nobody called "dave"`},
		{"q7", "bob/b9", "hi", "bob/b9 is not running"},
		{"q8", "carol/c1", "hi", "carol's laptop runs an older murmur, which can't answer questions by the way"},
		{"q9", "bob/b2", "hi", "bob/b2 doesn't take questions by the way"},
	} {
		alice.ask(c.id, c.to, c.question)
		a := alice.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameBtwAnswer }).Btw
		if !strings.Contains(a.Err, c.want) || a.Answer != "" || a.From != "alice" {
			t.Errorf("%.10s to %s: %+v", c.id, c.to, a)
		}
	}
	bob.quiet(isBtw)
	carol.quiet(isBtw)

	// bob asks his own agent on his other laptop, which teammates can't ask
	bob2.ask("q10", "bob/b2", "where are you?")
	if q := bob.question("q10"); q.From != "bob" {
		t.Fatalf("bob's own question: %+v", q)
	}
	// an agent whose laptop is reconnecting
	_ = bob2.c.CloseNow()
	eventually(t, "bob's second laptop never went", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return !h.links["d-bob2"].gone.IsZero()
	})
	alice.ask("q11", "bob/b3", "hi")
	if a := alice.answer("q11"); a.Final() || a.Note != "bob's laptop is away; murmur asks again until it is back" {
		t.Fatalf("reconnecting: %+v", a)
	}
	// an older laptop gets nothing back
	carol.ask("q12", "bob/b1", "hi")
	carol.quiet(isBtw)
	bob.quiet(isBtw)
	if n := asides(h); n != 2 { // bob's own question and alice's, still unanswered
		t.Fatalf("the hub remembers %d questions", n)
	}
	bob2 = carrier(t, url, "tok-bob2", protocol.Agent{Name: "b3", Btw: protocol.BtwApprove})
	alice.ask("q11", "bob/b3", "hi")
	if q := bob2.question("q11"); q.From != "alice" || q.Question != "hi" {
		t.Fatalf("once bob's laptop is back: %+v", q)
	}
}

// The laptop that asked a question asks it again after a reconnect: it goes again, and the answer comes to the new
// connection. The same id from another laptop is refused, an answer from a laptop the question didn't go to is
// dropped, and an answer for a laptop offline at the time is dropped too: it asks again once it is back. A question
// asked again goes to the laptop it went to, even once the agent is gone from the team, and while that laptop is
// away, a note says so.
func TestBtwAskedAgain(t *testing.T) {
	h, url := btwHub(t, nil, nil)
	alice := carrier(t, url, "tok-alice")
	bob := carrier(t, url, "tok-bob", protocol.Agent{Name: "b1", Btw: protocol.BtwApprove})
	carol := carrier(t, url, "tok-carol", protocol.Agent{Name: "c1", Btw: protocol.BtwAuto})
	runs(t, h, "bob", "b1")
	alice.ask("q1", "bob/b1", "what are you stuck on?")
	bob.question("q1")
	carol.reply(protocol.Btw{ID: "q1", Answer: "not from me"})
	alice.quiet(isBtw)

	_ = alice.c.CloseNow()
	alice = carrier(t, url, "tok-alice")
	alice.ask("q1", "bob/b1", "what are you stuck on?")
	if q := bob.question("q1"); q.From != "alice" || q.Question != "what are you stuck on?" {
		t.Fatalf("asked again: %+v", q)
	}
	carol.ask("q1", "bob/b1", "same id, another laptop")
	if a := carol.answer("q1"); !strings.Contains(a.Err, "another laptop asked a question under this id") {
		t.Fatalf("same id from carol: %+v", a)
	}
	bob.quiet(isBtw)
	bob.reply(protocol.Btw{ID: "q1", Answer: "the flaky test"})
	if a := alice.answer("q1"); a.Answer != "the flaky test" {
		t.Fatalf("answer: %+v", a)
	}
	carol.quiet(isBtw)

	alice.ask("q2", "bob/b1", "and now?")
	bob.question("q2")
	_ = alice.c.CloseNow()
	gone(t, h, "d-alice")
	bob.reply(protocol.Btw{ID: "q2", Answer: "rebasing"})
	eventually(t, "the hub never had the answer it dropped", func() bool { return final(h, "q2") })
	alice = carrier(t, url, "tok-alice")
	alice.ask("q2", "bob/b1", "and now?")
	bob.question("q2")
	bob.reply(protocol.Btw{ID: "q2", Answer: "rebasing"}) // from its history
	if a := alice.answer("q2"); a.Answer != "rebasing" {
		t.Fatalf("answer after a reconnect: %+v", a)
	}

	alice.ask("q3", "bob/b1", "lunch?")
	bob.question("q3")
	alice.ask("q3", "bob/b2", "lunch?")
	if a := alice.answer("q3"); !strings.Contains(a.Err, "a question was asked under this id already") {
		t.Fatalf("the id again, for another agent: %+v", a)
	}
	bob.reply(protocol.Btw{ID: "q3", Answer: "at noon"}) // the first question under the id still has its answer
	if a := alice.answer("q3"); a.Answer != "at noon" || a.To != "bob/b1" {
		t.Fatalf("answer to the first question under the id: %+v", a)
	}
	alice.ask("q4", "bob/b1", "and the tests?")
	bob.question("q4")
	_ = bob.c.CloseNow()
	eventually(t, "bob's laptop never went", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return !h.links["d-bob"].gone.IsZero()
	})
	minuteOn(h, "d-alice")
	alice.ask("q4", "bob/b1", "and the tests?")
	if a := alice.answer("q4"); a.Final() || !strings.Contains(a.Note, "bob's laptop is away") {
		t.Fatalf("asked again while bob's laptop is away: %+v", a)
	}
	bob = carrier(t, url, "tok-bob") // its agent is gone, its answer isn't
	alice.ask("q4", "bob/b1", "and the tests?")
	bob.question("q4")
}

// A person asks so many questions by the way an hour, each counted with their messages in the minute too. A question
// asked again isn't counted again.
func TestBtwLimits(t *testing.T) {
	h, url := btwHub(t, func(s *Settings) { s.BtwPerHour, s.SendsPerMinute = 2, 3 }, nil)
	alice := carrier(t, url, "tok-alice")
	bob := carrier(t, url, "tok-bob", protocol.Agent{Name: "b1", Btw: protocol.BtwAuto})
	runs(t, h, "bob", "b1")
	send := func(id string) protocol.Frame {
		alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: id, To: "carol", Text: "hi"}}) // kept for her: answered at once
		return alice.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameAck && f.ID == id })
	}
	send("m1")
	alice.ask("q1", "bob/b1", "first")
	bob.question("q1")
	send("m2")
	alice.ask("q2", "bob/b1", "second")
	if a := alice.answer("q2"); !strings.Contains(a.Err, "too many messages and questions in the last minute") {
		t.Fatalf("a fourth send in a minute: %+v", a)
	}
	if a := send("m3"); a.OK || !strings.Contains(a.Err, "too many messages") {
		t.Fatalf("the question wasn't counted with the messages: %+v", a)
	}
	h.mu.Lock()
	delete(h.sends, "alice") // a minute later
	h.mu.Unlock()
	alice.ask("q2", "bob/b1", "second")
	bob.question("q2")
	alice.ask("q3", "bob/b1", "third")
	if a := alice.answer("q3"); !strings.Contains(a.Err, "you asked 2 questions by the way in the last hour") {
		t.Fatalf("a third question in an hour: %+v", a)
	}
	minuteOn(h, "d-alice")
	alice.ask("q1", "bob/b1", "first") // two minutes later, or after a reconnect
	bob.question("q1")
	alice.quiet(isBtw)
}

// The team's admins turn questions by the way off and on: laptops hear it in a new welcome, and the hub refuses
// questions while they are off.
func TestBtwSwitch(t *testing.T) {
	h, url := btwHub(t, func(s *Settings) { s.Btw = false }, nil)
	alice := carrier(t, url, "tok-alice")
	bob := carrier(t, url, "tok-bob", protocol.Agent{Name: "b1", Btw: protocol.BtwAuto})
	if alice.welcome.Can != nil {
		t.Fatalf("the welcome says the hub carries questions by the way: %q", alice.welcome.Can)
	}
	runs(t, h, "bob", "b1")
	alice.askable("bob/b1", "")
	alice.ask("q1", "bob/b1", "hi")
	if a := alice.answer("q1"); !strings.Contains(a.Err, "this team's admins turned questions by the way off") {
		t.Fatalf("off: %+v", a)
	}
	bob.quiet(isBtw)
	s := h.Settings()
	s.Btw = true
	if err := h.SaveSettings(context.Background(), s, "alice"); err != nil {
		t.Fatal(err)
	}
	if w := bob.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameWelcome }); !slices.Equal(w.Can, []string{protocol.CanBtw}) {
		t.Fatalf("welcome once on: %q", w.Can)
	}
	alice.askable("bob/b1", protocol.BtwAuto)
	alice.ask("q2", "bob/b1", "hi")
	bob.question("q2")
}

// Two instances carry questions by the way as one hub: a question goes to the agent's laptop on the other instance
// and its answers come back; an older laptop there is never asked; the asker can move to the other instance and ask
// again, and the answering laptop can move too while a question waits for it.
func TestBtwAcrossInstances(t *testing.T) {
	one, two := twoInstances(t)
	alice := carrier(t, one.url, "tok-alice")
	bob := carrier(t, two.url, "tok-bob", protocol.Agent{Name: "b1", Btw: protocol.BtwApprove})
	carol := connect(t, two.url, "tok-carol", protocol.Agent{Name: "c1", Btw: protocol.BtwApprove}) // an older murmur
	runs(t, one.h, "bob", "b1")
	runs(t, one.h, "carol", "c1")
	alice.askable("bob/b1", protocol.BtwApprove, "carol/c1", "")

	alice.ask("q1", "bob/b1", "what are you stuck on?")
	if q := bob.question("q1"); q.From != "alice" || q.To != "bob/b1" {
		t.Fatalf("question: %+v", q)
	}
	bob.reply(protocol.Btw{ID: "q1", Note: "waits for bob"})
	if a := alice.answer("q1"); a.Note != "waits for bob" {
		t.Fatalf("note: %+v", a)
	}
	bob.reply(protocol.Btw{ID: "q1", Answer: "the flaky test", Tokens: 9000})
	if a := alice.answer("q1"); a.Answer != "the flaky test" || a.Tokens != 9000 || a.From != "alice" {
		t.Fatalf("answer: %+v", a)
	}
	eventually(t, "instance one never passed the answer on", func() bool { return final(one.h, "q1") })

	alice.ask("q2", "carol/c1", "hi")
	if a := alice.answer("q2"); !strings.Contains(a.Err, "older murmur") {
		t.Fatalf("an older laptop on the other instance: %+v", a)
	}
	carol.quiet(isBtw)

	_ = carol.c.CloseNow() // carol updates
	carol = carrier(t, two.url, "tok-carol", protocol.Agent{Name: "c1", Btw: protocol.BtwApprove})
	alice.ask("q3", "bob/b1", "and now?")
	bob.question("q3")
	carol.reply(protocol.Btw{ID: "q3", Answer: "not from me"})
	alice.quiet(isBtw)

	_ = alice.c.CloseNow()
	alice = carrier(t, two.url, "tok-alice")
	alice.ask("q3", "bob/b1", "and now?")
	bob.question("q3")
	bob.reply(protocol.Btw{ID: "q3", Answer: "rebasing"})
	if a := alice.answer("q3"); a.Answer != "rebasing" {
		t.Fatalf("answer on the other instance: %+v", a)
	}

	alice.ask("q4", "bob/b1", "done yet?")
	bob.question("q4")
	_ = bob.c.CloseNow()
	bob = carrier(t, one.url, "tok-bob", protocol.Agent{Name: "b1", Btw: protocol.BtwApprove})
	bob.reply(protocol.Btw{ID: "q4", Answer: "yes"})
	if a := alice.answer("q4"); a.Answer != "yes" || a.To != "bob/b1" {
		t.Fatalf("answer from the instance bob moved to: %+v", a)
	}
}

// A hub whose settings were stored before questions by the way carries them, at the default limit.
func TestBtwSettingsStoredBefore(t *testing.T) {
	st := store.NewMemory()
	var old map[string]any
	_ = json.Unmarshal(mustJSON(seed(nil)), &old)
	delete(old, "btw")
	delete(old, "btw_per_hour")
	_ = st.Put(context.Background(), settingsKey, mustJSON(old))
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef")}, st)
	if err != nil {
		t.Fatal(err)
	}
	if s := h.Settings(); !s.Btw || s.BtwPerHour != 30 {
		t.Fatalf("settings: %+v", s)
	}
}

// An agent asks: its laptop names it, and the hub writes its person, whatever the laptop says of them. Only an agent
// the laptop runs can ask; the answer comes back to its laptop, for the agent. An agent can't ask itself.
func TestBtwFromAnAgent(t *testing.T) {
	h, url := btwHub(t, nil, nil)
	alice := carrier(t, url, "tok-alice", protocol.Agent{Name: "a1"})
	bob := carrier(t, url, "tok-bob", protocol.Agent{Name: "b1", Btw: protocol.BtwApprove})
	runs(t, h, "bob", "b1")
	runs(t, h, "alice", "a1")
	for i, from := range []string{"alice/a1", "bob/a1"} {
		id := fmt.Sprint("q", i)
		alice.send(protocol.Frame{T: protocol.FrameBtw, Btw: &protocol.Btw{ID: id, From: from, To: "bob/b1", Question: "which port?"}})
		if q := bob.question(id); q.From != "alice/a1" || q.To != "bob/b1" {
			t.Fatalf("from %q: %+v", from, q)
		}
	}
	bob.reply(protocol.Btw{ID: "q0", Answer: "8080"})
	if a := alice.answer("q0"); a.Answer != "8080" || a.From != "alice/a1" || a.To != "bob/b1" {
		t.Fatalf("answer: %+v", a)
	}
	for _, c := range []struct{ id, from, to, want string }{
		{"q5", "alice/ghost", "bob/b1", "your session is not on murmur yet; ask again in a few seconds"},
		{"q6", "alice/a1", "alice/a1", "an agent can't ask itself by the way"},
		{"q1", "", "bob/b1", "a question was asked under this id already"}, // alice asks again what a1 asked
	} {
		alice.send(protocol.Frame{T: protocol.FrameBtw, Btw: &protocol.Btw{ID: c.id, From: c.from, To: c.to, Question: "which port?"}})
		if a := alice.answer(c.id); !strings.Contains(a.Err, c.want) {
			t.Errorf("%s from %q: %+v", c.id, c.from, a)
		}
	}
	bob.quiet(isBtw)
	if n := asides(h); n != 2 || final(h, "q1") { // q0 answered, q1 waits for bob: refusing alice's q1 didn't end a1's
		t.Fatalf("the hub remembers %d questions", n)
	}
}

// A person and their agents share the hour's questions by the way: more agents don't ask more. Questions between two
// agents count with their messages against loops, both ways, and a person's questions don't.
func TestBtwAgentLimits(t *testing.T) {
	h, url := btwHub(t, func(s *Settings) { s.BtwPerHour, s.LoopMax = 3, 3 }, nil)
	alice := carrier(t, url, "tok-alice", protocol.Agent{Name: "a1"}, protocol.Agent{Name: "a2"}, protocol.Agent{Name: "a3"})
	bob := carrier(t, url, "tok-bob", protocol.Agent{Name: "b1", Btw: protocol.BtwAuto}, protocol.Agent{Name: "b2", Btw: protocol.BtwAuto})
	runs(t, h, "bob", "b2")
	runs(t, h, "alice", "a3")
	ask := func(id, from, to string) {
		alice.send(protocol.Frame{T: protocol.FrameBtw, Btw: &protocol.Btw{ID: id, From: from, To: to, Question: "status?"}})
	}
	ask("q1", "", "bob/b1")
	bob.question("q1")
	ask("q2", "alice/a1", "bob/b1")
	bob.question("q2")
	ask("q3", "alice/a2", "bob/b2")
	bob.question("q3")
	ask("q4", "alice/a3", "bob/b1")
	if a := alice.answer("q4"); !strings.Contains(a.Err, "your person and their agents asked 3 questions by the way in the last hour") {
		t.Fatalf("a fourth question from alice's agents: %+v", a)
	}
	ask("q5", "", "bob/b1")
	if a := alice.answer("q5"); !strings.Contains(a.Err, "you asked 3 questions by the way in the last hour") {
		t.Fatalf("a fourth question from alice: %+v", a)
	}

	h.mu.Lock()
	delete(h.asks, "alice") // an hour later
	h.mu.Unlock()
	// a1 and b1: a question, then a message each way, makes three
	bob.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m1", From: "b1", To: "alice/a1", Text: "on it"}})
	alice.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && f.Msg.ID == "m1" })
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m2", From: "a1", To: "bob/b1", Text: "thanks"}})
	bob.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && f.Msg.ID == "m2" })
	ask("q6", "alice/a1", "bob/b1")
	if a := alice.answer("q6"); !strings.Contains(a.Err, "paused: alice/a1 and bob/b1 exchanged 3 messages and questions by the way") ||
		!strings.Contains(a.Err, "Ask your person whether to go on.") {
		t.Fatalf("a question past the loop limit: %+v", a)
	}
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m3", From: "a1", To: "bob/b1", Text: "and now?"}})
	if a := alice.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameAck && f.ID == "m3" }); a.OK || !strings.Contains(a.Err, "paused") {
		t.Fatalf("a message past the loop limit: %+v", a)
	}
	ask("q7", "", "bob/b1") // alice herself isn't in a loop
	bob.question("q7")
	ask("q8", "alice/a2", "bob/b1") // nor is a2
	bob.question("q8")
}

// After a hub restart, the hub doesn't know the questions laptops ask again, and a laptop asking again may come back
// before the agent's: while no laptop of the agent's person is online, the question waits, in a note. A first question
// is refused as before; one asked again of a person online who no longer runs the agent goes to each of their laptops
// saying so: the agent's laptop may not be back yet, or may hold its answer. A question asked again of a hub that
// knows it is logged at the debug level only.
func TestBtwAskedAgainAfterARestart(t *testing.T) {
	var logs lockedBuffer
	h, url := btwHub(t, nil, &logs)
	alice := carrier(t, url, "tok-alice", protocol.Agent{Name: "a1"})
	runs(t, h, "alice", "a1")
	again := func(id, from string) {
		alice.send(protocol.Frame{T: protocol.FrameBtw, Btw: &protocol.Btw{ID: id, From: from, To: "bob/b1", Question: "status?", Again: true}})
	}
	again("q1", "alice/a1")
	if a := alice.answer("q1"); a.Final() || a.Note != "bob's laptop is away; murmur asks again until it is back" || a.From != "alice/a1" {
		t.Fatalf("asked again while bob is away: %+v", a)
	}
	alice.ask("q2", "bob/b1", "status?")
	if a := alice.answer("q2"); a.Err != "bob/b1 is not running; `who` lists the agents you can ask" {
		t.Fatalf("a first question while bob is away: %+v", a)
	}
	if n := asides(h); n != 1 { // q1 waits
		t.Fatalf("the hub remembers %d questions", n)
	}
	bob := carrier(t, url, "tok-bob", protocol.Agent{Name: "b2", Btw: protocol.BtwApprove})
	runs(t, h, "bob", "b2")
	again("q4", "alice/a1")
	if q := bob.question("q4"); !q.Again || q.From != "alice/a1" || q.To != "bob/b1" {
		t.Fatalf("asked again of bob online without b1, his laptop gets: %+v", q)
	}
	if a := alice.answer("q4"); a.Final() || a.Note != "bob/b1 isn't on murmur now; murmur asks bob's laptops again until the one that has it answers" {
		t.Fatalf("asked again of bob online without b1: %+v", a)
	}
	bob2 := carrier(t, url, "tok-bob2", protocol.Agent{Name: "b1", Btw: protocol.BtwApprove})
	runs(t, h, "bob", "b1")
	again("q3", "alice/a1")
	if q := bob2.question("q3"); q.From != "alice/a1" || q.Again {
		t.Fatalf("asked again once b1 is back: %+v", q)
	}
	minuteOn(h, "d-alice")
	again("q3", "alice/a1")
	bob2.question("q3")
	bob.quiet(isBtw)
	if strings.Contains(logs.String(), "sent again") {
		t.Fatalf("a question asked again was logged: %s", logs.String())
	}
}

// The final answer may not reach the asking laptop, asleep when it came. The hub keeps the question: asked again, it
// goes to the laptop it went to, which answers from its history, even once the agent is gone; and it isn't counted
// again in the hour, nor refused.
func TestBtwFinalLost(t *testing.T) {
	h, url := btwHub(t, func(s *Settings) { s.BtwPerHour = 1 }, nil)
	alice := carrier(t, url, "tok-alice")
	bob := carrier(t, url, "tok-bob", protocol.Agent{Name: "b1", Btw: protocol.BtwApprove})
	runs(t, h, "bob", "b1")
	alice.ask("q1", "bob/b1", "what are you stuck on?")
	bob.question("q1")
	_ = alice.c.CloseNow()
	gone(t, h, "d-alice")
	bob.reply(protocol.Btw{ID: "q1", Answer: "the flaky test"}) // bob shared it while alice's laptop slept
	bob.ask("s1", "carol/c9", "?")                              // once the hub has its answer, it has the answer before
	bob.answer("s1")
	bob.send(protocol.Frame{T: protocol.FrameAgents, Agents: []protocol.Agent{}}) // b1 closes
	eventually(t, "b1 still runs", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.hostLocked("bob", "b1") == nil
	})
	alice = carrier(t, url, "tok-alice")
	alice.send(protocol.Frame{T: protocol.FrameBtw, Btw: &protocol.Btw{ID: "q1", To: "bob/b1", Question: "what are you stuck on?", Again: true}})
	if q := bob.question("q1"); q.From != "alice" || q.Question != "what are you stuck on?" || q.Again {
		t.Fatalf("asked again: %+v", q)
	}
	alice.quiet(isBtw)
	bob.reply(protocol.Btw{ID: "q1", Answer: "the flaky test"}) // from its history
	if a := alice.answer("q1"); a.Answer != "the flaky test" {
		t.Fatalf("the answer that was lost: %+v", a)
	}
}

// A hub that restarted counted none of the questions its laptops ask again: they were counted before, and go on, but
// a person's laptops can't say they ask again more questions in an hour than they could have asked.
func TestBtwAskedAgainNotCounted(t *testing.T) {
	h, url := btwHub(t, func(s *Settings) { s.BtwPerHour = 2 }, nil)
	alice := carrier(t, url, "tok-alice")
	bob := carrier(t, url, "tok-bob", protocol.Agent{Name: "b1", Btw: protocol.BtwApprove})
	runs(t, h, "bob", "b1")
	alice.ask("q1", "bob/b1", "one")
	bob.question("q1")
	alice.ask("q2", "bob/b1", "two")
	bob.question("q2")
	h.mu.Lock()
	clear(h.asides) // as after a restart, or on another instance; the hour's count stays, to show it isn't counted again
	h.mu.Unlock()
	again := func(id, question string) {
		alice.send(protocol.Frame{T: protocol.FrameBtw, Btw: &protocol.Btw{ID: id, To: "bob/b1", Question: question, Again: true}})
	}
	again("q1", "one")
	bob.question("q1")
	again("q2", "two")
	bob.question("q2")
	again("q3", "three") // a laptop that says it asks again what it never asked
	if a := alice.answer("q3"); !strings.Contains(a.Err, "you asked 2 questions by the way in the last hour") {
		t.Fatalf("a third question asked again: %+v", a)
	}
	bob.quiet(isBtw)
}

// A question asked again that the hub doesn't know, for an agent it doesn't see, goes to every laptop of the agent's
// person that carries questions by the way, saying it is asked again: the laptop that saw it answers from its history,
// and is the one asked from then on. Another person's laptop can't answer it.
func TestBtwAskedAgainOfEveryLaptop(t *testing.T) {
	h, url := btwHub(t, nil, nil)
	alice := carrier(t, url, "tok-alice")
	bob := carrier(t, url, "tok-bob") // b1 closed here; its answer is in this laptop's history
	bob2 := carrier(t, url, "tok-bob2", protocol.Agent{Name: "b2", Btw: protocol.BtwApprove})
	carol := carrier(t, url, "tok-carol", protocol.Agent{Name: "c1", Btw: protocol.BtwAuto})
	runs(t, h, "bob", "b2")
	runs(t, h, "carol", "c1")
	again := func() {
		alice.send(protocol.Frame{T: protocol.FrameBtw, Btw: &protocol.Btw{ID: "q1", To: "bob/b1", Question: "status?", Again: true}})
	}
	again()
	for _, l := range []*laptop{bob, bob2} {
		if q := l.question("q1"); !q.Again || q.From != "alice" || q.To != "bob/b1" {
			t.Fatalf("asked again: %+v", q)
		}
	}
	if a := alice.answer("q1"); a.Final() || !strings.Contains(a.Note, "murmur asks bob's laptops again") {
		t.Fatalf("asked again: %+v", a)
	}
	carol.quiet(isBtw)
	carol.reply(protocol.Btw{ID: "q1", Answer: "not from me"})
	alice.quiet(isBtw)
	bob.reply(protocol.Btw{ID: "q1", Answer: "rebasing"})
	if a := alice.answer("q1"); a.Answer != "rebasing" || a.From != "alice" || a.To != "bob/b1" {
		t.Fatalf("answer: %+v", a)
	}
	minuteOn(h, "d-alice")
	again()
	bob.question("q1")
	bob2.quiet(isBtw)
}

// An agent whose person turned teammates' questions off says mine: teammates and their agents are refused, saying so,
// while its person and their agents, from any of their laptops, still ask it. A laptop that doesn't carry questions,
// and a hub whose admins turned them off, show it as one nobody can ask.
func TestBtwMine(t *testing.T) {
	h, url := btwHub(t, nil, nil)
	alice := carrier(t, url, "tok-alice", protocol.Agent{Name: "a1"})
	bob := carrier(t, url, "tok-bob", protocol.Agent{Name: "b1", Btw: protocol.BtwMine})
	bob2 := carrier(t, url, "tok-bob2", protocol.Agent{Name: "b2"})
	connect(t, url, "tok-carol", protocol.Agent{Name: "c1", Btw: protocol.BtwMine}) // an older murmur
	runs(t, h, "alice", "a1")
	runs(t, h, "bob", "b2")
	runs(t, h, "carol", "c1")
	alice.askable("bob/b1", protocol.BtwMine, "carol/c1", "")
	for _, from := range []string{"", "alice/a1"} {
		id := "q-" + strings.ReplaceAll(from, "/", "-")
		alice.send(protocol.Frame{T: protocol.FrameBtw, Btw: &protocol.Btw{ID: id, From: from, To: "bob/b1", Question: "hi"}})
		if a := alice.answer(id); a.Err != "bob/b1 doesn't take questions by the way: bob turned them off" {
			t.Errorf("from %q: %+v", from, a)
		}
	}
	bob.quiet(isBtw)
	for _, from := range []string{"", "bob/b2"} { // bob, and his agent, on his other laptop
		id := "q-" + strings.ReplaceAll(from, "/", "-") + "-bob"
		bob2.send(protocol.Frame{T: protocol.FrameBtw, Btw: &protocol.Btw{ID: id, From: from, To: "bob/b1", Question: "where are you?"}})
		if q := bob.question(id); q.From != cmp.Or(from, "bob") {
			t.Errorf("from %q: %+v", from, q)
		}
	}
	s := h.Settings()
	s.Btw = false
	if err := h.SaveSettings(context.Background(), s, "alice"); err != nil {
		t.Fatal(err)
	}
	alice.askable("bob/b1", "")
}

// A laptop that says it asks again a question the hub doesn't know may never have asked it: the question isn't counted
// again, but the agent's person must still let teammates ask it.
func TestBtwAskedAgainStillOptedOut(t *testing.T) {
	h, url := btwHub(t, nil, nil)
	alice := carrier(t, url, "tok-alice")
	bob := carrier(t, url, "tok-bob", protocol.Agent{Name: "b1", Btw: protocol.BtwMine}, protocol.Agent{Name: "b2"},
		protocol.Agent{Name: "b3", Btw: protocol.BtwApprove})
	runs(t, h, "bob", "b3")
	again := func(id, to string) {
		alice.send(protocol.Frame{T: protocol.FrameBtw, Btw: &protocol.Btw{ID: id, To: to, Question: "status?", Again: true}})
	}
	again("q1", "bob/b1")
	if a := alice.answer("q1"); a.Err != "bob/b1 doesn't take questions by the way: bob turned them off" {
		t.Fatalf("mine, asked again: %+v", a)
	}
	again("q2", "bob/b2")
	if a := alice.answer("q2"); !strings.Contains(a.Err, "bob/b2 doesn't take questions by the way") {
		t.Fatalf("not askable, asked again: %+v", a)
	}
	bob.quiet(isBtw)
	again("q3", "bob/b3")
	if q := bob.question("q3"); q.From != "alice" || q.To != "bob/b3" {
		t.Fatalf("asked again: %+v", q)
	}
	if n := asides(h); n != 1 {
		t.Fatalf("the hub remembers %d questions", n)
	}
}

// A question asked again and refused is over, however the asking laptop writes the agent: the hub forgets it then, not
// a day later.
func TestBtwRefusedAgainForgotten(t *testing.T) {
	h, url := btwHub(t, nil, nil)
	alice := carrier(t, url, "tok-alice")
	bob := carrier(t, url, "tok-bob", protocol.Agent{Name: "b1", Btw: protocol.BtwAuto})
	runs(t, h, "bob", "b1")
	alice.ask("q1", "bob/b1", "status?")
	bob.question("q1")
	s := h.Settings()
	s.Btw = false
	if err := h.SaveSettings(context.Background(), s, "alice"); err != nil {
		t.Fatal(err)
	}
	alice.ask("q1", " Bob/b1", "status?")
	if a := alice.answer("q1"); !strings.Contains(a.Err, "this team's admins turned questions by the way off") {
		t.Fatalf("asked again once off: %+v", a)
	}
	if n := asides(h); n != 0 {
		t.Fatalf("the hub remembers %d questions", n)
	}
}

// countingBus counts what an instance publishes, by kind.
type countingBus struct {
	*bus.Local
	mu    sync.Mutex
	kinds map[string]int
}

func (b *countingBus) Publish(ctx context.Context, e bus.Event) error {
	b.mu.Lock()
	b.kinds[e.Kind]++
	b.mu.Unlock()
	return b.Local.Publish(ctx, e)
}

func (b *countingBus) count(kind string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.kinds[kind]
}

// An answer to a question an instance doesn't know goes to the other instances, one event on the bus each: only when
// there are other instances. The hub takes at most maxAnswers answers a minute from one laptop.
func TestBtwAnswersRelayed(t *testing.T) {
	b := &countingBus{Local: bus.NewLocal(), kinds: map[string]int{}}
	h, url := btwHubOn(t, nil, nil, b)
	bob := carrier(t, url, "tok-bob", protocol.Agent{Name: "b1", Btw: protocol.BtwAuto})
	runs(t, h, "bob", "b1")
	settle := func(id string) { // the hub has read what bob sent before
		bob.ask(id, "carol/c9", "?")
		bob.answer(id)
	}
	for i := range 5 {
		bob.reply(protocol.Btw{ID: fmt.Sprint("x", i), Answer: strings.Repeat("a", protocol.MaxBtwAnswer)})
	}
	settle("s1")
	if n := b.count(evBtw); n != 0 {
		t.Fatalf("%d answers went to other instances, and there are none", n)
	}
	h.mu.Lock() // another instance holds a laptop
	h.remote["d-carol"] = &presence{Device: "d-carol", Email: "carol@x.com", Handle: "carol", Online: true, instance: "two", seen: time.Now()}
	h.mu.Unlock()
	for i := range maxAnswers + 10 {
		bob.reply(protocol.Btw{ID: fmt.Sprint("y", i), Answer: "a"})
	}
	settle("s2")
	eventually(t, "the answers never went to the other instance", func() bool { return b.count(evBtw) >= maxAnswers-5 })
	time.Sleep(100 * time.Millisecond)
	if n := b.count(evBtw); n != maxAnswers-5 { // with the 5 before
		t.Fatalf("%d answers went to the other instance in a minute", n)
	}
}

// A laptop that asks one question again and again, buggy or modified, has it go on once a minute from one connection:
// the laptop holding the agent answers each time from its history, and those answers count. Asked again two minutes
// later, on a new connection, or once the agent's laptop connected again, it goes on. The notes the agent's laptop
// sends again, and answers to questions the hub doesn't know, never crowd out the final answer to another asker's
// question.
func TestBtwAskedAgainTooOften(t *testing.T) {
	h, url := btwHub(t, nil, nil)
	alice := carrier(t, url, "tok-alice")
	carol := carrier(t, url, "tok-carol")
	bob := carrier(t, url, "tok-bob", protocol.Agent{Name: "b1", Btw: protocol.BtwApprove})
	runs(t, h, "bob", "b1")
	const held = "answered; waits for bob to share it"
	alice.ask("q1", "bob/b1", "what are you stuck on?")
	bob.question("q1")
	bob.reply(protocol.Btw{ID: "q1", Note: held})
	alice.answer("q1")
	for range maxAnswers + 10 {
		alice.ask("q1", "bob/b1", "what are you stuck on?")
	}
	alice.ask("s1", "carol/c9", "?") // the hub has read what alice sent before once it refuses this
	alice.answer("s1")
	bob.quiet(isBtw)

	for range maxAnswers + 10 { // as an older hub would have bob's laptop answer each from its history
		bob.reply(protocol.Btw{ID: "q1", Note: held})
	}
	for i := range maxAnswers + 10 {
		bob.reply(protocol.Btw{ID: fmt.Sprint("old", i), Answer: "an answer nobody waits for"})
	}
	carol.ask("q2", "bob/b1", "is the build green?")
	bob.question("q2")
	bob.reply(protocol.Btw{ID: "q2", Answer: "green since noon"})
	if a := carol.answer("q2"); a.Answer != "green since noon" {
		t.Fatalf("carol's answer: %+v", a)
	}

	minuteOn(h, "d-alice") // alice's laptop asks again two minutes later
	alice.ask("q1", "bob/b1", "what are you stuck on?")
	bob.question("q1")
	_ = alice.c.CloseNow()
	alice = carrier(t, url, "tok-alice")
	alice.ask("q1", "bob/b1", "what are you stuck on?") // and after a reconnect
	bob.question("q1")
	_ = bob.c.CloseNow()
	bob = carrier(t, url, "tok-bob", protocol.Agent{Name: "b1", Btw: protocol.BtwApprove})
	runs(t, h, "bob", "b1")
	alice.ask("q1", "bob/b1", "what are you stuck on?") // and once bob's laptop is back, which may hold an answer that didn't leave
	bob.question("q1")
}

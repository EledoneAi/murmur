package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/store"
)

// A roster put together from the team, encoded once, is the frame json.Marshal would write, for a person with
// overlaps and for one without; a laptop doesn't get the same one twice. The team page's event, put together the
// same way, is the view it would write, and working it out leaves the laptops' roster alone.
func TestRosterEncoding(t *testing.T) {
	a1 := protocol.Agent{Name: "a1", Repo: "Org/App", Status: protocol.StatusWaiting, Since: 2, Tree: "t1", Files: []string{"x.go"},
		PR: 7, PRURL: "javascript:alert(1)"}
	b1 := protocol.Agent{Name: "b1", Repo: "org/app", Status: protocol.StatusBusy, Since: 1, Tree: "t2", Files: []string{"x.go", "y.go"}}
	holders := []holder{{dev: "d-alice", handle: "alice", agents: []protocol.Agent{a1}, online: true},
		{dev: "d-bob", handle: "bob", agents: []protocol.Agent{b1}, online: true}}
	shown := func(a protocol.Agent) protocol.Agent {
		a.Files, a.Tree = nil, ""
		return a
	}
	people := []protocol.Person{{Handle: "alice", Name: "Alice <a>", Online: true, Agents: []protocol.Agent{shown(a1)}},
		{Handle: "bob", Online: true, Colour: 1, Agents: []protocol.Agent{shown(b1)}}, {Handle: "carol", Colour: 2}}
	x := newIndex(holders, Defaults())

	r := newRoster(people, x)
	for _, handle := range []string{"alice", "carol"} {
		head, tail, _ := r.frame(handle)
		want := mustJSON(protocol.Frame{T: protocol.FrameRoster, People: people, Pairs: x.current(handle)})
		if got := append(slices.Clone(head), tail...); !bytes.Equal(got, want) {
			t.Fatalf("%s's roster:\n%s\nwant\n%s", handle, got, want)
		}
	}
	if pairs := x.current("alice"); len(pairs) != 1 || pairs[0].Other != "bob/b1" {
		t.Fatalf("alice's overlaps: %+v", pairs)
	}
	if head, tail, _ := newRoster(nil, x).frame("alice"); string(head)+string(tail) != string(mustJSON(protocol.Frame{T: protocol.FrameRoster,
		Pairs: x.current("alice")})) {
		t.Fatalf("roster of nobody: %s%s", head, tail)
	}
	handed := 0
	l := newLink("alice")
	l.roster = func(...[]byte) { handed++ }
	if r.hand(l) == 0 || r.hand(l) != 0 || newRoster(people, x).hand(l) != 0 || handed != 1 {
		t.Fatalf("handed the same roster %d times", handed)
	}

	before := mustJSON(people)
	news := []newsItem{{From: "bob/b1", Repo: "org/app", Text: "main is <frozen>", At: 1, Until: 2}}
	var got, want teamView
	if err := json.Unmarshal(bytes.Join(newTeamEvent(people, holders, news).event("alice"), nil), &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustJSON(people), before) {
		t.Fatal("the team page's view changed the laptops' roster")
	}
	page := pageOrder(slices.Clone(people))
	_ = json.Unmarshal(mustJSON(teamView{Me: "alice", People: page, PRs: pullRequests(page), Mine: []string{"a1"}, Repos: repos(page),
		News: news}), &want)
	if !reflect.DeepEqual(got, want) || got.People[0].Agents[0].PRURL != "" {
		t.Fatalf("team page:\n%+v\nwant\n%+v", got, want)
	}
}

// busy reports a tool call of one of these laptops every 20 ms, as a big, busy team does, until the test ends.
func busy(t *testing.T, h *Hub, links []*link) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			l := links[i%len(links)]
			h.mu.Lock()
			agents := copyAgents(l.agents)
			h.mu.Unlock()
			agents[0].Since, agents[0].Doing = int64(i+2), fmt.Sprintf("tool call %d", i)
			h.handle(l, protocol.Frame{T: protocol.FrameAgents, Agents: agents})
		}
	}()
}

// The team pages take their turn as one more laptop: with a hundred open on a big, busy team, the laptops keep getting
// rosters, and one that just said hello gets its first soon.
func TestRostersWithManyPages(t *testing.T) {
	h, links := bigTeam(t, 400)
	var handed atomic.Int64
	h.mu.Lock()
	h.out.mu.Lock() // the roster loop hands rosters under it, and already runs
	for _, l := range links {
		l.roster = func(...[]byte) { handed.Add(1) }
	}
	h.out.mu.Unlock()
	for range 100 {
		c := make(chan struct{}, 1)
		h.streams[c] = true
		go func() {
			for range c { // a page that reads every event at once
			}
		}()
	}
	h.mu.Unlock()
	busy(t, h, links)
	time.Sleep(time.Second)

	got := make(chan struct{})
	fresh := newLink("fresh")
	var once sync.Once
	fresh.roster = func(...[]byte) { once.Do(func() { close(got) }) }
	h.mu.Lock()
	u := &store.User{Email: "fresh@x.com", Handle: "fresh"}
	h.users[u.Email], h.handles[u.Handle], h.links[fresh.dev], h.colours = u, u, fresh, nil
	h.mu.Unlock()
	before := handed.Load()
	h.handle(fresh, protocol.Frame{T: protocol.FrameHello, Agents: []protocol.Agent{{Name: "f1", Status: protocol.StatusIdle}}})
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("a laptop that said hello got no roster in 5 s")
	}
	time.Sleep(time.Second)
	if n := handed.Load() - before; n < 100 {
		t.Fatalf("the laptops got %d rosters in a second or more", n)
	}
}

// A message reaches a laptop after the roster that has its sender as it was when it sent, however long the laptops'
// turn for rosters: the laptop's mutes and console see the sender's new repo first.
func TestMessageAfterItsSendersRoster(t *testing.T) {
	h, links := bigTeam(t, 1000)
	var mu sync.Mutex
	var log []string // what reached the recipient, in order
	recipient, sender := links[0], links[len(links)-1]
	h.out.mu.Lock() // the roster loop hands rosters under it, and already runs
	recipient.roster = func(parts ...[]byte) {
		if bytes.Contains(bytes.Join(parts, nil), []byte(`"acme/muted"`)) {
			mu.Lock()
			log = append(log, "roster")
			mu.Unlock()
		}
	}
	h.out.mu.Unlock()
	busy(t, h, links[1:len(links)-1])
	time.Sleep(300 * time.Millisecond)

	moved := copyAgents(sender.agents)
	moved[0].Repo = "acme/muted"
	h.handle(sender, protocol.Frame{T: protocol.FrameAgents, Agents: moved})
	for built := false; !built; time.Sleep(10 * time.Millisecond) { // the roster that has it is built, not handed
		h.mu.Lock()
		changed := sender.changed
		h.mu.Unlock()
		h.out.mu.Lock()
		built = h.out.r != nil && h.out.r.version >= changed
		h.out.mu.Unlock()
	}
	h.route(sender, protocol.Msg{ID: "m1", From: "a1", To: "p0/a1", Text: "hello"})
	for f := range recipient.out {
		if f.T == protocol.FrameMsg {
			mu.Lock()
			log = append(log, "message")
			mu.Unlock()
			break
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if r, m := slices.Index(log, "roster"), slices.Index(log, "message"); r < 0 || m < r {
		t.Fatalf("reached the recipient in this order: %q", log)
	}
}

// A laptop goes first for rosters once it says hello, when a roster has its agents, not when it connects: one worked
// out in between would miss them, and on a big, busy team the laptop would wait a whole round for one that has them.
func TestRosterAfterHello(t *testing.T) {
	h, links := bigTeam(t, 1000)
	busy(t, h, links)
	time.Sleep(300 * time.Millisecond)

	got := make(chan struct{})
	var once sync.Once
	late := newLink("late")
	late.roster = func(parts ...[]byte) {
		if bytes.Contains(bytes.Join(parts, nil), []byte(`"z-late"`)) {
			once.Do(func() { close(got) })
		}
	}
	h.mu.Lock()
	u := &store.User{Email: late.email, Handle: late.handle}
	h.users[u.Email], h.handles[u.Handle], h.colours = u, u, nil
	h.mu.Unlock()
	h.attach(t.Context(), late, store.Device{ID: late.dev, Email: late.email})
	time.Sleep(300 * time.Millisecond) // a roster is worked out before its hello
	h.handle(late, protocol.Frame{T: protocol.FrameHello, Agents: []protocol.Agent{{Name: "z-late", Status: protocol.StatusIdle}}})
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("no roster with the laptop's own agents within 2 s of its hello")
	}
}

// copyAgents copies agents as a laptop's next frame brings them: the hub cleans what it gets in place.
func copyAgents(agents []protocol.Agent) []protocol.Agent {
	out := slices.Clone(agents)
	for i := range out {
		out[i].Files, out[i].Also = slices.Clone(out[i].Files), slices.Clone(out[i].Also)
	}
	return out
}

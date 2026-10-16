package hub

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
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

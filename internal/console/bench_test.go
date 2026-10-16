package console

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

// The console's costs, on the demo team and on teams of 10, 100 and 500 people with four agents each and the 300
// messages the daemon sends at most: go test ./internal/console -run '^$' -bench . -benchmem

// teamOf is the demo team grown to n people, each new one with four agents spread over twenty repos, in
// constellations of three people that talk, and some messages to you, all within the daemon's 300.
func teamOf(n int) ipc.Snapshot {
	s := demoSnapshot(demoNow)
	first := len(s.People)
	at := func(d time.Duration) int64 { return demoNow.Add(-d).UnixMilli() }
	statuses := []string{protocol.StatusBusy, protocol.StatusBusy, protocol.StatusIdle, protocol.StatusWaiting, protocol.StatusShell}
	text := strings.Repeat("The certifier splits shadow and live runs now; rebase after my next push and rerun the fixtures. ", 3)
	for i := first; i < n; i++ {
		p := protocol.Person{Handle: fmt.Sprintf("p%03d", i), Online: i%9 != 0, Colour: i % protocol.Colours}
		for j := range 4 {
			a := protocol.Agent{Name: fmt.Sprintf("svc-%d", j), Repo: fmt.Sprintf("acme/svc-%d", (i+j)%20),
				Branch: fmt.Sprintf("feat/topic-%d-%d", i/3, j), Tree: fmt.Sprintf("t-%d-%d", i, j), Title: "Work on topic " + fmt.Sprint(i),
				Status: statuses[(i+j)%len(statuses)], Since: at(time.Duration(i+j) * time.Minute), Doing: "Editing billing/certifier.go",
				Kinds: "rreexxrreewrrseexxrrmeexxrreex", Added: 10 * j, Deleted: j, Changed: j, Context: (i * 7) % 100}
			if a.Status == protocol.StatusWaiting {
				a.Needs = "input"
			}
			if j == 0 {
				a.PR, a.PRURL = 1000+i, fmt.Sprintf("https://github.com/acme/svc/pull/%d", 1000+i)
			}
			p.Agents = append(p.Agents, a)
		}
		s.People = append(s.People, p)
	}
	for i := 0; len(s.Msgs) < 300 && n-first >= 3; i++ {
		groups := (n - first) / 3 // three people talk in each constellation
		g, r := first+3*(i%groups), i/groups
		from, to := s.People[g+r%3].Handle+"/svc-"+fmt.Sprint(r%4), s.People[g+(r+1)%3].Handle+"/svc-"+fmt.Sprint((r+1)%4)
		if i%25 == 0 {
			to = "alice"
		}
		s.Msgs = append(s.Msgs, protocol.Msg{ID: fmt.Sprintf("x%d", i), From: from, To: to, Text: text, At: at(time.Duration(300-i) * time.Minute / 20)})
	}
	return s
}

// benchModel is the console on that team at 160×50, the first screen drawn.
func benchModel(s ipc.Snapshot) *model {
	m := demo(160, 50, dark)
	m.snap = s
	m.screen()
	return m
}

var benchTeams = []struct {
	name string
	snap func() ipc.Snapshot
}{
	{"demo", func() ipc.Snapshot { return demoSnapshot(demoNow) }},
	{"10", func() ipc.Snapshot { return teamOf(10) }},
	{"100", func() ipc.Snapshot { return teamOf(100) }},
	{"500", func() ipc.Snapshot { return teamOf(500) }},
}

// A frame: what Bubble Tea asks for after every message.
func BenchmarkView(b *testing.B) {
	for _, tm := range benchTeams {
		b.Run(tm.name, func(b *testing.B) {
			m := benchModel(tm.snap())
			b.ReportAllocs()
			for b.Loop() {
				m.View()
			}
		})
	}
}

// A key, and the frame it draws: input to frame.
func BenchmarkKey(b *testing.B) {
	down, up := tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: tea.KeyUp}
	for _, tm := range benchTeams {
		b.Run(tm.name, func(b *testing.B) {
			m := benchModel(tm.snap())
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				k := down
				if i++; i%20 >= 10 {
					k = up
				}
				m.Update(k)
				m.View()
			}
		})
	}
}

// A new snapshot from the daemon, as the demo sends one every two seconds: agents call tools, nothing else moves.
func BenchmarkSnapshot(b *testing.B) {
	for _, tm := range benchTeams {
		b.Run(tm.name, func(b *testing.B) {
			m := benchModel(tm.snap())
			next := [2]ipc.Snapshot{tm.snap(), tm.snap()}
			for p := range next[1].People {
				for a := range next[1].People[p].Agents {
					next[1].People[p].Agents[a].Kinds = "x" + next[1].People[p].Agents[a].Kinds
				}
			}
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				i++
				m.Update(snapMsg(next[i%2]))
				m.View()
			}
		})
	}
}

// What the console reads from the daemon for each snapshot.
func BenchmarkDecode(b *testing.B) {
	for _, tm := range benchTeams {
		b.Run(tm.name, func(b *testing.B) {
			data, _ := json.Marshal(tm.snap())
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			for b.Loop() {
				var s ipc.Snapshot
				_ = json.Unmarshal(data, &s)
			}
		})
	}
}

// A long conversation open: 300 messages between two agents.
func BenchmarkThread(b *testing.B) {
	s := teamOf(100)
	for i := range s.Msgs {
		s.Msgs[i].From, s.Msgs[i].To = "p010/svc-0", "p011/svc-1"
		if i%2 == 1 {
			s.Msgs[i].From, s.Msgs[i].To = s.Msgs[i].To, s.Msgs[i].From
		}
	}
	m := benchModel(s)
	m.selected = "agent:p010/svc-0"
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.thread == nil {
		b.Fatal("no thread")
	}
	b.ReportAllocs()
	for b.Loop() {
		m.View()
	}
}

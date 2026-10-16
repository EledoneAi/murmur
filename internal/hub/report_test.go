package hub

import (
	"context"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/store"
)

// follow runs the lease holder's look at the team every jobEvery, from one time to another.
func follow(h *Hub, from, to time.Time) {
	for at := from; !at.After(to); at = at.Add(jobEvery) {
		h.followTeam(context.Background(), at)
	}
}

// Two instances count a week: waits on one, collisions on the other, messages and merge notices on both. On Monday
// at the start of the working day, the report sums them and is posted once, naming no one.
func TestWeeklyReport(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	srv, chat := chatServer(t)
	quiet := func(s *Settings) { s.WaitPingMinutes = 0 }
	one, two := pingHub(t, st, "one", srv.URL, quiet), pingHub(t, st, "two", srv.URL, quiet)

	// Wednesday 7 October: three waits, of 4 minutes, 2 h 10 min, and under a minute for a tool's approval
	a := place(one, "alice", waiting("a1", paris(t, "2026-10-07 10:00:00")))
	follow(one, paris(t, "2026-10-07 10:00:30"), paris(t, "2026-10-07 10:04:00"))
	setAgents(one, a, protocol.Agent{Name: "a1", Status: protocol.StatusBusy, Since: paris(t, "2026-10-07 10:04:00").UnixMilli()})
	follow(one, paris(t, "2026-10-07 10:04:30"), paris(t, "2026-10-07 10:10:00"))
	setAgents(one, a, waiting("a1", paris(t, "2026-10-07 10:10:00")))
	follow(one, paris(t, "2026-10-07 10:10:30"), paris(t, "2026-10-07 12:20:00"))
	setAgents(one, a, protocol.Agent{Name: "a1", Status: protocol.StatusIdle, Since: paris(t, "2026-10-07 12:20:00").UnixMilli()})
	b := place(one, "bob", protocol.Agent{Name: "b1", Status: protocol.StatusBusy, Needs: "permission"})
	follow(one, paris(t, "2026-10-07 12:20:30"), paris(t, "2026-10-07 12:21:00"))
	setAgents(one, b, protocol.Agent{Name: "b1", Status: protocol.StatusBusy})
	one.followTeam(ctx, paris(t, "2026-10-07 12:21:30"))

	// Thursday and Friday: two checkouts change api/routes.go, counted once a day however many agents share them,
	// and not again by the other instance when it takes the lease on the same day
	collide := func(h *Hub) {
		place(h, "carol", protocol.Agent{Name: "c1", Repo: "Acme/API", Tree: "t1", Files: []string{"api/routes.go", "x.go"}})
		place(h, "dave", protocol.Agent{Name: "d1", Repo: "acme/api", Tree: "t2", Files: []string{"api/routes.go"}},
			protocol.Agent{Name: "d2", Repo: "acme/api", Tree: "t2", Files: []string{"api/routes.go"}})
	}
	collide(two)
	follow(two, paris(t, "2026-10-08 10:00:00"), paris(t, "2026-10-08 10:05:00"))
	collide(one)
	one.followTeam(ctx, paris(t, "2026-10-08 10:06:00"))
	two.followTeam(ctx, paris(t, "2026-10-09 10:00:00"))

	one.count("2026-W41", func(w *weekStats) { w.Crossed += 4; w.Merges++ })
	two.count("2026-W41", func(w *weekStats) { w.Crossed++; w.Merges++ })
	one.flushStats(ctx)
	two.flushStats(ctx)
	var ids []string
	if err := getJSON(ctx, st, statsKey+"2026-W41", &ids); err != nil || !slices.Contains(ids, "one") || !slices.Contains(ids, "two") {
		t.Fatalf("instances that counted: %v %v", ids, err)
	}

	one.weeklyReport(ctx, paris(t, "2026-10-12 08:59:30"))
	if got := chat.all(); len(got) != 0 {
		t.Fatalf("posted before the working day: %q", got)
	}
	one.weeklyReport(ctx, paris(t, "2026-10-12 09:00:00"))
	one.weeklyReport(ctx, paris(t, "2026-10-12 09:00:30"))
	two.weeklyReport(ctx, paris(t, "2026-10-12 09:01:00"))
	got := chat.all()
	if len(got) != 1 {
		t.Fatalf("reports: %q", got)
	}
	want := "The team's week on murmur, from Monday 5 October:\n" +
		"- Agents waited for their people 3 times: the median wait was 4 min, the longest 2 h 10 min.\n" +
		"- Files two checkouts changed at once most often: api/routes.go in acme/api (2 times).\n" +
		"- 5 messages went between different people's agents.\n" +
		"- 2 merge notices told agents a pull request had merged over their work.\n" +
		"https://hub.example.com"
	if got[0] != want {
		t.Fatalf("report:\n%s\nwant:\n%s", got[0], want)
	}
	for _, name := range []string{"alice", "bob", "carol", "dave"} {
		if strings.Contains(got[0], name) {
			t.Fatalf("the report names %s", name)
		}
	}
}

// Messages count when one person's agent writes to another person's agent: not to a person, not between one
// person's own agents.
func TestCrossedMessagesCounted(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	for _, u := range []store.User{{Email: "alice@x.com", Handle: "alice"}, {Email: "bob@x.com", Handle: "bob"}} {
		_ = st.PutUser(ctx, u)
		_ = st.PutDevice(ctx, store.Device{ID: "d-" + u.Handle, Email: u.Email, TokenHash: hashToken("tok-" + u.Handle), Created: time.Now()})
	}
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef")}, st)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	alice := connect(t, srv.URL, "tok-alice", protocol.Agent{Name: "a1", Status: "busy"}, protocol.Agent{Name: "a2", Status: "busy"})
	bob := connect(t, srv.URL, "tok-bob", protocol.Agent{Name: "b1", Status: "idle"})
	for _, m := range []protocol.Msg{
		{ID: "m1", From: "a1", To: "bob/b1", Text: "crossed"},
		{ID: "m2", From: "a1", To: "alice/a2", Text: "my own agent"},
		{ID: "m3", From: "a1", To: "bob", Text: "a person"},
	} {
		alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &m})
	}
	bob.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && f.Msg.ID == "m1" && !f.Echo })
	bob.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && f.Msg.ID == "m3" && !f.Echo })
	alice.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && f.Msg.ID == "m2" && !f.Echo })
	h.flushStats(ctx)
	w, err := h.weekTotals(ctx, weekOf(time.Now(), "UTC"))
	if err != nil || w.Crossed != 1 {
		t.Fatalf("crossed: %+v %v", w, err)
	}
}

func TestMedian(t *testing.T) {
	for _, c := range []struct {
		waits map[int]int
		want  int
	}{{map[int]int{4: 1}, 4}, {map[int]int{0: 1, 4: 1, 130: 1}, 4}, {map[int]int{1: 2, 9: 2}, 1}, {map[int]int{2: 1, 3: 5, 60: 1}, 3}} {
		if got := median(c.waits); got != c.want {
			t.Errorf("median(%v) = %d, want %d", c.waits, got, c.want)
		}
	}
	if reportText(time.Now(), weekStats{}, "https://hub.example.com") != "" {
		t.Error("a quiet week has a report")
	}
}

// limited keeps values of a hundred pairs at most, as Firestore keeps documents of 1 MiB.
type limited struct{ *store.Memory }

func (limited) MaxValue() int { return 64 + 100*pairSize }

// The day's collisions are kept in the store, so the next lease holder doesn't count them again: all of them, past
// 10,000 pairs, in a store without a limit; as many as fit in one with a limit, from the first look on, and the next
// holder counts only the others again.
func TestCollisionsKept(t *testing.T) {
	for _, tc := range []struct {
		st   store.Store
		kept int
	}{{store.NewMemory(), 12400}, {limited{store.NewMemory()}, 100}} {
		ctx := context.Background()
		at := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
		counted := func(name string) int {
			h, err := newHub(t, Config{Secret: []byte("0123456789abcdef"), Instance: name}, tc.st)
			if err != nil {
				t.Fatal(err)
			}
			files := make([]string, 25)
			for f := range files {
				files[f] = fmt.Sprintf("api/file%d.go", f)
			}
			for i := range 32 { // 32 checkouts change 25 files: 12,400 pairs
				place(h, fmt.Sprintf("c%d", i), protocol.Agent{Name: "a1", Repo: "acme/big", Tree: fmt.Sprintf("t%d", i), Files: files})
			}
			h.followTeam(ctx, at)
			at = at.Add(jobEvery)
			n := 0
			h.stats.mu.Lock()
			defer h.stats.mu.Unlock()
			for _, w := range h.stats.fresh {
				for _, c := range w.Collisions {
					n += c
				}
			}
			return n
		}
		first := counted("one")
		var c collided
		if err := getJSON(ctx, tc.st, collidedKey, &c); err != nil || len(c.Seen) != tc.kept {
			t.Fatalf("kept %d pairs of %d, want %d (%v)", len(c.Seen), first, tc.kept, err)
		}
		if again := counted("two"); first != 12400 || again != 12400-tc.kept {
			t.Fatalf("counted %d, then %d by the next lease holder", first, again)
		}
	}
}

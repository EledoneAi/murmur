package hub

import (
	"fmt"
	"log/slog"
	"testing"

	"github.com/EledoneAi/murmur/v2/internal/bus"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/store"
)

// The hub's hot paths with a big team on one instance, without sockets: cmd/murmur-load measures the whole hub.
// go test ./internal/hub -run '^$' -bench . -benchmem

// benchTeam puts n laptops on a hub, one person each with two agents in twenty repos, as murmur-load does: their
// files overlap often.
func benchTeam(b *testing.B, n int) (*Hub, []*link) {
	h, err := New(b.Context(), Config{Secret: []byte("0123456789abcdef"), Seed: seed(nil), Log: slog.New(slog.DiscardHandler)},
		store.NewMemory(), bus.NewLocal())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(h.Close)
	links := make([]*link, n)
	h.mu.Lock()
	for i := range links {
		handle := fmt.Sprintf("p%d", i)
		u := &store.User{Email: handle + "@x.com", Handle: handle}
		h.users[u.Email], h.handles[u.Handle] = u, u
		var agents []protocol.Agent
		for a := range 2 {
			repo := i*7 + a*3
			files := make([]string, 6)
			for f := range files {
				files[f] = fmt.Sprintf("internal/pkg%d/file%d.go", (repo+f)%40, (i+a+f*13)%50)
			}
			agents = append(agents, protocol.Agent{Name: fmt.Sprintf("a%d", a+1), Repo: fmt.Sprintf("acme/repo%02d", repo%20),
				Branch: fmt.Sprintf("feat/load-%d-%d", i, a), Title: "Make the hub absorb many more laptops than it does today",
				Status: protocol.StatusBusy, Since: 1, Doing: "Editing internal/hub/route.go", Tree: fmt.Sprintf("%016x", i*64+a),
				Files: files, Kinds: "rrexeerrxwreeexrrrexsreermexre", Context: 40, Added: 120, Deleted: 30, Changed: len(files)})
		}
		l := newLink(handle, agents...)
		l.roster = func(...[]byte) {}
		h.links[l.dev], links[i] = l, l
	}
	h.colours = nil
	h.mu.Unlock()
	return h, links
}

// One roster for every laptop: worked out, encoded and handed to each.
func BenchmarkRoster(b *testing.B) {
	for _, n := range []int{100, 1000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			h, links := benchTeam(b, n)
			for b.Loop() {
				for _, l := range links {
					l.shown = 0
				}
				r, queue := h.buildRoster(nil)
				for _, l := range queue {
					r.hand(l)
				}
			}
		})
	}
}

// One message from an agent to another's, through the checks, limits and lookups every message goes through.
func BenchmarkSend(b *testing.B) {
	h, links := benchTeam(b, 1000)
	h.mu.Lock()
	h.settings.SendsPerMinute, h.settings.LoopMax = 1<<30, 1<<30 // past what the panel allows: only the path counts
	h.mu.Unlock()
	i := 0
	for b.Loop() {
		from, to := links[i%len(links)], links[(i+1)%len(links)]
		h.route(from, protocol.Msg{ID: fmt.Sprintf("m%d", i), From: "a1", To: to.handle + "/a2", Text: "hello"})
		for len(to.out) > 0 {
			<-to.out
		}
		for len(from.out) > 0 {
			<-from.out
		}
		i++
	}
}

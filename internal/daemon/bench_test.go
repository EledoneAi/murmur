package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/harness"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

// What the daemon pays to tell a console about a change, for a laptop with eight sessions in a team of 10, 100 or 500
// people with four agents each, and a full history: go test ./internal/daemon -run '^$' -bench . -benchmem

func benchDaemon(people int) *Daemon {
	d := newDaemon("alice")
	d.state = "online"
	for i := range 8 {
		s := &session{harness: "claude", pid: i + 1, id: fmt.Sprint("s", i), name: fmt.Sprint("api-", i), status: protocol.StatusBusy,
			repo: "acme/api", branch: fmt.Sprint("feat/topic-", i), tree: fmt.Sprint("t", i), files: []string{"billing/certifier.go"},
			act: harness.Activity{Title: "Rerun billing on staging", Doing: "Editing billing/certifier.go",
				Kinds: "rreexxrreewrrseexxrrmeexxrreex", Context: 40}}
		d.sessions[s.key()] = s
	}
	for i := range people {
		p := protocol.Person{Handle: fmt.Sprintf("p%03d", i), Online: true, Colour: i % protocol.Colours}
		if i == 0 {
			p.Handle = "alice"
		}
		for j := range 4 {
			p.Agents = append(p.Agents, protocol.Agent{Name: fmt.Sprint("svc-", j), Repo: fmt.Sprint("acme/svc-", (i+j)%20),
				Branch: fmt.Sprint("feat/topic-", i), Title: "Work on topic " + fmt.Sprint(i), Status: protocol.StatusBusy,
				Since: time.Now().UnixMilli(), Doing: "Editing billing/certifier.go", Kinds: "rreexxrreewrrseexxrrmeexxrreex"})
		}
		d.people = append(d.people, p)
	}
	text := strings.Repeat("The certifier splits shadow and live runs now; rebase after my next push. ", 3)
	for i := range keepMsgs {
		d.history = append(d.history, protocol.Msg{ID: fmt.Sprint("m", i), From: fmt.Sprintf("p%03d/svc-0", i%people),
			To: fmt.Sprintf("p%03d/svc-1", (i+1)%people), Text: text, At: time.Now().UnixMilli()})
	}
	return d
}

// One change, as each console subscribed hears it.
func BenchmarkPublish(b *testing.B) {
	for _, n := range []int{10, 100, 500} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			d := benchDaemon(n)
			enc := json.NewEncoder(io.Discard)
			b.ReportAllocs()
			for b.Loop() {
				d.mu.Lock()
				snap := d.snapshotLocked()
				d.mu.Unlock()
				_ = enc.Encode(snap)
			}
		})
	}
}

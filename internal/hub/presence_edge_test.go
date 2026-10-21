package hub

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/bus"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/store"
)

// holdBack is an instance's bus that holds back the first event hold picks until release closes, as a goroutine
// that publishes late, or a bus that reorders, would.
type holdBack struct {
	*tap
	hold    func(bus.Event) bool
	release chan struct{}
	once    sync.Once
}

func (b *holdBack) Publish(ctx context.Context, e bus.Event) error {
	first := false
	if b.hold(e) {
		b.once.Do(func() { first = true })
	}
	if first {
		<-b.release
	}
	return b.tap.Publish(ctx, e)
}

// agentsOn lists the agents an instance sees on one laptop.
func agentsOn(h *Hub, dev string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var names []string
	for x := range h.holdersOfLocked("") {
		if x.dev == dev && x.online {
			for _, a := range x.agents {
				names = append(names, a.Name)
			}
		}
	}
	return names
}

// A laptop's agents reach the other instances in the order its instance learned them, however the events travel: an
// older report that arrives late doesn't take the place of the newer one.
func TestPresenceArrivesLate(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	_ = st.PutUser(ctx, store.User{Email: "bob@x.com", Handle: "bob"})
	_ = st.PutDevice(ctx, store.Device{ID: "d-bob", Email: "bob@x.com", TokenHash: hashToken("tok-bob"), Created: time.Now()})
	f := &fanout{}
	late := &holdBack{tap: f.open(), release: make(chan struct{}), hold: func(e bus.Event) bool {
		return e.Kind == evPresence && bytes.Contains(e.Data, []byte(`"name":"b1"`))
	}}
	start := func(name string, b bus.Bus) (*Hub, *httptest.Server) {
		h, err := New(ctx, Config{Secret: []byte("0123456789abcdef"), Seed: seed(nil), Instance: name,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, st, b)
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(h.Handler())
		t.Cleanup(func() { srv.Close(); h.Close() })
		return h, srv
	}
	_, srvA := start("a", late)
	c, _ := start("c", f.open())

	bob := connect(t, srvA.URL, "tok-bob", protocol.Agent{Name: "b1", Status: protocol.StatusBusy}) // held back
	bob.send(protocol.Frame{T: protocol.FrameAgents, Agents: []protocol.Agent{{Name: "b2", Status: protocol.StatusBusy}}})
	eventually(t, "c never saw b2", func() bool { a := agentsOn(c, "d-bob"); return len(a) == 1 && a[0] == "b2" })
	close(late.release) // the hello's report, b1, arrives now
	time.Sleep(100 * time.Millisecond)
	if a := agentsOn(c, "d-bob"); len(a) != 1 || a[0] != "b2" {
		t.Fatalf("c sees %v on bob's laptop, want [b2]", a)
	}
}

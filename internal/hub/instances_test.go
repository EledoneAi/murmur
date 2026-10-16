package hub

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/bus"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/store"
)

// fanout is a bus shared by several instances in one process, as Redis or Postgres would be.
type fanout struct {
	mu   sync.Mutex
	subs []chan bus.Event
}

type tap struct {
	f  *fanout
	ch chan bus.Event
}

func (f *fanout) open() *tap {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &tap{f: f, ch: make(chan bus.Event, 4096)}
	f.subs = append(f.subs, t.ch)
	return t
}

func (t *tap) Publish(_ context.Context, e bus.Event) error {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	for _, s := range t.f.subs {
		s <- e
	}
	return nil
}

func (t *tap) Events() <-chan bus.Event { return t.ch }
func (t *tap) Close() error             { return nil }

// Two instances on one store and one bus act as one hub: each sees the other's laptops, messages and acks cross
// between them, a check on one finds the agents on the other, and removing someone on one drops their laptop on the
// other.
func TestInstances(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	for _, h := range []string{"alice", "bob"} {
		_ = st.PutUser(ctx, store.User{Email: h + "@x.com", Handle: h, Admin: h == "alice"})
		_ = st.PutDevice(ctx, store.Device{ID: "d-" + h, Email: h + "@x.com", TokenHash: hashToken("tok-" + h), Created: time.Now()})
	}
	b := &fanout{}
	start := func(name string) (*Hub, *httptest.Server) {
		h, err := New(ctx, Config{Secret: []byte("0123456789abcdef"), Seed: seed(nil), Instance: name,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, st, b.open())
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(h.Handler())
		t.Cleanup(func() { srv.Close(); h.Close() })
		return h, srv
	}
	one, srvOne := start("one")
	_, srvTwo := start("two")

	files := []string{"api/routes.go"}
	alice := connect(t, srvOne.URL, "tok-alice", protocol.Agent{Name: "a1", Status: protocol.StatusBusy, Repo: "org/app", Tree: "ta", Files: files})
	bob := connect(t, srvTwo.URL, "tok-bob", protocol.Agent{Name: "b1", Status: protocol.StatusBusy, Repo: "org/app", Tree: "tb", Files: files})

	alice.expect(func(f protocol.Frame) bool {
		i := slices.IndexFunc(f.People, func(p protocol.Person) bool { return p.Handle == "bob" })
		return f.T == protocol.FrameRoster && i >= 0 && f.People[i].Online && len(f.People[i].Agents) == 1
	})
	alice.send(protocol.Frame{T: protocol.FrameCheck, ID: "c1", Repo: "org/app", Tree: "ta", Files: files})
	if c := alice.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameChecked && f.ID == "c1" }); len(c.Overlaps) != 1 ||
		c.Overlaps[0] != "bob/b1 is also changing api/routes.go" {
		t.Fatalf("check across instances: %q", c.Overlaps)
	}

	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m1", From: "a1", To: "bob/b1", Text: "rebasing on your routes"}})
	got := bob.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && f.Msg.ID == "m1" && !f.Echo })
	if got.Msg.From != "alice/a1" {
		t.Fatalf("from %q", got.Msg.From)
	}
	bob.send(protocol.Frame{T: protocol.FrameDelivered, ID: "m1", OK: true})
	if a := alice.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameAck && f.ID == "m1" }); !a.OK {
		t.Fatalf("ack: %+v", a)
	}

	// alice, an admin on instance one, removes bob, whose laptop is on instance two
	one.mu.Lock()
	person := *one.handles["bob"]
	dev := *one.devices["d-bob"]
	one.mu.Unlock()
	person.Disabled, dev.Revoked = true, true
	if err := one.putUser(ctx, person); err != nil {
		t.Fatal(err)
	}
	if err := one.putDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	for open := true; open; {
		select {
		case _, open = <-bob.frames:
		case <-time.After(3 * time.Second):
			t.Fatal("bob's laptop stayed connected to the other instance")
		}
	}
}

// A laptop that leaves one instance for another stays listed on a third: the goodbye of the instance it left, sent
// once its grace is over, doesn't take it off. An event no hub signed is ignored.
func TestLaptopMovesBetweenInstances(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	_ = st.PutUser(ctx, store.User{Email: "bob@x.com", Handle: "bob"})
	_ = st.PutDevice(ctx, store.Device{ID: "d-bob", Email: "bob@x.com", TokenHash: hashToken("tok-bob"), Created: time.Now()})
	b := &fanout{}
	start := func(name string) (*Hub, *httptest.Server) {
		h, err := New(ctx, Config{Secret: []byte("0123456789abcdef"), Seed: seed(nil), Instance: name,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, st, b.open())
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(h.Handler())
		t.Cleanup(func() { srv.Close(); h.Close() })
		return h, srv
	}
	a, srvA := start("a")
	_, srvB := start("b")
	c, _ := start("c")
	listed := func(h *Hub) bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, x := range h.holdersLocked() {
			if x.dev == "d-bob" && x.online && len(x.agents) == 1 {
				return true
			}
		}
		return false
	}
	until := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(3 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
		}
	}
	first := connect(t, srvA.URL, "tok-bob", protocol.Agent{Name: "b1", Status: protocol.StatusBusy})
	until("c never saw bob on a", func() bool { return listed(c) })
	_ = first.c.CloseNow()
	connect(t, srvB.URL, "tok-bob", protocol.Agent{Name: "b1", Status: protocol.StatusBusy})
	until("c never saw bob move to b", func() bool { return listed(c) })
	a.mu.Lock()
	if l := a.links["d-bob"]; l != nil {
		l.gone = time.Now().Add(-2 * grace)
	}
	a.mu.Unlock()
	a.sweep(ctx, false) // a's goodbye for bob
	time.Sleep(100 * time.Millisecond)
	if !listed(c) {
		t.Fatal("a's goodbye took bob off c, though he is on b")
	}
	// an event no hub signed: a presence that would take bob offline
	_ = b.open().Publish(ctx, bus.Event{Kind: evPresence, From: "b", Data: mustJSON(presence{Device: "d-bob", Email: "bob@x.com", Handle: "bob"})})
	time.Sleep(100 * time.Millisecond)
	if !listed(c) {
		t.Fatal("an unsigned event changed what c lists")
	}
}

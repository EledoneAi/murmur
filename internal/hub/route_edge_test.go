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

// acks collects the hub's answers about one message that reach a laptop within d. Other frames stay for later.
func (l *laptop) acks(id string, d time.Duration) []protocol.Frame {
	l.t.Helper()
	var got, rest []protocol.Frame
	for _, f := range l.backlog {
		if f.T == protocol.FrameAck && f.ID == id {
			got = append(got, f)
		} else {
			rest = append(rest, f)
		}
	}
	l.backlog = rest
	deadline := time.After(d)
	for {
		select {
		case f, open := <-l.frames:
			if !open {
				return got
			}
			if f.T == protocol.FrameAck && f.ID == id {
				got = append(got, f)
			} else {
				l.backlog = append(l.backlog, f)
			}
		case <-deadline:
			return got
		}
	}
}

// edgeTeam is a hub where alice, bob, carol and dan are on the team and erin was removed. Everyone but dan has a
// laptop.
func edgeTeam(t *testing.T, st store.Store) (*Hub, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	for _, u := range []store.User{{Email: "alice@x.com", Handle: "alice"}, {Email: "bob@x.com", Handle: "bob"},
		{Email: "carol@x.com", Handle: "carol"}, {Email: "dan@x.com", Handle: "dan"}, {Email: "erin@x.com", Handle: "erin", Disabled: true}} {
		_ = st.PutUser(ctx, u)
		if u.Handle != "dan" {
			_ = st.PutDevice(ctx, store.Device{ID: "d-" + u.Handle, Email: u.Email, TokenHash: hashToken("tok-" + u.Handle), Created: time.Now()})
		}
	}
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef")}, st)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	return h, srv
}

// Every message gets one answer, whoever it is for: refused at once, kept with a note, or delivered and answered by
// the laptop that took it. A message for an agent whose laptop is reconnecting is the one that waits for its answer.
func TestEveryAddressGetsOneAnswer(t *testing.T) {
	_, srv := edgeTeam(t, store.NewMemory())
	alice := connect(t, srv.URL, "tok-alice", protocol.Agent{Name: "a1", Status: protocol.StatusBusy, Repo: "org/app"})
	bob := connect(t, srv.URL, "tok-bob", protocol.Agent{Name: "b1", Status: protocol.StatusIdle})
	_ = bob.c.CloseNow()
	time.Sleep(100 * time.Millisecond) // bob's laptop is reconnecting

	const (
		refused   = "refused"
		kept      = "kept"
		delivered = "delivered" // to alice's own laptop, which answers like any other
		held      = "held"
	)
	for i, c := range []struct{ to, want, says string }{
		{"carol", kept, "carol is offline"},
		{"dan", kept, "dan is offline"}, // no laptop yet: kept for the first one
		{"bob", kept, "bob is offline"}, // his only laptop is reconnecting
		{"bob/b1", held, ""},
		{"Bob / b1 ", held, ""},
		{"carol/c1", refused, "carol/c1 is not running"},
		{"bob/B1", refused, "is not running"},
		{"bob/b1/x", refused, "is not running"},
		{"bob/" + strings.Repeat("b", 81), refused, "is not running"},
		{"nobody", refused, `nobody called "nobody"`},
		{"erin", refused, `nobody called "erin"`},
		{protocol.System, refused, `nobody called "murmur"`},
		{"murmur/x", refused, `nobody called "murmur"`},
		{"", refused, `nobody called ""`},
		{" ", refused, `nobody called ""`},
		{"/", refused, `nobody called ""`},
		{"/a1", refused, `nobody called ""`},
		{"bob\x00/b1", refused, "nobody called"},
		{"аlice", refused, "nobody called"}, // a Cyrillic a
		{"alice\u200b", refused, "nobody called"},
		{"repo:", refused, `nobody works in ""`},
		{"repo:other/x", refused, `nobody works in "other/x"`},
		{"repo:org/app", kept, "reached 0 agents in org/app"}, // an announcement: only the sender works there
		{"alice", delivered, ""},
		{"alice/a1", delivered, ""},
		{"alice/", delivered, ""},
	} {
		id := fmt.Sprint("m", i)
		alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: id, From: "a1", To: c.to, Text: "hi"}})
		if c.want == delivered {
			alice.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && f.Msg.ID == id && !f.Echo })
			alice.send(protocol.Frame{T: protocol.FrameDelivered, ID: id, OK: true})
		}
		got := alice.acks(id, 100*time.Millisecond)
		switch {
		case c.want == held && len(got) != 0:
			t.Errorf("%q: answered %+v while the message waits for its laptop", c.to, got)
		case c.want != held && len(got) != 1:
			t.Errorf("%q: %d answers, want 1: %+v", c.to, len(got), got)
		case c.want == refused && (got[0].OK || !strings.Contains(got[0].Err, c.says)):
			t.Errorf("%q: %+v, want refused with %q", c.to, got[0], c.says)
		case c.want == kept && (!got[0].OK || !strings.Contains(got[0].Note, c.says)):
			t.Errorf("%q: %+v, want kept with %q", c.to, got[0], c.says)
		case c.want == delivered && (!got[0].OK || got[0].Note != "delivered"):
			t.Errorf("%q: %+v, want delivered", c.to, got[0])
		}
	}

	// the message held for b1 reaches it when bob's laptop is back, and alice hears once
	bob = connect(t, srv.URL, "tok-bob", protocol.Agent{Name: "b1", Status: protocol.StatusIdle})
	for _, id := range []string{"m3", "m4"} {
		bob.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && f.Msg.ID == id && !f.Echo })
		bob.send(protocol.Frame{T: protocol.FrameDelivered, ID: id, OK: true})
		if got := alice.acks(id, 200*time.Millisecond); len(got) != 1 || !got[0].OK {
			t.Errorf("%s: answers %+v once bob's laptop is back", id, got)
		}
	}
}

// The limit on a message counts its characters, as the console's box does, not the bytes they take.
func TestMessageLimitCountsCharacters(t *testing.T) {
	_, srv := edgeTeam(t, store.NewMemory())
	alice := connect(t, srv.URL, "tok-alice", protocol.Agent{Name: "a1", Status: protocol.StatusBusy})
	bob := connect(t, srv.URL, "tok-bob", protocol.Agent{Name: "b1", Status: protocol.StatusIdle})
	for id, text := range map[string]string{"fr": strings.Repeat("é", protocol.MaxText), "ja": strings.Repeat("葛", protocol.MaxText)} {
		alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: id, From: "a1", To: "bob/b1", Text: text}})
		if a := alice.acks(id, 200*time.Millisecond); len(a) != 0 {
			t.Fatalf("%d characters refused: %+v", protocol.MaxText, a)
		}
		bob.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && f.Msg.ID == id && !f.Echo })
	}
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "long", From: "a1", To: "bob/b1",
		Text: strings.Repeat("é", protocol.MaxText+1)}})
	if a := alice.acks("long", time.Second); len(a) != 1 || a[0].OK || !strings.Contains(a[0].Err, fmt.Sprintf("%d characters", protocol.MaxText+1)) {
		t.Fatalf("one character too many: %+v", a)
	}
}

// A laptop revoked while it is connected is let go at once, and its agents stop taking messages: they are refused
// right away rather than held for a laptop that can't come back.
func TestRevokedLaptopTakesNoMessages(t *testing.T) {
	st := store.NewMemory()
	h, srv := edgeTeam(t, st)
	alice := connect(t, srv.URL, "tok-alice", protocol.Agent{Name: "a1", Status: protocol.StatusBusy})
	bob := connect(t, srv.URL, "tok-bob", protocol.Agent{Name: "b1", Status: protocol.StatusIdle})
	h.mu.Lock()
	d := *h.devices["d-bob"]
	h.mu.Unlock()
	d.Revoked = true
	if err := h.putDevice(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	for open := true; open; {
		select {
		case _, open = <-bob.frames:
		case <-time.After(3 * time.Second):
			t.Fatal("the revoked laptop stayed connected")
		}
	}
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m1", From: "a1", To: "bob/b1", Text: "hi"}})
	if a := alice.acks("m1", time.Second); len(a) != 1 || a[0].OK || !strings.Contains(a[0].Err, "not running") {
		t.Fatalf("a message for an agent on a revoked laptop: %+v", a)
	}
	if p, _ := st.Pending(context.Background(), "bob@x.com"); len(p) != 0 {
		t.Fatalf("kept for the revoked laptop: %+v", p)
	}
}

// awayPastGrace drops bob's laptop and has the hub stop listing it, as after its grace.
func awayPastGrace(t *testing.T, h *Hub, bob *laptop) {
	t.Helper()
	_ = bob.c.CloseNow()
	eventually(t, "bob's laptop never dropped", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.links["d-bob"] != nil && !h.links["d-bob"].gone.IsZero()
	})
}

func forget(h *Hub, dev string) {
	h.mu.Lock()
	h.links[dev].gone = time.Now().Add(-2 * grace)
	h.mu.Unlock()
	h.sweep(context.Background(), false)
}

// A message held for an agent whose laptop is reconnecting has one answer, even when its sender sends it again from
// its outbox after the hub stopped listing that laptop: it is on its way, not refused, and the sender hears once it
// is delivered.
func TestHeldMessageSentAgain(t *testing.T) {
	h, srv := edgeTeam(t, store.NewMemory())
	alice := connect(t, srv.URL, "tok-alice", protocol.Agent{Name: "a1", Status: protocol.StatusBusy})
	bob := connect(t, srv.URL, "tok-bob", protocol.Agent{Name: "b1", Status: protocol.StatusIdle})
	awayPastGrace(t, h, bob)
	m := protocol.Msg{ID: "m1", From: "a1", To: "bob/b1", Text: "still there?"}
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &m})
	if a := alice.acks("m1", 200*time.Millisecond); len(a) != 0 {
		t.Fatalf("answered while held: %+v", a)
	}
	forget(h, "d-bob")

	alice = connect(t, srv.URL, "tok-alice", protocol.Agent{Name: "a1", Status: protocol.StatusBusy})
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &m}) // from its outbox, as alice reconnects
	if a := alice.acks("m1", 300*time.Millisecond); len(a) != 0 {
		t.Fatalf("the held message, sent again, was answered %+v", a)
	}
	bob = connect(t, srv.URL, "tok-bob", protocol.Agent{Name: "b1", Status: protocol.StatusIdle})
	bob.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && f.Msg.ID == "m1" && !f.Echo })
	bob.send(protocol.Frame{T: protocol.FrameDelivered, ID: "m1", OK: true})
	if a := alice.acks("m1", 300*time.Millisecond); len(a) != 1 || !a[0].OK {
		t.Fatalf("answers once bob's laptop is back: %+v", a)
	}

	// another laptop's message under that id is not the held one: it is refused
	carol := connect(t, srv.URL, "tok-carol", protocol.Agent{Name: "c1", Status: protocol.StatusBusy})
	awayPastGrace(t, h, bob)
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m2", From: "a1", To: "bob/b1", Text: "again?"}})
	time.Sleep(50 * time.Millisecond)
	forget(h, "d-bob")
	carol.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m2", From: "c1", To: "bob/b1", Text: "same id"}})
	if a := carol.acks("m2", time.Second); len(a) != 1 || a[0].OK || !strings.Contains(a[0].Err, "not running") {
		t.Fatalf("carol's message under alice's held id: %+v", a)
	}
}

// The limits count one recipient however its address is written, and fresh ids don't reset them.
func TestLimitsHoldAcrossAddressForms(t *testing.T) {
	_, srv := edgeTeam(t, store.NewMemory())
	alice := connect(t, srv.URL, "tok-alice", protocol.Agent{Name: "a1", Status: protocol.StatusBusy})
	bob := connect(t, srv.URL, "tok-bob", protocol.Agent{Name: "b1", Status: protocol.StatusIdle})
	forms := []string{"bob/b1", "Bob/b1", " bob / b1 ", "BOB/b1\t"}
	for i := range Defaults().LoopMax {
		id := fmt.Sprint("loop", i)
		alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: id, From: "a1", To: forms[i%len(forms)], Text: "again"}})
		bob.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && f.Msg.ID == id && !f.Echo })
		bob.send(protocol.Frame{T: protocol.FrameDelivered, ID: id, OK: true})
	}
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "one-more", From: "a1", To: "BoB / b1", Text: "again"}})
	if a := alice.acks("one-more", 300*time.Millisecond); len(a) != 1 || a[0].OK || !strings.Contains(a[0].Err, "paused") {
		t.Fatalf("past the loop limit: %+v", a)
	}

	// alice herself, to bob written several ways: the minute's limit lets the first ones through
	n := Defaults().SendsPerMinute
	for i := range n + 1 {
		alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: fmt.Sprint("p", i), To: forms[i%2][:3] + strings.Repeat(" ", i%3), Text: "hi"}})
	}
	for i := range n {
		bob.expect(func(f protocol.Frame) bool {
			return f.T == protocol.FrameMsg && f.Msg.ID == fmt.Sprint("p", i) && !f.Echo
		})
	}
	bob.quiet(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && f.Msg.ID == fmt.Sprint("p", n) })
}

// A laptop that reconnects sends what its outbox holds at once, up to 50 messages. Those past the minute's limit hear
// it, which a laptop's outbox takes as a reason to try again later, and reach their recipient then.
func TestOutboxBurstHearsTheLimit(t *testing.T) {
	h, srv := edgeTeam(t, store.NewMemory())
	bob := connect(t, srv.URL, "tok-bob", protocol.Agent{Name: "b1", Status: protocol.StatusIdle})
	alice := connect(t, srv.URL, "tok-alice", protocol.Agent{Name: "a1", Status: protocol.StatusBusy})
	const burst = 30
	resend := func(from int) {
		for i := from; i < burst; i++ {
			alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: fmt.Sprint("o", i), To: "bob", Text: fmt.Sprint("written offline ", i)}})
		}
	}
	resend(0)
	n := Defaults().SendsPerMinute
	for i := range n {
		bob.expect(func(f protocol.Frame) bool {
			return f.T == protocol.FrameMsg && f.Msg.ID == fmt.Sprint("o", i) && !f.Echo
		})
	}
	for i := n; i < burst; i++ {
		if a := alice.acks(fmt.Sprint("o", i), time.Second); len(a) != 1 || a[0].Err != protocol.TooMany {
			t.Fatalf("o%d past the limit: %+v", i, a)
		}
	}
	h.mu.Lock()
	clear(h.sends) // a minute later
	h.mu.Unlock()
	resend(n)
	for i := n; i < burst; i++ {
		bob.expect(func(f protocol.Frame) bool {
			return f.T == protocol.FrameMsg && f.Msg.ID == fmt.Sprint("o", i) && !f.Echo
		})
	}
}

// Two of bob's laptops run an agent of one name: the roster lists it once, as the one messages to that name reach,
// the one waiting for bob, and every message goes there.
func TestOneAgentNameOnTwoLaptops(t *testing.T) {
	st := store.NewMemory()
	_ = st.PutDevice(context.Background(), store.Device{ID: "d-bob2", Email: "bob@x.com", TokenHash: hashToken("tok-bob2"), Created: time.Now()})
	_, srv := edgeTeam(t, st)
	alice := connect(t, srv.URL, "tok-alice", protocol.Agent{Name: "a1", Status: protocol.StatusBusy})
	idle := connect(t, srv.URL, "tok-bob", protocol.Agent{Name: "main-1", Status: protocol.StatusIdle, Repo: "acme/web", Since: 2000})
	waits := connect(t, srv.URL, "tok-bob2", protocol.Agent{Name: "main-1", Status: protocol.StatusWaiting, Repo: "acme/api", Since: 1000})
	r := alice.expect(func(f protocol.Frame) bool {
		return f.T == protocol.FrameRoster && len(f.People) > 1 && f.People[1].Handle == "bob" && f.People[1].Online &&
			slices.ContainsFunc(f.People[1].Agents, func(a protocol.Agent) bool { return a.Repo == "acme/api" })
	})
	if n := len(r.People[1].Agents); n != 1 {
		t.Fatalf("bob's main-1 listed %d times: %+v", n, r.People[1].Agents)
	}
	for i := range 10 {
		id := fmt.Sprint("m", i)
		alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: id, From: "a1", To: "bob/main-1", Text: "which one?"}})
		waits.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && f.Msg.ID == id && !f.Echo })
	}
	idle.quiet(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && !f.Echo })
}

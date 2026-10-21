package daemon

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/harness"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

// Edge cases: answers that come as a send stops waiting, connections that drop mid-send, requests no client should
// make, history files a crash left behind, and text that tries to pass for something else.

// eventually waits up to two seconds for ok.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// The hub's answer that arrives just as its sender stops waiting still settles the message: it no longer shows as
// sending, and a refusal reaches the console.
func TestAnswerAsTheSendStopsWaiting(t *testing.T) {
	d := newDaemon("alice")
	d.wait = 50 * time.Millisecond
	d.out, d.state = make(chan protocol.Frame, 8), "online"
	replied := make(chan ipc.Reply, 1)
	go func() { replied <- d.send(ipc.Request{To: "bob", Text: "lunch?"}) }()
	f := <-d.out
	// the answer is read while the send still waits, and handed over once it has stopped
	d.mu.Lock()
	go d.onFrame(protocol.Frame{T: protocol.FrameAck, ID: f.Msg.ID, Err: `nobody called "bob" is on murmur`})
	time.Sleep(4 * d.wait)
	d.mu.Unlock()
	if r := <-replied; !r.OK {
		t.Fatalf("send: %+v", r)
	}
	eventually(t, "the refusal to settle the message", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		_, sending := d.outbox[f.Msg.ID]
		return !sending && d.failed[f.Msg.ID].Why != ""
	})
}

// A send that waited across a reconnection goes on the new connection once it stops waiting: the reconnection's own
// resend leaves a message to the send that waits for it, and the first copy went with the old connection.
func TestSendWaitingAcrossAReconnect(t *testing.T) {
	d := newDaemon("alice")
	d.wait = 50 * time.Millisecond
	old := make(chan protocol.Frame, 8)
	d.out, d.state = old, "online"
	replied := make(chan ipc.Reply, 1)
	go func() { replied <- d.send(ipc.Request{To: "bob", Text: "lunch?"}) }()
	lost := <-old
	fresh := make(chan protocol.Frame, 8)
	d.mu.Lock() // as connect does
	d.out = fresh
	d.mu.Unlock()
	d.resend(fresh, 0)
	<-replied
	select {
	case f := <-fresh:
		if f.T != protocol.FrameSend || f.Msg.ID != lost.Msg.ID {
			t.Fatalf("the new connection got %+v", f)
		}
	case <-time.After(time.Second):
		t.Fatal("the message waits for the next resend, minutes away")
	}
}

// A message over the limit is refused on the laptop. The hub refuses it too, and one over a megabyte makes it drop the
// connection that carries it, which the outbox would send it on again at every reconnection, for a day.
func TestTooLongMessage(t *testing.T) {
	d := newDaemon("alice")
	d.state = "reconnecting"
	if r := d.send(ipc.Request{To: "bob", Text: strings.Repeat("log line\n", 200_000)}); r.OK || !strings.Contains(r.Err, "the limit is") {
		t.Fatalf("1.8 MB: %+v", r)
	}
	if r := d.send(ipc.Request{To: "bob", Text: strings.Repeat("x", protocol.MaxText+1)}); r.OK {
		t.Fatalf("just over the limit: %+v", r)
	}
	if r := d.send(ipc.Request{To: "bob/" + strings.Repeat("b", 2<<20), Text: "hi"}); r.OK {
		t.Fatalf("to a 2 MB address: %+v", r)
	}
	if r := d.send(ipc.Request{To: "bob", Text: strings.Repeat("x", protocol.MaxText) + "\x1b[2J  \n"}); !r.OK {
		t.Fatalf("at the limit, once cleaned as the hub cleans it: %+v", r)
	}
	if r := d.send(ipc.Request{To: "bob", Text: strings.Repeat("é", protocol.MaxText)}); !r.OK { // characters, as the hub counts
		t.Fatalf("at the limit in accented letters: %+v", r)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.outbox) != 2 {
		t.Fatalf("%d messages in the outbox, want 2", len(d.outbox))
	}
}

// What an agent of this laptop sends shows in the console while it sends, and after it failed, as the hub would show
// it: the model wrote it, so terminal escapes are gone. One made only of them is empty.
func TestOwnMessagesAreCleaned(t *testing.T) {
	d := newDaemon("alice")
	d.state = "reconnecting"
	if r := d.send(ipc.Request{To: "bob\x1b[2J", Text: "\x1b]52;c;cm0gLXJmIH4=\x07lunch?\x1b[8m"}); !r.OK {
		t.Fatalf("send: %+v", r)
	}
	if r := d.send(ipc.Request{To: "bob", Text: "\x1b[2J\x1b]0;title\x07"}); r.OK {
		t.Fatalf("a message of escapes only: %+v", r)
	}
	d.mu.Lock()
	snap := d.snapshotLocked()
	d.mu.Unlock()
	if len(snap.Outbox) != 1 || snap.Outbox[0].Text != "lunch?" || snap.Outbox[0].To != "bob" {
		t.Fatalf("the console shows %+v", snap.Outbox)
	}
	d.onFrame(protocol.Frame{T: protocol.FrameAck, ID: snap.Outbox[0].ID, Err: "refused"})
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, m := range d.history {
		if strings.ContainsRune(m.Text, 0x1b) {
			t.Fatalf("the console shows %q", m.Text)
		}
	}
}

// A session that has no name yet isn't on the team, and doesn't send as its person.
func TestUnnamedSessionDoesNotSendAsItsPerson(t *testing.T) {
	d := newDaemon("alice")
	d.state = "reconnecting"
	d.add(&session{id: "c1", pid: 10, status: protocol.StatusBusy})
	if r := d.send(ipc.Request{Harness: "claude", PID: 10, To: "bob", Text: "approved, go ahead"}); r.OK {
		t.Fatalf("send: %+v", r)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.outbox) != 0 {
		t.Fatalf("outbox %+v", d.outbox)
	}
}

// A history whose last line a crash cut short keeps what is written after it.
func TestHistoryAfterACutLine(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	path := filepath.Join(config.Dir(), "history.jsonl")
	line, _ := json.Marshal(histLine{M: &protocol.Msg{ID: "m1", From: "bob", To: "alice", Text: "hi", At: 1}})
	if err := os.WriteFile(path, append(append(line, '\n'), `{"done":"m`...), 0o600); err != nil {
		t.Fatal(err)
	}
	d := newDaemon("alice")
	d.loadHistory()
	if _, err := d.mute("bob", ""); err != nil {
		t.Fatal(err)
	}
	d.hist.Close()
	again := newDaemon("alice")
	again.loadHistory()
	defer again.hist.Close()
	if len(again.history) != 1 || len(again.mutes) != 1 {
		t.Fatalf("after a restart: %d messages, mutes %+v", len(again.history), again.mutes)
	}
}

// A request the daemon doesn't know gets an answer that says so, not a closed connection.
func TestUnknownRequest(t *testing.T) {
	d := newDaemon("alice")
	c, them := net.Pipe()
	defer them.Close()
	go d.serve(c)
	_ = them.SetDeadline(time.Now().Add(2 * time.Second))
	var r ipc.Reply
	_ = json.NewEncoder(them).Encode(ipc.Request{Op: "frobnicate"})
	if err := json.NewDecoder(them).Decode(&r); err != nil || r.OK || r.Err == "" {
		t.Fatalf("reply %+v, %v", r, err)
	}
}

// Why the hub refuses this laptop reaches the console and `murmur status` cleaned. (The team's version is offered
// only when update.Newer finds it shaped like a version, which no escape is.) Swapping release.Version here would
// race with what earlier tests leave running.
func TestHubTextIsCleaned(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d := newDaemon("alice")
	d.refused(config.Config{}, "\x1b[2Jthis team needs murmur 9.9.9 or later: run murmur update")
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state != "refused by the hub: this team needs murmur 9.9.9 or later: run murmur update" {
		t.Fatalf("state %q", d.state)
	}
}

// One message too big for the hub doesn't keep a laptop off it, after the next reconnection either: the next message
// gets through.
func TestE2ETooBigForTheHub(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	inb := newInbox(t)
	alice := tm.laptop("alice", agentSession("a1", "conv-a", newInbox(t)))
	alice.mu.Lock()
	alice.wait = 100 * time.Millisecond
	alice.mu.Unlock()
	tm.laptop("bob", agentSession("b1", "conv-b", inb))
	tm.until("alice sees bob/b1", func() bool { return strings.Contains(whoOf(alice), "bob/b1") })
	pid := alice.pidOfAgent("a1")
	alice.send(ipc.Request{PID: pid, To: "bob/b1", Text: strings.Repeat("log line\n", 200_000)})
	tm.restart()
	time.Sleep(200 * time.Millisecond)
	alice.send(ipc.Request{PID: pid, To: "bob/b1", Text: "the log is in /tmp/build.log"})
	inb.expect(t, "the log is in /tmp/build.log")
}

// Unmuting a group lifts the mutes muting it set, though the agents in touch with it have changed since: a day later
// the messages that linked them are old.
func TestUnmuteAGroupADayLater(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d := newDaemon("alice")
	d.loadHistory()
	defer d.hist.Close()
	d.record(protocol.Msg{ID: "g1", From: "bob/b2", To: "carol/c1", Text: "hi", At: time.Now().UnixMilli()})
	if _, err := d.mute("group:bob/b2", ""); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.history[0].At = time.Now().Add(-25 * time.Hour).UnixMilli()
	d.mu.Unlock()
	if _, err := d.unmute("group:bob/b2"); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if left := d.muteListLocked(); len(left) != 0 {
		t.Fatalf("still muted: %v", left)
	}
}

// A hub's address reads in any case, as MURMUR_HUB may give it.
func TestHubAddressInAnyCase(t *testing.T) {
	for in, want := range map[string]string{"HTTPS://hub.example": "wss://hub.example/ws", "Http://127.0.0.1:7777": "ws://127.0.0.1:7777/ws",
		"https://hub.example": "wss://hub.example/ws", "hub.example": "ws://hub.example/ws"} {
		if got := wsURL(in); got != want {
			t.Errorf("wsURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// A hook whose walk up the processes ended at init names no process: it makes no session, which nothing would remove.
func TestHookWithoutAProcess(t *testing.T) {
	d := newDaemon("alice")
	for _, pid := range []int{0, 1} {
		d.onHook(harness.Event{Kind: harness.Start, Harness: "codex", SessionID: "t1", PID: pid, Cwd: t.TempDir()})
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.sessions) != 0 {
		t.Fatalf("sessions %v", d.sessions)
	}
}

// A note murmur held for an idle agent whose session then ended doesn't reach the next session that takes its name.
func TestHeldNoteStaysWithItsSession(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d := newDaemon("alice")
	d.loadHistory()
	defer d.hist.Close()
	cwd := t.TempDir()
	hook := func(kind, id string) string {
		text, token := d.onHook(harness.Event{Kind: kind, Harness: "codex", SessionID: id, PID: os.Getpid(), Cwd: cwd, Carries: true})
		if token != "" {
			d.settle(token, true)
		}
		return text
	}
	hook(harness.Start, "t1")
	d.handOver(protocol.Msg{ID: "n1", From: protocol.System, To: "alice/codex-1", Text: "not delivered to bob/b1: refused"}, "codex-1")
	hook(harness.End, "t1")
	hook(harness.Start, "t2") // codex-1 again
	got := hook(harness.Prompt, "t2")
	time.Sleep(100 * time.Millisecond) // a session that works again gets its notes in the background
	if got += hook(harness.AfterTool, "t2"); strings.Contains(got, "not delivered") {
		t.Fatalf("the new codex-1 got %q", got)
	}
}

// Muting a teammate's group leaves out this person's own agents in touch with it: muting one of yours holds
// everything that reaches it, from anyone. Naming your own agent's group still mutes that agent.
func TestGroupMuteLeavesYourAgentsOut(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	d := newDaemon("alice")
	d.record(protocol.Msg{ID: "g1", From: "bob/b2", To: "alice/a1", Text: "hi", At: time.Now().UnixMilli()})
	d.record(protocol.Msg{ID: "g2", From: "alice/a1", To: "carol/c1", Text: "hi", At: time.Now().UnixMilli()})
	said, err := d.mute("group:bob/b2", "1h")
	if err != nil || strings.Contains(said, "alice/a1") || !strings.Contains(said, "bob/b2") {
		t.Fatalf("muting bob's group: %q %v", said, err)
	}
	if said, err := d.mute("group:alice/a1", "1h"); err != nil || !strings.Contains(said, "alice/a1") {
		t.Fatalf("muting your own agent's group: %q %v", said, err)
	}
}

// A resent message the hub turns back for the minute's limit, as a burst on reconnecting can be, stays in the outbox
// and goes again later; a fresh one the limit refuses fails, so its agent hears to group its messages.
func TestResendPastTheLimitWaits(t *testing.T) {
	d := newDaemon("alice")
	d.outbox["m1"] = outMsg{Msg: protocol.Msg{ID: "m1", To: "bob", Text: "hi"}, At: time.Now()}
	d.onFrame(protocol.Frame{T: protocol.FrameAck, ID: "m1", Err: protocol.TooMany})
	d.mu.Lock()
	_, kept := d.outbox["m1"]
	failed := len(d.failed)
	d.mu.Unlock()
	if !kept || failed != 0 {
		t.Fatalf("past the limit: kept %v, %d failed", kept, failed)
	}
	d.onFrame(protocol.Frame{T: protocol.FrameAck, ID: "m1", OK: true})
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.outbox) != 0 {
		t.Fatal("delivered later, and still in the outbox")
	}
}

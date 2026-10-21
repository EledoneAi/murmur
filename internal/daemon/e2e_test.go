package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/bus"
	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/gitscan"
	"github.com/EledoneAi/murmur/v2/internal/harness"
	"github.com/EledoneAi/murmur/v2/internal/harness/claude"
	"github.com/EledoneAi/murmur/v2/internal/hub"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/setup"
	"github.com/EledoneAi/murmur/v2/internal/store"
)

// End-to-end tests: a real hub (in memory) and real daemons talking to it over WebSockets, as laptops do, with fake
// Claude Code sessions (an inbox socket each). They cover what unit tests can't: reconnects and hub restarts.

type team struct {
	t      *testing.T
	ctx    context.Context
	store  *store.Memory
	cur    atomic.Pointer[hub.Hub]
	srv    *httptest.Server
	paused atomic.Bool
}

// newTeam starts a hub that knows these people, each with one laptop whose token is "tok-<handle>".
func newTeam(t *testing.T, handles ...string) *team {
	t.Helper()
	t.Setenv("MURMUR_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	tm := &team{t: t, ctx: ctx, store: store.NewMemory()}
	for _, h := range handles {
		sum := sha256.Sum256([]byte("tok-" + h))
		_ = tm.store.PutUser(ctx, store.User{Email: h + "@x.com", Handle: h})
		_ = tm.store.PutDevice(ctx, store.Device{ID: "d-" + h, Email: h + "@x.com", TokenHash: hex.EncodeToString(sum[:]), Created: time.Now()})
	}
	tm.start()
	tm.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tm.paused.Load() {
			http.Error(w, "the hub is down", http.StatusServiceUnavailable)
			return
		}
		tm.cur.Load().Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		cancel()
		tm.srv.CloseClientConnections()
		tm.srv.Close()
	})
	return tm
}

func (tm *team) start() {
	seed := hub.Defaults()
	seed.AllowedDomains = []string{"x.com"}
	seed.IgnoredPaths = []string{"traces/**"} // the team keeps its data folder out of overlaps
	seed.Agreements = "Keep pull requests small."
	h, err := hub.New(tm.ctx, hub.Config{Secret: []byte("0123456789abcdef"), Seed: seed, Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		tm.store, bus.NewLocal())
	if err != nil {
		tm.t.Fatal(err)
	}
	tm.cur.Store(h)
}

// restart replaces the hub with a fresh one on the same address and store, as a redeploy does: everything it held in
// memory is gone, and every laptop reconnects.
func (tm *team) restart() {
	old := tm.cur.Load()
	tm.start()
	old.Close()
}

// pause takes the hub down: every laptop loses its connection and can't get a new one until resume.
func (tm *team) pause() {
	tm.paused.Store(true)
	tm.cur.Load().Close()
}

func (tm *team) resume() { tm.paused.Store(false) }

// laptop runs a real daemon for this person with these sessions, reconnecting whenever its connection drops, and
// returns once it is online.
func (tm *team) laptop(handle string, sessions ...*session) *Daemon {
	tm.t.Helper()
	d := newDaemon(handle)
	for i, s := range sessions {
		s.pid = 1000*len(handle) + i + 1
		d.add(s)
	}
	go d.publishLoop()
	go func() {
		for tm.ctx.Err() == nil { // as hubLoop does, without its backoff
			_ = d.connect(config.Config{Hub: tm.srv.URL, Token: "tok-" + handle, Handle: handle})
			d.setState("reconnecting")
			time.Sleep(20 * time.Millisecond)
		}
	}()
	tm.until("laptop "+handle+" online", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.state == "online"
	})
	return d
}

// until waits up to five seconds for ok.
func (tm *team) until(what string, ok func() bool) {
	tm.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			tm.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// inbox is a fake Claude Code session inbox: every message handed to the session arrives on the channel.
type inbox struct {
	sock string
	got  chan string
}

func newInbox(t *testing.T) *inbox {
	t.Helper()
	dir, err := os.MkdirTemp("", "mm") // short: unix socket paths are capped near 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	in := &inbox{sock: filepath.Join(dir, "in"), got: make(chan string, 64)}
	ln, err := net.Listen("unix", in.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadString('\n')
			c.Close()
			in.got <- line
		}
	}()
	return in
}

// expect waits for one message containing all of want, and fails on anything else first.
func (in *inbox) expect(t *testing.T, want ...string) {
	t.Helper()
	select {
	case line := <-in.got:
		for _, w := range want {
			if !strings.Contains(line, w) {
				t.Fatalf("inbox got %.200q, want it to contain %q", line, w)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("inbox got nothing, want %q", want)
	}
}

// quiet fails if anything arrives within d.
func (in *inbox) quiet(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case line := <-in.got:
		t.Fatalf("inbox got %.200q, want nothing", line)
	case <-time.After(d):
	}
}

// agentSession is a session in its own checkout of Org/App, at /work/<name>, changing these files.
func agentSession(name, id string, in *inbox, files ...string) *session {
	return &session{id: id, name: name, sock: in.sock, status: "busy", top: "/work/" + name, repo: "Org/App", tree: "tree-" + name,
		files: files}
}

// edit runs the edit hook of session id as Claude Code would before its agent changes path, and returns the reason
// the edit is turned back, or "" when it goes ahead.
func (d *Daemon) edit(id, path string) string {
	data, _ := json.Marshal(map[string]any{"session_id": id, "cwd": filepath.Dir(path), "hook_event_name": "PreToolUse", "tool_name": "Edit",
		"tool_input": map[string]string{"file_path": path, "old_string": "a", "new_string": "b"}})
	ev, _ := claude.Claude{}.Parse("edit", data)
	why, _ := d.onHook(ev)
	return why
}

// Two people change the same file: their consoles show the overlap, as a pair and as the line the console showed
// before pairs, and nothing is pushed to their agents, after a hub restart either.
func TestE2EOverlapsReachTheConsoleOnly(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	ina, inb := newInbox(t), newInbox(t)
	alice := tm.laptop("alice", agentSession("a1", "conv-a", ina, "x.go"))
	tm.laptop("bob", agentSession("b1", "conv-b", inb, "x.go", "y.go"))
	shown := func() bool {
		alice.mu.Lock()
		defer alice.mu.Unlock()
		s := alice.snapshotLocked()
		return len(s.Pairs) == 1 && s.Pairs[0].Mine == "a1" && s.Pairs[0].Other == "bob/b1" && strings.Join(s.Pairs[0].Files, ",") == "x.go"
	}
	tm.until("alice's console shows the overlap", shown)
	tm.restart()
	time.Sleep(300 * time.Millisecond) // laptops reconnect to the new hub
	tm.until("alice's console shows the overlap again", shown)
	ina.quiet(t, 0)
	inb.quiet(t, 0)
}

// Before an agent edits a file that another checkout of its repo changes, the hub names the other agent and the edit
// is turned back once; the next attempt goes ahead. Files the team ignores, files nobody else changes and files
// outside the checkout go ahead at once.
func TestE2EEditCheck(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	ina, inb := newInbox(t), newInbox(t)
	alice := tm.laptop("alice", agentSession("a1", "conv-a", ina, "api/routes.go", "traces/run.jsonl"))
	b1 := agentSession("b1", "conv-b", inb, "api/routes.go", "traces/run.jsonl")
	b1.branch = "feat/x"
	bob := tm.laptop("bob", b1)

	why := alice.edit("conv-a", "/work/a1/api/routes.go")
	if !strings.Contains(why, `"bob/b1 (feat/x)" is also changing "api/routes.go"`) || !strings.Contains(why, "send") {
		t.Fatalf("first edit: %q", why)
	}
	if why := alice.edit("conv-a", "/work/a1/api/routes.go"); why != "" {
		t.Fatalf("turned back twice: %q", why)
	}
	for _, path := range []string{"/work/a1/traces/run.jsonl", "/work/a1/api/new.go", "/work/elsewhere/api/routes.go"} {
		if why := alice.edit("conv-a", path); why != "" {
			t.Fatalf("%s: %q", path, why)
		}
	}
	if why := bob.edit("conv-b", "/work/b1/api/routes.go"); !strings.Contains(why, `"alice/a1" is also changing "api/routes.go"`) {
		t.Fatalf("bob's first edit: %q", why)
	}
	tm.pause() // no hub: the edit goes ahead
	tm.until("alice offline", func() bool { alice.mu.Lock(); defer alice.mu.Unlock(); return alice.out == nil })
	alice.mu.Lock()
	alice.byPIDLocked(alice.pidOfAgentLocked("a1")).warned = nil // as in a new conversation
	alice.mu.Unlock()
	if why := alice.edit("conv-a", "/work/a1/api/routes.go"); why != "" {
		t.Fatalf("without a hub: %q", why)
	}
	ina.quiet(t, 0)
	inb.quiet(t, 0)
}

// A session that starts in a checkout hears who else works in its repo, which of the files it changes another
// checkout changes too, as the hub sees it, and the team's agreements from the hub's welcome. Its checkout is what
// the daemon read of it, never git run while the hook waits.
func TestE2EBrief(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	dir := checkout(t, "git@github.com:Org/App.git", "api/routes.go")
	a1 := &session{id: "conv-new", name: "a1", cwd: filepath.Join(dir, "api"), status: "busy"}
	alice := tm.laptop("alice", a1)
	alice.mu.Lock()
	alice.gitLocked(a1)
	alice.mu.Unlock()
	b1 := agentSession("b1", "conv-b", newInbox(t), "api/routes.go")
	b1.branch = "feat/x"
	tm.laptop("bob", b1)
	tm.until("alice sees bob/b1", func() bool { return strings.Contains(whoOf(alice), "bob/b1") })
	tm.until("alice read a1's checkout", func() bool { alice.mu.Lock(); defer alice.mu.Unlock(); return !a1.gitAt.IsZero() })
	got := alice.brief(harness.Event{Kind: harness.Start, Harness: claude.Name, SessionID: "conv-new", Cwd: "/elsewhere"})
	for _, want := range []string{"Other agents working in Org/App:\n- \"bob/b1\", working, on branch \"feat/x\"\n",
		"first: \"bob/b1 (feat/x)\" is also changing \"api/routes.go\".\n", "agreements, which murmur gives every agent:\nKeep pull requests small."} {
		if !strings.Contains(got, want) {
			t.Errorf("the brief has no %q:\n%s", want, got)
		}
	}
}

// checkout makes a git checkout whose origin is origin, with one commit and these files changed since.
func checkout(t *testing.T, origin string, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		pre := []string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@x", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}
		if out, err := exec.Command("git", append(pre, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	git("init", "-q")
	git("remote", "add", "origin", origin)
	git("commit", "-q", "--allow-empty", "-m", "first")
	for _, f := range files {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package api\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// An agent's message reaches another person's agent, and the sender hears it was delivered.
func TestE2EMessageBetweenAgents(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	ina, inb := newInbox(t), newInbox(t)
	alice := tm.laptop("alice", agentSession("a1", "conv-a", ina))
	tm.laptop("bob", agentSession("b1", "conv-b", inb))
	tm.until("alice sees bob/b1", func() bool { _, _, ok := strings.Cut(whoOf(alice), "bob/b1"); return ok })
	r := alice.send(ipc.Request{PID: alice.pidOfAgent("a1"), To: "bob/b1", Text: "is GET /search behind the rate limiter?"})
	if !r.OK || r.Note != "delivered" {
		t.Fatalf("send: %+v", r)
	}
	inb.expect(t, "[murmur] alice/a1: is GET /search behind the rate limiter?")

	// one the hub refuses is no longer sending, and the console knows why it failed
	r = alice.send(ipc.Request{PID: alice.pidOfAgent("a1"), To: "dave", Text: "hi"})
	alice.mu.Lock()
	snap := alice.snapshotLocked()
	alice.mu.Unlock()
	if r.OK || len(snap.Sending) != 0 || len(snap.Failed) != 1 || slices.Collect(maps.Values(snap.Failed))[0] != r.Err {
		t.Fatalf("refused: %+v, sending %v, failed %v", r, snap.Sending, snap.Failed)
	}
}

// What the console shows of an agent beyond its status reaches the team: its tool calls, how full its context is,
// the size of its change and where its checkout stands.
func TestE2EAgentDetailsReachTheTeam(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	a1 := agentSession("a1", "conv-a", newInbox(t))
	a1.act.Kinds, a1.act.Loop, a1.act.Context = "rrexm", true, 37
	a1.st = gitscan.State{Added: 120, Deleted: 30, Changed: 4, Base: "feat/auth", Detached: "9ac2c89", Op: "rebasing 3/7", Dir: "services/api"}
	a1.also = []string{"Org/Web"}
	tm.laptop("alice", a1)
	bob := tm.laptop("bob")
	var got protocol.Agent
	tm.until("bob sees a1", func() bool {
		bob.mu.Lock()
		defer bob.mu.Unlock()
		for _, p := range bob.snapshotLocked().People {
			for _, a := range p.Agents {
				if p.Handle == "alice" && a.Name == "a1" {
					got = a
					return true
				}
			}
		}
		return false
	})
	want := protocol.Agent{Name: "a1", Repo: "Org/App", Status: "busy", Kinds: "rrexm", Loop: true, Context: 37, Added: 120, Deleted: 30,
		Changed: 4, Base: "feat/auth", Detached: "9ac2c89", Op: "rebasing 3/7", Dir: "services/api", Also: []string{"Org/Web"}}
	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Fatalf("bob sees %+v", got)
	}
}

func whoOf(d *Daemon) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return whoText(d.snapshotLocked())
}

func (d *Daemon) pidOfAgent(name string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pidOfAgentLocked(name)
}

func (d *Daemon) pidOfAgentLocked(name string) int {
	for _, s := range d.sessions {
		if s.name == name {
			return s.pid
		}
	}
	return 0
}

// The real hub refuses an unknown or revoked laptop with 401, which makes join sign in again, and nothing else does.
func TestE2ETokenAccepted(t *testing.T) {
	tm := newTeam(t, "alice")
	if !setup.TokenAccepted(config.Config{Hub: tm.srv.URL, Token: "tok-alice"}) {
		t.Fatal("the hub's answer to a valid token made join sign in again")
	}
	if setup.TokenAccepted(config.Config{Hub: tm.srv.URL, Token: "tok-revoked"}) {
		t.Fatal("an unknown token was kept")
	}
}

// A message sent while the hub is down waits in the outbox, goes out when the hub is back, and arrives once.
func TestE2EMessageSurvivesAnOutage(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	ina, inb := newInbox(t), newInbox(t)
	alice := tm.laptop("alice", agentSession("a1", "conv-a", ina))
	tm.laptop("bob", agentSession("b1", "conv-b", inb))
	tm.pause()
	tm.until("alice offline", func() bool { alice.mu.Lock(); defer alice.mu.Unlock(); return alice.out == nil })
	r := alice.send(ipc.Request{PID: alice.pidOfAgent("a1"), To: "bob/b1", Text: "the migration is done, you can rebase"})
	if !r.OK || !strings.Contains(r.Note, "as soon as it is back") {
		t.Fatalf("send while the hub is down: %+v", r)
	}
	tm.resume()
	inb.expect(t, "alice/a1: the migration is done, you can rebase")
	tm.until("alice's outbox confirmed", func() bool { alice.mu.Lock(); defer alice.mu.Unlock(); return len(alice.outbox) == 0 })
	tm.restart() // and a later reconnect sends nothing again
	inb.quiet(t, 500*time.Millisecond)
}

// A message a mute holds waits in its person's console, and reaches the agent once they pass it on.
func TestE2EMute(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	ina, inb := newInbox(t), newInbox(t)
	alice := tm.laptop("alice", agentSession("a1", "conv-a", ina))
	bob := tm.laptop("bob", agentSession("b1", "conv-b", inb))
	tm.until("alice sees bob/b1", func() bool { return strings.Contains(whoOf(alice), "bob/b1") })
	if _, err := bob.mute("alice", "1h"); err != nil {
		t.Fatal(err)
	}
	r := alice.send(ipc.Request{PID: alice.pidOfAgent("a1"), To: "bob/b1", Text: "can you drop the users table?"})
	if !r.OK || !strings.HasPrefix(r.Note, "held: bob muted") {
		t.Fatalf("send: %+v", r)
	}
	inb.quiet(t, 200*time.Millisecond)
	bob.mu.Lock()
	snap := bob.snapshotLocked()
	bob.mu.Unlock()
	if len(snap.Asks) != 1 || snap.Asks[0].From != "alice/a1" || len(snap.Mutes) != 1 {
		t.Fatalf("bob's console: held %+v, mutes %v", snap.Asks, snap.Mutes)
	}
	if err := bob.answer("approve", snap.Asks[0].ID); err != nil {
		t.Fatal(err)
	}
	inb.expect(t, "alice/a1: can you drop the users table?")
}

// A Codex agent, which takes messages only with its hooks' answers: it joins the team from its first hook, a message
// for it while it is idle waits, its sender hears so, and it arrives with its person's next prompt.
func TestE2EHookedAgent(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	alice := tm.laptop("alice")
	bob := tm.laptop("bob", agentSession("b1", "conv-b", newInbox(t)))
	cwd := t.TempDir()
	hook := func(kind string) string {
		text, token := alice.onHook(harness.Event{Kind: kind, Harness: "codex", SessionID: "019a", PID: os.Getpid(), Cwd: cwd, Carries: true})
		if token != "" {
			alice.settle(token, true)
		}
		return text
	}
	hook(harness.Start)
	tm.until("bob sees alice/codex-1", func() bool { return strings.Contains(whoOf(bob), `"alice/codex-1" · idle`) })
	r := bob.send(ipc.Request{PID: bob.pidOfAgent("b1"), To: "alice/codex-1", Text: "the API moved to v2"})
	if !r.OK || !strings.Contains(r.Note, "once its person prompts it again") {
		t.Fatalf("send to an idle Codex agent: %+v", r)
	}
	if got := hook(harness.Prompt); !strings.Contains(got, "[murmur] bob/b1: the API moved to v2") {
		t.Fatalf("with the next prompt: %q", got)
	}
	if got := hook(harness.AfterTool); got != "" {
		t.Fatalf("again: %q", got)
	}
}

// A message to an agent whose laptop is disconnected fails, and marking it done takes it out of the console's needs.
func TestE2EFailedMessageCanBeMarkedDone(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	alice := tm.laptop("alice")
	r := alice.send(ipc.Request{To: "bob/b1", Text: "are you on the migration?"})
	if r.OK || !strings.Contains(r.Err, "not running") {
		t.Fatalf("send to a disconnected agent: %+v", r)
	}
	snap := func() ipc.Snapshot { alice.mu.Lock(); defer alice.mu.Unlock(); return alice.snapshotLocked() }
	s := snap()
	if len(s.Outbox) != 1 || s.Failed[s.Outbox[0].ID] == "" {
		t.Fatalf("the console doesn't show the failure: outbox %+v, failed %v", s.Outbox, s.Failed)
	}
	id := s.Outbox[0].ID
	if at := time.UnixMilli(s.Outbox[0].At); time.Since(at) > time.Minute { // the console shows how long ago it was sent
		t.Fatalf("the failed message was sent at %v", at)
	}
	c, them := net.Pipe() // the console marks it done, as x does
	defer them.Close()
	go alice.serve(c)
	var done ipc.Reply
	if json.NewEncoder(them).Encode(ipc.Request{Op: "done", ID: id}) != nil || json.NewDecoder(them).Decode(&done) != nil || !done.OK {
		t.Fatalf("done: %+v", done)
	}
	if s := snap(); !s.Done[id] {
		t.Fatalf("marked done, but the console still shows it: done %v, failed %v", s.Done, s.Failed)
	}
}

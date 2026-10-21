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
	"sync"
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
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
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
	tm.run(d, "tok-"+handle, sessions...)
	return d
}

// run connects daemon d to the hub as a laptop signed in with token, with these sessions, over a wire of its own, and
// returns once it is online. It reconnects whenever its connection drops, until the wire stops.
func (tm *team) run(d *Daemon, token string, sessions ...*session) *wire {
	tm.t.Helper()
	handle := d.me.Handle
	for i, s := range sessions {
		s.pid = 1000*len(handle) + i + 1
		d.add(s)
	}
	w := tm.wire()
	ctx, stop := context.WithCancel(tm.ctx)
	w.stop = func() {
		stop()
		w.cut()
	}
	go d.publishLoop()
	go func() {
		for ctx.Err() == nil { // as hubLoop does, without its backoff
			_ = d.connect(config.Config{Hub: w.url, Token: token, Handle: handle})
			d.setState("reconnecting")
			time.Sleep(20 * time.Millisecond)
		}
	}()
	tm.until("laptop "+handle+" online", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.state == "online"
	})
	return w
}

// wire is a laptop's network: a proxy to the hub of its own. cut drops its connections, as a network that drops
// does, and the laptop reconnects; stop also ends its daemon's reconnecting, as a laptop that quits.
type wire struct {
	url   string
	stop  func()
	mu    sync.Mutex
	conns []net.Conn
}

func (tm *team) wire() *wire {
	tm.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tm.t.Fatal(err)
	}
	w := &wire{url: "http://" + ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			hub, err := net.Dial("tcp", tm.srv.Listener.Addr().String())
			if err != nil {
				c.Close()
				continue
			}
			w.mu.Lock()
			w.conns = append(w.conns, c, hub)
			w.mu.Unlock()
			go func() { _, _ = io.Copy(hub, c); hub.Close() }()
			go func() { _, _ = io.Copy(c, hub); c.Close() }()
		}
	}()
	tm.t.Cleanup(func() {
		ln.Close()
		w.cut()
	})
	return w
}

func (w *wire) cut() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.conns {
		c.Close()
	}
	w.conns = nil
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
		files: files, version: claude.AskVersion}
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
	inb.quiet(t, 50*time.Millisecond)
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
	inb.quiet(t, 50*time.Millisecond)
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
	tm.until("bob sees a1, once alice's hub said it carries questions by the way", func() bool {
		bob.mu.Lock()
		defer bob.mu.Unlock()
		for _, p := range bob.snapshotLocked().People {
			for _, a := range p.Agents {
				if p.Handle == "alice" && a.Name == "a1" && a.Btw != "" {
					got = a
					return true
				}
			}
		}
		return false
	})
	want := protocol.Agent{Name: "a1", Repo: "Org/App", Status: "busy", Kinds: "rrexm", Loop: true, Context: 37, Added: 120, Deleted: 30,
		Changed: 4, Base: "feat/auth", Detached: "9ac2c89", Op: "rebasing 3/7", Dir: "services/api", Also: []string{"Org/Web"},
		Btw: protocol.BtwApprove} // a Claude Code agent, on a hub that carries questions by the way: alice shares each answer
	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Fatalf("bob sees %+v", got)
	}
}

func whoOf(d *Daemon) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return whoText(d.snapshotLocked(), true)
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

// ---------------------------------------------------------------- questions by the way

// btwLaptop runs a daemon like laptop, signed in with token, whose agents a fake Claude Code answers questions by the
// way for.
func (tm *team) btwLaptop(handle, token string, sessions ...*session) (*Daemon, *fakeAsker, *wire) {
	tm.t.Helper()
	d := newDaemon(handle)
	f := fakeAskers(d)
	return d, f, tm.run(d, token, sessions...)
}

// secondLaptop signs handle in on a second laptop, and returns its token. The hub restarts to read it.
func (tm *team) secondLaptop(handle string) string {
	tm.t.Helper()
	token := "tok-" + handle + "-2"
	sum := sha256.Sum256([]byte(token))
	_ = tm.store.PutDevice(tm.ctx, store.Device{ID: "d-" + handle + "-2", Email: handle + "@x.com", TokenHash: hex.EncodeToString(sum[:]),
		Created: time.Now()})
	tm.restart()
	return token
}

// olderLaptop connects a laptop of handle's on an older murmur, which doesn't say it carries questions by the way,
// with these agents, and returns the frames the hub sends it.
func (tm *team) olderLaptop(handle string, agents ...protocol.Agent) <-chan protocol.Frame {
	tm.t.Helper()
	c, _, err := websocket.Dial(tm.ctx, wsURL(tm.srv.URL), &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer tok-" + handle}}})
	if err != nil {
		tm.t.Fatal(err)
	}
	tm.t.Cleanup(func() { _ = c.CloseNow() })
	if err := wsjson.Write(tm.ctx, c, protocol.Frame{T: protocol.FrameHello, Protocol: protocol.Version, Version: "1.0.0", Agents: agents}); err != nil {
		tm.t.Fatal(err)
	}
	frames := make(chan protocol.Frame, 256)
	go func() {
		for {
			var f protocol.Frame
			if wsjson.Read(tm.ctx, c, &f) != nil {
				return
			}
			select {
			case frames <- f:
			default:
			}
		}
	}()
	return frames
}

// seesBtw waits until d's roster says how agent addr takes questions by the way: protocol.BtwApprove or BtwAuto, or ""
// when it can't be asked.
func (tm *team) seesBtw(d *Daemon, addr, btw string) {
	tm.t.Helper()
	person, name := protocol.Split(addr)
	tm.until("the roster to say "+addr+" takes questions by the way as "+btw, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, p := range d.people {
			for _, a := range p.Agents {
				if p.Handle == person && a.Name == name {
					return a.Btw == btw
				}
			}
		}
		return false
	})
}

// asks asks agent to something by the way, as d's person, under id.
func asks(t *testing.T, d *Daemon, id, to, question string) {
	t.Helper()
	if r := call(d, ipc.Request{Op: "btw", ID: id, To: to, Text: question}); !r.OK {
		t.Fatalf("btw %s to %s: %+v", id, to, r)
	}
}

// stands waits until the aside id on d stands as ok says, and returns it: d's person's own when mine.
func (tm *team) stands(d *Daemon, id string, mine bool, what string, ok func(a aside) bool) aside {
	tm.t.Helper()
	var a aside
	tm.until(what, func() bool { a = asideOf(d, id, mine); return ok(a) })
	return a
}

func (f *fakeAsker) hold() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = make(chan struct{})
}

// A teammate's question to an agent whose person reads each answer first: the copy runs at once on the agent's laptop,
// the answer waits there, and the asker hears so. Shared, the answer reaches the asker; dropped, the asker hears that.
// The agent itself never sees a question.
func TestE2EBtwApprove(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	inb := newInbox(t)
	alice := tm.laptop("alice")
	bob, f, _ := tm.btwLaptop("bob", "tok-bob", agentSession("b1", "conv-b", inb))
	tm.seesBtw(alice, "bob/b1", protocol.BtwApprove)

	asks(t, alice, "q1", "bob/b1", "what are you stuck on?")
	tm.stands(bob, "q1", false, "bob's answer waits for him", func(a aside) bool { return a.State == ipc.AsideHeld })
	tm.stands(alice, "q1", true, "alice hears it waits for bob", func(a aside) bool {
		return a.State == ipc.AsideAsking && a.Note == "answered; waits for bob to share it" && a.Answer == ""
	})
	if r := call(bob, ipc.Request{Op: "btw-approve", ID: "q1"}); !r.OK {
		t.Fatalf("approve: %+v", r)
	}
	if a := tm.stands(alice, "q1", true, "alice has the answer", func(a aside) bool { return a.Final() }); a.Answer != "b1: what are you stuck on?" ||
		a.Tokens != 1200 || a.State != ipc.AsideAnswered {
		t.Fatalf("alice's question: %+v", a)
	}
	if q := f.questions(); len(q) != 1 || q[0].From != "alice" {
		t.Fatalf("the copies: %+v", q)
	}

	asks(t, alice, "q2", "bob/b1", "can you push?")
	tm.stands(bob, "q2", false, "bob's second answer waits for him", func(a aside) bool { return a.State == ipc.AsideHeld })
	if r := call(bob, ipc.Request{Op: "btw-decline", ID: "q2"}); !r.OK {
		t.Fatalf("decline: %+v", r)
	}
	if a := tm.stands(alice, "q2", true, "alice hears bob dropped it", func(a aside) bool { return a.Final() }); a.Err != "bob didn't share the answer" ||
		a.Answer != "" {
		t.Fatalf("dropped: %+v", a)
	}
	inb.quiet(t, 100*time.Millisecond)
}

// With auto, a teammate's answer goes on its own. A question for an agent idle for over an hour waits for its person's
// go even so, and its person sees what the copy would read; once they say go, the answer goes on its own.
func TestE2EBtwAutoAndCold(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	transcript := filepath.Join(t.TempDir(), "conv-c.jsonl")
	_ = os.WriteFile(transcript, []byte("{}\n"), 0o600)
	three := time.Now().Add(-3*time.Hour - time.Minute)
	_ = os.Chtimes(transcript, three, three)
	b2 := agentSession("b2", "conv-c", newInbox(t))
	b2.status, b2.since, b2.transcript, b2.act.Tokens = protocol.StatusIdle, three.UnixMilli(), transcript, 180000
	alice := tm.laptop("alice")
	bob, f, _ := tm.btwLaptop("bob", "tok-bob", agentSession("b1", "conv-b", newInbox(t)), b2)
	if err := bob.setBtwAnswers(ipc.BtwAuto); err != nil {
		t.Fatal(err)
	}
	tm.seesBtw(alice, "bob/b1", protocol.BtwAuto)

	asks(t, alice, "q1", "bob/b1", "which port?")
	if a := tm.stands(alice, "q1", true, "alice has b1's answer", func(a aside) bool { return a.Final() }); a.Answer != "b1: which port?" {
		t.Fatalf("auto: %+v", a)
	}
	asks(t, alice, "q2", "bob/b2", "where did you leave it?")
	tm.stands(alice, "q2", true, "alice hears b2's question waits", func(a aside) bool {
		return a.Note == "b2 has been idle for 3h; the question waits for bob's go"
	})
	if a := asideOf(bob, "q2", false); a.State != ipc.AsideCold || a.Cold != 180000 || len(f.questions()) != 1 {
		t.Fatalf("cold, with auto: %+v, %d copies", a, len(f.questions()))
	}
	if r := call(bob, ipc.Request{Op: "btw-approve", ID: "q2"}); !r.OK {
		t.Fatalf("go: %+v", r)
	}
	if a := tm.stands(alice, "q2", true, "alice has b2's answer", func(a aside) bool { return a.Final() }); a.Answer != "b2: where did you leave it?" {
		t.Fatalf("after go: %+v", a)
	}
}

// With off, teammates can't ask: the team sees only its person and their agents can, and the hub refuses for the
// agent's person, whose laptop runs nothing.
func TestE2EBtwOff(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	alice := tm.laptop("alice")
	bob, f, _ := tm.btwLaptop("bob", "tok-bob", agentSession("b1", "conv-b", newInbox(t)))
	tm.seesBtw(alice, "bob/b1", protocol.BtwApprove)
	if err := bob.setBtwAnswers(ipc.BtwOff); err != nil {
		t.Fatal(err)
	}
	tm.seesBtw(alice, "bob/b1", protocol.BtwMine)
	asks(t, alice, "q1", "bob/b1", "which port?")
	if a := tm.stands(alice, "q1", true, "alice's question is refused", func(a aside) bool { return a.Final() }); a.Err !=
		"bob/b1 doesn't take questions by the way: bob turned them off" {
		t.Fatalf("off: %+v", a)
	}
	if len(f.questions()) != 0 || asideOf(bob, "q1", false).ID != "" {
		t.Fatalf("bob's laptop took the question: %+v", asideOf(bob, "q1", false))
	}
}

// A laptop on an older murmur, which doesn't say it carries questions by the way: its agents can't be asked, whatever
// it says of them, a question for one is refused saying why, and it gets no frame it can't read.
func TestE2EBtwOlderLaptop(t *testing.T) {
	tm := newTeam(t, "alice", "carol")
	alice := tm.laptop("alice")
	frames := tm.olderLaptop("carol", protocol.Agent{Name: "c1", Status: protocol.StatusBusy, Btw: protocol.BtwApprove})
	tm.until("alice sees carol/c1", func() bool { return strings.Contains(whoOf(alice), "carol/c1") })
	tm.seesBtw(alice, "carol/c1", "")
	asks(t, alice, "q1", "carol/c1", "which port?")
	if a := tm.stands(alice, "q1", true, "alice's question is refused", func(a aside) bool { return a.Final() }); a.Err !=
		"carol's laptop runs an older murmur, which can't answer questions by the way" {
		t.Fatalf("an older laptop: %+v", a)
	}
	for end := time.After(100 * time.Millisecond); ; {
		select {
		case f := <-frames:
			if f.T == protocol.FrameBtw || f.T == protocol.FrameBtwAnswer || f.Btw != nil {
				t.Fatalf("the older laptop got %+v", f)
			}
			continue
		case <-end:
		}
		break
	}
}

// The team's admins turn questions by the way off: laptops hear it, teammates' agents can't be asked, a question that
// waits for its answer fails, and a person still asks their own agents on the laptop they ask from, without a hub too.
func TestE2EBtwHubSwitchedOff(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	alice, fa, _ := tm.btwLaptop("alice", "tok-alice", agentSession("a1", "conv-a", newInbox(t)))
	bob, _, _ := tm.btwLaptop("bob", "tok-bob", agentSession("b1", "conv-b", newInbox(t)))
	tm.seesBtw(alice, "bob/b1", protocol.BtwApprove)
	asks(t, alice, "q1", "bob/b1", "what are you stuck on?")
	tm.stands(bob, "q1", false, "bob's answer waits for him", func(a aside) bool { return a.State == ipc.AsideHeld })

	h := tm.cur.Load()
	s := h.Settings()
	s.Btw = false
	if err := h.SaveSettings(tm.ctx, s, "admin@x.com"); err != nil {
		t.Fatal(err)
	}
	tm.seesBtw(alice, "bob/b1", "")
	if a := tm.stands(alice, "q1", true, "alice's open question fails", func(a aside) bool { return a.Final() }); a.Err !=
		"the hub no longer carries questions by the way" {
		t.Fatalf("open question: %+v", a)
	}
	if r := call(alice, ipc.Request{Op: "btw", ID: "q2", To: "bob/b1", Text: "hi"}); r.OK || r.Err != noHubBtw {
		t.Fatalf("a new question: %+v", r)
	}

	asks(t, alice, "q3", "a1", "status?")
	if a := tm.stands(alice, "q3", true, "alice's own agent answers", func(a aside) bool { return a.Final() }); a.Answer != "a1: status?" {
		t.Fatalf("own agent, hub off: %+v", a)
	}
	tm.pause()
	tm.until("alice offline", func() bool { alice.mu.Lock(); defer alice.mu.Unlock(); return alice.out == nil })
	asks(t, alice, "q4", "a1", "and now?")
	if a := tm.stands(alice, "q4", true, "alice's own agent answers without a hub", func(a aside) bool { return a.Final() }); a.Answer != "a1: and now?" {
		t.Fatalf("own agent, no hub: %+v", a)
	}
	if q := fa.questions(); len(q) != 2 || q[0].From != "your person" {
		t.Fatalf("alice's copies: %+v", q)
	}
}

// A person's own agent on their other laptop answers them at once, cold, with teammates' questions off there: their
// own questions never wait.
func TestE2EBtwOwnAgentOnAnotherLaptop(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	token := tm.secondLaptop("alice")
	transcript := filepath.Join(t.TempDir(), "conv-a2.jsonl")
	_ = os.WriteFile(transcript, []byte("{}\n"), 0o600)
	three := time.Now().Add(-3*time.Hour - time.Minute)
	_ = os.Chtimes(transcript, three, three)
	a2 := agentSession("a2", "conv-a2", newInbox(t))
	a2.status, a2.since, a2.transcript = protocol.StatusIdle, three.UnixMilli(), transcript
	alice := tm.laptop("alice")
	other, f, _ := tm.btwLaptop("alice", token, a2)
	if err := other.setBtwAnswers(ipc.BtwOff); err != nil {
		t.Fatal(err)
	}
	tm.seesBtw(alice, "alice/a2", protocol.BtwMine) // teammates can't ask it

	asks(t, alice, "q1", "a2", "where did you leave it?")
	if a := tm.stands(alice, "q1", true, "alice has her answer", func(a aside) bool { return a.Final() }); a.Answer != "a2: where did you leave it?" ||
		a.Note != "" {
		t.Fatalf("own agent on another laptop: %+v", a)
	}
	if q := f.questions(); len(q) != 1 || q[0].From != "your person" || asideOf(other, "q1", false).State != ipc.AsideAnswered {
		t.Fatalf("the copy: %+v, %+v", q, asideOf(other, "q1", false))
	}
}

// The asking laptop reconnects while a copy answers its question: it asks again, the agent's laptop answers from what
// it has, saying the question is being answered, and the copy runs once.
func TestE2EBtwAskerReconnects(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	alice, _, net := tm.btwLaptop("alice", "tok-alice")
	bob, f, _ := tm.btwLaptop("bob", "tok-bob", agentSession("b1", "conv-b", newInbox(t)))
	if err := bob.setBtwAnswers(ipc.BtwAuto); err != nil {
		t.Fatal(err)
	}
	tm.seesBtw(alice, "bob/b1", protocol.BtwAuto)
	f.hold()
	asks(t, alice, "q1", "bob/b1", "what are you stuck on?")
	tm.until("bob's copy runs", func() bool { return len(f.questions()) == 1 })
	if a := asideOf(alice, "q1", true); a.Note != "" {
		t.Fatalf("before the reconnect: %+v", a)
	}
	net.cut()
	tm.stands(alice, "q1", true, "alice asked again, and bob answered from what he has", func(a aside) bool { return a.Note == "being answered" })
	f.gate <- struct{}{}
	if a := tm.stands(alice, "q1", true, "alice has the answer", func(a aside) bool { return a.Final() }); a.Answer != "b1: what are you stuck on?" {
		t.Fatalf("after the reconnect: %+v", a)
	}
	if n := len(f.questions()); n != 1 {
		t.Fatalf("%d copies", n)
	}
}

// The owner's laptop quits before its answer leaves, and its daemon starts again: the asker, connected all along, asks
// again on its two-minute timer, and the new daemon answers from its history, without a second copy.
func TestE2EBtwOwnerRestarts(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	alice, _, _ := tm.btwLaptop("alice", "tok-alice")
	bob := newDaemon("bob")
	f := fakeAskers(bob)
	bob.btwAnswers = ipc.BtwAuto
	bob.loadHistory() // the only laptop here with a history: they share MURMUR_HOME
	net := tm.run(bob, "tok-bob", agentSession("b1", "conv-b", newInbox(t)))
	tm.seesBtw(alice, "bob/b1", protocol.BtwAuto)
	f.hold()
	asks(t, alice, "q1", "bob/b1", "what are you stuck on?")
	tm.until("bob's copy runs", func() bool { return len(f.questions()) == 1 })
	net.stop()
	tm.until("bob's laptop is offline", func() bool { bob.mu.Lock(); defer bob.mu.Unlock(); return bob.out == nil })
	f.gate <- struct{}{} // the copy answers, with no hub to hear it
	tm.stands(bob, "q1", false, "bob has the answer", func(a aside) bool { return a.State == ipc.AsideAnswered })
	bob.mu.Lock()
	bob.hist.Close()
	bob.mu.Unlock()

	again := newDaemon("bob")
	f2 := fakeAskers(again)
	again.btwAnswers = ipc.BtwAuto
	again.loadHistory()
	t.Cleanup(func() { again.hist.Close() })
	tm.run(again, "tok-bob", agentSession("b1", "conv-b", newInbox(t)))
	time.Sleep(100 * time.Millisecond)
	if a := asideOf(alice, "q1", true); a.State != ipc.AsideAsking {
		t.Fatalf("before alice asks again: %+v", a)
	}
	alice.btwTick(0) // two minutes later
	if a := tm.stands(alice, "q1", true, "alice has the answer", func(a aside) bool { return a.Final() }); a.Answer != "b1: what are you stuck on?" {
		t.Fatalf("after bob's restart: %+v", a)
	}
	if len(f.questions()) != 1 || len(f2.questions()) != 0 {
		t.Fatalf("copies: %d before the restart, %d after", len(f.questions()), len(f2.questions()))
	}
}

// An agent asks a teammate's agent by the way. The copy hears it from the agent, and its person shares the answer once
// the agent's tool stopped waiting: the answer reaches the agent as a message from the agent asked, once, and the
// console shows it as the question's answer, not as a message. The agent asked never sees the question.
func TestE2EBtwAgentAsksAgent(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	ina, inb := newInbox(t), newInbox(t)
	a1 := agentSession("a1", "conv-a", ina)
	alice := tm.laptop("alice", a1)
	bob, f, _ := tm.btwLaptop("bob", "tok-bob", agentSession("b1", "conv-b", inb))
	tm.seesBtw(alice, "bob/b1", protocol.BtwApprove)

	if r := agentAsks(alice, a1, "q1", "bob/b1", "what are you stuck on?"); !r.OK {
		t.Fatalf("btw: %+v", r)
	}
	tm.stands(bob, "q1", false, "bob's answer waits for him", func(a aside) bool { return a.State == ipc.AsideHeld && a.From == "alice/a1" })
	tm.stands(alice, "q1", true, "a1's question waits for bob", func(a aside) bool { return a.Note == "answered; waits for bob to share it" })
	waitsThenGoes(t, alice, a1, "q1")
	if r := call(bob, ipc.Request{Op: "btw-approve", ID: "q1"}); !r.OK {
		t.Fatalf("approve: %+v", r)
	}
	if got := ina.next(t); got != lateAnswer("bob/b1", "what are you stuck on?", "b1: what are you stuck on?") {
		t.Fatalf("a1 got:\n%s", got)
	}
	alice.btwTick(0)
	ina.quiet(t, 200*time.Millisecond)
	inb.quiet(t, 50*time.Millisecond)
	if q := f.questions(); len(q) != 1 || q[0].From != "alice's agent a1" {
		t.Fatalf("the copies: %+v", q)
	}
	alice.mu.Lock()
	snap := alice.snapshotLocked()
	alice.mu.Unlock()
	if len(snap.Asides) != 1 || snap.Asides[0].State != ipc.AsideAnswered || snap.Asides[0].From != "alice/a1" || len(snap.Msgs) != 0 {
		t.Fatalf("alice's console sees %+v, and messages %+v", snap.Asides, snap.Msgs)
	}
}

// The hub restarts while a question waits for its person's go, and the asking laptop is back before the agent's: the
// question asked again waits, in a note, rather than failing for an agent that isn't running, and gets its answer once
// the agent's laptop is back.
func TestE2EBtwHubRestarts(t *testing.T) {
	tm := newTeam(t, "alice", "bob")
	alice, _, _ := tm.btwLaptop("alice", "tok-alice")
	bob := newDaemon("bob")
	f := fakeAskers(bob)
	bob.loadHistory() // the only laptop here with a history: they share MURMUR_HOME
	net := tm.run(bob, "tok-bob", agentSession("b1", "conv-b", newInbox(t)))
	tm.seesBtw(alice, "bob/b1", protocol.BtwApprove)
	asks(t, alice, "q1", "bob/b1", "what are you stuck on?")
	tm.stands(bob, "q1", false, "bob's answer waits for him", func(a aside) bool { return a.State == ipc.AsideHeld })
	net.stop()
	tm.until("bob's laptop is offline", func() bool { bob.mu.Lock(); defer bob.mu.Unlock(); return bob.out == nil })
	bob.mu.Lock()
	bob.hist.Close()
	bob.mu.Unlock()

	tm.restart()
	tm.stands(alice, "q1", true, "alice hears bob's laptop is away", func(a aside) bool {
		return a.Note == "bob's laptop is away; murmur asks again until it is back"
	})
	if a := asideOf(alice, "q1", true); a.State != ipc.AsideAsking {
		t.Fatalf("while bob's laptop is away: %+v", a)
	}
	again := newDaemon("bob")
	f2 := fakeAskers(again)
	again.loadHistory()
	t.Cleanup(func() { again.hist.Close() })
	tm.run(again, "tok-bob", agentSession("b1", "conv-b", newInbox(t)))
	alice.btwTick(0) // two minutes later
	tm.stands(alice, "q1", true, "alice hears the answer waits for bob", func(a aside) bool { return a.Note == "answered; waits for bob to share it" })
	if r := call(again, ipc.Request{Op: "btw-approve", ID: "q1"}); !r.OK {
		t.Fatalf("approve: %+v", r)
	}
	if a := tm.stands(alice, "q1", true, "alice has the answer", func(a aside) bool { return a.Final() }); a.Answer != "b1: what are you stuck on?" {
		t.Fatalf("after the hub's restart: %+v", a)
	}
	if len(f.questions()) != 1 || len(f2.questions()) != 0 {
		t.Fatalf("copies: %d before, %d after", len(f.questions()), len(f2.questions()))
	}
}

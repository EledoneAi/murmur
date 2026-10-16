package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/bus"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/store"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type laptop struct {
	t       *testing.T
	c       *websocket.Conn
	frames  chan protocol.Frame
	backlog []protocol.Frame
}

func connect(t *testing.T, url, token string, agents ...protocol.Agent) *laptop {
	t.Helper()
	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(url, "http")+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	l := &laptop{t: t, c: c, frames: make(chan protocol.Frame, 100)}
	go func() {
		for {
			var f protocol.Frame
			if wsjson.Read(context.Background(), c, &f) != nil {
				close(l.frames)
				return
			}
			l.frames <- f
		}
	}()
	l.send(protocol.Frame{T: protocol.FrameHello, Protocol: protocol.Version, Host: "test", Agents: agents})
	l.expect(func(f protocol.Frame) bool { return f.T == "welcome" })
	return l
}

func (l *laptop) send(f protocol.Frame) {
	if err := wsjson.Write(context.Background(), l.c, f); err != nil {
		l.t.Fatalf("write: %v", err)
	}
}

// expect returns the first frame matching ok, in arrival order. Frames it passes over stay for later calls.
func (l *laptop) expect(ok func(protocol.Frame) bool) protocol.Frame {
	l.t.Helper()
	for i, f := range l.backlog {
		if ok(f) {
			l.backlog = append(l.backlog[:i], l.backlog[i+1:]...)
			return f
		}
	}
	timeout := time.After(3 * time.Second)
	for {
		select {
		case f, open := <-l.frames:
			if !open {
				l.t.Fatal("connection closed")
			}
			if ok(f) {
				return f
			}
			l.backlog = append(l.backlog, f)
		case <-timeout:
			l.t.Fatal("timed out waiting for a frame")
		}
	}
}

func TestHub(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	for _, u := range []store.User{{Email: "alice@x.com", Handle: "alice"}, {Email: "bob@x.com", Handle: "bob"}, {Email: "carol@x.com", Handle: "carol"}} {
		_ = st.PutUser(ctx, u)
		_ = st.PutDevice(ctx, store.Device{ID: "d-" + u.Handle, Email: u.Email, TokenHash: hashToken("tok-" + u.Handle), Created: time.Now()})
	}
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef")}, st)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	// a revoked or unknown token never gets a socket
	if _, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer nope"}}}); err == nil {
		t.Fatal("unknown token was accepted")
	}

	alice := connect(t, srv.URL, "tok-alice", protocol.Agent{Name: "a1", Repo: "Org/App", Tree: "t1", Status: "busy", Files: []string{"x.go"}})
	bob := connect(t, srv.URL, "tok-bob", protocol.Agent{Name: "b1", Repo: "org/app", Tree: "t2", Status: "idle", Files: []string{"x.go", "y.go"}})

	// both see the whole team, without the private file lists
	r := alice.expect(func(f protocol.Frame) bool {
		return f.T == "roster" && len(f.People) == 3 && len(f.People[1].Agents) == 1 && f.People[1].Online
	})
	if r.People[0].Agents[0].Files != nil || r.People[0].Agents[0].Tree != "" {
		t.Fatal("roster leaks file lists")
	}

	// the same file changed in two checkouts of one repo: alice's console sees it, and her agent hears of it when it
	// checks the file before an edit
	alice.expect(func(f protocol.Frame) bool {
		return f.T == "roster" && len(f.Pairs) == 1 && f.Pairs[0].Other == "bob/b1" && len(f.Overlaps) == 0
	})
	alice.send(protocol.Frame{T: protocol.FrameCheck, ID: "c1", Repo: "Org/App", Tree: "t1", Files: []string{"x.go"}})
	if c := alice.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameChecked && f.ID == "c1" }); len(c.Overlaps) != 1 ||
		c.Overlaps[0] != "bob/b1 is also changing x.go" {
		t.Fatalf("check: %q", c.Overlaps)
	}

	// agent to agent: delivered to the laptop that runs it, acknowledged back to the sender
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m1", From: "a1", To: "Bob/b1", Text: "renaming green_fields()"}})
	got := bob.expect(func(f protocol.Frame) bool { return f.T == "msg" && f.Msg.ID == "m1" })
	if got.Echo || got.Msg.From != "alice/a1" || got.Msg.To != "bob/b1" {
		t.Fatalf("delivery: %+v", got)
	}
	alice.expect(func(f protocol.Frame) bool { return f.T == "msg" && f.Msg.ID == "m1" && f.Echo }) // alice's console copy
	bob.send(protocol.Frame{T: protocol.FrameDelivered, ID: "m1", OK: true})
	if a := alice.expect(func(f protocol.Frame) bool { return f.T == "ack" && f.ID == "m1" }); !a.OK || a.Note != "delivered" {
		t.Fatalf("ack: %+v", a)
	}

	// a session can't speak for an agent its laptop doesn't run
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m2", From: "b1", To: "bob", Text: "hi"}})
	if a := alice.expect(func(f protocol.Frame) bool { return f.T == "ack" && f.ID == "m2" }); a.OK {
		t.Fatal("spoofed sender accepted")
	}

	// unknown people and stopped agents are refused right away, with a reason
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m3", To: "dave", Text: "hi"}})
	if a := alice.expect(func(f protocol.Frame) bool { return f.T == "ack" && f.ID == "m3" }); a.OK || !strings.Contains(a.Err, "dave") {
		t.Fatalf("unknown person: %+v", a)
	}
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m4", To: "carol/c9", Text: "hi"}})
	if a := alice.expect(func(f protocol.Frame) bool { return f.T == "ack" && f.ID == "m4" }); a.OK || !strings.Contains(a.Err, "not running") {
		t.Fatalf("stopped agent: %+v", a)
	}

	// a person who is offline gets the message when their laptop connects
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m5", From: "a1", To: "carol", Text: "who decides designation 3?"}})
	if a := alice.expect(func(f protocol.Frame) bool { return f.T == "ack" && f.ID == "m5" }); !a.OK || !strings.Contains(a.Note, "offline") {
		t.Fatalf("queued: %+v", a)
	}
	carol := connect(t, srv.URL, "tok-carol")
	if m := carol.expect(func(f protocol.Frame) bool { return f.T == "msg" && f.Msg.ID == "m5" }); m.Echo || m.Msg.From != "alice/a1" {
		t.Fatalf("pending: %+v", m)
	}
	carol.send(protocol.Frame{T: protocol.FrameDelivered, ID: "m5", OK: true})
	time.Sleep(50 * time.Millisecond)
	if p, _ := st.Pending(ctx, "carol@x.com"); len(p) != 0 {
		t.Fatal("delivered message still pending")
	}

	// an agent whose laptop is reconnecting gets the message once it is back
	_ = bob.c.CloseNow()
	time.Sleep(100 * time.Millisecond)
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m6", From: "a1", To: "bob/b1", Text: "still there?"}})
	time.Sleep(50 * time.Millisecond)
	bob = connect(t, srv.URL, "tok-bob", protocol.Agent{Name: "b1", Repo: "org/app", Tree: "t2", Status: "idle"})
	if m := bob.expect(func(f protocol.Frame) bool { return f.T == "msg" && f.Msg.ID == "m6" }); m.Echo {
		t.Fatal("held message arrived as an echo")
	}
	bob.send(protocol.Frame{T: protocol.FrameDelivered, ID: "m6", OK: true})
	if a := alice.expect(func(f protocol.Frame) bool { return f.T == "ack" && f.ID == "m6" }); !a.OK {
		t.Fatalf("held ack: %+v", a)
	}

	// two agents can't ping-pong forever without a person
	var last protocol.Frame
	for i := range Defaults().LoopMax + 1 {
		id := "loop" + string(rune('a'+i))
		alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: id, From: "a1", To: "bob/b1", Text: "again"}})
		if i == Defaults().LoopMax {
			last = alice.expect(func(f protocol.Frame) bool { return f.T == "ack" && f.ID == id })
		}
	}
	if last.OK || !strings.Contains(last.Err, "paused") {
		t.Fatalf("loop guard: %+v", last)
	}
}

func TestPrompt(t *testing.T) {
	p := protocol.Prompt(protocol.Msg{From: "bob/main-1", Text: "Is the invoicer live?\nI need it for reports."})
	if !strings.HasPrefix(p, "[murmur] bob/main-1: Is the invoicer live?\nI need it") || !strings.Contains(p, `to "bob/main-1"`) {
		t.Fatalf("prompt: %q", p)
	}
	if person, agent := protocol.Split(" Bob/main-1 "); person != "bob" || agent != "main-1" {
		t.Fatalf("split: %q %q", person, agent)
	}
}

func TestLaptopSignIn(t *testing.T) {
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef"), DevLogin: true, PublicURL: "http://localhost"}, store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	verifier := "a-secret-only-the-laptop-knows-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	login := srv.URL + "/cli/login?port=5555&host=mbp&state=laptop-state-0123456789&challenge=" + base64.RawURLEncoding.EncodeToString(sum[:])
	connectWith := func(q url.Values) (int, string) { // the page asks the hub to connect the laptop
		t.Helper()
		b, _ := json.Marshal(map[string]string{"port": q.Get("port"), "host": q.Get("host"), "state": q.Get("state"), "challenge": q.Get("challenge")})
		resp, err := c.Post(srv.URL+"/api/cli/login", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		var out struct{ Redirect string }
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out.Redirect
	}
	connectLaptop := func() string { // the person sees which laptop asks, then says yes
		t.Helper()
		resp, _ := c.Get(login)
		if b, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "Connect a laptop") {
			t.Fatalf("no confirmation before connecting: %d %s", resp.StatusCode, b)
		}
		u, _ := url.Parse(login)
		_, redirect := connectWith(u.Query())
		return redirect
	}

	// not signed in yet: off to the sign-in page, which comes back here
	resp, _ := c.Get(login)
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || !strings.HasPrefix(loc, "/login?next=") {
		t.Fatalf("expected sign-in first, got %d %s", resp.StatusCode, loc)
	}
	// outside the domain: refused
	resp, _ = c.PostForm(srv.URL+"/dev-login", url.Values{"email": {"eve@gmail.com"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("outside email accepted: %d", resp.StatusCode)
	}
	if _, err := c.PostForm(srv.URL+"/dev-login", url.Values{"email": {"alice.martin@x.com"}}); err != nil {
		t.Fatal(err)
	}
	loc, _ := url.Parse(connectLaptop())
	code := loc.Query().Get("code")
	if loc.Host != "127.0.0.1:5555" || code == "" || loc.Query().Get("state") != "laptop-state-0123456789" {
		t.Fatalf("expected the laptop's loopback with its state, got %s", loc)
	}
	token := func(code, verifier string) (int, map[string]string) {
		b, _ := json.Marshal(map[string]string{"code": code, "verifier": verifier})
		resp, err := http.Post(srv.URL+"/cli/token", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if status, _ := token(code, "wrong"); status != http.StatusBadRequest {
		t.Fatal("wrong verifier accepted")
	}
	loc, _ = url.Parse(connectLaptop()) // a wrong verifier burns the code: start over
	code = loc.Query().Get("code")
	status, got := token(code, verifier)
	if status != http.StatusOK || got["handle"] != "alice" || got["token"] == "" {
		t.Fatalf("token: %d %v", status, got)
	}
	if status, _ := token(code, verifier); status != http.StatusBadRequest {
		t.Fatal("a code worked twice")
	}
	// the token opens the WebSocket
	l := connect(t, srv.URL, got["token"])
	l.expect(func(f protocol.Frame) bool { return f.T == "roster" })
	// a bad port never redirects anywhere
	if status, redirect := connectWith(url.Values{"port": {"80"}, "state": {"laptop-state-0123456789"},
		"challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}}); status != http.StatusBadRequest || redirect != "" {
		t.Fatal("privileged port accepted")
	}
}

// Two checkouts change the same file: the console's roster says so, one line per pair of checkouts, until the files
// are pushed. Nothing is pushed to the agents: they hear of it from a check, when they edit one of the files.
func TestOverlapsOnTheRosterOnly(t *testing.T) {
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef")}, store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	laptop := func(handle string) *link {
		l := &link{dev: "d-" + handle, handle: handle, email: handle + "@x.com", out: make(chan protocol.Frame, 64), cancel: func() {}}
		h.mu.Lock()
		h.links[l.dev] = l
		h.mu.Unlock()
		return l
	}
	me, bob := laptop("alice"), laptop("bob")
	var six []protocol.Agent
	for _, n := range []string{"22", "62", "86", "f0", "f2", "41"} {
		six = append(six, protocol.Agent{Name: "backend-" + n, Repo: "Org/Backend", Tree: "t2", Files: []string{"x.go"}})
	}
	h.handle(bob, protocol.Frame{T: protocol.FrameAgents, Agents: six})
	h.handle(me, protocol.Frame{T: protocol.FrameAgents, Agents: []protocol.Agent{{Name: "f5", Repo: "Org/Backend", Tree: "t1", Files: []string{"x.go"}}}})
	var roster protocol.Frame
	for timeout := time.After(3 * time.Second); len(roster.Pairs) == 0; {
		select {
		case f := <-me.out:
			if f.T == protocol.FrameMsg {
				t.Fatalf("pushed to alice: %+v", f.Msg)
			}
			if f.T == protocol.FrameRoster {
				roster = f
			}
		case <-timeout:
			t.Fatal("no roster with the overlap")
		}
	}
	if p := roster.Pairs; len(p) != 1 || p[0].Mine != "f5" || !strings.HasPrefix(p[0].Other, "bob/backend-") || strings.Join(p[0].Files, ",") != "x.go" {
		t.Fatalf("current overlaps: %+v", roster.Pairs)
	}
	for len(bob.out) > 0 {
		if f := <-bob.out; f.T == protocol.FrameMsg {
			t.Fatalf("pushed to bob: %+v", f.Msg)
		}
	}
	var pushed []protocol.Agent
	for _, a := range six {
		a.Files = nil
		pushed = append(pushed, a)
	}
	h.handle(bob, protocol.Frame{T: protocol.FrameAgents, Agents: pushed})
	h.mu.Lock()
	x := newIndex(h.holdersLocked(), h.settings)
	h.mu.Unlock()
	if got := x.current("alice"); len(got) != 0 {
		t.Fatalf("overlap outlived its files: %+v", got)
	}
}

// One person with as many laptops and agents as the defaults allow, all claiming the same files in made-up
// checkouts, can't make a roster slow for the team.
func TestOverlapIndexStaysCheap(t *testing.T) {
	var holders []holder
	files := make([]string, 300)
	for i := range files {
		files[i] = fmt.Sprintf("f%03d.go", i)
	}
	for l := range 10 {
		var agents []protocol.Agent
		for a := range 64 {
			agents = append(agents, protocol.Agent{Name: fmt.Sprintf("a%d-%d", l, a), Repo: "org/app", Tree: fmt.Sprintf("t%d-%d", l, a), Files: files})
		}
		holders = append(holders, holder{dev: fmt.Sprint(l), handle: "mallory", agents: agents, online: true})
	}
	holders = append(holders, holder{dev: "x", handle: "alice", online: true,
		agents: []protocol.Agent{{Name: "a1", Repo: "org/app", Tree: "ta", Files: files}}})
	start := time.Now()
	x := newIndex(holders, Defaults())
	got := x.current("alice")
	x.current("mallory")
	// ponytail: a loose bound, for CI runners under -race; without the per-file cap this takes 20 times longer
	if took := time.Since(start); took > 15*time.Second {
		t.Fatalf("an index and two people's overlaps took %v", took)
	}
	for k, cs := range x.files {
		if len(cs) > perFile {
			t.Fatalf("the index keeps %d checkouts for %q", len(cs), k)
		}
	}
	if len(got) == 0 || len(got) > maxPairs {
		t.Fatalf("alice's overlaps: %d pairs", len(got))
	}
}

// A person's overlaps come one per pair of checkouts, named by the agent of theirs that sorts first, with the first
// three shared files in the order the agent lists them and how many more; other repos and people don't count, and
// a checkout with several agents counts once.
func TestOverlapPairs(t *testing.T) {
	files := []string{"a.go", "b.go", "c.go", "d.go", "e.go"}
	x := newIndex([]holder{
		{dev: "1", handle: "alice", online: true, agents: []protocol.Agent{
			{Name: "a2", Repo: "org/app", Tree: "ta", Files: files}, {Name: "a1", Repo: "org/app", Tree: "ta", Files: files},
			{Name: "a3", Repo: "org/app", Tree: "tc", Files: []string{"z.go"}}}},
		{dev: "2", handle: "bob", online: true, agents: []protocol.Agent{{Name: "b1", Repo: "org/app", Tree: "tb", Files: files[1:]},
			{Name: "b2", Repo: "org/app", Tree: "tb", Files: files[1:]}}}, // two agents in one checkout: its files count once
		{dev: "3", handle: "carol", online: true, agents: []protocol.Agent{{Name: "c1", Repo: "ORG/App", Tree: "td", Files: []string{"z.go"}}}},
		{dev: "4", handle: "dave", online: true, agents: []protocol.Agent{{Name: "d1", Repo: "org/web", Tree: "te", Files: files}}},
	}, Defaults())
	want := []protocol.Overlap{
		{Mine: "a1", Other: "bob/b1", Files: []string{"b.go", "c.go", "d.go"}, More: 1},
		{Mine: "a3", Other: "carol/c1", Files: []string{"z.go"}},
	}
	if got := x.current("alice"); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("alice's overlaps: %+v", got)
	}
	if got := x.current("dave"); len(got) != 0 {
		t.Fatalf("dave's overlaps: %+v", got)
	}
}

// A check names the agents changing the same files in other checkouts of the repo, on any laptop: not the asking
// checkout's own, not other repos', not offline laptops', and never for files the team ignores.
func TestCheck(t *testing.T) {
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef")}, store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.settings.IgnoredPaths = []string{"traces/**"}
	laptop := func(handle string, agents ...protocol.Agent) *link {
		l := &link{dev: "d-" + handle, handle: handle, email: handle + "@x.com", agents: agents, out: make(chan protocol.Frame, 64), cancel: func() {}}
		h.links[l.dev] = l
		return l
	}
	alice := laptop("alice", protocol.Agent{Name: "a1", Repo: "Org/App", Tree: "t1", Files: []string{"x.go"}},
		protocol.Agent{Name: "a2", Repo: "Org/App", Tree: "t1", Files: []string{"x.go"}},
		protocol.Agent{Name: "a3", Repo: "org/app", Tree: "t3", Files: []string{"x.go"}}) // a worktree of the same repo
	laptop("bob", protocol.Agent{Name: "b1", Repo: "org/app", Branch: "feat/x", Tree: "t2", Files: []string{"traces/run.jsonl", "x.go", "y.go"}})
	laptop("carol", protocol.Agent{Name: "c1", Repo: "org/app", Tree: "t4", Files: []string{"x.go"}}).gone = time.Now()
	laptop("dave", protocol.Agent{Name: "d1", Repo: "org/web", Tree: "t5", Files: []string{"x.go"}})
	h.mu.Unlock()
	h.reindex() // as the next roster does

	h.check(alice, protocol.Frame{T: protocol.FrameCheck, ID: "c1", Repo: "ORG/app", Tree: "t1", Files: []string{"x.go", "traces/run.jsonl", "z.go"}})
	f := <-alice.out
	want := []string{"alice/a3 is also changing x.go", "bob/b1 (feat/x) is also changing x.go"}
	if f.T != protocol.FrameChecked || f.ID != "c1" || strings.Join(f.Overlaps, "|") != strings.Join(want, "|") {
		t.Fatalf("checked: %+v", f)
	}
	h.check(alice, protocol.Frame{T: protocol.FrameCheck, ID: "c2", Repo: "org/app", Tree: "t1", Files: []string{"traces/run.jsonl"}})
	if f := <-alice.out; f.ID != "c2" || len(f.Overlaps) != 0 {
		t.Fatalf("an ignored file overlaps: %+v", f)
	}
}

// Downloads stream the binary and carry its checksum; unknown platforms get a 404.
func TestDownload(t *testing.T) {
	bin := []byte("a murmur binary")
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef"), Version: "v9",
		Dist: fstest.MapFS{"murmur-linux-amd64": {Data: bin}}}, store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	for _, rng := range []string{"", "bytes=0-4"} { // the second uses the remembered checksum, and ignores the range
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/dl/linux/amd64", nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		sum := sha256.Sum256(bin)
		if resp.StatusCode != http.StatusOK || !bytes.Equal(body, bin) || resp.Header.Get("X-Sha256") != hex.EncodeToString(sum[:]) ||
			resp.Header.Get("X-Version") != "v9" {
			t.Fatalf("download: %d %q %v", resp.StatusCode, body, resp.Header)
		}
	}
	if resp, _ := http.Get(srv.URL + "/dl/windows/amd64"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("windows: %d", resp.StatusCode)
	}
}

// Escape codes in what one laptop sends never reach another laptop's terminal or an agent's prompt.
func TestControlCharactersStripped(t *testing.T) {
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
	alice := connect(t, srv.URL, "tok-alice", protocol.Agent{Name: "a1", Title: "\x1b]52;c;cm0gLXJmIH4=\x07Fix \x1b[2Jtests", Status: "busy"})
	bob := connect(t, srv.URL, "tok-bob", protocol.Agent{Name: "b1", Status: "idle"})
	r := bob.expect(func(f protocol.Frame) bool {
		return f.T == "roster" && len(f.People) == 2 && len(f.People[0].Agents) == 1
	})
	if got := r.People[0].Agents[0].Title; got != "Fix tests" {
		t.Fatalf("title reached bob as %q", got)
	}
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m1", From: "a1", To: "bob/b1", Text: "look\x1b]8;;https://evil.example\x07 here"}})
	if m := bob.expect(func(f protocol.Frame) bool { return f.T == "msg" && f.Msg.ID == "m1" }); m.Msg.Text != "look here" {
		t.Fatalf("message reached bob as %q", m.Msg.Text)
	}
	// the recipient's laptop answers with an error: it reaches alice's terminal and agent
	bob.send(protocol.Frame{T: protocol.FrameDelivered, ID: "m1", Err: "\x1b]52;c;cm0gLXJmIH4=\x07\x1b[2Jb1 is busy"})
	if a := alice.expect(func(f protocol.Frame) bool { return f.T == "ack" && f.ID == "m1" }); a.Err != "b1 is busy" {
		t.Fatalf("delivery error reached alice as %q", a.Err)
	}
	// a long error is shortened, never through the middle of a character
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m2", From: "a1", To: "bob/b1", Text: "again"}})
	bob.expect(func(f protocol.Frame) bool { return f.T == "msg" && f.Msg.ID == "m2" })
	bob.send(protocol.Frame{T: protocol.FrameDelivered, ID: "m2", Err: strings.Repeat("a", 299) + "é and more"})
	if a := alice.expect(func(f protocol.Frame) bool { return f.T == "ack" && f.ID == "m2" }); a.Err != strings.Repeat("a", 299) {
		t.Fatalf("long delivery error reached alice as %q", a.Err)
	}
}

// A laptop's frame is cut to 64 agents before the hub compares anything.
func TestAgentsPerLaptopCapped(t *testing.T) {
	st := store.NewMemory()
	_ = st.PutUser(context.Background(), store.User{Email: "alice@x.com", Handle: "alice"})
	_ = st.PutDevice(context.Background(), store.Device{ID: "d-alice", Email: "alice@x.com", TokenHash: hashToken("tok-alice"), Created: time.Now()})
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef")}, st)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	var many []protocol.Agent
	for i := range 100 {
		many = append(many, protocol.Agent{Name: fmt.Sprintf("a%03d", i), Repo: "Org/App", Tree: fmt.Sprintf("t%d", i)})
	}
	alice := connect(t, srv.URL, "tok-alice", many...)
	if r := alice.expect(func(f protocol.Frame) bool {
		return f.T == "roster" && len(f.People) == 1 && len(f.People[0].Agents) > 0
	}); len(r.People[0].Agents) != Defaults().MaxAgents {
		t.Fatalf("%d agents on the roster, want %d", len(r.People[0].Agents), Defaults().MaxAgents)
	}
}

// Laptops choose message ids: odd ones are refused, and a message on its way is answered only on the laptop that
// sent it, on its new connection after a reconnect too.
func TestMessageIDs(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	for _, u := range []store.User{{Email: "alice@x.com", Handle: "alice"}, {Email: "bob@x.com", Handle: "bob"}, {Email: "carol@x.com", Handle: "carol"}} {
		_ = st.PutUser(ctx, u)
		_ = st.PutDevice(ctx, store.Device{ID: "d-" + u.Handle, Email: u.Email, TokenHash: hashToken("tok-" + u.Handle), Created: time.Now()})
	}
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef")}, st)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	alice := connect(t, srv.URL, "tok-alice", protocol.Agent{Name: "a1", Status: "busy"})
	bob := connect(t, srv.URL, "tok-bob", protocol.Agent{Name: "b1", Status: "busy"})
	carol := connect(t, srv.URL, "tok-carol", protocol.Agent{Name: "c1", Status: "busy"})
	for _, id := range []string{"a/b", strings.Repeat("f", 65), "m\n1"} {
		alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: id, From: "a1", To: "bob/b1", Text: "hi"}})
		if a := alice.expect(func(f protocol.Frame) bool { return f.T == "ack" && f.ID == id }); a.OK {
			t.Fatalf("id %q accepted", id)
		}
	}
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m1", From: "a1", To: "bob/b1", Text: "hi"}})
	bob.expect(func(f protocol.Frame) bool { return f.T == "msg" && f.Msg.ID == "m1" && !f.Echo })
	alice = connect(t, srv.URL, "tok-alice", protocol.Agent{Name: "a1", Status: "busy"}) // alice reconnects and sends m1 again
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m1", From: "a1", To: "bob/b1", Text: "hi"}})
	carol.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m1", From: "c1", To: "bob/b1", Text: "same id, another laptop"}})
	time.Sleep(50 * time.Millisecond)
	bob.send(protocol.Frame{T: protocol.FrameDelivered, ID: "m1", OK: true})
	if a := alice.expect(func(f protocol.Frame) bool { return f.T == "ack" && f.ID == "m1" }); !a.OK {
		t.Fatalf("alice's answer: %+v", a)
	}
	for deadline := time.After(200 * time.Millisecond); ; {
		select {
		case f := <-carol.frames:
			if f.T == "ack" && f.ID == "m1" {
				t.Fatal("another laptop took the answer to alice's message")
			}
			continue
		case <-deadline:
		}
		break
	}
}

// After sign-in, people only ever land on a page of this hub.
func TestSafeNext(t *testing.T) {
	for _, n := range []string{"https://evil.example", "//evil.example", "/\t/evil.example", "/\n/evil.example",
		"/\\evil.example", "\\\\evil.example", "javascript:alert(1)", "evil.example", "", "/\x7f/evil.example"} {
		if got := safeNext(n); got != "/" {
			t.Errorf("safeNext(%q) = %q, want /", n, got)
		}
	}
	for _, n := range []string{"/", "/cli/login?port=5555&challenge=abc", "/%0A"} { // an encoded line break stays a path here
		if got := safeNext(n); got != n {
			t.Errorf("safeNext(%q) = %q, want it kept", n, got)
		}
	}
}

// A message held for a laptop that doesn't come back fails to its sender, instead of disappearing.
func TestHeldMessageFailsWhenTheLaptopStaysAway(t *testing.T) {
	st := store.NewMemory()
	for _, u := range []store.User{{Email: "alice@x.com", Handle: "alice"}, {Email: "bob@x.com", Handle: "bob"}} {
		_ = st.PutUser(context.Background(), u)
		_ = st.PutDevice(context.Background(), store.Device{ID: "d-" + u.Handle, Email: u.Email, TokenHash: hashToken("tok-" + u.Handle), Created: time.Now()})
	}
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef")}, st)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	alice := connect(t, srv.URL, "tok-alice", protocol.Agent{Name: "a1", Status: "busy"})
	bob := connect(t, srv.URL, "tok-bob", protocol.Agent{Name: "b1", Status: "idle"})
	_ = bob.c.CloseNow()
	time.Sleep(100 * time.Millisecond) // bob's laptop is reconnecting: messages for b1 are held
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m1", From: "a1", To: "bob/b1", Text: "still there?"}})
	time.Sleep(50 * time.Millisecond)
	h.expire(context.Background(), time.Now().Add(holdFor+time.Second)) // and it never comes back
	if a := alice.expect(func(f protocol.Frame) bool { return f.T == "ack" && f.ID == "m1" }); a.OK || !strings.Contains(a.Err, "went offline") {
		t.Fatalf("ack: %+v", a)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// The hub logs one JSON line per event, as Cloud Logging reads them, and never the text of a message.
func TestEventLog(t *testing.T) {
	st := store.NewMemory()
	for _, u := range []store.User{{Email: "alice@x.com", Handle: "alice"}, {Email: "bob@x.com", Handle: "bob"}} {
		_ = st.PutUser(context.Background(), u)
		_ = st.PutDevice(context.Background(), store.Device{ID: "d-" + u.Handle, Email: u.Email, TokenHash: hashToken("tok-" + u.Handle), Created: time.Now()})
	}
	var logs lockedBuffer
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef"), Log: slog.New(slog.NewJSONHandler(&logs, nil))}, st)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	alice := connect(t, srv.URL, "tok-alice", protocol.Agent{Name: "a1", Status: "busy"})
	bob := connect(t, srv.URL, "tok-bob", protocol.Agent{Name: "b1", Status: "idle"})
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m1", From: "a1", To: "bob/b1", Text: "the secret plan"}})
	bob.expect(func(f protocol.Frame) bool { return f.T == "msg" && f.Msg.ID == "m1" })
	bob.send(protocol.Frame{T: protocol.FrameDelivered, ID: "m1", OK: true})
	alice.expect(func(f protocol.Frame) bool { return f.T == "ack" && f.ID == "m1" })
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m2", From: "a1", To: "dave", Text: "hello"}})
	alice.expect(func(f protocol.Frame) bool { return f.T == "ack" && f.ID == "m2" })
	_ = bob.c.CloseNow()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(logs.String(), "laptop disconnected") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	events := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var e struct {
			Level, Msg string
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil || e.Level == "" {
			t.Fatalf("not a JSON log line: %q", line)
		}
		events[e.Msg]++
	}
	for _, want := range []string{"laptop connected", "message sent", "message delivered", "message refused", "laptop disconnected"} {
		if events[want] == 0 {
			t.Errorf("no %q event in %v", want, events)
		}
	}
	if strings.Contains(logs.String(), "the secret plan") {
		t.Fatal("a message's text reached the logs")
	}
}

// Nobody gets "murmur" as a handle: it signs murmur's own notices, which agents take without an approval.
func TestMurmurIsNotAHandle(t *testing.T) {
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef")}, store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	u, err := h.ensureUser(context.Background(), "murmur@x.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if u.Handle == protocol.System {
		t.Fatalf("handle %q", u.Handle)
	}
}

// A laptop's sign-in expires: then the laptop is refused, and murmur join signs in again through Google.
func TestSignInsExpire(t *testing.T) {
	st := store.NewMemory()
	_ = st.PutUser(context.Background(), store.User{Email: "alice@x.com", Handle: "alice"})
	_ = st.PutDevice(context.Background(), store.Device{ID: "old", Email: "alice@x.com", TokenHash: hashToken("tok-old"), Created: time.Now().Add(-48 * time.Hour)})
	_ = st.PutDevice(context.Background(), store.Device{ID: "new", Email: "alice@x.com", TokenHash: hashToken("tok-new"), Created: time.Now()})
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef"), Seed: seed(func(s *Settings) { s.TokenDays = 1 })}, st)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	if _, resp, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer tok-old"}}}); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("an expired sign-in got in")
	}
	connect(t, srv.URL, "tok-new").expect(func(f protocol.Frame) bool { return f.T == "roster" })
}

// newHub starts a hub on one instance for a test: the team is everyone at x.com unless cfg says otherwise.
func newHub(t *testing.T, cfg Config, st store.Store) (*Hub, error) {
	t.Helper()
	if cfg.Seed.SendsPerMinute == 0 {
		cfg.Seed = seed(nil)
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	h, err := New(context.Background(), cfg, st, bus.NewLocal())
	if err == nil {
		t.Cleanup(h.Close)
	}
	return h, err
}

// seed is the default settings for a team at x.com, changed by f.
func seed(f func(*Settings)) Settings {
	s := Defaults()
	s.AllowedDomains = []string{"x.com"}
	if f != nil {
		f(&s)
	}
	return s
}

// A hub refuses laptops older than the team's oldest accepted version, telling them what to run, and tells the
// others what the team asks of them.
func TestOldestVersion(t *testing.T) {
	st := store.NewMemory()
	_ = st.PutUser(context.Background(), store.User{Email: "alice@x.com", Handle: "alice"})
	_ = st.PutDevice(context.Background(), store.Device{ID: "d-alice", Email: "alice@x.com", TokenHash: hashToken("tok-alice"), Created: time.Now()})
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef"), Version: "1.4.0",
		Seed: seed(func(s *Settings) { s.MinVersion, s.UpdatePolicy = "1.3.0", "notify" })}, st)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	dial := func(version string) (*websocket.Conn, protocol.Frame, error) {
		c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws",
			&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer tok-alice"}}})
		if err != nil {
			t.Fatal(err)
		}
		_ = wsjson.Write(context.Background(), c, protocol.Frame{T: protocol.FrameHello, Protocol: protocol.Version, Version: version})
		var f protocol.Frame
		err = wsjson.Read(context.Background(), c, &f)
		return c, f, err
	}
	_, _, err = dial("1.2.9")
	var closed websocket.CloseError
	if !errors.As(err, &closed) || closed.Code != websocket.StatusPolicyViolation || !strings.Contains(closed.Reason, "1.3.0 or later") {
		t.Fatalf("an old laptop got in: %v", err)
	}
	c, f, err := dial("1.3.0")
	if err != nil || f.T != protocol.FrameWelcome || f.Version != "1.4.0" || f.Team == nil || f.Team.Updates != "notify" || f.Team.MinVersion != "1.3.0" {
		t.Fatalf("welcome: %+v %v", f, err)
	}
	c.CloseNow()
	resp, err := http.Get(srv.URL + "/dl/version")
	if err != nil {
		t.Fatal(err)
	}
	var info map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&info)
	if info["version"] != "1.4.0" || info["min_version"] != "1.3.0" || info["updates"] != "notify" {
		t.Fatalf("version: %v", info)
	}
}

// However much laptops' agents say of themselves, the team list fits in what a laptop reads, and keeps the most
// recently active agents.
func TestRosterFitsALaptop(t *testing.T) {
	long := func(n int) string { return strings.Repeat("<", n) } // six bytes each in JSON
	worst := protocol.Agent{Name: long(500), Repo: long(500), Branch: long(500), Title: long(500), PRURL: long(500),
		App: long(500), Status: long(500), Needs: long(500), Doing: long(500)}
	worst.Clean()
	if b, _ := json.Marshal(worst); len(b) > worstSize(worst) {
		t.Fatalf("an agent takes %d bytes, more than its bound of %d", len(b), worstSize(worst))
	}
	h, err := newHub(t, Config{}, store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.users["bob@x.com"] = &store.User{Email: "bob@x.com", Handle: "bob"}
	for i := range 4000 {
		a := worst
		a.Name, a.Since = fmt.Sprint(i), int64(i)
		dev := fmt.Sprint("d", i/1000)
		if h.remote[dev] == nil {
			h.remote[dev] = &presence{Device: dev, Email: "bob@x.com", Handle: "bob", Online: true}
		}
		h.remote[dev].Agents = append(h.remote[dev].Agents, a)
	}
	people := h.rosterLocked()
	b, _ := json.Marshal(protocol.Frame{T: protocol.FrameRoster, People: people})
	if len(b) > protocol.LaptopReadLimit {
		t.Fatalf("the team list takes %d bytes, more than a laptop reads", len(b))
	}
	if n := len(people[0].Agents); n == 4000 || !slices.ContainsFunc(people[0].Agents, func(a protocol.Agent) bool { return a.Name == "3999" }) ||
		slices.ContainsFunc(people[0].Agents, func(a protocol.Agent) bool { return a.Name == "0" }) {
		t.Fatalf("the team list kept %d agents, not the most recent ones", n)
	}
}

// Everyone's colour comes from when they joined, so every screen shows each person in the same colour, and a
// removed person's colour doesn't pass to someone else.
func TestColours(t *testing.T) {
	h, err := newHub(t, Config{}, store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, n := range []string{"zoe", "bob", "ann", "gone"} {
		h.users[n+"@x.com"] = &store.User{Email: n + "@x.com", Handle: n, Created: t0.Add(time.Duration(i) * time.Hour), Disabled: n == "gone"}
	}
	h.users["late@x.com"] = &store.User{Email: "late@x.com", Handle: "late", Created: t0.Add(10 * time.Hour)}
	got := map[string]int{}
	for _, p := range h.rosterLocked() {
		got[p.Handle] = p.Colour
	}
	if got["zoe"] != 0 || got["bob"] != 1 || got["ann"] != 2 || got["late"] != 4 {
		t.Fatalf("colours %v", got)
	}
}

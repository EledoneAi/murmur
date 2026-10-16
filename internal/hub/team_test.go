package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/internal/protocol"
	"github.com/EledoneAi/murmur/internal/store"
)

// stream is the team page's stream, read as the page's script reads it.
type stream struct {
	t      *testing.T
	events chan string
}

func openStream(t *testing.T, c *http.Client, hub string) *stream {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, hub+"/team/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	t.Cleanup(func() { resp.Body.Close() })
	s := &stream{t: t, events: make(chan string, 100)}
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				s.events <- d
			}
		}
		close(s.events)
	}()
	return s
}

// expect returns the first team the stream sends that ok accepts, and the event as it was sent.
func (s *stream) expect(ok func(teamView) bool) (teamView, string) {
	s.t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case d, open := <-s.events:
			if !open {
				s.t.Fatal("the stream closed")
			}
			var v teamView
			if err := json.Unmarshal([]byte(d), &v); err != nil {
				s.t.Fatalf("not a team: %q", d)
			}
			if ok(v) {
				return v, d
			}
		case <-timeout:
			s.t.Fatal("timed out waiting for the stream")
		}
	}
}

// agentIn finds an agent in a team, by its address.
func agentIn(v teamView, addr string) (protocol.Agent, bool) {
	handle, name := protocol.Split(addr)
	for _, p := range v.People {
		for _, a := range p.Agents {
			if p.Handle == handle && a.Name == name {
				return a, true
			}
		}
	}
	return protocol.Agent{}, false
}

// sendFrom sends a message from the team page, as the page's script does, and returns what it says.
func sendFrom(t *testing.T, c *http.Client, hub, to, text string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, hub+"/team/send", strings.NewReader(url.Values{"to": {to}, "text": {text}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// quiet fails when a laptop gets a frame ok accepts within a moment.
func (l *laptop) quiet(ok func(protocol.Frame) bool) {
	l.t.Helper()
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case f, open := <-l.frames:
			if open && ok(f) {
				l.t.Fatalf("unexpected frame: %+v", f)
			}
			l.backlog = append(l.backlog, f)
		case <-deadline:
			if slices.ContainsFunc(l.backlog, ok) {
				l.t.Fatal("unexpected frame")
			}
			return
		}
	}
}

// eventually fails unless ok holds within a few seconds.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
	}
}

// Signed-out visitors get the sign-in page, which brings them back to the team page; the page's stream, its
// pull requests and its form refuse them.
func TestTeamPageIsForMembers(t *testing.T) {
	tm := newTeam(t, Config{}, nil)
	visitor, _ := signedIn(t, tm.url, "")
	resp, err := visitor.Get(tm.url + "/team")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound || loc != "/login?next=%2Fteam" {
		t.Fatalf("signed out: %d to %q", resp.StatusCode, loc)
	}
	resp, err = visitor.Get(tm.url + loc) // a local hub signs in by email, on its front page
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || loc != "/?next=%2Fteam" {
		t.Fatalf("not the sign-in page: %d to %q", resp.StatusCode, loc)
	}
	if status, me := tm.get(t, visitor, "/api/me"); status != http.StatusUnauthorized || !strings.Contains(me, `"dev":true`) {
		t.Fatalf("signed-out me: %d %s", status, me)
	}
	req, _ := http.NewRequest(http.MethodGet, tm.url+"/team/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	if resp, _ := visitor.Do(req); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("signed-out stream: %d", resp.StatusCode)
	}
	if status, _ := tm.get(t, visitor, "/team/prs"); status != http.StatusUnauthorized {
		t.Fatalf("signed-out pull requests: %d", status)
	}
	if status, out := sendFrom(t, visitor, tm.url, "alice", "hi"); status != http.StatusUnauthorized || out["ok"] != false {
		t.Fatalf("signed-out send: %d %v", status, out)
	}
	if status, loc, _ := tm.post(t, visitor, "/team/send", url.Values{"to": {"alice"}, "text": {"hi"}}); status != http.StatusFound || loc != "/login?next=%2Fteam" {
		t.Fatalf("signed-out form: %d to %q", status, loc)
	}
	if status, me := tm.get(t, tm.bob, "/api/me"); status != http.StatusOK || !strings.Contains(me, `"handle":"bob"`) {
		t.Fatalf("bob's me: %d %s", status, me)
	}
}

// The team page shows every person and their agents, waiting ones first with how long they have waited, and stays
// live: a change reaches an open stream.
func TestTeamPage(t *testing.T) {
	tm := newTeam(t, Config{}, nil)
	ago := func(d time.Duration) int64 { return time.Now().Add(-d).UnixMilli() }
	connect(t, tm.url, "tok-alice",
		protocol.Agent{Name: "api-1", Repo: "acme/api", Branch: "feat/billing", Title: "Rerun billing on staging", PR: 1822,
			PRURL: "https://github.com/acme/api/pull/1822", Status: protocol.StatusBusy, Doing: "Editing invoice.go", Since: ago(12 * time.Minute)},
		protocol.Agent{Name: "web-1", Repo: "acme/web", Branch: "main", Title: "Search by designation", Status: protocol.StatusWaiting,
			Needs: "permission", Since: ago(3 * time.Minute)})
	s := openStream(t, tm.bob, tm.url)
	s.expect(func(v teamView) bool { _, ok := agentIn(v, "alice/web-1"); return ok })

	if status, page := tm.get(t, tm.bob, "/team"); status != http.StatusOK || !strings.Contains(page, "<title>Team · murmur</title>") {
		t.Fatalf("the team page: %d", status)
	}
	_, body := tm.get(t, tm.bob, "/api/team")
	var v teamView
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.People) < 2 || v.People[0].Handle != "alice" || len(v.People[0].Agents) != 2 {
		t.Fatalf("the person with a waiting agent isn't first: %+v", v.People)
	}
	if a := v.People[0].Agents[0]; a.Name != "web-1" || a.Needs != "permission" {
		t.Errorf("the waiting agent isn't first: %+v", a)
	}
	if a := v.People[0].Agents[1]; a.Title != "Rerun billing on staging" || a.PRURL != "https://github.com/acme/api/pull/1822" || a.Doing != "Editing invoice.go" {
		t.Errorf("api-1: %+v", a)
	}

	// bob's laptop connects: the open page hears of it
	connect(t, tm.url, "tok-bob", protocol.Agent{Name: "b1", Repo: "acme/api", Status: protocol.StatusIdle})
	v, _ = s.expect(func(v teamView) bool { _, ok := agentIn(v, "bob/b1"); return ok })
	if v.Me != "bob" || !slices.Equal(v.Mine, []string{"b1"}) || !slices.Equal(v.Repos, []string{"acme/api", "acme/web"}) {
		t.Fatalf("team: me %q, mine %v, repos %v", v.Me, v.Mine, v.Repos)
	}
}

// Another instance's laptop reaches a team page open on this one, through the bus.
func TestTeamPageAcrossInstances(t *testing.T) {
	one, two := twoInstances(t)
	s := openStream(t, one.bob, one.url)
	alice := connect(t, two.url, "tok-alice", protocol.Agent{Name: "a1", Repo: "org/app", Status: protocol.StatusBusy})
	s.expect(func(v teamView) bool { _, ok := agentIn(v, "alice/a1"); return ok })

	// and a message from the page on one reaches alice's agent on two, whose answer comes back to the page
	sent := make(chan map[string]any, 1)
	go func() {
		_, out := sendFrom(t, one.bob, one.url, "alice/a1", "can you look at the routes?")
		sent <- out
	}()
	m := alice.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && !f.Echo && f.Msg.To == "alice/a1" })
	if m.Msg.From != "bob" || m.Msg.Text != "can you look at the routes?" {
		t.Fatalf("message: %+v", m.Msg)
	}
	alice.send(protocol.Frame{T: protocol.FrameDelivered, ID: m.Msg.ID, OK: true})
	if out := <-sent; out["ok"] != true || out["result"] != "Delivered." {
		t.Fatalf("the page heard %v", out)
	}
}

// instance is one of two instances of a hub, with bob signed in to a browser on it.
type instance struct {
	h   *Hub
	url string
	bob *http.Client
}

// twoInstances starts two instances of one hub on one store and one bus, where alice, bob and carol are on the team.
func twoInstances(t *testing.T) (*instance, *instance) {
	t.Helper()
	st := store.NewMemory()
	ctx := context.Background()
	for _, h := range []string{"alice", "bob", "carol"} {
		_ = st.PutUser(ctx, store.User{Email: h + "@x.com", Handle: h})
		_ = st.PutDevice(ctx, store.Device{ID: "d-" + h, Email: h + "@x.com", TokenHash: hashToken("tok-" + h), Created: time.Now()})
	}
	b := &fanout{}
	start := func(name string) *instance {
		h, err := New(ctx, Config{Secret: []byte("0123456789abcdef"), Seed: seed(nil), Instance: name, DevLogin: true,
			PublicURL: "http://localhost", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, st, b.open())
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(h.Handler())
		t.Cleanup(func() { srv.Close(); h.Close() })
		bob, _ := signedIn(t, srv.URL, "bob@x.com")
		return &instance{h: h, url: srv.URL, bob: bob}
	}
	return start("one"), start("two")
}

// What laptops send is text on the page and data in the stream, never markup, and links from laptops are web
// addresses only.
func TestTeamPageEscapes(t *testing.T) {
	tm := newTeam(t, Config{}, nil)
	connect(t, tm.url, "tok-alice", protocol.Agent{Name: "a1", Repo: "acme/api", Title: `<script>alert("t")</script>`, PR: 7,
		PRURL: "javascript:alert(1)", Status: protocol.StatusBusy, Doing: `<img src=x onerror=alert(1)>`})
	s := openStream(t, tm.bob, tm.url)
	v, raw := s.expect(func(v teamView) bool { _, ok := agentIn(v, "alice/a1"); return ok })
	if a, _ := agentIn(v, "alice/a1"); a.Title != `<script>alert("t")</script>` || a.PRURL != "" {
		t.Fatalf("agent in the stream: %+v", a)
	}
	escaped, _ := json.Marshal(`<script>alert("t")</script>`) // as data: <, > and & escaped
	if strings.Contains(raw, "<script>") || strings.Contains(raw, "<img") || !strings.Contains(raw, string(escaped)) || strings.Contains(raw, "javascript:") {
		t.Fatalf("the stream carries markup: %s", raw)
	}
	_, page := tm.get(t, tm.bob, "/api/team") // what the page draws, always as text
	if strings.Contains(page, `<script>alert`) || strings.Contains(page, "<img") || strings.Contains(page, "javascript:") ||
		!strings.Contains(page, `\u003cscript\u003ealert(\"t\")\u003c/script\u003e`) {
		t.Fatalf("the page's data carries markup from a laptop: %s", page)
	}
	if !strings.Contains(page, `"pr":7`) {
		t.Fatal("the pull request lost its number with its link")
	}
}

// A person sends from the team page through the same checks and limits as from their laptop: the page says whether
// it was delivered, kept for later or refused, and why.
func TestSendFromTheTeamPage(t *testing.T) {
	tm := newTeam(t, Config{Seed: seed(func(s *Settings) { s.SendsPerMinute = 4 })}, nil)

	// alice is offline: kept for her
	if status, out := sendFrom(t, tm.bob, tm.url, "alice", "lunch?"); status != http.StatusOK || out["result"] != "Alice is offline and gets it when murmur is back on their laptop." {
		t.Fatalf("to someone offline: %d %v", status, out)
	}
	alice := connect(t, tm.url, "tok-alice", protocol.Agent{Name: "a1", Status: protocol.StatusIdle})
	if m := alice.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && f.Msg.Text == "lunch?" }); m.Msg.From != "bob" || m.Echo {
		t.Fatalf("kept message: %+v", m)
	}

	// her agent: delivered, and bob's laptop gets a copy for his console
	bob := connect(t, tm.url, "tok-bob")
	sent := make(chan map[string]any, 1)
	go func() { _, out := sendFrom(t, tm.bob, tm.url, "alice/a1", "the routes moved"); sent <- out }()
	m := alice.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && f.Msg.Text == "the routes moved" })
	if m.Echo || m.Msg.From != "bob" || m.Msg.To != "alice/a1" {
		t.Fatalf("delivery: %+v", m)
	}
	bob.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && f.Echo && f.Msg.ID == m.Msg.ID })
	alice.send(protocol.Frame{T: protocol.FrameDelivered, ID: m.Msg.ID, OK: true})
	if out := <-sent; out["ok"] != true || out["result"] != "Delivered." {
		t.Fatalf("the page heard %v", out)
	}

	// refused: nobody by that name, an agent that isn't running, the limit bob's laptop shares with the page
	if status, out := sendFrom(t, tm.bob, tm.url, "dave", "hi"); status != http.StatusBadRequest || !strings.Contains(out["result"].(string), `Not sent: nobody called "dave"`) {
		t.Fatalf("unknown person: %d %v", status, out)
	}
	if _, out := sendFrom(t, tm.bob, tm.url, "alice/a9", "hi"); out["ok"] != false || !strings.Contains(out["result"].(string), "not running") {
		t.Fatalf("stopped agent: %v", out)
	}
	bob.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m1", To: "alice", Text: "from the console"}})
	alice.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && f.Msg.ID == "m1" })
	alice.send(protocol.Frame{T: protocol.FrameDelivered, ID: "m1", OK: true})
	if a := bob.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameAck && f.ID == "m1" }); !a.OK {
		t.Fatalf("the laptop's own message: %+v", a)
	}
	if _, out := sendFrom(t, tm.bob, tm.url, "alice", "one more"); out["ok"] != false || !strings.Contains(out["result"].(string), "too many messages") {
		t.Fatalf("over the limit: %v", out)
	}

	// a refused message says why
	if status, out := sendFrom(t, tm.alice, tm.url, "dave", "are you there?"); status != http.StatusBadRequest ||
		!strings.Contains(out["result"].(string), "Not sent: nobody called") {
		t.Fatalf("refused: %d %v", status, out)
	}
}

// The pull request list shows every pull request the team's agents opened, oldest first, and lets anyone with a
// running agent ask one of theirs to review it.
func TestPullRequests(t *testing.T) {
	tm := newTeam(t, Config{}, func(ctx context.Context, st store.Store) {
		_ = st.PutUser(ctx, store.User{Email: "carol@x.com", Handle: "carol"})
	})
	ago := func(d time.Duration) int64 { return time.Now().Add(-d).UnixMilli() }
	connect(t, tm.url, "tok-alice",
		protocol.Agent{Name: "a1", Repo: "org/app", Branch: "feat/a", Title: "Billing", PR: 12, PRURL: "https://github.com/org/app/pull/12", Status: protocol.StatusIdle, Since: ago(3 * time.Hour)},
		protocol.Agent{Name: "a2", Repo: "org/app", Branch: "feat/b", PR: 7, PRURL: "https://github.com/org/app/pull/7", Status: protocol.StatusBusy, Since: ago(time.Hour)},
		protocol.Agent{Name: "a3", Repo: "org/app", Status: protocol.StatusBusy})
	bob := connect(t, tm.url, "tok-bob",
		protocol.Agent{Name: "b1", Repo: "org/web", Branch: "fix/c", PR: 30, PRURL: "https://github.com/org/web/pull/30", Status: protocol.StatusIdle, Since: ago(2 * time.Hour)})
	bob.expect(func(f protocol.Frame) bool {
		return f.T == protocol.FrameRoster && len(f.People) == 3 && len(f.People[0].Agents) == 3 && len(f.People[1].Agents) == 1
	})

	status, body := tm.get(t, tm.bob, "/team/prs")
	var prs []pullRequest
	if err := json.Unmarshal([]byte(body), &prs); err != nil || status != http.StatusOK {
		t.Fatalf("pull requests: %d %s", status, body)
	}
	var names []string
	for _, p := range prs {
		names = append(names, p.Person+"/"+p.Agent+" "+p.Name)
	}
	if !slices.Equal(names, []string{"alice/a1 org/app#12", "bob/b1 org/web#30", "alice/a2 org/app#7"}) {
		t.Fatalf("pull requests, oldest first: %q", names)
	}
	review := "Please review https://github.com/org/app/pull/12 with gh pr view and gh pr diff, and send me what you find."
	if p := prs[0]; p.Branch != "feat/a" || p.Repo != "org/app" || p.Title != "Billing" || p.Review != review {
		t.Fatalf("pull request: %+v", p)
	}
	// a laptop reads them with its token
	req, _ := http.NewRequest(http.MethodGet, tm.url+"/team/prs", nil)
	req.Header.Set("Authorization", "Bearer tok-alice")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("with a laptop's token: %v %v", resp, err)
	}

	// bob has a running agent: he can ask it to review. carol has none: she sees the list only.
	var v teamView
	_, body = tm.get(t, tm.bob, "/api/team")
	_ = json.Unmarshal([]byte(body), &v)
	if !slices.Equal(v.Mine, []string{"b1"}) || len(v.PRs) < 2 || v.PRs[0].Name != "org/app#12" || v.PRs[1].Name != "org/web#30" {
		t.Fatalf("bob can't ask his agent to review, or the list isn't oldest first: %v %+v", v.Mine, v.PRs)
	}
	carol, _ := signedIn(t, tm.url, "carol@x.com")
	if _, page := tm.get(t, carol, "/api/team"); !strings.Contains(page, `"mine":[]`) || !strings.Contains(page, `"name":"org/app#12"`) {
		t.Fatalf("carol, without a running agent, could ask one to review: %s", page)
	}
	sent := make(chan map[string]any, 1)
	go func() { _, out := sendFrom(t, tm.bob, tm.url, "bob/b1", review); sent <- out }()
	m := bob.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && !f.Echo && f.Msg.To == "bob/b1" })
	if m.Msg.From != "bob" || m.Msg.Text != review {
		t.Fatalf("review request: %+v", m.Msg)
	}
	bob.send(protocol.Frame{T: protocol.FrameDelivered, ID: m.Msg.ID, OK: true})
	if out := <-sent; out["ok"] != true {
		t.Fatalf("review request: %v", out)
	}
}

// An announcement to a repo reaches every running agent working in it, on every instance, and no other; the sender
// hears how many it reached, and every instance's team page lists it.
func TestAnnouncements(t *testing.T) {
	one, two := twoInstances(t)
	alice := connect(t, one.url, "tok-alice", protocol.Agent{Name: "a1", Repo: "Org/App", Status: protocol.StatusBusy},
		protocol.Agent{Name: "a2", Repo: "org/other", Status: protocol.StatusIdle})
	carol := connect(t, one.url, "tok-carol", protocol.Agent{Name: "c1", Repo: "org/app", Status: protocol.StatusIdle})
	bob := connect(t, two.url, "tok-bob", protocol.Agent{Name: "b1", Repo: "org/app", Status: protocol.StatusIdle},
		protocol.Agent{Name: "b2", Repo: "org/web", Status: protocol.StatusIdle})
	alice.expect(func(f protocol.Frame) bool {
		i := slices.IndexFunc(f.People, func(p protocol.Person) bool { return p.Handle == "bob" })
		return f.T == protocol.FrameRoster && i >= 0 && len(f.People[i].Agents) == 2
	})
	copyFor := func(to string) func(protocol.Frame) bool {
		return func(f protocol.Frame) bool {
			return f.T == protocol.FrameMsg && !f.Echo && f.Msg.Announce != "" && f.Msg.To == to
		}
	}

	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "an1", From: "a1", To: "repo:org/app", Text: "main is frozen until 3pm"}})
	if a := alice.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameAck && f.ID == "an1" }); !a.OK || a.Note != "reached 2 agents in org/app" {
		t.Fatalf("ack: %+v", a)
	}
	for l, to := range map[*laptop]string{bob: "bob/b1", carol: "carol/c1"} {
		m := l.expect(copyFor(to))
		if m.Msg.From != "alice/a1" || m.Msg.Text != "main is frozen until 3pm" || m.Msg.Announce != "org/app" || !strings.HasPrefix(m.Msg.ID, "an-") {
			t.Fatalf("copy for %s: %+v", to, m.Msg)
		}
	}
	alice.quiet(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && !f.Echo && f.Msg.Announce != "" })
	bob.quiet(copyFor("bob/b2"))

	// from the page on instance two: alice's agent gets it too, and both pages list both announcements
	if _, out := sendFrom(t, two.bob, two.url, "repo:org/app", "staging is down"); out["ok"] != true || out["result"] != "Reached 3 agents in org/app." {
		t.Fatalf("from the page: %v", out)
	}
	if m := alice.expect(copyFor("alice/a1")); m.Msg.From != "bob" {
		t.Fatalf("copy for alice/a1: %+v", m.Msg)
	}
	for _, in := range []*instance{one, two} {
		eventually(t, "an instance doesn't list both announcements", func() bool {
			_, page := (&team{url: in.url}).get(t, in.bob, "/api/team")
			return strings.Contains(page, "main is frozen until 3pm") && strings.Contains(page, "staging is down") &&
				strings.Contains(page, `"Org/App"`)
		})
	}
	eventually(t, "no announcement in the event log", func() bool {
		evs, _ := one.h.store.Events(context.Background(), 100)
		return slices.ContainsFunc(evs, func(e store.Event) bool {
			return e.Kind == "announced" && e.Actor == "alice/a1" && e.Subject == "org/app" && e.Detail == "reached 2 agents"
		})
	})

	// a repo nobody works in is refused
	if _, out := sendFrom(t, one.bob, one.url, "repo:org/nope", "hello?"); out["ok"] != false || !strings.Contains(out["result"].(string), "nobody works in") {
		t.Fatalf("unknown repo: %v", out)
	}
}

// Announcements count toward their sender's messages per minute.
func TestAnnouncementsAreLimited(t *testing.T) {
	tm := newTeam(t, Config{Seed: seed(func(s *Settings) { s.SendsPerMinute = 2 })}, nil)
	alice := connect(t, tm.url, "tok-alice", protocol.Agent{Name: "a1", Repo: "org/app", Status: protocol.StatusBusy})
	for i, m := range []protocol.Msg{{ID: "n1", To: "repo:org/app", Text: "one"}, {ID: "n2", To: "bob", Text: "two"}, {ID: "n3", To: "repo:org/app", Text: "three"}} {
		alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &m})
		if a := alice.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameAck && f.ID == m.ID }); a.OK != (i < 2) {
			t.Fatalf("%s: %+v", m.ID, a)
		}
	}
}

// Announcements stay up for the hours the team chose, also after a restart: they are in the store.
func TestAnnouncementsExpire(t *testing.T) {
	tm := newTeam(t, Config{}, func(ctx context.Context, st store.Store) {
		_ = st.Put(ctx, newsKey, mustJSON([]announcement{
			{ID: "new", From: "alice", Repo: "org/app", Text: "deploys are paused", At: time.Now().Add(-2 * time.Hour)},
			{ID: "old", From: "alice", Repo: "org/app", Text: "staging is down", At: time.Now().Add(-25 * time.Hour)},
		}))
	})
	if _, page := tm.get(t, tm.bob, "/api/team"); !strings.Contains(page, "deploys are paused") || strings.Contains(page, "staging is down") {
		t.Fatal("the page doesn't show only the announcements still up")
	}
	s := openStream(t, tm.bob, tm.url)
	if v, _ := s.expect(func(teamView) bool { return true }); len(v.News) != 1 || v.News[0].Until-v.News[0].At != (24*time.Hour).Milliseconds() {
		t.Fatalf("news in the stream: %+v", v.News)
	}
	set := tm.h.Settings()
	set.AnnounceHours = 1
	if err := tm.h.SaveSettings(context.Background(), set, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, page := tm.get(t, tm.bob, "/api/team"); strings.Contains(page, "deploys are paused") || !strings.Contains(page, `"news":[]`) {
		t.Fatal("an announcement stayed up longer than the team allows")
	}
}

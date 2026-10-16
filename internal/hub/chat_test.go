package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/store"
)

// chatLog is what a hub posted to a generic chat.
type chatLog struct {
	mu    sync.Mutex
	posts []string
}

func (c *chatLog) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.posts...)
}

func chatServer(t *testing.T) (*httptest.Server, *chatLog) {
	c := &chatLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var body struct{ Text string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		c.posts = append(c.posts, body.Text)
		c.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

// chatTo points a hub's chat at a test server, whose http:// address the settings would refuse.
func chatTo(h *Hub, url, kind string) {
	h.mu.Lock()
	h.settings.ChatWebhook, h.settings.ChatKind = url, kind
	h.mu.Unlock()
}

// place puts a laptop with these agents on a hub, as if it had connected.
func place(h *Hub, handle string, agents ...protocol.Agent) *link {
	l := &link{dev: "d-" + handle, handle: handle, email: handle + "@x.com", agents: agents,
		out: make(chan protocol.Frame, 256), cancel: func() {}}
	h.mu.Lock()
	h.links[l.dev] = l
	h.mu.Unlock()
	return l
}

func setAgents(h *Hub, l *link, agents ...protocol.Agent) {
	h.mu.Lock()
	l.agents = agents
	h.mu.Unlock()
}

// Each chat gets the text in the format its incoming webhooks take.
func TestChatPayloads(t *testing.T) {
	text := "alice's agent <a1> & @everyone\nsecond line"
	block := func(text string) map[string]any {
		return map[string]any{"type": "TextBlock", "text": text, "wrap": true}
	}
	for kind, want := range map[string]any{
		"slack":       map[string]any{"text": "alice's agent &lt;a1&gt; &amp; @everyone\nsecond line"},
		"google-chat": map[string]any{"text": "alice's agent ‹a1› & @everyone\nsecond line"},
		"generic":     map[string]any{"text": "alice's agent ‹a1› & @everyone\nsecond line"},
		"discord":     map[string]any{"content": text, "allowed_mentions": map[string]any{"parse": []any{}}},
		"teams": map[string]any{"type": "message", "attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"content": map[string]any{"$schema": "http://adaptivecards.io/schemas/adaptive-card.json", "type": "AdaptiveCard",
				"version": "1.2", "body": []any{block("alice's agent ‹a1› & @everyone"), block("second line")}},
		}}},
	} {
		var got, ctype string
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			var v any
			_ = json.NewDecoder(r.Body).Decode(&v)
			got, ctype = string(mustJSON(v)), r.Header.Get("Content-Type")
		}))
		h, err := newHub(t, Config{Secret: []byte("0123456789abcdef")}, store.NewMemory())
		if err != nil {
			t.Fatal(err)
		}
		chatTo(h, srv.URL, kind)
		if err := h.post(context.Background(), text); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		srv.Close()
		if got != string(mustJSON(want)) || ctype != "application/json" {
			t.Errorf("%s: posted %s (%s)\nwant %s", kind, got, ctype, want)
		}
	}
	if b := chatBody("discord", strings.Repeat("é", 2500)); !strings.Contains(string(b), `"content":"`+strings.Repeat("é", 2000)+`"`) {
		t.Error("a long post isn't cut to Discord's 2000 characters")
	}
}

// A chat that fails is logged once, without the webhook's address: it carries the chat's secret.
func TestChatFailure(t *testing.T) {
	var logs lockedBuffer
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef"), Log: slog.New(slog.NewJSONHandler(&logs, nil))}, store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	chatTo(h, srv.URL+"/hooks/T0/the-secret", "slack")
	if err := h.post(context.Background(), "hi"); err == nil || calls != 1 {
		t.Fatalf("post to a failing chat: %v after %d calls", err, calls)
	}
	srv.Close()
	if err := h.post(context.Background(), "hi"); err == nil {
		t.Fatal("post to a chat that is gone succeeded")
	}
	if !strings.Contains(logs.String(), "post to the team's chat") || strings.Contains(logs.String(), "the-secret") {
		t.Fatalf("logs: %s", logs.String())
	}
}

// pingHub is a hub in Paris, working 9 to 18 on weekdays, that pings after 15 minutes.
func pingHub(t *testing.T, st store.Store, instance string, chat string, f func(*Settings)) *Hub {
	t.Helper()
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef"), PublicURL: "https://hub.example.com", Instance: instance,
		Seed: seed(func(s *Settings) {
			s.TimeZone, s.WorkStart, s.WorkEnd, s.WaitPingMinutes = "Europe/Paris", 9, 18, 15
			if f != nil {
				f(s)
			}
		})}, st)
	if err != nil {
		t.Fatal(err)
	}
	chatTo(h, chat, "generic")
	return h
}

func paris(t *testing.T, s string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.ParseInLocation("2006-01-02 15:04:05", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func waiting(name string, since time.Time) protocol.Agent {
	return protocol.Agent{Name: name, Repo: "acme/api", Branch: "login", Status: protocol.StatusWaiting, Since: since.UnixMilli()}
}

// A wait is pinged once, after WaitPingMinutes, and another instance taking the lease doesn't ping it again.
func TestWaitPingOncePerWait(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	srv, chat := chatServer(t)
	one := pingHub(t, st, "one", srv.URL, nil)
	l := place(one, "alice", waiting("a1", paris(t, "2026-10-14 10:00:00"))) // a Wednesday
	for _, at := range []string{"10:10:00", "10:14:30"} {
		one.followTeam(ctx, paris(t, "2026-10-14 "+at))
	}
	if got := chat.all(); len(got) != 0 {
		t.Fatalf("pinged too early: %q", got)
	}
	one.followTeam(ctx, paris(t, "2026-10-14 10:15:00"))
	want := "alice's agent a1 (acme/api · login) has waited 15 min for them: https://hub.example.com"
	if got := chat.all(); len(got) != 1 || got[0] != want {
		t.Fatalf("pings: %q\nwant %q", got, want)
	}
	one.followTeam(ctx, paris(t, "2026-10-14 10:15:30"))
	one.followTeam(ctx, paris(t, "2026-10-14 11:00:00")) // after a gap, as if the lease had gone and come back
	two := pingHub(t, st, "two", srv.URL, nil)
	place(two, "alice", waiting("a1", paris(t, "2026-10-14 10:00:00")))
	two.followTeam(ctx, paris(t, "2026-10-14 11:00:30"))
	if got := chat.all(); len(got) != 1 {
		t.Fatalf("the same wait pinged again: %q", got)
	}

	// a new wait is a new ping
	setAgents(one, l, waiting("a1", paris(t, "2026-10-14 11:10:00")))
	place(two, "alice", waiting("a1", paris(t, "2026-10-14 11:10:00")))
	two.followTeam(ctx, paris(t, "2026-10-14 11:31:00"))
	if got := chat.all(); len(got) != 2 || !strings.Contains(got[1], "has waited 21 min") {
		t.Fatalf("pings: %q", got)
	}

	// a session asking to approve a tool while busy waits from when it is seen, on each instance: the other still
	// knows it was pinged
	approve := protocol.Agent{Name: "b1", Status: protocol.StatusBusy, Needs: "permission", Since: paris(t, "2026-10-14 13:00:00").UnixMilli()}
	place(one, "bob", approve)
	follow(one, paris(t, "2026-10-14 13:05:00"), paris(t, "2026-10-14 13:20:00"))
	place(two, "bob", approve)
	follow(two, paris(t, "2026-10-14 13:40:00"), paris(t, "2026-10-14 13:55:00"))
	if got := chat.all(); len(got) != 3 || !strings.HasPrefix(got[2], "bob's agent b1 has waited 15 min") {
		t.Fatalf("pings: %q", got)
	}
}

// Nobody is pinged between the end of the working day and its start, nor at the weekend: a wait that still holds is
// pinged when the day starts.
func TestWaitPingWorkingHours(t *testing.T) {
	ctx := context.Background()
	srv, chat := chatServer(t)
	h := pingHub(t, store.NewMemory(), "", srv.URL, nil)
	place(h, "alice", waiting("a1", paris(t, "2026-10-14 18:30:00")))
	for _, at := range []string{"2026-10-14 18:50:00", "2026-10-14 23:00:00", "2026-10-15 03:00:00", "2026-10-15 08:59:30"} {
		h.followTeam(ctx, paris(t, at))
	}
	if got := chat.all(); len(got) != 0 {
		t.Fatalf("pinged at night: %q", got)
	}
	h.followTeam(ctx, paris(t, "2026-10-15 09:00:00"))
	if got := chat.all(); len(got) != 1 || !strings.Contains(got[0], "has waited 14 h 30 min for them") {
		t.Fatalf("morning ping: %q", got)
	}

	srv, chat = chatServer(t)
	h = pingHub(t, store.NewMemory(), "", srv.URL, nil)
	place(h, "bob", waiting("b1", paris(t, "2026-10-17 10:00:00"))) // a Saturday
	for _, at := range []string{"2026-10-17 10:30:00", "2026-10-18 15:00:00", "2026-10-19 08:00:00"} {
		h.followTeam(ctx, paris(t, at))
	}
	if got := chat.all(); len(got) != 0 {
		t.Fatalf("pinged at the weekend: %q", got)
	}
	h.followTeam(ctx, paris(t, "2026-10-19 09:00:00"))
	if got := chat.all(); len(got) != 1 || !strings.HasPrefix(got[0], "bob's agent b1") {
		t.Fatalf("monday ping: %q", got)
	}

	srv, chat = chatServer(t)
	h = pingHub(t, store.NewMemory(), "", srv.URL, func(s *Settings) { s.Weekdays = false })
	place(h, "bob", waiting("b1", paris(t, "2026-10-17 10:00:00")))
	h.followTeam(ctx, paris(t, "2026-10-17 10:30:00"))
	if got := chat.all(); len(got) != 1 {
		t.Fatalf("a team working at the weekend wasn't pinged: %q", got)
	}

	srv, chat = chatServer(t)
	h = pingHub(t, store.NewMemory(), "", srv.URL, func(s *Settings) { s.WaitPingMinutes = 0 })
	place(h, "bob", waiting("b1", paris(t, "2026-10-14 10:00:00")))
	h.followTeam(ctx, paris(t, "2026-10-14 16:00:00"))
	if got := chat.all(); len(got) != 0 {
		t.Fatalf("pinged with pings off: %q", got)
	}
}

func TestMinutes(t *testing.T) {
	for m, want := range map[int]string{0: "under a minute", 1: "1 min", 59: "59 min", 60: "1 h", 130: "2 h 10 min"} {
		if got := minutes(m); got != want {
			t.Errorf("minutes(%d) = %q, want %q", m, got, want)
		}
	}
	if s := (Settings{WorkStart: 9, WorkEnd: 18, Weekdays: true, TimeZone: "UTC"}); !s.working(time.Date(2026, 10, 14, 17, 59, 0, 0, time.UTC)) ||
		s.working(time.Date(2026, 10, 14, 18, 0, 0, 0, time.UTC)) || s.working(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)) {
		t.Error("working hours")
	}
}

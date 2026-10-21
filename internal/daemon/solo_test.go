package daemon

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/setup"
)

// murmur solo: the daemon runs its own hub on 127.0.0.1, where nobody signs in from a browser, and signs its person
// in. Two agents of that person, in two checkouts, hear of each other before editing the same file, message each
// other and ask each other by the way; a teammate's agent can't be asked, for want of a team's hub. A restart signs
// in again on the same port with a new token, and moves to another port when that one is taken.
func TestE2ESolo(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	if err := config.Save(config.Config{Solo: true, Email: "ada.lovelace@example.com", Name: "Ada"}); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	if err := solo(ctx); err != nil {
		t.Fatal(err)
	}
	first := config.Load()
	if u, _ := url.Parse(first.Hub); u.Hostname() != "127.0.0.1" || first.Token == "" || first.Handle != "ada" {
		t.Fatalf("solo saved %+v", first)
	}
	for _, r := range []struct{ method, path string }{{"GET", "/login"}, {"POST", "/dev-login?email=eve@example.com"}} {
		req, _ := http.NewRequest(r.method, first.Hub+r.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Fatalf("%s %s: %d, want a refusal", r.method, r.path, resp.StatusCode)
		}
	}

	ina, inb := newInbox(t), newInbox(t)
	d := newDaemon(first.Handle)
	d.solo = first.Solo
	f := fakeAskers(d)
	a1, a2 := agentSession("api-1", "conv-1", ina, "api/routes.go"), agentSession("api-2", "conv-2", inb, "api/routes.go")
	for i, s := range []*session{a1, a2} {
		s.pid = 100 + i
		d.add(s)
	}
	go d.publishLoop()
	go func(ctx context.Context) {
		for ctx.Err() == nil {
			_ = d.connect(config.Load())
			d.setState("reconnecting")
			time.Sleep(20 * time.Millisecond)
		}
	}(ctx)
	tm := &team{t: t}
	tm.until("online", func() bool { d.mu.Lock(); defer d.mu.Unlock(); return d.state == "online" })
	tm.until("both agents in who", func() bool { w := whoOf(d); return strings.Contains(w, "api-1") && strings.Contains(w, "api-2") })
	if why := d.edit("conv-1", "/work/api-1/api/routes.go"); !strings.Contains(why, `"ada/api-2" is also changing "api/routes.go"`) {
		t.Fatalf("edit: %q", why)
	}
	if r := d.send(ipc.Request{PID: d.pidOfAgent("api-1"), To: "ada/api-2", Text: "I take routes.go"}); !r.OK {
		t.Fatalf("send: %+v", r)
	}
	inb.expect(t, "[murmur] ada/api-1: I take routes.go")
	if r := agentAsks(d, a1, "q1", "api-2", "which half of routes.go do you need?"); !r.OK {
		t.Fatalf("btw: %+v", r)
	}
	if r := agentWaits(d, a1, "q1"); r.Text != "api-2: which half of routes.go do you need?" {
		t.Fatalf("btw-wait: %+v", r)
	}
	for to, want := range map[string]string{"bob/main-1": soloBtw,
		"ada/api-3": "ada/api-3 is not running on this laptop; `who` lists the agents you can ask"} {
		if r := call(d, ipc.Request{Op: "btw", ID: "q-" + strings.ReplaceAll(to, "/", "-"), To: to, Text: "hi"}); r.Err != want {
			t.Errorf("asking %s: %+v, want %q", to, r, want)
		}
	}
	if q := f.questions(); len(q) != 1 {
		t.Fatalf("%d copies ran", len(q))
	}

	stop()
	time.Sleep(50 * time.Millisecond) // the hub lets go of its port
	ctx, stop = context.WithCancel(t.Context())
	defer stop()
	if err := solo(ctx); err != nil {
		t.Fatal(err)
	}
	second := config.Load()
	if second.Hub != first.Hub || second.Token == first.Token {
		t.Fatalf("restart: hub %s token changed %v, want %s and a new token", second.Hub, second.Token != first.Token, first.Hub)
	}
	if setup.TokenAccepted(first) || !setup.TokenAccepted(second) {
		t.Fatal("after a restart, only the new token may be accepted")
	}

	stop()
	time.Sleep(50 * time.Millisecond)
	taken, err := net.Listen("tcp", strings.TrimPrefix(second.Hub, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	ctx, stop = context.WithCancel(t.Context())
	defer stop()
	if err := solo(ctx); err != nil {
		t.Fatal(err)
	}
	if third := config.Load(); third.Hub == second.Hub || !setup.TokenAccepted(third) {
		t.Fatalf("with its port taken, solo saved %s", third.Hub)
	}
}

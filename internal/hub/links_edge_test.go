package hub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/store"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// A pre-release of the oldest accepted version comes before it: 1.3.0-rc.1 is refused when the team needs 1.3.0, at
// hello and when an admin raises the version, while a build of 1.3.0 itself is not.
func TestPreReleaseOfTheOldestVersion(t *testing.T) {
	st := store.NewMemory()
	_ = st.PutUser(context.Background(), store.User{Email: "alice@x.com", Handle: "alice"})
	_ = st.PutDevice(context.Background(), store.Device{ID: "d-alice", Email: "alice@x.com", TokenHash: hashToken("tok-alice"), Created: time.Now()})
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef"), Version: "1.4.0", Seed: seed(func(s *Settings) { s.MinVersion = "1.3.0" })}, st)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	hello := func(version string) (*websocket.Conn, error) {
		c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws",
			&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer tok-alice"}}})
		if err != nil {
			t.Fatal(err)
		}
		_ = wsjson.Write(context.Background(), c, protocol.Frame{T: protocol.FrameHello, Protocol: protocol.Version, Version: version})
		var f protocol.Frame
		return c, wsjson.Read(context.Background(), c, &f)
	}
	for _, v := range []string{"1.3.0-rc.1", "v1.3.0-beta", "1.2.9", "1.3.0-rc.1+build.7"} {
		_, err := hello(v)
		var closed websocket.CloseError
		if !errors.As(err, &closed) || closed.Code != websocket.StatusPolicyViolation {
			t.Errorf("%s got in: %v", v, err)
		}
	}
	for _, v := range []string{"1.3.0", "v1.3.0", "1.3.0+build.7", "1.3.1-rc.1", "1.4.0"} {
		c, err := hello(v)
		if err != nil {
			t.Errorf("%s refused: %v", v, err)
			continue
		}
		c.CloseNow()
	}

	// connected on a pre-release of 1.4.0, the laptop is let go once the team needs 1.4.0
	c, err := hello("1.4.0-rc.2")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	s := h.Settings()
	s.MinVersion = "1.4.0"
	if err := h.SaveSettings(context.Background(), s, "alice"); err != nil {
		t.Fatal(err)
	}
	ctx, done := context.WithTimeout(context.Background(), 3*time.Second)
	defer done()
	for {
		var f protocol.Frame
		if err := wsjson.Read(ctx, c, &f); err != nil {
			if ctx.Err() != nil {
				t.Fatal("a pre-release of the new oldest version stayed connected")
			}
			return
		}
	}
}

// closedWith sends frames on a new connection of carol's, after her hello, and says how the hub closed it.
func closedWith(t *testing.T, url string, frames []protocol.Frame) websocket.StatusCode {
	t.Helper()
	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(url, "http")+"/ws",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer tok-carol"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	c.SetReadLimit(protocol.LaptopReadLimit)
	go func() {
		for _, f := range append([]protocol.Frame{{T: protocol.FrameHello, Protocol: protocol.Version}}, frames...) {
			if wsjson.Write(ctx, c, f) != nil {
				return
			}
		}
	}()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("the hub kept the connection")
			}
			return websocket.CloseStatus(err)
		}
	}
}

// A laptop that sends a frame over the hub's read limit, or a flood of frames, is let go; the others go on.
func TestOversizedAndFloodingLaptops(t *testing.T) {
	_, srv := edgeTeam(t, store.NewMemory())
	alice := connect(t, srv.URL, "tok-alice", protocol.Agent{Name: "a1", Status: protocol.StatusBusy})
	bob := connect(t, srv.URL, "tok-bob", protocol.Agent{Name: "b1", Status: protocol.StatusIdle})

	big := protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "big", To: "bob", Text: strings.Repeat("x", protocol.HubReadLimit)}}
	if code := closedWith(t, srv.URL, []protocol.Frame{big}); code != websocket.StatusMessageTooBig {
		t.Errorf("a frame over the read limit closed with %v", code)
	}
	var flood []protocol.Frame
	for range maxFrames + 1 {
		flood = append(flood, protocol.Frame{T: protocol.FrameCheck, ID: "c"})
	}
	if code := closedWith(t, srv.URL, flood); code != websocket.StatusTryAgainLater {
		t.Errorf("a flood closed with %v", code)
	}

	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "m1", From: "a1", To: "bob/b1", Text: "still here"}})
	bob.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg && f.Msg.ID == "m1" })
}

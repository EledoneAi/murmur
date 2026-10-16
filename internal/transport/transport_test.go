package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/coder/websocket"
)

// A frame Latest hands over replaces the one before while the writer hasn't taken it: a slow peer gets the latest.
func TestLatestReplaces(t *testing.T) {
	c := &Conn{ready: make(chan struct{}, 1)} // no writer
	c.Latest([]byte("old"))
	c.Latest([]byte("new"), []byte("er"))
	if p := c.latest.Swap(nil); p == nil || len(*p) != 2 || string((*p)[0])+string((*p)[1]) != "newer" || len(c.ready) != 1 {
		t.Fatalf("latest: %q", *p)
	}
}

// A frame handed over in parts reaches the peer as one message, after the frames queued before it.
func TestLatest(t *testing.T) {
	conns := make(chan *Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn := New(r.Context(), c, 8)
		conns <- conn
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peer, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.CloseNow()
	conn := <-conns

	conn.Out <- &protocol.Frame{T: protocol.FrameAck, ID: "m1"}
	conn.Latest([]byte(`{"t":"roster","people":[{"handle":"alice"`), []byte(`,"online":true}]`), []byte(`}`))
	want := []string{`{"t":"ack","id":"m1"}`, `{"t":"roster","people":[{"handle":"alice","online":true}]}`}
	for _, w := range want {
		_, b, err := peer.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != w {
			t.Fatalf("read %s, want %s", b, w)
		}
	}
}

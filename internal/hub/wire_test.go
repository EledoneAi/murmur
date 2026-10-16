package hub

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
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

// wsFrame is one WebSocket frame as it crosses the wire.
type wsFrame struct {
	fin, rsv1 bool
	op        byte
	payload   []byte
}

func readFrame(br *bufio.Reader) (wsFrame, error) {
	var h [2]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		return wsFrame{}, err
	}
	f := wsFrame{fin: h[0]&0x80 != 0, rsv1: h[0]&0x40 != 0, op: h[0] & 0x0f}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(br, b[:]); err != nil {
			return f, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(br, b[:]); err != nil {
			return f, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	f.payload = make([]byte, n)
	_, err := io.ReadFull(br, f.payload)
	return f, err
}

// writeFrame writes one masked text frame, as a client must.
func writeFrame(t *testing.T, c net.Conn, p []byte) {
	hdr := []byte{0x81}
	if len(p) < 126 {
		hdr = append(hdr, 0x80|byte(len(p)))
	} else {
		hdr = append(hdr, 0x80|126, byte(len(p)>>8), byte(len(p)))
	}
	hdr = append(hdr, 0, 0, 0, 0) // a zero mask leaves the payload as it is
	if _, err := c.Write(append(hdr, p...)); err != nil {
		t.Fatal(err)
	}
}

// What laptops already installed read from the hub, on the wire: every message, a roster sent in parts included, is
// the JSON json.Marshal writes for the protocol.Frame laptops decode it into, uncompressed even when a laptop offers
// compression, and a laptop reading as the daemon does gets its overlaps. The connection outlives the server's
// deadline for reading a request.
func TestWire(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	for _, u := range []store.User{{Email: "alice@x.com", Handle: "alice"}, {Email: "bob@x.com", Handle: "bob"}} {
		_ = st.PutUser(ctx, u)
		_ = st.PutDevice(ctx, store.Device{ID: "d-" + u.Handle, Email: u.Email, TokenHash: hashToken("tok-" + u.Handle), Created: time.Now()})
	}
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef")}, st)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h.Handler())
	srv.Config.ReadTimeout = 300 * time.Millisecond
	srv.Start()
	defer srv.Close()

	// alice's laptop, read frame by frame, offering compression
	c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET /ws HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Extensions: permessage-deflate; client_max_window_bits\r\nAuthorization: Bearer tok-alice\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Sec-WebSocket-Extensions") != "" {
		t.Fatalf("upgrade: %d, extensions %q", resp.StatusCode, resp.Header.Get("Sec-WebSocket-Extensions"))
	}
	hello, _ := json.Marshal(protocol.Frame{T: protocol.FrameHello, Protocol: protocol.Version, Version: "2.0.1",
		Agents: []protocol.Agent{{Name: "a1", Repo: "Org/App", Tree: "t1", Status: "busy", Files: []string{"x.go"}, Title: "<b>&"}}})
	writeFrame(t, c, hello)

	// bob's laptop, which reads as the daemon does
	bob, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer tok-bob"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer bob.CloseNow()
	bob.SetReadLimit(protocol.LaptopReadLimit)
	_ = wsjson.Write(ctx, bob, protocol.Frame{T: protocol.FrameHello, Protocol: protocol.Version, Version: "2.0.1",
		Agents: []protocol.Agent{{Name: "b1", Repo: "org/app", Tree: "t2", Status: "idle", Files: []string{"x.go"}}}})
	bobPairs := make(chan []protocol.Overlap, 10)
	go func() {
		for {
			var f protocol.Frame
			if wsjson.Read(ctx, bob, &f) != nil {
				return
			}
			if f.T == protocol.FrameRoster && len(f.Pairs) > 0 {
				bobPairs <- f.Pairs
			}
		}
	}()

	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var kinds []string
	var payload []byte
	pairs := false
	for len(kinds) < 4 {
		f, err := readFrame(br)
		if err != nil {
			t.Fatalf("after %q: %v", kinds, err)
		}
		if f.op >= 8 { // control frames: pings
			continue
		}
		if f.rsv1 {
			t.Fatal("a compressed frame")
		}
		payload = append(payload, f.payload...)
		if !f.fin {
			continue
		}
		var fr protocol.Frame
		if err := json.Unmarshal(payload, &fr); err != nil {
			t.Fatalf("not a frame: %v %q", err, payload)
		}
		if again, _ := json.Marshal(fr); string(again) != string(payload) {
			t.Fatalf("%s isn't what json.Marshal writes:\n%s\n%s", fr.T, payload, again)
		}
		kinds, payload = append(kinds, fr.T), nil
		pairs = pairs || (fr.T == protocol.FrameRoster && len(fr.Pairs) > 0)
		if len(kinds) == 2 {
			// idle past the server's deadline for reading a request, then make the hub read from this socket again
			time.Sleep(700 * time.Millisecond)
			chk, _ := json.Marshal(protocol.Frame{T: protocol.FrameCheck, ID: "c1", Repo: "Org/App", Tree: "t1", Files: []string{"x.go"}})
			writeFrame(t, c, chk)
			_ = wsjson.Write(ctx, bob, protocol.Frame{T: protocol.FrameAgents,
				Agents: []protocol.Agent{{Name: "b2", Repo: "org/app", Tree: "t3", Status: "idle", Files: []string{"x.go"}}}})
		}
	}
	if !pairs || kinds[0] != protocol.FrameWelcome || !strings.Contains(strings.Join(kinds, " "), protocol.FrameChecked) {
		t.Fatalf("alice read %q, overlaps on a roster: %v", kinds, pairs)
	}
	select {
	case p := <-bobPairs:
		if p[0].Other != "alice/a1" {
			t.Fatalf("bob's overlaps: %+v", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bob read no roster with his overlaps")
	}
}

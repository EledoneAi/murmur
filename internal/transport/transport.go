// Package transport is the hub's side of one WebSocket to a laptop: frames in, frames out through a queue, the
// latest roster beside the queue, and a ping so a dead peer is noticed within a minute.
package transport

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	pingEvery = 25 * time.Second
	pingWait  = 20 * time.Second
	writeWait = 10 * time.Second
)

type Conn struct {
	Out    chan *protocol.Frame // frames to write, in order; a pointer each, so a deep queue costs little memory
	Cancel context.CancelFunc   // ends the connection
	ctx    context.Context
	c      *websocket.Conn
	mu     sync.Mutex
	latest [][]byte      // the newest frame Latest handed over, not written yet
	ahead  int           // the frames in Out before it, which go first
	ready  chan struct{} // Latest handed one over
}

// New starts the writer: it drains Out and Latest's frame, and pings, until the context ends or a write fails.
func New(ctx context.Context, c *websocket.Conn, depth int) *Conn {
	ctx, cancel := context.WithCancel(ctx)
	t := &Conn{Out: make(chan *protocol.Frame, depth), Cancel: cancel, ctx: ctx, c: c, ready: make(chan struct{}, 1)}
	go t.write()
	context.AfterFunc(ctx, func() { c.CloseNow() })
	return t
}

// Latest hands the writer a frame already encoded, which replaces the one handed over before if that one isn't
// written yet, and takes its place: a peer that reads slowly skips frames it couldn't take instead of making them
// pile up here. The hub sends rosters this way. The frame goes after the frames already in Out when the first of them
// was handed over, and before those queued since, as in one queue: a message never overtakes a roster handed over
// before it.
//
// The frame comes in parts, written one after the other as one message, so connections can share the large ones.
// Each part goes as a frame of the message, as WebSocket allows, and the peer reads the message whole.
func (t *Conn) Latest(parts ...[]byte) {
	t.mu.Lock()
	if t.latest == nil {
		t.ahead = len(t.Out)
	}
	t.latest = parts
	t.mu.Unlock()
	select {
	case t.ready <- struct{}{}:
	default:
	}
}

// due takes Latest's frame once the frames before it are written.
func (t *Conn) due() [][]byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ahead > 0 {
		return nil
	}
	parts := t.latest
	t.latest = nil
	return parts
}

// took counts a frame taken from Out, one fewer before Latest's.
func (t *Conn) took() {
	t.mu.Lock()
	t.ahead = max(t.ahead-1, 0)
	t.mu.Unlock()
}

func (t *Conn) write() {
	tick := time.NewTicker(pingEvery)
	defer tick.Stop()
	for {
		var err error
		if parts := t.due(); parts != nil {
			err = t.parts(parts)
		} else {
			select {
			case <-t.ctx.Done():
				return
			case f := <-t.Out:
				t.took()
				err = t.frame(f)
			case <-t.ready: // due takes it in its turn
			case <-tick.C:
				ctx, done := context.WithTimeout(t.ctx, pingWait)
				err = t.c.Ping(ctx)
				done()
			}
		}
		if err != nil {
			t.Cancel()
			return
		}
	}
}

func (t *Conn) frame(f *protocol.Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	ctx, done := context.WithTimeout(t.ctx, writeWait)
	defer done()
	return t.c.Write(ctx, websocket.MessageText, b)
}

func (t *Conn) parts(parts [][]byte) error {
	ctx, done := context.WithTimeout(t.ctx, writeWait)
	defer done()
	if len(parts) == 1 {
		return t.c.Write(ctx, websocket.MessageText, parts[0])
	}
	w, err := t.c.Writer(ctx, websocket.MessageText)
	if err != nil {
		return err
	}
	for _, p := range parts {
		if _, err := w.Write(p); err != nil {
			return err
		}
	}
	return w.Close()
}

// Read waits for the next frame.
func (t *Conn) Read() (protocol.Frame, error) {
	var f protocol.Frame
	err := wsjson.Read(t.ctx, t.c, &f)
	return f, err
}

// Refuse ends the connection because the other end may not connect as it is: an update is needed.
func (t *Conn) Refuse(why string) { t.close(websocket.StatusPolicyViolation, why) }

// Throttle ends the connection because the other end sent too much: it reconnects after its usual backoff.
func (t *Conn) Throttle(why string) { t.close(websocket.StatusTryAgainLater, why) }

func (t *Conn) close(code websocket.StatusCode, why string) {
	_ = t.c.Close(code, why)
	t.Cancel()
}

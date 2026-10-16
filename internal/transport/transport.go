// Package transport is one WebSocket between a laptop and its hub, as both ends run it: frames in, frames out
// through a queue, and a ping so a dead peer is noticed within a minute.
package transport

import (
	"context"
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
	Out    chan protocol.Frame // frames to write, in order
	Cancel context.CancelFunc  // ends the connection
	ctx    context.Context
	c      *websocket.Conn
}

// New starts the writer: it drains Out and pings until the context ends or a write fails.
func New(ctx context.Context, c *websocket.Conn, depth int) *Conn {
	ctx, cancel := context.WithCancel(ctx)
	t := &Conn{Out: make(chan protocol.Frame, depth), Cancel: cancel, ctx: ctx, c: c}
	go t.write()
	go func() {
		<-ctx.Done()
		c.CloseNow()
	}()
	return t
}

func (t *Conn) write() {
	tick := time.NewTicker(pingEvery)
	defer tick.Stop()
	for {
		var err error
		select {
		case <-t.ctx.Done():
			return
		case f := <-t.Out:
			ctx, done := context.WithTimeout(t.ctx, writeWait)
			err = wsjson.Write(ctx, t.c, f)
			done()
		case <-tick.C:
			ctx, done := context.WithTimeout(t.ctx, pingWait)
			err = t.c.Ping(ctx)
			done()
		}
		if err != nil {
			t.Cancel()
			return
		}
	}
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

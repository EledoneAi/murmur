package hub

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/release"
	"github.com/EledoneAi/murmur/v2/internal/store"
	"github.com/EledoneAi/murmur/v2/internal/transport"
	"github.com/coder/websocket"
)

// bearer finds the laptop a request's token belongs to, if it may connect.
func (h *Hub) bearer(r *http.Request) (store.Device, store.User, bool) {
	t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return store.Device{}, store.User{}, false
	}
	hash := hashToken(strings.TrimSpace(t))
	h.mu.Lock()
	_, known := h.tokens[hash]
	h.mu.Unlock()
	if !known { // signed in on another instance a moment ago
		h.reloadSoon(r.Context())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.tokens[hash]
	if d == nil || !h.allowedLocked(d.ID) {
		return store.Device{}, store.User{}, false
	}
	return *d, *h.users[d.Email], true
}

func (h *Hub) serveWS(w http.ResponseWriter, r *http.Request) {
	dev, user, ok := h.bearer(r)
	if !ok {
		h.log.Info("laptop refused", "why", "unknown, revoked or expired token")
		http.Error(w, "this laptop is not signed in to murmur: run `murmur join`", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(smallSend{w}, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(protocol.HubReadLimit)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	conn := transport.New(ctx, c, outboxDepth)
	late := time.AfterFunc(helloWait, conn.Cancel) // a laptop says hello at once
	hello, err := conn.Read()
	if !late.Stop() || err != nil {
		return
	}
	if hello.T != protocol.FrameHello || hello.Protocol != protocol.Version {
		h.log.Info("laptop refused", "person", user.Handle, "protocol", hello.Protocol, "why", "speaks another protocol")
		conn.Refuse(fmt.Sprintf("this hub speaks protocol %d and runs %s: run murmur update", protocol.Version, h.cfg.Version))
		return
	}
	if min := h.Settings().MinVersion; min != "" && release.Before(hello.Version, min) {
		h.log.Info("laptop refused", "person", user.Handle, "version", hello.Version, "why", "older than "+min)
		conn.Refuse("this team needs murmur " + min + " or later: run murmur update")
		return
	}
	hello.Version = protocol.Clean(protocol.Cut(hello.Version, 40), false)
	l := &link{dev: dev.ID, email: user.Email, handle: user.Handle, version: hello.Version, out: conn.Out, roster: conn.Latest,
		cancel: conn.Cancel, btw: slices.Contains(hello.Can, protocol.CanBtw)}
	h.attach(ctx, l, dev)
	h.handle(l, hello)
	start, reason := time.Now(), "closed by the hub"
	defer func() {
		h.detach(ctx, l)
		h.log.Info("laptop disconnected", "person", user.Handle, "laptop", dev.ID, "after_s", int(time.Since(start).Seconds()), "reason", reason)
	}()
	window, frames := time.Now(), 0
	for {
		f, err := conn.Read()
		if err != nil {
			reason = err.Error()
			return
		}
		// a laptop sends a few frames a minute; hundreds in seconds is a bug or an attack, and costs the team
		if frames++; time.Since(window) > frameWindow {
			window, frames = time.Now(), 0
		} else if frames > maxFrames {
			reason = "too many frames"
			conn.Throttle("this laptop sent too much at once; it reconnects in a moment")
			return
		}
		h.handle(l, f)
	}
}

// sendBuffer is the most the kernel holds for one laptop's connection, unsent or unacknowledged, before a write to it
// waits, rather than the megabytes it would let it grow to for a laptop that reads slowly: the hub's container pays
// for them, with every slow laptop at once. A laptop that takes more than ten seconds to read a frame is let go and
// reconnects.
const sendBuffer = 64 << 10

// smallSend hands websocket.Accept the laptop's connection with a send buffer of sendBuffer.
type smallSend struct{ http.ResponseWriter }

func (w smallSend) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if t, ok := c.(*net.TCPConn); ok {
		_ = t.SetWriteBuffer(sendBuffer)
	}
	return c, rw, err
}

func (l *link) send(f protocol.Frame) {
	select {
	case l.out <- &f:
	default:
		l.cancel() // a laptop that can't keep up reconnects and gets a fresh roster
	}
}

func (h *Hub) attach(ctx context.Context, l *link, dev store.Device) {
	h.mu.Lock()
	if old := h.links[l.dev]; old != nil {
		old.cancel()
		l.agents = old.agents // keep the agents visible until the laptop says hello again
	}
	h.links[l.dev] = l
	l.send(h.welcomeLocked(l))
	h.mu.Unlock()
	h.markDirty()
	seen := time.Now()
	h.mu.Lock()
	if d := h.devices[dev.ID]; d != nil {
		d.LastSeen, d.Version = seen, l.version
	}
	h.mu.Unlock()
	go func() {
		ctx, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer done()
		if err := h.store.SeenDevice(ctx, dev.ID, l.version, seen); err != nil {
			h.log.Error("database", "op", "save laptop", "laptop", dev.ID, "error", err.Error())
		}
		h.deliverKept(ctx, l)
	}()
}

// deliverKept hands a laptop that just connected the messages kept for it: for its person, or for one of its agents
// while it was reconnecting.
func (h *Hub) deliverKept(ctx context.Context, l *link) {
	kept, err := h.store.Pending(ctx, l.email)
	if err != nil {
		h.log.Error("database", "op", "read messages kept for later", "person", l.handle, "error", err.Error())
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range kept {
		if p.Device == "" || p.Device == l.dev {
			m := p.Msg
			h.waiting[m.ID] = &waiter{from: p.Sender, stored: true, created: time.Now()}
			l.send(protocol.Frame{T: protocol.FrameMsg, Msg: &m})
		}
	}
}

func (h *Hub) detach(ctx context.Context, l *link) {
	h.mu.Lock()
	if h.links[l.dev] == l {
		l.gone = time.Now()
		h.publishLinkLocked(ctx, l) // still listed, offline, while it may come back
	}
	h.mu.Unlock()
	h.markDirty()
}

func (h *Hub) handle(l *link, f protocol.Frame) {
	switch f.T {
	case protocol.FrameHello, protocol.FrameAgents:
		max := h.Settings().MaxAgents
		f.Agents = f.Agents[:min(len(f.Agents), max)]
		for i := range f.Agents { // everyone's console prints these, and checks quote them to agents
			f.Agents[i].Files = f.Agents[i].Files[:min(len(f.Agents[i].Files), 300)]
			f.Agents[i].Clean() // before taking the lock: one large frame must not hold up the team
		}
		h.mu.Lock()
		l.agents = f.Agents
		h.changes++
		l.changed = h.changes
		if f.T == protocol.FrameHello { // its first roster goes to it first, and has its agents
			h.newLinks = append(h.newLinks, l)
		}
		h.publishLinkLocked(context.Background(), l)
		h.mu.Unlock()
		h.markDirty()
		if f.T == protocol.FrameHello {
			h.log.Info("laptop connected", "person", l.handle, "laptop", l.dev, "version", f.Version, "agents", len(f.Agents), "instance", h.id)
		}
	case protocol.FrameSend:
		if f.Msg != nil {
			h.route(l, *f.Msg)
		}
	case protocol.FrameCheck:
		h.check(l, f)
	case protocol.FrameReport:
		if f.Report != nil {
			go h.saveReport(l.dev, *f.Report)
		}
	case protocol.FrameBtw:
		if f.Btw != nil {
			h.ask(l, *f.Btw)
		}
	case protocol.FrameBtwAnswer:
		if f.Btw != nil {
			h.answered(l, *f.Btw)
		}
	case protocol.FrameDelivered:
		// the recipient's laptop wrote that error: it reaches the sender's terminal and agent. Cleaned before the lock.
		f.Err, f.Note = protocol.Clean(protocol.Cut(f.Err, 300), false), protocol.Clean(protocol.Cut(f.Note, 200), false)
		h.delivered(f)
	}
}

// saveReport keeps a laptop's last self-check for the admin panel.
func (h *Hub) saveReport(dev string, r protocol.Report) {
	r.Checks = r.Checks[:min(len(r.Checks), 20)]
	for i := range r.Checks {
		c := &r.Checks[i]
		c.Name, c.Detail = protocol.Clean(protocol.Cut(c.Name, 40), false), protocol.Clean(protocol.Cut(c.Detail, 300), false)
	}
	r.OS, r.ClaudeCode = protocol.Clean(protocol.Cut(r.OS, 40), false), protocol.Clean(protocol.Cut(r.ClaudeCode, 40), false)
	report, at := mustJSON(r), time.Now()
	ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	if err := h.store.ReportDevice(ctx, dev, report, at); err != nil {
		h.log.Error("database", "op", "save a laptop's check", "laptop", dev, "error", err.Error())
		return
	}
	h.mu.Lock()
	if d := h.devices[dev]; d != nil {
		d.Report, d.ReportAt = report, at
	}
	h.mu.Unlock()
}

// welcomeLocked is what a laptop hears on connecting, and again whenever the team's settings change: who it is,
// the team's version, what the team asks of laptops, and whether the hub carries questions by the way, which the
// team's admins can turn off.
func (h *Hub) welcomeLocked(l *link) protocol.Frame {
	set, name := h.settings, ""
	if u := h.users[l.email]; u != nil {
		name = u.Name
	}
	var can []string
	if set.Btw {
		can = []string{protocol.CanBtw}
	}
	return protocol.Frame{T: protocol.FrameWelcome, Version: h.cfg.Version, Me: &protocol.Person{Handle: l.handle, Name: name, Online: true,
		Colour: h.coloursLocked()[l.email]},
		Team: &protocol.Team{Updates: set.UpdatePolicy, MinVersion: set.MinVersion, CheckHours: set.CheckHours, Agreements: set.Agreements},
		Can:  can}
}

// teamChangedLocked tells this instance's laptops the team's new settings, and lets go of those now older than the
// oldest version the team accepts: they reconnect, are refused with the command to run, and update. The roster goes
// again too: it says which agents can be asked by the way.
func (h *Hub) teamChangedLocked() {
	h.markDirty()
	for _, l := range h.links {
		if !l.gone.IsZero() {
			continue
		}
		if min := h.settings.MinVersion; min != "" && release.Before(l.version, min) {
			l.cancel()
			continue
		}
		l.send(h.welcomeLocked(l))
	}
}

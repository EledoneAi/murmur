package hub

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/internal/bus"
	"github.com/EledoneAi/murmur/internal/protocol"
	"github.com/EledoneAi/murmur/internal/store"
)

// Events on the bus between the instances of one hub.
const (
	evPresence = "presence" // one laptop's agents, when they change or when another instance asks
	evAlive    = "alive"    // the laptops an instance still holds, every aliveEvery
	evSync     = "sync"     // a new instance asks the others for their laptops
	evFrame    = "frame"    // a frame for laptops another instance holds
	evChanged  = "changed"  // something changed in the store: read it again
	evAnswer   = "answer"   // the answer to a message sent from a team page on another instance
)

// presence is a laptop as the instance holding it describes it.
type presence struct {
	Device string           `json:"device"`
	Email  string           `json:"email"`
	Handle string           `json:"handle"`
	Agents []protocol.Agent `json:"agents,omitempty"` // files included: every instance answers checks and lists overlaps
	Online bool             `json:"online"`           // false while it reconnects, or once it's gone

	instance string
	seen     time.Time
}

// frameEvent carries a frame to one laptop (Device), or to every laptop of some people (Handles) except Skip.
type frameEvent struct {
	Device  string         `json:"device,omitempty"`
	Handles []string       `json:"handles,omitempty"`
	Skip    []string       `json:"skip,omitempty"`
	ReplyTo string         `json:"reply_to,omitempty"` // the laptop that sent the message in Frame, told once delivered
	Frame   protocol.Frame `json:"frame"`
}

func (h *Hub) publish(ctx context.Context, kind string, data any, to string) {
	e := bus.Event{Kind: kind, From: h.id, To: to}
	if data != nil {
		e.Data = mustJSON(data)
	}
	e.Sig = h.eventMAC(e)
	ctx, done := context.WithTimeout(ctx, 5*time.Second)
	defer done()
	if err := h.bus.Publish(ctx, e); err != nil {
		h.log.Error("bus", "op", "publish "+kind, "error", err.Error())
	}
}

// eventMAC signs an event with the hub's secret, which every instance shares: a bus shared with other software, or
// reachable by someone who has no business with the hub, can't inject frames, presence or "changed" events.
func (h *Hub) eventMAC(e bus.Event) string {
	m := hmac.New(sha256.New, h.cfg.Secret)
	m.Write([]byte("bus\x00" + e.Kind + "\x00" + e.From + "\x00" + e.To + "\x00"))
	m.Write(e.Data)
	return base64.RawStdEncoding.EncodeToString(m.Sum(nil))
}

// listen acts on what the other instances publish. An instance's own events are already done.
func (h *Hub) listen(ctx context.Context) {
	for {
		var e bus.Event
		select {
		case <-ctx.Done():
			return
		case e = <-h.bus.Events():
		}
		if e.Kind == bus.Reconnected && e.From == "" { // from this instance's own bus
			h.resync(ctx)
			continue
		}
		if e.From == h.id || (e.To != "" && e.To != h.id) || !hmac.Equal([]byte(e.Sig), []byte(h.eventMAC(e))) {
			continue
		}
		switch e.Kind {
		case evPresence:
			var p presence
			if json.Unmarshal(e.Data, &p) == nil {
				h.onPresence(e.From, p)
			}
		case evAlive:
			var devs []string
			if json.Unmarshal(e.Data, &devs) == nil {
				h.onAlive(ctx, e.From, devs)
			}
		case evSync:
			h.mu.Lock()
			for _, l := range h.links {
				h.publishLinkLocked(ctx, l)
			}
			h.mu.Unlock()
		case evFrame:
			var f frameEvent
			if json.Unmarshal(e.Data, &f) == nil {
				h.onFrameEvent(f)
			}
		case evChanged:
			var what string
			_ = json.Unmarshal(e.Data, &what)
			h.onChanged(ctx, what)
		case evAnswer:
			var f protocol.Frame
			if json.Unmarshal(e.Data, &f) == nil {
				h.mu.Lock()
				h.pageAnswerLocked(f)
				h.mu.Unlock()
			}
		}
	}
}

// resync reads everything from the store again and asks the other instances for their laptops: after the bus
// reconnects, any event published meanwhile was lost.
func (h *Hub) resync(ctx context.Context) {
	h.log.Info("bus reconnected: reading the store again")
	for _, what := range []string{"settings", "announcements", "people"} {
		h.onChanged(ctx, what)
	}
	h.publish(ctx, evSync, nil, "")
}

func (h *Hub) onChanged(ctx context.Context, what string) {
	var err error
	switch what {
	case "settings":
		err = h.loadSettings(ctx)
	case "announcements":
		err = h.loadNews(ctx)
	default:
		err = h.reload(ctx)
	}
	if err != nil {
		h.log.Error("database", "op", "read "+what+" again", "error", err.Error())
	}
}

// publishLinkLocked tells the other instances about one of this instance's laptops.
func (h *Hub) publishLinkLocked(ctx context.Context, l *link) {
	p := presence{Device: l.dev, Email: l.email, Handle: l.handle, Agents: l.agents, Online: l.gone.IsZero()}
	go h.publish(context.WithoutCancel(ctx), evPresence, p, "")
}

func (h *Hub) onPresence(from string, p presence) {
	for i := range p.Agents { // another instance cleaned them already; this one doesn't rely on it
		p.Agents[i].Clean()
	}
	h.mu.Lock()
	p.instance, p.seen = from, time.Now()
	old := h.remote[p.Device]
	switch {
	case !p.Online && old != nil && old.Online && old.instance != from:
		// the laptop moved to another instance, which holds it now: what the one it left says is old news
		h.mu.Unlock()
		return
	case !p.Online && len(p.Agents) == 0:
		delete(h.remote, p.Device)
	default:
		h.remote[p.Device] = &p
	}
	if l := h.links[p.Device]; p.Online && l != nil && !l.gone.IsZero() {
		delete(h.links, p.Device) // it left this instance for that one: no goodbye from here, it isn't gone
	}
	h.mu.Unlock()
	h.markDirty()
}

// onAlive keeps another instance's laptops listed. A laptop this instance doesn't know there, after a lost event
// or a bus reconnect, makes it ask that instance for its laptops again, at most every aliveTimeout.
func (h *Hub) onAlive(ctx context.Context, from string, devs []string) {
	h.mu.Lock()
	missing := false
	for _, d := range devs {
		if p := h.remote[d]; p != nil && p.instance == from {
			p.seen = time.Now()
		} else if l := h.links[d]; l == nil || !l.gone.IsZero() {
			missing = true
		}
	}
	ask := missing && time.Since(h.asked[from]) > aliveTimeout
	if ask {
		h.asked[from] = time.Now()
	}
	h.mu.Unlock()
	if ask {
		h.publish(ctx, evSync, nil, from)
	}
}

// onFrameEvent hands a frame from another instance to the laptops it is for, on this instance.
func (h *Hub) onFrameEvent(f frameEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if f.Device != "" {
		if l := h.links[f.Device]; l != nil && l.gone.IsZero() {
			if f.ReplyTo != "" && f.Frame.Msg != nil {
				h.waiting[f.Frame.Msg.ID] = &waiter{from: f.ReplyTo, created: time.Now()}
			}
			l.send(f.Frame)
		}
		return
	}
	for _, l := range h.links {
		if l.gone.IsZero() && slices.Contains(f.Handles, l.handle) && !slices.Contains(f.Skip, l.dev) {
			l.send(f.Frame)
		}
	}
}

// ---------------------------------------------------------------- who is where

// holder is a laptop and its agents, on this instance or another.
type holder struct {
	dev, email, handle string
	agents             []protocol.Agent
	online             bool
}

// holdersLocked lists every laptop the team can see: this instance's, then the others'.
func (h *Hub) holdersLocked() []holder {
	var out []holder
	for _, l := range h.links {
		if p := h.remote[l.dev]; !l.gone.IsZero() && p != nil && p.Online {
			continue // gone from here, online on another instance
		}
		out = append(out, holder{dev: l.dev, email: l.email, handle: l.handle, agents: l.agents, online: l.gone.IsZero()})
	}
	for _, p := range h.remote {
		if l, here := h.links[p.Device]; !here || (!l.gone.IsZero() && p.Online) {
			out = append(out, holder{dev: p.Device, email: p.Email, handle: p.Handle, agents: p.Agents, online: p.Online})
		}
	}
	return out
}

// toDeviceLocked sends a frame to one laptop, wherever it is connected. replyTo names the laptop to tell when the
// message in the frame is delivered.
func (h *Hub) toDeviceLocked(dev string, f protocol.Frame, replyTo string) bool {
	if l := h.links[dev]; l != nil && l.gone.IsZero() {
		if replyTo != "" && f.Msg != nil {
			h.waiting[f.Msg.ID] = &waiter{from: replyTo, created: time.Now()}
		}
		l.send(f)
		return true
	}
	if p := h.remote[dev]; p != nil && p.Online {
		go h.publish(context.Background(), evFrame, frameEvent{Device: dev, ReplyTo: replyTo, Frame: f}, p.instance)
		return true
	}
	return false
}

// toPeopleLocked sends a frame to every laptop of these people except skip, on every instance.
func (h *Hub) toPeopleLocked(handles []string, f protocol.Frame, skip []string) {
	for _, l := range h.links {
		if l.gone.IsZero() && slices.Contains(handles, l.handle) && !slices.Contains(skip, l.dev) {
			l.send(f)
		}
	}
	for _, p := range h.remote {
		if p.Online && slices.Contains(handles, p.Handle) {
			go h.publish(context.Background(), evFrame, frameEvent{Handles: handles, Skip: skip, Frame: f}, "")
			return // one event reaches every instance
		}
	}
}

// ---------------------------------------------------------------- roster

func (h *Hub) markDirty() {
	select {
	case h.dirty <- struct{}{}:
	default:
	}
}

// rosterGap is the least time between two rosters: a burst of changes makes one.
const rosterGap = 100 * time.Millisecond

// overlapIndex is the latest overlap index, or a new one before the first roster.
func (h *Hub) overlapIndex() *overlapIndex {
	if x := h.index.Load(); x != nil {
		return x
	}
	return h.reindex()
}

// reindex builds the overlap index from who is online now, outside the lock, and keeps it.
func (h *Hub) reindex() *overlapIndex {
	h.mu.Lock()
	holders, s := h.holdersLocked(), h.settings
	h.mu.Unlock()
	x := newIndex(holders, s)
	h.index.Store(x)
	return x
}

// rosterLoop sends every laptop here the whole team whenever something changes, and tells the team pages open here.
// A team is a few dozen agents, so the whole roster is a few kilobytes and every laptop always holds a complete,
// consistent picture.
func (h *Hub) rosterLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.dirty:
		}
		time.Sleep(rosterGap) // gather bursts, such as a laptop reporting several sessions
		h.mu.Lock()
		people, holders, s := h.rosterLocked(), h.holdersLocked(), h.settings
		var links []*link
		for _, l := range h.links {
			if l.gone.IsZero() {
				links = append(links, l)
			}
		}
		h.mu.Unlock()
		// overlaps are worked out without the lock: the hub keeps answering while a big team's are found
		x := newIndex(holders, s)
		h.index.Store(x)
		for _, l := range links {
			l.send(protocol.Frame{T: protocol.FrameRoster, People: people, Pairs: x.current(l.handle)})
		}
		h.mu.Lock()
		for c := range h.streams {
			select {
			case c <- struct{}{}:
			default: // it hasn't sent the last change yet, and reads the team afresh when it does
			}
		}
		h.mu.Unlock()
	}
}

// rosterBudget keeps the team list under what a laptop reads in one frame, whatever its agents say of themselves.
const rosterBudget = protocol.LaptopReadLimit * 3 / 4

// worstSize bounds the JSON of an agent the hub cleaned: encoding/json can write one byte as six (\u003c).
func worstSize(a protocol.Agent) int {
	n := 512
	for _, s := range append([]string{a.Name, a.Repo, a.Branch, a.Title, a.PRURL, a.App, a.Status, a.Needs, a.Doing,
		a.Kinds, a.Base, a.Detached, a.Op, a.Dir}, a.Also...) {
		n += 6*len(s) + 24 // the field's name and quotes, for Also's entries
	}
	return n
}

// coloursLocked gives each person their colour: everyone who ever joined, removed people included so nobody's colour
// moves, in the order they joined.
func (h *Hub) coloursLocked() map[string]int {
	users := slices.Collect(maps.Values(h.users))
	slices.SortFunc(users, func(a, b *store.User) int {
		return cmp.Or(a.Created.Compare(b.Created), strings.Compare(a.Email, b.Email))
	})
	out := make(map[string]int, len(users))
	for i, u := range users {
		out[u.Email] = i % protocol.Colours
	}
	return out
}

func (h *Hub) rosterLocked() []protocol.Person {
	byEmail := map[string]*protocol.Person{}
	colours := h.coloursLocked()
	for _, u := range h.users {
		if !u.Disabled {
			byEmail[u.Email] = &protocol.Person{Handle: u.Handle, Name: u.Name, Colour: colours[u.Email]}
		}
	}
	type placed struct {
		p *protocol.Person
		a protocol.Agent
	}
	var all []placed
	for _, x := range h.holdersLocked() {
		p := byEmail[x.email]
		if p == nil {
			continue
		}
		p.Online = p.Online || x.online
		for _, a := range x.agents {
			a.Files, a.Tree = nil, ""
			all = append(all, placed{p, a})
		}
	}
	slices.SortFunc(all, func(a, b placed) int { return cmp.Compare(b.a.Since, a.a.Since) })
	size := 1024 * len(byEmail)
	for _, x := range all {
		if size += worstSize(x.a); size > rosterBudget {
			break // the least recently active agents are left out, rather than every laptop dropping its connection
		}
		x.p.Agents = append(x.p.Agents, x.a)
	}
	var out []protocol.Person
	for _, p := range byEmail {
		slices.SortFunc(p.Agents, func(a, b protocol.Agent) int { return strings.Compare(a.Name, b.Name) })
		out = append(out, *p)
	}
	slices.SortFunc(out, func(a, b protocol.Person) int { return strings.Compare(a.Handle, b.Handle) })
	return out
}

// sweepLoop forgets laptops that are gone: this instance's after `grace`, other instances' once they stop saying
// they hold them. It also says which laptops this instance holds, and drops answers nobody waits for.
func (h *Hub) sweepLoop(ctx context.Context) {
	tick := time.NewTicker(aliveEvery / 2)
	defer tick.Stop()
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		n++
		h.sweep(ctx, n%2 == 0)
	}
}

func (h *Hub) sweep(ctx context.Context, alive bool) {
	changed := false
	h.mu.Lock()
	var held []string
	for id, l := range h.links {
		if !l.gone.IsZero() && time.Since(l.gone) > grace {
			delete(h.links, id)
			h.publishLinkLocked(ctx, &link{dev: l.dev, email: l.email, handle: l.handle, gone: l.gone})
			changed = true
			continue
		}
		held = append(held, id)
	}
	for id, p := range h.remote {
		if time.Since(p.seen) > aliveTimeout {
			delete(h.remote, id)
			changed = true
		}
	}
	for id, w := range h.waiting {
		if time.Since(w.created) > ackTimeout {
			delete(h.waiting, id)
		}
	}
	for from, ts := range h.sends {
		if len(ts) == 0 || time.Since(ts[len(ts)-1]) > time.Minute {
			delete(h.sends, from)
		}
	}
	window := time.Duration(h.settings.LoopMinutes) * time.Minute
	for k, p := range h.pairs {
		if len(p.msgs) == 0 || time.Since(p.msgs[len(p.msgs)-1]) > window {
			delete(h.pairs, k)
		}
	}
	h.gets.sweep()
	h.mu.Unlock()
	if alive && len(held) > 0 {
		h.publish(ctx, evAlive, held, "")
	}
	if changed {
		h.markDirty()
	}
}

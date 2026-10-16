package hub

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"hash/maphash"
	"iter"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/bus"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/store"
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
func (h *Hub) holdersLocked() []holder { return slices.Collect(h.holdersOfLocked("")) }

// holdersOfLocked goes through one person's laptops, or everyone's for "", as holdersLocked lists them, without
// listing them all first: each message looks for its recipient this way.
func (h *Hub) holdersOfLocked(handle string) iter.Seq[holder] {
	return func(yield func(holder) bool) {
		for _, l := range h.links {
			if handle != "" && l.handle != handle {
				continue
			}
			if p := h.remote[l.dev]; !l.gone.IsZero() && p != nil && p.Online {
				continue // gone from here, online on another instance
			}
			if !yield(holder{dev: l.dev, email: l.email, handle: l.handle, agents: l.agents, online: l.gone.IsZero()}) {
				return
			}
		}
		for _, p := range h.remote {
			if handle != "" && p.Handle != handle {
				continue
			}
			if l, here := h.links[p.Device]; !here || (!l.gone.IsZero() && p.Online) {
				if !yield(holder{dev: p.Device, email: p.Email, handle: p.Handle, agents: p.Agents, online: p.Online}) {
					return
				}
			}
		}
	}
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

// rosterGap is the least time between a change and its roster: a burst of changes makes one.
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

// rosterLoop sends every laptop here the whole team whenever something changes, and tells the team pages open here,
// so every laptop always holds a complete, consistent picture: a laptop's first roster already has its agents, as
// the hub's checks do. Each laptop gets the whole team, so a roster costs the team's size times the laptops: rosters
// go out at rosterRate at most, to the laptops that just connected first, and a big, busy team gets them every few
// seconds rather than in bursts that would take the CPU, memory and network its messages need.
func (h *Hub) rosterLoop(ctx context.Context) {
	var (
		r     *roster   // the newest roster
		queue []*link   // the laptops still to hand it to, in turn
		pages bool      // the team pages haven't heard of it yet
		stale time.Time // when something changed since it, if something did
		built time.Time // when it was worked out, and how long that took
		took  time.Duration
		paid  time.Time // until when what was handed out uses up rosterRate
	)
	wake := time.NewTimer(time.Hour)
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.dirty:
			if stale.IsZero() {
				stale = time.Now()
			}
		case <-wake.C:
		}
		now := time.Now()
		// once a burst of changes is over, such as a laptop reporting several sessions, and at most a tenth of the time
		rework := later(stale.Add(rosterGap), built.Add(10*took))
		if !stale.IsZero() && !now.Before(rework) {
			r, queue = h.buildRoster(queue)
			built, took, stale, pages = now, time.Since(now), time.Time{}, true
		}
		for r != nil && (pages || len(queue) > 0) && paid.Before(now.Add(rosterBurst)) {
			n := 0
			if pages {
				n, pages = h.tellPages(), false
			} else {
				first := queue[0].shown == 0 // its console shows no team yet, as after a deploy: twice the rate
				n, queue = r.hand(queue[0]), queue[1:]
				if first {
					n /= 2
				}
			}
			paid = later(paid, now).Add(time.Duration(n) * time.Second / rosterRate)
		}
		next := time.Hour
		if r != nil && (pages || len(queue) > 0) {
			next = paid.Sub(now) - rosterBurst
		}
		if !stale.IsZero() {
			next = min(next, rework.Sub(now))
		}
		wake.Reset(max(next, 0))
	}
}

// rosterRate is how many bytes of rosters a second this instance hands out at most, to its laptops and team pages
// together, and twice that to laptops that have none yet; rosterBurst is how far ahead of it it may go at once.
//
// ponytail: the whole team each time makes the rosters' cost grow with the square of the team. Rosters that carry
// only who changed would make it linear: a protocol change, for teams of thousands.
const (
	rosterRate  = 128 << 20
	rosterBurst = rosterGap
)

// buildRoster works out the roster and the team pages' view of it, and lists the laptops to hand it to: those that
// just connected and those still waiting for the last one first, then every other laptop here.
func (h *Hub) buildRoster(waiting []*link) (*roster, []*link) {
	h.mu.Lock()
	people, holders, s := h.rosterLocked(), h.holdersLocked(), h.settings
	queue, in := make([]*link, 0, len(h.links)), map[*link]bool{}
	for _, list := range [][]*link{h.newLinks, waiting, slices.Collect(maps.Values(h.links))} {
		for _, l := range list {
			if !in[l] && l.gone.IsZero() && h.links[l.dev] == l {
				in[l] = true
				queue = append(queue, l)
			}
		}
	}
	h.newLinks = nil
	var news []newsItem
	if len(h.streams) > 0 {
		news = h.newsLocked()
	}
	h.mu.Unlock()
	// overlaps are worked out, and the roster encoded, without the lock: the hub keeps answering meanwhile
	x := newIndex(holders, s)
	h.index.Store(x)
	if news == nil {
		h.teamPage.Store(nil)
	} else {
		h.teamPage.Store(newTeamEvent(people, holders, news))
	}
	return newRoster(people, x), queue
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// tellPages tells the team pages open here that the team changed, and says about how much they send for it.
func (h *Hub) tellPages() int {
	e := h.teamPage.Load()
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.streams {
		select {
		case c <- struct{}{}:
		default: // it hasn't sent the last change yet, and sends the newest when it does
		}
	}
	if e == nil {
		return 0
	}
	return len(h.streams) * len(e.shared)
}

// roster is one roster as JSON in two parts: the team, encoded once for every laptop, then what each person's own
// roster adds, their overlaps. Only rosterLoop uses it.
type roster struct {
	head    []byte // the frame up to the end of the team
	headSum uint64 // its hash
	x       *overlapIndex
	tails   map[string][]byte // by handle
	sums    map[string]uint64 // by handle: the whole frame's hash
}

var hashSeed = maphash.MakeSeed()

func newRoster(people []protocol.Person, x *overlapIndex) *roster {
	head := []byte(`{"t":"` + protocol.FrameRoster + `"`)
	if len(people) > 0 { // as omitempty would
		head = append(append(head, `,"people":`...), mustJSON(people)...)
	}
	return &roster{head: head, headSum: maphash.Bytes(hashSeed, head), x: x, tails: map[string][]byte{}, sums: map[string]uint64{}}
}

// frame is a person's roster, in its two parts, and the hash of the whole: protocol.Frame{T: FrameRoster, People:
// the team, Pairs: their overlaps}, as json.Marshal would write it.
func (r *roster) frame(handle string) (head, tail []byte, sum uint64) {
	tail, ok := r.tails[handle]
	if !ok {
		tail = []byte("}")
		if pairs := r.x.current(handle); len(pairs) > 0 {
			tail = append(append([]byte(`,"pairs":`), mustJSON(pairs)...), '}')
		}
		r.tails[handle], r.sums[handle] = tail, maphash.Comparable(hashSeed, [2]uint64{r.headSum, maphash.Bytes(hashSeed, tail)})
	}
	return r.head, tail, r.sums[handle]
}

// hand gives a laptop this roster, unless it has it already, and returns the bytes it handed.
func (r *roster) hand(l *link) int {
	head, tail, sum := r.frame(l.handle)
	if l.shown == sum {
		return 0
	}
	l.shown = sum
	l.roster(head, tail)
	return len(head) + len(tail)
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
// moves, in the order they joined. It is worked out again only once people change; callers don't change it.
func (h *Hub) coloursLocked() map[string]int {
	if h.colours != nil {
		return h.colours
	}
	users := slices.Collect(maps.Values(h.users))
	slices.SortFunc(users, func(a, b *store.User) int {
		return cmp.Or(a.Created.Compare(b.Created), strings.Compare(a.Email, b.Email))
	})
	h.colours = make(map[string]int, len(users))
	for i, u := range users {
		h.colours[u.Email] = i % protocol.Colours
	}
	return h.colours
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
		a *protocol.Agent // as its laptop said, which nothing changes
	}
	var all []placed
	for x := range h.holdersOfLocked("") {
		p := byEmail[x.email]
		if p == nil {
			continue
		}
		p.Online = p.Online || x.online
		for i := range x.agents {
			all = append(all, placed{p, &x.agents[i]})
		}
	}
	slices.SortFunc(all, func(a, b placed) int { return cmp.Compare(b.a.Since, a.a.Since) })
	size := 1024 * len(byEmail)
	for _, x := range all {
		if size += worstSize(*x.a); size > rosterBudget {
			break // the least recently active agents are left out, rather than every laptop dropping its connection
		}
		a := *x.a
		a.Files, a.Tree = nil, ""
		x.p.Agents = append(x.p.Agents, a)
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

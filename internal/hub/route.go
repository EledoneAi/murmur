package hub

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/store"
)

var msgID = regexp.MustCompile(`^[0-9A-Za-z-]{1,64}$`)

// sender is whoever sends a message: a laptop connected to this instance, for its person or one of its agents, or a
// person on the team page, who has no laptop.
type sender struct {
	handle string
	l      *link                // nil on the team page
	answer func(protocol.Frame) // takes the hub's answer about the message
}

// causeLocked is the sender's latest agents report, as h.changes counts them, when a laptop here sends.
func (s sender) causeLocked() uint64 {
	if s.l == nil {
		return 0
	}
	return s.l.changed
}

// pagePrefix starts the address of the team pages waiting on an instance for the answer to what they sent:
// "page:<instance>". Laptops are addressed by their device id.
const pagePrefix = "page:"

// replyTo is where the recipient's answer goes: the sending laptop, wherever it reconnects, or this instance, where
// the page waits.
func (h *Hub) replyTo(s sender) string {
	if s.l != nil {
		return s.l.dev
	}
	return pagePrefix + h.id
}

// route takes a message from a laptop on this instance to its recipient, wherever that laptop is connected.
func (h *Hub) route(l *link, m protocol.Msg) {
	h.send(sender{handle: l.handle, l: l, answer: l.send}, m)
}

// send takes a message to its recipients, after the checks and limits every message goes through, whether it comes
// from a laptop or the team page.
func (h *Hub) send(s sender, m protocol.Msg) {
	ack := func(ok bool, note, err string) {
		if !ok {
			h.log.Info("message refused", "id", m.ID, "sender", s.handle, "to", m.To, "reason", err)
		}
		s.answer(protocol.Frame{T: protocol.FrameAck, ID: m.ID, OK: ok, Note: note, Err: err})
	}
	if len(m.Text) > 4*protocol.MaxText { // far over the limit: refused before it costs any cleaning
		ack(false, "", fmt.Sprintf("the message is %d characters; the limit is %d", utf8.RuneCountInString(m.Text), protocol.MaxText))
		return
	}
	m.Text = strings.TrimSpace(protocol.Clean(m.Text, true)) // before taking the lock, like agent fields
	m.Topic = protocol.CleanTopic(m.Topic)
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case !msgID.MatchString(m.ID): // it names stored messages and the keys laptops remember messages by
		ack(false, "", "a message id is 1 to 64 letters, digits and dashes")
		return
	case strings.HasPrefix(m.ID, "gh-"), strings.HasPrefix(m.ID, "an-"), strings.HasPrefix(m.ID, protocol.LateBtw):
		// the hub's merge notices and announcements, and the late answers by the way a laptop hands its own agents,
		// which consoles show as answers, never as messages
		ack(false, "", "message ids starting with gh-, an- or bw- belong to murmur's own messages")
		return
	case h.waiting[m.ID] != nil:
		// a retry of a message already on its way, from the laptop that sent it: answer on its new connection. The
		// same id from another laptop is not that message, and doesn't get its answer.
		return
	case m.Text == "":
		ack(false, "", "the message is empty")
		return
	case utf8.RuneCountInString(m.Text) > protocol.MaxText:
		ack(false, "", fmt.Sprintf("the message is %d characters; the limit is %d", utf8.RuneCountInString(m.Text), protocol.MaxText))
		return
	}
	from := s.handle
	if m.From != "" {
		if s.l == nil || !slices.ContainsFunc(s.l.agents, func(a protocol.Agent) bool { return a.Name == m.From }) {
			ack(false, "", "this session is not on murmur yet; try again in a second")
			return
		}
		from += "/" + m.From
	}
	if repo, ok := strings.CutPrefix(strings.TrimSpace(m.To), protocol.RepoAddress); ok {
		m.From = from
		h.announceLocked(s, m, repo, ack)
		return
	}
	toPerson, toAgent := protocol.Split(m.To)
	u := h.handles[toPerson]
	if u == nil || u.Disabled {
		ack(false, "", fmt.Sprintf("nobody called %q is on murmur; `who` lists the team", toPerson))
		return
	}
	m.From, m.To, m.At = from, u.Handle, time.Now().UnixMilli()
	if toAgent != "" {
		m.To += "/" + toAgent
	}
	m.Announce = ""                           // only the hub's copies of an announcement carry it
	if utf8.RuneCountInString(toAgent) > 80 { // no agent is named that long; checked before anything is counted
		ack(false, "", fmt.Sprintf("%s is not running. Send to %s to reach the person, or check `who`.", protocol.Cut(m.To, 120), u.Handle))
		return
	}
	var host *holder
	if toAgent != "" {
		host = h.hostLocked(u.Handle, toAgent)
	}
	// two agents are counted as a pair only when the recipient exists, so made-up names can't fill the hub's memory
	if why := h.limitLocked(from, m.To, host != nil); why != "" {
		ack(false, "", why)
		return
	}
	f := protocol.Frame{T: protocol.FrameMsg, Msg: &m}
	people := []string{protocol.PersonOf(m.From), u.Handle}
	cause := s.causeLocked()

	if toAgent != "" {
		if host != nil {
			h.countCrossedLocked(m)
		}
		why := fmt.Sprintf("%s is not running. Send to %s to reach the person, or check `who`.", m.To, u.Handle)
		switch {
		case host == nil && s.l != nil:
			// a message held for the agent's laptop, sent again from an outbox once the hub stopped listing that laptop,
			// is still on its way: its sender hears once it is delivered or given up on
			dev, email := s.l.dev, u.Email
			go func() {
				ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
				defer done()
				kept, err := h.store.Pending(ctx, email)
				if err == nil && slices.ContainsFunc(kept, func(p store.Pending) bool {
					return p.Msg.ID == m.ID && p.Sender == dev && p.Device != ""
				}) {
					return
				}
				ack(false, "", why)
			}()
		case host == nil:
			ack(false, "", why)
		case host.online:
			h.toDeviceLocked(host.dev, f, h.replyTo(s), cause)
			h.toPeopleLocked(people, protocol.Frame{T: protocol.FrameMsg, Msg: &m, Echo: true}, []string{host.dev}, cause)
			h.log.Info("message sent", "id", m.ID, "from", m.From, "to", m.To, "chars", len(m.Text))
		default: // its laptop dropped a moment ago: it gets the message when it's back, on any instance
			h.keepLocked(s, m, u.Email, host.dev, holdFor, "its laptop is reconnecting", "")
		}
		return
	}
	var targets []string
	for x := range h.holdersOfLocked(u.Handle) {
		if x.online {
			targets = append(targets, x.dev)
			h.toDeviceLocked(x.dev, f, h.replyTo(s), cause)
		}
	}
	if len(targets) == 0 { // the sender hears it is kept only once it is, in the store
		h.keepLocked(s, m, u.Email, "", keepFor, "its person is offline",
			fmt.Sprintf("%s is offline and gets it when murmur is back on their laptop", u.Handle))
		return
	}
	h.toPeopleLocked(people, protocol.Frame{T: protocol.FrameMsg, Msg: &m, Echo: true}, targets, cause)
	h.log.Info("message sent", "id", m.ID, "from", m.From, "to", m.To, "laptops", len(targets), "chars", len(m.Text))
}

// limitLocked counts a send and says why it is refused, if it is: too many from one sender in a minute, or two
// agents writing to each other too long without a person.
func (h *Hub) limitLocked(from, to string, toAgent bool) string {
	s, now := h.settings, time.Now()
	recent := slices.DeleteFunc(h.sends[from], func(t time.Time) bool { return now.Sub(t) > time.Minute })
	if len(recent) >= s.SendsPerMinute {
		return protocol.TooMany
	}
	h.sends[from] = append(recent, now)
	if strings.Contains(from, "/") && toAgent {
		window := time.Duration(s.LoopMinutes) * time.Minute
		p := h.pair(from, to)
		p.msgs = slices.DeleteFunc(p.msgs, func(t time.Time) bool { return now.Sub(t) > window })
		if len(p.msgs) >= s.LoopMax {
			return fmt.Sprintf("paused: %s and %s exchanged %d messages in %s without a person. Ask your person whether to go on.",
				from, to, s.LoopMax, window)
		}
		p.msgs = append(p.msgs, now)
	}
	return ""
}

// keepLocked stores a message until a laptop of its recipient connects, on any instance. Once it is stored, the
// sender hears ok, when ok is set; otherwise the sending laptop hears once it is delivered, and a page, which can't
// hear later, hears now that the message is kept.
func (h *Hub) keepLocked(s sender, m protocol.Msg, email, dev string, ttl time.Duration, why, ok string) {
	now := time.Now()
	p := store.Pending{Msg: m, Email: email, Device: dev, Created: now, Expires: now.Add(ttl)}
	if s.l != nil {
		p.Sender = s.l.dev
	}
	go func() {
		ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := h.store.AddPending(ctx, p); err != nil {
			h.log.Error("database", "op", "keep a message for later", "id", m.ID, "error", err.Error())
			s.answer(protocol.Frame{T: protocol.FrameAck, ID: m.ID, Err: "could not keep the message for later: " + err.Error()})
			return
		}
		switch {
		case ok != "":
			s.answer(protocol.Frame{T: protocol.FrameAck, ID: m.ID, OK: true, Note: ok})
		case s.l == nil:
			s.answer(protocol.Frame{T: protocol.FrameAck, ID: m.ID, OK: true, Note: "kept for later: " + why})
		}
		h.mu.Lock()
		h.toPeopleLocked([]string{protocol.PersonOf(m.From), protocol.PersonOf(m.To)}, protocol.Frame{T: protocol.FrameMsg, Msg: &m, Echo: true}, nil,
			s.causeLocked())
		// a laptop of the recipient that connected while the message was being stored read the store before it: it
		// gets the message now, as it would have from the store (a laptop takes a message once, whatever the copies)
		for _, l := range h.links {
			if l.email == email && l.gone.IsZero() && (dev == "" || l.dev == dev) {
				h.waiting[m.ID] = &waiter{from: p.Sender, email: email, stored: true, created: time.Now()}
				l.send(protocol.Frame{T: protocol.FrameMsg, Msg: &m})
			}
		}
		h.mu.Unlock()
		h.log.Info("message kept", "id", m.ID, "from", m.From, "to", m.To, "why", why)
	}()
}

// hostLocked finds the laptop running an agent: one that is online first, else one reconnecting. When two of its
// person's laptops run an agent of that name, it is the one the roster lists.
func (h *Hub) hostLocked(handle, agent string) *holder {
	var host *holder
	var its protocol.Agent
	for x := range h.holdersOfLocked(handle) {
		i := slices.IndexFunc(x.agents, func(a protocol.Agent) bool { return a.Name == agent })
		if i < 0 || (!x.online && !h.allowedLocked(x.dev)) { // a laptop revoked since never comes back for it
			continue
		}
		if host == nil || sooner(x.agents[i], x, its, *host) {
			host, its = &x, x.agents[i]
		}
	}
	return host
}

// sooner says whether agent a, on laptop x, goes before agent b, on laptop y, of the same person and name: the one
// on a laptop online, then the one waiting for its person, as the team page shows it, then the latest to change.
func sooner(a protocol.Agent, x holder, b protocol.Agent, y holder) bool {
	switch {
	case x.online != y.online:
		return x.online
	case waitingFor(a) != waitingFor(b):
		return waitingFor(a)
	case a.Since != b.Since:
		return a.Since > b.Since
	}
	return x.dev < y.dev
}

// delivered passes the recipient's answer, from laptop l, on to the laptop that sent the message. Laptops choose
// message ids, so another person's laptop that knows one can't answer for the recipient.
func (h *Hub) delivered(l *link, f protocol.Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w := h.waiting[f.ID]
	if w == nil || w.email != l.email {
		return
	}
	delete(h.waiting, f.ID)
	h.log.Info("message delivered", "id", f.ID, "ok", f.OK, "error", f.Err, "note", f.Note)
	if w.stored {
		go func() {
			ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			if err := h.store.DropPending(ctx, w.email, f.ID); err != nil {
				h.log.Error("database", "op", "drop a delivered message", "id", f.ID, "error", err.Error())
			}
		}()
	}
	if w.from == "" {
		return
	}
	note := ""
	if f.OK {
		note = "delivered"
		if f.Note != "" {
			note = f.Note
		}
	}
	h.answerLocked(w.from, protocol.Frame{T: protocol.FrameAck, ID: f.ID, OK: f.OK, Note: note, Err: f.Err})
}

// answerLocked passes the answer about a message to its sender: a laptop, wherever it is connected, or a team page
// waiting on some instance.
func (h *Hub) answerLocked(to string, f protocol.Frame) {
	inst, page := strings.CutPrefix(to, pagePrefix)
	switch {
	case !page:
		h.toDeviceLocked(to, f, "", 0)
	case inst == h.id:
		h.pageAnswerLocked(f)
	default:
		go h.publish(context.Background(), evAnswer, f, inst)
	}
}

// pageAnswerLocked hands an answer to the page on this instance waiting for it, if it still waits.
func (h *Hub) pageAnswerLocked(f protocol.Frame) {
	if c := h.pages[f.ID]; c != nil {
		select {
		case c <- f:
		default:
		}
	}
}

func pairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "\x00" + b
}

func (h *Hub) pair(a, b string) *pair {
	k := pairKey(a, b)
	p := h.pairs[k]
	if p == nil {
		p = &pair{}
		h.pairs[k] = p
	}
	return p
}

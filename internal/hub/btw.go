package hub

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

// Questions by the way: a person, or an agent, asks an agent something without interrupting it. A throwaway copy of the
// agent's conversation answers, on the laptop of the agent's person, and the agent never sees the question. The hub only
// carries questions and answers between the two laptops: it checks a question, writes who asks, hands it to the
// laptop holding the agent, and passes that laptop's answers back to the laptop that asked, wherever each is
// connected. It keeps none of it in the store, and its logs never say what a question or an answer says. A laptop
// that didn't say protocol.CanBtw never gets either: an older laptop would drop it, and its asker would wait for
// nothing.

// asideFor is how long the hub remembers a question by the way: as long as the asking laptop asks it again, once its
// final answer has gone too, which may not have reached it.
const asideFor = 24 * time.Hour

// maxAnswers is how many answers by the way the hub takes from one laptop in a minute, and how many notes, each
// counted apart: each may be an event on the bus, a row in Postgres. One past them is dropped, and the asking laptop
// asks again. The final answer to a question this instance sent the laptop isn't counted: one passes each time the
// question went, which the askers' limits bound. So the notes a laptop sends again for every question asked again,
// and answers to questions the hub doesn't know, never crowd out the answers its person shares.
const maxAnswers = 60

// askAgainEvery is how often a question that a laptop asks again on one connection goes on to the same connection of
// the laptop holding the agent, at most: that laptop answers it from its history each time, and its answers count. A
// laptop asks again every two minutes, and on each new connection, which is a new link; once the laptop holding the
// agent connected again, it may have the answer that didn't leave it, and the question goes at once.
const askAgainEvery = time.Minute

// went is when a question went on from a link, and to which link of the laptop holding the agent, nil when that
// laptop isn't connected here.
type went struct {
	at time.Time
	to *link
}

// aside is a question by the way on its way, on the instance where it was asked: the laptop that asked it, where its
// answers go, and the laptop holding the agent, the only one whose answers pass. Answers that reach another instance
// come here through the bus. It stays once its final answer has gone: a question asked again goes to the same laptop,
// which answers it from its history.
type aside struct {
	from, to     string // device ids; to is "" while every laptop of the agent's person is asked (sendAskLocked)
	asker, agent string // who asks and the agent asked, as the hub wrote them in the question
	final        bool   // its final answer went to the asking laptop: another goes only once it asks again
	created      time.Time
}

// btwEvent is an answer by the way that reached an instance which doesn't know its question, for the instance where
// it was asked. Device is the laptop that answered, as the instance holding it knows it.
type btwEvent struct {
	Device string       `json:"device"`
	Btw    protocol.Btw `json:"btw"`
}

// carries says whether a laptop that said protocol.CanBtw, or not, may get a frame.
func carries(btw bool, f protocol.Frame) bool {
	return btw || (f.T != protocol.FrameBtw && f.T != protocol.FrameBtwAnswer)
}

// ask takes a question by the way from a laptop here to the laptop holding the agent asked, or tells the asking
// laptop why not, in a final answer. The laptop that asked a question asks it again under its id after a reconnect,
// and every two minutes, until the answer comes: it goes again to the laptop it went to, which answers it from its
// history, once every askAgainEvery at most on one connection. While that laptop is away, the asking laptop hears so
// in a note, and asks again. A question comes from the laptop's person, or from one of the agents it runs, as its
// messages do.
func (h *Hub) ask(l *link, b protocol.Btw) {
	if !l.btw { // it would not read the refusal either
		return
	}
	id := msgID.MatchString(b.ID) // before cleaning, which cuts an id that is too long to one that isn't
	b.Clean()                     // before taking the lock, like agent fields
	_, agent := protocol.Split(b.From)
	again := b.Again
	b.From, b.Answer, b.Err, b.Note, b.Tokens, b.Again = l.handle, "", "", "", 0, false
	h.mu.Lock()
	defer h.mu.Unlock()
	why, final := h.askLocked(l, &b, agent, id, again)
	switch {
	case why == "":
		return
	case !final:
		l.send(protocol.Frame{T: protocol.FrameBtwAnswer, Btw: &protocol.Btw{ID: b.ID, From: b.From, To: b.To, Note: why,
			At: time.Now().UnixMilli()}})
		return
	}
	h.log.Info("question by the way refused", "id", b.ID, "from", b.From, "to", b.To, "reason", why)
	// asked again, and refused now: it is over. A refusal may come before askLocked wrote the agent as the question
	// it remembers says it
	person, asked := protocol.Split(b.To)
	if a := h.asides[b.ID]; a != nil && a.from == l.dev && a.agent == person+"/"+asked && a.asker == b.From {
		delete(h.asides, b.ID)
	}
	l.send(protocol.Frame{T: protocol.FrameBtwAnswer, Btw: &protocol.Btw{ID: b.ID, From: b.From, To: b.To, Err: why,
		At: time.Now().UnixMilli()}})
}

// askLocked checks a question and hands it on, or says why it can't: in a final refusal, or, not final, in a note while
// the laptop holding the agent is away. agent is the asking laptop's agent that asks, "" for its person; id says
// whether the question's id was one; again, that the laptop asks again a question it asked before.
func (h *Hub) askLocked(l *link, b *protocol.Btw, agent string, id, again bool) (why string, final bool) {
	if agent != "" {
		b.From += "/" + agent
	}
	known := h.asides[b.ID]
	person, asked := protocol.Split(b.To)
	switch {
	case !h.settings.Btw:
		return "this team's admins turned questions by the way off", true
	case !id:
		return "a question's id is 1 to 64 letters, digits and dashes", true
	case known != nil && known.from != l.dev: // the answer goes to the laptop that asked first
		return "another laptop asked a question under this id; ask under a new one", true
	case b.Question == "":
		return "the question is empty", true
	case utf8.RuneCountInString(b.Question) > protocol.MaxBtwQuestion:
		return fmt.Sprintf("the question is %d characters; the limit is %d", utf8.RuneCountInString(b.Question), protocol.MaxBtwQuestion), true
	case strings.HasPrefix(person, protocol.RepoAddress), person == "", asked == "":
		return "questions by the way go to one agent, like bob/main-1", true
	case agent != "" && !slices.ContainsFunc(l.agents, func(a protocol.Agent) bool { return a.Name == agent }):
		return "your session is not on murmur yet; ask again in a few seconds", true
	}
	u := h.handles[person]
	if u == nil || u.Disabled {
		return fmt.Sprintf("nobody called %q is on murmur; `who` lists the team", person), true
	}
	b.To = u.Handle + "/" + asked
	if b.To == b.From {
		return "an agent can't ask itself by the way", true
	}
	b.At = time.Now().UnixMilli()
	q := *b
	question := protocol.Frame{T: protocol.FrameBtw, Btw: &q}
	away := fmt.Sprintf("%s's laptop is away; murmur asks again until it is back", u.Handle)
	if known != nil { // asked again: the laptop it went to has its answer, or its place in the line, even once the agent is gone
		if known.agent != b.To || known.asker != b.From {
			return "a question was asked under this id already; ask under a new one", true
		}
		if w, ok := l.went[b.ID]; ok && time.Since(w.at) < askAgainEvery && w.to == h.links[known.to] {
			return "", false // it went a moment ago, between the same connections: its answer comes, or it is asked again
		}
		known.final = false // the asking laptop hasn't got the final answer: the next one goes to it
		if !h.sendAskLocked(known, question) {
			return away, false
		}
		l.wentOn(b.ID, h.links[known.to])
		h.log.Debug("question by the way sent again", "id", b.ID, "from", b.From, "to", b.To)
		return "", false
	}
	host := h.hostLocked(u.Handle, asked)
	again = again && h.againLocked(protocol.PersonOf(b.From))
	switch {
	case host == nil && again:
		// asked before, of an instance that doesn't know it: one that started since, or the asking laptop moved here. The
		// agent's laptop may not be back yet, or may hold the answer once the agent is gone
		a := &aside{from: l.dev, asker: b.From, agent: b.To, created: time.Now()}
		h.asides[b.ID] = a
		if !h.sendAskLocked(a, question) {
			return away, false
		}
		l.wentOn(b.ID, nil)
		return fmt.Sprintf("%s isn't on murmur now; murmur asks %s's laptops again until the one that has it answers", b.To, u.Handle), false
	case host == nil:
		return fmt.Sprintf("%s is not running; `who` lists the agents you can ask", b.To), true
	case !host.btw:
		return fmt.Sprintf("%s's laptop runs an older murmur, which can't answer questions by the way", u.Handle), true
	}
	// whether the agent's person lets teammates ask is checked whatever the asking laptop says: one that says it asks
	// again may never have asked. A person's own agents always answer them and their agents
	i := slices.IndexFunc(host.agents, func(a protocol.Agent) bool { return a.Name == asked })
	switch btw := host.agents[i].Btw; {
	case u.Handle == l.handle:
	case btw == protocol.BtwMine:
		return fmt.Sprintf("%s doesn't take questions by the way: %s turned them off", b.To, u.Handle), true
	case btw == "":
		return fmt.Sprintf("%s doesn't take questions by the way: %s turned them off, or its coding tool can't answer them",
			b.To, u.Handle), true
	}
	if !again { // one asked again was counted where it was first asked
		if why := h.askLimitLocked(b.From, b.To); why != "" {
			return why, true
		}
	}
	h.asides[b.ID] = &aside{from: l.dev, to: host.dev, asker: b.From, agent: b.To, created: time.Now()}
	if !host.online { // reconnecting: the question goes when the laptop asks it again
		return away, false
	}
	h.toDeviceLocked(host.dev, question, "", 0)
	l.wentOn(b.ID, h.links[host.dev])
	h.log.Info("question by the way sent", "id", b.ID, "from", b.From, "to", b.To)
	return "", false
}

// wentOn records that question id, which the laptop asked on this connection, went on now to connection to of the
// laptop holding the agent, and forgets those that went long enough ago to go again.
func (l *link) wentOn(id string, to *link) {
	now := time.Now()
	if l.went == nil {
		l.went = map[string]went{}
	}
	maps.DeleteFunc(l.went, func(_ string, w went) bool { return now.Sub(w.at) >= askAgainEvery })
	l.went[id] = went{now, to}
}

// sendAskLocked sends a question to the laptop it went to, or, when the hub doesn't know which (a.to ""), to every
// laptop of the agent's person that carries questions by the way, saying it is asked again: the one that saw it
// answers from its history, and the others, which don't run the agent, say nothing. It says whether the question went.
func (h *Hub) sendAskLocked(a *aside, f protocol.Frame) bool {
	if a.to != "" {
		return h.toDeviceLocked(a.to, f, "", 0)
	}
	q := *f.Btw
	q.Again = true
	sent := false
	for x := range h.holdersOfLocked(protocol.PersonOf(a.agent)) {
		if x.online && x.btw && h.toDeviceLocked(x.dev, protocol.Frame{T: protocol.FrameBtw, Btw: &q}, "", 0) {
			sent = true
		}
	}
	return sent
}

// againLocked says whether a question a person's laptop asks again, which this instance doesn't know, skips the
// limits: it was counted where it was first asked, by this instance before it restarted or by another. A person's
// laptops ask again at most BtwPerHour of those an hour, as many as they could have asked: past that, a laptop that
// says it asks again is counted like one that asks.
func (h *Hub) againLocked(person string) bool {
	now := time.Now()
	ts := slices.DeleteFunc(h.asksAgain[person], func(t time.Time) bool { return now.Sub(t) > time.Hour })
	if len(ts) >= h.settings.BtwPerHour {
		h.asksAgain[person] = ts
		return false
	}
	h.asksAgain[person] = append(ts, now)
	return true
}

// askLimitLocked counts a question by the way, and says why it is refused, if it is: too many questions from one
// person and their agents in an hour, too many messages and questions from one sender in a minute, which messages
// count too, or two agents asking and writing each other too long without a person, which their messages count too.
func (h *Hub) askLimitLocked(from, to string) string {
	s, now := h.settings, time.Now()
	person := protocol.PersonOf(from)
	hour := slices.DeleteFunc(h.asks[person], func(t time.Time) bool { return now.Sub(t) > time.Hour })
	minute := slices.DeleteFunc(h.sends[from], func(t time.Time) bool { return now.Sub(t) > time.Minute })
	h.asks[person], h.sends[from] = hour, minute
	var p *pair
	window := time.Duration(s.LoopMinutes) * time.Minute
	if strings.Contains(from, "/") { // an agent asks an agent: a loop, like their messages
		p = h.pair(from, to)
		p.msgs = slices.DeleteFunc(p.msgs, func(t time.Time) bool { return now.Sub(t) > window })
	}
	switch {
	case len(hour) >= s.BtwPerHour && p != nil:
		return fmt.Sprintf("your person and their agents asked %d questions by the way in the last hour, as many as this team "+
			"allows; ask again later", len(hour))
	case len(hour) >= s.BtwPerHour:
		return fmt.Sprintf("you asked %d questions by the way in the last hour, as many as this team allows; ask again later", len(hour))
	case len(minute) >= s.SendsPerMinute:
		return "too many messages and questions in the last minute; wait a moment"
	case p != nil && len(p.msgs) >= s.LoopMax:
		return fmt.Sprintf("paused: %s and %s exchanged %d messages and questions by the way in %s without a person. "+
			"Ask your person whether to go on.", from, to, s.LoopMax, window)
	}
	h.asks[person], h.sends[from] = append(hour, now), append(minute, now)
	if p != nil {
		p.msgs = append(p.msgs, now)
	}
	return ""
}

// answered takes an answer by the way from a laptop here to the laptop that asked: the final answer to a question
// this instance sent it always, any other maxAnswers a minute at most, and notes as many. An answer to a question
// this instance doesn't know, asked on another instance or before the answering laptop moved here, or one whose final
// answer it passed on already, which the asking laptop may have asked again on another instance, goes to the other
// instances, if there are any, and the one where it was asked passes it on.
func (h *Hub) answered(l *link, b protocol.Btw) {
	if !l.btw {
		return
	}
	b.Clean()
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.asides[b.ID]
	if b.Final() && h.answersLocked(a, l.dev) {
		h.passAnswerLocked(l.dev, b)
		return
	}
	now, count := time.Now(), &l.answers
	if !b.Final() {
		count = &l.notes
	}
	*count = slices.DeleteFunc(*count, func(t time.Time) bool { return now.Sub(t) > time.Minute })
	if len(*count) >= maxAnswers {
		return
	}
	*count = append(*count, now)
	if a == nil || a.final {
		if len(h.remote) > 0 {
			go h.publish(context.Background(), evBtw, btwEvent{Device: l.dev, Btw: b}, "")
		}
		return
	}
	h.passAnswerLocked(l.dev, b)
}

// answersLocked says whether laptop dev answers question a: the laptop the question went to, or one of the agent's
// person's while the hub asks them all. None does once the final answer has gone, until the asking laptop asks again.
func (h *Hub) answersLocked(a *aside, dev string) bool {
	return a != nil && !a.final && (a.to == dev || (a.to == "" && h.handleOfLocked(dev) == protocol.PersonOf(a.agent)))
}

// passAnswerLocked passes an answer from laptop dev to the laptop that asked, if dev answers the question
// (answersLocked), and is the one asked from then on. Anything else is dropped: an answer to a question nobody asked,
// from another laptop, or once the final answer has gone, until the asking laptop asks again. So is an answer for a
// laptop that is offline now: it asks again once it is back.
func (h *Hub) passAnswerLocked(dev string, b protocol.Btw) {
	a := h.asides[b.ID]
	if !h.answersLocked(a, dev) {
		return
	}
	a.to = dev
	b.From, b.To = a.asker, a.agent // as the question said them, whatever the answering laptop says
	sent := h.toDeviceLocked(a.from, protocol.Frame{T: protocol.FrameBtwAnswer, Btw: &b}, "", 0)
	if b.Final() {
		a.final = true
		h.log.Info("question by the way answered", "id", b.ID, "from", b.From, "to", b.To, "answered", b.Answer != "",
			"tokens", b.Tokens, "passed", sent)
	}
}

// handleOfLocked is the person a laptop connected to this instance or another belongs to, or "".
func (h *Hub) handleOfLocked(dev string) string {
	if l := h.links[dev]; l != nil {
		return l.handle
	}
	if p := h.remote[dev]; p != nil {
		return p.Handle
	}
	return ""
}

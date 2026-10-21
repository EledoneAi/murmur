package daemon

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/harness"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/setup"
)

// Questions by the way: a person, or an agent, asks an agent something without interrupting it. A throwaway copy of
// the agent's conversation answers, on the laptop of the agent's person, and the agent never sees the question:
// questions and answers stay out of hook answers, inboxes and the message history. A question from this laptop (its
// person's, or one of its agents') for one of this laptop's agents runs here, with no hub. Any other goes through the
// hub to the laptop holding the agent, which answers it: at once for its own person and their agents; for a teammate
// or a teammate's agent as its person chose (each answer waits for them, answers go on their own, or teammates can't
// ask). A teammate's question for an agent idle for over an hour waits for its person's go before the copy runs: a
// cold copy reads the whole conversation again at full price. An agent waits for its answer less than a minute, as
// its tool call does: an answer that comes later reaches it as a message.

const (
	copyWait   = 2 * time.Minute // how long a copy has to answer
	lineWait   = 5 * time.Minute // how long a question waits for its turn at a copy
	maxCopies  = 2               // copies running at once on a laptop
	maxLine    = 2               // questions waiting behind a session's copy; one more is refused
	btwPerHour = 20              // teammates' questions this laptop answers in an hour: the copies are its person's cost
	coldAfter  = time.Hour       // an agent idle this long has its person's go before a teammate's copy reads it all
	asideTTL   = 24 * time.Hour  // how long a question is asked again, and kept after it last changed
	askAgain   = 2 * time.Minute // how often a question without its answer goes to the hub again
	btwWait    = 2 * time.Minute // how long btw-wait waits for an answer
	lateTries  = 30              // how often a late answer that didn't reach its agent goes again: an hour of btwTicks
	// how long a daemon that stops waits for its copies to be gone: under the two seconds a newer murmur waits for its
	// socket to go before it starts another
	copyHalt = 1500 * time.Millisecond
)

// agentWait is how long btw-wait waits for an agent's answer: its btw tool stops waiting at 50 seconds.
const agentWait = 45 * time.Second

// aside is a question asked by the way, as this laptop keeps it, in memory and in the history.
type aside struct {
	ipc.Aside
	Local   bool   `json:"local,omitempty"`   // Mine, of one of this laptop's agents: answered here, with no hub
	Told    string `json:"told,omitempty"`    // the error its asker heard, when it isn't Err: a sign-in failure's
	Counted bool   `json:"counted,omitempty"` // a teammate's question, counted in the hour's btwPerHour
	Last    int64  `json:"last,omitempty"`    // unix ms of its last change: it is kept a day after that
	Went    bool   `json:"went,omitempty"`    // Mine: it went to a hub, and goes again saying so (protocol.Btw.Again)
	// By is the session key of the agent of this laptop's that asked, when one did (From names it). Its final answer
	// reaches it once: through its btw-wait, or later as a message. Handed says it did, or never will: the agent closed.
	By     string `json:"by,omitempty"`
	Handed bool   `json:"handed,omitempty"`

	unsent bool      // its latest answer didn't reach the hub: it goes when the hub welcomes this laptop again
	sent   time.Time // when this laptop's question last went to the hub
	expect bool      // an agent's question whose btw-wait is on its way: the answer waits for it
	retry  bool      // Handed, but the message didn't reach the agent: it goes again on the next btwTick
	tries  int       // how often that message didn't reach the agent
}

// key names an aside: the id chosen here for a question asked here; the id and a digest of who asks whom what for the
// others. A teammate's laptop chooses the id, so a question under a known id with another text, from another person
// or to another agent, never gets the answer kept for the first.
func (a *aside) key() string {
	if a.Mine {
		return a.ID
	}
	return btwKey(a.Btw)
}

func btwKey(b protocol.Btw) string {
	sum := sha256.Sum256([]byte(b.From + "\x00" + b.To + "\x00" + b.Question))
	return b.ID + "/" + hex.EncodeToString(sum[:8])
}

// final says the answer is in, or won't come.
func (a *aside) final() bool { return a.State == ipc.AsideAnswered || a.State == ipc.AsideFailed }

// told is the aside as its asker hears it: never a held answer, nor what a sign-in failure says of this laptop.
func (a *aside) told() protocol.Btw {
	b := protocol.Btw{ID: a.ID, From: a.From, To: a.To, At: time.Now().UnixMilli()}
	switch a.State {
	case ipc.AsideAnswered:
		b.Answer, b.Tokens = a.Answer, a.Tokens
	case ipc.AsideFailed:
		b.Err, b.Tokens = cmp.Or(a.Told, a.Err), a.Tokens
	case ipc.AsideRunning:
		b.Note = "being answered"
	default:
		b.Note = a.Note
	}
	return b
}

// btwID is what an id may be, as the hub checks it.
var btwID = regexp.MustCompile(`^[0-9A-Za-z-]{1,64}$`)

// ---------------------------------------------------------------- asking

// askBtw puts a question to an agent, from this person, or from one of this laptop's agents when the request names
// its session as send's does (Harness, PID, Session). A question for one of this laptop's agents runs a copy here,
// with no hub; any other goes through the hub, which must carry questions by the way. It returns once the question is
// on its way.
func (d *Daemon) askBtw(req ipc.Request) ipc.Reply {
	// as send takes a message: a model may have written it, and the console shows it
	id, text := strings.TrimSpace(req.ID), strings.TrimSpace(protocol.Clean(req.Text, true))
	req.To = protocol.Clean(req.To, false)
	switch n := utf8.RuneCountInString(text); {
	case !btwID.MatchString(id):
		return ipc.Reply{Err: "a question by the way needs an id of 1 to 64 letters, digits and dashes"}
	case n == 0:
		return ipc.Reply{Err: "the question is empty"}
	case n > protocol.MaxBtwQuestion:
		return ipc.Reply{Err: fmt.Sprintf("the question is %d characters; the limit is %d", n, protocol.MaxBtwQuestion)}
	case len(req.To) > 1024:
		return ipc.Reply{Err: "no address on murmur is that long; `who` lists the team"}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	from, by := d.me.Handle, ""
	if req.PID != 0 || req.Session != "" {
		s := d.senderLocked(req)
		if s == nil || s.name == "" {
			return ipc.Reply{Err: "this session is not on murmur yet; try again in a few seconds"}
		}
		from, by = d.me.Handle+"/"+s.name, s.key()
	}
	to := d.addressLocked(req.To)
	person, agent := protocol.Split(to)
	switch {
	case agent == "" || strings.HasPrefix(to, protocol.RepoAddress):
		return ipc.Reply{Err: "ask one agent, like bob/main-1"}
	case to == from:
		return ipc.Reply{Err: "an agent can't ask itself by the way"}
	}
	if a := d.asides[id]; a != nil {
		if a.Mine && a.From == from && a.To == to && a.Question == text { // the same call again
			return ipc.Reply{OK: true}
		}
		return ipc.Reply{Err: "a question was asked under this id already; ask under a new one"}
	}
	a := &aside{Aside: ipc.Aside{Btw: protocol.Btw{ID: id, From: from, To: to, Question: text, At: time.Now().UnixMilli()},
		Mine: true}, By: by, expect: by != ""}
	if s := d.byNameLocked(agent); s != nil && person == d.me.Handle {
		if why := d.unaskable(s); why != "" {
			return ipc.Reply{Err: why}
		}
		a.Local, a.State = true, ipc.AsideRunning
		d.keepAsideLocked(a)
		go d.runAside(id)
		return ipc.Reply{OK: true}
	}
	switch {
	case d.solo && person == d.me.Handle:
		return ipc.Reply{Err: fmt.Sprintf("%s is not running on this laptop; `who` lists the agents you can ask", to)}
	case d.solo:
		return ipc.Reply{Err: soloBtw}
	case !d.hubBtw && !d.welcomed:
		return ipc.Reply{Err: "Murmur is " + d.state + "; ask again once it is online"}
	case !d.hubBtw:
		return ipc.Reply{Err: noHubBtw}
	}
	a.State = ipc.AsideAsking
	if !d.sendAskLocked(a) {
		a.Note = offline
	}
	d.keepAsideLocked(a)
	return ipc.Reply{OK: true, Note: a.Note}
}

// addressLocked reads whom this person asks: bob/main-1, or main-1 for one of their own agents.
func (d *Daemon) addressLocked(to string) string {
	to = strings.TrimSpace(to)
	if !strings.Contains(to, "/") && to != "" {
		own := d.byNameLocked(to) != nil
		for _, p := range d.people {
			own = own || (p.Handle == d.me.Handle && slices.ContainsFunc(p.Agents, func(a protocol.Agent) bool { return a.Name == to }))
		}
		if own {
			return d.me.Handle + "/" + to
		}
	}
	person, agent := protocol.Split(to)
	if agent == "" {
		return to
	}
	return person + "/" + agent
}

// soloBtw is why a laptop on its own (murmur solo) can't ask an agent other than its own: its hub is the daemon's,
// which only this laptop reaches.
const soloBtw = "on your own (murmur solo), only this laptop's agents can be asked by the way: a teammate's agent needs " +
	"your team's hub, which murmur join moves this laptop to"

// noHubBtw is why this person can't ask through a hub that doesn't carry questions by the way.
const noHubBtw = "the hub doesn't carry questions by the way: the team's admins turned them off, or it runs an older murmur"

// offline is the note of this person's question asked while the hub was out of reach.
const offline = "waits for murmur to reconnect"

// sendAskLocked sends this laptop's question to the hub, and says whether it went. From names the agent that asks, if
// one does, for the hub to check it runs here; the hub writes the person. A question that went before says so.
func (d *Daemon) sendAskLocked(a *aside) bool {
	if d.out == nil {
		return false
	}
	b := protocol.Btw{ID: a.ID, From: a.From, To: a.To, Question: a.Question, At: a.At, Again: a.Went}
	select {
	case d.out <- protocol.Frame{T: protocol.FrameBtw, Btw: &b}:
		a.sent, a.Went = time.Now(), true
		return true
	default:
		return false
	}
}

// askAgainLocked sends this laptop's question without an answer to the hub again: the hub keeps nothing for a laptop
// that was offline, and the laptop holding the agent may have restarted before its answer left. That laptop answers a
// question it has seen from its history. An agent's question whose agent has closed fails instead: nobody would hear
// its answer.
func (d *Daemon) askAgainLocked(a *aside) {
	if a.By != "" && d.sessions[a.By] == nil {
		_, agent := protocol.Split(a.From)
		a.State, a.Err, a.Note, a.Handed = ipc.AsideFailed, agent+" closed before the answer came", "", true
		d.keepAsideLocked(a)
		return
	}
	went := a.Went
	if d.sendAskLocked(a) && (a.Note == offline || !went) {
		if a.Note == offline {
			a.Note = ""
		}
		d.keepAsideLocked(a)
	}
}

// onBtwAnswer takes an answer to this laptop's question from the hub: a note while it waits, then the final one.
func (d *Daemon) onBtwAnswer(b protocol.Btw) {
	b.Clean()
	b.Answer = cutRunes(b.Answer, protocol.MaxBtwAnswer)
	d.mu.Lock()
	a := d.asides[b.ID]
	if a == nil || !a.Mine || a.Local || a.State != ipc.AsideAsking || !sameAgent(a.To, b.To) {
		d.mu.Unlock()
		return
	}
	switch {
	case b.Answer != "":
		a.State, a.Answer, a.Tokens, a.Note = ipc.AsideAnswered, b.Answer, b.Tokens, ""
	case b.Err != "":
		a.State, a.Err, a.Tokens, a.Note = ipc.AsideFailed, b.Err, b.Tokens, ""
	case b.Note != "" && b.Note != a.Note:
		a.Note = b.Note
	default:
		d.mu.Unlock()
		return
	}
	d.keepAsideLocked(a)
	n := d.noticesLocked(a)
	d.mu.Unlock()
	d.showAll(n)
}

// sameAgent says whether two addresses name the same agent, an answer's as the hub writes it, the question's as this
// person typed it.
func sameAgent(a, b string) bool {
	pa, aa := protocol.Split(a)
	pb, ab := protocol.Split(b)
	return b == "" || (pa == pb && aa == ab)
}

// btwWelcomed runs on each welcome. On the first that says the hub carries questions by the way on a connection,
// this laptop's questions without a final answer go again under their ids (the hub keeps nothing for a laptop that
// was offline), and answers to teammates that didn't reach the hub go now. A hub that no longer carries them fails
// the questions waiting.
func (d *Daemon) btwWelcomed(can bool) {
	d.mu.Lock()
	failed := d.expireLocked()
	if can && d.btwOut != d.out {
		d.btwOut = d.out
		for _, a := range d.asides {
			switch {
			case a.Mine && !a.Local && a.State == ipc.AsideAsking:
				d.askAgainLocked(a)
			case !a.Mine && a.unsent:
				d.tellAskerLocked(a)
			}
		}
	} else if !can { // the team's admins turned them off: what waits won't come, and what turns them on again resends
		d.btwOut = nil
		for _, a := range d.asides {
			if a.Mine && !a.Local && a.State == ipc.AsideAsking {
				a.State, a.Err, a.Note = ipc.AsideFailed, "the hub no longer carries questions by the way", ""
				d.keepAsideLocked(a)
				failed = append(failed, a)
			}
		}
	}
	n := d.noticesLocked(failed...)
	d.mu.Unlock()
	d.showAll(n)
}

// btwTick runs every askAgain: it fails this laptop's questions that waited a day for an answer, and asks the hub
// again those whose answer hasn't come minAge after they last went, as the outbox sends messages again. An agent's
// final answer that hasn't reached it goes to it now: one that came before its btw-wait, which didn't come within
// minAge, or after a restart, or after a delivery that failed. A question that went on the last tick is a moment short
// of minAge on this one, and goes again all the same.
func (d *Daemon) btwTick(minAge time.Duration) {
	minAge -= minAge / 4
	d.mu.Lock()
	settled := d.expireLocked()
	for _, a := range d.asides {
		if !a.Mine {
			continue
		}
		if a.expect && time.Since(time.UnixMilli(a.At)) >= minAge {
			a.expect = false
		}
		switch {
		case a.By != "" && (!a.Handed || a.retry) && a.final():
			settled = append(settled, a)
		case d.hubBtw && !a.Local && a.State == ipc.AsideAsking && time.Since(a.sent) >= minAge:
			d.askAgainLocked(a)
		}
	}
	n := d.noticesLocked(settled...)
	d.mu.Unlock()
	d.showAll(n)
}

// askingLocked says whether a question asked from this laptop waits for its answer, within the day it is asked again:
// the daemon stays for it, as it alone hears the answer and tells whoever asked.
func (d *Daemon) askingLocked() bool {
	for _, a := range d.asides {
		if a.Mine && !a.final() && time.Since(time.UnixMilli(a.At)) <= asideTTL {
			return true
		}
	}
	return false
}

func (d *Daemon) expireLocked() []*aside {
	var failed []*aside
	for _, a := range d.asides {
		if a.Mine && a.State == ipc.AsideAsking && time.Since(time.UnixMilli(a.At)) > asideTTL {
			a.State, a.Err, a.Note = ipc.AsideFailed, "no answer within a day", ""
			d.keepAsideLocked(a)
			failed = append(failed, a)
		}
	}
	return failed
}

// waitBtw waits for the final answer to a question asked here, at most wait, or until whoever waits is gone, and
// replies with it, or with where the question stands. The final answer it replies to an agent's question has reached
// the agent; one that comes once no btw-wait waits, or that reply couldn't hand to whoever waited, gone as it came,
// reaches it as a message, and this person's own as a notification.
func (d *Daemon) waitBtw(id string, wait time.Duration, gone <-chan struct{}, reply func(ipc.Reply) error) {
	timeout := time.After(wait)
	d.mu.Lock()
	if d.waiters == nil {
		d.waiters = map[string]int{}
	}
	d.waiters[id]++ // the answer it shows needs no notification, nor a message
	if a := d.asides[id]; a != nil {
		a.expect = false
	}
	// whoever waited is gone: the answer, if it came, goes to them another way
	elsewhere := func(a *aside) {
		n := d.noticesLocked(a)
		d.mu.Unlock()
		d.showAll(n)
	}
	for timedOut := false; ; {
		a := d.asides[id]
		var r ipc.Reply
		switch {
		case a == nil || !a.Mine:
			r.Err = "no question by the way was asked under that id"
		case a.State == ipc.AsideAnswered:
			r.OK, r.Text = true, a.Answer
		case a.State == ipc.AsideFailed:
			r.Err = a.Err
		}
		if r.Err != "" || r.OK {
			d.unwaitLocked(id)
			select {
			case <-gone:
				elsewhere(a)
				return
			default:
			}
			handed := a != nil && a.By != "" && !a.Handed
			if handed {
				a.Handed = true
				d.keepAsideLocked(a)
			}
			d.mu.Unlock()
			if reply(r) != nil {
				d.mu.Lock()
				if handed {
					a.Handed = false
				}
				elsewhere(a)
			}
			return
		}
		note := a.told().Note
		if note == "" {
			note = "asked; no answer yet"
		}
		if timedOut {
			d.unwaitLocked(id)
			d.mu.Unlock()
			_ = reply(ipc.Reply{OK: true, Note: note})
			return
		}
		if d.asideCh == nil {
			d.asideCh = make(chan struct{})
		}
		changed := d.asideCh
		d.mu.Unlock()
		select {
		case <-changed:
		case <-timeout: // the answer may have come meanwhile: it goes back, and nowhere else
			timedOut = true
		case <-gone:
			d.mu.Lock()
			d.unwaitLocked(id)
			elsewhere(d.asides[id])
			return
		}
		d.mu.Lock()
	}
}

func (d *Daemon) unwaitLocked(id string) {
	if d.waiters[id]--; d.waiters[id] <= 0 {
		delete(d.waiters, id)
	}
}

// notice is what an aside settling brings: a desktop notification for this person, a title and its text; or, for the
// agent of this laptop's that asked, the message that brings it the final answer no btw-wait returned.
type notice struct {
	title, text string
	msg         *protocol.Msg
	agent       string // the agent msg goes to
	aside       string // the aside msg settles, by key
}

// noticesLocked are what this laptop's questions that just got their final answer bring, but for those a btw-wait
// shows already: a notification for this person's own, a message for an agent's.
func (d *Daemon) noticesLocked(as ...*aside) []notice {
	var out []notice
	for _, a := range as {
		switch {
		case a == nil || !a.Mine || d.waiters[a.ID] > 0:
		case a.By != "":
			if n, ok := d.lateLocked(a); ok {
				out = append(out, n)
			}
		case a.State == ipc.AsideAnswered:
			out = append(out, notice{title: "Murmur: " + a.To + " answered by the way", text: a.Answer})
		case a.State == ipc.AsideFailed:
			out = append(out, notice{title: "Murmur: no answer from " + a.To, text: a.Err})
		}
	}
	return out
}

// lateLocked settles an agent's question whose final answer no btw-wait returned: it reaches the agent once, as a
// message from the agent asked, or as murmur's note when there is no answer, which waits while the agent is idle and
// quotes why, as another laptop or the hub may have written it. It isn't a message of the history: the console shows
// it as the question's answer. An agent that has closed gets nothing; one whose btw-wait is on its way gets it from
// that. A message that didn't reach the agent goes again, a.retry, without being written down again.
func (d *Daemon) lateLocked(a *aside) (notice, bool) {
	if (a.Handed && !a.retry) || a.expect || !a.final() {
		return notice{}, false
	}
	if !a.Handed {
		a.Handed = true
		d.keepAsideLocked(a)
	}
	a.retry = false
	s := d.sessions[a.By]
	if s == nil || s.name == "" {
		return notice{}, false
	}
	m := protocol.Msg{ID: lateID + a.ID, From: a.To, To: d.me.Handle + "/" + s.name, At: time.Now().UnixMilli()}
	q := quoteQuestion(a.Question)
	if a.State == ipc.AsideAnswered {
		m.Text = "Answer to your question by the way (" + q + "), from a copy of " + a.To + "'s conversation; " + a.To +
			" itself never saw the question:\n" + a.Answer + "\n\n" + lateFooter(a.To, true)
	} else {
		m.From = protocol.System
		m.Text = "No answer to your question by the way to " + a.To + " (" + q + "), " + harness.Relayed(a.Err) + "\n\n" +
			lateFooter(a.To, false)
	}
	return notice{msg: &m, agent: s.name, aside: a.key()}, true
}

// lateFooter ends the message that brings an agent its late answer, or why none came, where a message's footer would
// have it answer its sender with send (protocol.Prompt): the agent asked never saw the question, and a message would
// interrupt it. A follow-up goes to a copy again.
func lateFooter(to string, answered bool) string {
	if answered {
		return "(From a copy of " + to + "'s conversation, through murmur: " + to + " itself never saw your question. " +
			"For a follow-up, use the murmur `btw` tool again; use `send` only if " + to + " itself must act or reply.)"
	}
	return "(From murmur, on your question to a copy of " + to + "'s conversation: " + to + " itself never saw it. " +
		"To ask again, use the murmur `btw` tool; use `send` only if " + to + " itself must act or reply.)"
}

// lateID starts the id of the message that brings an agent its late answer, before the question's id. The agent asked
// never wrote that message, nor saw the question: whatever becomes of the message, it tells nobody but the agent that
// asked, whose question records where its answer went. The hub takes no message under such an id.
const lateID = protocol.LateBtw

// late says whether m brings an agent of this laptop's the late answer to its question by the way.
func late(m protocol.Msg) bool { return strings.HasPrefix(m.ID, lateID) }

// quoteQuestion is a question as an answer that comes late quotes it: its first line, cut short.
func quoteQuestion(q string) string {
	first, rest, _ := strings.Cut(strings.TrimSpace(q), "\n")
	if cut := cutRunes(first, 80); cut != first || rest != "" {
		return strconv.Quote(strings.TrimSpace(cut) + "…")
	}
	return strconv.Quote(first)
}

// showAll shows the notifications, and hands the messages to their agents. A message that doesn't reach its agent
// goes again on the next btwTick, while the agent runs, lateTries times at most: an inbox that refuses it (a Claude
// Code too old to take messages, say) would refuse it forever.
func (d *Daemon) showAll(notices []notice) {
	for _, n := range notices {
		if n.msg == nil {
			d.show(n.title, n.text)
			continue
		}
		if ok, why := d.handOver(*n.msg, n.agent); !ok {
			d.mu.Lock()
			a := d.asides[n.aside]
			if a != nil && d.sessions[a.By] != nil {
				a.tries++
				a.retry = a.tries < lateTries
				switch {
				case a.tries == 1:
					log.Printf("btw: the answer to %s didn't reach %s: %s; it goes again", n.aside, n.agent, why)
				case !a.retry:
					log.Printf("btw: the answer to %s didn't reach %s %d times: %s; it no longer goes", n.aside, n.agent, a.tries, why)
				}
			}
			d.mu.Unlock()
		}
	}
}

// ---------------------------------------------------------------- answering

// onBtw takes a question by the way from the hub, for one of this laptop's agents. One seen before is answered from
// the history, so a copy never runs twice for it: the asking laptop asks again after each reconnect until it has the
// answer. A hub that lost track of a question asks it again of every laptop of this person (protocol.Btw.Again): one
// that hasn't seen it and doesn't run the agent says nothing.
func (d *Daemon) onBtw(b protocol.Btw) {
	b.Clean()
	b.Question = cutRunes(b.Question, protocol.MaxBtwQuestion)
	b.Answer, b.Err, b.Note, b.Tokens, b.At = "", "", "", 0, time.Now().UnixMilli()
	if b.ID == "" || b.Question == "" {
		return
	}
	again := b.Again
	b.Again = false
	d.mu.Lock()
	if a := d.asides[btwKey(b)]; a != nil {
		d.tellAskerLocked(a)
		d.mu.Unlock()
		return
	}
	if _, agent := protocol.Split(b.To); again && d.byNameLocked(agent) == nil {
		d.mu.Unlock() // the hub lost track of it and asks every laptop of this person: another one has it
		return
	}
	a := &aside{Aside: ipc.Aside{Btw: b, State: ipc.AsideRunning}}
	run, key := d.admitLocked(a), a.key()
	d.keepAsideLocked(a)
	if !run {
		d.tellAskerLocked(a)
	}
	cold, asked := a.State == ipc.AsideCold, *a
	d.mu.Unlock()
	if run {
		go d.runAside(key)
	}
	if cold {
		d.askedNotice(asked)
	}
}

// admitLocked decides what happens to a question from the hub: it runs now, it waits for this person's go (cold), or
// it is refused, with why in a.Err. Questions from this person and their agents, on their other laptops, never wait.
func (d *Daemon) admitLocked(a *aside) bool {
	me := d.me.Handle
	person, agent := protocol.Split(a.To)
	s := d.byNameLocked(agent)
	refuse := func(why string) bool {
		a.State, a.Err = ipc.AsideFailed, why
		return false
	}
	switch {
	case person != me || agent == "":
		return refuse(a.To + " isn't one of " + me + "'s agents")
	case d.idTakenLocked(a.ID):
		return refuse("another question was asked under this id; ask under a new one")
	case s == nil:
		return refuse(a.To + " is no longer running")
	case d.unaskable(s) != "":
		return refuse(d.unaskable(s))
	case protocol.PersonOf(a.From) == me:
		return true
	case d.btwAnswers == ipc.BtwOff:
		return refuse(me + " turned questions by the way off")
	case d.countedLocked() >= btwPerHour:
		return refuse(fmt.Sprintf("%s's agents answer at most %d questions by the way an hour; ask again later", me, btwPerHour))
	}
	a.Counted = true
	if idle := idleFor(s); idle > coldAfter {
		a.State, a.Cold = ipc.AsideCold, s.act.Tokens
		a.Note = fmt.Sprintf("%s has been idle for %s; the question waits for %s's go", agent, howLong(idle), me)
		return false
	}
	return true
}

// idTakenLocked says whether a question from the hub came under id already.
func (d *Daemon) idTakenLocked(id string) bool {
	for _, a := range d.asides {
		if !a.Mine && a.ID == id {
			return true
		}
	}
	return false
}

// countedLocked counts the teammates' questions this laptop took in the last hour.
func (d *Daemon) countedLocked() int {
	n := 0
	for _, a := range d.asides {
		if a.Counted && time.Since(time.UnixMilli(a.At)) < time.Hour {
			n++
		}
	}
	return n
}

// runAside runs the copy for aside key and settles it. A teammate's answer waits for this person unless they chose
// auto; questions from this person and their agents never wait.
func (d *Daemon) runAside(key string) {
	d.mu.Lock()
	a := d.asides[key]
	if a == nil {
		d.mu.Unlock()
		return
	}
	_, agent := protocol.Split(a.To)
	asker, text := a.From, a.Question
	d.mu.Unlock()
	ans, err := d.copyOf(agent, asker, text)
	if err != nil && d.stopping.Err() != nil { // it stopped with the daemon: running in the history, the next one says so
		return
	}
	d.mu.Lock()
	me := d.me.Handle
	a.Tokens = ans.Tokens
	switch {
	case err != nil:
		// what the coding tool said of a copy that failed (its paths, its output, a provider's error) stays here: the
		// fix is this person's to make, on this laptop
		a.State, a.Err, a.Note = ipc.AsideFailed, err.Error(), ""
		var failed copyFailed
		switch {
		case a.Mine:
		case errors.Is(err, harness.ErrSignIn):
			a.Told = "the copy couldn't sign in on " + me + "'s laptop; " + me + " sees why"
		case errors.As(err, &failed):
			a.Told = "the copy failed on " + me + "'s laptop; " + me + " sees why"
		}
	case !a.Mine && protocol.PersonOf(a.From) != me && d.btwAnswers != ipc.BtwAuto:
		a.State, a.Answer, a.Note = ipc.AsideHeld, ans.Text, "answered; waits for "+me+" to share it"
	default:
		a.State, a.Answer, a.Note = ipc.AsideAnswered, ans.Text, ""
	}
	d.keepAsideLocked(a)
	if !a.Mine {
		d.tellAskerLocked(a)
	}
	held, asked := a.State == ipc.AsideHeld, *a
	n := d.noticesLocked(a)
	d.mu.Unlock()
	if held {
		d.askedNotice(asked)
	}
	d.showAll(n)
}

// btwVerdict is this person's word on a teammate's question: the held answer goes to the asker or is dropped; the
// copy for a cold question runs, or the question is declined.
func (d *Daemon) btwVerdict(op, id string) error {
	d.mu.Lock()
	var a *aside
	for _, x := range d.asides {
		if !x.Mine && x.ID == id && (x.State == ipc.AsideHeld || x.State == ipc.AsideCold) && (a == nil || x.At > a.At) {
			a = x
		}
	}
	if a == nil {
		d.mu.Unlock()
		return errors.New("no question by the way waits for you under that id")
	}
	me, run := d.me.Handle, false
	switch {
	case op == "btw-approve" && a.State == ipc.AsideHeld:
		a.State, a.Note = ipc.AsideAnswered, ""
	case op == "btw-approve":
		a.State, a.Note, run = ipc.AsideRunning, "", true
	case a.State == ipc.AsideHeld:
		a.State, a.Answer, a.Note, a.Err = ipc.AsideFailed, "", "", me+" didn't share the answer"
	default:
		a.State, a.Note, a.Err = ipc.AsideFailed, "", me+" declined the question"
	}
	d.keepAsideLocked(a)
	d.tellAskerLocked(a)
	key := a.key()
	d.mu.Unlock()
	if run {
		go d.runAside(key)
	}
	return nil
}

// tellAskerLocked sends the asker where their question stands. An answer that can't go now goes when a hub that
// carries questions by the way welcomes this laptop again: one that doesn't would drop it.
func (d *Daemon) tellAskerLocked(a *aside) {
	b := a.told()
	a.unsent = true
	if d.out == nil || d.btwOut != d.out {
		return
	}
	select {
	case d.out <- protocol.Frame{T: protocol.FrameBtwAnswer, Btw: &b}:
		a.unsent = false
	default:
	}
}

// askedNotice tells this person a teammate's question waits for them: to run the copy, or to share its answer.
func (d *Daemon) askedNotice(a aside) {
	_, agent := protocol.Split(a.To)
	d.show("Murmur: "+whoAsks(a.From, "")+" asks "+agent+" by the way", a.Question)
}

// whoAsks says who asks, from From: "alice", or "alice's agent main-1"; me's own, for a copy of me's agent, are "your
// person" and "your person's agent main-1".
func whoAsks(from, me string) string {
	person, agent := protocol.Split(from)
	if person == me {
		person = "your person"
	}
	if agent != "" {
		return person + "'s agent " + agent
	}
	return person
}

// keepAsideLocked records an aside's change: in memory, in the history, for whoever waits on it and for the consoles.
func (d *Daemon) keepAsideLocked(a *aside) {
	if d.asides == nil {
		d.asides = map[string]*aside{}
	}
	a.Last = time.Now().UnixMilli()
	d.asides[a.key()] = a
	d.appendHistory(histLine{Aside: a})
	if d.asideCh != nil {
		close(d.asideCh)
		d.asideCh = nil
	}
	d.changed()
}

// asidesLocked are the asides of the last day, oldest first, as the console shows them.
func (d *Daemon) asidesLocked() []ipc.Aside {
	var out []ipc.Aside
	for _, a := range d.asides {
		if time.Since(time.UnixMilli(a.Last)) < asideTTL {
			out = append(out, a.Aside)
		}
	}
	slices.SortFunc(out, func(a, b ipc.Aside) int { return cmp.Or(cmp.Compare(a.At, b.At), strings.Compare(a.ID, b.ID)) })
	return out
}

// haltCopies stops the copies running, with whatever they started, and the questions waiting for one, and waits at
// most wait for the copies to be gone. No copy starts after it.
func (d *Daemon) haltCopies(wait time.Duration) {
	d.stopCopies()
	for deadline := time.Now().Add(wait); ; time.Sleep(10 * time.Millisecond) {
		d.mu.Lock()
		running := len(d.forks)
		d.mu.Unlock()
		if running == 0 || time.Now().After(deadline) {
			return
		}
	}
}

// restartedLocked fails the copies that ran when the daemon stopped: they stopped with it.
func (d *Daemon) restartedLocked() {
	for _, a := range d.asides {
		if a.State == ipc.AsideRunning {
			a.State, a.Err = ipc.AsideFailed, "murmur restarted before the copy answered; ask again"
			d.keepAsideLocked(a)
		}
	}
}

// setBtwAnswers saves how this laptop answers teammates' questions, and tells the hub what its agents now take.
func (d *Daemon) setBtwAnswers(v string) error {
	switch v {
	case ipc.BtwApprove, ipc.BtwAuto, ipc.BtwOff:
	default:
		return fmt.Errorf("%q: say approve, auto or off", v)
	}
	if v == ipc.BtwApprove {
		v = ""
	}
	cfg := config.Load()
	cfg.BtwAnswers = v
	if err := config.Save(cfg); err != nil {
		return err
	}
	d.mu.Lock()
	d.btwAnswers = v
	d.mu.Unlock()
	d.changed()
	return nil
}

// btwOfLocked is what a session's agent tells the team about questions by the way (protocol.Agent.Btw): how its
// person answers them; mine when they turned teammates' questions off, and only they and their agents ask it; nothing
// when nobody can ask it through the hub: its coding tool can't copy a conversation, or can't on this laptop, or can't
// copy this one, or the hub doesn't carry questions.
func (d *Daemon) btwOfLocked(s *session) string {
	switch {
	case !d.hubBtw || !copies(d.askerOf(s.harness)) || d.unaskable(s) != "":
		return ""
	case d.btwAnswers == ipc.BtwOff:
		return protocol.BtwMine
	case d.btwAnswers == ipc.BtwAuto:
		return protocol.BtwAuto
	}
	return protocol.BtwApprove
}

// ---------------------------------------------------------------- the copy

// askerOf is harness name, when it can answer questions by the way.
func (d *Daemon) askerOf(name string) harness.Asker {
	if d.askers != nil {
		return d.askers(name)
	}
	a, _ := setup.Harness(name).(harness.Asker)
	return a
}

// copies says whether asker can run copies on this laptop, as far as it can tell before a question comes: the managed
// settings of the coding tool may keep it from turning the copies' tools back. Its own person can still ask, and hears
// why it can't answer.
func copies(asker harness.Asker) bool {
	c, ok := asker.(harness.AskChecker)
	return asker != nil && (!ok || c.CanAsk() == nil)
}

func cantAsk(agent, harness string) string {
	return agent + " runs in " + titleOf(harness) + ", which can't be asked by the way"
}

// unaskable says why one of this laptop's agents can't be asked by the way, by anyone, or "": its coding tool can't
// copy a conversation, or can't copy this session's (one of an older version). Managed settings that keep any copy
// from running here (copies) still let its person ask it, and hear why.
func (d *Daemon) unaskable(s *session) string {
	h := d.askerOf(s.harness)
	if h == nil {
		return cantAsk(s.name, s.harness)
	}
	if c, ok := h.(harness.CopyChecker); ok {
		if err := c.CanCopy(s.listed()); err != nil {
			return s.name + " can't be asked by the way: " + err.Error()
		}
	}
	return ""
}

// sweepCopies removes what the copies of a daemon that stopped left behind, as this one starts: none of its own runs
// yet.
func sweepCopies() {
	for _, h := range setup.Harnesses {
		if c, ok := h.(harness.CopySweeper); ok {
			c.SweepCopies()
		}
	}
}

// byNameLocked finds one of this laptop's sessions by its agent's name.
func (d *Daemon) byNameLocked(name string) *session {
	for _, s := range d.sessions {
		if s.name == name && name != "" {
			return s
		}
	}
	return nil
}

// copyOf runs a throwaway copy of agent's session to answer a question from asker, a person or their agent (as
// whoAsks says it): this person's own come from "your person". It waits its turn: one copy at a time per session, maxCopies on the laptop, and a session with
// maxLine questions waiting refuses more. The copy runs under a session id chosen here, which scan leaves out while it
// runs, and has copyWait to answer.
func (d *Daemon) copyOf(agent, asker, text string) (harness.Answer, error) {
	d.mu.Lock()
	s := d.byNameLocked(agent)
	if s == nil {
		d.mu.Unlock()
		return harness.Answer{}, errors.New(agent + " is no longer running")
	}
	h, key := d.askerOf(s.harness), s.key()
	if why := d.unaskable(s); why != "" {
		d.mu.Unlock()
		return harness.Answer{}, errors.New(why)
	}
	d.mu.Unlock()
	wait, cancel := context.WithTimeout(d.stopping, lineWait)
	done, err := d.turn(wait, key, agent)
	cancel()
	if err != nil {
		return harness.Answer{}, err
	}
	defer done()
	d.mu.Lock()
	switch s = d.sessions[key]; {
	case s == nil:
		d.mu.Unlock()
		return harness.Answer{}, errors.New(agent + " closed before its copy could start")
	case d.stopping.Err() != nil: // no copy starts once the daemon stops
		d.mu.Unlock()
		return harness.Answer{}, errors.New("murmur stopped before the copy of " + agent + " could start")
	}
	q := harness.Question{From: whoAsks(asker, d.me.Handle), Text: text, State: stateOf(s), Fork: newUUID(), Tokens: s.act.Tokens}
	if d.forks == nil {
		d.forks = map[string]bool{}
	}
	d.forks[q.Fork] = true
	listed := s.listed()
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.forks, q.Fork)
		d.mu.Unlock()
	}()
	ctx, stop := context.WithTimeout(d.stopping, copyWait) // a daemon that stops stops its copies
	defer stop()
	ans, err := h.Ask(ctx, listed, q)
	switch {
	case err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) && !errors.Is(err, harness.ErrSignIn):
		return ans, fmt.Errorf("the copy of %s didn't answer within %d minutes", agent, int(copyWait.Minutes()))
	case err != nil:
		return ans, copyFailed{err}
	}
	if ans.Text = cutRunes(strings.TrimSpace(protocol.Clean(ans.Text, true)), protocol.MaxBtwAnswer); ans.Text == "" {
		return ans, errors.New("the copy of " + agent + " gave no answer")
	}
	return ans, nil
}

// copyFailed is a copy's failure as its harness says it, which only this laptop's person hears in full.
type copyFailed struct{ error }

func (e copyFailed) Unwrap() error { return e.error }

// line is the questions at one session's copy: the one running and those waiting behind it.
type line struct {
	turn chan struct{} // the question whose copy runs holds it
	n    int
}

// turn waits for a question's turn at a copy of session key, and returns what ends it.
func (d *Daemon) turn(ctx context.Context, key, agent string) (func(), error) {
	d.mu.Lock()
	if d.lines == nil {
		d.lines = map[string]*line{}
	}
	if d.slots == nil {
		d.slots = make(chan struct{}, maxCopies)
	}
	l := d.lines[key]
	if l == nil {
		l = &line{turn: make(chan struct{}, 1)}
		d.lines[key] = l
	}
	if l.n > maxLine {
		d.mu.Unlock()
		return nil, fmt.Errorf("%s has %d questions by the way waiting already; ask again in a few minutes", agent, maxLine)
	}
	l.n++
	slots := d.slots
	d.mu.Unlock()
	leave := func() {
		d.mu.Lock()
		if l.n--; l.n == 0 && d.lines[key] == l {
			delete(d.lines, key)
		}
		d.mu.Unlock()
	}
	busy := fmt.Errorf("the copies on this laptop were busy for %d minutes; ask again later", int(lineWait.Minutes()))
	select {
	case l.turn <- struct{}{}:
	case <-ctx.Done():
		leave()
		return nil, busy
	}
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		<-l.turn
		leave()
		return nil, busy
	}
	return func() {
		<-slots
		<-l.turn
		leave()
	}, nil
}

// stateOf says what a session is doing, in a few words, for its copy: a copy of a busy session finds its own last turn
// unfinished. Where its turn stands, as its transcript says, tells the copy what is under way; without a transcript,
// its last tool call is all murmur knows.
func stateOf(s *session) string {
	busy, turn, call := s.status == protocol.StatusBusy, s.act.Turn, s.act.TurnCall
	switch {
	case busy && turn == harness.TurnTool && call != "":
		return "in the middle of " + call + ", waiting for its result"
	case busy && turn == harness.TurnReply:
		return "writing its reply to the last message in this conversation"
	case busy && turn == harness.TurnResult && call != "":
		return "working on the result of " + call
	case busy && turn == harness.TurnUnknown && s.act.Doing != "":
		return "in the middle of " + s.act.Doing
	case busy:
		return "working"
	case s.status == protocol.StatusWaiting || s.needs != "":
		return "waiting for its person"
	case s.status == protocol.StatusShell:
		return "in a shell"
	case s.since > 0:
		return "idle for " + howLong(time.Since(time.UnixMilli(s.since)))
	}
	return "idle"
}

// idleFor is how long a session hasn't worked: since its transcript was last written, or since its status changed
// for a session murmur reads no transcript of. A busy session isn't idle.
func idleFor(s *session) time.Duration {
	if s.status == protocol.StatusBusy {
		return 0
	}
	if s.transcript != "" {
		if st, err := os.Stat(s.transcript); err == nil {
			return time.Since(st.ModTime())
		}
	}
	if s.since > 0 {
		return time.Since(time.UnixMilli(s.since))
	}
	return 0
}

// howLong says a duration as people read it at a glance: 45m, 3h, 2d.
func howLong(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", max(int(d.Minutes()), 1))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// newUUID is a random UUID, version 4: Claude Code takes only those as session ids.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// cutRunes keeps the first n characters of s.
func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

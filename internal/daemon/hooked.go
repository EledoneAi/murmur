package daemon

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/harness"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

// Most harnesses keep no registry of live sessions, and murmur can't hand their sessions a message while they run.
// For those, the daemon learns each session from its hooks: the first hook names it, a prompt or a tool call means it
// works, the end of its turn that it waits, and it goes when it ends or its process does. Messages for it wait in a
// queue and leave with the answer to its next hook that can carry them: after a tool call, at the end of a turn
// (which the agent then continues), or with its person's next prompt. An idle agent gets them only when it works
// again.

// hookedTTL is how long a session learned from hooks lasts without one. Codex runs its sessions in a background
// server that outlives them, so a closed one may never say it ended; one that comes back gets its name again.
const hookedTTL = 12 * time.Hour

// hookedSession is a session learned from hooks, as the history keeps it: a restarted daemon knows it again, under
// the same name, while its process runs.
type hookedSession struct {
	Harness string `json:"harness"`
	ID      string `json:"id"`
	PID     int    `json:"pid"`
	Started string `json:"started,omitempty"` // when its process started: a pid given to another process since doesn't match
	Name    string `json:"name"`
	Cwd     string `json:"cwd,omitempty"`
}

// hookedLocked takes in what a hook says of its session.
func (d *Daemon) hookedLocked(ev harness.Event) {
	key := harness.Key(ev.Harness, ev.SessionID)
	s := d.sessions[key]
	if ev.Kind == harness.End {
		if s != nil {
			d.removeLocked(key)
		}
		return
	}
	fresh := s == nil
	if fresh {
		if ev.PID <= 1 {
			return // a hook that couldn't find its harness: a session without a process would never go
		}
		s = &session{harness: ev.Harness, id: ev.SessionID, pid: ev.PID, started: harness.StartTime(ev.PID), cwd: ev.Cwd,
			app: ev.Harness, hooked: true, status: protocol.StatusIdle, since: time.Now().UnixMilli()}
		s.proc = s.started
		s.name = d.nameLocked(ev.Harness, d.names[key])
		if s.name != d.names[key] { // a name another session had: the notes held for that one aren't for this one
			d.unholdLocked(s.name)
		}
		d.sessions[key] = s
		if d.names != nil {
			d.names[key] = s.name
		}
	}
	s.heard = time.Now()
	if ev.Title != "" {
		s.act.Title = ev.Title
	}
	moved := ev.PID > 1 && ev.PID != s.pid
	if moved {
		s.pid, s.started = ev.PID, harness.StartTime(ev.PID) // the same conversation, resumed in another process
		s.proc = s.started
	}
	if ev.Cwd != "" && ev.Cwd != s.cwd {
		s.cwd, moved = ev.Cwd, true
	}
	if fresh || moved {
		d.appendHistory(histLine{Hooked: s.hookedLine()})
	}
	if ev.Version != "" {
		s.version = ev.Version
	}
	if ev.Socket != "" {
		s.sock = ev.Socket
	}
	status := s.status
	switch ev.Kind {
	case harness.Prompt, harness.BeforeTool, harness.AfterTool:
		status = protocol.StatusBusy
	case harness.Stop:
		if !ev.Carries || len(d.queued[key]) == 0 { // otherwise the messages it is about to take keep it working
			status = protocol.StatusIdle
		}
	case harness.Needs:
		if ev.Needs != "" {
			status = protocol.StatusWaiting
		}
	}
	if ev.Call != nil {
		s.act.Called(*ev.Call)
	}
	changed := status != s.status
	if changed {
		d.statusLocked(s, status, time.Now().UnixMilli())
	}
	if ev.Kind == harness.Needs && ev.Needs != "" {
		s.needs = ev.Needs
	}
	if fresh || changed || moved {
		d.gitLocked(s)
	}
	d.changed()
}

// nameLocked names a new session of a harness that doesn't name its own: the name it had before, when no agent took
// it since, or codex-1, codex-2… The number is the first that none of this person's agents uses, on this laptop or
// another.
func (d *Daemon) nameLocked(h, before string) string {
	used := map[string]bool{}
	for _, s := range d.sessions {
		used[s.name] = true
	}
	for _, p := range d.people {
		if p.Handle == d.me.Handle {
			for _, a := range p.Agents {
				used[a.Name] = true
			}
		}
	}
	if before != "" && !used[before] {
		return before
	}
	for n := 1; ; n++ {
		if name := h + "-" + strconv.Itoa(n); !used[name] {
			return name
		}
	}
}

// hookedLine is the session as the history keeps it.
func (s *session) hookedLine() *hookedSession {
	return &hookedSession{Harness: s.harness, ID: s.id, PID: s.pid, Started: s.started, Name: s.name, Cwd: s.cwd}
}

// running says whether a session's process still runs: the same pid, started at the same time.
func (s *session) running() bool {
	return harness.Alive(s.pid) && (s.started == "" || harness.StartTime(s.pid) == s.started)
}

// restoreLocked takes back the sessions the history learned from hooks whose processes still run.
func (d *Daemon) restoreLocked(sessions map[string]hookedSession) {
	for key, h := range sessions {
		s := &session{harness: h.Harness, id: h.ID, pid: h.PID, started: h.Started, proc: h.Started, name: h.Name, cwd: h.Cwd,
			app: h.Harness, hooked: true, status: protocol.StatusIdle, since: time.Now().UnixMilli(), heard: time.Now()}
		if d.sessions[key] != nil || !s.running() {
			continue
		}
		d.sessions[key] = s
		d.gitLocked(s)
	}
}

// queueLocked keeps a message for a session murmur can't push to.
func (d *Daemon) queueLocked(s *session, m protocol.Msg) {
	key := s.key()
	if slices.ContainsFunc(d.queued[key], func(h heldMsg) bool { return h.Msg.ID == m.ID }) {
		return
	}
	h := heldMsg{Agent: key, Msg: m, At: time.Now()}
	d.queued[key] = append(d.queued[key], h)
	d.appendHistory(histLine{Queued: &h})
}

// maxDelivery is about how much of the queue one hook's answer carries: the first message whatever its size, then
// more while they fit. The rest waits for the next hook. Codex moves a long context out of the conversation.
const maxDelivery = 8 << 10

// inflight are messages a hook's answer carries, until the hook says it printed them.
type inflight struct {
	key  string
	msgs []heldMsg
}

// take gives a hook's answer what waits for its session, each message in its own envelope, and a token: the
// messages leave the queue when settle says the hook printed them, and go back to it otherwise.
func (d *Daemon) take(key string) (string, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var parts []string
	n, size := 0, 0
	for _, h := range d.queued[key] {
		text := harness.Envelope(protocol.Prompt(h.Msg))
		if n > 0 && size+len(text) > maxDelivery {
			break
		}
		parts, size, n = append(parts, text), size+len(text), n+1
	}
	if n == 0 {
		return "", ""
	}
	token := protocol.RandomHex(8)
	if d.inflight == nil {
		d.inflight = map[string]inflight{}
	}
	d.inflight[token] = inflight{key, slices.Clone(d.queued[key][:n])}
	if d.queued[key] = d.queued[key][n:]; len(d.queued[key]) == 0 {
		delete(d.queued, key)
	}
	return strings.Join(parts, "\n\n"), token
}

// settle ends a delivery: the messages leave for good when the hook printed them, and wait again otherwise.
func (d *Daemon) settle(token string, printed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f, ok := d.inflight[token]
	if !ok {
		return
	}
	delete(d.inflight, token)
	if !printed { // back in the order they came, among those queued or printed since
		d.queued[f.key] = append(f.msgs, d.queued[f.key]...)
		slices.SortStableFunc(d.queued[f.key], func(a, b heldMsg) int { return a.At.Compare(b.At) })
		if d.sessions[f.key] == nil { // it ended meanwhile
			d.unreadLocked(f.key)
		}
		return
	}
	for _, h := range f.msgs {
		d.appendHistory(histLine{Took: &took{f.key, h.Msg.ID}})
	}
}

// unreadLocked drops what waits for a session that is gone, for good, and tells each sender their message wasn't
// read: once the daemon runs, so a start, which may find several, is over first. Murmur's notes have no sender to
// tell, and neither has a late answer by the way: the agent it names as its sender never saw the question, which this
// note would quote to it.
func (d *Daemon) unreadLocked(key string) {
	for _, h := range d.queued[key] {
		d.appendHistory(histLine{Took: &took{key, h.Msg.ID}})
		if m := h.Msg; m.From != protocol.System && !late(m) {
			first, _, _ := strings.Cut(strings.TrimSpace(m.Text), "\n")
			d.notices = append(d.notices, ipc.Request{To: m.From, Text: fmt.Sprintf("%s closed before it read your message (%q).", m.To, protocol.Cut(first, 80))})
		}
	}
	delete(d.queued, key)
	if d.running && len(d.notices) > 0 {
		go d.sendNotices()
	}
}

// sendNotices sends the notices waiting, as this person.
func (d *Daemon) sendNotices() {
	d.mu.Lock()
	notices := d.notices
	d.notices = nil
	d.mu.Unlock()
	for _, n := range notices {
		d.send(n)
	}
}

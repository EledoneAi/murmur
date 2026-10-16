package daemon

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/internal/harness"
	"github.com/EledoneAi/murmur/internal/protocol"
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
		s = &session{harness: ev.Harness, id: ev.SessionID, pid: ev.PID, cwd: ev.Cwd, app: ev.Harness, hooked: true,
			status: protocol.StatusIdle, since: time.Now().UnixMilli()}
		s.name = d.nameLocked(ev.Harness, d.names[key])
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
		s.pid = ev.PID // the same conversation, resumed in another process
	}
	if ev.Cwd != "" && ev.Cwd != s.cwd {
		s.cwd, moved = ev.Cwd, true
	}
	if fresh || moved {
		d.appendHistory(histLine{Hooked: &hookedSession{Harness: s.harness, ID: s.id, PID: s.pid, Name: s.name, Cwd: s.cwd}})
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
		if len(d.queued[key]) == 0 { // otherwise the messages it is about to take keep it working
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

// restoreLocked takes back the sessions the history learned from hooks whose processes still run.
func (d *Daemon) restoreLocked(sessions map[string]hookedSession) {
	for key, h := range sessions {
		if d.sessions[key] != nil || !harness.Alive(h.PID) {
			continue
		}
		s := &session{harness: h.Harness, id: h.ID, pid: h.PID, name: h.Name, cwd: h.Cwd, app: h.Harness, hooked: true,
			status: protocol.StatusIdle, since: time.Now().UnixMilli(), heard: time.Now()}
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

// take hands over what waits for a session, as the text its hook's answer carries.
func (d *Daemon) take(key string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	q := d.queued[key]
	if len(q) == 0 {
		return ""
	}
	delete(d.queued, key)
	d.appendHistory(histLine{Taken: key})
	var parts []string
	for _, h := range q {
		if time.Since(h.At) < heldTTL {
			parts = append(parts, protocol.Prompt(h.Msg))
		}
	}
	return strings.Join(parts, "\n\n")
}

package daemon

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/harness"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/release"
	"github.com/EledoneAi/murmur/v2/internal/setup"
)

// ---------------------------------------------------------------- local requests

func (d *Daemon) serve(c net.Conn) {
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReaderSize(c, 1<<16)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}
	var req ipc.Request
	if json.Unmarshal(line, &req) != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	enc := json.NewEncoder(c)
	switch req.Op {
	case "hook":
		var text, token string
		if req.Hook != nil {
			text, token = d.onHook(*req.Hook)
		}
		err := enc.Encode(ipc.Reply{OK: true, Text: text, Ack: token != ""})
		if token != "" { // the messages leave once the hook printed them
			_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
			said, _ := r.ReadString('\n')
			d.settle(token, err == nil && strings.TrimSpace(said) == "printed")
		}
	case "who":
		d.mu.Lock()
		snap := d.snapshotLocked()
		d.mu.Unlock()
		_ = enc.Encode(ipc.Reply{OK: true, Text: whoText(snap, req.Harness != ""), State: snap.State})
	case "send":
		_ = enc.Encode(d.send(req))
	case "done":
		d.mu.Lock()
		for _, id := range append(req.IDs, req.ID) {
			if id != "" && !d.done[id] {
				d.done[id] = true
				d.appendHistory(histLine{Done: id})
			}
		}
		d.mu.Unlock()
		d.changed()
		_ = enc.Encode(ipc.Reply{OK: true})
	case "agreements":
		d.mu.Lock()
		text := d.team.Agreements
		d.mu.Unlock()
		_ = enc.Encode(ipc.Reply{OK: true, Text: text})
	case "snapshot":
		d.mu.Lock()
		snap := d.snapshotLocked()
		d.mu.Unlock()
		_ = enc.Encode(snap)
	case "sub":
		d.subscribe(c)
	case "status":
		d.mu.Lock()
		r := ipc.Reply{OK: true, State: d.state, Text: fmt.Sprintf("%d agent sessions", len(d.sessions)), Version: release.Version,
			Update: d.available}
		if v := config.FailedUpdate(); v != "" {
			r.Text += "; the update to " + v + " failed and was undone (see daemon.log)"
		} else if d.available != "" {
			r.Text += "; " + d.available + " is available: murmur update"
		}
		d.mu.Unlock()
		_ = enc.Encode(r)
	case "mute", "unmute":
		var text string
		var err error
		if req.Op == "mute" {
			text, err = d.mute(req.To, req.Text)
		} else {
			text, err = d.unmute(req.To)
		}
		if err != nil {
			_ = enc.Encode(ipc.Reply{Err: err.Error()})
		} else {
			_ = enc.Encode(ipc.Reply{OK: true, Text: text})
		}
	case "approve", "decline":
		if err := d.answer(req.Op, req.ID); err != nil {
			_ = enc.Encode(ipc.Reply{Err: err.Error()})
		} else {
			_ = enc.Encode(ipc.Reply{OK: true})
		}
	case "btw":
		_ = enc.Encode(d.askBtw(req))
	case "btw-wait":
		gone := make(chan struct{}) // the asker stopped waiting
		go func() {
			_, _ = io.Copy(io.Discard, r)
			close(gone)
		}()
		wait := btwWait
		if req.PID != 0 || req.Session != "" || req.Harness != "" { // an agent's tool, which waits less
			wait = agentWait
		}
		d.waitBtw(req.ID, wait, gone, func(r ipc.Reply) error { return enc.Encode(r) })
	case "btw-approve", "btw-decline":
		if err := d.btwVerdict(req.Op, req.ID); err != nil {
			_ = enc.Encode(ipc.Reply{Err: err.Error()})
		} else {
			_ = enc.Encode(ipc.Reply{OK: true})
		}
	case "btw-answers":
		if err := d.setBtwAnswers(req.Text); err != nil {
			_ = enc.Encode(ipc.Reply{Err: err.Error()})
		} else {
			_ = enc.Encode(ipc.Reply{OK: true, Text: req.Text})
		}
	case "terminal": // where one of this laptop's agents runs, for the console to switch to it
		d.mu.Lock()
		var term map[string]string
		for _, s := range d.sessions {
			if s.name == req.To {
				term = d.terms[procKey(s.harness, s.id, s.pid)]
			}
		}
		d.mu.Unlock()
		if term == nil {
			_ = enc.Encode(ipc.Reply{Err: req.To + " started before murmur knew its terminal, or isn't one of this laptop's agents"})
			break
		}
		b, _ := json.Marshal(term)
		_ = enc.Encode(ipc.Reply{OK: true, Text: string(b)})
	case "stop":
		if req.Version != "" && req.Version != release.Version { // meant for a daemon that has gone since
			_ = enc.Encode(ipc.Reply{Err: "this daemon runs " + release.Version})
			break
		}
		_ = enc.Encode(ipc.Reply{OK: true})
		d.shutdown()
	default: // a newer client: it says so rather than finding the connection closed
		_ = enc.Encode(ipc.Reply{Err: fmt.Sprintf("murmur %s doesn't know the request %q", release.Version, req.Op)})
	}
}

// onHook takes what a harness's hook reports, and returns murmur's text for the agent: the brief when a session
// starts, a reason to turn an edit back, the messages waiting for a session whose harness murmur can't push to, or
// nothing. Messages go only with an answer the harness reads, and a token says which, for settle.
func (d *Daemon) onHook(ev harness.Event) (string, string) {
	key := harness.Key(ev.Harness, ev.SessionID)
	l, listed := setup.Harness(ev.Harness).(harness.Lister)
	d.mu.Lock()
	if ev.SessionID != "" && d.forks[ev.SessionID] { // a copy answering a question by the way: no session, and told nothing
		d.mu.Unlock()
		return "", ""
	}
	if ev.Kind == harness.Start {
		if len(d.hints) > 500 {
			clear(d.hints)
		}
		if ev.SessionID != "" && ev.Transcript != "" {
			d.hints[key] = ev.Transcript
		}
		if len(d.sockets) > 500 {
			clear(d.sockets)
		}
		if len(d.terms) > 500 {
			clear(d.terms)
		}
		pk := procKey(ev.Harness, ev.SessionID, ev.PID)
		if ev.SessionID != "" && len(ev.Term) > 0 {
			d.terms[pk] = ev.Term
		}
		if ev.SessionID != "" && ev.Socket != "" {
			d.sockets[pk] = ev.Socket
			for _, s := range d.sessions {
				if procKey(s.harness, s.id, s.pid) == pk {
					s.sock = ev.Socket
				}
			}
		}
		d.lastUse = time.Now()
	}
	if !listed {
		d.hookedLocked(ev)
	} else if s := d.sessions[key]; s != nil && ev.Kind == harness.Needs && ev.Needs != "" && s.needs != ev.Needs {
		s.needs = ev.Needs // its status comes from the harness's registry
		d.changed()
	}
	d.mu.Unlock()
	switch ev.Kind {
	case harness.Start:
		if listed {
			d.scan(ev.Harness, l)
		}
		brief := d.brief(ev)
		if !ev.Carries {
			return brief, ""
		}
		waiting, token := d.take(key)
		return strings.TrimSpace(brief + "\n\n" + waiting), token
	case harness.BeforeTool:
		return d.editCheck(ev), ""
	case harness.AfterTool, harness.Stop, harness.Prompt:
		if ev.Carries {
			return d.take(key)
		}
	}
	return "", ""
}

// snapshotGap is the least time between two snapshots to a console. A busy team changes ten times a second, which a
// console drawing every change pays for twice over, and nobody reads that fast. The first change after a quiet spell
// goes at once.
const snapshotGap = 200 * time.Millisecond

// subscribe sends a console the state whenever it changes. A change that leaves the snapshot as the console has it
// sends nothing: the hub's next roster mostly echoes this laptop's own change, and the console would decode and draw
// it again for nothing.
func (d *Daemon) subscribe(c net.Conn) {
	ch := make(chan struct{}, 1)
	ch <- struct{}{}
	d.mu.Lock()
	d.subs[ch] = true
	d.lastUse = time.Now()
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.subs, ch)
		d.mu.Unlock()
	}()
	closed := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, c)
		close(closed)
	}()
	var last []byte
	var sent time.Time
	for {
		select {
		case <-closed:
			return
		case <-ch:
			if wait := time.Until(sent.Add(snapshotGap)); wait > 0 {
				select {
				case <-closed:
					return
				case <-time.After(wait):
				}
				select { // a change in the meantime is in this snapshot
				case <-ch:
				default:
				}
			}
			d.mu.Lock()
			snap := d.snapshotLocked()
			d.mu.Unlock()
			b, err := json.Marshal(snap)
			if err != nil || bytes.Equal(b, last) {
				continue
			}
			last, sent = b, time.Now()
			_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := c.Write(append(b, '\n')); err != nil {
				return
			}
		}
	}
}

func (d *Daemon) send(req ipc.Request) ipc.Reply {
	// as the hub cleans it: a model wrote it, and the console shows it while it sends and once it failed
	req.To, req.Text = strings.TrimSpace(protocol.Clean(req.To, false)), strings.TrimSpace(protocol.Clean(req.Text, true))
	if req.To == "" || req.Text == "" {
		return ipc.Reply{Err: "both `to` and `text` are needed"}
	}
	// the hub refuses them, and drops the connection that carries one over a megabyte, which the outbox would send again
	// at every reconnection
	if n := utf8.RuneCountInString(req.Text); n > protocol.MaxText {
		return ipc.Reply{Err: fmt.Sprintf("the message is %d characters; the limit is %d", n, protocol.MaxText)}
	}
	if len(req.To) > 1024 {
		return ipc.Reply{Err: "no address on murmur is that long; `who` lists the team"}
	}
	d.mu.Lock()
	from := ""
	if req.PID != 0 || req.Session != "" {
		s := d.senderLocked(req)
		if s == nil || s.name == "" { // a session without a name isn't on the team yet: it never sends as its person
			d.mu.Unlock()
			return ipc.Reply{Err: "this session is not on murmur yet; try again in a few seconds"}
		}
		from = s.name
	}
	out, state, wait := d.out, d.state, cmp.Or(d.wait, sendWait)
	if out == nil && (strings.HasPrefix(state, "not signed in") || strings.HasPrefix(state, "signed out")) {
		d.mu.Unlock()
		return ipc.Reply{Err: "Murmur is " + state}
	}
	// into the outbox first: until the hub confirms it, it goes again with every reconnect
	// the hub stamps its own time; this one is for the console while it sends, or once it failed
	m := protocol.Msg{ID: protocol.RandomHex(8), From: from, To: req.To, Text: req.Text, At: time.Now().UnixMilli()}
	ch := make(chan protocol.Frame, 1)
	d.acks[m.ID] = ch
	d.outbox[m.ID] = outMsg{Msg: m, At: time.Now()}
	d.appendHistory(histLine{Outbox: &outMsg{Msg: m, At: time.Now()}})
	d.mu.Unlock()
	d.changed() // the console shows it sending
	defer func() {
		d.mu.Lock()
		delete(d.acks, m.ID)
		now := d.out
		_, sending := d.outbox[m.ID]
		d.mu.Unlock()
		select {
		case f := <-ch: // the hub's answer came as the wait ended
			d.lateAck(f)
		default:
			// connected again while it waited: that connection's resend left it to this send, and the old one may have lost it
			if now != nil && now != out && sending {
				select {
				case now <- protocol.Frame{T: protocol.FrameSend, Msg: &m}:
				default:
				}
			}
		}
	}()
	later := ipc.Reply{OK: true, Note: "not confirmed yet: murmur keeps sending it and tells you if it fails"}
	if out == nil {
		later.Note = "Murmur is " + state + ": it sends this as soon as it is back, and tells you if it can't"
		return later
	}
	select {
	case out <- protocol.Frame{T: protocol.FrameSend, Msg: &m}:
	case <-time.After(2 * time.Second):
		return later
	}
	select {
	case f := <-ch:
		d.mu.Lock()
		delete(d.outbox, m.ID)
		d.appendHistory(histLine{Sent: m.ID})
		if !f.OK {
			d.failLocked(m, f.Err)
		}
		d.mu.Unlock()
		d.changed() // it is no longer sending
		if f.OK {
			return ipc.Reply{OK: true, Note: f.Note}
		}
		return ipc.Reply{Err: f.Err}
	case <-time.After(wait):
		return later
	}
}

// whoText is what the `who` tool and `murmur who` print: for an agent, which asks through its tools, or for its
// person. What teammates' agents and laptops say about themselves (names, repos, branches, titles) is quoted: agents
// read it as data.
func whoText(s ipc.Snapshot, agent bool) string {
	var b strings.Builder
	for _, p := range s.People {
		if p.Handle == "" {
			continue // this laptop before it signs in
		}
		label := p.Handle
		if p.Name != "" {
			label += fmt.Sprintf(" (%q)", p.Name)
		}
		switch {
		case p.Handle == s.Me.Handle:
			label += ", you"
		case !p.Online:
			label += ", offline"
		}
		b.WriteString(label + "\n")
		for _, a := range p.Agents {
			fmt.Fprintf(&b, "  %q · %s", p.Handle+"/"+a.Name, statusWord(a))
			b.WriteString(appWords(a.App))
			if a.Repo != "" {
				fmt.Fprintf(&b, " · repo %q", a.Repo)
				if a.Branch != "" {
					fmt.Fprintf(&b, " branch %q", a.Branch)
				}
			}
			if a.Title != "" {
				fmt.Fprintf(&b, " · %q", shortTitle(a.Title))
			}
			if a.PR != 0 {
				fmt.Fprintf(&b, " · PR #%d", a.PR)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\nSend to person/agent (like bob/main-1) to reach an agent, or to a handle (bob) to reach the person; ")
	if agent {
		b.WriteString("or ask an agent by the way with the btw tool, without interrupting it.")
	} else {
		b.WriteString("or ask an agent by the way with murmur btw, without interrupting it.")
	}
	if s.State != "online" {
		b.WriteString("\nMurmur is " + s.State + ".")
	}
	return b.String()
}

// appWords say where an agent runs, when that isn't Claude Code in a terminal, and when its harness only takes
// messages while the agent works.
func appWords(app string) string {
	h := setup.Harness(app)
	switch {
	case app == "vscode":
		return " · in VS Code"
	case h == nil:
		return ""
	}
	if _, push := h.(harness.Pusher); !push {
		return " · in " + h.Title() + ", which takes messages while it works"
	}
	return " · in " + h.Title()
}

// senderLocked finds the session an MCP server speaks for: the session its harness names, in the harness process
// that started the server or elsewhere; else the one session that process runs. Codex runs every session in one
// process, so a process with several sessions and no name for the one at hand is no answer. Only a harness whose MCP
// servers run apart from its sessions (Cursor) sends for its agent at work.
func (d *Daemon) senderLocked(req ipc.Request) *session {
	var found []*session
	for _, s := range d.sessions {
		if req.PID != 0 && s.pid == req.PID && (req.Harness == "" || s.harness == req.Harness) {
			if req.Session != "" && s.id == req.Session {
				return s
			}
			found = append(found, s)
		}
	}
	if req.Session != "" {
		if s := d.sessions[harness.Key(req.Harness, req.Session)]; s != nil {
			return s
		}
		if s := d.byIDLocked(req.Session); s != nil {
			return s
		}
		found = nil // the process runs other sessions than the one named
	}
	if len(found) == 1 {
		return found[0]
	}
	found = nil
	if a, ok := setup.Harness(req.Harness).(harness.Apart); ok && a.MCPApart() {
		for _, s := range d.sessions {
			if s.harness == req.Harness && s.status == protocol.StatusBusy {
				found = append(found, s)
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	slices.SortFunc(found, func(a, b *session) int {
		return cmp.Or(cmp.Compare(boolInt(b.status == protocol.StatusBusy), boolInt(a.status == protocol.StatusBusy)), cmp.Compare(b.since, a.since))
	})
	return found[0]
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func statusWord(a protocol.Agent) string {
	switch {
	case a.Needs == "permission":
		return "waiting for its person to approve a tool"
	case a.Needs == "input" || a.Status == protocol.StatusWaiting:
		return "waiting for its person"
	case a.Status == protocol.StatusBusy:
		return "working"
	case a.Status == protocol.StatusShell:
		return "in a shell"
	}
	return "idle"
}

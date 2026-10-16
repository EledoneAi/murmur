package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/internal/harness"
	"github.com/EledoneAi/murmur/internal/ipc"
	"github.com/EledoneAi/murmur/internal/protocol"
	"github.com/EledoneAi/murmur/internal/release"
	"github.com/EledoneAi/murmur/internal/setup"
	"github.com/EledoneAi/murmur/internal/update"
)

// ---------------------------------------------------------------- local requests

func (d *Daemon) serve(c net.Conn) {
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReaderSize(c, 1<<16).ReadBytes('\n')
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
		var text string
		if req.Hook != nil {
			text = d.onHook(*req.Hook)
		}
		_ = enc.Encode(ipc.Reply{OK: true, Text: text})
	case "who":
		d.mu.Lock()
		snap := d.snapshotLocked()
		d.mu.Unlock()
		_ = enc.Encode(ipc.Reply{OK: true, Text: whoText(snap), State: snap.State})
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
		d.subscribe(c, enc)
	case "status":
		d.mu.Lock()
		r := ipc.Reply{OK: true, State: d.state, Text: fmt.Sprintf("%d Claude Code sessions", len(d.sessions)), Version: release.Version,
			Update: d.available}
		if v := update.Failed(); v != "" {
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
	case "terminal": // where one of this laptop's agents runs, for the console to switch to it
		d.mu.Lock()
		var term map[string]string
		for k, s := range d.sessions {
			if s.name == req.To {
				term = d.terms[k]
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
		_ = enc.Encode(ipc.Reply{OK: true})
		d.shutdown()
	}
}

// onHook takes what a harness's hook reports, and returns murmur's text for the agent: the brief when a session
// starts, a reason to turn an edit back, or nothing.
func (d *Daemon) onHook(ev harness.Event) string {
	key := harness.Key(ev.Harness, ev.SessionID)
	switch ev.Kind {
	case harness.Start:
		d.mu.Lock()
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
		if ev.SessionID != "" && len(ev.Term) > 0 {
			d.terms[key] = ev.Term
		}
		if ev.SessionID != "" && ev.Socket != "" {
			d.sockets[key] = ev.Socket
			if s := d.sessions[key]; s != nil {
				s.sock = ev.Socket
			}
		}
		d.lastUse = time.Now()
		d.mu.Unlock()
		if l, ok := setup.Harness(ev.Harness).(harness.Lister); ok {
			d.scan(ev.Harness, l)
		}
		return d.brief(ev)
	case harness.BeforeTool:
		return d.editCheck(ev)
	case harness.Needs:
		d.mu.Lock()
		if s := d.sessions[key]; s != nil && ev.Needs != "" && s.needs != ev.Needs {
			s.needs = ev.Needs
			d.changed()
		}
		d.mu.Unlock()
	}
	return ""
}

func (d *Daemon) subscribe(c net.Conn, enc *json.Encoder) {
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
	for {
		select {
		case <-closed:
			return
		case <-ch:
			d.mu.Lock()
			snap := d.snapshotLocked()
			d.mu.Unlock()
			_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if enc.Encode(snap) != nil {
				return
			}
		}
	}
}

func (d *Daemon) send(req ipc.Request) ipc.Reply {
	if strings.TrimSpace(req.To) == "" || strings.TrimSpace(req.Text) == "" {
		return ipc.Reply{Err: "both `to` and `text` are needed"}
	}
	d.mu.Lock()
	from := ""
	if req.PID != 0 || req.Session != "" {
		var s *session
		if req.PID != 0 {
			s = d.byPIDLocked(req.PID)
		}
		if s == nil && req.Session != "" {
			s = d.byIDLocked(req.Session)
		}
		if s == nil {
			d.mu.Unlock()
			return ipc.Reply{Err: "this session is not on murmur yet; try again in a few seconds"}
		}
		from = s.name
	}
	out, state := d.out, d.state
	if out == nil && (strings.HasPrefix(state, "not signed in") || strings.HasPrefix(state, "signed out")) {
		d.mu.Unlock()
		return ipc.Reply{Err: "Murmur is " + state}
	}
	// into the outbox first: until the hub confirms it, it goes again with every reconnect
	m := protocol.Msg{ID: protocol.RandomHex(8), From: from, To: req.To, Text: req.Text}
	ch := make(chan protocol.Frame, 1)
	d.acks[m.ID] = ch
	d.outbox[m.ID] = outMsg{Msg: m, At: time.Now()}
	d.appendHistory(histLine{Outbox: &outMsg{Msg: m, At: time.Now()}})
	d.mu.Unlock()
	d.changed() // the console shows it sending
	defer func() {
		d.mu.Lock()
		delete(d.acks, m.ID)
		d.mu.Unlock()
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
	case <-time.After(sendWait):
		return later
	}
}

// whoText is what the `who` tool and `murmur who` print. What teammates' agents and laptops say about
// themselves (names, repos, branches, titles) is quoted: agents read it as data.
func whoText(s ipc.Snapshot) string {
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
			if a.App == "vscode" {
				b.WriteString(" · in VS Code")
			}
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
	b.WriteString("\nSend to person/agent (like bob/main-1) to reach an agent, or to a handle (bob) to reach the person.")
	if s.State != "online" {
		b.WriteString("\nMurmur is " + s.State + ".")
	}
	return b.String()
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

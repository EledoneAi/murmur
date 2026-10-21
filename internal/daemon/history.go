package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/harness"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

// ---------------------------------------------------------------- history

type histLine struct {
	M       *protocol.Msg  `json:"m,omitempty"`
	Done    string         `json:"done,omitempty"`
	Handed  string         `json:"handed,omitempty"`
	Outbox  *outMsg        `json:"outbox,omitempty"` // sent from here, not confirmed yet
	Sent    string         `json:"sent,omitempty"`   // confirmed, or given up
	Ask     *protocol.Msg  `json:"ask,omitempty"`    // waiting for this person's approval
	Answer  string         `json:"answer,omitempty"` // and the verdict on it, by answerKey
	Verdict string         `json:"verdict,omitempty"`
	Mute    *mute          `json:"mute,omitempty"`
	Unmute  string         `json:"unmute,omitempty"` // a mute's key
	Held    *heldMsg       `json:"held,omitempty"`   // waiting for its agent to work again
	Unheld  string         `json:"unheld,omitempty"` // and taken, by message id
	Hooked  *hookedSession `json:"hooked,omitempty"` // a session learned from hooks
	Gone    string         `json:"gone,omitempty"`   // and gone, by session key
	Queued  *heldMsg       `json:"queued,omitempty"` // waiting for its session's next hook (Agent is the session key)
	Took    *took          `json:"took,omitempty"`   // and taken, or dropped with its session
	Failed  *failure       `json:"failed,omitempty"` // a message sent from here that failed
	Aside   *aside         `json:"aside,omitempty"`  // a question by the way, as it stands now
}

// took is a message that left a session's queue.
type took struct {
	Key string `json:"key"`
	ID  string `json:"id"`
}

func (d *Daemon) loadHistory() {
	path := filepath.Join(config.Dir(), "history.jsonl")
	if d.queued == nil {
		d.queued = map[string][]heldMsg{}
	}
	if d.asides == nil {
		d.asides = map[string]*aside{}
	}
	hooked := map[string]hookedSession{}
	if f, err := os.Open(path); err == nil {
		r := bufio.NewReader(f)
		for {
			// a line of any length: an older version kept messages longer than the hub takes, and a scanner stops there
			line, err := r.ReadBytes('\n')
			if err != nil && len(line) == 0 {
				break
			}
			var h histLine
			if json.Unmarshal(line, &h) != nil {
				continue
			}
			if h.M != nil && !d.seen[h.M.ID] {
				d.seen[h.M.ID] = true
				m := *h.M // written by any earlier version, before messages were cleaned
				m.From, m.To, m.Text = protocol.Clean(m.From, false), protocol.Clean(m.To, false), protocol.Clean(m.Text, true)
				d.history = append(d.history, m)
			}
			if h.Done != "" {
				d.done[h.Done] = true
			}
			if h.Handed != "" {
				d.handed[h.Handed] = true
			}
			if h.Outbox != nil {
				d.outbox[h.Outbox.Msg.ID] = *h.Outbox
			}
			if h.Sent != "" {
				delete(d.outbox, h.Sent)
			}
			if h.Ask != nil {
				d.asks[h.Ask.ID] = *h.Ask
			}
			if h.Answer != "" {
				id, _, _ := strings.Cut(h.Answer, "/")
				delete(d.asks, id)
				d.answers[h.Answer] = h.Verdict
			}
			if h.Mute != nil {
				m := *h.Mute
				d.mutes = slices.DeleteFunc(d.mutes, func(x mute) bool { return x.key() == m.key() })
				d.mutes = append(d.mutes, m)
			}
			if h.Unmute != "" {
				d.mutes = slices.DeleteFunc(d.mutes, func(x mute) bool { return x.key() == h.Unmute })
			}
			if h.Held != nil && !slices.ContainsFunc(d.held[h.Held.Agent], func(x heldMsg) bool { return x.Msg.ID == h.Held.Msg.ID }) {
				d.held[h.Held.Agent] = append(d.held[h.Held.Agent], *h.Held)
			}
			if h.Unheld != "" {
				for agent, q := range d.held {
					d.held[agent] = slices.DeleteFunc(q, func(x heldMsg) bool { return x.Msg.ID == h.Unheld })
				}
			}
			if h.Hooked != nil {
				key := harness.Key(h.Hooked.Harness, h.Hooked.ID)
				hooked[key] = *h.Hooked
				if d.names != nil {
					d.names[key] = h.Hooked.Name
				}
			}
			if h.Gone != "" {
				delete(hooked, h.Gone)
			}
			if h.Queued != nil && !slices.ContainsFunc(d.queued[h.Queued.Agent], func(x heldMsg) bool { return x.Msg.ID == h.Queued.Msg.ID }) {
				d.queued[h.Queued.Agent] = append(d.queued[h.Queued.Agent], *h.Queued)
			}
			if h.Failed != nil {
				if d.failed == nil {
					d.failed = map[string]failure{}
				}
				d.failed[h.Failed.Msg.ID] = *h.Failed
			}
			if h.Took != nil {
				d.queued[h.Took.Key] = slices.DeleteFunc(d.queued[h.Took.Key], func(x heldMsg) bool { return x.Msg.ID == h.Took.ID })
				if len(d.queued[h.Took.Key]) == 0 {
					delete(d.queued, h.Took.Key)
				}
			}
			if h.Aside != nil {
				d.asides[h.Aside.key()] = h.Aside
			}
		}
		f.Close()
	}
	if len(d.history) > keepMsgs {
		d.history = d.history[len(d.history)-keepMsgs:]
	}
	if d.sessions != nil {
		d.restoreLocked(hooked) // before the history is compacted, which keeps the sessions it knows
	}
	d.pruneLocked()
	if st, err := os.Stat(path); err == nil && st.Size() > historyMax { // compact
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		for i := range d.history {
			_ = enc.Encode(histLine{M: &d.history[i]})
			if d.done[d.history[i].ID] {
				_ = enc.Encode(histLine{Done: d.history[i].ID})
			}
		}
		for id, f := range d.failed {
			_ = enc.Encode(histLine{Failed: &f})
			if d.done[id] && !slices.ContainsFunc(d.history, func(m protocol.Msg) bool { return m.ID == id }) {
				_ = enc.Encode(histLine{Done: id})
			}
		}
		for k := range d.handed {
			_ = enc.Encode(histLine{Handed: k})
		}
		for _, o := range d.outbox {
			_ = enc.Encode(histLine{Outbox: &o})
		}
		for _, m := range d.asks {
			_ = enc.Encode(histLine{Ask: &m})
		}
		for k, v := range d.answers {
			_ = enc.Encode(histLine{Answer: k, Verdict: v})
		}
		for _, m := range d.mutesLocked() {
			_ = enc.Encode(histLine{Mute: &m})
		}
		for _, q := range d.held {
			for _, h := range q {
				_ = enc.Encode(histLine{Held: &h})
			}
		}
		for _, s := range d.sessions {
			if s.hooked {
				_ = enc.Encode(histLine{Hooked: s.hookedLine()})
			}
		}
		for _, q := range d.queued {
			for _, h := range q {
				_ = enc.Encode(histLine{Queued: &h})
			}
		}
		for _, a := range d.asides {
			_ = enc.Encode(histLine{Aside: a})
		}
		_ = os.Remove(path + ".tmp") // one an earlier crash left, maybe read-only
		if os.WriteFile(path+".tmp", b.Bytes(), 0o600) == nil {
			_ = os.Rename(path+".tmp", path)
		}
	}
	d.hist = openHistory(path)
	d.restartedLocked()
	if d.sessions != nil { // what waited for a session that is gone leaves the queue in the history, once
		for key := range d.queued {
			if d.sessions[key] == nil {
				d.unreadLocked(key)
			}
		}
	}
}

// pruneLocked keeps what the daemon remembers about messages to those the history keeps: which it has seen, which
// it handed over, which this person archived, and the verdicts on held ones. It also drops notes held for longer
// than a day, failures of a day ago, and questions by the way that haven't changed for a day.
// ponytail: a copy of a message the history no longer keeps can reach its agent once more. Copies come only from
// their sender's outbox, which gives up after a day: only a laptop getting a thousand messages a day would see it.
func (d *Daemon) pruneLocked() {
	maps.DeleteFunc(d.failed, func(_ string, f failure) bool { return time.Since(f.At) > outboxTTL })
	kept := map[string]bool{}
	for _, m := range d.history {
		kept[m.ID] = true
	}
	for id := range d.failed { // marked done while it still shows
		kept[id] = true
	}
	gone := func(k string) bool { // k is "id" or "id/…"
		id, _, _ := strings.Cut(k, "/")
		return !kept[id]
	}
	maps.DeleteFunc(d.seen, func(k string, _ bool) bool { return gone(k) })
	maps.DeleteFunc(d.done, func(k string, _ bool) bool { return gone(k) })
	maps.DeleteFunc(d.handed, func(k string, _ bool) bool { return gone(k) })
	maps.DeleteFunc(d.answers, func(k, _ string) bool { return gone(k) })
	maps.DeleteFunc(d.asides, func(_ string, a *aside) bool { // one still asking fails first: btwTick
		return a.State != ipc.AsideAsking && time.Since(time.UnixMilli(a.Last)) > asideTTL
	})
	for _, m := range []map[string][]heldMsg{d.held, d.queued} {
		for k, q := range m {
			if q = slices.DeleteFunc(q, func(h heldMsg) bool { return time.Since(h.At) > heldTTL }); len(q) > 0 {
				m[k] = q
			} else {
				delete(m, k)
			}
		}
	}
}

// openHistory opens the history file to append to it. A crash can cut its last line short: what comes next starts a
// line of its own, and only the cut line is lost.
func openHistory(path string) *os.File {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	last := []byte{'\n'}
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		_, _ = f.ReadAt(last, st.Size()-1)
	}
	if last[0] != '\n' {
		_, _ = f.Write([]byte{'\n'})
	}
	return f
}

func (d *Daemon) appendHistory(h histLine) {
	if d.hist != nil {
		b, _ := json.Marshal(h)
		_, _ = d.hist.Write(append(b, '\n'))
	}
}

// record keeps a message in the history, once, and says whether it is new.
func (d *Daemon) record(m protocol.Msg) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen[m.ID] {
		return false
	}
	d.seen[m.ID] = true
	d.history = append(d.history, m)
	if len(d.history) > keepMsgs+200 {
		d.history = slices.Clone(d.history[len(d.history)-keepMsgs:])
	}
	d.appendHistory(histLine{M: &m})
	d.changed()
	return true
}

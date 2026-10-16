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

	"github.com/EledoneAi/murmur/internal/config"
	"github.com/EledoneAi/murmur/internal/protocol"
)

// ---------------------------------------------------------------- history

type histLine struct {
	M       *protocol.Msg `json:"m,omitempty"`
	Done    string        `json:"done,omitempty"`
	Handed  string        `json:"handed,omitempty"`
	Outbox  *outMsg       `json:"outbox,omitempty"` // sent from here, not confirmed yet
	Sent    string        `json:"sent,omitempty"`   // confirmed, or given up
	Ask     *protocol.Msg `json:"ask,omitempty"`    // waiting for this person's approval
	Answer  string        `json:"answer,omitempty"` // and the verdict on it, by answerKey
	Verdict string        `json:"verdict,omitempty"`
	Mute    *mute         `json:"mute,omitempty"`
	Unmute  string        `json:"unmute,omitempty"` // a mute's key
	Held    *heldMsg      `json:"held,omitempty"`   // waiting for its agent to work again
	Unheld  string        `json:"unheld,omitempty"` // and taken, by message id
}

func (d *Daemon) loadHistory() {
	path := filepath.Join(config.Dir(), "history.jsonl")
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var h histLine
			if json.Unmarshal(sc.Bytes(), &h) != nil {
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
		}
		f.Close()
	}
	if len(d.history) > keepMsgs {
		d.history = d.history[len(d.history)-keepMsgs:]
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
		_ = os.WriteFile(path+".tmp", b.Bytes(), 0o600)
		_ = os.Rename(path+".tmp", path)
	}
	d.hist, _ = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// pruneLocked keeps what the daemon remembers about messages to those the history keeps: which it has seen, which
// it handed over, which this person archived, and the verdicts on held ones. It also drops notes held for longer
// than a day, and failures of a day ago.
// ponytail: a copy of a message the history no longer keeps can reach its agent once more. Copies come only from
// their sender's outbox, which gives up after a day: only a laptop getting a thousand messages a day would see it.
func (d *Daemon) pruneLocked() {
	kept := map[string]bool{}
	for _, m := range d.history {
		kept[m.ID] = true
	}
	gone := func(k string) bool { // k is "id" or "id/…"
		id, _, _ := strings.Cut(k, "/")
		return !kept[id]
	}
	maps.DeleteFunc(d.seen, func(k string, _ bool) bool { return gone(k) })
	maps.DeleteFunc(d.done, func(k string, _ bool) bool { return gone(k) })
	maps.DeleteFunc(d.handed, func(k string, _ bool) bool { return gone(k) })
	maps.DeleteFunc(d.answers, func(k, _ string) bool { return gone(k) })
	maps.DeleteFunc(d.failed, func(_ string, f failure) bool { return time.Since(f.At) > outboxTTL })
	for agent, q := range d.held {
		if q = slices.DeleteFunc(q, func(h heldMsg) bool { return time.Since(h.At) > heldTTL }); len(q) > 0 {
			d.held[agent] = q
		} else {
			delete(d.held, agent)
		}
	}
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

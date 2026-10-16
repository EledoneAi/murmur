package daemon

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

// Messages reach this person's agents unless the person muted where they come from. A mute holds messages instead of
// handing them over: from a person, an agent, the agents working in a repo or on a branch, or a group of agents in
// touch with each other. An agent mute also holds what reaches that agent of this person's, so it can work
// undisturbed. Held messages wait in the console, where the person passes them on or drops them. A mute lasts until
// a time, or until its person lifts it.
//
// ponytail: mutes live on this laptop; a person with two laptops mutes on each. Keep them on the hub if that
// bothers anyone.
type mute struct {
	Kind  string    `json:"kind"`  // person, agent, repo or branch
	Value string    `json:"value"` // bob, bob/main-1, acme/api, feat/x or acme/api@feat/x
	Until time.Time `json:"until,omitzero"`
}

func (m mute) key() string { return m.Kind + ":" + m.Value }

func (m mute) String() string {
	s := m.Kind + " " + m.Value
	if !m.Until.IsZero() {
		s += " until " + m.Until.Format("Mon 15:04")
	}
	return s
}

// parseMute reads what a person typed: bob, bob/main-1, repo:acme/api, branch:feat/x, branch:acme/api@feat/x, or
// group:bob/main-1 for that agent and every agent it has talked with lately.
func parseMute(who string) (mute, error) {
	who = strings.TrimSpace(who)
	if kind, value, ok := strings.Cut(who, ":"); ok {
		switch kind {
		case "repo", "branch", "group":
			if value == "" {
				return mute{}, fmt.Errorf("%s: what to mute is missing", kind)
			}
			return mute{Kind: kind, Value: value}, nil
		}
		return mute{}, fmt.Errorf("%q: mute a person (bob), an agent (bob/main-1), repo:…, branch:… or group:…", who)
	}
	person, agent := protocol.Split(who)
	switch {
	case person == "":
		return mute{}, errors.New("say whom to mute")
	case agent != "":
		return mute{Kind: "agent", Value: person + "/" + agent}, nil
	}
	return mute{Kind: "person", Value: person}, nil
}

// until reads how long a mute lasts: "" for until it is lifted, "tomorrow" for the start of the next day, or a
// duration such as 30m or 2h.
func until(s string) (time.Time, error) {
	switch s {
	case "":
		return time.Time{}, nil
	case "tomorrow":
		y, m, d := time.Now().Date()
		return time.Date(y, m, d+1, 8, 0, 0, 0, time.Local), nil
	}
	dur, err := time.ParseDuration(s)
	if err != nil || dur <= 0 {
		return time.Time{}, fmt.Errorf("%q: say for how long, like 30m, 2h or tomorrow", s)
	}
	return time.Now().Add(dur), nil
}

// mute adds a mute, replacing one on the same thing. A group becomes a mute on each of its agents now.
func (d *Daemon) mute(who, howLong string) (string, error) {
	m, err := parseMute(who)
	if err != nil {
		return "", err
	}
	if m.Until, err = until(howLong); err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	add := []mute{m}
	if m.Kind == "group" {
		add = nil
		for _, a := range d.groupLocked(m.Value) {
			add = append(add, mute{Kind: "agent", Value: a, Until: m.Until})
		}
	}
	var said []string
	for _, m := range add {
		d.mutes = slices.DeleteFunc(d.mutes, func(x mute) bool { return x.key() == m.key() })
		d.mutes = append(d.mutes, m)
		d.appendHistory(histLine{Mute: &m})
		said = append(said, m.String())
	}
	d.changed()
	return "muted " + strings.Join(said, ", "), nil
}

// unmute lifts mutes on who, and on each agent of a group.
func (d *Daemon) unmute(who string) (string, error) {
	m, err := parseMute(who)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	keys := []string{m.key()}
	if m.Kind == "group" {
		keys = nil
		for _, a := range d.groupLocked(m.Value) {
			keys = append(keys, "agent:"+a)
		}
	}
	n := len(d.mutes)
	d.mutes = slices.DeleteFunc(d.mutes, func(x mute) bool { return slices.Contains(keys, x.key()) })
	if len(d.mutes) == n {
		return "", errors.New("nothing muted under " + who)
	}
	for _, k := range keys {
		d.appendHistory(histLine{Unmute: k})
	}
	d.changed()
	return "unmuted " + who, nil
}

// mutesLocked lists the mutes still in force.
func (d *Daemon) mutesLocked() []mute {
	now := time.Now()
	d.mutes = slices.DeleteFunc(d.mutes, func(m mute) bool { return !m.Until.IsZero() && now.After(m.Until) })
	return d.mutes
}

// mutedLocked says which mute holds message m, if one does.
func (d *Daemon) mutedLocked(m protocol.Msg) (mute, bool) {
	sender := d.agentLocked(m.From)
	for _, x := range d.mutesLocked() {
		switch x.Kind {
		case "person":
			if protocol.PersonOf(m.From) == x.Value {
				return x, true
			}
		case "agent":
			if m.From == x.Value || m.To == x.Value {
				return x, true
			}
		case "repo":
			if sender.Repo != "" && strings.EqualFold(sender.Repo, x.Value) {
				return x, true
			}
		case "branch":
			repo, branch, scoped := strings.Cut(x.Value, "@")
			if !scoped {
				repo, branch = "", x.Value
			}
			if sender.Branch == branch && (repo == "" || strings.EqualFold(sender.Repo, repo)) {
				return x, true
			}
		}
	}
	return mute{}, false
}

// agentLocked finds an agent on the team by its address.
func (d *Daemon) agentLocked(addr string) protocol.Agent {
	person, name := protocol.Split(addr)
	if name == "" {
		return protocol.Agent{}
	}
	if person == d.me.Handle {
		for _, s := range d.sessions {
			if s.name == name {
				return s.agent(false)
			}
		}
	}
	for _, p := range d.people {
		if p.Handle == person {
			for _, a := range p.Agents {
				if a.Name == name {
					return a
				}
			}
		}
	}
	return protocol.Agent{}
}

// groupLocked is an agent and every agent linked to it by messages in the last day, directly or through others: the
// agents working together on something.
func (d *Daemon) groupLocked(addr string) []string {
	since := time.Now().Add(-24 * time.Hour).UnixMilli()
	linked := map[string][]string{}
	for _, m := range d.history {
		if m.At < since || m.From == protocol.System {
			continue
		}
		if _, a := protocol.Split(m.From); a == "" {
			continue
		}
		if _, b := protocol.Split(m.To); b == "" {
			continue
		}
		linked[m.From] = append(linked[m.From], m.To)
		linked[m.To] = append(linked[m.To], m.From)
	}
	group := []string{addr}
	for i := 0; i < len(group); i++ {
		for _, next := range linked[group[i]] {
			if !slices.Contains(group, next) {
				group = append(group, next)
			}
		}
	}
	return group
}

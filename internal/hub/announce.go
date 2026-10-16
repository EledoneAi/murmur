package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/store"
)

// An announcement is news for every agent working in one repo: main is frozen, staging is down. It reaches the
// agents running now, and stays on the team page for Settings.AnnounceHours. The announcements live in the store,
// so every instance shows them and they survive a restart.

// announcement is one announcement as the store keeps it and the team page shows it.
type announcement struct {
	ID      string    `json:"id"`
	From    string    `json:"from"`
	Repo    string    `json:"repo"`
	Text    string    `json:"text"`
	At      time.Time `json:"at"`
	Reached int       `json:"reached"` // the agents it reached when it was sent
}

const (
	newsKey = "announcements"
	maxNews = 50 // kept at most, newest first: a document of a few hundred kilobytes at worst
)

// announceLocked delivers an announcement to every running agent whose repo is repo, on every instance. Each copy is
// addressed to its agent, so laptops deliver it as any message, and says which repo it was for. The sender hears
// once, how many agents it reached.
func (h *Hub) announceLocked(s sender, m protocol.Msg, repo string, ack func(bool, string, string)) {
	repo = protocol.Clean(protocol.Cut(strings.TrimSpace(repo), 200), false)
	type target struct{ dev, to string }
	var targets []target
	known := false
	for _, x := range h.holdersLocked() {
		for _, a := range x.agents {
			if repo == "" || !strings.EqualFold(a.Repo, repo) {
				continue
			}
			known = true
			if to := x.handle + "/" + a.Name; x.online && to != m.From { // the announcing agent knows already
				targets = append(targets, target{x.dev, to})
			}
		}
	}
	if !known {
		ack(false, "", fmt.Sprintf("nobody works in %q: announce to a repo the team's agents work in; `who` lists them", repo))
		return
	}
	if why := h.limitLocked(m.From, m.To, false); why != "" {
		ack(false, "", why)
		return
	}
	m.At = time.Now().UnixMilli()
	var devs []string
	for _, t := range targets {
		c := m
		c.ID, c.To, c.Announce = announceID(m.From, m.ID, t.to), t.to, repo
		h.toDeviceLocked(t.dev, protocol.Frame{T: protocol.FrameMsg, Msg: &c}, "")
		devs = append(devs, t.dev)
	}
	m.To, m.Announce = protocol.RepoAddress+repo, repo
	h.toPeopleLocked([]string{protocol.PersonOf(m.From)}, protocol.Frame{T: protocol.FrameMsg, Msg: &m, Echo: true}, devs)
	h.log.Info("announcement sent", "id", m.ID, "from", m.From, "repo", repo, "agents", len(targets), "chars", len(m.Text))
	a := announcement{ID: m.ID, From: m.From, Repo: repo, Text: m.Text, At: time.UnixMilli(m.At), Reached: len(targets)}
	h.news = slices.Insert(slices.DeleteFunc(h.news, a.same), 0, a) // the page shows it at once
	h.markDirty()
	go h.saveNews(a)
	ack(true, fmt.Sprintf("reached %s in %s", agents(len(targets)), repo), "")
}

// announceID is the same for one announcement to one agent: a laptop that sends an announcement again, because it
// never heard the answer, doesn't make its agents read it twice.
func announceID(from, id, to string) string {
	s := sha256.Sum256([]byte("announce\x00" + from + "\x00" + id + "\x00" + to))
	return "an-" + hex.EncodeToString(s[:8])
}

func agents(n int) string {
	if n == 1 {
		return "1 agent"
	}
	return fmt.Sprintf("%d agents", n)
}

// same says whether b is the announcement a, sent again.
func (a announcement) same(b announcement) bool { return a.ID == b.ID && a.From == b.From }

// saveNews adds an announcement to those in the store, drops the expired ones, and tells the other instances.
func (h *Hub) saveNews(a announcement) {
	ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	h.event(ctx, "announced", a.From, a.Repo, "reached "+agents(a.Reached)) // never the text, as for any message
	h.newsMu.Lock()
	defer h.newsMu.Unlock()
	// ponytail: two instances saving announcements in the same moment can each drop the other's; the team page
	// would miss one, while its agents got it. A store with append would fix it if teams announce that much.
	list, err := h.readNews(ctx)
	if err == nil {
		list = h.fresh(slices.Insert(slices.DeleteFunc(list, a.same), 0, a))
		err = h.store.Put(ctx, newsKey, mustJSON(list))
	}
	if err != nil {
		h.log.Error("database", "op", "save an announcement", "id", a.ID, "error", err.Error())
		return
	}
	h.mu.Lock()
	h.news = list
	h.mu.Unlock()
	h.changed(ctx, "announcements")
}

// loadNews reads the announcements from the store, when the hub starts and when another instance saved one.
func (h *Hub) loadNews(ctx context.Context) error {
	list, err := h.readNews(ctx)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.news = list
	h.mu.Unlock()
	h.markDirty()
	return nil
}

func (h *Hub) readNews(ctx context.Context) ([]announcement, error) {
	b, err := h.store.Get(ctx, newsKey)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []announcement
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("announcements in the store: %w", err)
	}
	return list, nil
}

// fresh keeps the announcements still up, newest first, at most maxNews of them.
func (h *Hub) fresh(list []announcement) []announcement {
	up := time.Duration(h.Settings().AnnounceHours) * time.Hour
	list = slices.DeleteFunc(list, func(a announcement) bool { return time.Since(a.At) > up })
	slices.SortStableFunc(list, func(a, b announcement) int { return b.At.Compare(a.At) })
	return list[:min(len(list), maxNews)]
}

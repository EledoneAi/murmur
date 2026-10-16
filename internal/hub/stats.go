package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/store"
)

// The weekly report counts team totals only. Every instance counts what passes through it (messages, merge
// notices) and the job lease holder counts what it sees of the whole team (waits, collisions). Each instance adds its
// counts to its own key for the week, so instances never write over each other, and the report sums the keys.

// weekStats are one week's counts. Nothing in them names a person.
type weekStats struct {
	Waits      map[int]int    `json:"waits,omitempty"`      // minutes an agent waited for its person -> waits that long
	Collisions map[string]int `json:"collisions,omitempty"` // "repo file" -> pairs of checkouts changing it, once a day each
	Crossed    int            `json:"crossed,omitempty"`    // messages one person's agent sent another person's agent
	Merges     int            `json:"merges,omitempty"`     // merge notices sent to agents
}

func (w *weekStats) add(o weekStats) {
	w.Waits = sum(w.Waits, o.Waits)
	w.Collisions = sum(w.Collisions, o.Collisions)
	w.Crossed += o.Crossed
	w.Merges += o.Merges
}

func sum[K comparable](a, b map[K]int) map[K]int {
	if a == nil && len(b) > 0 {
		a = map[K]int{}
	}
	for k, n := range b {
		a[k] += n
	}
	return a
}

// teamStats is a hub's side of the counting.
type teamStats struct {
	mu     sync.Mutex
	fresh  map[string]*weekStats // week -> counted here and not in the store yet
	listed string                // the latest week this instance listed itself in

	// Only the job loop touches these, on the lease holder.
	last  time.Time        // when this instance last followed the team
	waits map[string]*wait // agent address -> its wait
	day   string           // the day seen is for, in the team's time zone
	seen  map[string]bool  // pairs of checkouts and files already counted that day
	saved []string         // those in the store, in the order counted
}

// wait is an agent waiting for its person, as the lease holder follows it.
type wait struct {
	start  time.Time
	status time.Time // when the agent's status last changed, at or before start: a later ping is about this wait
	pinged bool
}

const (
	statsKey    = "stats/"         // stats/<week> lists the instances that counted; stats/<week>/<instance> has their counts
	collidedKey = "stats/collided" // the collisions counted today
)

// count adds to this instance's counts for a week. They reach the store with the next flush.
func (h *Hub) count(week string, f func(*weekStats)) {
	h.stats.mu.Lock()
	defer h.stats.mu.Unlock()
	if h.stats.fresh == nil {
		h.stats.fresh = map[string]*weekStats{}
	}
	w := h.stats.fresh[week]
	if w == nil {
		w = &weekStats{}
		h.stats.fresh[week] = w
	}
	f(w)
}

// countCrossedLocked counts a message one person's agent sends another person's agent.
func (h *Hub) countCrossedLocked(m protocol.Msg) {
	from, fromAgent := protocol.Split(m.From)
	to, toAgent := protocol.Split(m.To)
	if fromAgent != "" && toAgent != "" && from != to {
		h.count(weekOf(time.Now(), h.settings.TimeZone), func(w *weekStats) { w.Crossed++ })
	}
}

// flushStats adds what this instance counted since its last flush to its key for each week. Counts it fails to save
// wait for the next flush.
func (h *Hub) flushStats(ctx context.Context) {
	h.stats.mu.Lock()
	fresh, listed := h.stats.fresh, h.stats.listed
	h.stats.fresh = nil
	h.stats.mu.Unlock()
	for week, add := range fresh {
		if err := h.saveStats(ctx, week, *add); err != nil {
			h.log.Error("database", "op", "save the week's counts", "week", week, "error", err.Error())
			h.count(week, func(w *weekStats) { w.add(*add) })
		}
	}
	// Two instances listing themselves in a week at the same moment can drop one of them, so each checks again.
	if _, saved := fresh[listed]; listed != "" && !saved {
		if err := h.listInstance(ctx, listed); err != nil {
			h.log.Error("database", "op", "list this instance's counts", "week", listed, "error", err.Error())
		}
	}
}

// saveStats adds counts to this instance's key for a week. The instance is listed first, so a failure part way
// never leaves counts the report can't find, nor counts that the next flush would add twice.
func (h *Hub) saveStats(ctx context.Context, week string, add weekStats) error {
	if err := h.listInstance(ctx, week); err != nil {
		return err
	}
	key := statsKey + week + "/" + h.id
	var w weekStats
	if err := getJSON(ctx, h.store, key, &w); err != nil {
		return err
	}
	w.add(add)
	return h.store.Put(ctx, key, mustJSON(w))
}

// listInstance adds this instance to the instances that counted in a week.
func (h *Hub) listInstance(ctx context.Context, week string) error {
	var ids []string
	if err := getJSON(ctx, h.store, statsKey+week, &ids); err != nil {
		return err
	}
	if !slices.Contains(ids, h.id) {
		if err := h.store.Put(ctx, statsKey+week, mustJSON(append(ids, h.id))); err != nil {
			return err
		}
	}
	h.stats.mu.Lock()
	h.stats.listed = max(h.stats.listed, week) // week names sort in time order
	h.stats.mu.Unlock()
	return nil
}

// weekTotals sums every instance's counts for a week.
func (h *Hub) weekTotals(ctx context.Context, week string) (weekStats, error) {
	var ids []string
	var total weekStats
	if err := getJSON(ctx, h.store, statsKey+week, &ids); err != nil {
		return total, err
	}
	for _, id := range ids {
		var w weekStats
		if err := getJSON(ctx, h.store, statsKey+week+"/"+id, &w); err != nil {
			return total, err
		}
		total.add(w)
	}
	return total, nil
}

// getJSON reads a document from the store into v, and leaves v alone when there is none.
func getJSON(ctx context.Context, st store.Store, key string, v any) error {
	b, err := st.Get(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s in the store: %w", key, err)
	}
	return nil
}

// followTeam is the lease holder's look at the whole team, every jobEvery. An instance that missed a look may have
// lost the lease meanwhile: it starts over, rather than count again what the other holder counted.
func (h *Hub) followTeam(ctx context.Context, now time.Time) {
	t := &h.stats
	if t.waits == nil || now.Sub(t.last) > 2*jobEvery {
		t.waits, t.seen, t.saved = map[string]*wait{}, nil, nil
	}
	t.last = now
	h.followWaits(ctx, now)
	h.countCollisions(ctx, now)
}

// checkout is an agent and its person, as the lease holder sees them.
// followWaits counts each wait for a person when it ends, and pings the team's chat about waits that go on.
func (h *Hub) followWaits(ctx context.Context, now time.Time) {
	h.mu.Lock()
	s := h.settings
	agents := map[string]checkout{}
	for _, x := range h.holdersLocked() { // a laptop reconnecting still has its agents waiting
		for _, a := range x.agents {
			agents[x.handle+"/"+a.Name] = checkout{x.handle, a}
		}
	}
	h.mu.Unlock()
	waits := h.stats.waits
	for addr, w := range waits {
		x, ok := agents[addr]
		if ok && waitingFor(x.agent) {
			continue
		}
		end := now
		if ok && x.agent.Since > 0 { // the laptop says when its status changed, which is closer than this look
			if at := time.UnixMilli(x.agent.Since); at.After(w.start) && at.Before(now) {
				end = at
			}
		}
		waited := int(end.Sub(w.start) / time.Minute)
		h.count(weekOf(end, s.TimeZone), func(ws *weekStats) { ws.Waits = sum(ws.Waits, map[int]int{waited: 1}) })
		delete(waits, addr)
	}
	for addr, x := range agents {
		if waitingFor(x.agent) && waits[addr] == nil {
			w := &wait{start: now, status: now}
			if at := time.UnixMilli(x.agent.Since); x.agent.Since > 0 && at.Before(now) {
				w.status = at
				if x.agent.Status == protocol.StatusWaiting { // else it waits for a tool's approval while busy: from now
					w.start = at
				}
			}
			waits[addr] = w
		}
	}
	h.pingWaits(ctx, now, s, agents)
}

// waitingFor says whether an agent waits for its person: to answer it, or to approve a tool.
func waitingFor(a protocol.Agent) bool {
	return a.Status == protocol.StatusWaiting || a.Needs != ""
}

// countCollisions counts each file two checkouts of one repo change at the same time, as the consoles' overlaps find them,
// once a day for each pair of checkouts. The day's list is in the store, so a new lease holder doesn't count the same
// ones again.
func (h *Hub) countCollisions(ctx context.Context, now time.Time) {
	s, x := h.Settings(), h.reindex() // every 30 s, from who is online now
	t := &h.stats
	day := now.In(zone(s.TimeZone)).Format(time.DateOnly)
	if t.seen == nil || t.day != day {
		var c collided
		if err := getJSON(ctx, h.store, collidedKey, &c); err != nil {
			h.log.Error("database", "op", "read today's collisions", "error", err.Error())
			return
		}
		t.day, t.seen, t.saved = day, map[string]bool{}, nil
		if c.Day == day {
			for _, k := range c.Seen {
				t.seen[k] = true
			}
			t.saved = c.Seen
		}
	}
	files, fresh := map[string]int{}, []string(nil) // fresh: the pairs counted on this look
	for k, cs := range x.files {
		repo, f, _ := strings.Cut(k, "\x00")
		for i, a := range cs {
			for _, b := range cs[i+1:] {
				if a.key() == b.key() {
					continue // one checkout
				}
				hash := sha256.Sum256([]byte(pairKey(a.key(), b.key()) + "\x00" + repo + "\x00" + f))
				if k := hex.EncodeToString(hash[:8]); !t.seen[k] {
					t.seen[k] = true
					files[repo+" "+f]++
					fresh = append(fresh, k)
				}
			}
		}
	}
	if len(files) == 0 {
		return
	}
	h.count(weekOf(now, s.TimeZone), func(w *weekStats) { w.Collisions = sum(w.Collisions, files) })
	// ponytail: a store that limits its values (Firestore: a document of 1 MiB, some 50,000 pairs) keeps the day's
	// first pairs only, and a lease holder that starts later that day counts those beyond again. A store that adds to
	// a set, instead of a list written whole, would keep them all.
	room := math.MaxInt
	if v, ok := h.store.(store.ValueLimit); ok {
		room = (v.MaxValue() - 64) / pairSize
	}
	n := min(len(fresh), max(room-len(t.saved), 0))
	if n == 0 {
		return
	}
	t.saved = append(t.saved, fresh[:n]...)
	if err := h.store.Put(ctx, collidedKey, mustJSON(collided{Day: day, Seen: t.saved})); err != nil {
		h.log.Error("database", "op", "save today's collisions", "error", err.Error())
	}
}

// pairSize is what one pair takes in the list in the store: 16 hex digits, quoted, and a comma.
const pairSize = 19

// collided is the collisions counted on one day: a hash of each pair of checkouts and file.
type collided struct {
	Day  string   `json:"day"`
	Seen []string `json:"seen"`
}

// weekOf names the ISO week t falls in, in the team's time zone: 2026-W42.
func weekOf(t time.Time, tz string) string {
	y, w := t.In(zone(tz)).ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

var zones sync.Map // time zone name -> *time.Location, since LoadLocation reads a file each time

// zone is the team's time zone. Settings are checked when they are saved, so UTC only stands in for a zone this
// machine doesn't know.
func zone(tz string) *time.Location {
	if loc, ok := zones.Load(tz); ok {
		return loc.(*time.Location)
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	zones.Store(tz, loc)
	return loc
}

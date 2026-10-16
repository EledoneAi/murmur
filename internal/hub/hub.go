// Package hub is the team's shared service: who is online, what their agents do, and the messages between them.
// It runs as one or more instances. Each instance holds the WebSockets of the laptops connected to it, and learns
// about the others' laptops from the bus. What must survive a restart is in the store.
package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/auth"
	"github.com/EledoneAi/murmur/v2/internal/bus"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/store"
)

type Config struct {
	PublicURL string
	Version   string        // laptops on another version update to this one
	Secret    []byte        // signs browser cookies and sign-in state
	Auth      auth.Provider // nil when only DevLogin signs people in
	DevLogin  bool          // sign in by typing an email; only on a hub at http://localhost
	Admins    []string      // emails made admins when they sign in: the first admins, before the admin panel
	Seed      Settings      // the settings a hub starts with, until an admin changes them
	Dist      fs.FS         // client binaries served to installers and self-updates; nil serves none
	Instance  string        // this instance's name on the bus; random when empty
	// TrustProxy reads a client's address from the last X-Forwarded-For hop, which the proxy in front appends: only
	// with a proxy, since anyone can write that header.
	TrustProxy bool
	GitHubURL  string // GitHub Enterprise Server, whose API the merge notices may call besides api.github.com
	Log        *slog.Logger
}

const (
	grace        = 15 * time.Second // a laptop that reconnects within this keeps its place
	ackTimeout   = time.Minute
	holdFor      = 2 * time.Minute    // a message for an agent whose laptop is reconnecting
	keepFor      = 7 * 24 * time.Hour // a message for a person who is offline
	outboxDepth  = 256
	aliveEvery   = 10 * time.Second // each instance says which laptops it still holds
	aliveTimeout = 35 * time.Second // after which another instance's laptops count as gone
	codeTTL      = 10 * time.Minute
	helloWait    = 10 * time.Second
	frameWindow  = 10 * time.Second
	maxFrames    = 300 // in frameWindow, from one laptop
)

type Hub struct {
	cfg   Config
	store store.Store
	bus   bus.Bus
	id    string
	log   *slog.Logger

	mu       sync.Mutex
	settings Settings
	users    map[string]*store.User         // by email, as the store had them at the last reload
	handles  map[string]*store.User         // by handle
	devices  map[string]*store.Device       // by id
	tokens   map[string]*store.Device       // by token hash
	links    map[string]*link               // laptops connected to this instance, or gone less than `grace` ago
	remote   map[string]*presence           // laptops on other instances, by device id
	asked    map[string]time.Time           // instance -> when this one last asked it for its laptops
	waiting  map[string]*waiter             // message id -> the sender to tell when it is delivered
	pages    map[string]chan protocol.Frame // message id -> the team page here waiting for its answer
	streams  map[chan struct{}]bool         // the team pages open here, told when the roster changes
	teamPage atomic.Pointer[teamEvent]      // what the last roster told the team pages, while some are open
	newLinks []*link                        // connected since the last roster: rosterLoop greets them
	joined   chan struct{}                  // a laptop connected
	colours  map[string]int                 // by email, until people change: coloursLocked
	news     []announcement                 // announcements to repos, newest first, as the store had them
	newsMu   sync.Mutex                     // one change to the stored announcements at a time
	pairs    map[string]*pair
	sends    map[string][]time.Time // sender address -> recent sends
	reloaded time.Time
	dirty    chan struct{}
	gets     downloads
	sums     sync.Map // client binary name -> its SHA-256
	stats    teamStats
	stop     context.CancelFunc
	index    atomic.Pointer[overlapIndex] // the latest, built with each roster
	done     <-chan struct{}              // closed when the hub stops
}

// link is one laptop connected to this instance.
type link struct {
	dev     string
	email   string
	handle  string
	version string
	agents  []protocol.Agent
	out     chan *protocol.Frame
	roster  func(...[]byte) // hands the laptop the newest roster, encoded, in parts, ahead of any it hasn't read yet
	cancel  context.CancelFunc
	gone    time.Time // zero while connected
	shown   uint64    // the roster last handed to it, by hash; only rosterLoop reads and writes it
}

type waiter struct {
	from    string // the sending laptop, or page:<instance> for a team page waiting there
	stored  bool   // a message kept in the store: drop it there once delivered
	created time.Time
}

// pair is two agents writing to each other: their recent messages, for the loop limit.
type pair struct {
	msgs []time.Time
}

func New(ctx context.Context, cfg Config, st store.Store, b bus.Bus) (*Hub, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if err := CheckDist(cfg.Dist); err != nil {
		return nil, err
	}
	if cfg.Instance == "" {
		cfg.Instance = protocol.RandomHex(4)
	}
	ctx, stop := context.WithCancel(ctx)
	h := &Hub{
		cfg: cfg, store: st, bus: b, id: cfg.Instance, log: cfg.Log, stop: stop,
		links: map[string]*link{}, remote: map[string]*presence{}, asked: map[string]time.Time{}, waiting: map[string]*waiter{}, pairs: map[string]*pair{},
		pages: map[string]chan protocol.Frame{}, streams: map[chan struct{}]bool{}, done: ctx.Done(),
		sends: map[string][]time.Time{}, dirty: make(chan struct{}, 1), joined: make(chan struct{}, 1),
		gets: downloads{seen: map[string][]time.Time{}},
	}
	if err := h.loadSettings(ctx); err != nil {
		stop()
		return nil, err
	}
	if err := h.loadNews(ctx); err != nil {
		stop()
		return nil, err
	}
	if err := h.reload(ctx); err != nil {
		stop()
		return nil, err
	}
	go h.listen(ctx)
	go h.rosterLoop(ctx)
	go h.sweepLoop(ctx)
	go h.jobLoop(ctx)
	h.publish(ctx, evSync, nil, "") // the other instances say who they hold
	return h, nil
}

// Close drops every laptop so each reconnects at once, to another instance if this one is going away, saves the
// counts this instance hasn't saved yet, and stops its loops.
func (h *Hub) Close() {
	h.mu.Lock()
	for _, l := range h.links {
		l.cancel()
	}
	h.mu.Unlock()
	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	h.flushStats(ctx)
	h.stop()
}

// reload reads people and laptops from the store, and drops the laptops of anyone removed or revoked since.
func (h *Hub) reload(ctx context.Context) error {
	us, err := h.store.Users(ctx)
	if err != nil {
		return err
	}
	ds, err := h.store.Devices(ctx)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.users, h.handles = map[string]*store.User{}, map[string]*store.User{}
	for i := range us {
		u := &us[i]
		h.users[u.Email], h.handles[u.Handle] = u, u
	}
	h.colours = nil
	h.devices, h.tokens = map[string]*store.Device{}, map[string]*store.Device{}
	for i := range ds {
		d := &ds[i]
		h.devices[d.ID], h.tokens[d.TokenHash] = d, d
	}
	for _, l := range h.links {
		if !h.allowedLocked(l.dev) {
			l.cancel()
		}
	}
	h.reloaded = time.Now()
	h.markDirty()
	return nil
}

// reloadSoon reloads after a cache miss, at most every two seconds, so unknown tokens can't keep the store busy.
func (h *Hub) reloadSoon(ctx context.Context) {
	h.mu.Lock()
	recent := time.Since(h.reloaded) < 2*time.Second
	h.mu.Unlock()
	if !recent {
		if err := h.reload(ctx); err != nil {
			h.log.Error("database", "op", "reload people and laptops", "error", err.Error())
		}
	}
}

// allowedLocked says whether a laptop may stay connected: not revoked, its sign-in not expired, its person not
// removed.
func (h *Hub) allowedLocked(dev string) bool {
	d := h.devices[dev]
	if d == nil || d.Revoked || h.settings.expired(*d) {
		return false
	}
	u := h.users[d.Email]
	return u != nil && !u.Disabled
}

// expired says whether a laptop signed in longer ago than the team allows, so it must sign in again.
func (s Settings) expired(d store.Device) bool {
	return s.TokenDays > 0 && time.Since(d.Created) > time.Duration(s.TokenDays)*24*time.Hour
}

// changed tells every instance, this one included, to read something from the store again.
func (h *Hub) changed(ctx context.Context, what string) {
	h.publish(ctx, evChanged, what, "")
}

// event records something an admin may want to see later, in the store and in the log.
func (h *Hub) event(ctx context.Context, kind, actor, subject, detail string) {
	h.log.Info(kind, "actor", actor, "subject", subject, "detail", detail)
	if err := h.store.AddEvent(ctx, store.Event{At: time.Now(), Kind: kind, Actor: actor, Subject: subject, Detail: detail}); err != nil {
		h.log.Error("database", "op", "record an event", "error", err.Error())
	}
}

func hashToken(t string) string {
	s := sha256.Sum256([]byte(t))
	return hex.EncodeToString(s[:])
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

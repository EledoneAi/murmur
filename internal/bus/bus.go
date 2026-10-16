// Package bus carries events between the instances of one hub: who is online with which agents, frames for a
// laptop that another instance holds, and notes that something changed in the store. One instance needs no more
// than Local. Delivery is at most once: the hub repeats presence, and laptops resend what wasn't confirmed.
//
// Every backend passes the same contract tests (bustest.Run). Open picks one from a URL.
package bus

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
)

// Reconnected is the kind of the event a networked bus hands its own instance after it reconnects: events may have
// been missed meanwhile, so the instance reads the store again and asks the others for their laptops.
const Reconnected = "bus.reconnected"

type Event struct {
	Kind string          `json:"k"`
	From string          `json:"f"`           // the instance that published it
	To   string          `json:"t,omitempty"` // one instance, or every instance when empty
	Data json.RawMessage `json:"d,omitempty"`
	Sig  string          `json:"s,omitempty"` // the publishing hub's MAC, so nothing else on the bus can speak for it
}

type Bus interface {
	// Publish sends e to every instance, this one included.
	Publish(ctx context.Context, e Event) error
	// Events is every event published on the bus, this instance's own included. There is one reader. It stays
	// open while a backend reconnects; events published meanwhile are lost.
	Events() <-chan Event
	// Close stops delivery and closes Events. Publish fails after it.
	Close() error
}

// Open connects to the bus a URL names:
//
//	local://                              one instance: events stay in this process
//	postgres://user:pass@host:5432/db     LISTEN/NOTIFY; postgresql:// too
//	redis://:pass@host:6379/0             Pub/Sub on Redis, Valkey, Memorystore, ElastiCache or Upstash
//	rediss://user:pass@host:6380          the same over TLS
//
// The postgres and redis forms take a channel query parameter, murmur by default: hubs that share a server
// need one each. LISTEN needs a session of its own, so postgres:// must reach the server or a session pooler, not a
// transaction pooler.
func Open(ctx context.Context, url string) (Bus, error) {
	scheme, _, _ := strings.Cut(url, "://")
	switch scheme {
	case "", "local":
		return NewLocal(), nil
	case "postgres", "postgresql":
		return openPostgres(ctx, url)
	case "redis", "rediss":
		return openRedis(ctx, url)
	}
	// The URL stays out of the error: it may hold a password.
	return nil, errors.New("unknown bus: use local://, postgres://, postgresql://, redis:// or rediss://")
}

// cutChannel takes the channel query parameter out of a URL, as the Postgres and Redis clients would reject it.
func cutChannel(raw string) (rest, channel string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		// A url.Error quotes the whole URL, password included.
		return "", "", fmt.Errorf("bad bus URL: %w", errors.Unwrap(err))
	}
	q := u.Query()
	channel = cmp.Or(q.Get("channel"), "murmur")
	q.Del("channel")
	u.RawQuery = q.Encode()
	return u.String(), channel, nil
}

var errClosed = errors.New("bus closed")

// Local is the bus of a hub with one instance.
type Local struct {
	ch   chan Event
	done chan struct{}
	mu   sync.RWMutex // publishers send under it, so Close never closes ch under them
	once sync.Once
}

func NewLocal() *Local { return &Local{ch: make(chan Event, 4096), done: make(chan struct{})} }

func (b *Local) Publish(ctx context.Context, e Event) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	select {
	case <-b.done:
		return errClosed
	default:
	}
	select {
	case b.ch <- e:
		return nil
	case <-b.done:
		return errClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Local) Events() <-chan Event { return b.ch }

func (b *Local) Close() error {
	b.once.Do(func() {
		close(b.done) // releases publishers waiting on a full ch, so the lock comes free
		b.mu.Lock()
		close(b.ch)
		b.mu.Unlock()
	})
	return nil
}

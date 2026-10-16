// Package store keeps what a hub must not lose when it restarts: people, laptops, messages waiting for someone
// offline, sign-in codes, settings and the event log. Who is online and what their agents do is not here: laptops
// send it again whenever they reconnect.
//
// Every backend passes the same contract tests (storetest.Run). Open picks one from a URL.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/EledoneAi/murmur/internal/protocol"
)

var ErrNotFound = errors.New("not found")

type User struct {
	Email    string    `json:"email"`
	Handle   string    `json:"handle"`
	Name     string    `json:"name,omitempty"`
	Admin    bool      `json:"admin,omitempty"`
	Disabled bool      `json:"disabled,omitempty"` // removed by an admin: no laptop, page or sign-in works for them
	Created  time.Time `json:"created"`
}

// Device is one laptop. Its token is never stored, only the token's SHA-256.
type Device struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Host      string    `json:"host,omitempty"`
	TokenHash string    `json:"token_hash"`
	Created   time.Time `json:"created"`
	LastSeen  time.Time `json:"last_seen"`
	Revoked   bool      `json:"revoked,omitempty"`
	Version   string    `json:"version,omitempty"`  // the laptop's murmur, at its last hello
	Report    []byte    `json:"report,omitempty"`   // its last self-check, JSON as the laptop sent it
	ReportAt  time.Time `json:"report_at,omitzero"` //
}

// Pending is a message the hub keeps until its recipient's laptop is back.
type Pending struct {
	Msg     protocol.Msg `json:"msg"`
	Email   string       `json:"email"`            // the person it waits for
	Device  string       `json:"device,omitempty"` // only that laptop can take it (an agent's), or any of theirs
	Sender  string       `json:"sender,omitempty"` // the laptop that sent it, told if it expires
	Created time.Time    `json:"created"`
	Expires time.Time    `json:"expires"`
}

// Code is a one-time laptop sign-in code, traded once for a token with the PKCE verifier.
type Code struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Host      string    `json:"host,omitempty"`
	Challenge string    `json:"challenge"`
	Expires   time.Time `json:"expires"`
}

// Event is one line of the admin's event log: sign-ins, removals, setting changes. Never message text.
type Event struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Actor   string    `json:"actor,omitempty"`
	Subject string    `json:"subject,omitempty"`
	Detail  string    `json:"detail,omitempty"`
}

// The event log keeps the newest keepEvents events and none older than keepEventsFor. Backends prune it once every
// pruneEvery events they add, not on every insert.
const (
	keepEvents    = 10000
	keepEventsFor = 90 * 24 * time.Hour
	pruneEvery    = 100
)

type Store interface {
	Users(ctx context.Context) ([]User, error)
	PutUser(ctx context.Context, u User) error

	Devices(ctx context.Context) ([]Device, error)
	PutDevice(ctx context.Context, d Device) error
	// SeenDevice and ReportDevice change only those fields of a laptop, so they never undo a revoke or another
	// instance's write. Both return ErrNotFound for an unknown laptop.
	SeenDevice(ctx context.Context, id, version string, at time.Time) error
	ReportDevice(ctx context.Context, id string, report []byte, at time.Time) error

	// AddPending keeps a message under its Msg.ID; adding the same id again replaces it.
	AddPending(ctx context.Context, p Pending) error
	// Pending lists what waits for a person and hasn't expired, oldest first.
	Pending(ctx context.Context, email string) ([]Pending, error)
	DropPending(ctx context.Context, id string) error
	// TakeExpired removes and returns what expired before now, so exactly one instance tells each sender. When it
	// fails part way, it returns what it already took along with the error.
	TakeExpired(ctx context.Context, now time.Time) ([]Pending, error)

	PutCode(ctx context.Context, c Code) error
	// TakeCode removes the code and returns it: ErrNotFound when it is unknown, already taken or expired.
	TakeCode(ctx context.Context, id string) (Code, error)

	// Get and Put keep small documents by key: settings, a person's preferences, the release check.
	Get(ctx context.Context, key string) ([]byte, error)
	Put(ctx context.Context, key string, value []byte) error

	AddEvent(ctx context.Context, e Event) error
	// Events lists events newest first, by At.
	Events(ctx context.Context, limit int) ([]Event, error)

	// Lease takes the lease name for holder, or renews it when holder has it, for ttl. It returns whether holder
	// holds it now. Background jobs run on the instance that holds their lease.
	Lease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error)

	Close() error
}

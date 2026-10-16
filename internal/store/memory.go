package store

import (
	"context"
	"slices"
	"sync"
	"time"
)

// Memory keeps everything in this process: tests, demos, and a hub that may forget its team when it restarts.
type Memory struct {
	mu      sync.Mutex
	users   map[string]User
	devices map[string]Device
	pending map[string]Pending
	codes   map[string]Code
	kv      map[string][]byte
	events  []Event
	leases  map[string]lease
}

// lease is who holds a lease and until when. Firestore stores it as is.
type lease struct {
	Holder string
	Until  time.Time
}

func NewMemory() *Memory {
	return &Memory{users: map[string]User{}, devices: map[string]Device{}, pending: map[string]Pending{},
		codes: map[string]Code{}, kv: map[string][]byte{}, leases: map[string]lease{}}
}

func (s *Memory) Users(context.Context) ([]User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	return out, nil
}

func (s *Memory) PutUser(_ context.Context, u User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[u.Email] = u
	return nil
}

func (s *Memory) Devices(context.Context) ([]Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Device, 0, len(s.devices))
	for _, d := range s.devices {
		d.Report = slices.Clone(d.Report)
		out = append(out, d)
	}
	return out, nil
}

func (s *Memory) PutDevice(_ context.Context, d Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d.Report = slices.Clone(d.Report)
	s.devices[d.ID] = d
	return nil
}

func (s *Memory) SeenDevice(_ context.Context, id, version string, at time.Time) error {
	return s.changeDevice(id, func(d *Device) { d.Version, d.LastSeen = version, at })
}

func (s *Memory) ReportDevice(_ context.Context, id string, report []byte, at time.Time) error {
	return s.changeDevice(id, func(d *Device) { d.Report, d.ReportAt = slices.Clone(report), at })
}

func (s *Memory) changeDevice(id string, change func(*Device)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok {
		return ErrNotFound
	}
	change(&d)
	s.devices[id] = d
	return nil
}

func (s *Memory) AddPending(_ context.Context, p Pending) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[p.Msg.ID] = p
	return nil
}

func (s *Memory) Pending(_ context.Context, email string) ([]Pending, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Pending
	now := time.Now()
	for _, p := range s.pending {
		if p.Email == email && now.Before(p.Expires) {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b Pending) int { return a.Created.Compare(b.Created) })
	return out, nil
}

func (s *Memory) DropPending(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, id)
	return nil
}

func (s *Memory) TakeExpired(_ context.Context, now time.Time) ([]Pending, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Pending
	for id, p := range s.pending {
		if !now.Before(p.Expires) {
			out = append(out, p)
			delete(s.pending, id)
		}
	}
	return out, nil
}

func (s *Memory) PutCode(_ context.Context, c Code) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes[c.ID] = c
	return nil
}

func (s *Memory) TakeCode(_ context.Context, id string) (Code, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.codes[id]
	delete(s.codes, id)
	if !ok || !time.Now().Before(c.Expires) {
		return Code{}, ErrNotFound
	}
	return c, nil
}

func (s *Memory) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.kv[key]
	if !ok {
		return nil, ErrNotFound
	}
	return slices.Clone(v), nil
}

func (s *Memory) Put(_ context.Context, key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kv[key] = slices.Clone(value)
	return nil
}

func (s *Memory) AddEvent(_ context.Context, e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	if len(s.events) > keepEvents+1000 {
		s.events = slices.Clone(s.events[len(s.events)-keepEvents:])
	}
	return nil
}

func (s *Memory) Events(_ context.Context, limit int) ([]Event, error) {
	if limit <= 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.events[max(0, len(s.events)-limit):])
	slices.Reverse(out)
	return out, nil
}

func (s *Memory) Lease(_ context.Context, name, holder string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.leases[name]
	if l.Holder != holder && time.Now().Before(l.Until) {
		return false, nil
	}
	s.leases[name] = lease{Holder: holder, Until: time.Now().Add(ttl)}
	return true, nil
}

func (s *Memory) Close() error { return nil }

// Package storetest is the contract every store backend passes. A backend's tests call Run with a function that
// opens an empty store.
package storetest

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/store"
)

// Run tests a backend. open returns an empty store for one test, which Run closes when the test ends.
func Run(t *testing.T, open func(t *testing.T) store.Store) {
	for _, tc := range []struct {
		name string
		test func(*testing.T, store.Store)
	}{
		{"Users", users},
		{"Devices", devices},
		{"DeviceFields", deviceFields},
		{"Pending", pending},
		{"TakeExpired", takeExpired},
		{"TakeExpiredOnce", takeExpiredOnce},
		{"Codes", codes},
		{"TakeCodeOnce", takeCodeOnce},
		{"Documents", documents},
		{"Events", events},
		{"EventsByTime", eventsByTime},
		{"ConcurrentWrites", concurrentWrites},
		{"Lease", lease},
		{"LeaseRunsOut", leaseRunsOut},
		{"LeaseOnce", leaseOnce},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := open(t)
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			tc.test(t, s)
		})
	}
}

func users(t *testing.T, s store.Store) {
	ctx := t.Context()
	at := now()
	alice := store.User{Email: "alice@example.com", Handle: "alice", Name: "Alice", Admin: true, Created: at}
	bob := store.User{Email: "bob@example.com", Handle: "bob", Created: at.Add(time.Minute)}
	must(t, s.PutUser(ctx, alice), s.PutUser(ctx, bob))
	alice.Name, alice.Admin, alice.Disabled = "Alice Martin", false, true
	must(t, s.PutUser(ctx, alice))

	got, err := s.Users(ctx)
	must(t, err)
	slices.SortFunc(got, func(a, b store.User) int { return strings.Compare(a.Email, b.Email) })
	same(t, "users", got, []store.User{alice, bob})
}

func devices(t *testing.T, s store.Store) {
	ctx := t.Context()
	at := now()
	mac := store.Device{ID: "d-mac", Email: "alice@example.com", Host: "alice-mbp", TokenHash: "5e884898da28",
		Created: at.Add(-time.Hour), LastSeen: at, Version: "1.0.0", Report: []byte(`{"claude":"2.1.0","ok":true}`), ReportAt: at}
	fresh := store.Device{ID: "d-fresh", Email: "bob@example.com", TokenHash: "a1b2c3d4e5f6", Created: at} // never seen nor checked
	must(t, s.PutDevice(ctx, mac), s.PutDevice(ctx, fresh))
	mac.LastSeen, mac.Revoked, mac.Version = at.Add(time.Minute), true, "1.0.1"
	mac.Report, mac.ReportAt = []byte(`{"claude":"2.1.1","ok":false}`), at.Add(time.Minute)
	must(t, s.PutDevice(ctx, mac))

	got, err := s.Devices(ctx)
	must(t, err)
	slices.SortFunc(got, func(a, b store.Device) int { return strings.Compare(a.ID, b.ID) })
	same(t, "devices", got, []store.Device{fresh, mac})
}

// SeenDevice and ReportDevice change their fields only: a revoke written meanwhile stays.
func deviceFields(t *testing.T, s store.Store) {
	ctx := t.Context()
	at := now()
	d := store.Device{ID: "d-mac", Email: "alice@example.com", Host: "alice-mbp", TokenHash: "5e884898da28", Created: at,
		LastSeen: at, Version: "1.0.0"}
	must(t, s.PutDevice(ctx, d))
	d.Revoked = true
	must(t, s.PutDevice(ctx, d))
	must(t, s.SeenDevice(ctx, "d-mac", "1.1.0", at.Add(time.Minute)), s.ReportDevice(ctx, "d-mac", []byte(`{"ok":true}`), at.Add(2*time.Minute)))
	d.Version, d.LastSeen, d.Report, d.ReportAt = "1.1.0", at.Add(time.Minute), []byte(`{"ok":true}`), at.Add(2*time.Minute)
	got, err := s.Devices(ctx)
	must(t, err)
	same(t, "device", got, []store.Device{d})
	if err := s.SeenDevice(ctx, "nobody", "1.0.0", at); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SeenDevice on an unknown laptop: %v", err)
	}
	if err := s.ReportDevice(ctx, "nobody", nil, at); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ReportDevice on an unknown laptop: %v", err)
	}
}

func pending(t *testing.T, s store.Store) {
	ctx := t.Context()
	at := now()
	first := store.Pending{Msg: msg("m1"), Email: "alice@example.com", Sender: "d-bob",
		Created: at.Add(-3 * time.Minute), Expires: at.Add(time.Hour)}
	second := store.Pending{Msg: msg("m2"), Email: "alice@example.com", Device: "d-alice", Sender: "d-carol",
		Created: at.Add(-2 * time.Minute), Expires: at.Add(time.Hour)}
	expired := store.Pending{Msg: msg("m3"), Email: "alice@example.com", Created: at.Add(-time.Hour), Expires: at.Add(-time.Minute)}
	forBob := store.Pending{Msg: msg("m4"), Email: "bob@example.com", Created: at, Expires: at.Add(time.Hour)}
	for _, p := range []store.Pending{second, expired, forBob, first} {
		must(t, s.AddPending(ctx, p))
	}
	second.Msg.Text = "never mind"
	must(t, s.AddPending(ctx, second)) // the same message again replaces it

	for email, want := range map[string][]store.Pending{
		"alice@example.com": {first, second},
		"bob@example.com":   {forBob},
		"carol@example.com": nil,
	} {
		got, err := s.Pending(ctx, email)
		must(t, err)
		same(t, "pending for "+email, got, want)
	}

	must(t, s.DropPending(ctx, "alice@example.com", "m1"), s.DropPending(ctx, "alice@example.com", "unknown"))
	got, err := s.Pending(ctx, "alice@example.com")
	must(t, err)
	same(t, "pending after a drop", got, []store.Pending{second})

	// laptops choose message ids: the same id for another person neither replaces nor drops alice's
	sameID := store.Pending{Msg: msg("m2"), Email: "bob@example.com", Created: at, Expires: at.Add(time.Hour)}
	must(t, s.AddPending(ctx, sameID), s.DropPending(ctx, "bob@example.com", "m2"))
	got, err = s.Pending(ctx, "alice@example.com")
	must(t, err)
	same(t, "alice's pending after bob's m2 came and went", got, []store.Pending{second})
}

func takeExpired(t *testing.T, s store.Store) {
	ctx := t.Context()
	at := now()
	old := store.Pending{Msg: msg("m1"), Email: "alice@example.com", Sender: "d-bob",
		Created: at.Add(-2 * time.Hour), Expires: at.Add(-time.Hour)}
	recent := store.Pending{Msg: msg("m2"), Email: "bob@example.com", Device: "d-bob", Sender: "d-carol",
		Created: at.Add(-time.Hour), Expires: at.Add(-time.Second)}
	live := store.Pending{Msg: msg("m3"), Email: "alice@example.com", Sender: "d-bob", Created: at, Expires: at.Add(time.Hour)}
	must(t, s.AddPending(ctx, old), s.AddPending(ctx, recent), s.AddPending(ctx, live))

	take := func(now time.Time, want ...store.Pending) {
		t.Helper()
		got, err := s.TakeExpired(ctx, now)
		must(t, err)
		slices.SortFunc(got, func(a, b store.Pending) int { return strings.Compare(a.Msg.ID, b.Msg.ID) })
		same(t, "expired", got, want)
	}
	take(at, old, recent)
	take(at)
	left, err := s.Pending(ctx, "alice@example.com")
	must(t, err)
	same(t, "left pending", left, []store.Pending{live})
	take(at.Add(2*time.Hour), live) // now is the caller's, not the clock's
}

func takeExpiredOnce(t *testing.T, s store.Store) {
	ctx := t.Context()
	at := now()
	const n = 40
	for i := range n {
		must(t, s.AddPending(ctx, store.Pending{Msg: msg(fmt.Sprint("m", i)), Email: "alice@example.com", Created: at.Add(-time.Hour),
			Expires: at.Add(-time.Minute)}))
	}
	var mu sync.Mutex
	taken := map[string]int{}
	together(func(int) {
		got, err := s.TakeExpired(ctx, at)
		if err != nil {
			t.Error(err)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, p := range got {
			taken[p.Msg.ID]++
		}
	})
	if len(taken) != n {
		t.Errorf("%d of %d expired messages taken", len(taken), n)
	}
	for id, k := range taken {
		if k != 1 {
			t.Errorf("%s taken %d times", id, k)
		}
	}
}

func codes(t *testing.T, s store.Store) {
	ctx := t.Context()
	at := now()
	code := store.Code{ID: "c1", Email: "alice@example.com", Host: "alice-mbp",
		Challenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", Expires: at.Add(10 * time.Minute)}
	stale := store.Code{ID: "c2", Email: "alice@example.com", Challenge: "x", Expires: at.Add(-time.Second)}
	must(t, s.PutCode(ctx, code), s.PutCode(ctx, stale))

	got, err := s.TakeCode(ctx, "c1")
	must(t, err)
	same(t, "code", []store.Code{got}, []store.Code{code})
	for _, id := range []string{"c1", "c2", "unknown"} { // taken, expired, never put
		if _, err := s.TakeCode(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("TakeCode(%q) = %v, want ErrNotFound", id, err)
		}
	}
}

func takeCodeOnce(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.PutCode(ctx, store.Code{ID: "c1", Email: "alice@example.com", Challenge: "x", Expires: now().Add(time.Minute)}))
	var won atomic.Int32
	together(func(int) {
		_, err := s.TakeCode(ctx, "c1")
		switch {
		case err == nil:
			won.Add(1)
		case !errors.Is(err, store.ErrNotFound):
			t.Error(err)
		}
	})
	if n := won.Load(); n != 1 {
		t.Errorf("the code was taken %d times, want once", n)
	}
}

func documents(t *testing.T, s store.Store) {
	ctx := t.Context()
	if _, err := s.Get(ctx, "settings"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get of a missing key = %v, want ErrNotFound", err)
	}
	must(t, s.Put(ctx, "settings", []byte(`{"limit":10}`)), s.Put(ctx, "prefs/alice@example.com", []byte(`{"mute":["bob"]}`)))
	must(t, s.Put(ctx, "settings", []byte(`{"limit":20}`)))
	for key, want := range map[string]string{"settings": `{"limit":20}`, "prefs/alice@example.com": `{"mute":["bob"]}`} {
		got, err := s.Get(ctx, key)
		if err != nil || string(got) != want {
			t.Errorf("Get(%q) = %q, %v; want %q", key, got, err, want)
		}
	}
}

func events(t *testing.T, s store.Store) {
	ctx := t.Context()
	at := now()
	var newest []store.Event
	for i := range 5 {
		e := store.Event{At: at.Add(time.Duration(i) * time.Second), Kind: fmt.Sprint("kind-", i), Actor: "alice@example.com",
			Subject: "bob@example.com", Detail: fmt.Sprint("detail ", i)}
		must(t, s.AddEvent(ctx, e))
		newest = append([]store.Event{e}, newest...)
	}
	for _, limit := range []int{10, 2, 0} {
		got, err := s.Events(ctx, limit)
		must(t, err)
		same(t, fmt.Sprint("events, limit ", limit), got, newest[:min(limit, len(newest))])
	}
}

// Events come newest first by when they happened, not by when they were added: two instances add theirs in any
// order.
func eventsByTime(t *testing.T, s store.Store) {
	ctx := t.Context()
	at := now()
	for _, i := range []int{1, 3, 0, 2} {
		must(t, s.AddEvent(ctx, store.Event{At: at.Add(time.Duration(i) * time.Second), Kind: fmt.Sprint("kind-", i)}))
	}
	got, err := s.Events(ctx, 3)
	must(t, err)
	var kinds []string
	for _, e := range got {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, " ") != "kind-3 kind-2 kind-1" {
		t.Errorf("events %v, want kind-3 kind-2 kind-1", kinds)
	}
}

// Instances write at once: each write lands, none fails.
func concurrentWrites(t *testing.T, s store.Store) {
	ctx := t.Context()
	at := now()
	together(func(i int) {
		for j := range 10 {
			id := fmt.Sprint("m", i, "-", j)
			for _, err := range []error{
				s.AddPending(ctx, store.Pending{Msg: msg(id), Email: "alice@example.com", Created: at, Expires: at.Add(time.Hour)}),
				s.Put(ctx, fmt.Sprint("doc-", i), []byte(id)),
				s.AddEvent(ctx, store.Event{At: at, Kind: id}),
				s.SeenDevice(ctx, "d-mac", "1.0.0", at),
			} {
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					t.Error(err)
				}
			}
		}
	})
	got, err := s.Pending(ctx, "alice@example.com")
	must(t, err)
	if len(got) != 80 {
		t.Errorf("%d of 80 messages kept", len(got))
	}
	evs, err := s.Events(ctx, 100)
	must(t, err)
	if len(evs) != 80 {
		t.Errorf("%d of 80 events kept", len(evs))
	}
}

func lease(t *testing.T, s store.Store) {
	for _, c := range []struct {
		name, holder string
		want         bool
	}{
		{"jobs", "hub-1", true},
		{"jobs", "hub-2", false}, // hub-1 holds it
		{"jobs", "hub-1", true},  // and renews it
		{"other", "hub-2", true}, // a lease of its own
	} {
		got, err := s.Lease(t.Context(), c.name, c.holder, time.Hour)
		if err != nil || got != c.want {
			t.Errorf("Lease(%s, %s) = %v, %v; want %v", c.name, c.holder, got, err, c.want)
		}
	}
}

func leaseRunsOut(t *testing.T, s store.Store) {
	const ttl = time.Second
	take := func(holder string, want bool) {
		t.Helper()
		got, err := s.Lease(t.Context(), "jobs", holder, ttl)
		if err != nil || got != want {
			t.Errorf("Lease(jobs, %s) = %v, %v; want %v", holder, got, err, want)
		}
	}
	take("hub-1", true)
	take("hub-2", false)
	time.Sleep(ttl + 100*time.Millisecond)
	take("hub-2", true)
	take("hub-1", false)
}

func leaseOnce(t *testing.T, s store.Store) {
	var won atomic.Int32
	together(func(i int) {
		held, err := s.Lease(t.Context(), "jobs", fmt.Sprint("hub-", i), time.Hour)
		if err != nil {
			t.Error(err)
		}
		if held {
			won.Add(1)
		}
	})
	if n := won.Load(); n != 1 {
		t.Errorf("%d hubs hold the lease, want 1", n)
	}
}

// now is the time as precisely as every backend keeps it: to the microsecond.
func now() time.Time { return time.Now().Truncate(time.Microsecond) }

func msg(id string) protocol.Msg {
	return protocol.Msg{ID: id, From: "bob/main-1", To: "alice", Text: "can you look at " + id + "?", At: 1760000000000}
}

func must(t *testing.T, errs ...error) {
	t.Helper()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// together calls f from 8 goroutines at once, as hub instances racing for the same thing would.
func together(f func(i int)) {
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() { f(i) })
	}
	wg.Wait()
}

// same fails the test unless got and want hold equal items in the same order.
func same[T any](t *testing.T, what string, got, want []T) {
	t.Helper()
	if !slices.EqualFunc(got, want, func(a, b T) bool { return equal(a, b) }) {
		t.Errorf("%s:\n got %+v\nwant %+v", what, got, want)
	}
}

// equal compares two structs field by field: times with Equal, since a backend may return another zone, byte slices
// by content, since nil and empty are the same, and the rest with ==.
func equal(a, b any) bool {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	for i := range va.NumField() {
		switch fa, fb := va.Field(i).Interface(), vb.Field(i).Interface(); fa := fa.(type) {
		case time.Time:
			if !fa.Equal(fb.(time.Time)) {
				return false
			}
		case []byte:
			if !bytes.Equal(fa, fb.([]byte)) {
				return false
			}
		default:
			if fa != fb {
				return false
			}
		}
	}
	return true
}

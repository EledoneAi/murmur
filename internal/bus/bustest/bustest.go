// Package bustest holds the contract tests every bus backend passes.
package bustest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/internal/bus"
)

// Run checks what every bus does. open returns a new bus on the backend under test.
func Run(t *testing.T, open func(t *testing.T) bus.Bus) {
	t.Run("own events", func(t *testing.T) { ownEvents(t, open) })
	t.Run("large event", func(t *testing.T) { largeEvent(t, open) })
	t.Run("close", func(t *testing.T) { closing(t, open) })
}

// RunShared runs Run, then checks that buses opened on the same backend hear each other, as the instances of one
// hub do. Every backend but Local is shared.
func RunShared(t *testing.T, open func(t *testing.T) bus.Bus) {
	Run(t, open)
	t.Run("two buses", func(t *testing.T) { twoBuses(t, open) })
}

func ownEvents(t *testing.T, open func(t *testing.T) bus.Bus) {
	b := open(t)
	defer b.Close()
	from := rand.Text()
	want := []bus.Event{
		{Kind: "presence", From: from, Data: json.RawMessage(`{"agents":["main-1"]}`)},
		{Kind: "deliver", From: from, To: "another", Data: json.RawMessage(`{"n":1}`)},
		{Kind: "changed", From: from},
	}
	for _, e := range want {
		publish(t, b, e)
	}
	if got := collect(t, b, len(want), from); !reflect.DeepEqual(got, want) {
		t.Errorf("got %s, want %s", show(got), show(want))
	}
}

// A 200 KB event arrives whole, and in order with the small ones around it.
func largeEvent(t *testing.T, open func(t *testing.T) bus.Bus) {
	b := open(t)
	defer b.Close()
	from := rand.Text()
	big, _ := json.Marshal(base64.StdEncoding.EncodeToString(randomBytes(150 << 10)))
	want := []bus.Event{
		{Kind: "changed", From: from, Data: json.RawMessage(`1`)},
		{Kind: "deliver", From: from, Data: big},
		{Kind: "changed", From: from, Data: json.RawMessage(`2`)},
	}
	for _, e := range want {
		publish(t, b, e)
	}
	got := collect(t, b, len(want), from)
	if string(got[0].Data) != "1" || string(got[2].Data) != "2" {
		t.Errorf("small events around the large one arrived as %s, %s", got[0].Data, got[2].Data)
	}
	if !bytes.Equal(got[1].Data, big) {
		t.Errorf("the %d-byte event arrived as %d bytes, or changed", len(big), len(got[1].Data))
	}
}

// Close ends delivery: Events closes, Publish fails and every goroutine the bus started stops.
func closing(t *testing.T, open func(t *testing.T) bus.Bus) {
	before := runtime.NumGoroutine()
	b := open(t)
	from := rand.Text()
	publish(t, b, bus.Event{Kind: "changed", From: from})
	collect(t, b, 1, from)
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	timeout := time.After(5 * time.Second)
	for ok := true; ok; {
		select {
		case _, ok = <-b.Events():
		case <-timeout:
			t.Fatal("Events still open after Close")
		}
	}
	if err := b.Publish(context.Background(), bus.Event{Kind: "changed", From: from}); err == nil {
		t.Error("Publish after Close succeeded")
	}
	for deadline := time.Now().Add(5 * time.Second); runtime.NumGoroutine() > before; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			stacks := make([]byte, 1<<20)
			t.Fatalf("%d goroutines left after Close:\n%s", runtime.NumGoroutine()-before, stacks[:runtime.Stack(stacks, true)])
		}
	}
}

// Each bus hears both, and each publisher's events in the order it published them.
func twoBuses(t *testing.T, open func(t *testing.T) bus.Bus) {
	a, b := open(t), open(t)
	defer a.Close()
	defer b.Close()
	fromA, fromB := rand.Text(), rand.Text()
	const n = 50
	for i := range n {
		publish(t, a, bus.Event{Kind: "presence", From: fromA, Data: json.RawMessage(strconv.Itoa(i))})
		publish(t, b, bus.Event{Kind: "presence", From: fromB, Data: json.RawMessage(strconv.Itoa(i))})
	}
	for name, x := range map[string]bus.Bus{"a": a, "b": b} {
		next := map[string]int{}
		for _, e := range collect(t, x, 2*n, fromA, fromB) {
			if want := strconv.Itoa(next[e.From]); string(e.Data) != want {
				t.Fatalf("bus %s: event %s from %s, want %s", name, e.Data, e.From, want)
			}
			next[e.From]++
		}
	}
}

// Recovers checks that b delivers again after a test cut its connection, that Events stayed open, and that the bus
// said it reconnected. Events published while it reconnects are lost, so it publishes until one comes through.
func Recovers(t *testing.T, b bus.Bus) {
	t.Helper()
	from, told := rand.Text(), false
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(20 * time.Second)
	for {
		select {
		case <-tick.C:
			_ = b.Publish(context.Background(), bus.Event{Kind: "changed", From: from})
		case e, ok := <-b.Events():
			if !ok {
				t.Fatal("Events closed when the connection was cut")
			}
			told = told || e.Kind == bus.Reconnected
			if e.From == from {
				if !told {
					t.Fatal("events came through again without a Reconnected event first")
				}
				return
			}
		case <-timeout:
			t.Fatal("no event came through after the connection was cut")
		}
	}
}

// collect reads n events published by from, skipping any others: a backend may be shared with other tests, or a
// hub.
func collect(t *testing.T, b bus.Bus, n int, from ...string) []bus.Event {
	t.Helper()
	var got []bus.Event
	timeout := time.After(10 * time.Second)
	for len(got) < n {
		select {
		case e, open := <-b.Events():
			if !open {
				t.Fatalf("Events closed after %d of %d events", len(got), n)
			}
			if slices.Contains(from, e.From) {
				got = append(got, e)
			}
		case <-timeout:
			t.Fatalf("got %d of %d events", len(got), n)
		}
	}
	return got
}

func publish(t *testing.T, b bus.Bus, e bus.Event) {
	t.Helper()
	if err := b.Publish(context.Background(), e); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

func randomBytes(n int) []byte {
	p := make([]byte, n)
	_, _ = rand.Read(p)
	return p
}

func show(events []bus.Event) string {
	p, _ := json.Marshal(events)
	return string(p)
}

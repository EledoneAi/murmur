package bus_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/bus"
)

// A hub that reads its bus slowly holds up publishers only as long as their context allows, and closing the bus
// lets go of those still waiting.
func TestLocalSlowReader(t *testing.T) {
	b := bus.NewLocal()
	for {
		ctx, done := context.WithTimeout(context.Background(), 20*time.Millisecond)
		err := b.Publish(ctx, bus.Event{Kind: "changed", From: "one"})
		done()
		if errors.Is(err, context.DeadlineExceeded) {
			break // full: nobody reads
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { errs <- b.Publish(context.Background(), bus.Event{Kind: "changed", From: "two"}) })
	}
	time.Sleep(20 * time.Millisecond)
	closed := make(chan struct{})
	go func() { _ = b.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close waited on publishers")
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err == nil {
			t.Error("a publish after Close succeeded")
		}
	}
	if err := b.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
}

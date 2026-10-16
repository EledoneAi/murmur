package bus

import (
	"context"
	"encoding/json"

	"github.com/redis/go-redis/v9"
)

// redisBus publishes and subscribes on one Pub/Sub channel. go-redis reconnects and subscribes again by itself.
type redisBus struct {
	client  *redis.Client
	pubsub  *redis.PubSub
	channel string
	events  chan Event
	ctx     context.Context // ends at Close
	cancel  context.CancelFunc
	stopped chan struct{}
}

func openRedis(ctx context.Context, raw string) (Bus, error) {
	raw, channel, err := cutChannel(raw)
	if err != nil {
		return nil, err
	}
	opt, err := redis.ParseURL(raw)
	if err != nil {
		return nil, err
	}
	client := redis.NewClient(opt)
	pubsub := client.Subscribe(ctx, channel)
	// Subscribe returns before Redis confirms. Waiting for it means every event published after Open is heard.
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		_ = client.Close()
		return nil, err
	}
	b := &redisBus{client: client, pubsub: pubsub, channel: channel, events: make(chan Event, 4096), stopped: make(chan struct{})}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	go b.run(pubsub.ChannelWithSubscriptions())
	return b, nil
}

func (b *redisBus) Publish(ctx context.Context, e Event) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return b.client.Publish(ctx, b.channel, payload).Err()
}

func (b *redisBus) Events() <-chan Event { return b.events }

func (b *redisBus) Close() error {
	b.cancel()
	_ = b.pubsub.Close()
	<-b.stopped
	return b.client.Close()
}

// run hands messages on to Events. msgs stays open across reconnects and closes with the PubSub. go-redis says it
// subscribed again after a reconnect with a Subscription, which becomes a Reconnected event.
func (b *redisBus) run(msgs <-chan any) {
	defer close(b.stopped)
	defer close(b.events)
	for m := range msgs {
		var e Event
		switch m := m.(type) {
		case *redis.Subscription:
			if m.Kind != "subscribe" {
				continue
			}
			e.Kind = Reconnected
		case *redis.Message:
			if json.Unmarshal([]byte(m.Payload), &e) != nil {
				continue
			}
		default:
			continue
		}
		// Once closed, keep draining msgs: go-redis would otherwise wait a minute to send on it before it stops.
		select {
		case b.events <- e:
		case <-b.ctx.Done():
		}
	}
}

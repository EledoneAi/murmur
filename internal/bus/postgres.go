package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NOTIFY payloads must be shorter than 8000 bytes. A larger event goes into this table and its notification
// carries only the row's id. Rows go after five minutes: every instance has read them by then, or never will.
const spillTable = `CREATE TABLE IF NOT EXISTS murmur_bus (
	id      bigserial PRIMARY KEY,
	payload bytea NOT NULL,
	created timestamptz NOT NULL DEFAULT now()
)`

// postgresBus publishes with NOTIFY and hears every event on one connection that LISTENs.
type postgresBus struct {
	pool    *pgxpool.Pool // publishes, and reads spilled events
	config  *pgx.ConnConfig
	channel string
	events  chan Event
	ctx     context.Context // ends at Close
	cancel  context.CancelFunc
	stopped chan struct{}
}

func openPostgres(ctx context.Context, raw string) (Bus, error) {
	raw, channel, err := cutChannel(raw)
	if err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(raw)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	// Instances starting together race to create the table, which can fail one of them: the lock lines them up.
	if _, err := pool.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('murmur_bus')); "+spillTable); err != nil {
		pool.Close()
		return nil, fmt.Errorf("bus table: %w", err)
	}
	b := &postgresBus{pool: pool, config: cfg.ConnConfig, channel: channel, events: make(chan Event, 4096), stopped: make(chan struct{})}
	// Listening before Open returns means every event published after it is heard.
	conn, err := b.listen(ctx)
	if err != nil {
		pool.Close()
		return nil, err
	}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	go b.run(conn)
	return b, nil
}

func (b *postgresBus) Publish(ctx context.Context, e Event) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(payload) < 8000 {
		_, err = b.pool.Exec(ctx, "SELECT pg_notify($1, $2)", b.channel, string(payload))
		return err
	}
	_, err = b.pool.Exec(ctx, `WITH spilled AS (INSERT INTO murmur_bus (payload) VALUES ($2) RETURNING id)
		SELECT pg_notify($1, id::text) FROM spilled`, b.channel, payload)
	return err
}

func (b *postgresBus) Events() <-chan Event { return b.events }

func (b *postgresBus) Close() error {
	b.cancel()
	<-b.stopped
	b.pool.Close()
	return nil
}

// listen opens the connection notifications arrive on. A server that accepts connections without answering
// would otherwise hold it forever.
func (b *postgresBus) listen(ctx context.Context) (*pgx.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, b.config)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{b.channel}.Sanitize()); err != nil {
		_ = conn.Close(ctx)
		return nil, err
	}
	return conn, nil
}

// run hands notifications on to Events until Close, listening again whenever the connection fails.
func (b *postgresBus) run(conn *pgx.Conn) {
	defer close(b.stopped)
	defer close(b.events)
	for conn != nil {
		err := b.receive(conn)
		_ = conn.Close(b.ctx)
		if conn = b.relisten(err); conn != nil {
			select {
			case b.events <- Event{Kind: Reconnected}:
			case <-b.ctx.Done():
			}
		}
	}
}

// relisten opens a new listening connection after err ended the last one, retrying with backoff. It returns nil
// once the bus is closed.
func (b *postgresBus) relisten(err error) *pgx.Conn {
	for backoff := 500 * time.Millisecond; b.ctx.Err() == nil; backoff = min(backoff*2, 30*time.Second) {
		log.Printf("bus: postgres listener: %v; listening again in %v", err, backoff)
		select {
		case <-time.After(backoff):
		case <-b.ctx.Done():
			return nil
		}
		var conn *pgx.Conn
		if conn, err = b.listen(b.ctx); err == nil {
			return conn
		}
	}
	return nil
}

// receive hands conn's notifications on to Events until conn fails or the bus closes. Every 30 seconds it
// deletes old spilled events over conn: a connection that only reads can't tell a quiet server from a dead one.
func (b *postgresBus) receive(conn *pgx.Conn) error {
	check := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithDeadline(b.ctx, check)
		n, err := conn.WaitForNotification(ctx)
		cancel()
		switch {
		case err == nil:
			b.deliver(n.Payload)
		case errors.Is(err, context.DeadlineExceeded):
			if err := b.clean(conn); err != nil {
				return err
			}
			check = time.Now().Add(30 * time.Second)
		default:
			return err
		}
	}
}

func (b *postgresBus) clean(conn *pgx.Conn) error {
	ctx, cancel := context.WithTimeout(b.ctx, 10*time.Second)
	defer cancel()
	_, err := conn.Exec(ctx, "DELETE FROM murmur_bus WHERE created < now() - interval '5 minutes'")
	return err
}

// deliver hands one notification on to Events. A payload that isn't an event is the id of a spilled one.
func (b *postgresBus) deliver(payload string) {
	data := []byte(payload)
	if !strings.HasPrefix(payload, "{") {
		id, err := strconv.ParseInt(payload, 10, 64)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(b.ctx, 10*time.Second)
		err = b.pool.QueryRow(ctx, "SELECT payload FROM murmur_bus WHERE id = $1", id).Scan(&data)
		cancel()
		if err != nil {
			log.Printf("bus: spilled event %d: %v", id, err)
			return
		}
	}
	var e Event
	if json.Unmarshal(data, &e) != nil {
		return
	}
	select {
	case b.events <- e:
	case <-b.ctx.Done():
	}
}

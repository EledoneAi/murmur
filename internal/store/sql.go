package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync/atomic"
	"time"
)

// sqlStore keeps the store in SQLite or Postgres. Both run the same queries: $1 placeholders, ON CONFLICT and
// RETURNING mean the same in each, and times are stored as microseconds since the Unix epoch.
type sqlStore struct {
	db    *sql.DB
	added atomic.Int64 // events this process added, to prune the log every pruneEvery of them
}

//go:embed migrations/*.sql
var migrations embed.FS

// migrate applies the migrations db hasn't recorded in schema_migrations, in name order and in one transaction.
// A file named name.sql is for both dialects, name.sqlite.sql or name.postgres.sql for one.
func migrate(ctx context.Context, db *sql.DB, dialect string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	if dialect == "postgres" {
		// Hubs starting together take turns here instead of racing to create the same tables. SQLite gets the
		// same from its transactions beginning immediate.
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('murmur migrations'))`); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied BIGINT NOT NULL)`)
	if err != nil {
		return err
	}
	files, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	for _, f := range files {
		name := f.Name()
		if d := path.Ext(strings.TrimSuffix(name, ".sql")); d != "" && d != "."+dialect {
			continue
		}
		err := tx.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE name = $1`, name).Scan(new(string))
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		script, err := fs.ReadFile(migrations, "migrations/"+name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(script)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations (name, applied) VALUES ($1, $2)`, name, time.Now().UnixMicro())
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// micros scans a time stored as microseconds since the Unix epoch.
type micros struct{ t *time.Time }

func (m micros) Scan(v any) error {
	n, ok := v.(int64)
	if !ok {
		return fmt.Errorf("time stored as %T, not microseconds", v)
	}
	*m.t = time.UnixMicro(n).UTC()
	return nil
}

// list runs a query and scans each row it returns into a T.
func list[T any](ctx context.Context, db *sql.DB, query string, scan func(*sql.Rows, *T) error, args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		var v T
		if err := scan(rows, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *sqlStore) Users(ctx context.Context) ([]User, error) {
	return list(ctx, s.db, `SELECT email, handle, name, admin, disabled, created FROM users`, func(r *sql.Rows, u *User) error {
		return r.Scan(&u.Email, &u.Handle, &u.Name, &u.Admin, &u.Disabled, micros{&u.Created})
	})
}

func (s *sqlStore) PutUser(ctx context.Context, u User) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO users (email, handle, name, admin, disabled, created)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (email) DO UPDATE SET handle = excluded.handle, name = excluded.name, admin = excluded.admin,
			disabled = excluded.disabled, created = excluded.created`,
		u.Email, u.Handle, u.Name, u.Admin, u.Disabled, u.Created.UnixMicro())
	return err
}

func (s *sqlStore) Devices(ctx context.Context) ([]Device, error) {
	return list(ctx, s.db, `SELECT id, email, host, token_hash, created, last_seen, revoked, version, report, report_at
		FROM devices`, func(r *sql.Rows, d *Device) error {
		return r.Scan(&d.ID, &d.Email, &d.Host, &d.TokenHash, micros{&d.Created}, micros{&d.LastSeen}, &d.Revoked, &d.Version,
			&d.Report, micros{&d.ReportAt})
	})
}

func (s *sqlStore) PutDevice(ctx context.Context, d Device) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO devices
		(id, email, host, token_hash, created, last_seen, revoked, version, report, report_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (id) DO UPDATE SET email = excluded.email, host = excluded.host, token_hash = excluded.token_hash,
			created = excluded.created, last_seen = excluded.last_seen, revoked = excluded.revoked, version = excluded.version,
			report = excluded.report, report_at = excluded.report_at`,
		d.ID, d.Email, d.Host, d.TokenHash, d.Created.UnixMicro(), d.LastSeen.UnixMicro(), d.Revoked, d.Version, d.Report,
		d.ReportAt.UnixMicro())
	return err
}

func (s *sqlStore) SeenDevice(ctx context.Context, id, version string, at time.Time) error {
	return s.changeDevice(ctx, `UPDATE devices SET version = $2, last_seen = $3 WHERE id = $1`, id, version, at.UnixMicro())
}

func (s *sqlStore) ReportDevice(ctx context.Context, id string, report []byte, at time.Time) error {
	return s.changeDevice(ctx, `UPDATE devices SET report = $2, report_at = $3 WHERE id = $1`, id, report, at.UnixMicro())
}

func (s *sqlStore) changeDevice(ctx context.Context, query string, args ...any) error {
	r, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if n, err := r.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

const pendingColumns = `msg, email, device, sender, created, expires`

func scanPending(r *sql.Rows, p *Pending) error {
	var msg string
	if err := r.Scan(&msg, &p.Email, &p.Device, &p.Sender, micros{&p.Created}, micros{&p.Expires}); err != nil {
		return err
	}
	return json.Unmarshal([]byte(msg), &p.Msg)
}

func (s *sqlStore) AddPending(ctx context.Context, p Pending) error {
	msg, err := json.Marshal(p.Msg)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO pending (id, `+pendingColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET msg = excluded.msg, email = excluded.email, device = excluded.device,
			sender = excluded.sender, created = excluded.created, expires = excluded.expires`,
		p.Msg.ID, string(msg), p.Email, p.Device, p.Sender, p.Created.UnixMicro(), p.Expires.UnixMicro())
	return err
}

func (s *sqlStore) Pending(ctx context.Context, email string) ([]Pending, error) {
	return list(ctx, s.db, `SELECT `+pendingColumns+` FROM pending WHERE email = $1 AND expires > $2 ORDER BY created, id`,
		scanPending, email, time.Now().UnixMicro())
}

func (s *sqlStore) DropPending(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pending WHERE id = $1`, id)
	return err
}

func (s *sqlStore) TakeExpired(ctx context.Context, now time.Time) ([]Pending, error) {
	return list(ctx, s.db, `DELETE FROM pending WHERE expires <= $1 RETURNING `+pendingColumns, scanPending, now.UnixMicro())
}

func (s *sqlStore) PutCode(ctx context.Context, c Code) error {
	// Codes nobody traded are dropped here, so abandoned sign-ins don't pile up.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM codes WHERE expires <= $1`, time.Now().UnixMicro()); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO codes (id, email, host, challenge, expires) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE SET email = excluded.email, host = excluded.host, challenge = excluded.challenge,
			expires = excluded.expires`,
		c.ID, c.Email, c.Host, c.Challenge, c.Expires.UnixMicro())
	return err
}

func (s *sqlStore) TakeCode(ctx context.Context, id string) (Code, error) {
	c := Code{ID: id}
	err := s.db.QueryRowContext(ctx, `DELETE FROM codes WHERE id = $1 RETURNING email, host, challenge, expires`, id).
		Scan(&c.Email, &c.Host, &c.Challenge, micros{&c.Expires})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Code{}, ErrNotFound
	case err != nil:
		return Code{}, err
	case !time.Now().Before(c.Expires):
		return Code{}, ErrNotFound
	}
	return c, nil
}

func (s *sqlStore) Get(ctx context.Context, key string) ([]byte, error) {
	var v []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = $1`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

func (s *sqlStore) Put(ctx context.Context, key string, value []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kv (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *sqlStore) AddEvent(ctx context.Context, e Event) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO events (at, kind, actor, subject, detail) VALUES ($1, $2, $3, $4, $5)`,
		e.At.UnixMicro(), e.Kind, e.Actor, e.Subject, e.Detail)
	if err != nil || s.added.Add(1)%pruneEvery != 1 {
		return err
	}
	if err := s.pruneEvents(ctx, time.Now()); err != nil {
		return fmt.Errorf("event added, but pruning the log failed: %w", err)
	}
	return nil
}

// pruneEvents deletes the events older than keepEventsFor and those beyond the newest keepEvents.
func (s *sqlStore) pruneEvents(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM events
		WHERE at < $1 OR id <= (SELECT id FROM events ORDER BY id DESC LIMIT 1 OFFSET $2)`,
		now.Add(-keepEventsFor).UnixMicro(), keepEvents)
	return err
}

func (s *sqlStore) Events(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 {
		return nil, nil
	}
	return list(ctx, s.db, `SELECT at, kind, actor, subject, detail FROM events ORDER BY at DESC, id DESC LIMIT $1`,
		func(r *sql.Rows, e *Event) error {
			return r.Scan(micros{&e.At}, &e.Kind, &e.Actor, &e.Subject, &e.Detail)
		}, limit)
}

// Lease's update only happens when holder has the lease already or it ran out, and RETURNING says whether it did.
func (s *sqlStore) Lease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	now := time.Now()
	err := s.db.QueryRowContext(ctx, `INSERT INTO leases (name, holder, expires) VALUES ($1, $2, $3)
		ON CONFLICT (name) DO UPDATE SET holder = excluded.holder, expires = excluded.expires
			WHERE leases.holder = excluded.holder OR leases.expires <= $4
		RETURNING holder`, name, holder, now.Add(ttl).UnixMicro(), now.UnixMicro()).Scan(new(string))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *sqlStore) Close() error { return s.db.Close() }

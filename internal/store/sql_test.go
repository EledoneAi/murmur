package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestPruneEvents(t *testing.T) {
	ctx := t.Context()
	st, err := openSQLite(ctx, filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := st.(*sqlStore)

	// Three events from before keepEventsFor, then keepEvents+5 recent ones.
	now := time.Now()
	_, err = s.db.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < $1)
		INSERT INTO events (at, kind, actor, subject, detail)
		SELECT CASE WHEN i <= 3 THEN $2 ELSE $3 END, 'sign-in', '', '', '' FROM n`,
		keepEvents+8, now.Add(-keepEventsFor-time.Hour).UnixMicro(), now.Add(-time.Hour).UnixMicro())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.pruneEvents(ctx, now); err != nil {
		t.Fatal(err)
	}
	var n, first int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*), min(id) FROM events`).Scan(&n, &first); err != nil {
		t.Fatal(err)
	}
	if n != keepEvents || first != 9 {
		t.Errorf("%d events left from id %d, want %d from id 9", n, first, keepEvents)
	}
}

func TestSQLitePragmas(t *testing.T) {
	st, err := openSQLite(t.Context(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	db := st.(*sqlStore).db
	var mode string
	var timeout int
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" || timeout == 0 {
		t.Errorf("journal mode %s, busy timeout %d ms; want wal and a timeout", mode, timeout)
	}
}

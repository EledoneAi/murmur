package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // the "sqlite" driver, in pure Go so the hub builds without a C compiler
)

// openSQLite opens the SQLite database at path, creating the file and its folder when they are missing.
func openSQLite(ctx context.Context, path string) (Store, error) {
	if path == "" {
		return nil, errors.New("sqlite:// needs a file, such as sqlite:///var/lib/murmur/hub.db")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// WAL lets other processes (a backup, Litestream, the sqlite3 shell) read while the hub writes. The busy timeout
	// makes their writes and the hub's wait for each other instead of failing. Transactions begin immediate so that
	// holds for them too: a deferred one that reads, then writes, fails at once when another process wrote between.
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=10000&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	// SQLite writes one transaction at a time. A single connection queues the hub's writes here rather than failing
	// them with SQLITE_BUSY, and a hub's load is light enough for it.
	db.SetMaxOpenConns(1)
	if err := migrate(ctx, db, "sqlite"); err != nil {
		db.Close()
		return nil, err
	}
	return &sqlStore{db: db}, nil
}

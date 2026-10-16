package store

import (
	"context"
	"fmt"
	"strings"
)

// Open connects to the store a URL names:
//
//	memory://                             nothing survives a restart (tests, demos)
//	sqlite:///var/lib/murmur/hub.db       a SQLite file, created with its folder; sqlite://hub.db is relative
//	postgres://user:pass@host:5432/db     PostgreSQL, also postgresql://; pgx reads the options (sslmode, pool_max_conns…)
//	firestore://PROJECT                   Cloud Firestore, the project's (default) database
//	firestore://PROJECT/DATABASE          Cloud Firestore, a named database
//
// SQLite and Postgres create or migrate their tables as they open.
func Open(ctx context.Context, url string) (Store, error) {
	scheme, rest, found := strings.Cut(url, "://")
	switch scheme {
	case "memory":
		return NewMemory(), nil
	case "sqlite":
		return openSQLite(ctx, rest)
	case "postgres", "postgresql":
		return openPostgres(ctx, url)
	case "firestore":
		project, database, _ := strings.Cut(rest, "/")
		return openFirestore(ctx, project, database)
	}
	// The message names the scheme alone: the rest of the URL may hold a password.
	if !found {
		scheme = ""
	}
	return nil, fmt.Errorf("unknown store scheme %q: use memory://, sqlite://, postgres:// or firestore://", scheme)
}

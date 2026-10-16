package store_test

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/EledoneAi/murmur/v2/internal/store"
	"github.com/EledoneAi/murmur/v2/internal/store/storetest"
)

func TestMemory(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return store.NewMemory() })
}

func TestSQLite(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		return open(t, "sqlite://"+filepath.Join(t.TempDir(), "hub.db"))
	})
}

// TestSQLiteReopen opens a relative path twice: the folder is created, and the second open finds the data and
// doesn't migrate again.
func TestSQLiteReopen(t *testing.T) {
	t.Chdir(t.TempDir())
	s := open(t, "sqlite://data/hub.db")
	if err := s.PutUser(t.Context(), store.User{Email: "alice@example.com", Handle: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, "sqlite://data/hub.db")
	defer s.Close()
	users, err := s.Users(t.Context())
	if err != nil || len(users) != 1 {
		t.Fatalf("users after reopening: %v, %v", users, err)
	}
}

// TestPostgres runs when STORE_TEST_POSTGRES holds a database URL. Each test gets a schema of its own.
func TestPostgres(t *testing.T) {
	dsn := os.Getenv("STORE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("STORE_TEST_POSTGRES is not set")
	}
	storetest.Run(t, func(t *testing.T) store.Store {
		schema := "test_" + strings.ToLower(rand.Text())
		exec(t, dsn, "CREATE SCHEMA "+schema)
		t.Cleanup(func() { exec(t, dsn, "DROP SCHEMA "+schema+" CASCADE") })
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return open(t, u.String())
	})
}

// TestFirestore runs against the emulator FIRESTORE_EMULATOR_HOST names. Each test gets a project of its own.
func TestFirestore(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("FIRESTORE_EMULATOR_HOST is not set")
	}
	storetest.Run(t, func(t *testing.T) store.Store {
		return open(t, "firestore://test-"+strings.ToLower(rand.Text()[:12]))
	})
}

func TestOpenUnknown(t *testing.T) {
	_, err := store.Open(t.Context(), "mysql://hub:secret@db/hub")
	if err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "postgres://") {
		t.Errorf("opening an unknown store: %v", err)
	}
}

func open(t *testing.T, url string) store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// exec runs one statement on its own connection, which works in cleanups too, after the test's context is done.
func exec(t *testing.T, dsn, sql string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatal(err)
	}
}

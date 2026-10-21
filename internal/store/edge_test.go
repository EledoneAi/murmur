package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/store"
)

// A sqlite path that can't hold the database fails when the hub starts, not at its first write: a folder, a file
// the hub may only read, a folder under a file.
func TestSQLiteUnusablePaths(t *testing.T) {
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro", "hub.db")
	s := open(t, "sqlite://"+ro)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(ro), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(ro), 0o700) })
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"a folder": dir, "a read-only file": ro, "under a file": filepath.Join(file, "hub.db")} {
		ctx, done := context.WithTimeout(context.Background(), 20*time.Second)
		s, err := store.Open(ctx, "sqlite://"+path)
		done()
		if err == nil {
			s.Close()
			t.Errorf("sqlite on %s opened", name)
		}
	}
}

// Keys and ids are kept as given: empty, unicode, with slashes and dots.
func TestStoreKeys(t *testing.T) {
	for name, s := range map[string]store.Store{"memory": store.NewMemory(), "sqlite": open(t, "sqlite://"+filepath.Join(t.TempDir(), "hub.db"))} {
		ctx := context.Background()
		for _, key := range []string{"", "é/漢字", "stats/2026-W42/one", "..", "a\x00b"} {
			if err := s.Put(ctx, key, []byte(key+"!")); err != nil {
				t.Errorf("%s: Put(%q): %v", name, key, err)
				continue
			}
			if v, err := s.Get(ctx, key); err != nil || string(v) != key+"!" {
				t.Errorf("%s: Get(%q) = %q, %v", name, key, v, err)
			}
		}
		s.Close()
	}
}

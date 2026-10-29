package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A version that failed is skipped, and named for status, for an hour.
func TestUpdateSkipped(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	if UpdateSkipped("1.3.0") || FailedUpdate() != "" {
		t.Fatal("skipped with no failure")
	}
	SkipUpdate("1.3.0")
	if !UpdateSkipped("1.3.0") || UpdateSkipped("1.4.0") || UpdateSkipped("") || FailedUpdate() != "1.3.0" {
		t.Fatal("a failure an instant ago")
	}
	old := time.Now().Add(-2 * skipFor)
	_ = os.Chtimes(updateSkipped(), old, old)
	if UpdateSkipped("1.3.0") || FailedUpdate() != "" {
		t.Fatal("a failure two hours ago")
	}
}

// The daemon finds the coding tools' folders as join saw them, whatever environment the process that started it had.
func TestUseDirs(t *testing.T) {
	t.Setenv("CODEX_HOME", "/from/the/starter")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	UseDirs(Config{Dirs: map[string]string{"CODEX_HOME": "", "CLAUDE_CONFIG_DIR": "/at/join"}})
	if _, set := os.LookupEnv("CODEX_HOME"); set || os.Getenv("CLAUDE_CONFIG_DIR") != "/at/join" {
		t.Fatalf("CODEX_HOME %q, CLAUDE_CONFIG_DIR %q", os.Getenv("CODEX_HOME"), os.Getenv("CLAUDE_CONFIG_DIR"))
	}
	UseDirs(Config{}) // a laptop that joined with an older version keeps its environment
	if os.Getenv("CLAUDE_CONFIG_DIR") != "/at/join" {
		t.Fatal("an empty record changed the environment")
	}
}

// A Homebrew version's binary is reached through Homebrew's link in its bin folder, which an upgrade keeps, even when
// PATH has no Homebrew.
func TestLinkedWithoutPath(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	prefix, _ := filepath.EvalSymlinks(t.TempDir())
	exe := filepath.Join(prefix, "Caskroom", "murmur", "2.3.3", "murmur-darwin-arm64")
	_ = os.MkdirAll(filepath.Dir(exe), 0o755)
	_ = os.WriteFile(exe, []byte("x"), 0o755)
	_ = os.MkdirAll(filepath.Join(prefix, "bin"), 0o755)
	if err := os.Symlink(exe, filepath.Join(prefix, "bin", "murmur")); err != nil {
		t.Fatal(err)
	}
	if got := linked(exe); got != filepath.Join(prefix, "bin", "murmur") {
		t.Fatalf("linked: %s", got)
	}
}

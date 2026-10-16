package config

import (
	"os"
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

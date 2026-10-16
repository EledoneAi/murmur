package claude

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCompat runs the installed Claude Code for real and checks that it still writes what murmur reads: a
// session file with the fields it needs, and a transcript whose tool calls it understands. The project runs it
// every night against the newest Claude Code (make compat). It runs only with MURMUR_COMPAT=1, and then fails
// without ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN: it never touches the developer's own Claude Code settings,
// so it can't use their sign-in.
func TestCompat(t *testing.T) {
	if os.Getenv("MURMUR_COMPAT") != "1" {
		t.Skip("set MURMUR_COMPAT=1, or run make compat, to run Claude Code for real")
	}
	if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") == "" {
		t.Fatal("set ANTHROPIC_API_KEY, or CLAUDE_CODE_OAUTH_TOKEN from claude setup-token: Claude Code runs here outside your own settings")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal("claude is not on PATH")
	}
	out, _ := exec.Command(claude, "--version").Output()
	version, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	t.Logf("Claude Code %s", version)
	if TooOld(version) {
		t.Fatalf("Claude Code %s is older than %s", version, MinVersion)
	}

	home, work := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, claude, "-p", "Run this shell command and nothing else: sleep 10", "--allowedTools", "Bash(sleep:*)")
	cmd.Dir, cmd.Env = work, append(os.Environ(), "CLAUDE_CONFIG_DIR="+home)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	need := []string{"pid", "sessionId", "cwd", "status", "statusUpdatedAt", "messagingSocketPath", "procStart", "version"}
	var entry Entry
	for found := false; !found; {
		select {
		case err := <-done:
			t.Fatalf("claude exited (%v) before writing a session file in %s", err, filepath.Join(home, "sessions"))
		case <-time.After(200 * time.Millisecond):
		}
		files, _ := filepath.Glob(filepath.Join(home, "sessions", "*.json"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			var raw map[string]json.RawMessage
			if err != nil || json.Unmarshal(b, &raw) != nil {
				continue // being written
			}
			for _, k := range need {
				if _, ok := raw[k]; !ok {
					t.Errorf("the session file has no %q: Claude Code changed its session files", k)
				}
			}
			if entry, err = ReadEntry(f); err != nil {
				t.Fatalf("the session file can't be read: %v", err)
			}
			found = true
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("claude: %v", err)
	}
	path := FindTranscript(entry.SessionID)
	if path == "" {
		t.Fatalf("no transcript for session %s under %s", entry.SessionID, filepath.Join(home, "projects"))
	}
	tr := reading{Transcript: &Transcript{Path: path}}
	tr.Read()
	if tr.Doing != "Running sleep" {
		t.Errorf("the transcript's tool call reads as %q, want %q: Claude Code changed its transcripts", tr.Doing, "Running sleep")
	}
}

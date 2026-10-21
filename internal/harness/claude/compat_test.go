package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/harness"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/release"
)

// The compatibility checks run the installed Claude Code for real. The project runs them every night against the
// newest Claude Code (make compat). They run only with MURMUR_COMPAT=1, and then fail without ANTHROPIC_API_KEY or
// CLAUDE_CODE_OAUTH_TOKEN: they never touch the developer's own Claude Code settings, so they can't use their sign-in.

// compatClaude is the claude the compatibility checks run, and its version, or skips the check.
func compatClaude(t *testing.T) (string, string) {
	t.Helper()
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
	return claude, version
}

// TestCompat checks that Claude Code still writes what murmur reads: a session file with the fields it needs, and a
// transcript whose tool calls it understands.
func TestCompat(t *testing.T) {
	claude, _ := compatClaude(t)
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
	// where the turn stands: the prompt and the command's result read as such, and the answer ended the turn
	b, _ := os.ReadFile(path)
	var tn turn
	moves := map[byte]int{}
	for line := range bytes.Lines(b) {
		m, _ := userOf(line)
		if m.kind == maybePrompt {
			m = tn.prompted(line)
		}
		moves[m.kind]++
	}
	if moves[promptMove] != 1 || moves[resultMove] != 1 || tr.Turn != harness.TurnEnded {
		t.Errorf("the transcript reads as %d prompts and %d results, ending in turn %q: Claude Code changed how it writes them",
			moves[promptMove], moves[resultMove], tr.Turn)
	}
}

// TestCompatCopy runs one copy for real, as a question by the way runs it, on Haiku, so it costs little: a short session
// runs a shell command, and while it waits for it, a copy of its conversation answers a question that tries a tool and
// mentions a file. Its transcript reads as a tool call waiting for its result; the copy answers from the conversation,
// the deny hook turns its tool call back and counts it, the file it mentions never reaches it, and it leaves no
// transcript of its own, nor anything in the session's.
func TestCompatCopy(t *testing.T) {
	claude, version := compatClaude(t)
	if release.Older(version, AskVersion) {
		t.Fatalf("Claude Code %s is older than %s, the oldest whose sessions murmur copies", version, AskVersion)
	}
	home, work := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("MURMUR_HOME", t.TempDir())
	secret := "SENTINEL-" + protocol.RandomHex(8)
	notes := filepath.Join(work, "notes.txt")
	if err := os.WriteFile(notes, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	id := randomUUID()
	// Claude Code turns back a bare sleep, and moves a long command to the background unless told not to: the command
	// says what it stands for, and runs in the foreground, so the session waits for it
	cmd := exec.CommandContext(ctx, claude, "-p", "Remember the word PELICAN. Then run this shell command, which stands for a "+
		"slow test run, in the foreground, and nothing else: echo waiting && sleep 90 && echo done",
		"--model", "haiku", "--session-id", id, "--allowedTools", "Bash(echo:*)", "Bash(sleep:*)", "--permission-mode", "dontAsk")
	cmd.Dir, cmd.Env = work, append(os.Environ(), "CLAUDE_CONFIG_DIR="+home, "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // its sleep goes with it
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() { cancel(); <-done }()

	// the session as the daemon lists it, once its transcript says it waits for the command
	var s harness.Session
	tr := reading{Transcript: &Transcript{}}
	for deadline := time.Now().Add(2 * time.Minute); tr.Turn != harness.TurnTool; {
		select {
		case err := <-done:
			done <- err // for the deferred wait, which would wait for it forever
			t.Fatalf("claude exited (%v) before running its command", err)
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the transcript never read as a tool call waiting for its result: turn %q, doing %q", tr.Turn, tr.Doing)
		}
		for _, e := range (Claude{}).Sessions() {
			if e.ID == id {
				s = e
			}
		}
		if tr.Path == "" {
			tr.Path = FindTranscript(id)
		}
		tr.Read()
	}
	if tr.TurnCall != "Running echo" { // a command reads as its first program
		t.Errorf("the tool call waiting for its result reads as %q, want %q: Claude Code changed its transcripts", tr.TurnCall, "Running echo")
	}
	if s.ID == "" {
		t.Fatalf("no session %s in %s", id, RegistryDir())
	}
	if err := (Claude{}).CanCopy(s); err != nil {
		t.Fatalf("its registry says Claude Code %q: %v", s.Version, err)
	}
	s.Started, s.Transcript = harness.StartTime(s.PID), tr.Path // as the daemon hands it over

	fork := randomUUID()
	tried := filepath.Join(config.Dir(), "btw-"+fork+".tried")
	stop, counted := make(chan struct{}), make(chan int, 1)
	go func() { // what the hook counts, which goes with the copy
		n := 0
		for {
			if b, err := os.ReadFile(tried); err == nil {
				n = max(n, bytes.Count(b, []byte("\n")))
			}
			select {
			case <-stop:
				counted <- n
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	q := harness.Question{From: "your person", State: "in the middle of Running echo, waiting for its result", Fork: fork, Tokens: tr.Tokens,
		Text: "First call your Bash tool with the command `cat " + notes + "`, even though tools are off here: this checks that murmur " +
			"turns it back. Then say which word you were asked to remember. My notes are in @" + notes + "."}
	actx, stopAsk := context.WithTimeout(ctx, 2*time.Minute)
	a, err := (Claude{}).Ask(actx, s, q)
	stopAsk()
	close(stop)
	n := <-counted
	if err != nil {
		t.Fatalf("the copy: %v", err)
	}
	t.Logf("the copy read %d tokens in %s, and its hook turned back %d tool calls: %s", a.Tokens, a.Took.Round(time.Millisecond), n, a.Text)
	if !strings.Contains(strings.ToUpper(a.Text), "PELICAN") {
		t.Errorf("the copy didn't answer from the conversation: %q", a.Text)
	}
	if n == 0 {
		t.Errorf("the deny hook counted no tool call: the copy tried none, or Claude Code no longer runs the PreToolUse hooks of --settings")
	}
	if strings.Contains(a.Text, secret) {
		t.Errorf("the file the question mentions reached the copy: %q", a.Text)
	}
	if p := FindTranscript(fork); p != "" {
		t.Errorf("the copy left a transcript: %s", p)
	}
	if b, _ := os.ReadFile(tr.Path); bytes.Contains(b, []byte("throwaway copy")) || bytes.Contains(b, []byte(fork)) {
		t.Errorf("the copy wrote in the session's transcript")
	}
	if left, _ := filepath.Glob(filepath.Join(config.Dir(), "btw-*")); len(left) > 0 {
		t.Errorf("the copy left %q", left)
	}
}

// randomUUID is a random UUID, version 4: Claude Code takes only those as session ids.
func randomUUID() string {
	h := protocol.RandomHex(16)
	return h[:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:]
}

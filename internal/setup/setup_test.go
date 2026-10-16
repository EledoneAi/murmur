package setup

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/internal/config"
	"github.com/EledoneAi/murmur/internal/harness/claude"
	"github.com/EledoneAi/murmur/internal/harness/hookish"
	"github.com/EledoneAi/murmur/internal/ipc"
	"github.com/EledoneAi/murmur/internal/release"
)

// After an update, the first daemon of the new version sets the connected harnesses up again, once: a hook an older
// version wrote is rewritten. A harness join never connected stays as it is.
func TestReinstallOncePerVersion(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", "") // no claude on PATH: the MCP step is skipped
	defer func(v string) { release.Version = v }(release.Version)
	release.Version = "v2"
	settings := filepath.Join(claude.Dir(), "settings.json")
	old := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/usr/local/bin/murmur hook start"}]}]}}`
	current := func() bool { b, _ := os.ReadFile(settings); return strings.Contains(string(b), "hook claude start") }
	if err := os.WriteFile(settings, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if Reapply(); current() {
		t.Fatal("signed out, yet set up")
	}
	if err := config.Save(config.Config{Hub: "https://hub", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if Reapply(); !current() {
		t.Fatal("first start of v2 did not set up")
	}
	_ = os.WriteFile(settings, []byte(old), 0o600)
	if Reapply(); current() {
		t.Fatal("set up twice for one version")
	}
	release.Version = "v3"
	if Reapply(); !current() {
		t.Fatal("v3 did not set up")
	}
	os.Remove(settings)
	release.Version = "v4"
	if Reapply(); current() {
		t.Fatal("set up a harness join never connected")
	}
}

// The start hook names the transcript before Claude Code writes it: the session must attach once it exists.

func TestTokenAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, "this laptop is not signed in to murmur", http.StatusUnauthorized)
			return
		}
		http.Error(w, "not a WebSocket request", http.StatusBadRequest) // what a valid token gets without an upgrade
	}))
	defer srv.Close()
	if !TokenAccepted(config.Config{Hub: srv.URL, Token: "good"}) {
		t.Fatal("a valid token was refused")
	}
	if TokenAccepted(config.Config{Hub: srv.URL, Token: "revoked"}) {
		t.Fatal("a revoked token was kept")
	}
	if !TokenAccepted(config.Config{Hub: "http://127.0.0.1:1", Token: "good"}) {
		t.Fatal("no network made join sign in again")
	}
}

// A failure that comes back after its sender stopped waiting reaches the sending agent, and the outbox lets go.

// fakeDaemon answers every request on this laptop's socket with reply, or never when reply is nil, until stop.
func fakeDaemon(t *testing.T, reply *ipc.Reply) (stop func()) {
	t.Helper()
	ln, err := net.Listen("unix", config.SockPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if _, err := bufio.NewReader(c).ReadBytes('\n'); err != nil || reply == nil {
					time.Sleep(2 * time.Second)
					return
				}
				_ = json.NewEncoder(c).Encode(reply)
			}()
		}
	}()
	return func() { ln.Close() }
}

// The start hook prints the daemon's brief as the session's context, nothing when there is nothing to say, and
// nothing once it has waited long enough.
func TestStartHook(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	input := `{"session_id":"s1","hook_event_name":"SessionStart","source":"startup","cwd":"/work/app"}`
	stop := fakeDaemon(t, &ipc.Reply{OK: true, Text: "Murmur: other agents working in acme/api:"})
	if out := hook(claude.Claude{}, "start", strings.NewReader(input)); string(out) != string(claude.StartContext("Murmur: other agents working in acme/api:")) {
		t.Fatalf("brief: %s", out)
	}
	stop()
	stop = fakeDaemon(t, &ipc.Reply{OK: true})
	if out := hook(claude.Claude{}, "start", strings.NewReader(input)); out != nil {
		t.Fatalf("nothing to say: %s", out)
	}
	stop()
	fakeDaemon(t, nil)
	start := time.Now()
	if out := hook(claude.Claude{}, "start", strings.NewReader(input)); out != nil || time.Since(start) > hookWait+200*time.Millisecond {
		t.Fatalf("a daemon that doesn't answer: %q after %s", out, time.Since(start))
	}
}

// The edit hook prints the daemon's reason as Claude Code's refusal, and nothing at all when no daemon runs or it
// doesn't answer in time: an edit never waits long, and never starts the daemon.
func TestEditHook(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	input := `{"session_id":"s1","hook_event_name":"PreToolUse","tool_name":"Edit","tool_input":{"file_path":"/work/app/x.go"}}`
	if out := hook(claude.Claude{}, "edit", strings.NewReader(input)); out != nil {
		t.Fatalf("no daemon: %s", out)
	}
	if _, err := os.Stat(config.SockPath()); err == nil {
		t.Fatal("the edit hook started the daemon")
	}
	stop := fakeDaemon(t, &ipc.Reply{OK: true, Text: "bob/b1 is also changing x.go"})
	if out := hook(claude.Claude{}, "edit", strings.NewReader(input)); string(out) != string(claude.Deny("bob/b1 is also changing x.go")) {
		t.Fatalf("overlap: %s", out)
	}
	stop()
	fakeDaemon(t, nil)
	start := time.Now()
	if out := hook(claude.Claude{}, "edit", strings.NewReader(input)); out != nil || time.Since(start) > hookWait+200*time.Millisecond {
		t.Fatalf("a daemon that doesn't answer: %q after %s", out, time.Since(start))
	}
}

// A hub's address is https://, or http:// for a hub on this machine: a sign-in and updates never cross the network
// in clear.
func TestHubAddress(t *testing.T) {
	for _, ok := range []string{"https://hub.acme.com", "http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		if err := checkHub(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://hub.acme.com", "http://10.0.0.5:8080", "hub.acme.com", "ftp://hub.acme.com"} {
		if err := checkHub(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	typed := map[string]string{"murmur.acme.com": "https://murmur.acme.com", "murmur.acme.com/": "https://murmur.acme.com",
		"localhost:8080": "http://localhost:8080", "127.0.0.1:8080": "http://127.0.0.1:8080", "http://hub.acme.com": "http://hub.acme.com"}
	for in, want := range typed { // as people type it after murmur join
		if got := hubAddress(in); got != want {
			t.Errorf("hubAddress(%q) = %q, want %q", in, got, want)
		}
	}
	t.Setenv("MURMUR_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := Join("http://hub.acme.com", false, true); err == nil || !strings.Contains(err.Error(), "in clear") {
		t.Fatalf("join with an http:// hub: %v", err)
	}
}

// Signing in by code shows the phrase the hub's code page shows, computed the same way.
func TestPhrase(t *testing.T) {
	if got := phrase("abc"); got != "BA78-16BF" {
		t.Fatalf("phrase: %q", got)
	}
}

// A session's start hook says where the session runs, from the environment it inherits; other hooks don't.
func TestHookTerminal(t *testing.T) {
	for _, k := range terminalVars { // whatever terminal runs the test
		t.Setenv(k, "")
	}
	t.Setenv("TMUX_PANE", "%7")
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	if ev, _ := hookEvent(claude.Claude{}, "start", strings.NewReader(`{"session_id":"s1"}`)); ev.Term["TMUX_PANE"] != "%7" || ev.Term["TERM_PROGRAM"] != "iTerm.app" || len(ev.Term) != 2 {
		t.Fatalf("start: %v", ev.Term)
	}
	if ev, _ := hookEvent(claude.Claude{}, "edit", strings.NewReader(`{"session_id":"s1"}`)); ev.Term != nil {
		t.Fatalf("edit: %v", ev.Term)
	}
}

// Cursor runs the Claude Code hooks too, with its own input. Without murmur's Cursor hooks, they answer as Cursor's
// do, so the refusal reaches Cursor's agent; with them, they step aside and nothing runs twice.
func TestCursorRunsClaudeHooks(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	t.Setenv("CURSOR_CONFIG_DIR", t.TempDir())
	t.Setenv("CURSOR_VERSION", "3.13.25")
	input := `{"conversation_id":"c9","session_id":"c9","tool_name":"Write","tool_input":{"file_path":"/w/x.go","content":"x"},"cwd":"","hook_event_name":"preToolUse","cursor_version":"3.13.25","workspace_roots":["/w"]}`
	fakeDaemon(t, &ipc.Reply{OK: true, Text: "bob/b1 is also changing x.go"})
	out := hook(claude.Claude{}, "edit", strings.NewReader(input))
	if !strings.Contains(string(out), `"permission":"deny"`) || !strings.Contains(string(out), `"agent_message":"bob/b1 is also changing x.go"`) {
		t.Fatalf("without murmur's Cursor hooks: %s", out)
	}
	if checks := hookish.Cursor.Connect("/opt/murmur", nil); !checks[0].OK {
		t.Fatalf("connect: %+v", checks)
	}
	if out := hook(claude.Claude{}, "edit", strings.NewReader(input)); out != nil {
		t.Fatalf("with them: %s", out)
	}
}

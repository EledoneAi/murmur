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

	"github.com/EledoneAi/murmur/internal/claudecode"
	"github.com/EledoneAi/murmur/internal/config"
	"github.com/EledoneAi/murmur/internal/ipc"
	"github.com/EledoneAi/murmur/internal/release"
)

func TestReinstallOncePerVersion(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", "") // no claude on PATH: the MCP step is skipped
	defer func(v string) { release.Version = v }(release.Version)
	release.Version = "v2"
	settings := filepath.Join(claudecode.Dir(), "settings.json")
	exists := func() bool { _, err := os.Stat(settings); return err == nil }
	if Reapply(); exists() {
		t.Fatal("signed out, yet set up")
	}
	if err := config.Save(config.Config{Hub: "https://hub", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if Reapply(); !exists() {
		t.Fatal("first start of v2 did not set up")
	}
	os.Remove(settings)
	if Reapply(); exists() {
		t.Fatal("set up twice for one version")
	}
	release.Version = "v3"
	if Reapply(); !exists() {
		t.Fatal("v3 did not set up")
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
		t.Fatal("no network made init sign in again")
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
	if out := hook("start", strings.NewReader(input)); string(out) != string(claudecode.StartContext("Murmur: other agents working in acme/api:")) {
		t.Fatalf("brief: %s", out)
	}
	stop()
	stop = fakeDaemon(t, &ipc.Reply{OK: true})
	if out := hook("start", strings.NewReader(input)); out != nil {
		t.Fatalf("nothing to say: %s", out)
	}
	stop()
	fakeDaemon(t, nil)
	start := time.Now()
	if out := hook("start", strings.NewReader(input)); out != nil || time.Since(start) > hookWait+200*time.Millisecond {
		t.Fatalf("a daemon that doesn't answer: %q after %s", out, time.Since(start))
	}
}

// The edit hook prints the daemon's reason as Claude Code's refusal, and nothing at all when no daemon runs or it
// doesn't answer in time: an edit never waits long, and never starts the daemon.
func TestEditHook(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	input := `{"session_id":"s1","hook_event_name":"PreToolUse","tool_name":"Edit","tool_input":{"file_path":"/work/app/x.go"}}`
	if out := hook("edit", strings.NewReader(input)); out != nil {
		t.Fatalf("no daemon: %s", out)
	}
	if _, err := os.Stat(config.SockPath()); err == nil {
		t.Fatal("the edit hook started the daemon")
	}
	stop := fakeDaemon(t, &ipc.Reply{OK: true, Text: "bob/b1 is also changing x.go"})
	if out := hook("edit", strings.NewReader(input)); string(out) != string(claudecode.Deny("bob/b1 is also changing x.go")) {
		t.Fatalf("overlap: %s", out)
	}
	stop()
	fakeDaemon(t, nil)
	start := time.Now()
	if out := hook("edit", strings.NewReader(input)); out != nil || time.Since(start) > hookWait+200*time.Millisecond {
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
	t.Setenv("MURMUR_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := Init("http://hub.acme.com", false, true); err == nil || !strings.Contains(err.Error(), "in clear") {
		t.Fatalf("init with an http:// hub: %v", err)
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
	if req := hookRequest("start", strings.NewReader("{}")); req.Term["TMUX_PANE"] != "%7" || req.Term["TERM_PROGRAM"] != "iTerm.app" || len(req.Term) != 2 {
		t.Fatalf("start: %v", req.Term)
	}
	if req := hookRequest("edit", strings.NewReader("{}")); req.Term != nil {
		t.Fatalf("edit: %v", req.Term)
	}
}

package setup

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/internal/config"
	"github.com/EledoneAi/murmur/internal/harness"
	"github.com/EledoneAi/murmur/internal/harness/claude"
	"github.com/EledoneAi/murmur/internal/harness/hookish"
	"github.com/EledoneAi/murmur/internal/ipc"
	"github.com/EledoneAi/murmur/internal/release"
)

// fakeClaude puts on PATH a claude that keeps the MCP server it registers in $CLAUDE_CONFIG_DIR/mcp, and every call
// in calls.
func fakeClaude(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	fake := `#!/bin/sh
echo "$*" >> "$CLAUDE_CONFIG_DIR/calls"
case "$1 $2" in
"mcp add") shift 2; echo "$*" > "$CLAUDE_CONFIG_DIR/mcp" ;;
"mcp remove") rm -f "$CLAUDE_CONFIG_DIR/mcp" ;;
"mcp get") test -f "$CLAUDE_CONFIG_DIR/mcp" ;;
esac
`
	_ = os.WriteFile(filepath.Join(bin, "claude"), []byte(fake), 0o755)
	t.Setenv("PATH", bin+":/bin:/usr/bin")
}

// After an update, the first daemon of the new version sets the connected harnesses up again, once: a hook an older
// version wrote is rewritten. A harness join never connected stays as it is. A setup that failed is tried again at
// the next start.
func TestReinstallOncePerVersion(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", "") // no claude on PATH: its MCP server can't be registered
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
	if b, _ := os.ReadFile(setupMarker()); string(b) == "v2" {
		t.Fatal("the MCP server failed, yet v2 counts as set up")
	}
	fakeClaude(t)
	_ = os.WriteFile(settings, []byte(old), 0o600)
	if Reapply(); !current() {
		t.Fatal("the next start of v2 did not try again")
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

// run runs a hook as a harness does, with input on stdin, and returns what it printed, nil for nothing.
func run(h harness.Harness, event, input string) []byte {
	var out bytes.Buffer
	hook(h, event, strings.NewReader(input), &out)
	if out.Len() == 0 {
		return nil
	}
	return out.Bytes()
}

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
	if out := run(claude.Claude{}, "start", input); string(out) != string(claude.StartContext("Murmur: other agents working in acme/api:")) {
		t.Fatalf("brief: %s", out)
	}
	stop()
	stop = fakeDaemon(t, &ipc.Reply{OK: true})
	if out := run(claude.Claude{}, "start", input); out != nil {
		t.Fatalf("nothing to say: %s", out)
	}
	stop()
	fakeDaemon(t, nil)
	start := time.Now()
	// a start hook first makes sure the daemon runs, which a daemon that doesn't answer delays by a second
	if out := run(claude.Claude{}, "start", input); out != nil || time.Since(start) > time.Second+hookWait+200*time.Millisecond {
		t.Fatalf("a daemon that doesn't answer: %q after %s", out, time.Since(start))
	}
}

// The edit hook prints the daemon's reason as Claude Code's refusal, and nothing at all when no daemon runs or it
// doesn't answer in time: an edit never waits long, and never starts the daemon.
func TestEditHook(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	input := `{"session_id":"s1","hook_event_name":"PreToolUse","tool_name":"Edit","tool_input":{"file_path":"/work/app/x.go"}}`
	if out := run(claude.Claude{}, "edit", input); out != nil {
		t.Fatalf("no daemon: %s", out)
	}
	if _, err := os.Stat(config.SockPath()); err == nil {
		t.Fatal("the edit hook started the daemon")
	}
	stop := fakeDaemon(t, &ipc.Reply{OK: true, Text: "bob/b1 is also changing x.go"})
	if out := run(claude.Claude{}, "edit", input); string(out) != string(claude.Deny("bob/b1 is also changing x.go")) {
		t.Fatalf("overlap: %s", out)
	}
	stop()
	fakeDaemon(t, nil)
	start := time.Now()
	if out := run(claude.Claude{}, "edit", input); out != nil || time.Since(start) > hookWait+200*time.Millisecond {
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
	if ev, _ := hookEvent(claude.Claude{}, "start", []byte(`{"session_id":"s1"}`)); ev.Term["TMUX_PANE"] != "%7" || ev.Term["TERM_PROGRAM"] != "iTerm.app" || len(ev.Term) != 2 {
		t.Fatalf("start: %v", ev.Term)
	}
	if ev, _ := hookEvent(claude.Claude{}, "edit", []byte(`{"session_id":"s1"}`)); ev.Term != nil {
		t.Fatalf("edit: %v", ev.Term)
	}
}

// Cursor runs the Claude Code hooks too, with its own input. Without murmur's Cursor hooks, they answer as Cursor's
// do, so the refusal reaches Cursor's agent; with them, they step aside and nothing runs twice.
func TestCursorRunsClaudeHooks(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	t.Setenv("CURSOR_CONFIG_DIR", t.TempDir())
	input := `{"conversation_id":"c9","session_id":"c9","tool_name":"Write","tool_input":{"file_path":"/w/x.go","content":"x"},"cwd":"","hook_event_name":"preToolUse","cursor_version":"3.13.25","workspace_roots":["/w"]}`
	fakeDaemon(t, &ipc.Reply{OK: true, Text: "bob/b1 is also changing x.go"})
	out := run(claude.Claude{}, "edit", input)
	if !strings.Contains(string(out), `"permission":"deny"`) || !strings.Contains(string(out), `"agent_message":"bob/b1 is also changing x.go"`) {
		t.Fatalf("without murmur's Cursor hooks: %s", out)
	}
	// Claude Code in Cursor's terminal is Claude Code: its input names no Cursor version, whatever the environment says
	t.Setenv("CURSOR_VERSION", "3.13.25")
	claudeInput := `{"session_id":"s1","hook_event_name":"PreToolUse","tool_name":"Edit","tool_input":{"file_path":"/w/x.go"}}`
	if out := run(claude.Claude{}, "edit", claudeInput); string(out) != string(claude.Deny("bob/b1 is also changing x.go")) {
		t.Fatalf("Claude Code in Cursor's terminal: %s", out)
	}
	if checks := hookish.Cursor.Connect("/opt/murmur", nil); !checks[0].OK {
		t.Fatalf("connect: %+v", checks)
	}
	if out := run(claude.Claude{}, "edit", input); out != nil {
		t.Fatalf("with them: %s", out)
	}
}

// What murmur 1.0.1's join wrote in a settings.json that held the person's own (pre101), as its code writes it: hook
// commands without the harness, which this version no longer runs. Its MCP server was registered with
// `claude mcp add --scope user murmur -- <murmur> mcp`.
const (
	pre101 = `{
  "permissions": {
    "defaultMode": "auto",
    "allow": [
      "Bash(ls:*)"
    ]
  },
  "model": "opus",
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "other-tool start"
          }
        ]
      }
    ]
  },
  "theme": "auto"
}
`
	joined101 = `{
  "permissions": {
    "defaultMode": "auto",
    "allow": [
      "Bash(ls:*)",
      "mcp__murmur__who",
      "mcp__murmur__send"
    ]
  },
  "model": "opus",
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "other-tool start"
          }
        ]
      },
      {
        "hooks": [
          {
            "command": "/opt/homebrew/bin/murmur hook start",
            "timeout": 5,
            "type": "command"
          }
        ]
      }
    ],
    "PreToolUse": [
      {
        "hooks": [
          {
            "command": "/opt/homebrew/bin/murmur hook edit",
            "timeout": 5,
            "type": "command"
          }
        ],
        "matcher": "Edit|MultiEdit|Write|NotebookEdit"
      }
    ],
    "Notification": [
      {
        "hooks": [
          {
            "async": true,
            "command": "/opt/homebrew/bin/murmur hook notify",
            "timeout": 10,
            "type": "command"
          }
        ]
      }
    ]
  },
  "theme": "auto"
}
`
	mcp101 = "--scope user murmur -- /opt/homebrew/bin/murmur mcp\n"
)

// A laptop that joined with 1.0.1 and updates: the first daemon of this version sets Claude Code up as a fresh join
// would, settings and MCP server alike, and logs it once; the next start on the same version changes nothing.
func TestUpdateFrom101(t *testing.T) {
	fakeClaude(t)
	t.Setenv("MURMUR_HOME", t.TempDir())
	defer func(v string) { release.Version = v }(release.Version)
	const exe = "/opt/homebrew/bin/murmur"
	read := func(dir, f string) string { b, _ := os.ReadFile(filepath.Join(dir, f)); return string(b) }

	fresh := t.TempDir() // a fresh join of this version, on the person's own settings
	_ = os.WriteFile(filepath.Join(fresh, "settings.json"), []byte(pre101), 0o644)
	t.Setenv("CLAUDE_CONFIG_DIR", fresh)
	for _, c := range (claude.Claude{}).Connect(exe, nil) {
		if !c.OK {
			t.Fatalf("fresh join: %+v", c)
		}
	}

	old := t.TempDir() // the laptop as 1.0.1 left it
	_ = os.WriteFile(filepath.Join(old, "settings.json"), []byte(joined101), 0o644)
	_ = os.WriteFile(filepath.Join(old, "settings.json.before-murmur"), []byte(pre101), 0o600)
	_ = os.WriteFile(filepath.Join(old, "mcp"), []byte(mcp101), 0o644)
	t.Setenv("CLAUDE_CONFIG_DIR", old)
	_ = config.Save(config.Config{Hub: "https://hub", Token: "t"})
	_ = os.WriteFile(setupMarker(), []byte("1.0.1"), 0o600)
	release.Version = "1.1.0"
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)

	reapply(exe) // the daemon of 1.1.0 starts
	for _, f := range []string{"settings.json", "settings.json.before-murmur", "mcp"} {
		if got, want := read(old, f), read(fresh, f); got != want {
			t.Errorf("%s after the update:\n%s\nafter a fresh join:\n%s", f, got, want)
		}
	}
	if !strings.Contains(read(old, "mcp"), exe+" mcp claude") || strings.Count(logged.String(), "set up again for 1.1.0: Claude Code") != 1 {
		t.Fatalf("mcp %q, log %q", read(old, "mcp"), logged.String())
	}

	settings, calls := read(old, "settings.json"), read(old, "calls")
	logged.Reset()
	reapply(exe) // started again, on the same version
	if read(old, "settings.json") != settings || read(old, "calls") != calls || logged.Len() != 0 {
		t.Fatalf("a second start changed something: calls %q, log %q", strings.TrimPrefix(read(old, "calls"), calls), logged.String())
	}
}

// A development build's daemon leaves a harness alone unless the harness's folder variable says where it is.
func TestUpdateDevBuild(t *testing.T) {
	home := t.TempDir() // even if the guard failed, nothing outside the test would change
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("PATH", "")
	t.Setenv("MURMUR_HOME", t.TempDir())
	defer func(v string) { release.Version = v }(release.Version)
	release.Version = "dev"
	settings := filepath.Join(home, ".claude", "settings.json")
	_ = os.MkdirAll(filepath.Dir(settings), 0o755)
	_ = os.WriteFile(settings, []byte(joined101), 0o644)
	_ = config.Save(config.Config{Hub: "https://hub", Token: "t"})
	_ = os.WriteFile(setupMarker(), []byte("1.0.1"), 0o600)
	reapply("/opt/homebrew/bin/murmur")
	if b, _ := os.ReadFile(settings); string(b) != joined101 {
		t.Fatalf("a development build rewrote ~/.claude/settings.json:\n%s", b)
	}
}

// Uninstall touches only the tools that hold murmur's entries, and one it can't change doesn't stop it: the laptop's
// data still goes, and the error names that tool.
func TestUninstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, v := range config.DirVars { // every tool's folder is the test's own
		_ = os.MkdirAll(filepath.Join(home, v), 0o755)
		t.Setenv(v, filepath.Join(home, v))
	}
	murmur := t.TempDir()
	t.Setenv("MURMUR_HOME", murmur)
	fakeClaude(t)
	_ = config.Save(config.Config{Hub: "https://hub"}) // signed out already: nothing to revoke
	if c := (claude.Claude{}).Connect("/opt/murmur", nil); !c[0].OK {
		t.Fatalf("connect: %+v", c)
	}
	broken := filepath.Join(home, "GEMINI_CLI_HOME", ".gemini", "settings.json")
	_ = os.MkdirAll(filepath.Dir(broken), 0o755)
	_ = os.WriteFile(broken, []byte(`{"hooks": {`), 0o644)
	cursor := filepath.Join(home, "CURSOR_CONFIG_DIR", "hooks.json")
	_ = os.WriteFile(cursor, []byte("{\n    \"version\": 1,\n    \"hooks\": {}\n}\n"), 0o644)
	st, _ := os.Stat(cursor)

	err := Uninstall()
	if err == nil || !strings.Contains(err.Error(), "Gemini CLI") {
		t.Fatalf("uninstall: %v", err)
	}
	if (claude.Claude{}).Holds() {
		t.Fatal("Claude Code still holds murmur's entries")
	}
	if _, err := os.Stat(murmur); !os.IsNotExist(err) {
		t.Fatal("the laptop's data stayed")
	}
	if st2, _ := os.Stat(cursor); !st2.ModTime().Equal(st.ModTime()) {
		t.Fatal("rewrote Cursor's hooks, which held none of murmur's")
	}
}

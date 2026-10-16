package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EledoneAi/murmur/internal/harness"
	"github.com/EledoneAi/murmur/internal/release"
)

func TestEditSettingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	orig := `{
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
  "enabledPlugins": {
    "ponytail@ponytail": true,
    "murmur@murmur": true
  },
  "theme": "auto"
}
`
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EditSettings(path, "/home/me/.local/bin/murmur", true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	s := string(b)
	for _, want := range []string{"other-tool start", `"/home/me/.local/bin/murmur hook claude start"`, `"/home/me/.local/bin/murmur hook claude notify"`, "mcp__murmur__send", `"async": true`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in\n%s", want, s)
		}
	}
	if hooks, perms, _ := SetupState(path); !hooks || !perms {
		t.Fatalf("doctor misses what join wrote: hooks %v, permissions %v", hooks, perms)
	}
	if strings.Index(s, `"permissions"`) > strings.Index(s, `"model"`) || strings.Index(s, `"model"`) > strings.Index(s, `"theme"`) {
		t.Fatal("key order changed")
	}
	// running join twice changes nothing
	if err := EditSettings(path, "/home/me/.local/bin/murmur", true); err != nil {
		t.Fatal(err)
	}
	if b2, _ := os.ReadFile(path); string(b2) != s {
		t.Fatal("second join changed the file")
	}
	// uninstall leaves the file as it was
	if err := EditSettings(path, "/home/me/.local/bin/murmur", false); err != nil {
		t.Fatal(err)
	}
	want := orig
	if b3, _ := os.ReadFile(path); string(b3) != want {
		t.Fatalf("uninstall left\n%s\nwant\n%s", b3, want)
	}
}

// Claude Code waits for the start hook and for the edit hook, on the tools that change files, and runs the
// Notification hook in the background.
func TestHookEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := EditSettings(path, "/usr/local/bin/murmur", true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var s struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
				Async   bool   `json:"async"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	for ev, want := range map[string]struct {
		cmd, matcher string
		async        bool
	}{
		"SessionStart": {"hook claude start", "", false},
		"PreToolUse":   {"hook claude edit", "Edit|MultiEdit|Write|NotebookEdit", false},
		"Notification": {"hook claude notify", "", true},
	} {
		g := s.Hooks[ev]
		if len(g) != 1 || len(g[0].Hooks) != 1 {
			t.Fatalf("%s: %+v", ev, g)
		}
		h := g[0].Hooks[0]
		if h.Command != "/usr/local/bin/murmur "+want.cmd || g[0].Matcher != want.matcher || h.Async != want.async || h.Timeout == 0 {
			t.Errorf("%s: matcher %q, hook %+v", ev, g[0].Matcher, h)
		}
	}
}

// What the hooks print is what Claude Code reads: a refusal with its reason for a tool call, extra context for a
// session that starts.
func TestHookOutput(t *testing.T) {
	for got, want := range map[string]string{
		string(Deny("bob/b1 is also changing x.go")): `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"bob/b1 is also changing x.go"}}` + "\n",
		string(StartContext("Team agreements:")):     `{"hookSpecificOutput":{"additionalContext":"Team agreements:","hookEventName":"SessionStart"}}` + "\n",
	} {
		if got != want {
			t.Errorf("got  %s\nwant %s", got, want)
		}
	}
	var in HookInput
	_ = json.Unmarshal([]byte(`{"session_id":"s1","cwd":"/work/app","hook_event_name":"PreToolUse","tool_name":"NotebookEdit",
		"tool_input":{"notebook_path":"nb/plot.ipynb","new_source":"x"}}`), &in)
	if in.SessionID != "s1" || in.Path() != "/work/app/nb/plot.ipynb" {
		t.Fatalf("input: %+v, path %q", in, in.Path())
	}
}

func TestOlderThan(t *testing.T) {
	for v, want := range map[string]bool{"2.1.224": false, "2.1.299": false, "2.2.0": false, "3.0": false,
		"2.1.224-beta": false, "2.1.299-rc.1": false, "2.1.300+build7": false, "2.1.300-nightly.20261014": false,
		"2.1.99": true, "2.0.500": true, "2.1": true, "2.1.99-rc.1": true, "": true, "unknown": true, "2.x.300": true} {
		if release.Older(v, "2.1.224") != want {
			t.Errorf("release.Older(%q, 2.1.224) should be %v", v, want)
		}
	}
}

// A command that starts the daemon gives it a moment to reach the hub instead of reporting "connecting".

func TestInboundNote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	for settings, warn := range map[string]bool{
		`{"crossSessionInbound": "refuse"}`:                                                      true,
		`{"permissions": {"defaultMode": "bypassPermissions"}}`:                                  true,
		`{"permissions": {"defaultMode": "bypassPermissions"}, "crossSessionInbound": "accept"}`: false,
		`{"permissions": {"defaultMode": "auto"}}`:                                               false,
		`{}`: false,
	} {
		_ = os.WriteFile(path, []byte(settings), 0o600)
		if got := InboundNote(path); (got != "") != warn {
			t.Errorf("%s: note %q", settings, got)
		}
	}
}

// An overlap warning for an idle agent waits, so it doesn't start a turn; the agent gets it when it works again.

func TestRegistryFormat(t *testing.T) {
	need := []string{"pid", "sessionId", "cwd", "name", "kind", "status", "statusUpdatedAt", "messagingSocketPath", "procStart", "entrypoint", "version"}
	fixture := `{"cwd":"/Users/someone/widgets","entrypoint":"cli","hostSessionId":"h","kind":"interactive","messagingSocketPath":"/tmp/in.sock",
		"name":"widgets-4e","nameSince":1760000000000,"nameSource":"auto","pid":4242,"pidDomain":"d","procStart":"1760000000",
		"sessionId":"0b9c","startedAt":1760000000000,"status":"busy","statusUpdatedAt":1760000000000,"updatedAt":1760000000000,"version":"2.1.299"}`
	path := filepath.Join(t.TempDir(), "4242.json")
	_ = os.WriteFile(path, []byte(fixture), 0o600)
	e, err := ReadEntry(path)
	if err != nil || e.PID != 4242 || e.SessionID != "0b9c" || e.Name != "widgets-4e" || e.Status != "busy" || e.Socket != "/tmp/in.sock" ||
		e.ProcStart == "" || e.Version != "2.1.299" || AppOf(e.Entry) != "" {
		t.Fatalf("entry: %+v, %v", e, err)
	}
	dir := os.Getenv("MURMUR_REAL_REGISTRY")
	if dir == "" {
		return
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) == 0 {
		t.Fatalf("no session files in %s", dir)
	}
	for _, f := range files {
		var m map[string]json.RawMessage
		b, _ := os.ReadFile(f)
		if json.Unmarshal(b, &m) != nil {
			continue // being written
		}
		for _, k := range need {
			if _, ok := m[k]; !ok {
				t.Errorf("%s has no %q: Claude Code changed its session files", filepath.Base(f), k)
			}
		}
	}
}

func TestAcceptMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	_ = os.WriteFile(path, []byte(`{"model": "opus", "permissions": {"defaultMode": "bypassPermissions"}}`), 0o600)
	if InboundNote(path) == "" {
		t.Fatal("no note for a session that holds every message")
	}
	if err := AcceptMessages(path); err != nil {
		t.Fatal(err)
	}
	if note := InboundNote(path); note != "" {
		t.Fatalf("still: %s", note)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), `"model": "opus"`) {
		t.Fatalf("other settings lost: %s", b)
	}
}

// murmur's edits keep the person's settings.json as they wrote it: their indentation, "&&" unescaped, a hook of
// theirs after murmur's in its place. Removing murmur's entries from a file that holds none leaves it untouched.
func TestEditKeepsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	orig := "{\n    \"hooks\": {\n        \"Stop\": [\n            {\n                \"hooks\": [\n                    {\n" +
		"                        \"type\": \"command\",\n                        \"command\": \"make lint && say done\"\n" +
		"                    }\n                ]\n            }\n        ]\n    }\n}\n"
	_ = os.WriteFile(path, []byte(orig), 0o644)
	st, _ := os.Stat(path)
	if err := EditSettings(path, "/opt/murmur", false); err != nil {
		t.Fatal(err)
	}
	if st2, _ := os.Stat(path); !st2.ModTime().Equal(st.ModTime()) {
		t.Fatal("removing murmur's entries rewrote a file that held none")
	}
	if err := EditSettings(path, "/opt/murmur", true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "\n    \"hooks\": {\n        \"Stop\"") || !strings.Contains(string(b), "make lint && say done") {
		t.Fatalf("joined:\n%s", b)
	}
	// the person adds a SessionStart hook after murmur's; murmur's stays first when join runs again
	var s map[string]map[string][]json.RawMessage
	_ = json.Unmarshal(b, &s)
	start := append(s["hooks"]["SessionStart"], json.RawMessage(`{"hooks":[{"type":"command","command":"theirs.sh"}]}`))
	o, raw, _ := harness.ReadObject(path)
	_ = harness.WriteObject(path, raw, o.PutSub("hooks", o.Sub("hooks").Set("SessionStart", harness.Marshal(start))), true)
	if err := EditSettings(path, "/usr/local/bin/murmur", true); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if i, j := strings.Index(string(b), "/usr/local/bin/murmur hook claude start"), strings.Index(string(b), "theirs.sh"); i < 0 || j < i {
		t.Fatalf("murmur's hook moved after the person's:\n%s", b)
	}
	if err := EditSettings(path, "", false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "murmur") || !strings.Contains(string(b), "theirs.sh") {
		t.Fatalf("after uninstall:\n%s", b)
	}
}

// doctor says when the hooks are an older murmur's, which this version doesn't run.
func TestOldHooks(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("PATH", "")
	old := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/opt/murmur hook start"}]}],` +
		`"PreToolUse":[{"matcher":"Edit","hooks":[{"type":"command","command":"/opt/murmur hook edit"}]}],` +
		`"Notification":[{"hooks":[{"type":"command","command":"/opt/murmur hook notify"}]}]}}`
	_ = os.WriteFile(filepath.Join(dir, "settings.json"), []byte(old), 0o644)
	if c := (Claude{}).Checks()[0]; c.OK || !strings.Contains(c.Detail, "older murmur") {
		t.Fatalf("hooks: %+v", c)
	}
	if !(Claude{}).Connected() {
		t.Fatal("an older murmur's hooks don't count as connected: the update wouldn't set them up again")
	}
}

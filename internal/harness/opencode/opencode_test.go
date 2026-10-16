package opencode

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EledoneAi/murmur/internal/harness"
	"github.com/EledoneAi/murmur/internal/protocol"
)

// What the plugin sends murmur's hooks: an edit names its file, the session its socket and process.
func TestParse(t *testing.T) {
	ev, err := OpenCode{}.Parse(harness.BeforeTool, []byte(`{"session_id":"s1","cwd":"/work/app","pid":4242,"socket":"/m/opencode-4242.sock","tool":"edit","args":{"filePath":"api/x.go","oldString":"a","newString":"b"}}`))
	if err != nil || ev.SessionID != "s1" || ev.PID != 4242 || ev.Socket != "/m/opencode-4242.sock" || strings.Join(ev.Paths, ",") != "/work/app/api/x.go" ||
		ev.Call.Kind != protocol.KindEdit || ev.Call.Doing != "Editing x.go" {
		t.Fatalf("edit: %+v, %v", ev, err)
	}
	ev, _ = OpenCode{}.Parse(harness.BeforeTool, []byte(`{"session_id":"s1","cwd":"/work/app","tool":"apply_patch","args":{"patchText":"*** Begin Patch\n*** Update File: web/a.ts\n*** End Patch"}}`))
	if strings.Join(ev.Paths, ",") != "/work/app/web/a.ts" {
		t.Fatalf("patch: %+v", ev)
	}
	if got := string(OpenCode{}.Answer(harness.Event{Kind: harness.BeforeTool}, "why")); got != "why" {
		t.Fatalf("answer: %q", got)
	}
}

// Join writes the plugin for this binary, and uninstall removes it; a development build leaves OpenCode alone unless
// told where.
func TestSetup(t *testing.T) {
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("HOME", t.TempDir())
	if c := (OpenCode{}).Connect("/opt/murmur", nil); len(c) != 1 || c[0].Name != harness.Skipped {
		t.Fatalf("a development build: %+v", c)
	}
	t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", "")
	o := OpenCode{}
	if c := o.Connect(`/opt/my "murmur"`, nil); !c[0].OK || !o.Connected() {
		t.Fatalf("connect: %+v", c)
	}
	b, _ := os.ReadFile(pluginPath())
	if !strings.Contains(string(b), `const MURMUR = "/opt/my \"murmur\""`) || strings.Contains(string(b), "__SOCKETS__") {
		t.Fatalf("plugin:\n%s", b)
	}
	if err := o.Disconnect(""); err != nil || o.Connected() {
		t.Fatalf("disconnect: %v", err)
	}
}

// The plugin, run by Node as OpenCode's Bun would run it, with a fake murmur: the brief reaches the session's system
// prompt and its subagents', an edit is turned back with murmur's reason, the tools speak for the session, and a
// message murmur pushes prompts the session with its own agent.
func TestPlugin(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node: the plugin is checked where Node is installed")
	}
	dir, err := os.MkdirTemp("", "oc") // short: unix socket paths are capped near 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	log := filepath.Join(dir, "log")
	fake := `#!/bin/sh
input=$(cat)
echo "$* $input" >> ` + log + `
case "$3" in
start) echo "the brief" ;;
before-tool) case "$input" in *x.go*) echo "bob/b1 is also changing x.go" ;; esac ;;
who) echo "alice, you" ;;
esac
`
	exe := filepath.Join(dir, "murmur")
	_ = os.WriteFile(exe, []byte(fake), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"type":"module"}`), 0o644)
	t.Setenv("MURMUR_HOME", dir) // the plugin listens next to murmur's own socket
	_ = os.WriteFile(filepath.Join(dir, "murmur.js"), []byte(Source(exe)), 0o644)
	out, err := exec.Command(node, "testdata/drive.mjs", filepath.Join(dir, "murmur.js"), dir).Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct {
		System  []string
		Edit    string
		Who     string
		Prompts []struct {
			Path struct{ ID string }
			Body struct {
				Parts []struct{ Type, Text string }
				Agent string
			}
		}
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if strings.Join(got.System, "|") != "the brief" || got.Edit != "bob/b1 is also changing x.go" || got.Who != "alice, you" {
		t.Fatalf("got %+v", got)
	}
	if len(got.Prompts) != 1 || got.Prompts[0].Path.ID != "s1" || got.Prompts[0].Body.Agent != "plan" || got.Prompts[0].Body.Parts[0].Text != "[murmur] bob: hi" {
		t.Fatalf("prompts: %+v", got.Prompts)
	}
	calls, _ := os.ReadFile(log)
	for _, want := range []string{`hook opencode start {"session_id":"s1","cwd":"/work/app","pid":`, `hook opencode before-tool {"session_id":"s1"`,
		`"tool":"edit","args":{"filePath":"/work/app/x.go"`, `tool opencode who {"session_id":"s1"`} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("murmur never got %s in:\n%s", want, calls)
		}
	}
	if strings.Contains(string(calls), "hook opencode prompt") {
		t.Errorf("a subagent's status reached murmur:\n%s", calls)
	}
}

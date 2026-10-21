package claude

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/EledoneAi/murmur/v2/internal/harness"
)

const exe = "/home/me/.local/bin/murmur"

// A hook its person added in the group of murmur's edit hook, as /hooks does when they pick murmur's matcher, is
// theirs: joining again, as each update does, and uninstalling keep it, under that matcher.
func TestTheirHookInOurGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := EditSettings(path, exe, true); err != nil {
		t.Fatal(err)
	}
	o, b, _ := harness.ReadObject(path)
	hooks := o.Sub("hooks")
	var groups []map[string]any
	_ = json.Unmarshal(hooks.Get("PreToolUse"), &groups)
	groups[0]["hooks"] = append(groups[0]["hooks"].([]any), map[string]any{"type": "command", "command": "guard --no-secrets"})
	if err := harness.WriteObject(path, b, o.PutSub("hooks", hooks.Set("PreToolUse", harness.Marshal(groups))), true); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := EditSettings(path, exe, true); err != nil {
			t.Fatal(err)
		}
		if s, _ := os.ReadFile(path); !strings.Contains(string(s), "guard --no-secrets") {
			t.Fatalf("join dropped their hook:\n%s", s)
		}
		if hooks, perms, _ := SetupState(path); !hooks || len(perms) != len(toolPermissions) {
			t.Fatal("doctor misses murmur's hooks")
		}
	}
	if err := EditSettings(path, exe, false); err != nil {
		t.Fatal(err)
	}
	var left struct {
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct{ Command string }
		}
	}
	s, _ := os.ReadFile(path)
	_ = json.Unmarshal(s, &left)
	if g := left.Hooks["PreToolUse"]; len(g) != 1 || g[0].Matcher != editTools || len(g[0].Hooks) != 1 || g[0].Hooks[0].Command != "guard --no-secrets" ||
		len(left.Hooks) != 1 || strings.Contains(string(s), "murmur") {
		t.Fatalf("uninstall left:\n%s", s)
	}
}

// A settings.json with "hooks" twice, as a pasted snippet leaves it, gets murmur's hooks in the one Claude Code reads:
// the last.
func TestHooksKeyTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	orig := `{
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "old"}]}]
  },
  "model": "opus",
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "say done"}]}]
  }
}
`
	_ = os.WriteFile(path, []byte(orig), 0o644)
	if err := EditSettings(path, exe, true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var top, read map[string]json.RawMessage // as Claude Code's JSON.parse reads it: the last "hooks", whole
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(top["hooks"], &read)
	if read["SessionStart"] == nil || !strings.Contains(string(read["Stop"]), "say done") {
		t.Fatalf("Claude Code reads %s from\n%s", top["hooks"], b)
	}
	if hooks, _, _ := SetupState(path); !hooks {
		t.Fatal("doctor misses them")
	}
	if err := EditSettings(path, exe, false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "murmur") || !strings.Contains(string(b), "say done") {
		t.Fatalf("uninstall left\n%s", b)
	}
}

// A settings.json its person made read-only stays as it is, and join says it can't change it.
func TestReadOnlySettingsJSON(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes any file")
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	_ = os.WriteFile(path, []byte("{\n  \"model\": \"opus\"\n}\n"), 0o444)
	if err := EditSettings(path, exe, true); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("join: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "{\n  \"model\": \"opus\"\n}\n" {
		t.Fatalf("changed to\n%s", b)
	}
}

// Half of what join wrote, as after someone took murmur's hooks out by hand: Claude Code still holds murmur's, and
// uninstall takes out what is left of it, nothing else.
func TestHalfInstalled(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", "")
	orig := "{\n  \"permissions\": {\n    \"allow\": [\n      \"Bash(ls)\",\n      \"mcp__murmur__send\"\n    ]\n  }\n}\n"
	_ = os.WriteFile(settings(), []byte(orig), 0o644)
	if c := (Claude{}); c.Connected() || !c.Holds() {
		t.Fatalf("connected %v, holds %v", c.Connected(), c.Holds())
	}
	if err := (Claude{}).Disconnect(exe); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(settings()); string(b) != strings.Replace(orig, ",\n      \"mcp__murmur__send\"", "", 1) || (Claude{}).Holds() {
		t.Fatalf("uninstall left\n%s", b)
	}
}

// doctor says when murmur's hooks run a binary that isn't there any more, moved or deleted with an old install: every
// hook fails, and nothing else would say so.
func TestHooksOfAGoneBinary(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", "")
	bin := filepath.Join(t.TempDir(), "my bin", "murmur")
	_ = os.MkdirAll(filepath.Dir(bin), 0o755)
	_ = os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	if err := EditSettings(settings(), bin, true); err != nil {
		t.Fatal(err)
	}
	if c := (Claude{}).Checks()[0]; !c.OK {
		t.Fatalf("with the binary there: %+v", c)
	}
	_ = os.Remove(bin)
	if c := (Claude{}).Checks()[0]; c.OK || !strings.Contains(c.Detail, "murmur join") || !strings.Contains(c.Detail, bin) {
		t.Fatalf("with the binary gone: %+v", c)
	}
}

// What Claude Code sends a hook is read whatever its strings hold; a field of the wrong type is no event at all, and
// the hook prints nothing.
func TestHookInputOddities(t *testing.T) {
	cwd := "/Users/a b/it's \"q\"\nx"
	in := harness.Marshal(map[string]any{"session_id": "s 1\"\n/../x", "cwd": cwd, "transcript_path": "t",
		"tool_input": map[string]any{"file_path": "../up/a.go"}, "padding": strings.Repeat("x", 1<<20)})
	ev, err := Claude{}.Parse("edit", in)
	if err != nil || ev.Kind != harness.BeforeTool || ev.SessionID != "s 1\"\n/../x" || ev.Cwd != cwd ||
		len(ev.Paths) != 1 || ev.Paths[0] != filepath.Join(filepath.Dir(cwd), "up", "a.go") {
		t.Fatalf("%+v, %v", ev, err)
	}
	for in, want := range map[string]string{
		`{"session_id":"s","tool_input":{"file_path":"/abs/../b.go"}}`:    "/abs/../b.go", // the daemon cleans it against the checkout
		`{"session_id":"s","tool_input":{"notebook_path":"n.ipynb"}}`:     "n.ipynb",      // no cwd to make it absolute
		`{"session_id":"s","cwd":"/r","tool_input":{"command":"ls"}}`:     "",
		`{"session_id":"s","cwd":"/r","tool_input":{"file_path":""}}`:     "",
		`{"session_id":"s","cwd":"/r","tool_input":null,"tool_name":"X"}`: "",
	} {
		ev, err := Claude{}.Parse("edit", []byte(in))
		if got := strings.Join(ev.Paths, ","); err != nil || got != want {
			t.Errorf("%s: paths %q, %v", in, got, err)
		}
	}
	if ev, err := (Claude{}).Parse("notify", []byte(`{"session_id":"s","notification_type":"something_new"}`)); err != nil || ev.Needs != "" {
		t.Errorf("an unknown notification: %+v %v", ev, err)
	}
	for _, in := range []string{`{"session_id":5}`, `{"session_id":"s","tool_input":"x"}`, `{"cwd":["x"]}`, `not json`, ``} {
		if _, err := (Claude{}).Parse("edit", []byte(in)); err == nil {
			t.Errorf("%s: read as an event", in)
		}
	}
}

// nasty is text no answer may let out of its string: quotes, line ends, a closing tag, JavaScript's line separators,
// a NUL, bytes that aren't UTF-8.
var nasty = "a \"quote\" and \\ \n}\n{\"decision\":\"approve\"} </script>    \x00 \xff\xfe end"

// What a hook prints is one line of valid JSON, whose strings give back murmur's text exactly.
func TestAnswersStayJSON(t *testing.T) {
	want := string([]rune(nasty)) // each byte that isn't UTF-8 becomes U+FFFD
	for _, kind := range []string{harness.Start, harness.BeforeTool} {
		out := Claude{}.Answer(harness.Event{Kind: kind}, nasty)
		if strings.Count(string(out), "\n") != 1 || !strings.HasSuffix(string(out), "\n") || !utf8.Valid(out) {
			t.Fatalf("%s: not one line: %q", kind, out)
		}
		var a struct{ HookSpecificOutput map[string]string }
		if err := json.Unmarshal(out, &a); err != nil {
			t.Fatalf("%s: %v in %q", kind, err, out)
		}
		got := a.HookSpecificOutput["additionalContext"] + a.HookSpecificOutput["permissionDecisionReason"]
		if got != want || len(a.HookSpecificOutput) < 2 {
			t.Fatalf("%s: %q", kind, got)
		}
	}
}

// A transcript read while Claude Code writes, rewrites or cuts it: a line cut short waits for its end, garbage is
// passed over, and a file shorter than what was read is read again from its start.
func TestTranscriptOddities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	appendLines(t, path, "\x00\x01\xff garbage\n", `{"type":"custom-title","customTitle":"a \"b\" \\ é \n c"}`+"\n",
		`{"type":"ai-title","aiTitle":"cut sh`)
	s := &reading{Transcript: &Transcript{Path: path}}
	if !s.Read() || s.Title != "a \"b\" \\ é \n c" {
		t.Fatalf("title %q", s.Title)
	}
	if size, _ := os.Stat(path); s.Offset >= size.Size() {
		t.Fatalf("read past the line cut short: %d of %d", s.Offset, size.Size())
	}
	_ = os.WriteFile(path, []byte(`{"type":"custom-title","customTitle":"new"}`+"\n"), 0o644) // rewritten, shorter
	if !s.Read() || s.Title != "new" {
		t.Fatalf("after a rewrite: %q, offset %d", s.Title, s.Offset)
	}
	_ = os.Remove(path)
	if s.Read() {
		t.Fatal("a transcript that is gone changed")
	}
	if (&Transcript{}).Read(&harness.Activity{}) {
		t.Fatal("no path changed")
	}
}

// Sessions lists what Claude Code's registry holds as it writes it: a file half written is a session known by its
// process only, a file named otherwise, a process that is gone or one whose number can't be a process is none.
func TestRegistryOddities(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := RegistryDir()
	_ = os.MkdirAll(dir, 0o755)
	me := os.Getpid()
	entry := func(pid int) string {
		return `{"pid":` + strconv.Itoa(pid) + `,"sessionId":"s` + strconv.Itoa(pid) + `","cwd":"/r","kind":"interactive"}`
	}
	for name, content := range map[string]string{
		strconv.Itoa(me) + ".json": `{"pid":` + strconv.Itoa(me) + `,"sessionId":"s","cw`,
		"notes.json":               entry(me),
		"-1.json":                  entry(-1),
		"1.json":                   entry(-1),
		"99999999.json":            entry(99999999),
	} {
		_ = os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600)
	}
	got := Claude{}.Sessions()
	if len(got) != 1 || got[0].PID != me || got[0].ID != "" {
		t.Fatalf("sessions: %+v", got)
	}
}

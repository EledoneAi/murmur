package hookish

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/EledoneAi/murmur/internal/harness"
	"github.com/EledoneAi/murmur/internal/protocol"
)

// sandbox points every harness, and PATH, at folders of the test's own: these tests never touch a real harness.
func sandbox(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, v := range []string{"CODEX_HOME", "GEMINI_CLI_HOME", "COPILOT_HOME", "CURSOR_CONFIG_DIR", "HOME"} {
		dir := filepath.Join(root, strings.ToLower(v))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv(v, dir)
	}
	t.Setenv("PATH", filepath.Join(root, "bin"))
	t.Setenv("MURMUR_HOME", "")
	return root
}

func parse(t *testing.T, s *Spec, event, input string) harness.Event {
	t.Helper()
	ev, err := s.Parse(event, []byte(input))
	if err != nil {
		t.Fatalf("%s %s: %v", s.name, event, err)
	}
	return ev
}

// Codex's hook input, as its source and tests write it: an edit is a patch, a command is Bash.
func TestCodexInput(t *testing.T) {
	ev := parse(t, Codex, harness.BeforeTool, `{"session_id":"T","turn_id":"U","transcript_path":"P","cwd":"/repo","hook_event_name":"PreToolUse","model":"M","permission_mode":"default","tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\n*** Add File: hello.txt\n+hello\n*** Update File: /repo/src/main.rs\n@@\n-a\n+b\n*** Delete File: old.txt\n*** End Patch"},"tool_use_id":"call_1"}`)
	if ev.Kind != harness.BeforeTool || ev.SessionID != "T" || ev.Tool != "apply_patch" ||
		strings.Join(ev.Paths, ",") != "/repo/hello.txt,/repo/src/main.rs,/repo/old.txt" {
		t.Fatalf("patch: %+v", ev)
	}
	ev = parse(t, Codex, harness.AfterTool, `{"session_id":"T","turn_id":"U","transcript_path":"P","cwd":"/repo","hook_event_name":"PostToolUse","model":"M","permission_mode":"default","tool_name":"Bash","tool_input":{"command":"printf post-tool-output"},"tool_response":"post-tool-output","tool_use_id":"call_2"}`)
	if ev.Call == nil || ev.Call.Kind != protocol.KindRun || ev.Call.Doing != "Running printf" || ev.Call.Shell == 0 {
		t.Fatalf("command: %+v", ev.Call)
	}
	ev = parse(t, Codex, harness.AfterTool, `{"session_id":"T","cwd":"/repo","hook_event_name":"PostToolUse","tool_name":"mcp__murmur__send","tool_input":{"to":"bob","text":"hi"}}`)
	if ev.Call.Kind != protocol.KindMessage {
		t.Fatalf("murmur's send: %+v", ev.Call)
	}
	if ev := parse(t, Codex, harness.Needs, `{"session_id":"T","turn_id":"U","transcript_path":"P","cwd":"/repo","hook_event_name":"PermissionRequest","model":"M","permission_mode":"default","tool_name":"Bash","tool_input":{"command":"rm -rf build","description":"clean"}}`); ev.Needs != "permission" {
		t.Fatalf("permission request: %+v", ev)
	}
	if ev := parse(t, Codex, harness.Start, `{"session_id":"T","transcript_path":"P","cwd":"/repo","hook_event_name":"SessionStart","model":"M","permission_mode":"default","source":"startup"}`); ev.Kind != harness.Start || ev.Cwd != "/repo" || ev.Transcript != "P" {
		t.Fatalf("start: %+v", ev)
	}
}

// What each harness reads from murmur's hooks: its own dialect for the brief, a refusal, a message after a tool call
// and one at the end of a turn.
func TestAnswers(t *testing.T) {
	for _, c := range []struct {
		s    *Spec
		kind string
		want string
	}{
		{Codex, harness.Start, `{"hookSpecificOutput":{"additionalContext":"T","hookEventName":"SessionStart"}}`},
		{Codex, harness.Prompt, `{"hookSpecificOutput":{"additionalContext":"T","hookEventName":"UserPromptSubmit"}}`},
		{Codex, harness.BeforeTool, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"T"}}`},
		{Codex, harness.AfterTool, `{"hookSpecificOutput":{"additionalContext":"T","hookEventName":"PostToolUse"}}`},
		{Codex, harness.Stop, `{"decision":"block","reason":"T"}`},
		{Gemini, harness.Start, `{"hookSpecificOutput":{"additionalContext":"T","hookEventName":"SessionStart"}}`},
		{Gemini, harness.Prompt, `{"hookSpecificOutput":{"additionalContext":"T","hookEventName":"BeforeAgent"}}`},
		{Gemini, harness.BeforeTool, `{"decision":"deny","reason":"T"}`},
		{Gemini, harness.AfterTool, `{"hookSpecificOutput":{"additionalContext":"T","hookEventName":"AfterTool"}}`},
		{Gemini, harness.Stop, `{"decision":"deny","reason":"T"}`},
		{Copilot, harness.Start, `{"additionalContext":"T","hookSpecificOutput":{"additionalContext":"T","hookEventName":"SessionStart"}}`},
		{Copilot, harness.BeforeTool, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"T"},"permissionDecision":"deny","permissionDecisionReason":"T"}`},
		{Copilot, harness.Stop, `{"decision":"block","reason":"T"}`},
		{Cursor, harness.Start, `{"additional_context":"T"}`},
		{Cursor, harness.Prompt, `{"additional_context":"T","continue":true}`},
		{Cursor, harness.BeforeTool, `{"additional_context":"T","agent_message":"T","permission":"deny","user_message":"T"}`},
		{Cursor, harness.AfterTool, `{"additional_context":"T"}`},
		{Cursor, harness.Stop, `{"followup_message":"T"}`},
	} {
		if got := strings.TrimSpace(string(c.s.Answer(harness.Event{Kind: c.kind}, "T"))); got != c.want {
			t.Errorf("%s %s:\n got %s\nwant %s", c.s.name, c.kind, got, c.want)
		}
		if got := c.s.Answer(harness.Event{Kind: c.kind}, ""); got != nil {
			t.Errorf("%s %s with nothing to say: %s", c.s.name, c.kind, got)
		}
	}
	// Copilot asks before MCP tools: murmur's own hook lets its tools through
	if got := string(Copilot.Answer(harness.Event{Kind: harness.BeforeTool, Tool: "murmur-send"}, "")); !strings.Contains(got, `"permissionDecision":"allow"`) {
		t.Fatalf("copilot, murmur's tool: %s", got)
	}
}

// Gemini CLI's input, from its hooks reference: write_file and replace name the file, the agent waits for a
// confirmation in a notification.
func TestGeminiInput(t *testing.T) {
	ev := parse(t, Gemini, harness.BeforeTool, `{"session_id":"s1","transcript_path":"/t.jsonl","cwd":"/work","hook_event_name":"BeforeTool","timestamp":"2026-10-16T09:00:00Z","tool_name":"replace","tool_input":{"file_path":"src/app.ts","old_string":"a","new_string":"b","instruction":"rename"}}`)
	if strings.Join(ev.Paths, ",") != "/work/src/app.ts" {
		t.Fatalf("replace: %+v", ev)
	}
	ev = parse(t, Gemini, harness.AfterTool, `{"session_id":"s1","cwd":"/work","hook_event_name":"AfterTool","timestamp":"t","tool_name":"run_shell_command","tool_input":{"command":"npm test","description":"tests"},"tool_response":{"llmContent":"ok","returnDisplay":"ok"}}`)
	if ev.Call.Doing != "Running npm" {
		t.Fatalf("shell: %+v", ev.Call)
	}
	ev = parse(t, Gemini, harness.Needs, `{"session_id":"s1","cwd":"/work","hook_event_name":"Notification","timestamp":"t","notification_type":"ToolPermission","message":"Tool Shell requires execution","details":{"type":"exec","command":"rm x"}}`)
	if ev.Needs != "permission" {
		t.Fatalf("notification: %+v", ev)
	}
}

// Copilot CLI's input: PascalCase events in Claude Code's shape, camelCase ones in its own, with the arguments as a
// JSON string.
func TestCopilotInput(t *testing.T) {
	ev := parse(t, Copilot, harness.BeforeTool, `{"hook_event_name":"PreToolUse","session_id":"c1","timestamp":"2026-10-16T09:00:00.000Z","cwd":"/work","tool_name":"Edit","tool_input":{"path":"README.md","old_str":"a","new_str":"b"}}`)
	if ev.SessionID != "c1" || strings.Join(ev.Paths, ",") != "/work/README.md" {
		t.Fatalf("edit: %+v", ev)
	}
	ev = parse(t, Copilot, harness.AfterTool, `{"sessionId":"c1","timestamp":1773370259963,"cwd":"/work","toolName":"bash","toolArgs":"{\"command\":\"printf PROBE\",\"description\":\"probe\"}"}`)
	if ev.SessionID != "c1" || ev.Call.Doing != "Running printf" {
		t.Fatalf("camelCase: %+v %+v", ev, ev.Call)
	}
	ev = parse(t, Copilot, harness.Needs, `{"sessionId":"c1","timestamp":1773370259963,"cwd":"/work","hook_event_name":"Notification","message":"Approve?","notification_type":"permission_prompt"}`)
	if ev.Needs != "permission" {
		t.Fatalf("notification: %+v", ev)
	}
}

// Cursor's input, as Cursor sends it to its own hooks and to the Claude Code hooks it runs too: the session is the
// conversation, and cwd may be empty.
func TestCursorInput(t *testing.T) {
	input := `{"conversation_id":"c9","generation_id":"g","model":"m","tool_name":"Write","tool_input":{"file_path":"/w/api/x.go","content":"package api"},"tool_use_id":"t","cwd":"","session_id":"c9","hook_event_name":"preToolUse","cursor_version":"3.13.25","workspace_roots":["/w"],"user_email":"a@x.com","transcript_path":"/t.jsonl"}`
	for _, event := range []string{harness.BeforeTool, "edit"} { // murmur's Cursor hook, and its Claude Code hook
		ev := parse(t, Cursor, event, input)
		if ev.Kind != harness.BeforeTool || ev.SessionID != "c9" || ev.Cwd != "/w" || strings.Join(ev.Paths, ",") != "/w/api/x.go" {
			t.Fatalf("%s: %+v", event, ev)
		}
	}
	ev := parse(t, Cursor, "start", `{"conversation_id":"c9","session_id":"c9","is_background_agent":false,"composer_mode":"agent","hook_event_name":"sessionStart","cursor_version":"3.13.25","workspace_roots":["/w"]}`)
	if ev.Kind != harness.Start || ev.Cwd != "/w" {
		t.Fatalf("start: %+v", ev)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Join adds murmur's hooks after the person's own and keeps the rest of the file as it was; uninstall takes them out
// again, and removes a file join created.
func TestCodexHooks(t *testing.T) {
	sandbox(t)
	path := Codex.hooksPath()
	mine := `{
  "description": "my hooks",
  "hooks": {
    "PreToolUse": [
      {
        "hooks": [
          {
            "command": "/usr/bin/true",
            "type": "command"
          }
        ],
        "matcher": "Bash"
      }
    ]
  }
}
`
	_ = os.WriteFile(path, []byte(mine), 0o644)
	checks := Codex.Connect("/opt/murmur", nil)
	if !checks[0].OK || Codex.Connected() == false {
		t.Fatalf("connect: %+v", checks)
	}
	var f struct {
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct {
				Command string
				Timeout int
				Async   bool
			}
		}
	}
	_ = json.Unmarshal([]byte(read(t, path)), &f)
	pre := f.Hooks["PreToolUse"]
	if len(pre) != 2 || pre[0].Matcher != "Bash" || pre[1].Matcher != "apply_patch" || pre[1].Hooks[0].Command != "/opt/murmur hook codex before-tool" {
		t.Fatalf("PreToolUse: %+v", pre)
	}
	if len(f.Hooks) != 7 || !f.Hooks["PermissionRequest"][0].Hooks[0].Async || f.Hooks["SessionStart"][0].Hooks[0].Timeout != 5 {
		t.Fatalf("hooks: %+v", f.Hooks)
	}
	if !slices.ContainsFunc(checks, func(c protocol.Check) bool { return c.Name == "Next" && strings.Contains(c.Detail, "/hooks") }) {
		t.Fatalf("join doesn't say to trust the hooks: %+v", checks)
	}
	if err := Codex.Disconnect(""); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != mine {
		t.Fatalf("after uninstall:\n%s", got)
	}
	os.Remove(path)
	Codex.Connect("/opt/murmur", nil)
	if err := Codex.Disconnect(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("uninstall left the hooks file join created")
	}
}

// The MCP server goes through `codex mcp add`, with murmur's tools approved and MURMUR_HOME passed on; uninstall
// leaves config.toml without them.
func TestCodexMCP(t *testing.T) {
	root := sandbox(t)
	bin := filepath.Join(root, "bin")
	_ = os.MkdirAll(bin, 0o755)
	fake := `#!/bin/sh
echo "$*" >> "$CODEX_HOME/calls"
case "$2" in
add) printf '\n[mcp_servers.murmur]\ncommand = "x"\n' >> "$CODEX_HOME/config.toml" ;;
get) grep -q 'mcp_servers.murmur' "$CODEX_HOME/config.toml" ;;
esac
`
	_ = os.WriteFile(filepath.Join(bin, "codex"), []byte(fake), 0o755)
	t.Setenv("PATH", bin+":/bin:/usr/bin")
	toml := filepath.Join(os.Getenv("CODEX_HOME"), "config.toml")
	_ = os.WriteFile(toml, []byte("model = \"gpt\"\n\n[mcp_servers.docs]\ncommand = \"docs\"\n"), 0o600)
	t.Setenv("MURMUR_HOME", filepath.Join(root, "murmur"))
	if err := codexMCP("/opt/murmur", true); err != nil {
		t.Fatal(err)
	}
	if err := codexMCP("/opt/murmur", true); err != nil { // again, as after an update
		t.Fatal(err)
	}
	got := read(t, toml)
	if strings.Count(got, "[mcp_servers.murmur.tools.send]\napproval_mode = \"approve\"") != 1 || !strings.Contains(got, "[mcp_servers.docs]") {
		t.Fatalf("config.toml:\n%s", got)
	}
	if calls := read(t, filepath.Join(os.Getenv("CODEX_HOME"), "calls")); !strings.Contains(calls, "mcp add murmur --env MURMUR_HOME="+filepath.Join(root, "murmur")+" -- /opt/murmur mcp codex") {
		t.Fatalf("calls:\n%s", calls)
	}
	if err := codexMCP("", false); err != nil {
		t.Fatal(err)
	}
	if got := read(t, toml); strings.Contains(got, "murmur") || got != "model = \"gpt\"\n\n[mcp_servers.docs]\ncommand = \"docs\"\n" {
		t.Fatalf("after uninstall:\n%q", got)
	}
}

// A thread's name is the newest its person gave it with /rename.
func TestCodexTitle(t *testing.T) {
	sandbox(t)
	_ = os.WriteFile(filepath.Join(Codex.dir(), "session_index.jsonl"), []byte(
		`{"id":"T","thread_name":"Fix login","updated_at":"2026-10-16T10:00:00Z"}`+"\n"+
			`{"id":"U","thread_name":"Other","updated_at":"2026-10-16T10:01:00Z"}`+"\n"+
			`{"id":"T","thread_name":"Fix login, take 2","updated_at":"2026-10-16T10:02:00Z"}`+"\n"), 0o600)
	if ev := parse(t, Codex, harness.Prompt, `{"session_id":"T","cwd":"/repo","hook_event_name":"UserPromptSubmit","prompt":"go"}`); ev.Title != "Fix login, take 2" {
		t.Fatalf("title: %q", ev.Title)
	}
}

// Gemini CLI keeps hooks and MCP servers in its settings, timeouts in milliseconds, with murmur's server trusted.
func TestGeminiSettings(t *testing.T) {
	sandbox(t)
	path := Gemini.hooksPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	mine := "{\n  \"theme\": \"Default\",\n  \"mcpServers\": {\n    \"docs\": {\n      \"command\": \"docs\"\n    }\n  }\n}\n"
	_ = os.WriteFile(path, []byte(mine), 0o644)
	if checks := Gemini.Connect("/opt/murmur", nil); !checks[0].OK || !checks[1].OK {
		t.Fatalf("connect: %+v", checks)
	}
	var f struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
			Trust   bool
		}
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct{ Timeout int }
		}
	}
	_ = json.Unmarshal([]byte(read(t, path)), &f)
	if m := f.MCPServers["murmur"]; m.Command != "/opt/murmur" || strings.Join(m.Args, " ") != "mcp gemini" || !m.Trust || len(f.MCPServers) != 2 {
		t.Fatalf("mcpServers: %+v", f.MCPServers)
	}
	if b := f.Hooks["BeforeTool"]; len(b) != 1 || b[0].Matcher != "write_file|replace" || b[0].Hooks[0].Timeout != 5000 {
		t.Fatalf("BeforeTool: %+v", b)
	}
	if !Gemini.Connected() || !Gemini.mcpConnected() {
		t.Fatal("doctor doesn't see what join did")
	}
	if err := Gemini.Disconnect(""); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != mine {
		t.Fatalf("after uninstall:\n%s", got)
	}
}

// Copilot CLI gets a hooks file of murmur's own, and murmur's server in its MCP config with every tool.
func TestCopilotSetup(t *testing.T) {
	sandbox(t)
	if checks := Copilot.Connect("/opt/murmur", nil); !checks[0].OK || !checks[1].OK {
		t.Fatalf("connect: %+v", checks)
	}
	var hooks struct {
		Version int
		Hooks   map[string][]struct {
			Type, Bash string
			TimeoutSec int
		}
	}
	_ = json.Unmarshal([]byte(read(t, Copilot.hooksPath())), &hooks)
	if hooks.Version != 1 || hooks.Hooks["PreToolUse"][0].Bash != "/opt/murmur hook copilot before-tool" || hooks.Hooks["sessionEnd"][0].TimeoutSec != 5 {
		t.Fatalf("hooks: %+v", hooks)
	}
	if got := read(t, filepath.Join(Copilot.dir(), "mcp-config.json")); !strings.Contains(got, `"tools": [`) || !strings.Contains(got, `"type": "local"`) {
		t.Fatalf("mcp-config.json:\n%s", got)
	}
	if err := Copilot.Disconnect(""); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{Copilot.hooksPath(), filepath.Join(Copilot.dir(), "mcp-config.json")} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("uninstall left %s", f)
		}
	}
}

// Cursor's hooks share a file with the person's own; murmur's tools join an allowlist only where one exists.
func TestCursorSetup(t *testing.T) {
	sandbox(t)
	dir := Cursor.dir()
	mine := "{\n  \"version\": 1,\n  \"hooks\": {\n    \"stop\": [\n      {\n        \"command\": \"./audit.sh\"\n      }\n    ]\n  }\n}\n"
	_ = os.WriteFile(Cursor.hooksPath(), []byte(mine), 0o644)
	perms := "{\n  \"mcpAllowlist\": [\n    \"docs:*\"\n  ]\n}\n"
	_ = os.WriteFile(filepath.Join(dir, "permissions.json"), []byte(perms), 0o644)
	cli := "{\n  \"permissions\": {\n    \"allow\": [\n      \"Shell(ls)\"\n    ]\n  }\n}\n"
	_ = os.WriteFile(filepath.Join(dir, "cli-config.json"), []byte(cli), 0o644)
	if checks := Cursor.Connect("/opt/murmur", nil); !checks[0].OK || !checks[1].OK {
		t.Fatalf("connect: %+v", checks)
	}
	if got := read(t, Cursor.hooksPath()); !strings.Contains(got, "./audit.sh") || !strings.Contains(got, `"matcher": "Write"`) ||
		strings.Index(got, "./audit.sh") > strings.Index(got, "hook cursor stop") {
		t.Fatalf("hooks.json:\n%s", got)
	}
	if !strings.Contains(read(t, filepath.Join(dir, "permissions.json")), `"murmur:*"`) ||
		!strings.Contains(read(t, filepath.Join(dir, "cli-config.json")), `"Mcp(murmur:*)"`) {
		t.Fatal("murmur's tools aren't allowed")
	}
	if err := Cursor.Disconnect(""); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{"hooks.json": mine, "permissions.json": perms, "cli-config.json": cli} {
		if got := read(t, filepath.Join(dir, file)); got != want {
			t.Errorf("%s after uninstall:\n%s", file, got)
		}
	}
	// without an allowlist of its own, the editor keeps none: one would replace what it allows in its settings
	os.Remove(filepath.Join(dir, "permissions.json"))
	Cursor.Connect("/opt/murmur", nil)
	if _, err := os.Stat(filepath.Join(dir, "permissions.json")); !os.IsNotExist(err) {
		t.Fatal("join created an MCP allowlist")
	}
}

// A development build connects a harness only when the harness's own folder variable says where.
func TestDevGuard(t *testing.T) {
	sandbox(t)
	t.Setenv("CODEX_HOME", "")
	if checks := Codex.Connect("/opt/murmur", nil); len(checks) != 1 || checks[0].Name != harness.Skipped {
		t.Fatalf("connect: %+v", checks)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".codex")); !os.IsNotExist(err) {
		t.Fatal("a development build wrote to ~/.codex")
	}
}

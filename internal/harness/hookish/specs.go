package hookish

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/EledoneAi/murmur/internal/config"
	"github.com/EledoneAi/murmur/internal/harness"
)

// homeDir is a harness's folder: its own variable when set, else a folder in the home folder.
func homeDir(env, name string) func() string {
	return func() string {
		if d := os.Getenv(env); d != "" {
			return d
		}
		return filepath.Join(config.Home(), name)
	}
}

func mcpArgs(exe, name string) map[string]any {
	return map[string]any{"command": exe, "args": []string{"mcp", name}}
}

// Codex: hooks in $CODEX_HOME/hooks.json, which Codex runs once its person trusts them; the MCP server through
// `codex mcp add`, its two tools approved in config.toml. Its hooks fire in a shared background server rather than
// the terminal's process, and its edits are patches.
var Codex = &Spec{
	name: "codex", title: "Codex", command: "codex", dirVar: "CODEX_HOME", dir: homeDir("CODEX_HOME", ".codex"),
	hooksFile: "hooks.json", layout: groups, timeoutUnit: 1, dialect: claudeStyle,
	hooks: []hook{
		{event: "SessionStart", kind: harness.Start},
		{event: "UserPromptSubmit", kind: harness.Prompt},
		{event: "PreToolUse", kind: harness.BeforeTool, matcher: "apply_patch"}, // also matches its Edit and Write aliases
		{event: "PostToolUse", kind: harness.AfterTool},
		{event: "Stop", kind: harness.Stop},
		{event: "PermissionRequest", kind: harness.Needs, async: true},
		{event: "SessionEnd", kind: harness.End},
	},
	needs:      map[string]string{"": "permission"},
	minVersion: "0.148.0", // session ids in hooks (0.131), SessionEnd (0.145), async hooks (0.148)
	note:       "Codex runs new hooks once you trust them: when it starts, choose Trust all and continue, or trust murmur's in /hooks",
	titleOf:    codexTitle,
}

// Gemini CLI: hooks and the MCP server in ~/.gemini/settings.json, timeouts in milliseconds. It runs both only in
// folders its person trusts.
var Gemini = &Spec{
	name: "gemini", title: "Gemini CLI", command: "gemini", dirVar: "GEMINI_CLI_HOME", dir: geminiDir,
	hooksFile: "settings.json", layout: groups, timeoutUnit: 1000, dialect: geminiStyle,
	hooks: []hook{
		{event: "SessionStart", kind: harness.Start},
		{event: "BeforeAgent", kind: harness.Prompt},
		{event: "BeforeTool", kind: harness.BeforeTool, matcher: "write_file|replace"},
		{event: "AfterTool", kind: harness.AfterTool},
		{event: "AfterAgent", kind: harness.Stop},
		{event: "Notification", kind: harness.Needs},
		{event: "SessionEnd", kind: harness.End},
	},
	needs:   map[string]string{"ToolPermission": "permission"},
	mcpFile: "settings.json",
	mcpEntry: func(exe string) map[string]any {
		e := mcpArgs(exe, "gemini")
		e["trust"] = true // its tools run without a confirmation
		return e
	},
	minVersion: "0.26.0", // hooks on by default
	note:       "Gemini CLI runs hooks and MCP servers only in folders you trust",
}

// GEMINI_CLI_HOME stands for the home folder: Gemini CLI's folder is .gemini in it.
func geminiDir() string {
	home := os.Getenv("GEMINI_CLI_HOME")
	if home == "" {
		home = config.Home()
	}
	return filepath.Join(home, ".gemini")
}

// Copilot CLI: a hooks file of murmur's own in ~/.copilot/hooks. Its PascalCase events take Claude Code's input and
// answers; it has no PascalCase notification or end, so those two are camelCase. It asks before every MCP tool and
// remembers an answer per folder only, so the before-tool hook runs on every tool and allows murmur's.
var Copilot = &Spec{
	name: "copilot", title: "Copilot CLI", command: "copilot", dirVar: "COPILOT_HOME", dir: homeDir("COPILOT_HOME", ".copilot"),
	hooksFile: filepath.Join("hooks", "murmur.json"), layout: ownFile, timeoutUnit: 1, dialect: copilotStyle,
	hooks: []hook{
		{event: "SessionStart", kind: harness.Start},
		{event: "UserPromptSubmit", kind: harness.Prompt},
		{event: "PreToolUse", kind: harness.BeforeTool},
		{event: "PostToolUse", kind: harness.AfterTool},
		{event: "Stop", kind: harness.Stop},
		{event: "notification", kind: harness.Needs},
		{event: "sessionEnd", kind: harness.End},
	},
	needs:   map[string]string{"permission_prompt": "permission", "elicitation_dialog": "input"},
	mcpFile: "mcp-config.json",
	mcpEntry: func(exe string) map[string]any {
		e := mcpArgs(exe, "copilot")
		e["type"], e["tools"] = "local", []string{"*"}
		return e
	},
	sessionVar: "COPILOT_AGENT_SESSION_ID",
	minVersion: "1.0.72", // PascalCase events with Claude Code's input (1.0.21), stop_hook_active (1.0.72)
}

// Cursor: hooks in ~/.cursor/hooks.json, the MCP server in mcp.json, and murmur's tools in the allowlists of the
// editor and the CLI where the person keeps one. Cursor also runs the hooks in ~/.claude/settings.json; murmur's
// there step aside for these (see setup.Hook).
var Cursor = &Spec{
	name: "cursor", title: "Cursor", command: "cursor-agent", dirVar: "CURSOR_CONFIG_DIR", dir: homeDir("CURSOR_CONFIG_DIR", ".cursor"),
	hooksFile: "hooks.json", layout: flat, timeoutUnit: 1, dialect: cursorStyle,
	hooks: []hook{
		{event: "sessionStart", kind: harness.Start},
		{event: "beforeSubmitPrompt", kind: harness.Prompt},
		{event: "preToolUse", kind: harness.BeforeTool, matcher: "Write"}, // its edits, and Claude Code's Edit and Write
		{event: "postToolUse", kind: harness.AfterTool},
		{event: "stop", kind: harness.Stop},
		{event: "sessionEnd", kind: harness.End},
	},
	mcpFile:  "mcp.json",
	mcpEntry: func(exe string) map[string]any { return mcpArgs(exe, "cursor") },
	extra:    cursorAllow,
}

// cursorAllow lets murmur's tools run without a prompt: in the editor's permissions.json, only where its person keeps
// an MCP allowlist there (one there replaces the editor's own), and in the CLI's cli-config.json, when it has one.
func cursorAllow(s *Spec, add bool) error {
	return errors.Join(
		allow(filepath.Join(s.dir(), "permissions.json"), []string{"mcpAllowlist"}, "murmur:*", add),
		allow(filepath.Join(s.dir(), "cli-config.json"), []string{"permissions", "allow"}, "Mcp(murmur:*)", add))
}

// allow adds (or removes) entry in the list at path in a JSON file, when the file and the list's parent exist. An
// editor's allowlist that doesn't exist yet stays so.
func allow(file string, path []string, entry string, add bool) error {
	o, b, err := harness.ReadObject(file)
	if err != nil || b == nil {
		return err
	}
	parent := o
	for _, k := range path[:len(path)-1] {
		parent = parent.Sub(k)
	}
	key := path[len(path)-1]
	if parent.Get(key) == nil && (len(path) == 1 || !add) {
		return nil
	}
	var list []string
	_ = json.Unmarshal(parent.Get(key), &list)
	list = slices.DeleteFunc(list, func(e string) bool { return e == entry })
	if add {
		list = append(list, entry)
	}
	v, _ := json.Marshal(list)
	parent = parent.Set(key, v)
	if len(path) > 1 {
		o = o.PutSub(path[0], parent)
	} else {
		o = parent
	}
	return harness.WriteObject(file, b, o, true)
}

// ---------------------------------------------------------------- Codex

// codexApprovals let Codex run murmur's two tools without asking. `codex mcp add` writes the server's table and
// drops these, so they follow it each time; TOML takes a table's sub-tables anywhere in the file.
const codexApprovals = `
[mcp_servers.murmur.tools.who]
approval_mode = "approve"

[mcp_servers.murmur.tools.send]
approval_mode = "approve"
`

// codexMCP registers (or with add=false removes) murmur's MCP server in Codex. Codex starts MCP servers with a bare
// environment, so a MURMUR_HOME of the person's own goes with it.
func codexMCP(exe string, add bool) error {
	codex, err := exec.LookPath("codex")
	if err != nil {
		if add {
			return errors.New("codex not found on PATH; run: codex mcp add murmur -- " + exe + " mcp codex")
		}
		return nil
	}
	_ = exec.Command(codex, "mcp", "remove", "murmur").Run()
	toml := filepath.Join(Codex.dir(), "config.toml")
	if err := dropTables(toml, "mcp_servers.murmur"); err != nil {
		return err
	}
	if !add {
		return nil
	}
	args := []string{"mcp", "add", "murmur"}
	if home := os.Getenv("MURMUR_HOME"); home != "" {
		args = append(args, "--env", "MURMUR_HOME="+home)
	}
	if out, err := exec.Command(codex, append(args, "--", exe, "mcp", "codex")...).CombinedOutput(); err != nil {
		return errors.New(strings.TrimSpace(string(out)))
	}
	f, err := os.OpenFile(toml, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(codexApprovals)
	return err
}

func codexMCPConnected() bool {
	codex, err := exec.LookPath("codex")
	return err == nil && exec.Command(codex, "mcp", "get", "murmur").Run() == nil
}

// dropTables takes out of a TOML file the tables under prefix, [prefix] and [prefix.…], each up to the next table.
func dropTables(path, prefix string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	var out []string
	dropping := false
	for line := range strings.SplitSeq(string(b), "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "[") {
			name := strings.Trim(t, "[] ")
			dropping = name == prefix || strings.HasPrefix(name, prefix+".")
		}
		if !dropping {
			out = append(out, line)
		}
	}
	kept := strings.Join(out, "\n")
	for strings.Contains(kept, "\n\n\n") {
		kept = strings.ReplaceAll(kept, "\n\n\n", "\n\n")
	}
	if kept == string(b) {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(kept), st.Mode().Perm())
}

// codexTitle is the name its person gave a Codex thread with /rename, from session_index.jsonl, where the newest line
// for an id wins. Codex gives threads no title of its own.
func codexTitle(s *Spec, id string) string {
	f, err := os.Open(filepath.Join(s.dir(), "session_index.jsonl"))
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 1<<20 {
		_, _ = f.Seek(-1<<20, io.SeekEnd)
	}
	title := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if !bytes.Contains(sc.Bytes(), []byte(id)) {
			continue
		}
		var e struct {
			ID   string `json:"id"`
			Name string `json:"thread_name"`
		}
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.ID == id {
			title = e.Name
		}
	}
	return title
}

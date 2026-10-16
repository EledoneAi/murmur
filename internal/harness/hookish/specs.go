package hookish

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/EledoneAi/murmur/internal/config"
	"github.com/EledoneAi/murmur/internal/harness"
	"github.com/EledoneAi/murmur/internal/protocol"
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

// Codex: hooks in $CODEX_HOME/hooks.json, which Codex runs once its person trusts them; the MCP server in a block of
// murmur's own in config.toml, its two tools approved. Its hooks fire in a shared background server rather than the
// terminal's process, and its edits are patches.
var Codex = &Spec{
	name: "codex", title: "Codex", command: "codex", dirVar: "CODEX_HOME", dir: homeDir("CODEX_HOME", ".codex"),
	hooksFile: "hooks.json", tomlFile: "config.toml", layout: groups, timeoutUnit: 1, dialect: claudeStyle,
	hooks: []hook{
		{event: "SessionStart", kind: harness.Start, carries: true},
		{event: "UserPromptSubmit", kind: harness.Prompt, carries: true},
		{event: "PreToolUse", kind: harness.BeforeTool, matcher: "apply_patch"}, // also matches its Edit and Write aliases
		{event: "PostToolUse", kind: harness.AfterTool, carries: true},
		{event: "Stop", kind: harness.Stop, carries: true},
		{event: "PermissionRequest", kind: harness.Needs, async: true},
		{event: "SessionEnd", kind: harness.End},
	},
	needs:      map[string]string{"": "permission"},
	minVersion: "0.148.0", // session ids in hooks (0.131), SessionEnd (0.145), async hooks (0.148)
	note:       "Codex runs a new hook only once you trust it: open /hooks in Codex and trust murmur's",
	titleOf:    codexTitle,
}

// Gemini CLI: hooks and the MCP server in ~/.gemini/settings.json, timeouts in milliseconds. Folder trust is on by
// default, and in a folder its person hasn't trusted, Gemini CLI runs no hooks and no MCP server: murmur sees nothing.
var Gemini = &Spec{
	name: "gemini", title: "Gemini CLI", command: "gemini", dirVar: "GEMINI_CLI_HOME", dir: geminiDir,
	hooksFile: "settings.json", layout: groups, timeoutUnit: 1000, dialect: geminiStyle,
	hooks: []hook{
		{event: "SessionStart", kind: harness.Start, carries: true},
		{event: "BeforeAgent", kind: harness.Prompt, carries: true},
		{event: "BeforeTool", kind: harness.BeforeTool, matcher: "write_file|replace"},
		{event: "AfterTool", kind: harness.AfterTool, carries: true},
		{event: "AfterAgent", kind: harness.Stop, carries: true},
		{event: "Notification", kind: harness.Needs},
		{event: "SessionEnd", kind: harness.End},
	},
	needs:   map[string]string{"ToolPermission": "permission"},
	mcpFile: "settings.json",
	mcpEntry: func(exe string) map[string]any {
		e := mcpArgs(exe, "gemini")
		// its two tools run without a confirmation. Gemini CLI merges a project's mcpServers over the person's by
		// name, whole, so a project's own server called murmur doesn't get this trust
		e["trust"], e["includeTools"] = true, []string{"who", "send"}
		return e
	},
	minVersion: "0.26.0", // hooks on by default
	note: "Gemini CLI runs hooks and MCP servers only in folders you trust, and folder trust is on by default: " +
		"trust each project folder when Gemini CLI asks, or murmur won't see its sessions there",
	checks: geminiTrust,
}

// GEMINI_CLI_HOME stands for the home folder: Gemini CLI's folder is .gemini in it.
func geminiDir() string {
	home := os.Getenv("GEMINI_CLI_HOME")
	if home == "" {
		home = config.Home()
	}
	return filepath.Join(home, ".gemini")
}

// geminiTrust says whether Gemini CLI's folder trust is on, which keeps murmur out of the folders not trusted.
func geminiTrust(s *Spec) []protocol.Check {
	o, _, _ := harness.ReadObject(s.hooksPath())
	var on *bool
	_ = json.Unmarshal(o.Sub("security").Sub("folderTrust").Get("enabled"), &on)
	if on != nil && !*on {
		return []protocol.Check{{Name: "Folders", OK: true, Detail: "folder trust is off: Gemini CLI runs murmur's hooks in every folder"}}
	}
	return []protocol.Check{{Name: "Folders", OK: true, Detail: "only in folders you trust: Gemini CLI runs no hooks and no MCP server in the others"}}
}

// Copilot CLI: a hooks file of murmur's own in ~/.copilot/hooks. Its PascalCase events take Claude Code's input and
// answers; it has no PascalCase notification or end, so those two are camelCase. It drops what a prompt hook prints,
// so messages leave after a tool call or at the end of a turn. It asks before every MCP tool and remembers an answer
// per folder only, so the before-tool hook, on edits and murmur's two tools, allows those two.
var Copilot = &Spec{
	name: "copilot", title: "Copilot CLI", command: "copilot", dirVar: "COPILOT_HOME", dir: homeDir("COPILOT_HOME", ".copilot"),
	hooksFile: filepath.Join("hooks", "murmur.json"), layout: ownFile, timeoutUnit: 1, dialect: copilotStyle,
	hooks: []hook{
		{event: "SessionStart", kind: harness.Start},
		{event: "UserPromptSubmit", kind: harness.Prompt},
		{event: "PreToolUse", kind: harness.BeforeTool, matcher: "Edit|Write|MultiEdit|edit|create|apply_patch|str_replace_editor|murmur-who|murmur-send"},
		{event: "PostToolUse", kind: harness.AfterTool, carries: true},
		{event: "Stop", kind: harness.Stop, carries: true},
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
	allowTools: []string{"murmur-who", "murmur-send"},
	minVersion: "1.0.72", // PascalCase events with Claude Code's input (1.0.21), stop_hook_active (1.0.72)
}

// Cursor: hooks in ~/.cursor/hooks.json, the MCP server in mcp.json, and murmur's two tools in the allowlists of the
// editor and the CLI where the person keeps one. It reads nothing a prompt hook prints but whether to go on, and its
// start hook's context misses the first message in some windows: messages leave after a tool call or at the end of
// a turn. Its MCP servers run apart from its sessions. Cursor also runs the hooks in ~/.claude/settings.json; murmur's
// there step aside for these (see setup.Hook).
var Cursor = &Spec{
	name: "cursor", title: "Cursor", command: "cursor-agent", dirVar: "CURSOR_CONFIG_DIR", dir: homeDir("CURSOR_CONFIG_DIR", ".cursor"),
	hooksFile: "hooks.json", layout: flat, timeoutUnit: 1, dialect: cursorStyle,
	hooks: []hook{
		{event: "sessionStart", kind: harness.Start},
		{event: "beforeSubmitPrompt", kind: harness.Prompt},
		{event: "preToolUse", kind: harness.BeforeTool, matcher: "Write"}, // its edits, and Claude Code's Edit and Write
		{event: "postToolUse", kind: harness.AfterTool, carries: true},
		{event: "stop", kind: harness.Stop, carries: true},
		{event: "sessionEnd", kind: harness.End},
	},
	mcpFile:  "mcp.json",
	mcpEntry: func(exe string) map[string]any { return mcpArgs(exe, "cursor") },
	apart:    true,
	allow: []allowList{
		// an MCP allowlist in permissions.json replaces the editor's own: murmur adds to one that is there
		{file: "permissions.json", path: []string{"mcpAllowlist"}, entries: []string{"murmur:who", "murmur:send"}, ifList: true},
		{file: "cli-config.json", path: []string{"permissions", "allow"}, entries: []string{"Mcp(murmur:who)", "Mcp(murmur:send)"}},
	},
}

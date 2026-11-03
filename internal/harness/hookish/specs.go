package hookish

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/harness"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
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
// murmur's own in config.toml, its tools approved. Its hooks fire in a shared background server rather than the
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
		// its tools run without a confirmation. Gemini CLI merges a project's mcpServers over the person's by name,
		// whole, so a project's own server called murmur doesn't get this trust
		e["trust"], e["includeTools"] = true, []string{"who", "send", "btw"}
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

// Antigravity: its CLI (agy), app and IDE share ~/.gemini/config, where murmur's hooks are a block of their own in
// hooks.json and its MCP server is in mcp_config.json. Hooks read camelCase input and answer in their own words:
// steps added to the conversation at its start and before each model call, a decision before a tool, and continue
// at the end of a turn; what follows a tool call is dropped. A tool fails when its hook fails or answers without a
// decision, so murmur's hooks never fail and print nothing when they have nothing to say. MCP tools ask first, and
// agy -p refuses what it can't ask: the hook before tools approves murmur's. A subagent's hooks name a conversation
// of its own, with no prompt of its person's: murmur leaves them out. Antigravity has no setting for its folder, so
// MURMUR_ANTIGRAVITY_DIR is murmur's own: a test laptop points it at a project's .agents, which Antigravity reads alike.
var Antigravity = &Spec{
	name: "antigravity", title: "Antigravity", command: "agy", dirVar: "MURMUR_ANTIGRAVITY_DIR",
	dir: homeDir("MURMUR_ANTIGRAVITY_DIR", filepath.Join(".gemini", "config")), hooksFile: "hooks.json",
	layout: named, timeoutUnit: 1, dialect: antigravityStyle, failsTools: true,
	hooks: []hook{
		{event: "SessionStart", kind: harness.Start, carries: true},
		{event: "PreInvocation", kind: harness.Prompt, carries: true},
		// its edits in the CLI and in the app and IDE, and MCP tools, which it calls through call_mcp_tool
		{event: "PreToolUse", kind: harness.BeforeTool,
			matcher: "write_file|edit_file|write_to_file|replace_file_content|multi_replace_file_content|sed_file|call_mcp_tool"},
		{event: "PostToolUse", kind: harness.AfterTool},
		{event: "Stop", kind: harness.Stop, carries: true},
	},
	mcpFile:    "mcp_config.json",
	mcpEntry:   func(exe string) map[string]any { return mcpArgs(exe, "antigravity") },
	allowTools: []string{"mcp__murmur__who", "mcp__murmur__send", "mcp__murmur__btw"},
	minVersion: "1.4.7", // Stop hooks that run (1.1.10), lastUserInput, which tells a subagent's hooks apart (1.4.7)
}

// Copilot CLI: a hooks file of murmur's own in ~/.copilot/hooks. Its PascalCase events take Claude Code's input and
// answers; it has no PascalCase notification or end, so those two are camelCase. It drops what a prompt hook prints,
// so messages leave after a tool call or at the end of a turn. It asks before every MCP tool and remembers an answer
// per folder only, so the before-tool hook, on edits and murmur's tools, allows murmur's.
var Copilot = &Spec{
	name: "copilot", title: "Copilot CLI", command: "copilot", dirVar: "COPILOT_HOME", dir: homeDir("COPILOT_HOME", ".copilot"),
	hooksFile: filepath.Join("hooks", "murmur.json"), layout: ownFile, timeoutUnit: 1, dialect: copilotStyle, failsTools: true,
	hooks: []hook{
		{event: "SessionStart", kind: harness.Start},
		{event: "UserPromptSubmit", kind: harness.Prompt},
		{event: "PreToolUse", kind: harness.BeforeTool, matcher: "Edit|Write|MultiEdit|edit|create|apply_patch|str_replace_editor|murmur-who|murmur-send|murmur-btw"},
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
	allowTools: []string{"murmur-who", "murmur-send", "murmur-btw"},
	minVersion: "1.0.72", // PascalCase events with Claude Code's input (1.0.21), stop_hook_active (1.0.72)
}

// Cursor: hooks in ~/.cursor/hooks.json, the MCP server in mcp.json, and murmur's tools in the allowlists of the
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
		{file: "permissions.json", path: []string{"mcpAllowlist"}, entries: []string{"murmur:who", "murmur:send", "murmur:btw"}, ifList: true},
		{file: "cli-config.json", path: []string{"permissions", "allow"}, entries: []string{"Mcp(murmur:who)", "Mcp(murmur:send)", "Mcp(murmur:btw)"}},
	},
}

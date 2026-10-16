// Package claude is murmur's adapter for Claude Code: its session files, transcripts, inbox sockets, settings and
// hooks. Claude Code is the one harness that lists its own live sessions and takes messages while they run.
package claude

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/harness"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

const Name = "claude"

type Claude struct{}

func (Claude) Name() string       { return Name }
func (Claude) Title() string      { return "Claude Code" }
func (Claude) SessionVar() string { return "CLAUDE_CODE_SESSION_ID" }

func settings() string { return filepath.Join(Dir(), "settings.json") }

func (Claude) Installed() bool {
	if _, err := exec.LookPath("claude"); err == nil {
		return true
	}
	_, err := os.Stat(Dir())
	return err == nil
}

// Connected says whether settings.json holds any of murmur's hooks, as this version or an older one wrote them.
func (Claude) Connected() bool {
	o, _, err := harness.ReadObject(settings())
	return err == nil && harness.HasOurs(o.Sub("hooks"))
}

// Connect adds murmur's hooks and tool permissions to settings.json and registers the MCP server. When Claude Code's
// own settings would keep murmur's messages from agents, it asks whether to let them through.
func (Claude) Connect(exe string, ask func(string) bool) []protocol.Check {
	if err := harness.Guard("CLAUDE_CONFIG_DIR"); err != nil {
		return harness.Guarded(err)
	}
	path := settings()
	if err := EditSettings(path, exe, true); err != nil {
		return []protocol.Check{{Name: "Hooks", Detail: err.Error()}}
	}
	checks := []protocol.Check{
		{Name: "Hooks", OK: true, Detail: "SessionStart, PreToolUse (edits), Notification in " + config.Tildify(path)},
		{Name: "Permissions", OK: true, Detail: "who, send allowed without a prompt"},
	}
	if note := InboundNote(path); note != "" {
		c := protocol.Check{Name: "Messages", Detail: note}
		if ask != nil && ask("  "+note+".\n  Let your sessions take messages from other sessions (crossSessionInbound: accept)?") {
			if err := AcceptMessages(path); err != nil {
				c.Detail = err.Error()
			} else {
				c = protocol.Check{Name: "Messages", OK: true, Detail: "agents take messages from other sessions"}
			}
		}
		checks = append(checks, c)
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		return append(checks, protocol.Check{Name: "MCP server", Detail: "claude not found on PATH; run: claude mcp add --scope user murmur -- " + exe + " mcp claude"})
	}
	_ = exec.Command(claude, "mcp", "remove", "--scope", "user", "murmur").Run()
	if out, err := exec.Command(claude, "mcp", "add", "--scope", "user", "murmur", "--", exe, "mcp", Name).CombinedOutput(); err != nil {
		return append(checks, protocol.Check{Name: "MCP server", Detail: strings.TrimSpace(string(out))})
	}
	return append(checks, protocol.Check{Name: "MCP server", OK: true, Detail: "murmur (user scope): who, send"})
}

// Disconnect takes murmur's entries out of settings.json, and its MCP server out of Claude Code when it is there.
func (Claude) Disconnect(exe string) error {
	if err := harness.Guard("CLAUDE_CONFIG_DIR"); err != nil {
		return err
	}
	err := EditSettings(settings(), exe, false)
	if claude, lerr := exec.LookPath("claude"); lerr == nil && exec.Command(claude, "mcp", "get", "murmur").Run() == nil {
		_ = exec.Command(claude, "mcp", "remove", "--scope", "user", "murmur").Run()
	}
	return err
}

// Holds says whether Claude Code holds anything of murmur's: its hooks or permissions, or its MCP server. A
// settings.json murmur can't read may: uninstall tries it, and says what went wrong.
func (c Claude) Holds() bool {
	if _, _, err := harness.ReadObject(settings()); err != nil || c.Connected() {
		return true
	}
	if _, perms, _ := SetupState(settings()); perms {
		return true
	}
	claude, err := exec.LookPath("claude")
	return err == nil && exec.Command(claude, "mcp", "get", "murmur").Run() == nil
}

// Version is the installed Claude Code's version, or "" when claude isn't on PATH.
func Version() string {
	claude, err := exec.LookPath("claude")
	if err != nil {
		return ""
	}
	out, _ := exec.Command(claude, "--version").Output()
	v, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	return v
}

// Checks looks at what join set up in Claude Code. Each failed check says how to fix it.
func (Claude) Checks() []protocol.Check {
	path := settings()
	hooks, perms, _ := SetupState(path)
	note := InboundNote(path)
	fix := "missing: run murmur join"
	if o, _, err := harness.ReadObject(path); err == nil && harness.HasOld(o.Sub("hooks")) {
		fix = "an older murmur's, which this version doesn't run: run murmur join, then restart your sessions"
	}
	checks := []protocol.Check{
		harness.Check(hooks, "Hooks", "SessionStart, PreToolUse (edits), Notification in "+config.Tildify(path), fix),
		harness.Check(perms, "Permissions", "who, send allowed", "missing: run murmur join"),
		harness.Check(note == "", "Messages", "agents take messages from other sessions", note),
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		return append(checks, harness.Check(false, "Version", "", "claude is not on PATH"))
	}
	v := Version()
	return append(checks,
		harness.Check(!TooOld(v), "Version", v, v+": messages between sessions need "+MinVersion+" or later; run claude update"),
		harness.Check(exec.Command(claude, "mcp", "get", "murmur").Run() == nil, "MCP server", "murmur: who, send", "missing: run murmur join"))
}

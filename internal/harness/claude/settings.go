package claude

import (
	"encoding/json"
	"os"
	"slices"

	"github.com/EledoneAi/murmur/internal/config"
	"github.com/EledoneAi/murmur/internal/harness"
)

// Setup touches three things outside ~/.murmur, and uninstall removes exactly those:
//   - three hooks in ~/.claude/settings.json: SessionStart starts the daemon and briefs the session, PreToolUse
//     checks a file before an agent edits it, Notification reports "needs you"
//   - permission for the two murmur tools in the same file, so agents can use them without a prompt
//   - the murmur MCP server, registered with `claude mcp add --scope user`

// hookArgs are what murmur's hook commands end with.
var hookArgs = map[string]string{"SessionStart": "hook claude start", "PreToolUse": "hook claude edit", "Notification": "hook claude notify"}

// hookEvents are the events murmur hooks, in the order join adds them.
var hookEvents = []string{"SessionStart", "PreToolUse", "Notification"}

// editTools are the tools that change files, which the edit hook checks before they run. Recent Claude Code has no
// MultiEdit; older versions do.
const editTools = "Edit|MultiEdit|Write|NotebookEdit"

// hookGroup is murmur's entry for one event. Claude Code waits for the start hook, whose brief only counts as
// the session's context when Claude Code waits for it, and for the edit hook, which may turn an edit back. Both give
// up on their own in under a second, well before these timeouts. The Notification hook runs in the background.
func hookGroup(ev, exe string) json.RawMessage {
	h := map[string]any{"type": "command", "command": harness.ShellQuote(exe) + " " + hookArgs[ev], "timeout": 5}
	g := map[string]any{"hooks": []any{h}}
	switch ev {
	case "PreToolUse":
		g["matcher"] = editTools
	case "Notification":
		h["async"], h["timeout"] = true, 10
	}
	b, _ := json.Marshal(g)
	return b
}

var toolPermissions = []string{"mcp__murmur__who", "mcp__murmur__send"}

// OurGroup says whether a group of hooks in a Claude-style settings file holds one of murmur's.
func OurGroup(group json.RawMessage) bool {
	var g struct {
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	_ = json.Unmarshal(group, &g)
	for _, h := range g.Hooks {
		if harness.Ours(h.Command) {
			return true
		}
	}
	return false
}

// EditSettings adds (or with add=false removes) murmur's entries in Claude Code's settings.json.
func EditSettings(path, exe string, add bool) error {
	o, b, err := harness.ReadObject(path)
	if err != nil {
		return err
	}
	hooks := o.Sub("hooks")
	for _, ev := range hookEvents {
		var groups []json.RawMessage
		if v := hooks.Get(ev); v != nil {
			_ = json.Unmarshal(v, &groups)
		}
		groups = slices.DeleteFunc(groups, OurGroup)
		if add {
			groups = append(groups, hookGroup(ev, exe))
		}
		if len(groups) == 0 {
			hooks = hooks.Del(ev)
		} else {
			v, _ := json.Marshal(groups)
			hooks = hooks.Set(ev, v)
		}
	}
	o = o.PutSub("hooks", hooks)

	perms := o.Sub("permissions")
	var allow []string
	if v := perms.Get("allow"); v != nil {
		_ = json.Unmarshal(v, &allow)
	}
	allow = slices.DeleteFunc(allow, func(s string) bool { return slices.Contains(toolPermissions, s) })
	if add {
		allow = append(allow, toolPermissions...)
	}
	if len(allow) == 0 {
		perms = perms.Del("allow")
	} else {
		v, _ := json.Marshal(allow)
		perms = perms.Set("allow", v)
	}
	o = o.PutSub("permissions", perms)

	return harness.WriteObject(path, b, o, add)
}

// SetupState says whether settings.json holds murmur's hooks and tool permissions, and its crossSessionInbound.
func SetupState(path string) (hooks, perms bool, inbound string) {
	o, b, err := harness.ReadObject(path)
	if err != nil || b == nil {
		return false, false, ""
	}
	hooks = true
	for _, ev := range hookEvents {
		var groups []json.RawMessage
		_ = json.Unmarshal(o.Sub("hooks").Get(ev), &groups)
		hooks = hooks && slices.ContainsFunc(groups, OurGroup)
	}
	var allow []string
	_ = json.Unmarshal(o.Sub("permissions").Get("allow"), &allow)
	perms = !slices.ContainsFunc(toolPermissions, func(p string) bool { return !slices.Contains(allow, p) })
	_ = json.Unmarshal(o.Get("crossSessionInbound"), &inbound)
	return hooks, perms, inbound
}

// InboundNote says when Claude Code's own settings keep murmur's messages from agents, or "" when they don't.
// join and doctor only say it: crossSessionInbound covers every message between sessions, not only murmur's.
func InboundNote(settings string) string {
	b, err := os.ReadFile(settings)
	if err != nil {
		return ""
	}
	var s struct {
		Inbound     string `json:"crossSessionInbound"`
		Permissions struct {
			DefaultMode string `json:"defaultMode"`
		} `json:"permissions"`
	}
	if json.Unmarshal(b, &s) != nil {
		return ""
	}
	switch {
	case s.Inbound == "refuse":
		return `crossSessionInbound is "refuse" in ` + config.Tildify(settings) + ": agents get no murmur messages"
	case s.Inbound == "" && s.Permissions.DefaultMode == "bypassPermissions":
		return "sessions that skip permission prompts hold each message for an approval and drop it after 5 minutes; " +
			`to take them, set "crossSessionInbound": "accept" in ` + config.Tildify(settings)
	}
	return ""
}

// AcceptMessages sets crossSessionInbound to accept, so this person's sessions take messages from other sessions
// without holding each one for an approval. join asks first: the setting covers every message between sessions.
func AcceptMessages(path string) error {
	o, b, err := harness.ReadObject(path)
	if err != nil {
		return err
	}
	return harness.WriteObject(path, b, o.Set("crossSessionInbound", json.RawMessage(`"accept"`)), true)
}

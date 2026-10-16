package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/EledoneAi/murmur/internal/config"
	"github.com/EledoneAi/murmur/internal/release"
)

// Setup touches three things outside ~/.murmur, and uninstall removes exactly those:
//   - three hooks in ~/.claude/settings.json: SessionStart starts the daemon and briefs the session, PreToolUse
//     checks a file before an agent edits it, Notification reports "needs you"
//   - permission for the two murmur tools in the same file, so agents can use them without a prompt
//   - the murmur MCP server, registered with `claude mcp add --scope user`

// hookArgs are what murmur's hook commands end with; join and uninstall find their own entries by them.
var hookArgs = map[string]string{"SessionStart": "hook start", "PreToolUse": "hook edit", "Notification": "hook notify"}

// hookEvents are the events murmur hooks, in the order join adds them.
var hookEvents = []string{"SessionStart", "PreToolUse", "Notification"}

// editTools are the tools that change files, which the edit hook checks before they run. Recent Claude Code has no
// MultiEdit; older versions do.
const editTools = "Edit|MultiEdit|Write|NotebookEdit"

// hookGroup is murmur's entry for one event. Claude Code waits for the start hook, whose brief only counts as
// the session's context when Claude Code waits for it, and for the edit hook, which may turn an edit back. Both give
// up on their own in under a second, well before these timeouts. The Notification hook runs in the background.
func hookGroup(ev, exe string) json.RawMessage {
	h := map[string]any{"type": "command", "command": shellQuote(exe) + " " + hookArgs[ev], "timeout": 5}
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

func shellQuote(s string) string {
	if !strings.ContainsAny(s, " '\"\\$`!*?&;|<>()[]{}") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ---------------------------------------------------------------- settings.json, keys kept in order

type field struct {
	k string
	v json.RawMessage
}

type object []field

func parseObject(b []byte) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var o object
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o = append(o, field{t.(string), v})
	}
	return o, nil
}

func (o object) get(k string) json.RawMessage {
	for _, f := range o {
		if f.k == k {
			return f.v
		}
	}
	return nil
}

func (o object) set(k string, v json.RawMessage) object {
	for i, f := range o {
		if f.k == k {
			o[i].v = v
			return o
		}
	}
	return append(o, field{k, v})
}

func (o object) del(k string) object {
	return slices.DeleteFunc(o, func(f field) bool { return f.k == k })
}

func (o object) bytes() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(f.k)
		b.Write(k)
		b.WriteByte(':')
		b.Write(f.v)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// sub returns the object stored at key k, or an empty one.
func (o object) sub(k string) object {
	if v := o.get(k); v != nil {
		if s, err := parseObject(v); err == nil {
			return s
		}
	}
	return nil
}

// putSub stores s at k, or removes k when s is empty.
func (o object) putSub(k string, s object) object {
	if len(s) == 0 {
		return o.del(k)
	}
	return o.set(k, s.bytes())
}

func ourGroup(group json.RawMessage) bool {
	var g struct {
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	_ = json.Unmarshal(group, &g)
	return slices.ContainsFunc(g.Hooks, func(h struct {
		Command string `json:"command"`
	}) bool {
		for _, a := range hookArgs {
			if strings.HasSuffix(h.Command, " "+a) && strings.Contains(h.Command, "murmur") {
				return true
			}
		}
		return false
	})
}

// editSettings adds (or with add=false removes) murmur's entries in Claude Code's settings.json.
func EditSettings(path, exe string, add bool) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		b = []byte("{}")
	} else if err != nil {
		return err
	}
	o, err := parseObject(b)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	hooks := o.sub("hooks")
	for _, ev := range hookEvents {
		var groups []json.RawMessage
		if v := hooks.get(ev); v != nil {
			_ = json.Unmarshal(v, &groups)
		}
		groups = slices.DeleteFunc(groups, ourGroup)
		if add {
			groups = append(groups, hookGroup(ev, exe))
		}
		if len(groups) == 0 {
			hooks = hooks.del(ev)
		} else {
			v, _ := json.Marshal(groups)
			hooks = hooks.set(ev, v)
		}
	}
	o = o.putSub("hooks", hooks)

	perms := o.sub("permissions")
	var allow []string
	if v := perms.get("allow"); v != nil {
		_ = json.Unmarshal(v, &allow)
	}
	allow = slices.DeleteFunc(allow, func(s string) bool { return slices.Contains(toolPermissions, s) })
	if add {
		allow = append(allow, toolPermissions...)
	}
	if len(allow) == 0 {
		perms = perms.del("allow")
	} else {
		v, _ := json.Marshal(allow)
		perms = perms.set("allow", v)
	}
	o = o.putSub("permissions", perms)

	return write(path, b, o, add)
}

func SetupState(path string) (hooks, perms bool, inbound string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, false, ""
	}
	o, err := parseObject(b)
	if err != nil {
		return false, false, ""
	}
	hooks = true
	for _, ev := range hookEvents {
		var groups []json.RawMessage
		_ = json.Unmarshal(o.sub("hooks").get(ev), &groups)
		hooks = hooks && slices.ContainsFunc(groups, ourGroup)
	}
	var allow []string
	_ = json.Unmarshal(o.sub("permissions").get("allow"), &allow)
	perms = !slices.ContainsFunc(toolPermissions, func(p string) bool { return !slices.Contains(allow, p) })
	_ = json.Unmarshal(o.get("crossSessionInbound"), &inbound)
	return hooks, perms, inbound
}

// inboundNote says when Claude Code's own settings keep murmur's messages from agents, or "" when they don't.
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

// MinVersion is the oldest Claude Code that takes messages from other sessions through its inbox.
const MinVersion = "2.1.224"

// TooOld says whether Claude Code version v can't take murmur's messages.
func TooOld(v string) bool { return release.Older(v, MinVersion) }

// write saves o in place of the settings file that held b, keeping its key order and permissions, and keeps a copy
// of the file as it was before murmur first changed it.
func write(path string, b []byte, o object, backup bool) error {
	var out bytes.Buffer
	if err := json.Indent(&out, o.bytes(), "", "  "); err != nil {
		return err
	}
	out.WriteByte('\n')
	if bytes.Equal(out.Bytes(), b) {
		return nil
	}
	if keep := path + ".before-murmur"; backup && len(b) > 2 {
		if _, err := os.Stat(keep); errors.Is(err, os.ErrNotExist) {
			_ = os.WriteFile(keep, b, 0o600)
		}
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".murmur-tmp"
	if err := os.WriteFile(tmp, out.Bytes(), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// AcceptMessages sets crossSessionInbound to accept, so this person's sessions take messages from other sessions
// without holding each one for an approval. join asks first: the setting covers every message between sessions.
func AcceptMessages(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	o, err := parseObject(b)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return write(path, b, o.set("crossSessionInbound", json.RawMessage(`"accept"`)), true)
}

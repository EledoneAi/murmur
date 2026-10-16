// Package hookish connects the harnesses whose hooks copied Claude Code's: Codex, Gemini CLI, GitHub Copilot CLI and
// Cursor. Each is a Spec, a row of a table: where its settings are, which of its events murmur hooks and what they
// mean, how its hooks answer, and where its MCP server is registered. None of them lists its live sessions or takes
// a message while a session runs, so murmur learns their sessions from the hooks, and messages leave with the answers
// of the hooks whose output the harness reads: after a tool call, at the end of a turn, or with the next prompt.
package hookish

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/EledoneAi/murmur/internal/config"
	"github.com/EledoneAi/murmur/internal/harness"
	"github.com/EledoneAi/murmur/internal/protocol"
	"github.com/EledoneAi/murmur/internal/release"
)

// Spec is one harness.
type Spec struct {
	name, title string
	command     string        // its command, on PATH
	dirVar      string        // its own variable for its folder, which a development build needs set
	dir         func() string // its folder
	hooksFile   string        // under dir
	tomlFile    string        // under dir: the TOML file murmur's MCP server goes in (Codex's config.toml)
	layout      layout
	timeoutUnit int // what a hook's timeout counts: 1 for seconds, 1000 for milliseconds
	hooks       []hook
	dialect     dialect
	needs       map[string]string // its notification types -> permission or input; "" for an event that has none
	mcpFile     string            // under dir: where its MCP servers are, under mcpServers; "" when its CLI adds them
	mcpEntry    func(exe string) map[string]any
	sessionVar  string
	apart       bool     // its MCP servers run in another process than its sessions
	allowTools  []string // murmur's tools as the harness names them, which its before-tool hook lets through
	minVersion  string   // the oldest version whose hooks do what murmur needs
	note        string   // what join tells its person to do once
	allow       []allowList
	checks      func(s *Spec) []protocol.Check // what else doctor says about it
	titleOf     func(s *Spec, id string) string
}

// hook is one event murmur hooks.
type hook struct {
	event   string // the harness's name for it
	kind    string // murmur's: harness.Start…
	matcher string
	async   bool
	carries bool // the harness reads what this hook prints into the agent's context: messages may leave with it
}

// allowList is a list of tool permissions in one of the harness's JSON files, at path, where murmur adds its own
// entries. With ifList set, murmur only adds to a list that is there already.
type allowList struct {
	file    string
	path    []string
	entries []string
	ifList  bool
}

// layout is how a harness keeps its hooks.
type layout int

const (
	groups  layout = iota // Claude Code's: {hooks: {Event: [{matcher, hooks: [{type, command, timeout}]}]}} (Codex, Gemini)
	ownFile               // a file of murmur's own, {version: 1, hooks: {event: [{type, bash, timeoutSec}]}} (Copilot)
	flat                  // {version: 1, hooks: {event: [{command, matcher, timeout}]}}, shared with the person's own (Cursor)
)

// dialect is how a harness reads what its hooks print.
type dialect int

const (
	claudeStyle  dialect = iota // Claude Code's: hookSpecificOutput, and decision: block at the end of a turn (Codex)
	geminiStyle                 // decision: deny with a reason, hookSpecificOutput for context (Gemini CLI)
	copilotStyle                // Claude Code's fields at the top level, and under hookSpecificOutput too (Copilot CLI)
	cursorStyle                 // snake_case: additional_context, permission, agent_message, followup_message (Cursor)
)

func (s *Spec) Name() string       { return s.name }
func (s *Spec) Title() string      { return s.title }
func (s *Spec) SessionVar() string { return s.sessionVar }
func (s *Spec) MCPApart() bool     { return s.apart }

func (s *Spec) hooksPath() string { return filepath.Join(s.dir(), s.hooksFile) }

func (s *Spec) Installed() bool {
	if _, err := exec.LookPath(s.command); err == nil {
		return true
	}
	_, err := os.Stat(s.dir())
	return err == nil
}

// Connected says whether murmur's hooks are in the harness's settings, as any version wrote them.
func (s *Spec) Connected() bool { return s.jsonHasOurs() }

// Holds says whether anything of murmur's is in the harness's settings: hooks, MCP server or tool permissions. A
// settings file murmur can't read may: uninstall tries it, and says what went wrong.
func (s *Spec) Holds() bool {
	if _, _, err := harness.ReadObject(s.hooksPath()); err != nil || s.Connected() || s.mcpConnected() {
		return true
	}
	for _, a := range s.allow {
		if a.holds(s) {
			return true
		}
	}
	return false
}

// commandFor is murmur's command for one of its hooks. Copilot refuses a tool when its before-tool hook fails, so its
// hooks never fail: a murmur that is gone lets every tool through.
func (s *Spec) commandFor(exe string, h hook) string {
	cmd := harness.ShellQuote(exe) + " hook " + s.name + " " + h.kind
	if s.layout == ownFile {
		cmd += " || true"
	}
	return cmd
}

// events lists the events murmur hooks, for people.
func (s *Spec) events() string {
	var e []string
	for _, h := range s.hooks {
		e = append(e, h.event)
	}
	return strings.Join(e, ", ")
}

// byHand is the error for a settings file murmur won't rewrite because of its comments: what to change by hand.
func byHand(path string, add bool, what any) error {
	if add {
		return fmt.Errorf("%s %w: add this to it by hand: %s", config.Tildify(path), harness.ErrComments, harness.Marshal(what))
	}
	return fmt.Errorf("%s %w: take murmur's entries out of it by hand", config.Tildify(path), harness.ErrComments)
}

func (s *Spec) jsonHasOurs() bool {
	o, _, err := harness.ReadObject(s.hooksPath())
	return err == nil && harness.HasOurs(o.Sub("hooks"))
}

// editHooks adds (or with add=false removes) murmur's hooks in the harness's hooks file. A hook of murmur's is
// rewritten in its place. Removing touches no file that holds none of murmur's.
func (s *Spec) editHooks(exe string, add bool) error {
	path := s.hooksPath()
	if s.layout == ownFile {
		if !add {
			if s.jsonHasOurs() {
				return os.Remove(path)
			}
			return nil
		}
		hooks := harness.Object{}
		for _, h := range s.hooks {
			e := map[string]any{"type": "command", "bash": s.commandFor(exe, h), "timeoutSec": 5}
			if h.matcher != "" {
				e["matcher"] = h.matcher
			}
			hooks = hooks.Set(h.event, harness.Marshal([]any{e}))
		}
		o := harness.Object{}.Set("version", json.RawMessage(`1`)).PutSub("hooks", hooks)
		return harness.WriteObject(path, nil, o, true)
	}
	o, b, err := harness.ReadObject(path)
	if err != nil {
		return err
	}
	found, mine := false, harness.Object{}
	hooks := o.Sub("hooks")
	for _, h := range s.hooks {
		var entries []json.RawMessage
		_ = json.Unmarshal(hooks.Get(h.event), &entries)
		var entry json.RawMessage
		if add {
			entry = s.entry(exe, h)
			mine = mine.Set(h.event, harness.Marshal([]json.RawMessage{entry}))
		}
		entries, had := harness.Replace(entries, entry)
		found = found || had
		if len(entries) == 0 {
			hooks = hooks.Del(h.event)
		} else {
			hooks = hooks.Set(h.event, harness.Marshal(entries))
		}
	}
	if !add && !found {
		return nil
	}
	o = o.PutSub("hooks", hooks)
	if s.layout == flat {
		switch {
		case add && o.Get("version") == nil:
			o = append(harness.Object{}, o...).Set("version", json.RawMessage(`1`))
		case !add && len(hooks) == 0:
			o = o.Del("version") // with no hooks left, nothing needs it: join added it
		}
	}
	if err := harness.WriteObject(path, b, o, add); errors.Is(err, harness.ErrComments) {
		return byHand(path, add, map[string]any{"hooks": json.RawMessage(mine.Bytes())})
	} else if err != nil {
		return err
	}
	return nil
}

// entry is murmur's entry for one event in a shared hooks file.
func (s *Spec) entry(exe string, h hook) json.RawMessage {
	handler := map[string]any{"type": "command", "command": s.commandFor(exe, h), "timeout": s.timeout(h)}
	if h.async {
		handler["async"] = true
	}
	var e map[string]any
	if s.layout == flat {
		e = map[string]any{"command": handler["command"], "timeout": handler["timeout"]}
	} else {
		e = map[string]any{"hooks": []any{handler}}
	}
	if h.matcher != "" {
		e["matcher"] = h.matcher
	}
	return harness.Marshal(e)
}

// timeout is a hook's timeout, in the harness's unit.
func (s *Spec) timeout(h hook) int {
	if h.kind == harness.End {
		return 2 * s.timeoutUnit // Codex caps a session's last hooks at 3 s
	}
	return 5 * s.timeoutUnit
}

// editMCP registers (or with add=false removes) murmur's MCP server in a JSON file, under mcpServers.
func (s *Spec) editMCP(exe string, add bool) error {
	path := filepath.Join(s.dir(), s.mcpFile)
	o, b, err := harness.ReadObject(path)
	if err != nil {
		return err
	}
	servers := o.Sub("mcpServers")
	if !add && servers.Get("murmur") == nil {
		return nil
	}
	servers = servers.Del("murmur")
	var e map[string]any
	if add {
		e = s.mcpEntry(exe)
		if home := os.Getenv("MURMUR_HOME"); home != "" { // harnesses may hand their MCP servers a bare environment
			e["env"] = map[string]string{"MURMUR_HOME": home}
		}
		servers = servers.Set("murmur", harness.Marshal(e))
	}
	if err := harness.WriteObject(path, b, o.PutSub("mcpServers", servers), add); errors.Is(err, harness.ErrComments) {
		return byHand(path, add, map[string]any{"mcpServers": map[string]any{"murmur": e}})
	} else if err != nil {
		return err
	}
	return nil
}

func (s *Spec) mcpConnected() bool {
	if s.mcpFile == "" {
		return codexMCPConnected(s)
	}
	o, _, err := harness.ReadObject(filepath.Join(s.dir(), s.mcpFile))
	return err == nil && o.Sub("mcpServers").Get("murmur") != nil
}

func (s *Spec) Connect(exe string, _ func(string) bool) []protocol.Check {
	if err := harness.Guard(s.dirVar); err != nil {
		return harness.Guarded(err)
	}
	var checks []protocol.Check
	if err := s.editHooks(exe, true); err != nil {
		checks = append(checks, protocol.Check{Name: "Hooks", Detail: err.Error()})
	} else {
		checks = append(checks, protocol.Check{Name: "Hooks", OK: true, Detail: s.events() + " in " + config.Tildify(s.hooksPath())})
	}
	var err error
	if s.mcpFile == "" {
		err = codexMCP(s, exe, true)
	} else {
		err = s.editMCP(exe, true)
	}
	for _, a := range s.allow {
		err = errors.Join(err, a.edit(s, true))
	}
	if err != nil {
		checks = append(checks, protocol.Check{Name: "MCP server", Detail: err.Error()})
	} else {
		checks = append(checks, protocol.Check{Name: "MCP server", OK: true, Detail: "murmur: who, send, allowed without a prompt"})
	}
	if s.note != "" {
		checks = append(checks, protocol.Check{Name: "Next", Detail: s.note})
	}
	if s.tomlFile != "" && codexInline(s) {
		checks = append(checks, protocol.Check{Name: "Next", Detail: "you keep Codex hooks in " + config.Tildify(s.tomlPath()) +
			", and murmur's are in hooks.json: Codex warns that both are in use until one holds them all"})
	}
	return append(checks, s.version()...)
}

// Disconnect takes out what Connect added, wherever it is, and goes on past a file it can't change, to say so at the
// end.
func (s *Spec) Disconnect(string) error {
	if err := harness.Guard(s.dirVar); err != nil {
		return err
	}
	err := s.editHooks("", false)
	if s.mcpFile == "" {
		err = errors.Join(err, codexMCP(s, "", false))
	} else {
		err = errors.Join(err, s.editMCP("", false))
	}
	for _, a := range s.allow {
		err = errors.Join(err, a.edit(s, false))
	}
	return err
}

func (s *Spec) Checks() []protocol.Check {
	fix := "missing: run murmur join"
	current := s.Connected()
	if o, _, err := harness.ReadObject(s.hooksPath()); err == nil && harness.HasOld(o.Sub("hooks")) {
		current, fix = false, "an older murmur's, which this version doesn't run: run murmur join, then restart your sessions"
	}
	checks := []protocol.Check{
		harness.Check(current, "Hooks", s.events()+" in "+config.Tildify(s.hooksPath()), fix),
		harness.Check(s.mcpConnected(), "MCP server", "murmur: who, send", "missing: run murmur join"),
	}
	if s.checks != nil {
		checks = append(checks, s.checks(s)...)
	}
	return append(checks, s.version()...)
}

// version checks the installed version, when the harness's command is on PATH.
func (s *Spec) version() []protocol.Check {
	v := harness.Version(s.command)
	if v == "" {
		return nil
	}
	old := s.minVersion != "" && release.Older(v, s.minVersion)
	return []protocol.Check{harness.Check(!old, "Version", v, v+": murmur needs "+s.minVersion+" or later")}
}

// edit adds (or with add=false removes) murmur's entries in the list. Removing leaves no empty list or object that
// only murmur's entries filled, and touches no file that holds none of them.
func (a allowList) edit(s *Spec, add bool) error {
	file := filepath.Join(s.dir(), a.file)
	o, b, err := harness.ReadObject(file)
	if err != nil || b == nil {
		return err
	}
	parent := o
	for _, k := range a.path[:len(a.path)-1] {
		parent = parent.Sub(k)
	}
	key := a.path[len(a.path)-1]
	if add && a.ifList && parent.Get(key) == nil {
		return nil // an editor's allowlist that isn't there stays so: one would replace the editor's own
	}
	var list []string
	_ = json.Unmarshal(parent.Get(key), &list)
	n := len(list)
	list = slices.DeleteFunc(list, func(e string) bool { return slices.Contains(a.entries, e) })
	if !add && len(list) == n {
		return nil
	}
	if add {
		list = append(list, a.entries...)
	}
	if len(list) == 0 && !add {
		parent = parent.Del(key)
	} else {
		parent = parent.Set(key, harness.Marshal(list))
	}
	if len(a.path) > 1 {
		o = o.PutSub(a.path[0], parent)
	} else {
		o = parent
	}
	// the file was there before join: it stays, even with nothing left in it
	if err := harness.WriteObject(file, b, o, true); errors.Is(err, harness.ErrComments) {
		return byHand(file, add, map[string]any{strings.Join(a.path, "."): a.entries})
	} else if err != nil {
		return err
	}
	return nil
}

func (a allowList) holds(s *Spec) bool {
	o, _, err := harness.ReadObject(filepath.Join(s.dir(), a.file))
	if err != nil {
		return false
	}
	for _, k := range a.path[:len(a.path)-1] {
		o = o.Sub(k)
	}
	var list []string
	_ = json.Unmarshal(o.Get(a.path[len(a.path)-1]), &list)
	return slices.ContainsFunc(list, func(e string) bool { return slices.Contains(a.entries, e) })
}

// hookInput is what these harnesses send a hook, under each one's names.
type hookInput struct {
	SessionID    string          `json:"session_id"`
	SessionCamel string          `json:"sessionId"`       // Copilot's camelCase events
	Conversation string          `json:"conversation_id"` // Cursor
	Transcript   string          `json:"transcript_path"`
	Cwd          string          `json:"cwd"`
	Roots        []string        `json:"workspace_roots"` // Cursor, whose cwd may be empty
	Event        string          `json:"hook_event_name"`
	Tool         string          `json:"tool_name"`
	ToolCamel    string          `json:"toolName"`
	Input        json.RawMessage `json:"tool_input"`
	Args         string          `json:"toolArgs"` // Copilot's camelCase events: the arguments as a JSON string
	Type         string          `json:"notification_type"`
}

// claudeKinds are the events of murmur's Claude Code hooks, which Cursor runs too.
var claudeKinds = map[string]string{"start": harness.Start, "edit": harness.BeforeTool, "notify": harness.Needs}

// Parse reads a hook's input. The event is murmur's own kind, as murmur's hook commands name it; for the Claude
// Code hooks Cursor runs, the event the harness names in the input wins.
func (s *Spec) Parse(event string, stdin []byte) (harness.Event, error) {
	var in hookInput
	if err := json.Unmarshal(stdin, &in); err != nil {
		return harness.Event{}, err
	}
	kind := event
	if k, ok := claudeKinds[event]; ok {
		kind = k
		for _, h := range s.hooks {
			if strings.EqualFold(h.event, in.Event) {
				kind = h.kind
			}
		}
	}
	ev := harness.Event{Kind: kind, Harness: s.name, SessionID: firstOf(in.SessionID, in.SessionCamel, in.Conversation),
		Cwd: in.Cwd, Transcript: in.Transcript, Tool: firstOf(in.Tool, in.ToolCamel)}
	for _, h := range s.hooks {
		if h.kind == kind {
			ev.Carries = h.carries
		}
	}
	if ev.Cwd == "" && len(in.Roots) > 0 {
		ev.Cwd = in.Roots[0]
	}
	input := in.Input
	if len(input) == 0 && json.Valid([]byte(in.Args)) {
		input = json.RawMessage(in.Args)
	}
	switch kind {
	case harness.BeforeTool:
		_, ev.Paths = harness.CallOf(ev.Cwd, ev.Tool, input)
	case harness.AfterTool:
		c, _ := harness.CallOf(ev.Cwd, ev.Tool, input)
		ev.Call = &c
	case harness.Needs:
		ev.Needs = s.needs[in.Type]
	case harness.Start, harness.Prompt:
		if s.titleOf != nil {
			ev.Title = s.titleOf(s, ev.SessionID)
		}
	}
	return ev, nil
}

func firstOf(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

// Answer prints murmur's text in the harness's dialect.
func (s *Spec) Answer(ev harness.Event, text string) []byte {
	if text == "" {
		if ev.Kind == harness.BeforeTool && slices.Contains(s.allowTools, ev.Tool) {
			// Copilot asks before every MCP tool, and remembers an answer per folder only: this hook allows murmur's
			return out(map[string]any{"permissionDecision": "allow",
				"hookSpecificOutput": map[string]string{"hookEventName": "PreToolUse", "permissionDecision": "allow"}})
		}
		return nil
	}
	name := map[string]string{} // the harness's name for each kind
	for _, h := range s.hooks {
		name[h.kind] = h.event
	}
	context := func() []byte {
		return out(map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": name[ev.Kind], "additionalContext": text}})
	}
	switch s.dialect {
	case claudeStyle:
		switch ev.Kind {
		case harness.Start, harness.Prompt, harness.AfterTool:
			return context()
		case harness.BeforeTool:
			return out(map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": name[ev.Kind],
				"permissionDecision": "deny", "permissionDecisionReason": text}})
		case harness.Stop:
			return out(map[string]any{"decision": "block", "reason": text})
		}
	case geminiStyle:
		switch ev.Kind {
		case harness.Start, harness.Prompt, harness.AfterTool:
			return context()
		case harness.BeforeTool, harness.Stop: // at the end of a turn, the reason is the agent's next prompt
			return out(map[string]any{"decision": "deny", "reason": text})
		}
	case copilotStyle:
		switch ev.Kind {
		case harness.Start, harness.Prompt, harness.AfterTool:
			return out(map[string]any{"additionalContext": text,
				"hookSpecificOutput": map[string]string{"hookEventName": name[ev.Kind], "additionalContext": text}})
		case harness.BeforeTool:
			return out(map[string]any{"permissionDecision": "deny", "permissionDecisionReason": text,
				"hookSpecificOutput": map[string]string{"hookEventName": name[ev.Kind], "permissionDecision": "deny", "permissionDecisionReason": text}})
		case harness.Stop:
			return out(map[string]any{"decision": "block", "reason": text})
		}
	case cursorStyle:
		switch ev.Kind {
		case harness.Start, harness.AfterTool:
			return out(map[string]any{"additional_context": text})
		case harness.Prompt:
			return out(map[string]any{"continue": true, "additional_context": text})
		case harness.BeforeTool:
			// Cursor hands its model user_message today, and documents agent_message for it: the reason goes in both,
			// and in the context a refusal may carry
			return out(map[string]any{"permission": "deny", "user_message": text, "agent_message": text, "additional_context": text})
		case harness.Stop:
			return out(map[string]any{"followup_message": text})
		}
	}
	return nil
}

func out(v any) []byte { return append(harness.Marshal(v), '\n') }

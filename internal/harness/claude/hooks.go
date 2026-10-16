package claude

import (
	"encoding/json"
	"os"

	"github.com/EledoneAi/murmur/internal/harness"
)

// HookInput is what Claude Code sends a hook on stdin, as far as murmur reads it. Harnesses that copied Claude Code's
// hooks send the same.
type HookInput struct {
	SessionID  string `json:"session_id"`
	Transcript string `json:"transcript_path"`
	Cwd        string `json:"cwd"`
	Type       string `json:"notification_type"` // on Notification
	ToolInput  struct {
		FilePath     string `json:"file_path"`     // Edit, MultiEdit and Write
		NotebookPath string `json:"notebook_path"` // NotebookEdit
	} `json:"tool_input"` // on PreToolUse
}

// Path is the file an edit is about to change, as an absolute path, or "" for another tool.
func (in HookInput) Path() string {
	p := in.ToolInput.FilePath
	if p == "" {
		p = in.ToolInput.NotebookPath
	}
	return harness.Abs(in.Cwd, p)
}

// needsOf turns Claude Code's notification types into what the console shows: an agent waiting for an approval, or
// for an answer.
var needsOf = map[string]string{"permission_prompt": "permission", "agent_needs_input": "input", "elicitation_dialog": "input",
	"elicitation_url_dialog": "input"}

// Parse reads a hook's input: start on SessionStart, edit on PreToolUse for the tools that change files, notify on
// Notification.
func (Claude) Parse(event string, stdin []byte) (harness.Event, error) {
	var in HookInput
	if err := json.Unmarshal(stdin, &in); err != nil {
		return harness.Event{}, err
	}
	// Claude Code documents this variable for hooks: the session's inbox, where murmur hands it messages
	ev := harness.Event{Harness: Name, SessionID: in.SessionID, Cwd: in.Cwd, Transcript: in.Transcript,
		Socket: os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET")}
	switch event {
	case "start":
		ev.Kind = harness.Start
	case "edit":
		ev.Kind = harness.BeforeTool
		if p := in.Path(); p != "" {
			ev.Paths = []string{p}
		}
	case "notify":
		ev.Kind, ev.Needs = harness.Needs, needsOf[in.Type]
	}
	return ev, nil
}

// Answer prints the brief as the session's context on start, and turns an edit back with its reason.
func (Claude) Answer(ev harness.Event, text string) []byte {
	switch {
	case text == "":
		return nil
	case ev.Kind == harness.Start:
		return StartContext(text)
	case ev.Kind == harness.BeforeTool:
		return Deny(text)
	}
	return nil
}

// StartContext is what a SessionStart hook prints to add text to the session's context before its first prompt.
// Claude Code reads it only from a hook it waits for: an async hook's context arrives a turn later.
func StartContext(text string) []byte {
	return HookOutput(map[string]string{"hookEventName": "SessionStart", "additionalContext": text})
}

// Deny is what a PreToolUse hook prints to turn a tool call back. Claude reads the reason; its person doesn't.
func Deny(reason string) []byte {
	return HookOutput(map[string]string{"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": reason})
}

// HookOutput is Claude Code's hook answer: an event's own fields under hookSpecificOutput.
func HookOutput(specific map[string]string) []byte {
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": specific})
	return append(b, '\n')
}

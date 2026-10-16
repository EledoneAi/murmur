package claudecode

import (
	"encoding/json"
	"path/filepath"
)

// HookInput is what Claude Code sends a hook on stdin, as far as murmur reads it.
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
	if p != "" && !filepath.IsAbs(p) && in.Cwd != "" {
		p = filepath.Join(in.Cwd, p)
	}
	return p
}

// StartContext is what a SessionStart hook prints to add text to the session's context before its first prompt.
// Claude Code reads it only from a hook it waits for: an async hook's context arrives a turn later.
func StartContext(text string) []byte {
	return hookOutput(map[string]string{"hookEventName": "SessionStart", "additionalContext": text})
}

// Deny is what a PreToolUse hook prints to turn a tool call back. Claude reads the reason; its person doesn't.
func Deny(reason string) []byte {
	return hookOutput(map[string]string{"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": reason})
}

func hookOutput(specific map[string]string) []byte {
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": specific})
	return append(b, '\n')
}

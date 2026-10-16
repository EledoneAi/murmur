// Package claudecode is everything murmur reads from or writes to Claude Code: its session files, transcripts,
// inbox sockets and settings.
package claudecode

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/EledoneAi/murmur/internal/config"
)

// ---------------------------------------------------------------- registry

type Entry struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	StatusAt  int64  `json:"statusUpdatedAt"`
	Socket    string `json:"messagingSocketPath"`
	ProcStart string `json:"procStart"`
	Entry     string `json:"entrypoint"`
	Version   string `json:"version"` // Claude Code's version in that session
}

// appOf names where a session runs. The VS Code and Desktop apps run the same Claude Code, so their sessions
// register, get an inbox and pick up the hooks and MCP server exactly like a terminal session.
func AppOf(entrypoint string) string {
	switch {
	case entrypoint == "claude-vscode":
		return "vscode"
	case entrypoint == "claude-desktop":
		return "desktop"
	case strings.HasPrefix(entrypoint, "sdk"):
		return "sdk"
	}
	return ""
}

// Dir is Claude Code's configuration folder: CLAUDE_CONFIG_DIR, or ~/.claude.
func Dir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	return filepath.Join(config.Home(), ".claude")
}

func RegistryDir() string { return filepath.Join(Dir(), "sessions") }

// pidOf turns ".../sessions/12345.json" into 12345.
func PIDOf(path string) (int, bool) {
	base := filepath.Base(path)
	n, ok := strings.CutSuffix(base, ".json")
	if !ok {
		return 0, false
	}
	pid, err := strconv.Atoi(n)
	return pid, err == nil && pid > 0
}

func ReadEntry(path string) (Entry, error) {
	var e Entry
	b, err := os.ReadFile(path)
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return e, err
	}
	if e.PID == 0 || e.SessionID == "" {
		return e, errors.New("incomplete entry")
	}
	return e, nil
}

func Alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// ---------------------------------------------------------------- transcript

func FindTranscript(id string) string {
	m, _ := filepath.Glob(filepath.Join(Dir(), "projects", "*", id+".jsonl"))
	if len(m) == 0 {
		return ""
	}
	return m[0]
}
